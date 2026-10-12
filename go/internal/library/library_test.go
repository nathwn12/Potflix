package library

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"potflix/internal/metadata"
)

// metaCacheDir returns a metadata cache directory OUTSIDE the test's temp
// tree.
//
// Cross-package handle lifetime: internal/metadata holds its store file
// (<dir>.idx, cacheHandleFor in internal/metadata/cache.go) open for the
// process, and Windows refuses to unlink that file while the handle lives, so
// a cache created under t.TempDir() fails the test's own TempDir cleanup.
// That package's tests call closeCacheHandles from their own t.Cleanup; this
// package cannot (it is unexported), so the cache lives under the OS temp dir
// and is removed best-effort here. Reported to the fleet as a cross-package
// blocker; when a release path is exported this helper collapses back to
// t.TempDir().
func metaCacheDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "potflix-libtest-meta-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(dir)
		_ = os.Remove(dir + ".idx")
	})
	return dir
}

// seedLib builds a tiny fake library:
//   - the same episode in two folders (must dedupe to one key)
//   - a second episode of the same show (distinct key)
//   - a trailer under Extras (KindUnknown: skipped)
//   - a movie (distinct key)
func seedLib(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := []string{
		filepath.Join(root, "A", "Breaking.Bad.S01E01.mkv"),
		filepath.Join(root, "B", "Breaking.Bad.S01E01.mkv"),
		filepath.Join(root, "B", "Breaking.Bad.S01E02.mkv"),
		filepath.Join(root, "ShowName", "Extras", "SomeShow trailer.mkv"),
		filepath.Join(root, "Movies", "The.Matrix.1999.mkv"),
	}
	for _, f := range files {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A non-video file must never be collected.
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestBuildPlanSkipsUnknownAndDedupes(t *testing.T) {
	root := seedLib(t)
	files, denied, err := CollectFiles([]string{root}, 0)
	if err != nil {
		t.Fatalf("CollectFiles: %v", err)
	}
	if denied != 0 {
		t.Errorf("denied = %d, want 0 for a readable tree", denied)
	}
	if len(files) != 5 {
		t.Fatalf("files = %d, want 5 (non-video excluded): %v", len(files), files)
	}
	plans, groups := BuildPlan(files)
	skipped := 0
	for _, p := range plans {
		if p.Skipped {
			skipped++
			if !strings.HasSuffix(p.Path, "SomeShow trailer.mkv") {
				t.Errorf("skipped unexpected file: %s", p.Path)
			}
		}
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1", skipped)
	}
	// 5 files - 1 skipped = 4 queries, but the S01E01 pair shares a key:
	// groups = {S01E01, S01E02, movie} = 3.
	if len(groups) != 3 {
		t.Fatalf("unique groups = %d, want 3", len(groups))
		for _, g := range groups {
			t.Logf("key=%s query=%+v paths=%v", g.Key, g.Query, g.Paths)
		}
	}
	for _, g := range groups {
		if len(g.Paths) == 2 && !strings.Contains(g.Paths[0], "S01E01") {
			t.Errorf("shared group is not the S01E01 pair: %v", g.Paths)
		}
	}
}

func TestFetchAllLooksUpOncePerUniqueKey(t *testing.T) {
	root := seedLib(t)
	files, _, err := CollectFiles([]string{root}, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, groups := BuildPlan(files)
	var calls atomic.Int32
	stub := func(ctx context.Context, q metadata.Query) (metadata.Resolution, error) {
		calls.Add(1)
		return metadata.Resolution{Found: true}, nil
	}
	results := FetchAll(context.Background(), groups, stub, 3, 0, nil)
	if got := calls.Load(); got != int32(len(groups)) {
		t.Errorf("lookup calls = %d, want %d (once per unique key)", got, len(groups))
	}
	if len(results) != len(groups) {
		t.Errorf("results = %d, want %d", len(results), len(groups))
	}
}

// fakeEntry is a minimal fs.DirEntry for an injected walker.
type fakeEntry struct {
	name string
	dir  bool
}

func (e fakeEntry) Name() string { return e.name }
func (e fakeEntry) IsDir() bool  { return e.dir }
func (e fakeEntry) Type() fs.FileMode {
	if e.dir {
		return fs.ModeDir
	}
	return 0
}
func (e fakeEntry) Info() (fs.FileInfo, error) { return nil, nil }

// TestCollectFilesSkipsDeniedSubdir proves one unreadable subdirectory is
// never fatal: the injected walker emits a permission error for a subdir,
// CollectFiles skips it (denied == 1) and still collects the other files.
func TestCollectFilesSkipsDeniedSubdir(t *testing.T) {
	root := t.TempDir()
	good := filepath.Join(root, "good.mkv")
	other := filepath.Join(root, "other.mp4")
	badDir := filepath.Join(root, "bad")
	inner := filepath.Join(badDir, "hidden.mkv")

	orig := walkDir
	defer func() { walkDir = orig }()
	walkDir = func(walkRoot string, fn fs.WalkDirFunc) error {
		if walkRoot != root {
			t.Errorf("walkRoot = %q, want %q", walkRoot, root)
		}
		if err := fn(walkRoot, fakeEntry{name: filepath.Base(walkRoot), dir: true}, nil); err != nil {
			return err
		}
		if err := fn(good, fakeEntry{name: "good.mkv"}, nil); err != nil {
			return err
		}
		bd := fakeEntry{name: "bad", dir: true}
		if err := fn(badDir, bd, nil); err != nil {
			return err
		}
		perr := &os.PathError{Op: "open", Path: badDir, Err: os.ErrPermission}
		if err := fn(badDir, bd, perr); err != nil {
			if err == filepath.SkipDir {
				// Skipped: the file under the denied dir must never surface.
			} else {
				return err
			}
		}
		_ = inner
		if err := fn(other, fakeEntry{name: "other.mp4"}, nil); err != nil {
			return err
		}
		return nil
	}

	files, denied, err := CollectFiles([]string{root}, 0)
	if err != nil {
		t.Fatalf("CollectFiles with one denied subdir: %v", err)
	}
	if denied != 1 {
		t.Errorf("denied = %d, want 1", denied)
	}
	if len(files) != 2 {
		t.Fatalf("files = %v, want [good other]", files)
	}
	got := map[string]bool{}
	for _, f := range files {
		got[f] = true
	}
	if !got[good] || !got[other] {
		t.Errorf("files = %v, want %q and %q collected", files, good, other)
	}
	for _, f := range files {
		if f == inner || strings.HasPrefix(f, badDir+string(os.PathSeparator)) {
			t.Errorf("collected file under denied dir: %q", f)
		}
	}
}

// TestWalkFiltersAndToleratesUnreadableRoot proves Walk calls fn only for
// video files and tolerates an unreadable root when another root reads fine;
// an all-unreadable set is a fatal error.
func TestWalkFiltersAndToleratesUnreadableRoot(t *testing.T) {
	root := t.TempDir()
	video := filepath.Join(root, "Show.S01E01.mkv")
	if err := os.WriteFile(video, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "no-such-dir")

	var got []string
	if err := Walk([]string{root, missing}, func(path string) error {
		got = append(got, path)
		return nil
	}); err != nil {
		t.Fatalf("Walk with one unreadable root: %v", err)
	}
	if len(got) != 1 || got[0] != video {
		t.Errorf("Walk collected = %v, want [%q] (non-video filtered)", got, video)
	}

	if err := Walk([]string{missing}, func(path string) error { return nil }); err == nil {
		t.Error("Walk with only unreadable roots = nil, want fatal error")
	}
}

// fakeSource is a controllable metadata provider: Find answers every query
// with media carrying tag as the episode title.
type fakeSource struct {
	name  string
	calls atomic.Int32
	tag   string
}

func (f *fakeSource) Name() string { return f.name }

func (f *fakeSource) Find(ctx context.Context, q metadata.Query) (*metadata.Media, error) {
	f.calls.Add(1)
	m := &metadata.Media{
		Source: "fake", SourceID: "1",
		Kind: q.Kind, Title: q.Title, Year: q.Year,
		FetchedAt: time.Now().UTC(),
	}
	if q.Kind != metadata.KindMovie {
		m.Episodes = []metadata.Episode{{Season: q.Season, Number: q.Episode, Title: f.tag}}
	}
	return m, nil
}

// slowCancelSource blocks each lookup until ctx is done (user cancel) or
// release is closed (normal completion in checkpoint test). It respects ctx
// so a cancel returns promptly (<1s).
type slowCancelSource struct {
	name    string
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
	tag     string
	once    sync.Once
}

func (s *slowCancelSource) Name() string { return s.name }

func (s *slowCancelSource) Find(ctx context.Context, q metadata.Query) (*metadata.Media, error) {
	s.calls.Add(1)
	s.once.Do(func() { close(s.started) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.release:
	}
	m := &metadata.Media{
		Source: "fake", SourceID: "1",
		Kind: q.Kind, Title: q.Title, Year: q.Year,
		FetchedAt: time.Now().UTC(),
	}
	if q.Kind != metadata.KindMovie {
		m.Episodes = []metadata.Episode{{Season: q.Season, Number: q.Episode, Title: s.tag}}
	}
	return m, nil
}

// splitSpeedSource answers ShowA fast and blocks ShowB until ctx done or
// release: after a fetch-phase cancel, ShowA is cached but ShowB is not, so
// a resume skips the ShowA-only folder but re-walks the mixed folder.
type splitSpeedSource struct {
	name    string
	calls   atomic.Int32
	release chan struct{}
	tag     string
}

func (s *splitSpeedSource) Name() string { return s.name }

func (s *splitSpeedSource) Find(ctx context.Context, q metadata.Query) (*metadata.Media, error) {
	s.calls.Add(1)
	if !strings.Contains(strings.ToLower(q.Title), "showa") {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.release:
		}
	}
	m := &metadata.Media{
		Source: "fake", SourceID: "1",
		Kind: q.Kind, Title: q.Title, Year: q.Year,
		FetchedAt: time.Now().UTC(),
	}
	if q.Kind != metadata.KindMovie {
		m.Episodes = []metadata.Episode{{Season: q.Season, Number: q.Episode, Title: s.tag}}
	}
	return m, nil
}

func seedScanLib(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range []string{
		filepath.Join(root, "A", "ShowA.S01E01.mkv"),
		filepath.Join(root, "B", "ShowA.S01E01.mkv"),
		filepath.Join(root, "B", "ShowB.S01E01.mkv"),
	} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestScanIncrementalAndForce proves the Scan semantics end to end against a
// temp library and a temp cache dir: run 1 resolves and caches (one fetch per
// unique key), run 2 without force is a no-op (zero fetches, cache files
// untouched), run 3 with force refetches and rewrites every entry.
func TestScanIncrementalAndForce(t *testing.T) {
	root := seedScanLib(t)
	cache := metaCacheDir(t)
	ctx := context.Background()

	orig := newResolver
	defer func() { newResolver = orig }()

	fake1 := &fakeSource{name: "fake", tag: "V1"}
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{fake1}})
	}
	var last Progress
	st1, err := Scan(ctx, []string{root}, false, cache, func(p Progress) { last = p })
	if err != nil {
		t.Fatalf("run 1 Scan: %v", err)
	}
	if st1.Files != 3 || st1.Skipped != 0 || st1.Unique != 2 || st1.Resolved != 3 || st1.NotFound != 0 || st1.Errors != 0 || st1.Denied != 0 {
		t.Errorf("run 1 stats = %+v, want {Files:3 Unique:2 Resolved:3}", st1)
	}
	// Run 1 hit an empty cache, so every resolved file was a fresh fetch.
	if st1.Cached != 0 || st1.Fetched != 3 {
		t.Errorf("run 1 provenance = cached %d fetched %d, want cached 0 fetched 3", st1.Cached, st1.Fetched)
	}
	if st1.Cached+st1.Fetched != st1.Resolved {
		t.Errorf("run 1 Cached+Fetched = %d, want Resolved %d", st1.Cached+st1.Fetched, st1.Resolved)
	}
	if n := fake1.calls.Load(); n != 2 {
		t.Errorf("run 1 fetches = %d, want 2 (once per unique key)", n)
	}
	if last.Done != 3 || last.Total != 3 || last.Resolved != 3 || last.Last == "" {
		t.Errorf("run 1 final progress = %+v, want {Done:3 Total:3 Resolved:3}", last)
	}
	if last.Cached != 0 || last.Fetched != 3 {
		t.Errorf("run 1 final progress provenance = cached %d fetched %d, want 0/3", last.Cached, last.Fetched)
	}
	q := metadata.ResolveQuery("ShowA.S01E01.mkv", filepath.Join(root, "A", "ShowA.S01E01.mkv"))
	offline := metadata.New(metadata.Options{Dir: cache, Offline: true})
	if res, err := offline.LookupNow(ctx, q); err != nil || !res.Found || res.Episode != "V1" {
		t.Fatalf("run 1 cached content = %+v,%v; want episode V1 found", res, err)
	}
	// The cache medium is internal/metadata's store file now (per-key JSON
	// files are its legacy import source), so "one entry per show" is asserted
	// through the cache's own probe rather than a directory count.
	for _, n := range []string{"ShowA.S01E01.mkv", "ShowB.S01E01.mkv"} {
		key := metadata.CacheKeyFor(metadata.ResolveQuery(n, filepath.Join(root, "A", n)))
		if key == "" || !metadata.CacheHas(cache, key) {
			t.Fatalf("cache has no entry for %q (key %q) after run 1", n, key)
		}
	}

	// Run 2 (non-force): everything is cached, so no fetch may happen and no
	// cache file may change.
	fake2 := &fakeSource{name: "fake", tag: "V2-should-never-fetch"}
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{fake2}})
	}
	st2, err := Scan(ctx, []string{root}, false, cache, nil)
	if err != nil {
		t.Fatalf("run 2 Scan: %v", err)
	}
	if st2.Files != st1.Files || st2.Skipped != st1.Skipped || st2.Unique != st1.Unique ||
		st2.Resolved != st1.Resolved || st2.NotFound != st1.NotFound || st2.Errors != st1.Errors || st2.Denied != st1.Denied {
		t.Errorf("run 2 verdict stats = %+v, want run-1 verdicts %+v", st2, st1)
	}
	// Run 2 is the honest no-op: same verdicts, but every resolved file
	// was served from cache, none fetched.
	if st2.Cached != 3 || st2.Fetched != 0 {
		t.Errorf("run 2 provenance = cached %d fetched %d, want cached 3 fetched 0", st2.Cached, st2.Fetched)
	}
	if n := fake2.calls.Load(); n != 0 {
		t.Errorf("run 2 fetches = %d, want 0 (all served from disk)", n)
	}
	// "No rewrite without force" is asserted where the cache can see it: zero
	// provider calls above and zero fetched keys, plus every probe still live.
	for _, n := range []string{"ShowA.S01E01.mkv", "ShowB.S01E01.mkv"} {
		key := metadata.CacheKeyFor(metadata.ResolveQuery(n, filepath.Join(root, "A", n)))
		if !metadata.CacheLive(cache, key) {
			t.Errorf("run 2 lost the live cache entry for %q (key %q)", n, key)
		}
	}

	// Run 3 (force): every key refetches and every entry is rewritten.
	time.Sleep(60 * time.Millisecond) // separate mtimes on coarse filesystems
	fake3 := &fakeSource{name: "fake", tag: "V2"}
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{fake3}})
	}
	st3, err := Scan(ctx, []string{root}, true, cache, nil)
	if err != nil {
		t.Fatalf("run 3 Scan: %v", err)
	}
	if st3.Files != st1.Files || st3.Skipped != st1.Skipped || st3.Unique != st1.Unique ||
		st3.Resolved != st1.Resolved || st3.NotFound != st1.NotFound || st3.Errors != st1.Errors || st3.Denied != st1.Denied {
		t.Errorf("run 3 verdict stats = %+v, want %+v", st3, st1)
	}
	// Run 3 forced a refetch, so nothing may count as cached.
	if st3.Cached != 0 || st3.Fetched != 3 {
		t.Errorf("run 3 provenance = cached %d fetched %d, want cached 0 fetched 3", st3.Cached, st3.Fetched)
	}
	if n := fake3.calls.Load(); n != 2 {
		t.Errorf("run 3 fetches = %d, want 2 (forced refetch per key)", n)
	}
	// The forced refetch is proven by the provider count and by the entry's
	// content: the rewritten cache must now answer V2.
	offline3 := metadata.New(metadata.Options{Dir: cache, Offline: true})
	if res, err := offline3.LookupNow(ctx, q); err != nil || !res.Found || res.Episode != "V2" {
		t.Fatalf("run 3 cached content = %+v,%v; want episode V2 found", res, err)
	}
}

// TestScanCachedVsFetched proves the per-file provenance split end to end
// over a temp dir + temp cache: a non-force re-scan reports Cached=N and
// Fetched=0, while a force scan reports Fetched=N and Cached=0, with
// Cached + Fetched == Resolved in both runs.
func TestScanCachedVsFetched(t *testing.T) {
	root := seedScanLib(t) // 3 files, 2 unique keys, all resolvable
	cache := metaCacheDir(t)
	ctx := context.Background()

	orig := newResolver
	defer func() { newResolver = orig }()
	fake := &fakeSource{name: "fake", tag: "V1"}
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{fake}})
	}

	// Fill the cache (fresh dir: everything must fetch).
	fill, err := Scan(ctx, []string{root}, false, cache, nil)
	if err != nil {
		t.Fatalf("fill Scan: %v", err)
	}
	if fill.Resolved != 3 || fill.Cached != 0 || fill.Fetched != 3 {
		t.Fatalf("fill stats = %+v, want {Resolved:3 Cached:0 Fetched:3}", fill)
	}

	// (a) Non-force re-scan: an honest no-op.
	rescan, err := Scan(ctx, []string{root}, false, cache, nil)
	if err != nil {
		t.Fatalf("re-scan Scan: %v", err)
	}
	if rescan.Files != 3 || rescan.Unique != 2 || rescan.Resolved != 3 {
		t.Errorf("re-scan verdicts = %+v, want {Files:3 Unique:2 Resolved:3}", rescan)
	}
	if rescan.Cached != 3 || rescan.Fetched != 0 {
		t.Errorf("re-scan provenance = cached %d fetched %d, want cached 3 fetched 0", rescan.Cached, rescan.Fetched)
	}
	if rescan.Cached+rescan.Fetched != rescan.Resolved {
		t.Errorf("re-scan Cached+Fetched = %d, want Resolved %d", rescan.Cached+rescan.Fetched, rescan.Resolved)
	}

	// (b) Force scan: essentially all fetched.
	refresh, err := Scan(ctx, []string{root}, true, cache, nil)
	if err != nil {
		t.Fatalf("force Scan: %v", err)
	}
	if refresh.Resolved != 3 {
		t.Errorf("force verdicts = %+v, want Resolved 3", refresh)
	}
	if refresh.Cached != 0 || refresh.Fetched != 3 {
		t.Errorf("force provenance = cached %d fetched %d, want cached 0 fetched 3", refresh.Cached, refresh.Fetched)
	}
	if refresh.Cached+refresh.Fetched != refresh.Resolved {
		t.Errorf("force Cached+Fetched = %d, want Resolved %d", refresh.Cached+refresh.Fetched, refresh.Resolved)
	}
}

// ---- Indexed re-scan (two-tier skip) ----
//
// Corpus: three independent leaf roots (d1..d3), each a directory with
// video files, passed as Dirs=[d1,d2,d3]. Folder tallies are per skip
// decision, so a full skip is FoldersScanned==0/FoldersSkipped==3 and a
// single-folder change is 1/2. All caches live under t.TempDir().
func seedIndexCorpus(t *testing.T) (dirs []string, parent string) {
	t.Helper()
	parent = t.TempDir()
	ordered := []string{"d1", "d2", "d3"}
	seed := map[string][]string{
		"d1": {"ShowA.S01E01.mkv", "ShowA.S01E02.mkv"},
		"d2": {"ShowB.S01E01.mkv"},
		"d3": {"ShowC.S01E01.mkv", "ShowCtrailer.mkv"},
	}
	for _, d := range ordered {
		for _, n := range seed[d] {
			p := filepath.Join(parent, d, n)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		dirs = append(dirs, filepath.Join(parent, d))
	}
	return dirs, parent
}

// indexScanOpts is the indexed-run configuration for the scan tests: they pin
// the index phase (walk, per-file diff, folder skip tallies), so they run with
// SkipCatchUp - no background metadata pass, and nothing racing the temp-dir
// cleanup. The catch-up has its own tests (catchup_test.go).
func indexScanOpts(dirs []string, cache, index string, force bool) Options {
	return Options{Dirs: dirs, Force: force, CacheDir: cache, IndexPath: index, UseIndex: true, SkipCatchUp: true}
}

func withFakeResolver(tag string, calls *atomic.Int32) func() {
	orig := newResolver
	fake := &fakeSource{name: "fake", tag: tag}
	if calls != nil {
		fake.calls.Store(calls.Load())
	}
	_ = fake
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{fake}})
	}
	return func() { newResolver = orig }
}

// TestIndexSecondScanSkipsUnchanged proves a second scan of an unchanged
// corpus skips everything: FoldersScanned==0, FoldersSkipped==3,
// Files==0, FilesSkipped==total. Method: an injected walkDir wrapper
// counts file visits; skipped subtrees return filepath.SkipDir before any
// file callback, so the count stays 0 without timing tricks.
func TestIndexSecondScanSkipsUnchanged(t *testing.T) {
	dirs, _ := seedIndexCorpus(t)
	cache := metaCacheDir(t)
	index := filepath.Join(t.TempDir(), "scanindex.json")
	ctx := context.Background()

	restore := withFakeResolver("V1", nil)
	defer restore()
	st1, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, false))
	if err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	if st1.FoldersSkipped != 0 || st1.FilesSkipped != 0 {
		t.Fatalf("scan 1 tallies = scanned %d skipped %d fileskipped %d, want 0 skips",
			st1.FoldersScanned, st1.FoldersSkipped, st1.FilesSkipped)
	}
	if st1.FoldersScanned != 3 {
		t.Fatalf("scan 1 FoldersScanned = %d, want 3", st1.FoldersScanned)
	}
	totalFiles := st1.Files

	origWalk := walkDir
	defer func() { walkDir = origWalk }()
	fileVisits := 0
	walkDir = func(root string, fn fs.WalkDirFunc) error {
		return origWalk(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d != nil && !d.IsDir() {
				fileVisits++
			}
			return fn(p, d, err)
		})
	}

	var last Progress
	st2, err := ScanWithOptions(ctx, Options{Dirs: dirs, CacheDir: cache, IndexPath: index, UseIndex: true, SkipCatchUp: true, Progress: func(p Progress) { last = p }})
	if err != nil {
		t.Fatalf("scan 2: %v", err)
	}
	if st2.FoldersScanned != 0 {
		t.Errorf("scan 2 FoldersScanned = %d, want 0", st2.FoldersScanned)
	}
	if st2.FoldersSkipped != 3 {
		t.Errorf("scan 2 FoldersSkipped = %d, want 3", st2.FoldersSkipped)
	}
	if st2.Files != 0 {
		t.Errorf("scan 2 Files = %d, want 0 (all skipped)", st2.Files)
	}
	if st2.FilesSkipped != totalFiles {
		t.Errorf("scan 2 FilesSkipped = %d, want %d (Files+FilesSkipped adds up)", st2.FilesSkipped, totalFiles)
	}
	if fileVisits != 0 {
		t.Errorf("scan 2 visited %d files, want 0 (subtrees skipped before any file)", fileVisits)
	}
	if last.FoldersScanned != 0 || last.FoldersSkipped != 3 {
		t.Errorf("scan 2 final progress folders = %d scanned %d skipped, want 0/3 (%+v)", last.FoldersScanned, last.FoldersSkipped, last)
	}
}

// TestIndexAddFileRescansOnlyThatFolder proves adding a file to d1
// rescans only d1; d2/d3 still skip.
func TestIndexAddFileRescansOnlyThatFolder(t *testing.T) {
	dirs, _ := seedIndexCorpus(t)
	cache := metaCacheDir(t)
	index := filepath.Join(t.TempDir(), "scanindex.json")
	ctx := context.Background()

	restore := withFakeResolver("V1", nil)
	defer restore()
	if _, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, false)); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	addP := filepath.Join(dirs[0], "ShowA.S01E03.mkv")
	if err := os.WriteFile(addP, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	st2, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, false))
	if err != nil {
		t.Fatalf("scan 2: %v", err)
	}
	if st2.FoldersScanned != 1 || st2.FoldersSkipped != 2 {
		t.Errorf("scan 2 folders = %d scanned %d skipped, want 1/2 (%+v)", st2.FoldersScanned, st2.FoldersSkipped, st2)
	}
}

// TestIndexRenameIsNotSkipped proves a same-count rename is caught by the
// tier-1 names hash: d1 holds a KindUnknown trailer alongside its episode,
// so renaming ONLY the trailer keeps every recorded cache key present
// (guard passes) while the names hash differs -> d1 rescans.
func TestIndexRenameIsNotSkipped(t *testing.T) {
	dirs, parent := seedIndexCorpus(t)
	_ = parent
	trailer := filepath.Join(dirs[2], "ShowCtrailer.mkv")
	if _, err := os.Stat(trailer); err != nil {
		t.Fatalf("trailer seed missing: %v", err)
	}
	cache := metaCacheDir(t)
	index := filepath.Join(t.TempDir(), "scanindex.json")
	ctx := context.Background()

	restore := withFakeResolver("V1", nil)
	defer restore()
	if _, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, false)); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	if err := os.Rename(trailer, filepath.Join(dirs[2], "ShowCfeaturette.mkv")); err != nil {
		t.Fatal(err)
	}
	st2, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, false))
	if err != nil {
		t.Fatalf("scan 2: %v", err)
	}
	if st2.FoldersScanned != 1 || st2.FoldersSkipped != 2 {
		t.Errorf("rename: folders = %d scanned %d skipped, want 1/2 (%+v)", st2.FoldersScanned, st2.FoldersSkipped, st2)
	}
}

// TestIndexStaleTier1RefreshesViaTier2 proves the tier-2 fallback: with a
// stale NamesHash but current fingerprint/keys, the folder still skips
// and its record is refreshed to the current names hash.
// (Both tiers include the dir mtime, so no pure-filesystem touch can
// produce tier-1-miss+tier-2-hit; the state is injected directly.)
func TestIndexStaleTier1RefreshesViaTier2(t *testing.T) {
	dirs, _ := seedIndexCorpus(t)
	cache := metaCacheDir(t)
	index := filepath.Join(t.TempDir(), "scanindex.json")
	ctx := context.Background()

	restore := withFakeResolver("V1", nil)
	defer restore()
	if _, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, false)); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	ix := LoadScanIndex(index)
	rec, ok := ix.Lookup(dirs[0])
	if !ok {
		t.Fatalf("no record for d1")
	}
	fp := rec.Fingerprint
	ix.RecordFull(dirs[0], "stale-names-hash", fp, rec.Schema, rec.Keys, rec.Files)
	if err := ix.Save(index); err != nil {
		t.Fatalf("save stale record: %v", err)
	}
	st2, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, false))
	if err != nil {
		t.Fatalf("scan 2: %v", err)
	}
	if st2.FoldersSkipped != 3 || st2.FoldersScanned != 0 {
		t.Errorf("stale tier1: folders = %d scanned %d skipped, want 0/3 (%+v)", st2.FoldersScanned, st2.FoldersSkipped, st2)
	}
	now, _ := FolderNamesHash(dirs[0])
	got, ok := LoadScanIndex(index).Lookup(dirs[0])
	if !ok {
		t.Fatalf("record vanished for d1")
	}
	if got.NamesHash != now {
		t.Errorf("record not refreshed: NamesHash = %q, want current %q", got.NamesHash, now)
	}
}

// TestIndexMissingCacheKeyDisablesSkip proves where payload loss is handled
// now: a folder whose recorded keys have no live cache entry is no longer
// re-WALKED for that (its skip is a filesystem decision), but the catch-up
// re-resolves every key whose payload is gone. Here the metadata cache is
// cleared, the folder still skips, and the next pass calls the provider for
// every key again.
func TestIndexMissingCacheKeyDisablesSkip(t *testing.T) {
	dirs, _ := seedIndexCorpus(t)
	cache := metaCacheDir(t)
	index := filepath.Join(t.TempDir(), "scanindex.json")
	ctx := context.Background()

	fast := &fakeSource{name: "fake", tag: "V1"}
	orig := newResolver
	defer func() { newResolver = orig }()
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{fast}})
	}
	if _, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, false)); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	StartCatchUp(CatchUpConfig{Dirs: dirs, CacheDir: cache, IndexPath: index}, nil).Wait()
	before := fast.calls.Load()
	if before == 0 {
		t.Fatalf("first catch-up called the provider 0 times, want every key")
	}

	// Drop every cached payload: the records still say resolved, so only the
	// catch-up's cache probe can notice the loss.
	if err := metadata.ClearCache(cache); err != nil {
		t.Fatalf("clear metadata cache: %v", err)
	}
	st2, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, false))
	if err != nil {
		t.Fatalf("scan 2: %v", err)
	}
	if st2.FoldersScanned != 0 || st2.FoldersSkipped != 3 {
		t.Errorf("scan 2 folders = %d scanned %d skipped, want 0/3 (%+v)", st2.FoldersScanned, st2.FoldersSkipped, st2)
	}
	StartCatchUp(CatchUpConfig{Dirs: dirs, CacheDir: cache, IndexPath: index}, nil).Wait()
	if got := int(fast.calls.Load()) - int(before); got != int(before) {
		t.Errorf("re-resolve calls = %d, want %d (every key whose payload vanished)", got, before)
	}
}

// TestIndexForceRewritesWithoutSkipping proves force=true walks everything
// and re-records every visited dir. Identical content is not rewritten to
// disk (dirty-gated folder rollups), so the test pins the ROLLUP content
// instead of a blind mtime bump - plus a follow-up force scan over changed
// content, which must update the record. The index medium is the store file
// beside the path (IndexStorePath), not the retired JSON.
func TestIndexForceRewritesWithoutSkipping(t *testing.T) {
	dirs, _ := seedIndexCorpus(t)
	cache := metaCacheDir(t)
	index := filepath.Join(t.TempDir(), "scanindex.json")
	ctx := context.Background()

	restore := withFakeResolver("V1", nil)
	defer restore()
	if _, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, false)); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	before, ok := LoadScanIndex(index).Lookup(dirs[0])
	if !ok {
		t.Fatalf("no rollup for %q after scan 1", dirs[0])
	}
	time.Sleep(60 * time.Millisecond)
	st, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, true))
	if err != nil {
		t.Fatalf("force scan: %v", err)
	}
	if st.FoldersSkipped != 0 {
		t.Errorf("force: FoldersSkipped = %d, want 0", st.FoldersSkipped)
	}
	if st.FoldersScanned != 3 {
		t.Errorf("force: FoldersScanned = %d, want 3", st.FoldersScanned)
	}
	// Unchanged content re-records the same rollup (ScannedAt aside), so the
	// store write is dirty-gated away: the content must be identical.
	after, ok := LoadScanIndex(index).Lookup(dirs[0])
	if !ok || !sameScanRecord(before, after) {
		t.Errorf("force scan changed the rollup content:\nbefore = %+v\nafter  = %+v", before, after)
	}
	// Changed content still updates the record: drop a file and force-scan.
	if err := os.Remove(filepath.Join(dirs[0], "ShowA.S01E02.mkv")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if _, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, true)); err != nil {
		t.Fatalf("force scan 2: %v", err)
	}
	if rec, ok := LoadScanIndex(index).Lookup(dirs[0]); !ok || rec.Files != 1 {
		t.Errorf("d1 record after removal = %+v, want Files:1", rec)
	}
}

// scanRecordsEqual compares two indexes record by record, ignoring ScannedAt
// (write-only clock stamp, never read by the skip guard).
func scanRecordsEqual(t *testing.T, a, b *ScanIndex) bool {
	t.Helper()
	na, nb := a.Len(), b.Len()
	if na != nb {
		return false
	}
	for _, dir := range a.ix.folderNames() {
		ra, ok := a.Lookup(dir)
		if !ok {
			return false
		}
		rb, ok := b.Lookup(dir)
		if !ok {
			return false
		}
		if !sameScanRecord(ra, rb) {
			return false
		}
	}
	return true
}

// TestIndexSchemaBumpSkipsNothing proves records under an older schema
// never skip: rewriting every record's schema to a stale value forces a
// full walk.
func TestIndexSchemaBumpSkipsNothing(t *testing.T) {
	dirs, _ := seedIndexCorpus(t)
	cache := metaCacheDir(t)
	index := filepath.Join(t.TempDir(), "scanindex.json")
	ctx := context.Background()

	restore := withFakeResolver("V1", nil)
	defer restore()
	if _, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, false)); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	// Stale schema, written through the index's own API (the JSON rewrite the
	// old test did is gone with the medium).
	ix := LoadScanIndex(index)
	for _, d := range dirs {
		rec, ok := ix.Lookup(d)
		if !ok {
			t.Fatalf("no rollup for %q after scan 1", d)
		}
		ix.RecordFull(d, rec.NamesHash, rec.Fingerprint, metadata.CacheSchema()+1, rec.Keys, rec.Files)
	}
	if err := ix.Save(index); err != nil {
		t.Fatalf("save stale-schema records: %v", err)
	}
	st2, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, false))
	if err != nil {
		t.Fatalf("scan 2: %v", err)
	}
	if st2.FoldersSkipped != 0 || st2.FoldersScanned != 3 {
		t.Errorf("schema bump: folders = %d scanned %d skipped, want 3/0 (%+v)", st2.FoldersScanned, st2.FoldersSkipped, st2)
	}
}

// TestIndexMissingOrCorruptIsFullWalk proves a missing or corrupt index is
// a full walk, never an error. The medium is the store file beside the index
// path, so the corruption is applied there: a file this layer cannot read as
// a store is quarantined and the index starts empty.
func TestIndexMissingOrCorruptIsFullWalk(t *testing.T) {
	dirs, _ := seedIndexCorpus(t)
	cache := metaCacheDir(t)
	index := filepath.Join(t.TempDir(), "scanindex.json")
	ctx := context.Background()

	restore := withFakeResolver("V1", nil)
	defer restore()
	st1, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, false))
	if err != nil {
		t.Fatalf("missing index scan: %v", err)
	}
	if st1.FoldersScanned != 3 || st1.FoldersSkipped != 0 {
		t.Errorf("missing index: folders = %d scanned %d skipped, want 3/0 (%+v)", st1.FoldersScanned, st1.FoldersSkipped, st1)
	}
	if err := CloseIndex(index); err != nil {
		t.Fatalf("close index: %v", err)
	}
	if err := os.WriteFile(IndexStorePath(index), []byte("{{not a store file at all, forty bytes or so"), 0o644); err != nil {
		t.Fatal(err)
	}
	st2, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, false))
	if err != nil {
		t.Fatalf("corrupt index scan: %v", err)
	}
	if st2.FoldersScanned != 3 || st2.FoldersSkipped != 0 {
		t.Errorf("corrupt index: folders = %d scanned %d skipped, want 3/0 (%+v)", st2.FoldersScanned, st2.FoldersSkipped, st2)
	}
	if _, err := os.Stat(IndexStorePath(index) + ".corrupt"); err != nil {
		t.Errorf("corrupt store was not quarantined beside the index: %v", err)
	}
}

// missSource is a provider that never resolves: every Find is a miss.
type missSource struct {
	calls atomic.Int32
}

func (m *missSource) Name() string { return "miss" }

func (m *missSource) Find(ctx context.Context, q metadata.Query) (*metadata.Media, error) {
	m.calls.Add(1)
	return nil, nil
}

// TestIndexAllNegativeFolderSkipped proves folders whose files all resolved
// not-found are still skippable without network: the scan itself never
// fetches (the folder is indexed with its files pending), the catch-up
// persists the negatives, and the next scan skips the folder entirely
// (FoldersScanned==0) with the numbers staying honest (first pass
// Pending==files, second pass FilesSkipped==files).
func TestIndexAllNegativeFolderSkipped(t *testing.T) {
	parent := t.TempDir()
	folder := filepath.Join(parent, "missfolder")
	for _, n := range []string{"ShowM.S01E01.mkv", "ShowN.S01E01.mkv"} {
		p := filepath.Join(folder, n)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dirs := []string{folder}
	cache := metaCacheDir(t)
	index := filepath.Join(t.TempDir(), "scanindex.json")
	ctx := context.Background()

	orig := newResolver
	defer func() { newResolver = orig }()

	miss1 := &missSource{}
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{miss1}})
	}
	st1, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, false))
	if err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	if st1.Files != 2 || st1.Pending != 2 || st1.Resolved != 0 {
		t.Fatalf("scan 1 stats = %+v, want {Files:2 Pending:2 Resolved:0} (the scan never fetches)", st1)
	}
	if st1.FoldersScanned != 1 || st1.FoldersSkipped != 0 {
		t.Fatalf("scan 1 folders = %d scanned %d skipped, want 1/0 (%+v)", st1.FoldersScanned, st1.FoldersSkipped, st1)
	}
	// The catch-up owns the lookups: a definitive miss persists a negative
	// entry per key and leaves the record pending (attempt 1 of 3).
	StartCatchUp(CatchUpConfig{Dirs: dirs, CacheDir: cache, IndexPath: index}, nil).Wait()
	for _, n := range []string{"ShowM.S01E01.mkv", "ShowN.S01E01.mkv"} {
		q := metadata.ResolveQuery(n, filepath.Join(folder, n))
		if q.Kind == metadata.KindUnknown {
			t.Fatalf("seed file %q parsed as KindUnknown", n)
		}
		if !metadata.CacheHas(cache, metadata.CacheKeyFor(q)) {
			t.Fatalf("CacheHas = false for negative-cached key %q, want true", metadata.CacheKeyFor(q))
		}
	}

	// Second pass: a fresh miss provider must never be called - the folder
	// skips on its unchanged filesystem identity.
	miss2 := &missSource{}
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{miss2}})
	}
	st2, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, false))
	if err != nil {
		t.Fatalf("scan 2: %v", err)
	}
	if st2.FoldersScanned != 0 {
		t.Errorf("scan 2 FoldersScanned = %d, want 0", st2.FoldersScanned)
	}
	if st2.FoldersSkipped != 1 {
		t.Errorf("scan 2 FoldersSkipped = %d, want 1", st2.FoldersSkipped)
	}
	if st2.Files != 0 {
		t.Errorf("scan 2 Files = %d, want 0 (all skipped)", st2.Files)
	}
	if st2.FilesSkipped != 2 {
		t.Errorf("scan 2 FilesSkipped = %d, want 2", st2.FilesSkipped)
	}
	if n := miss2.calls.Load(); n != 0 {
		t.Errorf("scan 2 provider calls = %d, want 0 (skipped folder, no network)", n)
	}
}

// TestScanKeysFullyCached proves cached work is pre-credited: after a fill,
// a non-force re-scan's FIRST progress callback already reports
// KeysDone == Keys (== KeysTotal) with KeysFetched == 0, and the final
// Stats carry Keys == KeysDone == KeysCached.
func TestScanKeysFullyCached(t *testing.T) {
	root := seedScanLib(t) // 3 files, 2 unique keys, all resolvable
	cache := metaCacheDir(t)
	ctx := context.Background()

	orig := newResolver
	defer func() { newResolver = orig }()
	fake := &fakeSource{name: "fake", tag: "V1"}
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{fake}})
	}

	if _, err := Scan(ctx, []string{root}, false, cache, nil); err != nil {
		t.Fatalf("fill Scan: %v", err)
	}

	var mu sync.Mutex
	var calls []Progress
	st, err := Scan(ctx, []string{root}, false, cache, func(p Progress) {
		mu.Lock()
		calls = append(calls, p)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("rescan Scan: %v", err)
	}
	if len(calls) == 0 {
		t.Fatal("rescan emitted no progress callbacks")
	}
	first := calls[0]
	if first.KeysTotal != 2 || first.KeysDone != 2 || first.KeysDone != first.KeysTotal {
		t.Errorf("first progress keys = total %d done %d, want total 2 done 2 (%+v)", first.KeysTotal, first.KeysDone, first)
	}
	if first.KeysFetched != 0 {
		t.Errorf("first progress KeysFetched = %d, want 0 (fully cached)", first.KeysFetched)
	}
	for i, p := range calls {
		if p.KeysDone != p.KeysCached+p.KeysFetched+p.KeysNotFound+p.KeysFailed {
			t.Errorf("callback %d invariant KeysDone %d != Cached %d + Fetched %d + NotFound %d + Failed %d (%+v)", i, p.KeysDone, p.KeysCached, p.KeysFetched, p.KeysNotFound, p.KeysFailed, p)
		}
		if p.KeysDone != 2 || p.KeysTotal != 2 {
			t.Errorf("callback %d keys = total %d done %d, want total 2 done 2", i, p.KeysTotal, p.KeysDone)
		}
	}
	if st.Keys != 2 {
		t.Errorf("stats Keys = %d, want 2 (%+v)", st.Keys, st)
	}
	if st.KeysCached != 2 || st.KeysFetched != 0 || st.KeysNotFound != 0 || st.KeysFailed != 0 || st.KeysDone != 2 {
		t.Errorf("stats keys = cached %d fetched %d notfound %d failed %d done %d, want 2/0/0/0/2 (%+v)", st.KeysCached, st.KeysFetched, st.KeysNotFound, st.KeysFailed, st.KeysDone, st)
	}
	if st.KeysDone != st.KeysCached+st.KeysFetched+st.KeysNotFound+st.KeysFailed || st.KeysDone != st.Keys {
		t.Errorf("stats invariant KeysDone %d != sum / Keys %d (%+v)", st.KeysDone, st.Keys, st)
	}
}

// TestScanKeysPartiallyCached proves progress measures unique-key work with
// pre-credit: after warming 2 keys, adding a third (missing) key rescans
// with KeysDone starting at KeysCached (>0), the invariant holding at every
// callback and at the end.
func TestScanKeysPartiallyCached(t *testing.T) {
	root := seedScanLib(t) // 2 keys warmed below
	cache := metaCacheDir(t)
	ctx := context.Background()

	orig := newResolver
	defer func() { newResolver = orig }()
	fake := &fakeSource{name: "fake", tag: "V1"}
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{fake}})
	}
	if _, err := Scan(ctx, []string{root}, false, cache, nil); err != nil {
		t.Fatalf("fill Scan: %v", err)
	}

	// Add a new show (third unique key) that the rescan provider misses.
	newFile := filepath.Join(root, "C", "ShowC.S01E01.mkv")
	if err := os.MkdirAll(filepath.Dir(newFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	miss := &missSource{}
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{miss}})
	}

	var mu sync.Mutex
	var calls []Progress
	st, err := Scan(ctx, []string{root}, false, cache, func(p Progress) {
		mu.Lock()
		calls = append(calls, p)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("rescan Scan: %v", err)
	}
	if len(calls) == 0 {
		t.Fatal("rescan emitted no progress callbacks")
	}
	first := calls[0]
	if first.KeysCached <= 0 {
		t.Errorf("first progress KeysCached = %d, want >0 (two warmed keys)", first.KeysCached)
	}
	if first.KeysDone != first.KeysCached {
		t.Errorf("first progress KeysDone %d != KeysCached %d (pre-credit) (%+v)", first.KeysDone, first.KeysCached, first)
	}
	if first.KeysTotal != 3 {
		t.Errorf("first progress KeysTotal = %d, want 3", first.KeysTotal)
	}
	for i, p := range calls {
		if p.KeysDone != p.KeysCached+p.KeysFetched+p.KeysNotFound+p.KeysFailed {
			t.Errorf("callback %d invariant KeysDone %d != Cached %d + Fetched %d + NotFound %d + Failed %d (%+v)", i, p.KeysDone, p.KeysCached, p.KeysFetched, p.KeysNotFound, p.KeysFailed, p)
		}
	}
	if st.Keys != 3 {
		t.Errorf("stats Keys = %d, want 3 (%+v)", st.Keys, st)
	}
	if st.KeysCached != 2 {
		t.Errorf("stats KeysCached = %d, want 2", st.KeysCached)
	}
	if st.KeysDone != st.KeysCached+st.KeysFetched+st.KeysNotFound+st.KeysFailed {
		t.Errorf("stats invariant KeysDone %d != Cached %d + Fetched %d + NotFound %d + Failed %d (%+v)", st.KeysDone, st.KeysCached, st.KeysFetched, st.KeysNotFound, st.KeysFailed, st)
	}
	if st.KeysDone != st.Keys {
		t.Errorf("stats KeysDone %d != Keys %d (%+v)", st.KeysDone, st.Keys, st)
	}
}

// TestScanCancelReturnsPartialQuickly proves prompt cancellation: a cancel
// that lands during the walk stops the run within ~1s with Cancelled=true and
// a nil error, keeps the numbers honest, and leaves the store-backed index
// readable. (The fetch phase these tests used to cancel is the catch-up's now;
// see TestCatchUpCancelStopsPass.)
func TestScanCancelReturnsPartialQuickly(t *testing.T) {
	root := seedScanLib(t)
	cache := metaCacheDir(t)
	index := filepath.Join(t.TempDir(), "scanindex.json")

	origWalk := walkDir
	defer func() { walkDir = origWalk }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seen := 0
	walkDir = func(walkRoot string, fn fs.WalkDirFunc) error {
		return origWalk(walkRoot, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d != nil && !d.IsDir() {
				// Cancel on the second file: mid-walk, deterministic.
				seen++
				if seen == 2 {
					cancel()
				}
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fn(p, d, err)
		})
	}

	start := time.Now()
	st, scanErr := ScanWithOptions(ctx, indexScanOpts([]string{root}, cache, index, false))
	latency := time.Since(start)
	t.Logf("cancel latency=%dms", latency.Milliseconds())
	if latency > 2*time.Second {
		t.Errorf("cancel latency = %v, want <2s", latency)
	}
	if scanErr != nil {
		t.Errorf("cancelled Scan err = %v, want nil (cancel is not failure)", scanErr)
	}
	if !st.Cancelled {
		t.Errorf("cancelled Scan Cancelled = false, want true (%+v)", st)
	}
	if st.Files == 0 {
		t.Errorf("cancelled Scan Files = 0, want the walked-so-far count kept (%+v)", st)
	}
	fi, err := os.Stat(IndexStorePath(index))
	if err != nil {
		t.Fatalf("index store missing after cancel: %v", err)
	}
	if fi.Size() == 0 {
		t.Errorf("index store empty after cancel")
	}
	// A cancelled run records NOTHING: no folder rollup may survive it.
	if got := LoadScanIndex(index).Len(); got != 0 {
		t.Errorf("cancelled walk recorded %d folder rollups, want 0", got)
	}
}

// TestScanResumeAfterCancelSkipsCachedFolders proves the resume contract over
// the same corpus and the same index: once the metadata is caught up, a
// second scan skips the unchanged folders and calls the provider for nothing.
func TestScanResumeAfterCancelSkipsCachedFolders(t *testing.T) {
	root := seedScanLib(t) // A: ShowA; B: ShowA+ShowB
	cache := metaCacheDir(t)
	index := filepath.Join(t.TempDir(), "scanindex.json")

	orig := newResolver
	defer func() { newResolver = orig }()

	// First run: index the corpus and let the catch-up resolve every key.
	fast := &fakeSource{name: "fake", tag: "V1"}
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{fast}})
	}
	st1, err := ScanWithOptions(context.Background(), indexScanOpts([]string{root}, cache, index, false))
	if err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	if st1.Cancelled {
		t.Fatalf("first Scan Cancelled = true, want false (%+v)", st1)
	}
	StartCatchUp(CatchUpConfig{Dirs: []string{root}, CacheDir: cache, IndexPath: index}, nil).Wait()

	// Resume with a counting fast provider: everything is indexed and
	// resolved, so the resume skips every folder and calls nothing.
	resume := &fakeSource{name: "fake", tag: "V1"}
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{resume}})
	}
	st2, err := ScanWithOptions(context.Background(), indexScanOpts([]string{root}, cache, index, false))
	if err != nil {
		t.Fatalf("resume Scan: %v", err)
	}
	if st2.Cancelled {
		t.Errorf("resume Cancelled = true, want false")
	}
	if st2.FoldersScanned != 0 || st2.FilesSkipped != 3 {
		t.Errorf("resume folders = %d scanned, files skipped %d, want 0 scanned / 3 skipped (%+v)", st2.FoldersScanned, st2.FilesSkipped, st2)
	}
	if n := resume.calls.Load(); n != 0 {
		t.Errorf("resume provider calls = %d, want 0 (indexed and caught up)", n)
	}
}

// TestScanCheckpointExistsBeforeFetchCompletes proves mid-run flushing: the
// index phase is committed to the store before any metadata work runs, so the
// index exists while the catch-up is still blocked on a provider.
func TestScanCheckpointExistsBeforeFetchCompletes(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 60; i++ {
		f := filepath.Join(root, filepath.FromSlash("F"+pad3(i)+"/Show"+pad3(i)+".S01E01.mkv"))
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cache := metaCacheDir(t)
	index := filepath.Join(t.TempDir(), "scanindex.json")

	orig := newResolver
	defer func() { newResolver = orig }()
	block := &slowCancelSource{name: "fake", tag: "V1", started: make(chan struct{}), release: make(chan struct{})}
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{block}})
	}
	st, err := ScanWithOptions(context.Background(), indexScanOpts([]string{root}, cache, index, false))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	_ = st

	// The index phase is on disk; the catch-up now blocks in the provider.
	c := StartCatchUp(CatchUpConfig{Dirs: []string{root}, CacheDir: cache, IndexPath: index}, nil)
	select {
	case <-block.started:
	case <-time.After(10 * time.Second):
		close(block.release)
		StopCatchUp(index)
		t.Fatal("blocking provider never started")
	}
	fi, err := os.Stat(IndexStorePath(index))
	if err != nil {
		t.Fatalf("index store missing while the catch-up is blocked: %v", err)
	}
	if fi.Size() == 0 {
		t.Errorf("index store empty while the catch-up is blocked")
	}
	close(block.release)
	c.Wait()
}

// pad3 formats 0-999 as three digits without fmt (stdlib-only, no extra dep).
func pad3(i int) string {
	return string([]byte{byte('0' + i/100), byte('0' + (i/10)%10), byte('0' + i%10)})
}

// TestScanCancelledWalkRecordsNothing proves F1: a walk that ends by
// cancellation records NOTHING for that run, so the next scan cannot skip
// files that were never walked. The walk is cancelled as it reaches dir B;
// afterwards neither the fully walked dir A nor the partial dir B may hold
// an index record.
func TestScanCancelledWalkRecordsNothing(t *testing.T) {
	root := t.TempDir()
	dirA := filepath.Join(root, "A")
	dirB := filepath.Join(root, "B")
	for _, f := range []string{
		filepath.Join(dirA, "ShowA.S01E01.mkv"),
		filepath.Join(dirB, "ShowB.S01E01.mkv"),
	} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cache := metaCacheDir(t)
	index := filepath.Join(t.TempDir(), "scanindex.json")

	origWalk := walkDir
	defer func() { walkDir = origWalk }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	walkDir = func(walkRoot string, fn fs.WalkDirFunc) error {
		return origWalk(walkRoot, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d != nil && d.IsDir() && filepath.Base(p) == "B" {
				cancel()
				return ctx.Err()
			}
			return fn(p, d, err)
		})
	}

	st, err := ScanWithOptions(ctx, indexScanOpts([]string{root}, cache, index, false))
	if err != nil {
		t.Fatalf("cancelled Scan err = %v, want nil (cancel is not failure)", err)
	}
	if !st.Cancelled {
		t.Fatalf("cancelled Scan Cancelled = false, want true (%+v)", st)
	}
	ix := LoadScanIndex(index)
	for _, d := range []string{root, dirA, dirB} {
		if rec, ok := ix.Lookup(d); ok {
			t.Errorf("cancelled walk recorded %q (files=%d keys=%d), want nothing recorded", d, rec.Files, len(rec.Keys))
		}
	}
}

// TestRecordIndexNeverStampsStaleFolder proves F2: a stamped record's hash,
// file count and key list must describe exactly the files that were walked.
// A file added after the plan was built (a concurrent filesystem change)
// must leave the folder unrecorded rather than stamped with a partial key
// list that a later scan would trust to skip the new file.
func TestRecordIndexNeverStampsStaleFolder(t *testing.T) {
	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "ShowA.S01E01.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, _, err := CollectFiles([]string{folder}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("walked files = %d, want 1", len(files))
	}
	plans, _ := BuildPlan(files)
	index := filepath.Join(t.TempDir(), "scanindex.json")

	// Concurrent change: a second file lands after the plan was built.
	if err := os.WriteFile(filepath.Join(folder, "ShowB.S01E01.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	RecordScan(index, []string{folder}, nil, plans)
	if rec, ok := LoadScanIndex(index).Lookup(folder); ok {
		t.Fatalf("stale record written for %q (files=%d keys=%v): key list is a subset of the folder's files", folder, rec.Files, rec.Keys)
	}

	// Positive control: a fresh plan covering both files records fine.
	files2, _, err := CollectFiles([]string{folder}, 0)
	if err != nil {
		t.Fatal(err)
	}
	plans2, _ := BuildPlan(files2)
	RecordScan(index, []string{folder}, nil, plans2)
	rec, ok := LoadScanIndex(index).Lookup(folder)
	if !ok {
		t.Fatal("fresh full plan recorded nothing, want a record")
	}
	if rec.Files != 2 {
		t.Errorf("recorded Files = %d, want 2", rec.Files)
	}
	if len(rec.Keys) != 2 {
		t.Errorf("recorded Keys = %d (%v), want 2 (one per show)", len(rec.Keys), rec.Keys)
	}
}

// TestShouldSkipValidityAware proves the skip guard's contract moved one
// level: the folder guard is a filesystem decision, and the verifier it is
// handed is what carries metadata liveness. The scan itself no longer
// consults the metadata cache to decide a walk skip (an unchanged folder with
// pending metadata is still skippable - the catch-up resolves those keys from
// the index), so this drives both: the guard itself with a liveness verifier,
// and the scan with the per-file state that replaced it.
func TestShouldSkipValidityAware(t *testing.T) {
	ctx := context.Background()
	live := func(cache string) func(string) bool {
		return func(k string) bool { return metadata.CacheLive(cache, k) }
	}

	// Live positive control: the scan indexes the folder, the catch-up fills
	// the metadata, and the guard then skips with a liveness verifier.
	posDir := t.TempDir()
	posName := "ShowP.S01E01.mkv"
	if err := os.WriteFile(filepath.Join(posDir, posName), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	posCache := metaCacheDir(t)
	posIndex := filepath.Join(t.TempDir(), "scanindex.json")
	restore := withFakeResolver("V1", nil)
	if _, err := ScanWithOptions(ctx, indexScanOpts([]string{posDir}, posCache, posIndex, false)); err != nil {
		restore()
		t.Fatalf("positive fill scan: %v", err)
	}
	StartCatchUp(CatchUpConfig{Dirs: []string{posDir}, CacheDir: posCache, IndexPath: posIndex}, nil).Wait()
	restore()
	posFP, err := FolderFingerprint(posDir)
	if err != nil {
		t.Fatalf("fingerprint positive dir: %v", err)
	}
	if !LoadScanIndex(posIndex).ShouldSkip(posDir, posFP, metadata.CacheSchema(), live(posCache)) {
		t.Errorf("ShouldSkip(live positive) = false, want true")
	}
	// The scan's own guard needs no cache at all: filesystem identity plus
	// per-file records is its whole contract.
	if !LoadScanIndex(posIndex).ShouldSkip(posDir, posFP, metadata.CacheSchema(), func(string) bool { return true }) {
		t.Errorf("ShouldSkip(filesystem-identical) = false, want true")
	}

	// Negative setup: a miss resolver persists a fresh negative entry.
	negDir := t.TempDir()
	negName := "ShowM.S01E01.mkv"
	negPath := filepath.Join(negDir, negName)
	if err := os.WriteFile(negPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	negCache := metaCacheDir(t)
	negIndex := filepath.Join(t.TempDir(), "scanindex.json")
	orig := newResolver
	miss := &missSource{}
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{miss}})
	}
	if _, err := ScanWithOptions(ctx, indexScanOpts([]string{negDir}, negCache, negIndex, false)); err != nil {
		newResolver = orig
		t.Fatalf("negative fill scan: %v", err)
	}
	// The catch-up persists the fresh negative entries the guard verifies.
	StartCatchUp(CatchUpConfig{Dirs: []string{negDir}, CacheDir: negCache, IndexPath: negIndex}, nil).Wait()
	newResolver = orig
	negFP, err := FolderFingerprint(negDir)
	if err != nil {
		t.Fatalf("fingerprint negative dir: %v", err)
	}
	negQ := metadata.ResolveQuery(negName, negPath)
	if negQ.Kind == metadata.KindUnknown {
		t.Fatalf("seed file %q parsed as KindUnknown", negName)
	}
	negKey := metadata.CacheKeyFor(negQ)
	negIx := LoadScanIndex(negIndex)
	if !metadata.CacheLive(negCache, negKey) {
		t.Fatalf("CacheLive(fresh negative) = false, want true")
	}
	if !negIx.ShouldSkip(negDir, negFP, metadata.CacheSchema(), live(negCache)) {
		t.Fatalf("ShouldSkip(fresh negative) = false, want true")
	}

	// Payload loss must stop the guard from skipping: the verifier is passed
	// in, so the guard honours liveness, not mere record presence. (The
	// expired-negative and corrupt-file cases moved with the per-key cache
	// files into internal/metadata's own store-backed cache tests.)
	if err := metadata.ClearCache(negCache); err != nil {
		t.Fatalf("clear metadata cache: %v", err)
	}
	if metadata.CacheLive(negCache, negKey) {
		t.Errorf("CacheLive(after clear) = true, want false")
	}
	if LoadScanIndex(negIndex).ShouldSkip(negDir, negFP, metadata.CacheSchema(), live(negCache)) {
		t.Errorf("ShouldSkip(no payload) = true, want false (a key with no live answer must not skip)")
	}
}

// TestCollectFilesWithContextHonoursCancel proves F4: the context-aware
// walk stops promptly on cancel (within the existing ~2s bound) and reports
// cancelled rather than failing.
func TestCollectFilesWithContextHonoursCancel(t *testing.T) {
	root := seedScanLib(t)
	origWalk := walkDir
	defer func() { walkDir = origWalk }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	var once sync.Once
	walkDir = func(walkRoot string, fn fs.WalkDirFunc) error {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return ctx.Err()
	}
	done := make(chan struct{})
	var denied int
	var cancelled bool
	var werr error
	var files []string
	go func() {
		defer close(done)
		files, denied, cancelled, werr = CollectFilesWithContext(ctx, []string{root}, 0)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("blocking walk never started")
	}
	time.Sleep(100 * time.Millisecond) // ensure we are parked in the walk
	cancelAt := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled walk did not return within 5s")
	}
	if latency := time.Since(cancelAt); latency > 2*time.Second {
		t.Errorf("cancel latency = %v, want <2s", latency)
	}
	if werr != nil {
		t.Errorf("cancelled walk err = %v, want nil (cancel is not failure)", werr)
	}
	if !cancelled {
		t.Errorf("cancelled walk cancelled = false, want true (files=%d denied=%d)", len(files), denied)
	}
}

// TestScanCancelledFetchCountsNoError proves F5: fetches that end because
// the context was cancelled are reported as cancelled/incomplete, never as
// failures: Errors stays 0.
// TestScanCancelledFetchCountsNoError proves a cancelled metadata lookup is an
// incomplete result, never a failure: cancelling the catch-up pass mid-lookup
// leaves no failed/missed count and parks nothing as needs_repair.
func TestScanCancelledFetchCountsNoError(t *testing.T) {
	root := seedScanLib(t)
	cache := metaCacheDir(t)
	index := filepath.Join(t.TempDir(), "scanindex.json")

	orig := newResolver
	defer func() { newResolver = orig }()
	slow := &slowCancelSource{name: "fake", tag: "V1", started: make(chan struct{}), release: make(chan struct{})}
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{slow}})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := ScanWithOptions(ctx, Options{Dirs: []string{root}, CacheDir: cache, IndexPath: index, UseIndex: true, SkipCatchUp: true}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	c := StartCatchUp(CatchUpConfig{Dirs: []string{root}, CacheDir: cache, IndexPath: index}, nil)
	select {
	case <-slow.started:
	case <-time.After(5 * time.Second):
		close(slow.release)
		StopCatchUp(index)
		t.Fatal("slow provider never started")
	}
	StopCatchUp(index)
	close(slow.release)
	c.Wait()

	stats := c.Stats()
	if stats.Failed != 0 {
		t.Errorf("cancelled catch-up Failed = %d, want 0 (a cancelled lookup is incomplete, not a failure)", stats.Failed)
	}
	if stats.Missed != 0 {
		t.Errorf("cancelled catch-up Missed = %d, want 0", stats.Missed)
	}
	if got := LoadScanIndex(index).State().FilesNeedsRepair; got != 0 {
		t.Errorf("cancelled catch-up parked %d records as needs_repair, want 0", got)
	}
}

// honestsSource answers one title, misses another, and fails a third:
// "Found Show" resolves, "Missing Show" is a definitive miss (nil, nil),
// "Unreachable Show" fails transiently (provider error).
type honestSource struct{}

func (honestSource) Name() string { return "honest" }

func (honestSource) Find(ctx context.Context, q metadata.Query) (*metadata.Media, error) {
	switch {
	case strings.Contains(q.Title, "Unreachable"):
		return nil, errors.New("boom")
	case strings.Contains(q.Title, "Missing"):
		return nil, nil
	default:
		m := &metadata.Media{
			Source: "honest", SourceID: "1",
			Kind: q.Kind, Title: q.Title, Year: q.Year,
			FetchedAt: time.Now().UTC(),
		}
		if q.Kind != metadata.KindMovie {
			m.Episodes = []metadata.Episode{{Season: q.Season, Number: q.Episode, Title: "Found"}}
		}
		return m, nil
	}
}

// TestScanTransientFailureHonestCounters proves the honest-counters contract
// end to end: a transient provider failure lands in KeysFailed (never
// KeysNotFound), a definitive miss still lands in KeysNotFound, the
// four-bucket invariant holds on every progress callback and the final
// Stats, and the transient failure persists nothing (exactly the positive
// plus the negative are on disk).
func TestScanTransientFailureHonestCounters(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for _, n := range []string{"Found.S01E01.mkv", "Missing.S01E01.mkv", "Unreachable.S01E01.mkv"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cache := metaCacheDir(t)

	orig := newResolver
	defer func() { newResolver = orig }()
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{honestSource{}}})
	}

	var mu sync.Mutex
	var calls []Progress
	st, err := Scan(ctx, []string{root}, false, cache, func(p Progress) {
		mu.Lock()
		calls = append(calls, p)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if st.Keys != 3 {
		t.Fatalf("stats Keys = %d, want 3 (%+v)", st.Keys, st)
	}
	if st.KeysFetched != 1 || st.KeysNotFound != 1 || st.KeysFailed != 1 || st.KeysCached != 0 {
		t.Errorf("key buckets = fetched %d notfound %d failed %d cached %d, want 1/1/1/0 (%+v)",
			st.KeysFetched, st.KeysNotFound, st.KeysFailed, st.KeysCached, st)
	}
	if st.KeysDone != 3 {
		t.Errorf("stats KeysDone = %d, want 3 (%+v)", st.KeysDone, st)
	}
	if st.KeysDone != st.KeysCached+st.KeysFetched+st.KeysNotFound+st.KeysFailed || st.KeysDone != st.Keys {
		t.Errorf("stats invariant KeysDone %d != sum / Keys %d (%+v)", st.KeysDone, st.Keys, st)
	}
	for i, p := range calls {
		if p.KeysDone != p.KeysCached+p.KeysFetched+p.KeysNotFound+p.KeysFailed {
			t.Errorf("callback %d invariant KeysDone %d != Cached %d + Fetched %d + NotFound %d + Failed %d (%+v)",
				i, p.KeysDone, p.KeysCached, p.KeysFetched, p.KeysNotFound, p.KeysFailed, p)
		}
	}
	// Per-file verdicts stay honest too: 1 resolved, 1 not-found, 1 error.
	if st.Resolved != 1 || st.NotFound != 1 || st.Errors != 1 {
		t.Errorf("file verdicts = resolved %d notfound %d errors %d, want 1/1/1 (%+v)", st.Resolved, st.NotFound, st.Errors, st)
	}
	// The transient failure persisted nothing: the fetched positive and the
	// definitive-miss negative are both in the cache, and the key whose
	// provider failed is not.
	for _, tc := range []struct {
		name string
		key  string
		want bool
	}{
		{"Found.S01E01.mkv", metadata.CacheKeyFor(metadata.ResolveQuery("Found.S01E01.mkv", filepath.Join(root, "Found.S01E01.mkv"))), true},
		{"Missing.S01E01.mkv", metadata.CacheKeyFor(metadata.ResolveQuery("Missing.S01E01.mkv", filepath.Join(root, "Missing.S01E01.mkv"))), true},
		{"Unreachable.S01E01.mkv", metadata.CacheKeyFor(metadata.ResolveQuery("Unreachable.S01E01.mkv", filepath.Join(root, "Unreachable.S01E01.mkv"))), false},
	} {
		if got := metadata.CacheHas(cache, tc.key); got != tc.want {
			t.Errorf("cache entry for %s = %t, want %t (transient failures persist nothing)", tc.name, got, tc.want)
		}
	}
}

// TestScanUseIndexOffMatchesPlainScan proves verdict equivalence with the
// index off: Scan (public) and ScanWithOptions{UseIndex:false} over
// identical corpora and identically-seeded caches produce identical Stats
// and identical final Progress payloads.
func TestScanUseIndexOffMatchesPlainScan(t *testing.T) {
	ctx := context.Background()
	// One episode per cache key: same-key concurrent episodes would join
	// a single inflight fetch and resolve with the wrong episode
	// (scheduling-nondeterministic), so the corpus keeps every key
	// unique to compare exact verdicts.
	seedUnique := func(t *testing.T) string {
		t.Helper()
		root := t.TempDir()
		for _, f := range []string{
			filepath.Join(root, "A", "UniqShowA.S01E01.mkv"),
			filepath.Join(root, "B", "UniqShowB.S02E03.mkv"),
			filepath.Join(root, "B", "UniqShowC.S01E01.mkv"),
			filepath.Join(root, "Movies", "UniqFilm.2001.mkv"),
			filepath.Join(root, "Extras", "UniqShowA trailer.mkv"),
		} {
			if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return root
	}
	run := func(scan func(ctx context.Context, root, cache string, prog func(Progress)) (Stats, error)) (Stats, Progress) {
		root := seedUnique(t)
		cache := metaCacheDir(t)
		restore := withFakeResolver("V1", nil)
		defer restore()
		var last Progress
		st, err := scan(ctx, root, cache, func(p Progress) { last = p })
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		last.Last = "" // completion order is nondeterministic; values must match
		return st, last
	}
	stPlain, lastPlain := run(func(ctx context.Context, root, cache string, prog func(Progress)) (Stats, error) {
		return Scan(ctx, []string{root}, false, cache, prog)
	})
	stOpt, lastOpt := run(func(ctx context.Context, root, cache string, prog func(Progress)) (Stats, error) {
		return ScanWithOptions(ctx, Options{Dirs: []string{root}, Force: false, CacheDir: cache, Progress: prog})
	})
	if stPlain != stOpt {
		t.Errorf("Stats differ:\nplain = %+v\nopt   = %+v", stPlain, stOpt)
	}
	if lastPlain != lastOpt {
		t.Errorf("final Progress differs:\nplain = %+v\nopt   = %+v", lastPlain, lastOpt)
	}
}

// TestScanIndexedFirstPassMatchesPlainTallies proves the first indexed scan
// (empty index, everything walked) sees the same corpus as a plain Scan: every
// file/folder tally matches. The metadata verdicts that used to be produced
// inline are the catch-up's now, so the test drives the catch-up and checks
// that the same key count resolves.
func TestScanIndexedFirstPassMatchesPlainTallies(t *testing.T) {
	ctx := context.Background()
	// Deterministic corpus: one episode per cache key (same-key
	// concurrent episodes would join a single inflight fetch and resolve
	// with the first completer's media, which is
	// scheduling-nondeterministic).
	root := t.TempDir()
	for _, f := range []string{
		filepath.Join(root, "A", "UniqShowA.S01E01.mkv"),
		filepath.Join(root, "B", "UniqShowB.S02E03.mkv"),
		filepath.Join(root, "B", "UniqShowC.S01E01.mkv"),
		filepath.Join(root, "Movies", "UniqFilm.2001.mkv"),
		filepath.Join(root, "Extras", "UniqShowA trailer.mkv"),
	} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	restore := withFakeResolver("V1", nil)
	defer restore()
	cache := metaCacheDir(t)
	stPlain, err := Scan(ctx, []string{root}, false, cache, nil)
	if err != nil {
		t.Fatalf("plain Scan: %v", err)
	}
	index := filepath.Join(t.TempDir(), "scanindex.json")
	stIdx, err := ScanWithOptions(ctx, indexScanOpts([]string{root}, cache, index, false))
	if err != nil {
		t.Fatalf("indexed Scan: %v", err)
	}
	if stIdx.Files != stPlain.Files || stIdx.Skipped != stPlain.Skipped ||
		stIdx.FoldersScanned != stPlain.FoldersScanned || stIdx.FoldersSkipped != stPlain.FoldersSkipped {
		t.Errorf("corpus tallies differ:\nplain = %+v\nindex = %+v", stPlain, stIdx)
	}
	if stIdx.FoldersSkipped != 0 || stIdx.FilesSkipped != 0 {
		t.Errorf("first indexed pass skipped folders/files %+v, want none (empty index walks all)", stIdx)
	}
	if stIdx.Pending != stPlain.Files-stPlain.Skipped {
		t.Errorf("indexed Pending = %d, want %d (resolvable files wait for the catch-up)", stIdx.Pending, stPlain.Files-stPlain.Skipped)
	}
	// The catch-up resolves them: the same keys the plain scan resolved.
	StartCatchUp(CatchUpConfig{Dirs: []string{root}, CacheDir: cache, IndexPath: index}, nil).Wait()
	state := LoadScanIndex(index).State()
	if state.Keys != stPlain.Keys {
		t.Errorf("index keys = %d, want plain Keys %d", state.Keys, stPlain.Keys)
	}
	if state.KeysResolved != stPlain.Keys {
		t.Errorf("caught-up resolved keys = %d, want %d (every key resolvable)", state.KeysResolved, stPlain.Keys)
	}
}

// TestCollectSkippedSetsIndexedSecondPass proves the skipped/visited folder
// sets: after one indexed scan and its catch-up (so the recorded keys have
// live metadata, which is what CollectIndexed's guard verifies), an
// index-aware collect skips exactly the three corpus dirs with the recorded
// file counts, and the second scan's tallies agree.
func TestCollectSkippedSetsIndexedSecondPass(t *testing.T) {
	dirs, _ := seedIndexCorpus(t)
	cache := metaCacheDir(t)
	index := filepath.Join(t.TempDir(), "scanindex.json")
	ctx := context.Background()

	restore := withFakeResolver("V1", nil)
	defer restore()
	st1, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, false))
	if err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	StartCatchUp(CatchUpConfig{Dirs: dirs, CacheDir: cache, IndexPath: index}, nil).Wait()
	cr, err := CollectIndexed([]string{dirs[0], dirs[1], dirs[2]}, cache, index, true, false)
	if err != nil {
		t.Fatalf("CollectIndexed: %v", err)
	}
	if len(cr.Visited) != 0 {
		t.Errorf("Visited = %v, want none (all skipped)", cr.Visited)
	}
	if len(cr.Skipped) != 3 {
		t.Fatalf("Skipped dirs = %d, want 3 (%v)", len(cr.Skipped), cr.Skipped)
	}
	for _, d := range dirs {
		if _, ok := cr.Skipped[normalizeScanDir(d)]; !ok {
			t.Errorf("skipped set lacks %q (%v)", d, cr.Skipped)
		}
	}
	if cr.FilesSkipped != st1.Files {
		t.Errorf("FilesSkipped = %d, want scan-1 Files %d", cr.FilesSkipped, st1.Files)
	}
	st2, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, false))
	if err != nil {
		t.Fatalf("scan 2: %v", err)
	}
	if st2.FoldersScanned != 0 || st2.FoldersSkipped != 3 || st2.FilesSkipped != st1.Files {
		t.Errorf("scan 2 folders = %+v, want Scanned:0 Skipped:3 FilesSkipped:%d", st2, st1.Files)
	}
}

// TestScanCancelFetchPhaseKeepsCheckpoint proves a cancelled catch-up keeps
// what it applied: verdicts written before the cancel are committed and
// survive a reopen of the index, and unfinished keys stay pending (no
// invented failures).
func TestScanCancelFetchPhaseKeepsCheckpoint(t *testing.T) {
	_, parent := seedIndexCorpus(t)
	root := parent
	cache := metaCacheDir(t)
	index := filepath.Join(t.TempDir(), "scanindex.json")

	orig := newResolver
	defer func() { newResolver = orig }()
	// Three lookups land (the catch-up's concurrency), every later one blocks:
	// the pass is cancelled with real, order-independent progress in hand.
	block := &firstNThenBlockSource{name: "fake", n: catchUpConcurrency, release: make(chan struct{})}
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{block}})
	}
	if _, err := ScanWithOptions(context.Background(), Options{Dirs: []string{root}, CacheDir: cache, IndexPath: index, UseIndex: true, SkipCatchUp: true}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	c := StartCatchUp(CatchUpConfig{Dirs: []string{root}, CacheDir: cache, IndexPath: index}, nil)
	deadline := time.Now().Add(5 * time.Second)
	for LoadScanIndex(index).State().KeysResolved == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	StopCatchUp(index)
	close(block.release)
	c.Wait()

	state := LoadScanIndex(index).State()
	if state.KeysResolved == 0 {
		t.Fatalf("cancelled catch-up kept no applied verdicts in memory (%+v)", state)
	}
	// Reopen the store from disk: the applied verdicts must have survived.
	if err := CloseIndex(index); err != nil {
		t.Fatalf("close index: %v", err)
	}
	reopened := LoadScanIndex(index).State()
	if reopened.KeysResolved == 0 {
		t.Errorf("reopened index lost every applied verdict (%+v)", reopened)
	}
	if reopened.FilesNeedsRepair != 0 {
		t.Errorf("cancelled catch-up parked %d records as needs_repair, want 0", reopened.FilesNeedsRepair)
	}
}

// firstNThenBlockSource resolves its first n lookups immediately and blocks
// every later one until ctx is done or release is closed. With the catch-up's
// concurrency of 3 it pins "some verdicts land, then the pass is cancelled
// mid-flight" without depending on lookup order.
type firstNThenBlockSource struct {
	name    string
	n       int
	calls   atomic.Int32
	release chan struct{}
}

func (s *firstNThenBlockSource) Name() string { return s.name }

func (s *firstNThenBlockSource) Find(ctx context.Context, q metadata.Query) (*metadata.Media, error) {
	if int(s.calls.Add(1)) > s.n {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.release:
		}
	}
	m := &metadata.Media{
		Source: "fake", SourceID: "1",
		Kind: q.Kind, Title: q.Title, Year: q.Year,
		FetchedAt: time.Now().UTC(),
	}
	if q.Kind != metadata.KindMovie {
		m.Episodes = []metadata.Episode{{Season: q.Season, Number: q.Episode, Title: "V1"}}
	}
	return m, nil
}

// TestStaleRecordScanRefusesNewSubdir is the finding-1 regression: a
// RecordScan stamped from stale walk state (plans missing a file added
// under a brand-new subdirectory afterwards) must refuse every affected
// dir. The pre-fix rollup kept the walked count while the stamped names
// hash already covered the new names, so the next scan skipped the new
// files entirely.
func TestStaleRecordScanRefusesNewSubdir(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	aPath := filepath.Join(root, "ShowA.S01E01.mkv")
	if err := os.WriteFile(aPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := metaCacheDir(t)
	index := filepath.Join(t.TempDir(), "scanindex.json")
	restore := withFakeResolver("V1", nil)
	defer restore()

	if _, err := ScanWithOptions(ctx, indexScanOpts([]string{root}, cache, index, false)); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	// Stale walk state: visited/plans captured before New/B.mkv exists.
	crForce, _, err := CollectIndexedWithContext(ctx, []string{root}, cache, index, true, true)
	if err != nil {
		t.Fatalf("force collect: %v", err)
	}
	plans, _ := BuildPlan(crForce.Files)
	bPath := filepath.Join(root, "New", "ShowB.S01E01.mkv")
	if err := os.MkdirAll(filepath.Dir(bPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	RecordScan(index, crForce.Visited, crForce.Skipped, plans)
	cr2, _, err := CollectIndexedWithContext(ctx, []string{root}, cache, index, true, false)
	if err != nil {
		t.Fatalf("scan 2: %v", err)
	}
	for _, want := range []string{aPath, bPath} {
		found := false
		for _, f := range cr2.Files {
			if f == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("scan 2 files %v: missing %q (stale stamp skipped it)", cr2.Files, want)
		}
	}
}

// TestMidWalkAddUsesFreshDecision is the finding-2 regression: a file added
// to a child dir after the parent's fingerprint but before the child's skip
// decision (with the child's mtime restored) must still be found. The
// pre-fix decision hashed the listing cached in the earlier pass, which
// predates the add and still matches the record, so the child skipped.
func TestMidWalkAddUsesFreshDecision(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	child := filepath.Join(root, "Child")
	aPath := filepath.Join(child, "ShowA.S01E01.mkv")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(aPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := metaCacheDir(t)
	index := filepath.Join(t.TempDir(), "scanindex.json")
	restore := withFakeResolver("V1", nil)
	defer restore()

	if _, err := ScanWithOptions(ctx, indexScanOpts([]string{root}, cache, index, false)); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	cfi, err := os.Stat(child)
	if err != nil {
		t.Fatal(err)
	}
	childMtime := cfi.ModTime()
	// A benign root-level change forces scan 2 to descend past the root.
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bPath := filepath.Join(child, "ShowB.S01E01.mkv")
	origWalk := walkDir
	defer func() { walkDir = origWalk }()
	mutated := false
	walkDir = func(wroot string, fn fs.WalkDirFunc) error {
		return origWalk(wroot, func(p string, d fs.DirEntry, err error) error {
			if !mutated && err == nil && d != nil && d.IsDir() && normalizeScanDir(p) == normalizeScanDir(child) {
				mutated = true
				if err := os.WriteFile(bPath, []byte("x"), 0o644); err != nil {
					t.Errorf("mid-walk add: %v", err)
				}
				if err := os.Chtimes(child, childMtime, childMtime); err != nil {
					t.Errorf("mid-walk mtime restore: %v", err)
				}
				fi, serr := os.Stat(child)
				if serr != nil || !fi.ModTime().Equal(childMtime) {
					t.Skipf("platform cannot restore dir mtime exactly, hook unverifiable here")
				}
			}
			return fn(p, d, err)
		})
	}
	st2, err := ScanWithOptions(ctx, indexScanOpts([]string{root}, cache, index, false))
	if err != nil {
		t.Fatalf("scan 2: %v", err)
	}
	if !mutated {
		t.Fatal("mid-walk hook never fired")
	}
	if st2.Files != 2 {
		t.Errorf("scan 2 Files = %d, want 2 (ShowA + mid-walk ShowB); filesSkipped=%d foldersSkipped=%d", st2.Files, st2.FilesSkipped, st2.FoldersSkipped)
	}
}
