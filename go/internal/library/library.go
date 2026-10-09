// Package library is the reusable walk + resolve loop behind both the
// metawarm CLI (go/cmd/metawarm) and the tray window's Scan/Refresh buttons.
//
// Walk collects video files under the given dirs. Scan walks the dirs,
// resolves metadata once per unique query (dedupe key: Kind, lowercased
// Title, Year, Season, Absolute, Episode), and caches it via the metadata
// resolver. force=false is incremental: an already-cached entry is served
// from disk without a fetch, so only newly added files hit the network.
// force=true (Refresh) bypasses and rewrites every cached entry.
//
// The progress callback, when non-nil, is invoked from worker goroutines as
// each unique key's fetch completes (Done counts files, Total is the walked
// file count); it must be non-blocking and thread-safe (e.g. PostMessage).
package library

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"potflix/internal/metadata"
)

const (
	// rateInterval is the historical global fetch spacing (~1 request /
	// 400ms). Scan no longer uses it: provider throttling now lives in
	// the metadata per-host limiter, so cached keys never sleep. It is
	// retained for FetchAll callers that opt into an additional global
	// floor by passing a positive gateInterval.
	rateInterval = 400 * time.Millisecond
	// lookupTimeout bounds a single unique-key fetch so one slow key
	// cannot stall the run; other keys proceed regardless.
	lookupTimeout = 120 * time.Second
)

// defaultConcurrency bounds concurrent fetches, as metawarm's -concurrency
// default always did.
const defaultConcurrency = 3

// checkpointEveryFolders and checkpointEveryInterval bound mid-scan index
// flushes: every ~50 visited folders or every ~2s, whichever comes first,
// plus a final flush at the end.
const (
	checkpointEveryFolders  = 50
	checkpointEveryInterval = 2 * time.Second
)

var videoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".m4v": true,
	".mov": true, ".wmv": true, ".ts": true, ".flv": true,
	".webm": true, ".mpg": true, ".mpeg": true,
}

// walkDir is filepath.WalkDir by default but injectable for tests.
var walkDir = filepath.WalkDir

// Stats tallies one Scan run. Files counts every walked video file;
// Resolved/NotFound/Errors count per file (a deduped key's verdict applies
// to each of its files). Resolved keeps its historical meaning (verdict
// Found); Cached/Fetched split it per file by lookup provenance, so
// Cached + Fetched == Resolved always holds.
//
// Index runs (ScanWithOptions with UseIndex) may skip whole subtrees:
// FoldersScanned counts directories actually walked, FoldersSkipped counts
// directories skipped via the index, and FilesSkipped sums the recorded
// video-file counts of skipped folders, so Files + FilesSkipped is the
// total logical file count. Skipped files bypass resolution entirely:
// they contribute only to FilesSkipped, never to Resolved/Cached/Fetched.
// Keys progress counters: Keys is the unique-key count (len(groups));
// KeysCached counts stat-hit keys pre-credited before fetching; KeysFetched
// counts uncached keys resolved by fetch; KeysNotFound counts uncached keys
// missed or errored (so every key lands in exactly one bucket).
// Invariant: KeysDone == KeysCached + KeysFetched + KeysNotFound == Keys.
type Stats struct {
	Keys           int
	KeysCached     int
	KeysFetched    int
	KeysNotFound   int
	KeysDone       int
	Cancelled      bool
	Files          int
	Skipped        int
	Unique         int
	Resolved       int
	Cached         int
	Fetched        int
	NotFound       int
	Errors         int
	Denied         int
	FoldersScanned int
	FoldersSkipped int
	FilesSkipped   int
}

// Progress is one live scan update. Done counts finished files against the
// walked Total; Resolved is the running resolved-file tally (split into
// running Cached/Fetched tallies); KeysTotal/KeysDone track unique-key work
// (KeysDone == KeysCached + KeysFetched + KeysNotFound); Last is the most
// path. FoldersScanned/FoldersSkipped carry the index walk totals so the
// window status line can print "folders: N scanned, M skipped".
type Progress struct {
	Done           int
	Total          int
	Resolved       int
	Cached         int
	Fetched        int
	KeysTotal      int
	KeysDone       int
	KeysCached     int
	KeysFetched    int
	KeysNotFound   int
	Last           string
	FoldersScanned int
	FoldersSkipped int
}

// Options configures ScanWithOptions. Dirs/Force/CacheDir/Progress mirror
// Scan's parameters. IndexPath is the scan-index file (e.g.
// %LOCALAPPDATA%\Potflix\scanindex.json); UseIndex enables the
// two-tier skip (default off, so plain Scan behaviour is unchanged).
// An empty IndexPath with UseIndex set disables the index (full walk,
// no save), never an error.
type Options struct {
	Dirs      []string
	Force     bool
	CacheDir  string
	IndexPath string
	UseIndex  bool
	Progress  func(Progress)
}

// newResolver builds the scan resolver; tests may replace it to inject fake
// sources (the production resolver hits the network).
var newResolver = func(cacheDir string) *metadata.Resolver {
	return metadata.New(metadata.Options{Dir: cacheDir})
}

// CollectFiles walks each root recursively (lexical walk order) for video
// files. limit > 0 truncates the result. A subdirectory or file that cannot
// be opened (permission or any walk error on a non-root entry) is logged
// once, counted as denied, and skipped; only an unreadable ROOT (or no
// readable roots at all) is a fatal error.
func CollectFiles(roots []string, limit int) ([]string, int, error) {
	var files []string
	var denied int
	var okRoots int
	var firstErr error
	for _, root := range roots {
		fi, err := os.Stat(root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "library: skipping unreadable path %q: %v\n", root, err)
			denied++
			if firstErr == nil {
				firstErr = fmt.Errorf("unwalkable root %q: %w", root, err)
			}
			continue
		}
		if !fi.IsDir() {
			fmt.Fprintf(os.Stderr, "library: skipping unreadable path %q: not a directory\n", root)
			denied++
			if firstErr == nil {
				firstErr = fmt.Errorf("unwalkable root %q: not a directory", root)
			}
			continue
		}
		if err := walkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				if path == root {
					return err
				}
				fmt.Fprintf(os.Stderr, "library: skipping unreadable path %q: %v\n", path, err)
				denied++
				if d != nil && d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if videoExts[strings.ToLower(filepath.Ext(path))] {
				files = append(files, path)
			}
			return nil
		}); err != nil {
			fmt.Fprintf(os.Stderr, "library: skipping unreadable path %q: %v\n", root, err)
			denied++
			if firstErr == nil {
				firstErr = fmt.Errorf("unwalkable root %q: %w", root, err)
			}
			continue
		}
		okRoots++
	}
	if okRoots == 0 && firstErr != nil {
		return nil, denied, firstErr
	}
	if limit > 0 && len(files) > limit {
		files = files[:limit]
	}
	return files, denied, nil
}

// CollectFilesWithContext is CollectFiles with a cancellable context: a
// cancelled ctx aborts the walk promptly and returns partial results with
// cancelled=true and a nil error (a user cancel is not a failure).
func CollectFilesWithContext(ctx context.Context, roots []string, limit int) ([]string, int, bool, error) {
	var files []string
	var denied int
	var okRoots int
	var firstErr error
	cancelled := false
	for _, root := range roots {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		fi, err := os.Stat(root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "library: skipping unreadable path %q: %v\n", root, err)
			denied++
			if firstErr == nil {
				firstErr = fmt.Errorf("unwalkable root %q: %w", root, err)
			}
			continue
		}
		if !fi.IsDir() {
			fmt.Fprintf(os.Stderr, "library: skipping unreadable path %q: not a directory\n", root)
			denied++
			if firstErr == nil {
				firstErr = fmt.Errorf("unwalkable root %q: not a directory", root)
			}
			continue
		}
		if err := walkDir(root, func(path string, d os.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				if path == root {
					return err
				}
				fmt.Fprintf(os.Stderr, "library: skipping unreadable path %q: %v\n", path, err)
				denied++
				if d != nil && d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if videoExts[strings.ToLower(filepath.Ext(path))] {
				files = append(files, path)
			}
			return nil
		}); err != nil {
			if ctx.Err() != nil || err == context.Canceled || err == context.DeadlineExceeded {
				cancelled = true
				break
			}
			fmt.Fprintf(os.Stderr, "library: skipping unreadable path %q: %v\n", root, err)
			denied++
			if firstErr == nil {
				firstErr = fmt.Errorf("unwalkable root %q: %w", root, err)
			}
			continue
		}
		okRoots++
	}
	if ctx.Err() != nil {
		cancelled = true
	}
	if okRoots == 0 && firstErr != nil && !cancelled {
		return nil, denied, cancelled, firstErr
	}
	if limit > 0 && len(files) > limit {
		files = files[:limit]
	}
	return files, denied, cancelled, nil
}

// Walk calls fn once per video file under dirs, in lexical walk order.
// Denied subdirectories are skipped (as in CollectFiles); only an
// unreadable root set is a fatal error. If fn returns an error the walk
// stops and that error is returned.
func Walk(dirs []string, fn func(path string) error) error {
	files, _, err := CollectFiles(dirs, 0)
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := fn(f); err != nil {
			return err
		}
	}
	return nil
}

// DedupeKey is the resolver's cache identity for one query.
func DedupeKey(q metadata.Query) string {
	return fmt.Sprintf("%d|%s|%d|%d|%t|%d",
		int(q.Kind), strings.ToLower(q.Title), q.Year, q.Season, q.Absolute, q.Episode)
}

// FilePlan is one walked file's parsed query.
type FilePlan struct {
	Path    string
	Query   metadata.Query
	Key     string
	Skipped bool
}

// Group is one unique query shared by one or more files.
type Group struct {
	Key   string
	Query metadata.Query
	Paths []string
}

// BuildPlan parses every file and dedupes non-Unknown queries into groups,
// preserving walk order.
func BuildPlan(files []string) ([]FilePlan, []*Group) {
	var plans []FilePlan
	var groups []*Group
	byKey := make(map[string]*Group)
	for _, path := range files {
		q := metadata.ResolveQuery(filepath.Base(path), path)
		if q.Kind == metadata.KindUnknown {
			plans = append(plans, FilePlan{Path: path, Query: q, Skipped: true})
			continue
		}
		key := DedupeKey(q)
		g, ok := byKey[key]
		if !ok {
			g = &Group{Key: key, Query: q}
			byKey[key] = g
			groups = append(groups, g)
		}
		g.Paths = append(g.Paths, path)
		plans = append(plans, FilePlan{Path: path, Query: q, Key: key})
	}
	return plans, groups
}

// LookupFunc resolves one unique query; see FetchAll.
type LookupFunc func(ctx context.Context, q metadata.Query) (metadata.Resolution, error)

// FetchResult is one unique key's verdict. FromCache mirrors the resolver's
// provenance bit: true when a FOUND resolution was served from cache.
type FetchResult struct {
	Res       metadata.Resolution
	Err       error
	FromCache bool
}

// rateGate spaces calls ~1 per interval; the first call goes immediately.
type rateGate struct {
	mu       sync.Mutex
	last     time.Time
	interval time.Duration
}

func (g *rateGate) wait() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.last.IsZero() {
		if d := time.Until(g.last.Add(g.interval)); d > 0 {
			time.Sleep(d)
		}
	}
	g.last = time.Now()
}

// FetchAll calls lookup once per unique group with bounded concurrency. A
// single key's error is recorded, never fatal. gateInterval <= 0 disables
// rate spacing (cache-only runs). onDone, when non-nil, is called once per
// group as its fetch completes, from a worker goroutine; it must be
// non-blocking and thread-safe. A cancelled ctx fails the lookups still
// waiting to start via their derived timeout context, and FetchAll stops
// queueing new groups once ctx is done so a cancel returns promptly.
func FetchAll(ctx context.Context, groups []*Group, lookup LookupFunc, concurrency int, gateInterval time.Duration, onDone func(g *Group, r FetchResult)) map[string]FetchResult {
	out := make(map[string]FetchResult, len(groups))
	var mu sync.Mutex
	var gate *rateGate
	if gateInterval > 0 {
		gate = &rateGate{interval: gateInterval}
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, g := range groups {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(g *Group) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			if gate != nil {
				gate.wait()
			}
			if ctx.Err() != nil {
				return
			}
			lctx, cancel := context.WithTimeout(ctx, lookupTimeout)
			defer cancel()
			res, err := lookup(lctx, g.Query)
			fr := FetchResult{Res: res, Err: err}
			mu.Lock()
			out[g.Key] = fr
			mu.Unlock()
			if onDone != nil {
				onDone(g, fr)
			}
		}(g)
	}
	wg.Wait()
	return out
}

// Scan walks dirs for video files and resolves each unique query, caching it
// under cacheDir ("" selects the default metadata cache dir). force=false is
// incremental (cached entries are served from disk, never refetched);
// force=true bypasses and rewrites every cached entry. progress, when
// non-nil, receives live per-group updates from worker goroutines.
//
// Back-compat: Scan never consults the scan index (UseIndex off), so its
// walk and verdicts are exactly the historical ones; it delegates to
// ScanWithOptions. Index runs go through ScanWithOptions with UseIndex set.
func Scan(ctx context.Context, dirs []string, force bool, cacheDir string, progress func(Progress)) (Stats, error) {
	return ScanWithOptions(ctx, Options{
		Dirs: dirs, Force: force, CacheDir: cacheDir, Progress: progress,
	})
}

// collectIndexed walks each root for video files, consulting ix when
// enabled. It returns the walked files (skipped subtrees excluded), the
// visited (non-skipped) directories, the skipped records by normalized dir,
// and the folder/file tallies. force disables all skipping but still
// visits every dir for recording. Any hash doubt (stat/read errors) walks
// the folder normally. Tier-2 hits refresh the record's tier-1 hash.
// ctx cancels the walk promptly: the walk callback checks ctx.Err() and
// aborts, and collectIndexed reports cancelled=true with partial results
// (not an error). When indexed, the index is checkpointed every
// checkpointEveryFolders visited folders or every checkpointEveryInterval,
// whichever comes first, via the existing atomic Save, so an interrupted
// scan keeps what was walked so far.
func collectIndexed(ctx context.Context, roots []string, ix *ScanIndex, useIndex bool, schema int, keyPresent func(string) bool, force bool, indexPath string) (files []string, visited []string, skipped map[string]FolderRecord, foldersScanned, foldersSkipped, filesSkipped, denied int, firstErr error, cancelled bool) {
	skipped = make(map[string]FolderRecord)
	enabled := useIndex && !force && ix != nil
	indexed := useIndex && indexPath != "" && ix != nil
	var okRoots int
	lastFlush := time.Now()
	lastFlushCount := 0
	flush := func() {
		if !indexed {
			return
		}
		lastFlush = time.Now()
		lastFlushCount = len(visited)
		if err := ix.Save(indexPath); err != nil {
			fmt.Fprintf(os.Stderr, "library: checkpoint scan index %q: %v\n", indexPath, err)
		}
	}
	for _, root := range roots {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		fi, err := os.Stat(root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "library: skipping unreadable path %q: %v\n", root, err)
			denied++
			if firstErr == nil {
				firstErr = fmt.Errorf("unwalkable root %q: %w", root, err)
			}
			continue
		}
		if !fi.IsDir() {
			fmt.Fprintf(os.Stderr, "library: skipping unreadable path %q: not a directory\n", root)
			denied++
			if firstErr == nil {
				firstErr = fmt.Errorf("unwalkable root %q: not a directory", root)
			}
			continue
		}
		rootOK := false
		if err := walkDir(root, func(path string, d os.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				if path == root {
					return err
				}
				fmt.Fprintf(os.Stderr, "library: skipping unreadable path %q: %v\n", path, err)
				denied++
				if d != nil && d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				if enabled {
					if nh, nhErr := FolderNamesHash(path); nhErr == nil {
						if ix.ShouldSkipNames(path, nh, schema, keyPresent) {
							if rec, ok := ix.Lookup(path); ok {
								filesSkipped += rec.Files
								skipped[normalizeScanDir(path)] = rec
							}
							foldersSkipped++
							return filepath.SkipDir
						}
					}
					if fp, fpErr := FolderFingerprint(path); fpErr == nil {
						if ix.ShouldSkip(path, fp, schema, keyPresent) {
							rec, ok := ix.Lookup(path)
							if ok {
								filesSkipped += rec.Files
								skipped[normalizeScanDir(path)] = rec
								// Refresh tier 1: benign dir-mtime bump
								// with identical contents. Recompute the
								// names hash (it missed above) and
								// re-record so the next scan hits tier 1.
								if nh, nhErr := FolderNamesHash(path); nhErr == nil {
									ix.RecordFull(path, nh, fp, schema, rec.Keys, rec.Files)
								}
							}
							foldersSkipped++
							return filepath.SkipDir
						}
					}
				}
				foldersScanned++
				visited = append(visited, path)
				if indexed && (len(visited)-lastFlushCount >= checkpointEveryFolders || time.Since(lastFlush) >= checkpointEveryInterval) {
					flush()
				}
				return nil
			}
			if videoExts[strings.ToLower(filepath.Ext(path))] {
				files = append(files, path)
			}
			return nil
		}); err != nil {
			if ctx.Err() != nil || err == context.Canceled || err == context.DeadlineExceeded {
				cancelled = true
				break
			}
			fmt.Fprintf(os.Stderr, "library: skipping unreadable path %q: %v\n", root, err)
			denied++
			if firstErr == nil {
				firstErr = fmt.Errorf("unwalkable root %q: %w", root, err)
			}
			continue
		}
		rootOK = true
		if rootOK {
			okRoots++
		}
	}
	if ctx.Err() != nil {
		cancelled = true
	}
	if okRoots == 0 && firstErr != nil && !cancelled {
		return nil, nil, skipped, foldersScanned, foldersSkipped, filesSkipped, denied, firstErr, cancelled
	}
	return files, visited, skipped, foldersScanned, foldersSkipped, filesSkipped, denied, nil, cancelled
}

// CollectResult is the outcome of an index-aware walk: the walked video
// files (skipped subtrees excluded), visited directories, skipped records,
// and folder/file tallies.
// Window integration (window.go is owned by a sibling worker; this package
// only supplies the fields). To show folder skips, print exactly:
//
//	fmt.Sprintf("folders: %d scanned, %d skipped", stats.FoldersScanned, stats.FoldersSkipped)
//
// Recommended status line (onScanDone):
//
//	kind + complete: ... + " folders: N scanned, M skipped"
//
// Recommended log line: append " foldersScanned=%d foldersSkipped=%d
// filesSkipped=%d" to the existing scan/refresh complete log. Live progress
// can show p.FoldersScanned/p.FoldersSkipped the same way.
type CollectResult struct {
	Files          []string
	Visited        []string
	Skipped        map[string]FolderRecord
	FoldersScanned int
	FoldersSkipped int
	FilesSkipped   int
	Denied         int
}

// CollectIndexedWithContext is CollectIndexed with a cancellable context:
// a cancelled ctx aborts the walk and returns partial results with
// cancelled=true and a nil error (a user cancel is not a failure).
func CollectIndexedWithContext(ctx context.Context, roots []string, cacheDir, indexPath string, useIndex, force bool) (CollectResult, bool, error) {
	var out CollectResult
	out.Skipped = make(map[string]FolderRecord)
	indexed := useIndex && indexPath != ""
	var ix *ScanIndex
	schema := metadata.CacheSchema()
	var keyPresent func(string) bool
	if indexed {
		ix = LoadScanIndex(indexPath)
		keyPresent = func(key string) bool { return metadata.CacheLive(cacheDir, key) }
	}
	files, visited, skipped, fs, fk, fsk, denied, err, cancelled := collectIndexed(ctx, roots, ix, indexed, schema, keyPresent, force, indexPath)
	if err != nil {
		return CollectResult{}, cancelled, err
	}
	out.Files = files
	out.Visited = visited
	out.Skipped = skipped
	out.FoldersScanned = fs
	out.FoldersSkipped = fk
	out.FilesSkipped = fsk
	out.Denied = denied
	return out, cancelled, nil
}

// CollectIndexed walks roots with the two-tier index skip when useIndex is
// set (indexPath locates the file; force disables skipping but still
// visits). cacheDir selects the metadata cache for the CacheLive guard
// ("" resolves to the default). A missing/corrupt index is a full walk,
// never an error. Only an all-unreadable root set is fatal.
func CollectIndexed(roots []string, cacheDir, indexPath string, useIndex, force bool) (CollectResult, error) {
	cr, _, err := CollectIndexedWithContext(context.Background(), roots, cacheDir, indexPath, useIndex, force)
	if err != nil {
		return CollectResult{}, err
	}
	return cr, nil
}

// RecordScan re-records every visited dir from an indexed walk (keys from
// the walked plans plus skipped-subtree totals), prunes, and saves
// atomically. It is a no-op for an empty indexPath or with no visited
// dirs (still prunes/saves when an index exists). Save errors are logged,
// never fatal.
func RecordScan(indexPath string, visited []string, skipped map[string]FolderRecord, plans []FilePlan) {
	if indexPath == "" {
		return
	}
	ix := LoadScanIndex(indexPath)
	recordIndex(ix, visited, skipped, plans, indexPath, metadata.CacheSchema())
}

// ScanWithOptions is Scan with an opt-in folder index. With UseIndex off
// (or an empty IndexPath) it walks everything, exactly like Scan.
// With UseIndex on: force=false consults the index with the two-tier skip
// (tier-1 FolderNamesHash, tier-2 FolderFingerprint, both guarded by
// metadata.CacheLive per recorded key); force=true ignores the index for
// skipping but still rewrites every visited record. A missing/corrupt
// index file is a full walk, never an error. On success the index is
// pruned (PruneExisting) and saved atomically; a save failure is logged
// to stderr but does not fail the scan.
//
// Checkpointing: when indexed, the walk checkpoints the index every
// checkpointEveryFolders visited folders or every checkpointEveryInterval
// via the existing atomic Save. The walk-phase checkpoint only persists
// records from earlier complete runs (plus tier-2 refreshed skip records),
// never the in-progress walk, so it can only ever write fully walked
// folders. The pre-fetch, fetch-phase and final recordIndex flushes all
// verify a live context first: a cancelled run records NOTHING (a partial
// key list stamped with full-directory hashes would let the next scan
// skip files that were never walked). This is safe because ShouldSkip
// refuses to skip unless every recorded key is live in the cache, so a
// folder recorded before its keys resolve is simply re-walked. A cancelled
// ctx stops the walk and fetch promptly (walk callback and fetch loop
// check ctx.Err()) and returns partial stats with Cancelled=true and a
// nil error: a user cancel is not a failure.
func ScanWithOptions(ctx context.Context, opts Options) (Stats, error) {
	indexed := opts.UseIndex && opts.IndexPath != ""
	var ix *ScanIndex
	schema := metadata.CacheSchema()
	var keyPresent func(string) bool
	if indexed {
		ix = LoadScanIndex(opts.IndexPath)
		cacheDir := opts.CacheDir
		keyPresent = func(key string) bool { return metadata.CacheLive(cacheDir, key) }
	}
	files, visited, skipped, foldersScanned, foldersSkipped, filesSkipped, denied, walkErr, walkCancelled := collectIndexed(ctx, opts.Dirs, ix, indexed, schema, keyPresent, opts.Force, opts.IndexPath)
	if walkErr != nil {
		return Stats{}, walkErr
	}
	plans, groups := BuildPlan(files)
	if walkCancelled || ctx.Err() != nil {
		st := Stats{Files: len(plans), Unique: len(groups), Denied: denied,
			FoldersScanned: foldersScanned, FoldersSkipped: foldersSkipped, FilesSkipped: filesSkipped,
			Keys: len(groups), Cancelled: true}
		// Cancelled/partial walk: record NOTHING. A partial key list
		// stamped with the current full-directory hashes would let the
		// next scan skip files that were never walked.
		return st, nil
	}
	// Pre-fetch checkpoint: record every visited dir with its full cache-key
	// list (from what is known: the plans) so the index file exists before
	// the fetch phase completes. Unresolved keys simply miss the live-cache
	// guard, so their folders are re-walked next time (see safety note
	// above). Skipped when the context is already cancelled: a cancelled
	// run records nothing.
	if indexed && ctx.Err() == nil {
		recordIndex(ix, visited, skipped, plans, opts.IndexPath, schema)
	}
	st := Stats{Files: len(plans), Unique: len(groups), Denied: denied,
		FoldersScanned: foldersScanned, FoldersSkipped: foldersSkipped, FilesSkipped: filesSkipped,
		Keys: len(groups)}
	keysTotal := len(groups)
	keysCached := 0
	cachedHit := make(map[string]bool, len(groups))
	if !opts.Force {
		for _, g := range groups {
			if metadata.CacheLive(opts.CacheDir, metadata.CacheKeyFor(g.Query)) {
				keysCached++
				cachedHit[g.Key] = true
			}
		}
	}
	keysFetched, keysNotFound := 0, 0
	keysDone := keysCached
	st.KeysCached = keysCached
	st.KeysDone = keysDone
	if len(groups) == 0 && opts.Progress != nil {
		opts.Progress(Progress{FoldersScanned: foldersScanned, FoldersSkipped: foldersSkipped,
			KeysTotal: keysTotal, KeysDone: keysDone, KeysCached: keysCached})
	}
	statusByKey := make(map[string]string, len(groups))
	cachedByKey := make(map[string]bool, len(groups))
	if len(groups) > 0 {
		r := newResolver(opts.CacheDir)
		// Provenance rides a mutex-guarded side map keyed by the same
		// dedupe key BuildPlan used, so the FetchAll LookupFunc signature
		// (shared with metawarm's resolver.LookupNow) stays unchanged.
		var srcMu sync.Mutex
		srcByKey := make(map[string]bool, len(groups))
		lookup := func(lctx context.Context, q metadata.Query) (metadata.Resolution, error) {
			res, fromCache, err := r.LookupNowForceSrc(lctx, q, opts.Force)
			srcMu.Lock()
			srcByKey[DedupeKey(q)] = fromCache
			srcMu.Unlock()
			return res, err
		}
		total := len(plans)
		var pmu sync.Mutex
		done, resolved, cached, fetched := 0, 0, 0, 0
		var ckptMu sync.Mutex
		lastCkpt := time.Now()
		maybeCheckpoint := func() {
			if !indexed || ctx.Err() != nil {
				return
			}
			ckptMu.Lock()
			defer ckptMu.Unlock()
			if time.Since(lastCkpt) < checkpointEveryInterval {
				return
			}
			lastCkpt = time.Now()
			recordIndex(ix, visited, skipped, plans, opts.IndexPath, schema)
		}
		// No global gate (gateInterval 0): cache hits must never sleep.
		// Provider throttling is enforced per host inside metadata's HTTP
		// layer; the -concurrency semaphore below still bounds fan-out.
		// FetchAll keeps its gateInterval parameter for callers that opt
		// into an extra global floor (metawarm -gate, tests).
		progress := opts.Progress
		if progress != nil {
			progress(Progress{Done: done, Total: total, FoldersScanned: foldersScanned, FoldersSkipped: foldersSkipped,
				KeysTotal: keysTotal, KeysDone: keysDone, KeysCached: keysCached, KeysFetched: keysFetched, KeysNotFound: keysNotFound})
		}
		results := FetchAll(ctx, groups, lookup, defaultConcurrency, 0,
			func(g *Group, fr FetchResult) {
				srcMu.Lock()
				fromCache := srcByKey[g.Key]
				srcMu.Unlock()
				n := len(g.Paths)
				add, addCached, addFetched := 0, 0, 0
				if fr.Err == nil && fr.Res.Found {
					add = n
					if fromCache {
						addCached = n
					} else {
						addFetched = n
					}
				}
				pmu.Lock()
				done += n
				resolved += add
				cached += addCached
				fetched += addFetched
				if !cachedHit[g.Key] {
					if fr.Err != nil {
						// A cancelled fetch is incomplete, never a
						// miss: leave it out of KeysNotFound.
						if ctx.Err() == nil {
							keysNotFound++
						}
					} else if fr.Res.Found {
						if !fromCache {
							keysFetched++
						} else {
							// Stat missed but served from cache (e.g.
							// in-memory hit): credit as cached.
							keysCached++
							cachedHit[g.Key] = true
						}
					} else {
						keysNotFound++
					}
					keysDone = keysCached + keysFetched + keysNotFound
				} else {
					keysDone = keysCached + keysFetched + keysNotFound
				}
				p := Progress{Done: done, Total: total, Resolved: resolved, Cached: cached, Fetched: fetched, Last: g.Paths[n-1], FoldersScanned: foldersScanned, FoldersSkipped: foldersSkipped,
					KeysTotal: keysTotal, KeysDone: keysDone, KeysCached: keysCached, KeysFetched: keysFetched, KeysNotFound: keysNotFound}
				pmu.Unlock()
				maybeCheckpoint()
				if progress == nil {
					return
				}
				progress(p)
			})
		for k, fr := range results {
			srcMu.Lock()
			fr.FromCache = srcByKey[k]
			srcMu.Unlock()
			results[k] = fr
		}
		for _, g := range groups {
			r, ok := results[g.Key]
			switch {
			case !ok || r.Err != nil:
				// A fetch that never ran (!ok) or failed only because
				// the context was cancelled is incomplete, never an
				// error: it must not feed the error tally.
				if ctx.Err() != nil {
					statusByKey[g.Key] = "cancelled"
				} else {
					statusByKey[g.Key] = "error"
				}
			case r.Res.Found:
				statusByKey[g.Key] = "resolved"
				cachedByKey[g.Key] = r.FromCache
			default:
				statusByKey[g.Key] = "not-found"
			}
		}
	}
	statusOf := func(p FilePlan) string {
		if p.Skipped {
			return "skipped"
		}
		return statusByKey[p.Key]
	}
	for _, p := range plans {
		switch statusOf(p) {
		case "skipped":
			st.Skipped++
		case "resolved":
			st.Resolved++
			if cachedByKey[p.Key] {
				st.Cached++
			} else {
				st.Fetched++
			}
		case "not-found":
			st.NotFound++
		case "error":
			st.Errors++
		}
	}
	st.KeysCached = keysCached
	st.KeysFetched = keysFetched
	st.KeysNotFound = keysNotFound
	st.KeysDone = keysCached + keysFetched + keysNotFound
	if ctx.Err() != nil {
		st.Cancelled = true
	}
	// A cancelled run records nothing: the fetch may have resolved only
	// some keys, so any record would stamp a partial key list.
	if indexed && ctx.Err() == nil {
		recordIndex(ix, visited, skipped, plans, opts.IndexPath, schema)
	}
	return st, nil
}

// recordIndex aggregates per-directory cache keys and file counts
// bottom-up (walked files plus skipped-subtree totals) and re-records
// every visited dir, then prunes and saves. Save errors are logged,
// never fatal to the scan.
func recordIndex(ix *ScanIndex, visited []string, skipped map[string]FolderRecord, plans []FilePlan, indexPath string, schema int) {
	if ix == nil || indexPath == "" || len(visited) == 0 {
		if ix != nil && indexPath != "" {
			ix.PruneExisting()
			if err := ix.Save(indexPath); err != nil {
				fmt.Fprintf(os.Stderr, "library: save scan index %q: %v\n", indexPath, err)
			}
		}
		return
	}
	// Per-file cache keys for resolvable files.
	fileCacheKey := make(map[string]string, len(plans))
	for _, p := range plans {
		if p.Skipped {
			continue
		}
		fileCacheKey[p.Path] = metadata.CacheKeyFor(p.Query)
	}
	// Order visited deepest-first so children aggregate into parents.
	ordered := append([]string(nil), visited...)
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	type agg struct {
		files int
		keys  map[string]bool
		paths []string
	}
	aggs := make(map[string]*agg, len(ordered))
	get := func(norm string) *agg {
		a, ok := aggs[norm]
		if !ok {
			a = &agg{keys: make(map[string]bool)}
			aggs[norm] = a
		}
		return a
	}
	for _, p := range plans {
		dir := filepath.Dir(p.Path)
		norm := normalizeScanDir(dir)
		// Attribute the file to every visited ancestor (prefix match). The
		// record is derived from this plan data itself: per-dir paths are
		// kept so the stamp below can verify the filesystem still shows
		// exactly what was walked.
		for _, v := range visited {
			nv := normalizeScanDir(v)
			if norm == nv || strings.HasPrefix(norm, nv+string(filepath.Separator)) {
				a := get(nv)
				a.files++
				a.paths = append(a.paths, p.Path)
				if k, ok := fileCacheKey[p.Path]; ok && k != "" {
					a.keys[k] = true
				}
			}
		}
	}
	// Fold skipped subtrees into their visited ancestors.
	for snorm, rec := range skipped {
		for _, v := range visited {
			nv := normalizeScanDir(v)
			if snorm == nv || strings.HasPrefix(snorm, nv+string(filepath.Separator)) {
				a := get(nv)
				a.files += rec.Files
				for _, k := range rec.Keys {
					if k != "" {
						a.keys[k] = true
					}
				}
			}
		}
		_ = snorm
	}
	// Stamp each visited dir, but ONLY when the filesystem still shows
	// exactly what was walked. The hashes below come from a fresh listing,
	// so a file added or removed concurrently with the walk would otherwise
	// be stamped with hashes describing files absent from the plan (and a
	// key list missing them); the next scan would then skip files that were
	// never walked. Fail closed: same recursive video count, every walked
	// path still present, and every skipped subtree beneath still
	// fingerprint-identical; anything else is left unrecorded and simply
	// re-walked next time.
	freshCount := freshVideoCounts(visited)
	for _, v := range visited {
		nv := normalizeScanDir(v)
		a := get(nv)
		if freshCount[nv] != a.files {
			continue
		}
		verified := true
		for _, p := range a.paths {
			if _, err := os.Stat(p); err != nil {
				verified = false
				break
			}
		}
		if !verified {
			continue
		}
		for snorm, rec := range skipped {
			if snorm == nv || strings.HasPrefix(snorm, nv+string(filepath.Separator)) {
				fpNow, fpErr := FolderFingerprint(snorm)
				if fpErr != nil || fpNow != rec.Fingerprint {
					verified = false
					break
				}
			}
		}
		if !verified {
			continue
		}
		keys := make([]string, 0, len(a.keys))
		for k := range a.keys {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		nh, nhErr := FolderNamesHash(v)
		fp, fpErr := FolderFingerprint(v)
		if nhErr != nil || fpErr != nil {
			continue
		}
		ix.RecordFull(v, nh, fp, schema, keys, a.files)
		_ = nv
	}
	ix.PruneExisting()
	if err := ix.Save(indexPath); err != nil {
		fmt.Fprintf(os.Stderr, "library: save scan index %q: %v\n", indexPath, err)
	}
}

// freshVideoCounts walks each topmost visited dir once (via walkDir, like
// the main walk) and tallies the current recursive video-file count per
// visited ancestor. Unreadable entries are ignored, mirroring the walk;
// a dir that cannot be walked simply reports no fresh files (fail closed
// at the stamp check above).
func freshVideoCounts(visited []string) map[string]int {
	out := make(map[string]int, len(visited))
	if len(visited) == 0 {
		return out
	}
	sep := string(filepath.Separator)
	norms := make([]string, len(visited))
	for i, v := range visited {
		norms[i] = normalizeScanDir(v)
	}
	isTop := make([]bool, len(visited))
	for i := range visited {
		top := true
		for j := range visited {
			if i == j {
				continue
			}
			if norms[i] == norms[j] || strings.HasPrefix(norms[i], norms[j]+sep) {
				top = false
				break
			}
		}
		isTop[i] = top
	}
	for i, v := range visited {
		if !isTop[i] {
			continue
		}
		_ = walkDir(v, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				if d != nil && d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if !videoExts[strings.ToLower(filepath.Ext(path))] {
				return nil
			}
			dirNorm := normalizeScanDir(filepath.Dir(path))
			for _, nv := range norms {
				if dirNorm == nv || strings.HasPrefix(dirNorm, nv+sep) {
					out[nv]++
				}
			}
			return nil
		})
	}
	return out
}
