// catchup.go is the background metadata catch-up: after a scan has indexed
// the tree, and at every launch, this worker resolves whatever metadata the
// index still owes - pending keys, and needs_repair entries whose cooldown
// has expired - and writes the needs-repair inventory report.
//
// It is deliberately separate from the scan. The scan answers "what is on
// disk" in one walk with no network; metadata is slow, rate-limited and
// mostly cached, so it runs here, three lookups at a time, off the UI thread,
// and resumes where the last pass stopped, because the pending set lives in
// the index rather than in a goroutine's memory.
//
// Idempotence: the work set is derived from the index's own state on every
// pass, so starting a pass twice (the launch hook plus a scan trigger, or two
// launches) resolves each key once and cannot double-count. A key already
// settled is not work; a key in a repair cooldown is not work until its
// NextTry passes.
package library

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"potflix/internal/metadata"
	"potflix/internal/repair"
)

// CatchUpConfig is one catch-up request. Dirs are the configured library
// roots: a pending file that no longer exists under one of them is pruned
// instead of resolved.
type CatchUpConfig struct {
	Dirs       []string
	CacheDir   string
	IndexPath  string
	Force      bool
	AppVersion string

	// Ctx, when set, stops the pass when it is done. A caller that owns a
	// PASS-LEVEL lifetime (the CLI, a test) may set it; the scan must not:
	// its own context ends with the walk (window.go cancels it on every scan
	// landing), so a pass bound to it is cancelled the moment it starts -
	// see the hand-off in scanIndexed. Nil means the pass runs until it
	// finishes or StopCatchUp cancels it.
	Ctx context.Context
}

// CatchUpStats reports the worker's cumulative work, for logs and tests.
type CatchUpStats struct {
	Passes       int
	Lookups      int // keys handed to the resolver
	Resolved     int // keys that ended a pass with usable metadata
	Missed       int // keys with no usable answer
	Failed       int // keys whose lookup failed transiently
	NeedsRepair  int // file records parked as needs_repair
	FilesApplied int // file records rewritten by a verdict
}

// CatchUp is one store's catch-up worker. The zero value is not usable; use
// StartCatchUp.
type CatchUp struct {
	mu      sync.Mutex
	cfg     CatchUpConfig
	running bool
	cancel  context.CancelFunc
	done    chan struct{}
	cb      *progressSink

	passes, lookups, resolved, missed, failed, repair, applied int
}

// progressSink carries the live progress callback behind a pointer swapped
// under CatchUp.mu, so a scan can install (or replace) a callback without
// blocking the worker.
type progressSink struct{ fn func(Progress) }

var (
	catchMu   sync.Mutex
	catchLive = map[string]*CatchUp{}
)

// StartCatchUp ensures the catch-up worker for cfg.IndexPath is running with
// progressCb installed (nil keeps the current callback) and returns
// immediately - the pass runs on its own goroutine and never blocks the UI
// thread. A pass already in flight is never duplicated, so calling this twice
// resolves nothing twice. It returns nil when there is no index path.
func StartCatchUp(cfg CatchUpConfig, progressCb func(Progress)) *CatchUp {
	if cfg.IndexPath == "" {
		return nil
	}
	key := IndexStorePath(cfg.IndexPath)
	catchMu.Lock()
	c := catchLive[key]
	if c == nil {
		c = &CatchUp{}
		catchLive[key] = c
	}
	c.mu.Lock()
	c.cfg = cfg
	if progressCb != nil {
		c.cb = &progressSink{fn: progressCb}
	}
	if c.running {
		c.mu.Unlock()
		catchMu.Unlock()
		return c
	}
	c.running = true
	ctx, cancel := context.WithCancel(context.Background())
	var detach func() bool
	if cfg.Ctx != nil {
		// The caller's context (a scan's) also stops this pass, so a UI
		// Cancel ends the metadata work, not just the walk.
		detach = context.AfterFunc(cfg.Ctx, cancel)
	}
	c.cancel = cancel
	c.done = make(chan struct{})
	done := c.done
	c.mu.Unlock()
	catchMu.Unlock()

	go func() {
		c.pass(ctx)
		if detach != nil {
			detach()
		}
		// Clear the running flag BEFORE closing done: a caller that waits
		// then starts again must see a finished worker, never a "running"
		// one whose channel is already closed (which would return the
		// finished pass's counters for a pass that never ran).
		c.mu.Lock()
		c.running = false
		c.cancel = nil
		c.mu.Unlock()
		close(done)
	}()
	return c
}

// Wait blocks until the worker's current pass finishes (it returns at once
// when no pass is running).
func (c *CatchUp) Wait() {
	if c == nil {
		return
	}
	c.mu.Lock()
	done, running := c.done, c.running
	c.mu.Unlock()
	if running && done != nil {
		<-done
	}
}

// Stats reports the worker's cumulative counters.
func (c *CatchUp) Stats() CatchUpStats {
	if c == nil {
		return CatchUpStats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return CatchUpStats{
		Passes: c.passes, Lookups: c.lookups, Resolved: c.resolved,
		Missed: c.missed, Failed: c.failed, NeedsRepair: c.repair, FilesApplied: c.applied,
	}
}

// StopCatchUp cancels the running pass for indexPath (every pass when
// indexPath is empty) and waits for it to unwind: the cancel path commits
// what it applied and writes no report.
func StopCatchUp(indexPath string) {
	catchMu.Lock()
	var targets []*CatchUp
	if indexPath == "" {
		for _, c := range catchLive {
			targets = append(targets, c)
		}
	} else if c := catchLive[IndexStorePath(indexPath)]; c != nil {
		targets = append(targets, c)
	}
	catchMu.Unlock()
	for _, c := range targets {
		c.mu.Lock()
		cancel := c.cancel
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		c.Wait()
	}
}

// post hands one progress snapshot to the installed callback, if any.
func (c *CatchUp) post(p Progress) {
	c.mu.Lock()
	sink := c.cb
	c.mu.Unlock()
	if sink == nil || sink.fn == nil {
		return
	}
	sink.fn(p)
}

// keyAgg aggregates one metadata key's files for a pass.
type keyAgg struct {
	query     metadata.Query
	paths     []string
	unsettled bool
	nextTry   int64
}

// passTally is one pass's counters.
type passTally struct {
	files, keys, keysResolved       int
	cached, fetched, missed, failed int
	applied, repair                 int
	lastPath                        string
}

// pass is one catch-up run: recompute the pending set from the index, resolve
// it three keys at a time, apply each verdict as it lands, then write the
// inventory report.
func (c *CatchUp) pass(ctx context.Context) {
	c.mu.Lock()
	cfg := c.cfg
	c.mu.Unlock()

	idx, err := sharedIndex(cfg.IndexPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "library: catch-up %q: %v (pass continues in memory)\n", cfg.IndexPath, err)
	}
	if idx == nil {
		return
	}
	resolver := newResolver(cfg.CacheDir)
	now := time.Now()

	// The work set comes from the index's own state. Each distinct key is
	// probed once: ProbeCache validates the cache body and warms the
	// resolver's memory, so the lookup that follows is served from memory.
	live := make(map[string]bool, 1024)
	probe := func(key string) bool {
		if v, ok := live[key]; ok {
			return v
		}
		v := resolver.ProbeCache(key)
		live[key] = v
		return v
	}

	records := idx.filesSnapshot()
	var t passTally
	t.files = len(records)
	aggs := make(map[string]*keyAgg, 256)
	byPath := make(map[string]string, len(records))
	pruned := 0
	for i := range records {
		rec := &records[i]
		if rec.MetaKey == "" {
			continue // unknown kind: nothing to look up
		}
		if rec.State != StateResolved {
			if !underRoots(cfg.Dirs, rec.Path) {
				continue
			}
			if !fileExists(rec.Path) {
				// Vanished since the last scan; the walk's prune missed it
				// only because its folder was skipped.
				if idx.deleteFile(rec.Path) {
					pruned++
				}
				continue
			}
		}
		agg := aggs[rec.MetaKey]
		if agg == nil {
			agg = &keyAgg{query: rec.Query()}
			aggs[rec.MetaKey] = agg
		}
		agg.paths = append(agg.paths, rec.Path)
		byPath[normalizeScanDir(rec.Path)] = rec.MetaKey
		if rec.State == StateNeedsRepair && rec.NextTry > agg.nextTry {
			agg.nextTry = rec.NextTry
		}
		if rec.State == StateResolved && probe(rec.MetaKey) {
			continue
		}
		// Pending, or resolved with a payload that is no longer live.
		agg.unsettled = true
	}

	t.keys = len(aggs)
	t.files -= pruned
	groups := make([]*Group, 0, len(aggs))
	for key, agg := range aggs {
		if !agg.unsettled {
			t.keysResolved++
			continue
		}
		if !cfg.Force && agg.nextTry > 0 && now.UnixNano() < agg.nextTry {
			continue // needs_repair, still cooling down
		}
		groups = append(groups, &Group{Key: key, Query: agg.query, Paths: agg.paths})
	}
	if len(groups) == 0 {
		c.finishPass(idx, cfg, t)
		return
	}

	c.mu.Lock()
	c.passes++
	c.lookups += len(groups)
	c.mu.Unlock()

	var mu sync.Mutex // guards the pass counters and lastPath
	var srcMu sync.Mutex
	srcByKey := make(map[string]bool, len(groups))
	lookup := func(lctx context.Context, q metadata.Query) (metadata.Resolution, error) {
		res, fromCache, err := resolver.LookupNowForceSrc(lctx, q, cfg.Force)
		srcMu.Lock()
		srcByKey[DedupeKey(q)] = fromCache
		srcMu.Unlock()
		return res, err
	}
	commitGate := time.Now()
	apply := func(key string, agg *keyAgg, res metadata.Resolution, lerr error) {
		// A lookup that ended because the pass was cancelled (or its timeout
		// fired while the caller was going away) is INCOMPLETE, never a
		// failure: it counts nowhere and writes no verdict, so a cancel can
		// never invent attempts or needs_repair entries.
		if lerr != nil && (errors.Is(lerr, context.Canceled) || errors.Is(lerr, context.DeadlineExceeded) || ctx.Err() != nil) {
			mu.Lock()
			p := Progress{
				Done: t.files, Total: t.files,
				Resolved: t.keysResolved, KeysTotal: t.keys,
				KeysDone:    t.cached + t.fetched + t.missed + t.failed,
				KeysCached:  t.cached,
				KeysFetched: t.fetched, KeysNotFound: t.missed, KeysFailed: t.failed,
				Last: t.lastPath,
			}
			mu.Unlock()
			c.post(p)
			return
		}
		srcMu.Lock()
		fromCache := srcByKey[DedupeKey(agg.query)]
		srcMu.Unlock()
		switch {
		case lerr != nil:
			mu.Lock()
			t.failed++
			mu.Unlock()
		case !resolvedUsable(res):
			mu.Lock()
			t.missed++
			mu.Unlock()
		case fromCache:
			mu.Lock()
			t.cached++
			t.keysResolved++
			mu.Unlock()
		default:
			mu.Lock()
			t.fetched++
			t.keysResolved++
			mu.Unlock()
		}
		applied, repairN, path := idx.applyVerdict(key, byPath, res, lerr, time.Now())
		mu.Lock()
		t.applied += applied
		t.repair += repairN
		if path != "" {
			t.lastPath = path
		}
		p := Progress{
			Done: t.files, Total: t.files,
			Resolved: t.keysResolved, KeysTotal: t.keys,
			KeysDone:    t.cached + t.fetched + t.missed + t.failed,
			KeysCached:  t.cached,
			KeysFetched: t.fetched, KeysNotFound: t.missed, KeysFailed: t.failed,
			Last: t.lastPath,
		}
		mu.Unlock()
		c.post(p)
		if time.Since(commitGate) > catchUpCommitInterval {
			commitGate = time.Now()
			if err := idx.commit(); err != nil {
				fmt.Fprintf(os.Stderr, "library: catch-up commit: %v\n", err)
			}
		}
	}

	FetchAll(ctx, groups, lookup, catchUpConcurrency, 0, func(g *Group, fr FetchResult) {
		apply(g.Key, aggs[g.Key], fr.Res, fr.Err)
	})

	c.mu.Lock()
	c.resolved += t.keysResolved
	c.missed += t.missed
	c.failed += t.failed
	c.applied += t.applied
	c.repair += t.repair
	c.mu.Unlock()

	if ctx.Err() != nil {
		// A cancelled pass keeps what it applied (every verdict was written
		// through to the store) and writes no report.
		if err := idx.commit(); err != nil {
			fmt.Fprintf(os.Stderr, "library: catch-up commit: %v\n", err)
		}
		return
	}
	c.finishPass(idx, cfg, t)
}

// finishPass commits the index, posts the closing progress snapshot and
// writes the needs-repair inventory report.
func (c *CatchUp) finishPass(idx *Index, cfg CatchUpConfig, t passTally) {
	state := idx.state()
	if err := idx.commit(); err != nil {
		fmt.Fprintf(os.Stderr, "library: catch-up commit: %v\n", err)
	}
	c.post(Progress{
		Done: t.files, Total: t.files,
		Resolved: state.KeysResolved, KeysTotal: t.keys,
		KeysDone:    state.KeysResolved + t.missed + t.failed,
		KeysCached:  state.KeysResolved - t.fetched,
		KeysFetched: t.fetched, KeysNotFound: t.missed, KeysFailed: t.failed,
		Last: t.lastPath,
	})
	// The report is a statement about an ENUMERATED library, so it is only
	// written once the index actually holds per-file records. An index with
	// none (the folder-rollup-only state the legacy migration leaves, before
	// the first index-phase scan) has no file totals, and writing the report
	// there claimed "0 files, 0 indexed" for a library whose thousands of
	// files were merely not enumerated yet - measured live 2026-10-13
	// 00:37:34, written at launch with 7,806 files on disk, and never
	// refreshed because that pass had nothing to examine.
	if state.Files == 0 {
		return
	}
	if _, _, err := idx.WriteInventory(cfg.IndexPath, cfg.AppVersion); err != nil {
		fmt.Fprintf(os.Stderr, "library: inventory report: %v\n", err)
	}
}

// WriteInventory writes the needs-repair report for this index into
// <dir of indexPath>\inventory and returns both report paths. It is the
// on-demand form of the report every catch-up pass writes.
func (ix *Index) WriteInventory(indexPath, appVersion string) (string, string, error) {
	if ix == nil {
		return "", "", fmt.Errorf("library: inventory: no index")
	}
	r := repair.Report{GeneratedAt: time.Now().UTC(), AppVersion: appVersion}
	for _, rec := range ix.filesSnapshot() {
		if rec.State != StateNeedsRepair {
			continue
		}
		r.Entries = append(r.Entries, repair.Entry{
			Path:      rec.Path,
			Name:      rec.Name,
			Kind:      rec.Kind,
			Reason:    repair.ReasonNoMetadata,
			Attempts:  rec.Attempts,
			LastError: rec.LastError,
			LastTry:   time.Unix(0, rec.LastTry).UTC(),
		})
	}
	state := ix.state()
	r.Totals = repair.Totals{
		Files:       state.Files,
		Indexed:     state.Files,
		Resolved:    state.KeysResolved,
		NeedsRepair: len(r.Entries),
	}
	return repair.WriteReport(filepath.Join(filepath.Dir(indexPath), "inventory"), r)
}

// WriteInventory writes the on-demand needs-repair report for the index at
// indexPath: the same file a catch-up pass writes.
func WriteInventory(indexPath, appVersion string) (string, string, error) {
	idx, err := sharedIndex(indexPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "library: inventory %q: %v\n", indexPath, err)
	}
	if idx == nil {
		return "", "", fmt.Errorf("library: inventory: no index for %q", indexPath)
	}
	return idx.WriteInventory(indexPath, appVersion)
}

// applyVerdict writes one key's verdict onto every file record carrying it: a
// usable resolution clears the record; anything else counts an attempt and
// parks the record as needs_repair once it has burned maxRepairAttempts.
func (ix *Index) applyVerdict(key string, byPath map[string]string, res metadata.Resolution, lerr error, now time.Time) (applied, repairN int, lastPath string) {
	if ix == nil || key == "" {
		return 0, 0, ""
	}
	ok := lerr == nil && resolvedUsable(res)
	ix.mu.Lock()
	defer ix.mu.Unlock()
	for norm, k := range byPath {
		if k != key {
			continue
		}
		rec, exists := ix.files[norm]
		if !exists {
			continue
		}
		next := *rec
		next.LastTry = now.UnixNano()
		if ok {
			next.State = StateResolved
			next.Attempts = 0
			next.LastError = ""
			next.NextTry = 0
		} else {
			next.Attempts = rec.Attempts + 1
			next.LastError = verdictError(lerr, res)
			if next.Attempts >= maxRepairAttempts {
				next.State = StateNeedsRepair
				next.NextTry = now.Add(repairCooldown).UnixNano()
				repairN++
			} else {
				next.State = StatePending
				next.NextTry = 0
			}
		}
		ix.putFileLocked(next)
		applied++
		lastPath = next.Path
	}
	return applied, repairN, lastPath
}

// verdictError is the stored reason a lookup produced nothing usable: the
// provider error verbatim, or the plain-language miss.
func verdictError(lerr error, res metadata.Resolution) string {
	if lerr != nil {
		return lerr.Error()
	}
	if !res.Found {
		return "no metadata found"
	}
	return "metadata found but empty (no title, cover or synopsis)"
}

// resolvedUsable reports whether a resolution carries anything worth keeping.
// A found-but-empty answer is not a repair success: the needs-repair list
// exists precisely for the entries with no title, cover or synopsis.
func resolvedUsable(r metadata.Resolution) bool {
	return r.Found && (r.Series != "" || r.Episode != "" || r.Artwork != "" || r.Synopsis != "")
}

// underRoots reports whether path lies under one of the configured roots
// (case-insensitive, Windows path semantics). No roots means no restriction.
func underRoots(roots []string, path string) bool {
	if len(roots) == 0 {
		return true
	}
	p := normalizeScanDir(path)
	for _, root := range roots {
		r := normalizeScanDir(root)
		if p == r {
			return true
		}
		if len(p) > len(r) && p[:len(r)+1] == r+string(filepath.Separator) {
			return true
		}
	}
	return false
}

// fileExists reports whether path is still on disk.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
