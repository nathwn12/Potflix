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
	"errors"
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
// confirmed missing (definitive miss only); KeysFailed counts uncached keys
// whose fetch failed transiently (metadata.ErrLookupUnavailable - timeout,
// 429, DNS, outage - unknown outcome, never a miss). A transient failure is
// never binned as not-found, so every key lands in exactly one bucket.
// Invariant (completed runs): KeysDone == KeysCached + KeysFetched + KeysNotFound + KeysFailed == Keys; a cancelled run leaves unfinished keys uncounted.
type Stats struct {
	Keys           int
	KeysCached     int
	KeysFetched    int
	KeysNotFound   int
	KeysFailed     int
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

	// Store-backed index counters (zero for a non-indexed run). Unchanged
	// counts walked files whose (size, mtime) matched their stored record -
	// the files the fast path never re-parsed; Parsed counts the files it
	// did re-parse (new, changed, or a forced Refresh); Changed counts
	// records replaced; Pruned counts records deleted because the file is
	// gone; Pending/NeedsRepair count the index's metadata states.
	Unchanged   int
	Parsed      int
	Changed     int
	Pruned      int
	Pending     int
	NeedsRepair int
}

// Progress is one live scan update. Done counts finished files against the
// walked Total; Resolved is the running resolved-file tally (split into
// running Cached/Fetched tallies); KeysTotal/KeysDone track unique-key work
// (KeysDone == KeysCached + KeysFetched + KeysNotFound + KeysFailed); Last
// is the most recent path. FoldersScanned/FoldersSkipped carry the index
// walk totals so the window status line can print
// "folders: N scanned, M skipped".
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
	KeysFailed     int
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
	// SkipCatchUp keeps an indexed run to its index phase: no background
	// metadata pass is triggered. Tests and benchmarks use it to measure the
	// walk+diff alone; the app leaves it false.
	SkipCatchUp bool
	// AppVersion stamps the needs-repair report the catch-up writes.
	AppVersion string
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

// cachedDir memoizes one os.ReadDir per directory plus one FileInfo per
// path for the duration of ONE recordIndex pass, so verification and the
// record-time re-hash share each listing instead of re-reading it. It is
// deliberately not shared across phases: the walk's skip decisions read
// fresh (freshNamesHash/freshFingerprint), and each recordIndex pass owns
// a new cachedDir, because reusing a listing across the traversal, the
// decision, the pre-fetch record and the final record would let one
// rewalked directory be decided, verified and stamped from progressively
// staler enumerations. The duplicate reads across phases are the cost of
// fail-closed verification (see collectIndexed and recordIndex).
type cachedDir struct {
	lists map[string][]os.DirEntry
	infos map[string]os.FileInfo
}

// list returns the directory's entries, reading them at most once.
// Failures are never cached, so a later retry re-reads.
func (c *cachedDir) list(path string) ([]os.DirEntry, error) {
	if c.lists != nil {
		if es, ok := c.lists[path]; ok {
			return es, nil
		}
	} else {
		c.lists = make(map[string][]os.DirEntry)
	}
	es, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	c.lists[path] = es
	return es, nil
}

// info returns the FileInfo for path, calling d.Info() (falling back to
// os.Stat when d is nil or Info fails) at most once per path.
func (c *cachedDir) info(path string, d os.DirEntry) (os.FileInfo, error) {
	if c.infos != nil {
		if fi, ok := c.infos[path]; ok {
			return fi, nil
		}
	} else {
		c.infos = make(map[string]os.FileInfo)
	}
	var fi os.FileInfo
	if d != nil {
		if f, err := d.Info(); err == nil {
			fi = f
		}
	}
	if fi == nil {
		f, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		fi = f
	}
	c.infos[path] = fi
	return fi, nil
}

// namesHash computes the tier-1 FolderNamesHash identity from the single
// cached listing (same core, identical record spelling).
func (c *cachedDir) namesHash(path string, modTime time.Time) (string, error) {
	es, err := c.list(path)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(es))
	for _, e := range es {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return hashNames(modTime.UnixNano(), len(names), names), nil
}

// fingerprint computes the tier-2 FolderFingerprint identity from the
// single cached listing (same core, identical record spelling).
// Sub-directory listings are read through the same cache, so a folder
// enumerated here as a parent's second level is not re-enumerated when
// the walk descends into it.
func (c *cachedDir) fingerprint(path string, modTime time.Time) (string, error) {
	es, err := c.list(path)
	if err != nil {
		return "", err
	}
	sorted := append([]os.DirEntry(nil), es...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name() < sorted[j].Name() })
	fps := make([]fpEntry, 0, len(sorted))
	for _, e := range sorted {
		ep := filepath.Join(path, e.Name())
		fi, err := c.info(ep, e)
		if err != nil {
			return "", err
		}
		fe := fpEntry{name: e.Name(), size: fi.Size(), mtimeUnixNano: fi.ModTime().UnixNano(), isDir: e.IsDir()}
		if e.IsDir() {
			sub, err := c.list(ep)
			if err != nil {
				return "", err
			}
			ssorted := append([]os.DirEntry(nil), sub...)
			sort.Slice(ssorted, func(i, j int) bool { return ssorted[i].Name() < ssorted[j].Name() })
			for _, s := range ssorted {
				sp := filepath.Join(ep, s.Name())
				sfi, err := c.info(sp, s)
				if err != nil {
					return "", err
				}
				fe.sub = append(fe.sub, fpSub{name: s.Name(), size: sfi.Size()})
			}
		}
		fps = append(fps, fe)
	}
	return hashFingerprint(modTime.UnixNano(), fps), nil
}

// freshNamesHash computes the tier-1 identity from one fresh os.ReadDir:
// a skip decision must never run on a listing cached in an earlier pass
// (this dir as its parent's fingerprint second level), which can predate a
// concurrent add and - with the dir mtime restored - still match the
// record. Same core and spelling as FolderNamesHash.
func freshNamesHash(path string, modTime time.Time) (string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return hashNames(modTime.UnixNano(), len(names), names), nil
}

// freshFingerprint computes the tier-2 identity from one fresh os.ReadDir
// plus fresh stats, mirroring FolderFingerprint exactly (same core, same
// field order): every input is read now, never from an earlier pass's
// cache, so a concurrent change between the parent's fingerprint and this
// dir's decision refuses the skip instead of trusting stale names.
func freshFingerprint(path string, modTime time.Time) (string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	fps := make([]fpEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			return "", err
		}
		fe := fpEntry{name: e.Name(), size: info.Size(), mtimeUnixNano: info.ModTime().UnixNano(), isDir: e.IsDir()}
		if e.IsDir() {
			sub, err := os.ReadDir(filepath.Join(path, e.Name()))
			if err != nil {
				return "", err
			}
			sort.Slice(sub, func(i, j int) bool { return sub[i].Name() < sub[j].Name() })
			for _, s := range sub {
				sinfo, err := s.Info()
				if err != nil {
					return "", err
				}
				fe.sub = append(fe.sub, fpSub{name: s.Name(), size: sinfo.Size()})
			}
		}
		fps = append(fps, fe)
	}
	return hashFingerprint(modTime.UnixNano(), fps), nil
}

// walkDirInfo carries one visited directory's walk-time hash identities
// into recordIndex so recording reuses them instead of re-hashing. Empty
// hashes mean the dir was never hashed in this pass (force walks, or a
// hash doubt); recordIndex then hashes it fresh from a single listing
// instead of a second full walk.
type walkDirInfo struct {
	namesHash   string
	fingerprint string
}

// scanWalk is one collectIndexed pass's per-directory hash state, keyed by
// normalized dir.
type scanWalk struct {
	dirs map[string]walkDirInfo
}

// collectIndexed walks each root for video files, consulting ix when
// enabled. It returns the walked files (skipped subtrees excluded), the
// visited (non-skipped) directories, the skipped records by normalized dir,
// this pass's per-directory hash state for recordIndex (nil unless indexed),
// and the folder/file tallies. force disables all skipping but still
// visits every dir for recording. Any hash doubt (stat/read errors) walks
// the folder normally. Tier-2 hits refresh the record's tier-1 hash.
// ctx cancels the walk promptly: the walk callback checks ctx.Err() and
// aborts, and collectIndexed reports cancelled=true with partial results
// (not an error). When indexed, the index is checkpointed every
// checkpointEveryFolders visited folders or every checkpointEveryInterval,
// whichever comes first, via SaveIfDirty (same cadence, a write only when
// records changed), so an interrupted scan keeps what was walked so far.
//
// Disk discipline: the traversal itself is WalkDir (its own enumeration,
// which the walk cannot reuse because a dir's skip decision is due before
// its children arrive), each skip decision reads its own directory fresh
// (tier-1 names, tier-2 fingerprint only on a tier-1 miss), and each
// recordIndex pass re-lists every visited dir for verification. The
// re-enumeration across phases is deliberate: sharing a listing across a
// skip decision, the pre-fetch record and the final record would let a
// concurrent add slip a stamp whose hashes the next scan trusts, so every
// phase reads current filesystem state (see freshNamesHash and the
// recordIndex verification below).
func collectIndexed(ctx context.Context, roots []string, ix *ScanIndex, useIndex bool, schema int, keyPresent func(string) bool, force bool, indexPath string, onFile func(path string, size, mtimeUnixNano int64)) (files []string, visited []string, skipped map[string]FolderRecord, walk *scanWalk, foldersScanned, foldersSkipped, filesSkipped, denied int, firstErr error, cancelled bool) {
	skipped = make(map[string]FolderRecord)
	enabled := useIndex && !force && ix != nil
	indexed := useIndex && indexPath != "" && ix != nil
	if indexed {
		walk = &scanWalk{dirs: make(map[string]walkDirInfo)}
	}
	var okRoots int
	lastFlush := time.Now()
	lastFlushCount := 0
	flush := func() {
		if !indexed {
			return
		}
		lastFlush = time.Now()
		lastFlushCount = len(visited)
		if err := ix.SaveIfDirty(indexPath); err != nil {
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
					// One os.Stat for the dir's mtime plus one fresh
					// listing per tier, in the exact historical skip
					// order: names hash first, fingerprint on a
					// tier-1 miss. Both tiers read the filesystem
					// fresh at decision time, never a listing cached
					// in an earlier pass: this dir may already sit
					// in the cache as its parent's fingerprint
					// second level, and that listing can predate a
					// concurrent add - with the dir mtime restored
					// the stale names still match, so the skip
					// would hide the new files (the old
					// FolderNamesHash read fresh names and
					// refused). Reading fresh once is fail-closed
					// by construction; a re-verify-on-hit would
					// cost the same extra listing on every steady-
					// state skip without removing the stale input.
					// Any hash doubt walks the folder normally.
					var nh, fp string
					if fi, err := os.Stat(path); err == nil && fi.IsDir() {
						if h, err := freshNamesHash(path, fi.ModTime()); err == nil {
							nh = h
							if ix.ShouldSkipNames(path, nh, schema, keyPresent) {
								if rec, ok := ix.Lookup(path); ok {
									filesSkipped += rec.Files
									skipped[normalizeScanDir(path)] = rec
								}
								foldersSkipped++
								return filepath.SkipDir
							}
						}
						if h, err := freshFingerprint(path, fi.ModTime()); err == nil {
							fp = h
							if ix.ShouldSkip(path, fp, schema, keyPresent) {
								rec, ok := ix.Lookup(path)
								if ok {
									filesSkipped += rec.Files
									skipped[normalizeScanDir(path)] = rec
									// Refresh tier 1: benign dir-mtime
									// bump with identical contents. The
									// names hash above missed, so it is
									// current; re-record from this
									// pass's fresh hashes (no
									// recompute) so the next scan hits
									// tier 1.
									ix.RecordFull(path, nh, fp, schema, rec.Keys, rec.Files)
								}
								foldersSkipped++
								return filepath.SkipDir
							}
						}
					}
					if walk != nil {
						walk.dirs[normalizeScanDir(path)] = walkDirInfo{namesHash: nh, fingerprint: fp}
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
				if onFile != nil {
					// One more listener on the same enumeration the walk
					// already paid for: the dir entry's own FileInfo, so
					// the per-file diff gets (size, mtime) without a
					// second stat of every walked file.
					if d != nil {
						if fi, err := d.Info(); err == nil {
							onFile(path, fi.Size(), fi.ModTime().UnixNano())
						}
					}
				}
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
		return nil, nil, skipped, walk, foldersScanned, foldersSkipped, filesSkipped, denied, firstErr, cancelled
	}
	return files, visited, skipped, walk, foldersScanned, foldersSkipped, filesSkipped, denied, nil, cancelled
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
		// One read per cache key per walk: the tier-1 and tier-2
		// guards share this memo instead of each re-reading the
		// same cache file.
		memo := make(map[string]bool)
		keyPresent = func(key string) bool {
			if v, ok := memo[key]; ok {
				return v
			}
			v := metadata.CacheLive(cacheDir, key)
			memo[key] = v
			return v
		}
	}
	files, visited, skipped, _, fs, fk, fsk, denied, err, cancelled := collectIndexed(ctx, roots, ix, indexed, schema, keyPresent, force, indexPath, nil)
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
// the walked plans plus skipped-subtree totals) and saves when anything
// changed. It is a no-op for an empty indexPath or with no visited dirs
// (still saves when pruning changed the index). Save errors
// are logged, never fatal. Without walk state every visited dir is hashed
// fresh from a single listing; staleness is still refused (see
// recordIndex).
func RecordScan(indexPath string, visited []string, skipped map[string]FolderRecord, plans []FilePlan) {
	if indexPath == "" {
		return
	}
	ix := LoadScanIndex(indexPath)
	recordIndex(ix, visited, skipped, plans, indexPath, metadata.CacheSchema(), nil)
}

// ScanWithOptions is Scan with an opt-in folder index. With UseIndex off
// (or an empty IndexPath) it walks everything, exactly like Scan.
// With UseIndex on: force=false consults the index with the two-tier skip
// (tier-1 FolderNamesHash, tier-2 FolderFingerprint, both guarded by
// metadata.CacheLive per recorded key); force=true ignores the index for
// skipping but still rewrites every visited record. A missing/corrupt
// index file is a full walk, never an error. On success the index is
// pruned in the scan write path and saved when changed; a save failure is logged
// to stderr but does not fail the scan.
//
// Checkpointing: when indexed, the walk checkpoints the index every
// checkpointEveryFolders visited folders or every checkpointEveryInterval
// via SaveIfDirty (same cadence, a write only when records changed). The walk-phase checkpoint only persists
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
	// Indexed runs take the store-backed enumerate+diff path (scan.go): one
	// walk, a per-file (size, mtime) diff, zero fetches, then the background
	// catch-up. The body below is the historical non-indexed fetch path,
	// which the index-off switch, the CLI and the tests still use verbatim.
	if opts.UseIndex && opts.IndexPath != "" {
		return scanIndexed(ctx, opts)
	}
	indexed := opts.UseIndex && opts.IndexPath != ""
	var ix *ScanIndex
	schema := metadata.CacheSchema()
	var keyPresent func(string) bool
	// scanResolver backs the index guard and the pre-count below with one
	// shared view: the first touch of a cache key probes its file
	// (ProbeCache warms the resolver's memory for live entries only)
	// and every later touch in this scan is served from memory,
	// revalidated against deletion. liveMemo is that shared verdict map,
	// covering the guard's recorded keys and the pre-count's group keys
	// alike. A live entry's file is read once; a missing, corrupt,
	// expired or oversize entry warms nothing, so the fetch-phase lookup
	// re-reads it from disk (two reads for a miss) and the disk stays the
	// source of truth.
	var scanResolver *metadata.Resolver
	var liveMemo map[string]bool
	if indexed {
		ix = LoadScanIndex(opts.IndexPath)
		scanResolver = newResolver(opts.CacheDir)
		liveMemo = make(map[string]bool)
		keyPresent = func(key string) bool {
			if v, ok := liveMemo[key]; ok {
				return v
			}
			v := scanResolver.ProbeCache(key)
			liveMemo[key] = v
			return v
		}
	}
	files, visited, skipped, walk, foldersScanned, foldersSkipped, filesSkipped, denied, walkErr, walkCancelled := collectIndexed(ctx, opts.Dirs, ix, indexed, schema, keyPresent, opts.Force, opts.IndexPath, nil)
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
		recordIndex(ix, visited, skipped, plans, opts.IndexPath, schema, walk)
	}
	st := Stats{Files: len(plans), Unique: len(groups), Denied: denied,
		FoldersScanned: foldersScanned, FoldersSkipped: foldersSkipped, FilesSkipped: filesSkipped,
		Keys: len(groups)}
	keysTotal := len(groups)
	keysCached := 0
	cachedHit := make(map[string]bool, len(groups))
	if !opts.Force {
		for _, g := range groups {
			// One shared probe per distinct cache key: guard and
			// pre-count verdicts are memoised; live entries are
			// served from warmed memory in the fetch phase,
			// misses re-read from disk (see above).
			ck := metadata.CacheKeyFor(g.Query)
			var live bool
			if scanResolver != nil {
				if v, ok := liveMemo[ck]; ok {
					live = v
				} else {
					live = scanResolver.ProbeCache(ck)
					liveMemo[ck] = live
				}
			} else {
				live = metadata.CacheLive(opts.CacheDir, ck)
			}
			if live {
				keysCached++
				cachedHit[g.Key] = true
			}
		}
	}
	keysFetched, keysNotFound, keysFailed := 0, 0, 0
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
		r := scanResolver
		if r == nil {
			r = newResolver(opts.CacheDir)
		}
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
			recordIndex(ix, visited, skipped, plans, opts.IndexPath, schema, walk)
		}
		// No global gate (gateInterval 0): cache hits must never sleep.
		// Provider throttling is enforced per host inside metadata's HTTP
		// layer; the -concurrency semaphore below still bounds fan-out.
		// FetchAll keeps its gateInterval parameter for callers that opt
		// into an extra global floor (metawarm -gate, tests).
		progress := opts.Progress
		if progress != nil {
			progress(Progress{Done: done, Total: total, FoldersScanned: foldersScanned, FoldersSkipped: foldersSkipped,
				KeysTotal: keysTotal, KeysDone: keysDone, KeysCached: keysCached, KeysFetched: keysFetched, KeysNotFound: keysNotFound, KeysFailed: keysFailed})
		}
		results := make(map[string]FetchResult, len(groups))
		onDone := func(g *Group, fr FetchResult) {
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
					// miss: leave it out of every bucket. A
					// transient provider failure is unknown,
					// never a miss: it gets its own bucket, never
					// KeysNotFound.
					if ctx.Err() == nil {
						if errors.Is(fr.Err, metadata.ErrLookupUnavailable) {
							keysFailed++
						} else {
							keysNotFound++
						}
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
				keysDone = keysCached + keysFetched + keysNotFound + keysFailed
			} else {
				keysDone = keysCached + keysFetched + keysNotFound + keysFailed
			}
			p := Progress{Done: done, Total: total, Resolved: resolved, Cached: cached, Fetched: fetched, Last: g.Paths[n-1], FoldersScanned: foldersScanned, FoldersSkipped: foldersSkipped,
				KeysTotal: keysTotal, KeysDone: keysDone, KeysCached: keysCached, KeysFetched: keysFetched, KeysNotFound: keysNotFound, KeysFailed: keysFailed}
			pmu.Unlock()
			maybeCheckpoint()
			if progress == nil {
				return
			}
			progress(p)
		}
		// Pre-credited cache hits resolve synchronously from the
		// probe-warmed resolver: no network, no semaphore, no fetch.
		// ResolveCached never spawns: a hit that no longer resolves
		// from the cache (stale entry missing this episode, or a
		// vanished file un-warmed by probedLive) falls back to the
		// fetch list with its pre-credit kept, exactly as if its
		// lookup had missed the cache. The stale keys then fetch
		// through FetchAll under the semaphore with scan-context
		// timeouts, instead of fanning out one unbounded,
		// uncancellable background fetch per stale key (Resolve
		// spawns outside the semaphore with context.Background()).
		var missGroups []*Group
		for _, g := range groups {
			if !cachedHit[g.Key] {
				missGroups = append(missGroups, g)
				continue
			}
			if ctx.Err() != nil {
				break
			}
			if res, ok := r.ResolveCached(g.Query); ok && res.Found {
				srcMu.Lock()
				srcByKey[g.Key] = true
				srcMu.Unlock()
				fr := FetchResult{Res: res, Err: nil, FromCache: true}
				results[g.Key] = fr
				onDone(g, fr)
				continue
			}
			missGroups = append(missGroups, g)
		}
		for k, fr := range FetchAll(ctx, missGroups, lookup, defaultConcurrency, 0, onDone) {
			results[k] = fr
		}
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
	st.KeysFailed = keysFailed
	st.KeysDone = keysCached + keysFetched + keysNotFound + keysFailed
	if ctx.Err() != nil {
		st.Cancelled = true
	}
	// A cancelled run records nothing: the fetch may have resolved only
	// some keys, so any record would stamp a partial key list.
	if indexed && ctx.Err() == nil {
		recordIndex(ix, visited, skipped, plans, opts.IndexPath, schema, walk)
	}
	return st, nil
}

// recordIndex aggregates per-directory cache keys and file counts
// bottom-up (walked files plus skipped-subtree totals) and re-records
// every visited dir, then saves when anything changed. Save errors are
// logged, never fatal to the scan.
//
// Attribution is one ancestor walk per plan/skipped record (not one full
// visited scan per plan), and verification re-lists each visited dir once
// plus any new subdirectories found beneath (recursive totals descend
// through every fresh subdirectory, skipped subtrees excepted;
// walked-path presence is listing membership, never per-plan stats).
// A dir is stamped only when the filesystem still shows exactly what was
// walked: same recursive video count, every walked path still present, and
// every skipped subtree beneath still fingerprint-identical; anything else
// is left unrecorded and simply re-walked next time.
//
// Hashes come from the walk when carried (walk != nil with both hashes
// present): the carried pair describes the same walk the key list derives
// from, so no re-hash and no second walk is needed. Otherwise the dir is
// hashed fresh from its single verification listing. Either way a
// concurrent add/remove/rename refuses the stamp (fail closed), so the
// next scan re-walks instead of trusting a partial key list.
func recordIndex(ix *ScanIndex, visited []string, skipped map[string]FolderRecord, plans []FilePlan, indexPath string, schema int, walk *scanWalk) {
	// Per-file cache keys for resolvable files.
	fileCacheKey := make(map[string]string, len(plans))
	for _, p := range plans {
		if p.Skipped {
			continue
		}
		fileCacheKey[p.Path] = metadata.CacheKeyFor(p.Query)
	}
	recordIndexKeys(ix, visited, skipped, fileCacheKey, indexPath, schema, walk)
}

// recordIndexKeys is recordIndex with the per-file metadata key map supplied
// directly, so the store-backed scan can stamp rollups from the keys its file
// records already carry instead of re-deriving them from a parsed plan.
func recordIndexKeys(ix *ScanIndex, visited []string, skipped map[string]FolderRecord, fileCacheKey map[string]string, indexPath string, schema int, walk *scanWalk) {
	if ix != nil && indexPath != "" {
		// Scan write path only: drop records for deleted folders while
		// scanning live roots, so a save never resurrects them.
		ix.PruneExisting()
	}
	if ix == nil || indexPath == "" || len(visited) == 0 {
		if ix != nil && indexPath != "" {
			if err := ix.SaveIfDirty(indexPath); err != nil {
				fmt.Fprintf(os.Stderr, "library: save scan index %q: %v\n", indexPath, err)
			}
		}
		return
	}
	// Dedupe visited by normalized dir, first spelling wins.
	normOf := make(map[string]string, len(visited))
	norms := make([]string, 0, len(visited))
	for _, v := range visited {
		nv := normalizeScanDir(v)
		if _, ok := normOf[nv]; !ok {
			normOf[nv] = v
			norms = append(norms, nv)
		}
	}
	// parentNorm walks one level up; "" terminates at the filesystem root.
	parentNorm := func(nv string) string {
		par := normalizeScanDir(filepath.Dir(nv))
		if par == nv {
			return ""
		}
		return par
	}
	type dirAgg struct {
		files int
		keys  map[string]bool
		paths []string
	}
	aggs := make(map[string]*dirAgg, len(norms))
	for _, nv := range norms {
		aggs[nv] = &dirAgg{keys: make(map[string]bool)}
	}
	// addToAncestors attributes one file bundle to a dir and every visited
	// ancestor-or-self: the old prefix match without the O(plans x
	// visited) scan. The record derives from this plan data itself:
	// per-dir paths are kept so the stamp below can verify the filesystem
	// still shows exactly what was walked.
	addToAncestors := func(nv string, files int, keys []string, path string) {
		for a := nv; a != ""; a = parentNorm(a) {
			if agg, ok := aggs[a]; ok {
				agg.files += files
				for _, k := range keys {
					if k != "" {
						agg.keys[k] = true
					}
				}
				if path != "" {
					agg.paths = append(agg.paths, path)
				}
			}
		}
	}
	for path, k := range fileCacheKey {
		dir := normalizeScanDir(filepath.Dir(path))
		var keys []string
		if k != "" {
			keys = []string{k}
		}
		addToAncestors(dir, 1, keys, path)
	}
	// Fold skipped subtrees into their visited ancestors, and remember
	// which skipped record each visited dir depends on for the
	// fingerprint re-verify below.
	depend := make(map[string][]string)
	for snorm, rec := range skipped {
		addToAncestors(snorm, rec.Files, rec.Keys, "")
		for a := snorm; a != ""; a = parentNorm(a) {
			if _, ok := aggs[a]; ok {
				depend[a] = append(depend[a], snorm)
			}
		}
	}
	// Stamp each visited dir, but ONLY when the filesystem still shows
	// exactly what was walked. Fail closed: same recursive video count
	// (recounted now by descending through every fresh subdirectory,
	// visited or not - never rolled up from visited immediates alone),
	// every walked path still present (listing membership, never per-plan
	// stats), and every skipped subtree beneath still
	// fingerprint-identical (verified once per skipped dir, not once per
	// visited ancestor); anything else is left unrecorded and simply
	// re-walked next time.
	dc := &cachedDir{}
	freshNames := make(map[string]map[string]bool, len(norms))
	badList := make(map[string]bool)
	for _, nv := range norms {
		es, err := dc.list(normOf[nv])
		if err != nil {
			// Unreadable now: fail closed below (matches the old
			// re-hash doubt, which also refused the stamp).
			badList[nv] = true
			continue
		}
		set := make(map[string]bool, len(es))
		for _, e := range es {
			set[e.Name()] = true
		}
		freshNames[nv] = set
	}
	// Fresh recursive totals descend through EVERY fresh subdirectory -
	// visited or not - so a video added anywhere under a visited dir
	// after the walk refuses the stamp. Rolling only visited immediates
	// up keeps the walked count while the stamped names hash already
	// covers the new names (e.g. a brand-new New/B.mkv: count stays 1,
	// hash includes New), and the next scan would skip the new files.
	// Skipped subtrees contribute their recorded counts (their
	// fingerprints are re-verified per skipped dir below, so a changed
	// skip refuses too). Any unreadable fresh directory poisons its
	// visited ancestors: fail closed.
	freshRec := make(map[string]int, len(norms))
	poisoned := make(map[string]bool)
	memoRec := make(map[string]int)
	var recCount func(nv, path string) (int, bool)
	recCount = func(nv, path string) (int, bool) {
		if v, ok := memoRec[nv]; ok {
			return v, true
		}
		es, err := dc.list(path)
		if err != nil {
			return 0, false
		}
		total := 0
		for _, e := range es {
			if e.IsDir() {
				cn := normalizeScanDir(filepath.Join(path, e.Name()))
				if rec, ok := skipped[cn]; ok {
					total += rec.Files
					continue
				}
				sub, ok := recCount(cn, filepath.Join(path, e.Name()))
				if !ok {
					return 0, false
				}
				total += sub
				continue
			}
			if videoExts[strings.ToLower(filepath.Ext(e.Name()))] {
				total++
			}
		}
		memoRec[nv] = total
		return total, true
	}
	for _, nv := range norms {
		if badList[nv] {
			continue
		}
		if tot, ok := recCount(nv, normOf[nv]); ok {
			freshRec[nv] = tot
		} else {
			poisoned[nv] = true
		}
	}
	skipFP := make(map[string]string, len(skipped))
	for snorm := range skipped {
		if fp, err := FolderFingerprint(snorm); err == nil {
			skipFP[snorm] = fp
		}
	}
	for _, nv := range norms {
		v := normOf[nv]
		a := aggs[nv]
		if badList[nv] {
			continue
		}
		if poisoned[nv] {
			continue
		}
		if freshRec[nv] != a.files {
			continue
		}
		verified := true
		for _, p := range a.paths {
			set, ok := freshNames[normalizeScanDir(filepath.Dir(p))]
			if !ok || !set[filepath.Base(p)] {
				verified = false
				break
			}
		}
		if !verified {
			continue
		}
		for _, sn := range depend[nv] {
			rec := skipped[sn]
			fpNow, ok := skipFP[sn]
			if !ok || fpNow != rec.Fingerprint {
				verified = false
				break
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
		var nh, fp string
		if walk != nil {
			if info, ok := walk.dirs[nv]; ok && info.namesHash != "" && info.fingerprint != "" {
				nh, fp = info.namesHash, info.fingerprint
			}
		}
		if nh == "" || fp == "" {
			// No carried hashes (RecordScan callers, force walks, or a
			// walk-time hash doubt): hash fresh from the single
			// verification listing already read above, reusing visited
			// sub-listings instead of a second full walk.
			fi, err := os.Stat(v)
			if err != nil || !fi.IsDir() {
				continue
			}
			nh, err = dc.namesHash(v, fi.ModTime())
			if err != nil {
				continue
			}
			fp, err = dc.fingerprint(v, fi.ModTime())
			if err != nil {
				continue
			}
		}
		ix.RecordFull(v, nh, fp, schema, keys, a.files)
	}
	if err := ix.SaveIfDirty(indexPath); err != nil {
		fmt.Fprintf(os.Stderr, "library: save scan index %q: %v\n", indexPath, err)
	}
}
