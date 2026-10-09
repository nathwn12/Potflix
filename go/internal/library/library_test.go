package library

import (
	"context"
	"encoding/json"
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

func cacheModTimes(t *testing.T, dir string) map[string]time.Time {
	t.Helper()
	out := map[string]time.Time{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = fi.ModTime()
	}
	return out
}

// TestScanIncrementalAndForce proves the Scan semantics end to end against a
// temp library and a temp cache dir: run 1 resolves and caches (one fetch per
// unique key), run 2 without force is a no-op (zero fetches, cache files
// untouched), run 3 with force refetches and rewrites every entry.
func TestScanIncrementalAndForce(t *testing.T) {
	root := seedScanLib(t)
	cache := t.TempDir()
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
	before := cacheModTimes(t, cache)
	if len(before) != 2 {
		t.Fatalf("cache files = %d, want 2 (one per show)", len(before))
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
	for name, mt := range cacheModTimes(t, cache) {
		if !mt.Equal(before[name]) {
			t.Errorf("run 2 rewrote %q (mtime changed without force)", name)
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
	after := cacheModTimes(t, cache)
	for name, mt := range after {
		if !mt.After(before[name]) {
			t.Errorf("run 3 did not rewrite %q (mtime %v, want after %v)", name, mt, before[name])
		}
	}
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
	cache := t.TempDir()
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

func indexScanOpts(dirs []string, cache, index string, force bool) Options {
	return Options{Dirs: dirs, Force: force, CacheDir: cache, IndexPath: index, UseIndex: true}
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
	cache := t.TempDir()
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
	st2, err := ScanWithOptions(ctx, Options{Dirs: dirs, CacheDir: cache, IndexPath: index, UseIndex: true, Progress: func(p Progress) { last = p }})
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
	cache := t.TempDir()
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
	cache := t.TempDir()
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
	cache := t.TempDir()
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

// TestIndexMissingCacheKeyDisablesSkip proves the skip guard: deleting one
// key's cache file forces its folder to rescan.
func TestIndexMissingCacheKeyDisablesSkip(t *testing.T) {
	dirs, _ := seedIndexCorpus(t)
	cache := t.TempDir()
	index := filepath.Join(t.TempDir(), "scanindex.json")
	ctx := context.Background()

	restore := withFakeResolver("V1", nil)
	defer restore()
	if _, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, false)); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	q := metadata.ResolveQuery("ShowA.S01E01.mkv", filepath.Join(dirs[0], "ShowA.S01E01.mkv"))
	key := metadata.CacheKeyFor(q)
	if err := os.Remove(filepath.Join(cache, metadata.CacheFileNameFor(key))); err != nil {
		t.Fatalf("remove cache file: %v", err)
	}
	st2, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, false))
	if err != nil {
		t.Fatalf("scan 2: %v", err)
	}
	if st2.FoldersScanned != 1 || st2.FoldersSkipped != 2 {
		t.Errorf("missing key: folders = %d scanned %d skipped, want 1/2 (%+v)", st2.FoldersScanned, st2.FoldersSkipped, st2)
	}
}

// TestIndexForceRewritesWithoutSkipping proves force=true walks everything
// and rewrites records.
func TestIndexForceRewritesWithoutSkipping(t *testing.T) {
	dirs, _ := seedIndexCorpus(t)
	cache := t.TempDir()
	index := filepath.Join(t.TempDir(), "scanindex.json")
	ctx := context.Background()

	restore := withFakeResolver("V1", nil)
	defer restore()
	if _, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, false)); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	before, err := os.Stat(index)
	if err != nil {
		t.Fatalf("stat index: %v", err)
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
	after, err := os.Stat(index)
	if err != nil {
		t.Fatalf("stat index after: %v", err)
	}
	if !after.ModTime().After(before.ModTime()) {
		t.Errorf("force did not rewrite index (mtime %v, want after %v)", after.ModTime(), before.ModTime())
	}
}

// TestIndexSchemaBumpSkipsNothing proves records under an older schema
// never skip: rewriting every record's schema to a stale value forces a
// full walk.
func TestIndexSchemaBumpSkipsNothing(t *testing.T) {
	dirs, _ := seedIndexCorpus(t)
	cache := t.TempDir()
	index := filepath.Join(t.TempDir(), "scanindex.json")
	ctx := context.Background()

	restore := withFakeResolver("V1", nil)
	defer restore()
	if _, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, false)); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	data, err := os.ReadFile(index)
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	var raw map[string]FolderRecord
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal index: %v", err)
	}
	for k, v := range raw {
		v.Schema = metadata.CacheSchema() + 1
		raw[k] = v
	}
	data, err = json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(index, data, 0o644); err != nil {
		t.Fatal(err)
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
// a full walk, never an error.
func TestIndexMissingOrCorruptIsFullWalk(t *testing.T) {
	dirs, _ := seedIndexCorpus(t)
	cache := t.TempDir()
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
	if err := os.WriteFile(index, []byte("{{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	st2, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, false))
	if err != nil {
		t.Fatalf("corrupt index scan: %v", err)
	}
	if st2.FoldersScanned != 3 || st2.FoldersSkipped != 0 {
		t.Errorf("corrupt index: folders = %d scanned %d skipped, want 3/0 (%+v)", st2.FoldersScanned, st2.FoldersSkipped, st2)
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

// TestIndexAllNegativeFolderSkipped proves folders whose files all
// resolved not-found are still skippable: the first pass persists a
// negative entry per key (so CacheHas passes), and the second pass skips
// the folder entirely (FoldersScanned==0) with the numbers staying
// honest (first pass NotFound==files, second pass FilesSkipped==files).
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
	cache := t.TempDir()
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
	if st1.Files != 2 || st1.NotFound != 2 || st1.Resolved != 0 {
		t.Fatalf("scan 1 stats = %+v, want {Files:2 NotFound:2 Resolved:0}", st1)
	}
	if st1.FoldersScanned != 1 || st1.FoldersSkipped != 0 {
		t.Fatalf("scan 1 folders = %d scanned %d skipped, want 1/0 (%+v)", st1.FoldersScanned, st1.FoldersSkipped, st1)
	}
	// Every recorded key must now satisfy the skip guard via its
	// persisted negative entry.
	for _, n := range []string{"ShowM.S01E01.mkv", "ShowN.S01E01.mkv"} {
		q := metadata.ResolveQuery(n, filepath.Join(folder, n))
		if q.Kind == metadata.KindUnknown {
			t.Fatalf("seed file %q parsed as KindUnknown", n)
		}
		if !metadata.CacheHas(cache, metadata.CacheKeyFor(q)) {
			t.Fatalf("CacheHas = false for negative-cached key %q, want true", metadata.CacheKeyFor(q))
		}
	}

	// Second pass: a fresh miss provider must never be called - the
	// folder skips on its negative entries.
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
		t.Errorf("scan 2 provider calls = %d, want 0 (negative-cached folder skipped, no network)", n)
	}
}

// TestScanKeysFullyCached proves cached work is pre-credited: after a fill,
// a non-force re-scan's FIRST progress callback already reports
// KeysDone == Keys (== KeysTotal) with KeysFetched == 0, and the final
// Stats carry Keys == KeysDone == KeysCached.
func TestScanKeysFullyCached(t *testing.T) {
	root := seedScanLib(t) // 3 files, 2 unique keys, all resolvable
	cache := t.TempDir()
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
	cache := t.TempDir()
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

// TestScanCancelReturnsPartialQuickly proves prompt cancellation: with a
// slow ctx-aware provider, cancelling mid-fetch returns within ~1s with
// Cancelled=true and a nil error, and the index file exists and parses.
func TestScanCancelReturnsPartialQuickly(t *testing.T) {
	root := seedScanLib(t)
	cache := t.TempDir()
	index := filepath.Join(t.TempDir(), "scanindex.json")

	orig := newResolver
	defer func() { newResolver = orig }()
	slow := &slowCancelSource{name: "fake", tag: "V1", started: make(chan struct{}), release: make(chan struct{})}
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{slow}})
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var st Stats
	var scanErr error
	go func() {
		defer close(done)
		st, scanErr = ScanWithOptions(ctx, Options{Dirs: []string{root}, CacheDir: cache, IndexPath: index, UseIndex: true})
	}()
	select {
	case <-slow.started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("slow provider never started")
	}
	cancelAt := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled Scan did not return within 5s")
	}
	latency := time.Since(cancelAt)
	t.Logf("cancel latency=%dms", latency.Milliseconds())
	if latency > 2*time.Second {
		t.Errorf("cancel latency = %v, want <2s (~1s)", latency)
	}
	if scanErr != nil {
		t.Errorf("cancelled Scan err = %v, want nil (cancel is not failure)", scanErr)
	}
	if !st.Cancelled {
		t.Errorf("cancelled Scan Cancelled = false, want true (%+v)", st)
	}
	fi, err := os.Stat(index)
	if err != nil {
		t.Fatalf("index file missing after cancel: %v", err)
	}
	if fi.Size() == 0 {
		t.Errorf("index file empty after cancel")
	}
	data, err := os.ReadFile(index)
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	var raw map[string]FolderRecord
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("index file does not parse: %v", err)
	}
}

// TestScanResumeAfterCancelSkipsCachedFolders proves a resume over the same
// corpus with the same index skips folders whose keys are all present and
// never calls the provider for them.
func TestScanResumeAfterCancelSkipsCachedFolders(t *testing.T) {
	root := seedScanLib(t) // A: ShowA; B: ShowA+ShowB
	cache := t.TempDir()
	index := filepath.Join(t.TempDir(), "scanindex.json")

	orig := newResolver
	defer func() { newResolver = orig }()

	// Cancelled run: ShowA fast, ShowB blocks until cancel.
	split := &splitSpeedSource{name: "fake", tag: "V1", release: make(chan struct{})}
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{split}})
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var st1 Stats
	go func() {
		defer close(done)
		st1, _ = ScanWithOptions(ctx, Options{Dirs: []string{root}, CacheDir: cache, IndexPath: index, UseIndex: true})
	}()
	time.Sleep(200 * time.Millisecond) // let ShowA finish, ShowB block
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled Scan did not return")
	}
	if !st1.Cancelled {
		t.Fatalf("first Scan Cancelled = false, want true (%+v)", st1)
	}
	close(split.release)

	// Resume with a counting fast provider: ShowA cached, ShowB missing.
	fast := &fakeSource{name: "fake", tag: "V1"}
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{fast}})
	}
	st2, err := ScanWithOptions(context.Background(), Options{Dirs: []string{root}, CacheDir: cache, IndexPath: index, UseIndex: true})
	if err != nil {
		t.Fatalf("resume Scan: %v", err)
	}
	if st2.Cancelled {
		t.Errorf("resume Cancelled = true, want false")
	}
	if st2.FoldersSkipped < 1 {
		t.Errorf("resume FoldersSkipped = %d, want >=1 (ShowA-only folder) (%+v)", st2.FoldersSkipped, st2)
	}
	if n := fast.calls.Load(); n != 1 {
		t.Errorf("resume provider calls = %d, want 1 (ShowB only, ShowA served from cache)", n)
	}
}

// TestScanCheckpointExistsBeforeFetchCompletes proves mid-scan flushing:
// with a corpus big enough to flush (>=50 folders), the index file exists
// while a blocking provider still holds the fetch phase open.
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
	cache := t.TempDir()
	index := filepath.Join(t.TempDir(), "scanindex.json")

	orig := newResolver
	defer func() { newResolver = orig }()
	block := &slowCancelSource{name: "fake", tag: "V1", started: make(chan struct{}), release: make(chan struct{})}
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{block}})
	}
	done := make(chan struct{})
	var st Stats
	var scanErr error
	go func() {
		defer close(done)
		st, scanErr = ScanWithOptions(context.Background(), Options{Dirs: []string{root}, CacheDir: cache, IndexPath: index, UseIndex: true})
	}()
	select {
	case <-block.started:
	case <-time.After(10 * time.Second):
		t.Fatal("blocking provider never started")
	}
	// Fetch is blocked: the walk + pre-fetch checkpoint must have flushed.
	fi, err := os.Stat(index)
	if err != nil {
		t.Fatalf("index file missing while fetch blocked: %v", err)
	}
	if fi.Size() == 0 {
		t.Errorf("index file empty while fetch blocked")
	}
	close(block.release)
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Scan did not complete after release")
	}
	if scanErr != nil {
		t.Fatalf("Scan after release: %v", scanErr)
	}
	_ = st
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
	cache := t.TempDir()
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

// TestShouldSkipValidityAware proves F3: the skip guard verifies keys by
// liveness (metadata.CacheLive), not mere file existence. An expired
// negative entry or a corrupt cache file must NOT satisfy the guard (no
// skip), while a live positive and a fresh (unexpired) negative still do.
func TestShouldSkipValidityAware(t *testing.T) {
	ctx := context.Background()
	live := func(cache string) func(string) bool {
		return func(k string) bool { return metadata.CacheLive(cache, k) }
	}

	// Live positive control: fill via a fake resolver and index the folder.
	posDir := t.TempDir()
	posName := "ShowP.S01E01.mkv"
	if err := os.WriteFile(filepath.Join(posDir, posName), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	posCache := t.TempDir()
	posIndex := filepath.Join(t.TempDir(), "scanindex.json")
	restore := withFakeResolver("V1", nil)
	if _, err := ScanWithOptions(ctx, indexScanOpts([]string{posDir}, posCache, posIndex, false)); err != nil {
		restore()
		t.Fatalf("positive fill scan: %v", err)
	}
	restore()
	posFP, err := FolderFingerprint(posDir)
	if err != nil {
		t.Fatalf("fingerprint positive dir: %v", err)
	}
	if !LoadScanIndex(posIndex).ShouldSkip(posDir, posFP, metadata.CacheSchema(), live(posCache)) {
		t.Errorf("ShouldSkip(live positive) = false, want true")
	}

	// Negative setup: a miss resolver persists a fresh negative entry.
	negDir := t.TempDir()
	negName := "ShowM.S01E01.mkv"
	negPath := filepath.Join(negDir, negName)
	if err := os.WriteFile(negPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	negCache := t.TempDir()
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
	if !negIx.ShouldSkip(negDir, negFP, metadata.CacheSchema(), live(negCache)) {
		t.Fatalf("ShouldSkip(fresh negative) = false, want true")
	}

	// Expire the negative entry by backdating its CheckedAt past the TTL.
	entries, err := os.ReadDir(negCache)
	if err != nil || len(entries) != 1 {
		t.Fatalf("cache entries = %v, %v; want exactly 1 negative file", entries, err)
	}
	negFile := filepath.Join(negCache, entries[0].Name())
	raw, err := os.ReadFile(negFile)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("negative cache file does not parse: %v", err)
	}
	doc["checkedAt"] = time.Now().Add(-31 * 24 * time.Hour).UTC().Format(time.RFC3339)
	backdated, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(negFile, backdated, 0o644); err != nil {
		t.Fatal(err)
	}
	if metadata.CacheLive(negCache, negKey) {
		t.Errorf("CacheLive(expired negative) = true, want false")
	}
	if LoadScanIndex(negIndex).ShouldSkip(negDir, negFP, metadata.CacheSchema(), live(negCache)) {
		t.Errorf("ShouldSkip(expired negative) = true, want false (stale miss must rescan)")
	}

	// Corrupt the cache file: existence alone must never satisfy the guard.
	if err := os.WriteFile(negFile, []byte("corrupt{{{not-json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if metadata.CacheLive(negCache, negKey) {
		t.Errorf("CacheLive(corrupt entry) = true, want false")
	}
	if LoadScanIndex(negIndex).ShouldSkip(negDir, negFP, metadata.CacheSchema(), live(negCache)) {
		t.Errorf("ShouldSkip(corrupt entry) = true, want false")
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
func TestScanCancelledFetchCountsNoError(t *testing.T) {
	root := seedScanLib(t)
	cache := t.TempDir()
	index := filepath.Join(t.TempDir(), "scanindex.json")

	orig := newResolver
	defer func() { newResolver = orig }()
	slow := &slowCancelSource{name: "fake", tag: "V1", started: make(chan struct{}), release: make(chan struct{})}
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{slow}})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var st Stats
	var scanErr error
	go func() {
		defer close(done)
		st, scanErr = ScanWithOptions(ctx, Options{Dirs: []string{root}, CacheDir: cache, IndexPath: index, UseIndex: true})
	}()
	select {
	case <-slow.started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("slow provider never started")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled Scan did not return within 5s")
	}
	if scanErr != nil {
		t.Errorf("cancelled Scan err = %v, want nil (cancel is not failure)", scanErr)
	}
	if !st.Cancelled {
		t.Errorf("cancelled Scan Cancelled = false, want true (%+v)", st)
	}
	if st.Errors != 0 {
		t.Errorf("cancelled Scan Errors = %d, want 0 (cancelled fetches are incomplete, not failures) (%+v)", st.Errors, st)
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
	cache := t.TempDir()

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
	// The transient failure persisted nothing: exactly the fetched positive
	// plus the definitive-miss negative are on disk.
	entries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("cache dir has %d entries, want 2 (positive + negative, transient wrote nothing)", len(entries))
	}
}
