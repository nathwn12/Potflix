// Command metawarm pre-warms the metadata cache for a media library so
// episodes resolve instantly (and offline) later.
//
// Usage:
//
//	metawarm [-roots "D:\Movies;D:\Shows"] [-cache <dir>] [-dry] [-offline] [-limit N] [-concurrency N] [-gate <dur>] [-index <file>]
//
// -index <file> is opt-in: unchanged folders skip re-walk/re-fetch via the
// scan index (two-tier FolderNamesHash/FolderFingerprint guarded by the
// metadata cache). Empty (default) is a full walk; the summary always ends
// with "folders: N scanned, M skipped".
//
// Default roots come from -roots; if empty the command errors with usage.
// Each root is walked recursively for video files. For each file the query
// is parsed with metadata.ResolveQuery and KindUnknown files (themes/extras)
// are skipped. The resolver's LookupNow is called ONLY once per unique query
// (dedupe key: Kind, lowercased Title, Year, Season, Absolute, Episode).
// -dry parses and reports but fetches nothing. -offline serves the cache
// only and never touches the network.
//
// A per-file report (status<TAB>path lines) is written to
// %LOCALAPPDATA%\Potflix\inventory\warm-report--hold--<sessionId>.txt and a
// one-block summary is printed to stdout. Exit 0 unless fatal (bad args or
// an unwalkable root); a single file's error never aborts the run.
//
// The walk + resolve loop lives in potflix/internal/library (shared with the
// tray window's Scan/Refresh); this command adds only flag parsing, the
// per-file report, and the summary lines.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"potflix/internal/library"
	"potflix/internal/metadata"
)

const (
	// rateInterval is the historical global fetch spacing (~1 request /
	// 400ms). Provider throttling now lives in metadata's per-host
	// limiter, so online runs default to no extra global gate; -gate opts
	// back into one. Retained for documentation and for -gate callers.
	rateInterval = 400 * time.Millisecond
)

// Report statuses, one per walked file.
const (
	statusSkipped  = "skipped"   // KindUnknown: themes/extras, never fetched
	statusDry      = "dry"       // -dry: parsed, would fetch
	statusResolved = "resolved"  // LookupNow found (offline: cache hit)
	statusNotFound = "not-found" // LookupNow missed (offline: cache miss)
	statusError    = "error"     // LookupNow returned an error
	// statusCancelled marks a key whose fetch never ran or failed only
	// because the context was cancelled: incomplete, never a failure, and
	// never counted toward the error tally.
	statusCancelled = "cancelled"
)

type config struct {
	roots       []string
	cacheDir    string
	dry         bool
	offline     bool
	limit       int
	concurrency int
	gate        time.Duration
	indexPath   string
}

func parseArgs(args []string, stderr io.Writer) (config, error) {
	var c config
	fs := flag.NewFlagSet("metawarm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	roots := fs.String("roots", "", "semicolon-separated library roots to walk")
	fs.StringVar(&c.cacheDir, "cache", "", "cache directory (default %LOCALAPPDATA%\\Potflix\\meta)")
	fs.BoolVar(&c.dry, "dry", false, "parse and report only; fetch nothing")
	fs.BoolVar(&c.offline, "offline", false, "cache-only: never touch the network")
	fs.IntVar(&c.limit, "limit", 0, "max files to process (0 = no limit)")
	fs.IntVar(&c.concurrency, "concurrency", 3, "bounded concurrent fetches")
	fs.DurationVar(&c.gate, "gate", 0, "extra global spacing between fetches (0 = per-host limiter only)")
	fs.StringVar(&c.indexPath, "index", "", "opt-in scan-index file for folder skips (empty = off, full walk)")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: metawarm -roots \"D:\\Movies;D:\\Shows\" [-cache <dir>] [-dry] [-offline] [-limit N] [-concurrency N] [-gate <dur>] [-index <file>]\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	for _, r := range strings.Split(*roots, ";") {
		if r = strings.TrimSpace(r); r != "" {
			c.roots = append(c.roots, r)
		}
	}
	if len(c.roots) == 0 {
		fs.Usage()
		return config{}, fmt.Errorf("no roots: -roots is required")
	}
	if c.limit < 0 {
		return config{}, fmt.Errorf("bad -limit %d: must be >= 0", c.limit)
	}
	if c.concurrency < 1 {
		return config{}, fmt.Errorf("bad -concurrency %d: must be >= 1", c.concurrency)
	}
	if c.gate < 0 {
		return config{}, fmt.Errorf("bad -gate %v: must be >= 0", c.gate)
	}
	return c, nil
}

func sessionID() string {
	id := strings.TrimSpace(os.Getenv("OPENCODE_SESSION_ID"))
	if id == "" {
		return "manual"
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case '<', '>', ':', '"', '/', '\\', '|', '?', '*':
			return '_'
		}
		if r < 0x20 {
			return '_'
		}
		return r
	}, id)
}

// reportPath resolves %LOCALAPPDATA%\Potflix\inventory\warm-report--hold--<sessionId>.txt.
func reportPath() (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		d, err := os.UserCacheDir()
		if err != nil || d == "" {
			return "", fmt.Errorf("cannot resolve report dir: no LOCALAPPDATA and no user cache dir")
		}
		base = d
	}
	return filepath.Join(base, "Potflix", "inventory",
		"warm-report--hold--"+sessionID()+".txt"), nil
}

func writeReport(path string, plans []library.FilePlan, statusOf func(library.FilePlan) string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create report dir: %w", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create report: %w", err)
	}
	w := bufio.NewWriter(f)
	for _, p := range plans {
		if _, err := w.WriteString(statusOf(p) + "\t" + p.Path + "\n"); err != nil {
			f.Close()
			return fmt.Errorf("write report: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return fmt.Errorf("flush report: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close report: %w", err)
	}
	return nil
}

type counts struct {
	files          int
	skipped        int
	unique         int
	dry            int
	resolved       int
	notFound       int
	errors         int
	denied         int
	foldersScanned int
	foldersSkipped int
	keys           int
	keysCached     int
	keysFetched    int
	keysNotFound   int
	cancelled      bool
}

// newResolver builds the metadata resolver for a run; tests override it to
// inject fake sources (including slow/blocking ones) without touching the
// network.
var newResolver = func(cacheDir string, offline bool) *metadata.Resolver {
	return metadata.New(metadata.Options{Dir: cacheDir, Offline: offline})
}

func realMain(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return realMainWithContext(ctx, args, stdout, stderr)
}

func realMainWithContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cfg, err := parseArgs(args, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "metawarm: %v\n", err)
		return 2
	}
	useIndex := cfg.indexPath != ""
	var files []string
	var denied int
	var fScanned, fSkipped int
	var visited []string
	var skipped map[string]library.FolderRecord
	cancelled := false
	if useIndex && !cfg.dry {
		cr, walkCancelled, err := library.CollectIndexedWithContext(ctx, cfg.roots, cfg.cacheDir, cfg.indexPath, true, false)
		if err != nil {
			fmt.Fprintf(stderr, "metawarm: %v\n", err)
			return 1
		}
		if walkCancelled || ctx.Err() != nil {
			cancelled = true
		}
		files, denied, fScanned, fSkipped = cr.Files, cr.Denied, cr.FoldersScanned, cr.FoldersSkipped
		visited, skipped = cr.Visited, cr.Skipped
		if cfg.limit > 0 && len(files) > cfg.limit {
			files = files[:cfg.limit]
			// Partial walk: don't poison the index with truncated totals.
			visited, skipped = nil, nil
		}
	} else {
		var err error
		var walkCancelled bool
		files, denied, walkCancelled, err = library.CollectFilesWithContext(ctx, cfg.roots, cfg.limit)
		if err != nil {
			fmt.Fprintf(stderr, "metawarm: %v\n", err)
			return 1
		}
		if walkCancelled || ctx.Err() != nil {
			cancelled = true
		}
	}
	plans, groups := library.BuildPlan(files)

	c := counts{files: len(plans), unique: len(groups), denied: denied, foldersScanned: fScanned, foldersSkipped: fSkipped}
	c.keys = len(groups)
	preCached := make(map[string]bool, len(groups))
	if !cfg.dry {
		for _, g := range groups {
			// Cache-identity for skip/key accounting (not the finer dedupe key).
			// Live only: an expired negative or corrupt entry must not
			// pre-credit, or progress would claim cached work that still
			// needs a fetch.
			if metadata.CacheLive(cfg.cacheDir, metadata.CacheKeyFor(g.Query)) {
				c.keysCached++
				preCached[g.Key] = true
			}
		}
	}
	// Pre-fetch checkpoint so the index file exists before the fetch phase
	// completes (blocking provider test).
	if useIndex && !cfg.dry && visited != nil && !cancelled && ctx.Err() == nil {
		library.RecordScan(cfg.indexPath, visited, skipped, plans)
	}
	statusByKey := make(map[string]string, len(groups))
	if cfg.dry {
		for _, g := range groups {
			statusByKey[g.Key] = statusDry
		}
	} else {
		resolver := newResolver(cfg.cacheDir, cfg.offline)
		var gateInterval time.Duration
		if !cfg.offline {
			gateInterval = cfg.gate
		}
		var ckptMu sync.Mutex
		lastCkpt := time.Now()
		results := library.FetchAll(ctx, groups, resolver.LookupNow, cfg.concurrency, gateInterval,
			func(g *library.Group, _ library.FetchResult) {
				if !useIndex || cfg.dry || visited == nil || ctx.Err() != nil {
					return
				}
				ckptMu.Lock()
				defer ckptMu.Unlock()
				if time.Since(lastCkpt) < 2*time.Second {
					return
				}
				lastCkpt = time.Now()
				library.RecordScan(cfg.indexPath, visited, skipped, plans)
			})
		// Key accounting from results: cached = pre-credited hits that resolved,
		// fetched = uncached resolved, notfound = missed/errored uncached.
		for _, g := range groups {
			r, ok := results[g.Key]
			switch {
			case !ok || r.Err != nil:
				// A fetch that never ran (!ok) or failed only because
				// the context was cancelled is incomplete, never an
				// error: it must not feed the error tally.
				if ctx.Err() != nil {
					statusByKey[g.Key] = statusCancelled
				} else {
					statusByKey[g.Key] = statusError
				}
			case r.Res.Found:
				statusByKey[g.Key] = statusResolved
			default:
				statusByKey[g.Key] = statusNotFound
			}
		}
		// Split resolved keys into cached vs fetched using the pre-fetch
		// snapshot: a resolved key that was pre-credited stays cached, else
		// fetched. Unresolved uncached keys count as not found. The snapshot
		// must predate the fetch: fetched keys are cached by now.
		for _, g := range groups {
			st := statusByKey[g.Key]
			switch st {
			case statusResolved:
				if !preCached[g.Key] {
					c.keysFetched++
				}
			case statusNotFound, statusError:
				if !preCached[g.Key] {
					c.keysNotFound++
				}
			}
		}
		if ctx.Err() != nil {
			cancelled = true
		}
		c.cancelled = cancelled
	}
	if ctx.Err() != nil {
		cancelled = true
		c.cancelled = true
	}
	statusOf := func(p library.FilePlan) string {
		if p.Skipped {
			return statusSkipped
		}
		return statusByKey[p.Key]
	}
	for _, p := range plans {
		switch statusOf(p) {
		case statusSkipped:
			c.skipped++
		case statusDry:
			c.dry++
		case statusResolved:
			c.resolved++
		case statusNotFound:
			c.notFound++
		case statusError:
			c.errors++
		}
	}

	report, err := reportPath()
	if err != nil {
		fmt.Fprintf(stderr, "metawarm: %v\n", err)
		return 1
	}
	if err := writeReport(report, plans, statusOf); err != nil {
		fmt.Fprintf(stderr, "metawarm: %v\n", err)
		return 1
	}

	// A cancelled walk records nothing: stamping partial key lists would let
	// the next scan skip files that were never walked.
	if useIndex && !cfg.dry && visited != nil && !cancelled && ctx.Err() == nil {
		library.RecordScan(cfg.indexPath, visited, skipped, plans)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "metawarm summary: roots=%d files=%d skipped(unknown)=%d unique=%d denied=%d",
		len(cfg.roots), c.files, c.skipped, c.unique, c.denied)
	fmt.Fprintf(&b, " folders: %d scanned, %d skipped", c.foldersScanned, c.foldersSkipped)
	fmt.Fprintf(&b, " keys: %d (%d cached, %d fetched, %d not found)", c.keys, c.keysCached, c.keysFetched, c.keysNotFound)
	switch {
	case cfg.dry:
		fmt.Fprintf(&b, " dry=%d", c.dry)
	case cfg.offline:
		fmt.Fprintf(&b, " hits=%d missing=%d", c.resolved, c.notFound)
	default:
		fmt.Fprintf(&b, " resolved=%d not-found=%d", c.resolved, c.notFound)
	}
	fmt.Fprintf(&b, " errors=%d dry=%t offline=%t", c.errors, cfg.dry, cfg.offline)
	if cancelled {
		fmt.Fprintf(&b, " cancelled")
	}
	fmt.Fprintf(&b, "\nreport: %s\n", report)
	fmt.Fprint(stdout, b.String())
	return 0
}

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}
