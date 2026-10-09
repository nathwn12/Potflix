// Command metawarm pre-warms the metadata cache for a media library so
// episodes resolve instantly (and offline) later.
//
// Usage:
//
//	metawarm [-roots "D:\Movies;D:\Shows"] [-cache <dir>] [-dry] [-offline] [-limit N] [-concurrency N]
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
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"potflix/internal/metadata"
)

const (
	// rateInterval spaces network fetches (~1 request / 400ms): TVmaze is
	// ~20/10s and AniList ~30/min, and the metadata http client already
	// honors 429/Retry-After with one retry.
	rateInterval = 400 * time.Millisecond
	// lookupTimeout bounds a single unique-key fetch so one slow key
	// cannot stall the run; other keys proceed regardless.
	lookupTimeout = 120 * time.Second
)

// Report statuses, one per walked file.
const (
	statusSkipped  = "skipped"   // KindUnknown: themes/extras, never fetched
	statusDry      = "dry"       // -dry: parsed, would fetch
	statusResolved = "resolved"  // LookupNow found (offline: cache hit)
	statusNotFound = "not-found" // LookupNow missed (offline: cache miss)
	statusError    = "error"     // LookupNow returned an error
)

var videoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".m4v": true,
	".mov": true, ".wmv": true, ".ts": true, ".flv": true,
	".webm": true, ".mpg": true, ".mpeg": true,
}

// walkDir is filepath.WalkDir by default but injectable for tests.
var walkDir = filepath.WalkDir

type config struct {
	roots       []string
	cacheDir    string
	dry         bool
	offline     bool
	limit       int
	concurrency int
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
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: metawarm -roots \"D:\\Movies;D:\\Shows\" [-cache <dir>] [-dry] [-offline] [-limit N] [-concurrency N]\n")
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
	return c, nil
}

// collectFiles walks each root recursively (lexical walk order) for video
// files. limit > 0 truncates the result. A subdirectory or file that cannot
// be opened (permission or any walk error on a non-root entry) is logged
// once, counted as denied, and skipped; only an unreadable ROOT (or no
// readable roots at all) is a fatal error.
func collectFiles(roots []string, limit int) ([]string, int, error) {
	var files []string
	var denied int
	var okRoots int
	var firstErr error
	for _, root := range roots {
		fi, err := os.Stat(root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "metawarm: skipping unreadable path %q: %v\n", root, err)
			denied++
			if firstErr == nil {
				firstErr = fmt.Errorf("unwalkable root %q: %w", root, err)
			}
			continue
		}
		if !fi.IsDir() {
			fmt.Fprintf(os.Stderr, "metawarm: skipping unreadable path %q: not a directory\n", root)
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
				fmt.Fprintf(os.Stderr, "metawarm: skipping unreadable path %q: %v\n", path, err)
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
			fmt.Fprintf(os.Stderr, "metawarm: skipping unreadable path %q: %v\n", root, err)
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

// dedupeKey is the resolver's cache identity for one query.
func dedupeKey(q metadata.Query) string {
	return fmt.Sprintf("%d|%s|%d|%d|%t|%d",
		int(q.Kind), strings.ToLower(q.Title), q.Year, q.Season, q.Absolute, q.Episode)
}

type filePlan struct {
	path    string
	query   metadata.Query
	key     string
	skipped bool
}

type group struct {
	key   string
	query metadata.Query
	paths []string
}

// buildPlan parses every file and dedupes non-Unknown queries into groups,
// preserving walk order for the report.
func buildPlan(files []string) (plans []filePlan, groups []*group) {
	byKey := make(map[string]*group)
	for _, path := range files {
		q := metadata.ResolveQuery(filepath.Base(path), path)
		if q.Kind == metadata.KindUnknown {
			plans = append(plans, filePlan{path: path, query: q, skipped: true})
			continue
		}
		key := dedupeKey(q)
		g, ok := byKey[key]
		if !ok {
			g = &group{key: key, query: q}
			byKey[key] = g
			groups = append(groups, g)
		}
		g.paths = append(g.paths, path)
		plans = append(plans, filePlan{path: path, query: q, key: key})
	}
	return plans, groups
}

type lookupFunc func(ctx context.Context, q metadata.Query) (metadata.Resolution, error)

type fetchResult struct {
	res metadata.Resolution
	err error
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

// fetchAll calls lookup once per unique group with bounded concurrency.
// A single key's error is recorded, never fatal.
func fetchAll(groups []*group, lookup lookupFunc, concurrency int, gate *rateGate) map[string]fetchResult {
	out := make(map[string]fetchResult, len(groups))
	var mu sync.Mutex
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, g := range groups {
		wg.Add(1)
		go func(g *group) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if gate != nil {
				gate.wait()
			}
			ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
			defer cancel()
			res, err := lookup(ctx, g.query)
			mu.Lock()
			out[g.key] = fetchResult{res: res, err: err}
			mu.Unlock()
		}(g)
	}
	wg.Wait()
	return out
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

func writeReport(path string, plans []filePlan, statusOf func(filePlan) string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create report dir: %w", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create report: %w", err)
	}
	w := bufio.NewWriter(f)
	for _, p := range plans {
		if _, err := w.WriteString(statusOf(p) + "\t" + p.path + "\n"); err != nil {
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
	files    int
	skipped  int
	unique   int
	dry      int
	resolved int
	notFound int
	errors   int
	denied   int
}

func realMain(args []string, stdout, stderr io.Writer) int {
	cfg, err := parseArgs(args, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "metawarm: %v\n", err)
		return 2
	}
	files, denied, err := collectFiles(cfg.roots, cfg.limit)
	if err != nil {
		fmt.Fprintf(stderr, "metawarm: %v\n", err)
		return 1
	}
	plans, groups := buildPlan(files)

	c := counts{files: len(plans), unique: len(groups), denied: denied}
	statusByKey := make(map[string]string, len(groups))
	if cfg.dry {
		for _, g := range groups {
			statusByKey[g.key] = statusDry
		}
	} else {
		resolver := metadata.New(metadata.Options{Dir: cfg.cacheDir, Offline: cfg.offline})
		var gate *rateGate
		if !cfg.offline {
			gate = &rateGate{interval: rateInterval}
		}
		results := fetchAll(groups, resolver.LookupNow, cfg.concurrency, gate)
		for _, g := range groups {
			r, ok := results[g.key]
			switch {
			case !ok || r.err != nil:
				statusByKey[g.key] = statusError
			case r.res.Found:
				statusByKey[g.key] = statusResolved
			default:
				statusByKey[g.key] = statusNotFound
			}
		}
	}
	statusOf := func(p filePlan) string {
		if p.skipped {
			return statusSkipped
		}
		return statusByKey[p.key]
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

	var b strings.Builder
	fmt.Fprintf(&b, "metawarm summary: roots=%d files=%d skipped(unknown)=%d unique=%d denied=%d",
		len(cfg.roots), c.files, c.skipped, c.unique, c.denied)
	switch {
	case cfg.dry:
		fmt.Fprintf(&b, " dry=%d", c.dry)
	case cfg.offline:
		fmt.Fprintf(&b, " hits=%d missing=%d", c.resolved, c.notFound)
	default:
		fmt.Fprintf(&b, " resolved=%d not-found=%d", c.resolved, c.notFound)
	}
	fmt.Fprintf(&b, " errors=%d dry=%t offline=%t\nreport: %s\n", c.errors, cfg.dry, cfg.offline, report)
	fmt.Fprint(stdout, b.String())
	return 0
}

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}
