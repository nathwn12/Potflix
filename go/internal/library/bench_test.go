// Benchmark harness for the library hot path (measurement only).
//
// These benchmarks change no production behaviour: every cache lives under
// b.TempDir() (never %LOCALAPPDATA%\Potflix\meta) and no benchmark can reach
// the real network (in-process fakeSource or loopback httptest only).
// Baseline table: BENCH.md (this directory).
package library

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"potflix/internal/metadata"
)

var (
	benchPlansSink  []FilePlan
	benchGroupsSink []*Group
	benchStatsSink  Stats
	benchFetchSink  map[string]FetchResult
)

// benchPlanPaths generates n deterministic synthetic paths spread over
// folders directories (fixed seed, so before/after runs compare the same
// tree). Patterns mirror the real library: SxxEyy series, absolute-numbered
// anime, movies, and folder-context bare numbers.
func benchPlanPaths(seed int64, n, folders int) []string {
	rng := rand.New(rand.NewSource(seed))
	paths := make([]string, 0, n)
	for i := 0; i < n; i++ {
		folder := rng.Intn(folders)
		show := rng.Intn(900) + 100
		season := rng.Intn(6) + 1
		ep := rng.Intn(24) + 1
		dir := fmt.Sprintf(`D:\bench\lib\folder%03d`, folder)
		var base string
		switch rng.Intn(4) {
		case 0:
			base = fmt.Sprintf("Show%03d.S%02dE%02d.1080p.WEB-DL.x264-GRP.mkv", show, season, ep)
		case 1:
			base = fmt.Sprintf("[GRP] Anime Show %03d - %03d [1080p].mkv", show, rng.Intn(200)+1)
		case 2:
			base = fmt.Sprintf("Movie Title %03d (%d) 2160p.BluRay.x265-GRP.mkv", show, 1990+rng.Intn(35))
		default:
			dir = fmt.Sprintf(`D:\bench\lib\Show%03d\Season %d`, show, season)
			base = fmt.Sprintf("%02d.mkv", ep)
		}
		paths = append(paths, dir+`\`+base)
	}
	return paths
}

// BenchmarkBuildPlan7847 measures pure parse+dedupe over 7,847 synthetic
// paths across ~700 folders (no filesystem access).
func BenchmarkBuildPlan7847(b *testing.B) {
	files := benchPlanPaths(1, 7847, 700)
	if len(files) != 7847 {
		b.Fatalf("paths = %d, want 7847", len(files))
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		plans, groups := BuildPlan(files)
		benchPlansSink, benchGroupsSink = plans, groups
	}
}

// benchWriteVideo creates one tiny video file under root/sub/name.
func benchWriteVideo(b *testing.B, root, sub, name string) {
	b.Helper()
	dir := filepath.Join(root, sub)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
		b.Fatal(err)
	}
}

// BenchmarkScanWarm measures the warm-scan floor: 50 unique keys are
// pre-populated into a temp cache via the test's fake lookup, then
// Scan(force=false) is timed. Every op is ~50 rate-gated (400ms) disk hits,
// and the post-check proves zero fetches happened during measurement.
func BenchmarkScanWarm(b *testing.B) {
	root := b.TempDir()
	cache := b.TempDir()
	ctx := context.Background()
	const n = 50
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("WarmShow%02d.S01E%02d.1080p.WEB-DL.x264-GRP.mkv", i, rng.Intn(6)+1)
		benchWriteVideo(b, root, fmt.Sprintf("Folder%02d", i), name)
	}

	orig := newResolver
	defer func() { newResolver = orig }()
	fake := &fakeSource{name: "fake", tag: "Warm"}
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{fake}})
	}

	// Prefill (cold): fills the temp cache; the live cache dir is untouched.
	if _, err := Scan(ctx, []string{root}, false, cache, nil); err != nil {
		b.Fatalf("prefill Scan: %v", err)
	}
	pre := fake.calls.Load()
	if pre != int32(n) {
		b.Fatalf("prefill fetches = %d, want %d (once per unique key)", pre, n)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		st, err := Scan(ctx, []string{root}, false, cache, nil)
		if err != nil {
			b.Fatalf("warm Scan: %v", err)
		}
		benchStatsSink = st
	}
	b.StopTimer()
	if got := fake.calls.Load() - pre; got != 0 {
		b.Fatalf("warm Scan fetched %d times, want 0 (all disk-cache hits)", got)
	}
}

// httpBenchSource is a loopback-httptest provider: Find performs one real HTTP
// round trip against the test server (no real network), then synthesizes
// media answering q, mirroring the in-process fakeSource contract.
type httpBenchSource struct {
	name   string
	base   string
	client *http.Client
	calls  atomic.Int32
}

func (s *httpBenchSource) Name() string { return s.name }

func (s *httpBenchSource) Find(ctx context.Context, q metadata.Query) (*metadata.Media, error) {
	s.calls.Add(1)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/meta?title="+url.QueryEscape(q.Title), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("bench-http: status %s", resp.Status)
	}
	m := &metadata.Media{
		Source: "bench-http", SourceID: "1",
		Kind: q.Kind, Title: q.Title, Year: q.Year,
		FetchedAt: time.Now().UTC(),
	}
	if q.Kind != metadata.KindMovie {
		m.Episodes = []metadata.Episode{{Season: q.Season, Number: q.Episode, Title: "Bench Episode"}}
	}
	return m, nil
}

// BenchmarkScanColdFakeProvider measures the fetch path end to end without the
// real network: a cold temp cache plus N=20 unique groups resolved through a
// loopback httptest provider injected via newResolver (the existing tests'
// injection pattern). The cache wipe runs outside the timer so each op is cold
// and the timer covers walk + gated fetch only.
func BenchmarkScanColdFakeProvider(b *testing.B) {
	mux := http.NewServeMux()
	mux.HandleFunc("/meta", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	srv := httptest.NewServer(mux)
	b.Cleanup(srv.Close)
	src := &httpBenchSource{name: "bench-http", base: srv.URL, client: srv.Client()}

	root := b.TempDir()
	cache := b.TempDir()
	const n = 20
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("ColdShow%02d.S01E01.1080p.WEB-DL.x264-GRP.mkv", i)
		benchWriteVideo(b, root, fmt.Sprintf("Folder%02d", i), name)
	}

	orig := newResolver
	defer func() { newResolver = orig }()
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{src}})
	}

	ctx := context.Background()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		// Re-cold the temp cache outside the timer (live cache untouched).
		if err := os.RemoveAll(cache); err != nil {
			b.Fatalf("wipe cache: %v", err)
		}
		if err := os.MkdirAll(cache, 0o755); err != nil {
			b.Fatalf("wipe cache: %v", err)
		}
		b.StartTimer()
		st, err := Scan(ctx, []string{root}, false, cache, nil)
		if err != nil {
			b.Fatalf("cold Scan: %v", err)
		}
		benchStatsSink = st
	}
}

// BenchmarkScanIndexSecondPass measures a warm re-scan of a 50-file synthetic
// corpus with a populated folder index (second pass, all subtrees skipped)
// versus without the index (second pass, full walk + disk-cache hits).
// Both use b.TempDir() caches and the in-process fakeSource (no network);
// the NoIndex prefill/measure path is plain Scan, the WithIndex path is
// ScanWithOptions with UseIndex set (first pass populates the index, the
// timed passes are the indexed second pass). Each sub-benchmark asserts
// zero fetches during measurement.
func BenchmarkScanIndexSecondPass(b *testing.B) {
	// seedIdxCorpus writes 50 folders x 1 episode (50 unique shows, so the
	// cold prefill is exactly 50 fetches, one per unique key - the same
	// shape as BenchmarkScanWarm). Unique shows avoid same-season cache
	// sharing between concurrent lookups, which would make the fetch
	// count racy.
	seedIdxCorpus := func(b *testing.B, root string) {
		b.Helper()
		for i := 0; i < 50; i++ {
			name := fmt.Sprintf("IdxShow%02d.S01E01.1080p.WEB-DL.x264-GRP.mkv", i)
			benchWriteVideo(b, root, fmt.Sprintf("Folder%02d", i), name)
		}
	}

	b.Run("NoIndex", func(b *testing.B) {
		root := b.TempDir()
		cache := b.TempDir()
		seedIdxCorpus(b, root)
		ctx := context.Background()

		orig := newResolver
		defer func() { newResolver = orig }()
		fake := &fakeSource{name: "fake", tag: "Idx"}
		newResolver = func(dir string) *metadata.Resolver {
			return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{fake}})
		}

		// Prefill (cold): fills the temp cache; the live cache dir is untouched.
		if _, err := Scan(ctx, []string{root}, false, cache, nil); err != nil {
			b.Fatalf("prefill Scan: %v", err)
		}
		if pre := fake.calls.Load(); pre != 50 {
			b.Fatalf("prefill fetches = %d, want 50 (once per unique key)", pre)
		}
		pre := fake.calls.Load()

		b.ResetTimer()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			st, err := Scan(ctx, []string{root}, false, cache, nil)
			if err != nil {
				b.Fatalf("warm Scan: %v", err)
			}
			benchStatsSink = st
		}
		b.StopTimer()
		if got := fake.calls.Load() - pre; got != 0 {
			b.Fatalf("warm Scan fetched %d times, want 0 (all disk-cache hits)", got)
		}
	})

	b.Run("WithIndex", func(b *testing.B) {
		root := b.TempDir()
		cache := b.TempDir()
		index := filepath.Join(b.TempDir(), "scanindex.json")
		seedIdxCorpus(b, root)
		ctx := context.Background()

		orig := newResolver
		defer func() { newResolver = orig }()
		fake := &fakeSource{name: "fake", tag: "Idx"}
		newResolver = func(dir string) *metadata.Resolver {
			return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{fake}})
		}
		// SkipCatchUp: the index phase is what this sub-benchmark measures;
		// the metadata pass is driven explicitly below so its fetches are
		// counted where they happen.
		opts := func() Options {
			return Options{Dirs: []string{root}, Force: false, CacheDir: cache, IndexPath: index, UseIndex: true, SkipCatchUp: true}
		}

		// Index phase: one walk, zero fetches (the store-backed scan never
		// touches the network).
		if _, err := ScanWithOptions(ctx, opts()); err != nil {
			b.Fatalf("first indexed Scan: %v", err)
		}
		if pre := fake.calls.Load(); pre != 0 {
			b.Fatalf("first indexed scan fetches = %d, want 0 (index phase is offline)", pre)
		}
		// Catch-up: the metadata pass, one fetch per unique key.
		StartCatchUp(CatchUpConfig{Dirs: []string{root}, CacheDir: cache, IndexPath: index}, nil).Wait()
		if pre := fake.calls.Load(); pre != 50 {
			b.Fatalf("catch-up fetches = %d, want 50 (once per unique key)", pre)
		}
		// Probe: the populated second pass must skip everything.
		probe, err := ScanWithOptions(ctx, opts())
		if err != nil {
			b.Fatalf("probe indexed Scan: %v", err)
		}
		if probe.Files != 0 || probe.FoldersScanned != 0 || probe.FoldersSkipped == 0 {
			b.Fatalf("probe indexed Scan = %+v, want Files:0 Scanned:0 Skipped>0", probe)
		}
		pre := fake.calls.Load()

		b.ResetTimer()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			st, err := ScanWithOptions(ctx, opts())
			if err != nil {
				b.Fatalf("indexed re-scan: %v", err)
			}
			benchStatsSink = st
		}
		b.StopTimer()
		if got := fake.calls.Load() - pre; got != 0 {
			b.Fatalf("indexed re-scan fetched %d times, want 0 (all subtrees skipped)", got)
		}
	})
}

// BenchmarkFetchAll1000 measures lookup fan-out over 1000 unique groups
// against an in-process stub (no network). gateInterval 0 disables the 400ms
// rate gate - the gate floor itself is covered by the Scan benchmarks - so
// this isolates FetchAll concurrency/aggregation cost.
func BenchmarkFetchAll1000(b *testing.B) {
	const n = 1000
	groups := make([]*Group, 0, n)
	for i := 0; i < n; i++ {
		q := metadata.Query{
			Kind: metadata.KindSeries, Title: fmt.Sprintf("Bench Show %04d", i),
			Season: 1, Episode: (i % 24) + 1,
		}
		groups = append(groups, &Group{
			Key:   DedupeKey(q),
			Query: q,
			Paths: []string{fmt.Sprintf(`D:\bench\fetch\show%04d\e.mkv`, i)},
		})
	}
	stub := func(ctx context.Context, q metadata.Query) (metadata.Resolution, error) {
		return metadata.Resolution{Found: true, Series: q.Title, Episode: "Bench"}, nil
	}
	ctx := context.Background()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out := FetchAll(ctx, groups, stub, 3, 0, nil)
		if len(out) != n {
			b.Fatalf("results = %d, want %d", len(out), n)
		}
		benchFetchSink = out
	}
}

// realShapeScale is the corpus shape the real-shape benchmarks use: 1/10 of
// the owner's library by default (93 folders, 2,300 files, 230 cache keys),
// because the full shape (930 folders / 23,000 files / 2,300 keys) needs a
// multi-minute seeding pass and never converges under -benchtime. Set
// POTFLIX_BENCH_REALSHAPE=full to measure the full shape deliberately.
func realShapeScale() (folders, shows, seasons, dupFiles int) {
	folders, shows, seasons, dupFiles = 93, 46, 5, 10
	if strings.EqualFold(os.Getenv("POTFLIX_BENCH_REALSHAPE"), "full") {
		folders, shows = 930, 460
	}
	return folders, shows, seasons, dupFiles
}

// seedRealShape writes shows*seasons*dupFiles video files into folders dirs:
// one episode per key, so a key's duplicate files are the same episode in
// several folders. It returns the file count.
func seedRealShape(b *testing.B, root string, folders, shows, seasons, dupFiles int) int {
	b.Helper()
	for f := 0; f < folders; f++ {
		if err := os.MkdirAll(filepath.Join(root, fmt.Sprintf("folder%03d", f)), 0o755); err != nil {
			b.Fatal(err)
		}
	}
	n := 0
	for show := 0; show < shows; show++ {
		for season := 1; season <= seasons; season++ {
			name := fmt.Sprintf("RealShapeShow%03d.S%02dE01.1080p.WEB-DL.x264-GRP.mkv", show, season)
			for d := 0; d < dupFiles; d++ {
				p := filepath.Join(root, fmt.Sprintf("folder%03d", n%folders), name)
				if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
					b.Fatal(err)
				}
				n++
			}
		}
	}
	return n
}

// BenchmarkScanRealShape measures the store-backed scan over an owner-shaped
// corpus: 93 folders, 2,300 files, 230 distinct cache keys (one episode per
// key, ten duplicate files each), all under b.TempDir() with an in-process
// provider and ZERO network.
//
// Both measured shapes assert the fetch count is exactly zero:
//
//   - WarmDiff: every folder's mtime is bumped outside the timer, so no
//     rollup matches and each file is enumerated and (size, mtime)-compared.
//     Unchanged files must reuse their stored record, so the parse counter and
//     Parsed stay at zero - this is the per-file fast path the rework exists
//     for.
//   - WarmSkip: the steady state. The rollups match, whole subtrees are
//     skipped, and the walk never lists their files.
func BenchmarkScanRealShape(b *testing.B) {
	folders, shows, seasons, dupFiles := realShapeScale()
	wantFiles := shows * seasons * dupFiles
	wantKeys := shows * seasons

	root := b.TempDir()
	cache := b.TempDir()
	index := filepath.Join(b.TempDir(), "scanindex.json")
	ctx := context.Background()

	if got := seedRealShape(b, root, folders, shows, seasons, dupFiles); got != wantFiles {
		b.Fatalf("seeded files = %d, want %d", got, wantFiles)
	}

	orig := newResolver
	defer func() { newResolver = orig }()
	origParse := parseQuery
	defer func() { parseQuery = origParse }()
	fake := &fakeSource{name: "fake", tag: "Real"}
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{fake}})
	}
	parses := 0
	parseQuery = func(rawTitle, path string) metadata.Query {
		parses++
		return origParse(rawTitle, path)
	}
	opts := func() Options {
		return Options{Dirs: []string{root}, Force: false, CacheDir: cache, IndexPath: index, UseIndex: true, SkipCatchUp: true}
	}

	// Index phase (untimed): one walk, one parse per file, zero fetches.
	if _, err := ScanWithOptions(ctx, opts()); err != nil {
		b.Fatalf("first indexed Scan: %v", err)
	}
	if pre := fake.calls.Load(); pre != 0 {
		b.Fatalf("index phase fetches = %d, want 0 (the scan never touches the network)", pre)
	}
	if parses != wantFiles {
		b.Fatalf("index phase parses = %d, want %d (one per new file)", parses, wantFiles)
	}
	if state := LoadScanIndex(index).State(); state.Files != wantFiles || state.Keys != wantKeys {
		b.Fatalf("indexed state = %+v, want %d files / %d keys", state, wantFiles, wantKeys)
	}
	pre := fake.calls.Load()

	// bumpFolders moves every folder's mtime so the rollups cannot match: the
	// walk must enumerate and diff every file. Untimed, like the evictions it
	// replaces.
	bump := time.Now().Add(30 * time.Second)
	bumpFolders := func() {
		if err := os.Chtimes(root, bump, bump); err != nil {
			b.Fatal(err)
		}
		for f := 0; f < folders; f++ {
			if err := os.Chtimes(filepath.Join(root, fmt.Sprintf("folder%03d", f)), bump, bump); err != nil {
				b.Fatal(err)
			}
		}
		bump = bump.Add(30 * time.Second)
	}

	b.Run("WarmDiff", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			parses = 0
			bumpFolders()
			b.StartTimer()
			st, err := ScanWithOptions(ctx, opts())
			if err != nil {
				b.Fatalf("warm diff scan: %v", err)
			}
			b.StopTimer()
			benchStatsSink = st
			if st.Files != wantFiles || st.Unchanged != wantFiles || st.Parsed != 0 || parses != 0 {
				b.Fatalf("iter %d stats = %+v (parses %d), want Files:Unchanged:%d Parsed:0", i, st, parses, wantFiles)
			}
			if got := fake.calls.Load() - pre; got != 0 {
				b.Fatalf("iter %d fetches = %d, want 0", i, got)
			}
			b.StartTimer()
		}
	})

	b.Run("WarmSkip", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			st, err := ScanWithOptions(ctx, opts())
			if err != nil {
				b.Fatalf("warm skip scan: %v", err)
			}
			benchStatsSink = st
			if st.Files != 0 || st.FilesSkipped != wantFiles || st.FoldersSkipped == 0 {
				b.Fatalf("iter %d stats = %+v, want Files:0 FilesSkipped:%d", i, st, wantFiles)
			}
			if got := fake.calls.Load() - pre; got != 0 {
				b.Fatalf("iter %d fetches = %d, want 0", i, got)
			}
		}
	})
}
