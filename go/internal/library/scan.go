// scan.go is the store-backed indexed scan: one walk, a per-file diff, and no
// network at all.
//
// The JSON index made the folder the unit of work, so the scan had to resolve
// metadata in the same pass to know whether a folder could be skipped at all.
// With per-file records the two questions separate:
//
//   - "what changed on disk" is answered by one walk plus a (size, mtime)
//     compare per walked file; unchanged files keep their stored record and
//     its resolved state and are never re-parsed;
//   - "what metadata is still missing" is the index's own state, resolved by
//     the background catch-up worker (catchup.go), which resumes at launch
//     and keeps posting progress after this function has returned.
//
// So an indexed Scan performs ZERO fetches: it walks, diffs, prunes, stamps
// rollups and hands the pending keys to the catch-up. Refresh (force=true)
// walks everything, re-parses every file, resets every record to pending and
// runs the catch-up in force mode, which re-resolves from the providers.
package library

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"potflix/internal/metadata"
)

// parseQuery parses one video file into its metadata query. It is a variable
// so a test can count re-parses; the production value is the same
// metadata.ResolveQuery (ParseRaw + titleclean + path context) the scan
// always used.
var parseQuery = metadata.ResolveQuery

// fileStamp is one walked file's filesystem identity for the diff.
type fileStamp struct {
	path  string
	size  int64
	mtime int64 // unix nanos
}

// scanIndexed is the indexed scan: enumerate + diff, then hand the pending
// keys to the catch-up worker.
//
// Progress contract (the UI is written against it): Done/Total are FILES and
// complete as soon as the index phase ends; Resolved/KeysTotal are metadata
// KEYS; after that, the catch-up keeps posting snapshots with
// Done == Total && Resolved < KeysTotal for as long as metadata work remains.
func scanIndexed(ctx context.Context, opts Options) (Stats, error) {
	schema := metadata.CacheSchema()
	idx, err := sharedIndex(opts.IndexPath)
	if err != nil {
		// A missing/unusable index is a full walk with an in-memory index,
		// never an error: the JSON index behaved the same way.
		fmt.Fprintf(os.Stderr, "library: index %q: %v (continuing without a persistent index)\n", opts.IndexPath, err)
	}
	if idx == nil {
		idx = &Index{folders: map[string]FolderRecord{}, files: map[string]*FileRecord{}}
	}
	si := &ScanIndex{ix: idx}

	// The folder skip is a filesystem-identity decision only. Metadata
	// liveness is the store's own per-file state now - a folder whose keys
	// are merely pending is still skippable while it is byte-identical -
	// because the catch-up resolves those keys from the index, not from a
	// walk. The always-true verifier keeps the exported guard's contract
	// while removing the cache-file dependency the JSON pass needed.
	keyPresent := func(string) bool { return true }

	enumerated := make(map[string]fileStamp, 4096)
	last := ""
	files, visited, skipped, walk, foldersScanned, foldersSkipped, filesSkipped, denied, walkErr, cancelled :=
		collectIndexed(ctx, opts.Dirs, si, true, schema, keyPresent, opts.Force, opts.IndexPath,
			func(path string, size, mtimeNano int64) {
				enumerated[normalizeScanDir(path)] = fileStamp{path: path, size: size, mtime: mtimeNano}
				last = path
			})
	if walkErr != nil {
		return Stats{}, walkErr
	}
	if cancelled {
		// A cancelled walk records NOTHING (the same rule as the legacy
		// path): a partial file set stamped with full-directory hashes
		// would let the next scan skip files that were never walked.
		st := Stats{Files: len(enumerated), Denied: denied, Cancelled: true,
			FoldersScanned: foldersScanned, FoldersSkipped: foldersSkipped, FilesSkipped: filesSkipped}
		return st, nil
	}

	// --- diff ---------------------------------------------------------
	// Every walked file compares (size, mtime) against its stored record.
	// Unchanged: keep the record, never re-parse. Changed or new: re-parse
	// the title and reset the metadata state this record's key owns.
	keys := make(map[string]string, len(enumerated))
	var unchanged, parsed, changed int
	for _, st := range enumerated {
		rec, ok := idx.file(st.path)
		if ok && !opts.Force && rec.Size == st.size && rec.Mtime == st.mtime {
			unchanged++
			keys[st.path] = rec.MetaKey
			continue
		}
		q := parseQuery(filepath.Base(st.path), st.path)
		next := FileRecord{
			Path:     st.path,
			Name:     filepath.Base(st.path),
			Ext:      strings.ToLower(filepath.Ext(st.path)),
			Size:     st.size,
			Mtime:    st.mtime,
			Kind:     recordKind(q.Kind),
			Title:    q.Title,
			Season:   q.Season,
			Episode:  q.Episode,
			Year:     q.Year,
			Absolute: q.Absolute,
			State:    StateResolved, // from here: StatePending unless nothing to resolve
		}
		if q.Kind != metadata.KindUnknown {
			next.MetaKey = metadata.CacheKeyFor(q)
			next.State = StatePending
		}
		if ok && rec.MetaKey != "" && rec.MetaKey == next.MetaKey {
			// Same swappable key, same repair history: a re-encode is not a
			// new title, so the cooldown and the attempt count stand.
			next.State, next.Attempts = rec.State, rec.Attempts
			next.LastError, next.LastTry, next.NextTry = rec.LastError, rec.LastTry, rec.NextTry
		}
		if !ok {
			changed++
		}
		parsed++
		idx.putFile(next)
		keys[st.path] = next.MetaKey
		if opts.Progress != nil && parsed%500 == 0 {
			// The scan's only mid-index progress: total is not known until
			// every file is walked, so this reports files SEEN, not a
			// fraction (Total stays 0 = indeterminate for the bar).
			opts.Progress(Progress{Done: len(keys), FoldersScanned: foldersScanned, FoldersSkipped: foldersSkipped, Last: st.path})
		}
	}

	// --- prune --------------------------------------------------------
	// A record whose parent directory was just visited but whose path was
	// not enumerated is gone from disk (or stopped being a video file).
	enumeratedPaths := make(map[string]bool, len(files))
	for _, p := range files {
		enumeratedPaths[normalizeScanDir(p)] = true
	}
	pruned := idx.pruneFiles(visitedSet(visited), enumeratedPaths)

	// --- folder rollups ----------------------------------------------
	// The same verified stamping the JSON pass used: a rollup is written
	// only when the filesystem still shows exactly what was walked, so a
	// concurrent change refuses the stamp and the folder is re-walked.
	recordIndexKeys(si, visited, skipped, keys, opts.IndexPath, schema, walk)

	// --- report the index phase --------------------------------------
	state := idx.state()
	st := Stats{
		Files:          len(enumerated),
		Unique:         distinctKeys(keys),
		Keys:           state.Keys,
		KeysDone:       state.KeysResolved,
		KeysCached:     state.KeysResolved,
		KeysNotFound:   state.Keys - state.KeysResolved,
		Resolved:       state.FilesResolved,
		Cached:         state.FilesResolved,
		NotFound:       state.FilesPending,
		Skipped:        state.FilesNoKey,
		Denied:         denied,
		FoldersScanned: foldersScanned,
		FoldersSkipped: foldersSkipped,
		FilesSkipped:   filesSkipped,
		Unchanged:      unchanged,
		Parsed:         parsed,
		Changed:        changed,
		Pruned:         pruned,
		Pending:        state.FilesPending,
		NeedsRepair:    state.FilesNeedsRepair,
	}
	if opts.Progress != nil {
		opts.Progress(Progress{
			Done: state.Files, Total: state.Files,
			Resolved: state.KeysResolved, KeysTotal: state.Keys,
			KeysDone: state.KeysResolved, KeysCached: state.KeysResolved,
			KeysNotFound:   state.Keys - state.KeysResolved,
			Last:           last,
			FoldersScanned: foldersScanned, FoldersSkipped: foldersSkipped,
		})
	}

	// The index phase is over: the UI can show "indexed N of M" now, and the
	// catch-up keeps the same callback fed until the metadata catches up.
	//
	// The pass is started WITHOUT the scan's own context, deliberately. The
	// window cancels that context the moment the scan worker posts its result
	// (window.go onScanDone cancels scanCancel on every landing, not only on a
	// user cancel), so a pass bound to it was cancelled the instant it began:
	// measured live 2026-10-13 00:37:41, the hand-off after a 7,806-file /
	// 2,302-key scan settled nothing, created no metadata cache store and wrote
	// no report, leaving 7,303 pending files unfetched until the next launch.
	// The pass owns its own lifetime, derives its work set from the index and
	// resumes there; StopCatchUp (or a caller-supplied Ctx, pass-level) ends it.
	if !opts.SkipCatchUp && ctx.Err() == nil {
		StartCatchUp(CatchUpConfig{
			Dirs:       opts.Dirs,
			CacheDir:   opts.CacheDir,
			IndexPath:  opts.IndexPath,
			Force:      opts.Force,
			AppVersion: opts.AppVersion,
		}, opts.Progress)
	}
	return st, nil
}

// visitedSet normalizes a visited-directory list into a set.
func visitedSet(visited []string) map[string]bool {
	set := make(map[string]bool, len(visited))
	for _, v := range visited {
		set[normalizeScanDir(v)] = true
	}
	return set
}

// distinctKeys counts the distinct non-empty metadata keys of one walk's
// file-key map.
func distinctKeys(keys map[string]string) int {
	seen := make(map[string]bool, len(keys))
	for _, k := range keys {
		if k != "" {
			seen[k] = true
		}
	}
	return len(seen)
}

// recordKind maps a parsed query kind onto the stored kind string.
func recordKind(k metadata.Kind) string {
	switch k {
	case metadata.KindMovie:
		return fileKindMovie
	case metadata.KindSeries:
		return fileKindSeries
	default:
		return fileKindUnknown
	}
}
