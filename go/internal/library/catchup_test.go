// Tests for the background metadata catch-up (catchup.go): the launch entry
// point, its idempotence, the needs_repair state machine with its cooldown,
// cancellation, and the inventory report it writes.
package library

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"potflix/internal/metadata"
	"potflix/internal/repair"
)

// catchUpCorpus seeds n files that each parse to a distinct series key.
func catchUpCorpus(t *testing.T, n int) (root string, names []string) {
	t.Helper()
	root = t.TempDir()
	for i := 0; i < n; i++ {
		name := "Show" + string(rune('A'+i)) + ".S01E01.mkv"
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	return root, names
}

// TestCatchUpIdempotentStart proves the launch entry point is idempotent: two
// calls resolve each key exactly once and double-count nothing, and a later
// call after the pass finished finds nothing left to do.
func TestCatchUpIdempotentStart(t *testing.T) {
	root, names := catchUpCorpus(t, 3)
	cache := metaCacheDir(t)
	index := filepath.Join(t.TempDir(), "scanindex.json")

	orig := newResolver
	defer func() { newResolver = orig }()
	fake := &fakeSource{name: "fake", tag: "V1"}
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{fake}})
	}
	cfg := CatchUpConfig{Dirs: []string{root}, CacheDir: cache, IndexPath: index}
	if _, err := ScanWithOptions(context.Background(), Options{Dirs: []string{root}, CacheDir: cache, IndexPath: index, UseIndex: true, SkipCatchUp: true}); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	c1 := StartCatchUp(cfg, nil)
	c2 := StartCatchUp(cfg, nil) // second launch while the first is in flight
	if c1 != c2 {
		t.Errorf("two starts produced two workers (%p, %p), want one", c1, c2)
	}
	c1.Wait()
	c2.Wait()
	if got := int(fake.calls.Load()); got != len(names) {
		t.Errorf("provider calls = %d, want %d (one per key, no double-resolve)", got, len(names))
	}
	first := c1.Stats()
	if first.Passes != 1 || first.Lookups != len(names) {
		t.Errorf("first pass = %+v, want 1 pass over %d keys", first, len(names))
	}

	// A third start after the pass finished: nothing is pending any more.
	c3 := StartCatchUp(cfg, nil)
	c3.Wait()
	settled := c3.Stats()
	if settled.Lookups != first.Lookups || settled.Missed != first.Missed || settled.Failed != first.Failed {
		t.Errorf("settled re-start counted work: %+v, want the same counters as %+v", settled, first)
	}
	if got := int(fake.calls.Load()); got != len(names) {
		t.Errorf("provider calls after a settled re-start = %d, want %d", got, len(names))
	}
	if state := LoadScanIndex(index).State(); state.KeysResolved != state.Keys {
		t.Errorf("after catch-up KeysResolved = %d, want Keys %d (%+v)", state.KeysResolved, state.Keys, state)
	}
}

// TestCatchUpNeedsRepairAfterAttempts proves the needs_repair rule: an entry
// that still has no usable metadata after maxRepairAttempts lookups is parked
// with its attempts and last error, and is not retried before its cooldown.
func TestCatchUpNeedsRepairAfterAttempts(t *testing.T) {
	root, _ := catchUpCorpus(t, 1)
	cache := metaCacheDir(t)
	index := filepath.Join(t.TempDir(), "scanindex.json")
	ctx := context.Background()

	orig := newResolver
	defer func() { newResolver = orig }()
	miss := &missSource{}
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{miss}})
	}
	path := filepath.Join(root, "ShowA.S01E01.mkv")
	if _, err := ScanWithOptions(ctx, Options{Dirs: []string{root}, CacheDir: cache, IndexPath: index, UseIndex: true, SkipCatchUp: true}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	cfg := CatchUpConfig{Dirs: []string{root}, CacheDir: cache, IndexPath: index}

	// Each pass burns one attempt; the resolver serves the fresh negative from
	// disk, so only the first pass touches the provider.
	for i := 1; i <= maxRepairAttempts; i++ {
		StartCatchUp(cfg, nil).Wait()
		rec, ok := LoadScanIndex(index).ix.file(path)
		if !ok {
			t.Fatalf("record for %q vanished", path)
		}
		if rec.Attempts != i {
			t.Fatalf("pass %d: attempts = %d, want %d (state %s)", i, rec.Attempts, i, rec.State)
		}
		if i < maxRepairAttempts && rec.State == StateNeedsRepair {
			t.Fatalf("pass %d: parked as needs_repair too early (attempts=%d)", i, rec.Attempts)
		}
		if i == maxRepairAttempts {
			if rec.State != StateNeedsRepair {
				t.Fatalf("pass %d: state = %q, want %q", i, rec.State, StateNeedsRepair)
			}
			if rec.LastError == "" {
				t.Errorf("needs_repair record carries no last error")
			}
			if rec.NextTry <= time.Now().UnixNano() {
				t.Errorf("next try = %d, want in the future (cooldown %v)", rec.NextTry, repairCooldown)
			}
		}
	}

	// Cooldown: a further pass must not touch it, even with a provider that
	// would answer.
	fake := &fakeSource{name: "fake", tag: "V1"}
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{fake}})
	}
	StartCatchUp(cfg, nil).Wait()
	if n := fake.calls.Load(); n != 0 {
		t.Errorf("provider calls inside the cooldown = %d, want 0", n)
	}
	rec, _ := LoadScanIndex(index).ix.file(path)
	if rec.State != StateNeedsRepair || rec.Attempts != maxRepairAttempts {
		t.Errorf("cooldown pass changed the record: state %q attempts %d", rec.State, rec.Attempts)
	}

	// Past the cooldown it is retried. The retry is forced: the earlier
	// passes persisted a negative entry, and an unforced lookup would
	// correctly serve that cached miss instead of calling the provider.
	ix := LoadScanIndex(index).ix
	rec.NextTry = time.Now().Add(-time.Minute).UnixNano()
	ix.putFile(rec)
	if err := ix.commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	forced := cfg
	forced.Force = true
	StartCatchUp(forced, nil).Wait()
	if n := fake.calls.Load(); n != 1 {
		t.Errorf("provider calls after the cooldown = %d, want 1", n)
	}
	after, _ := LoadScanIndex(index).ix.file(path)
	if after.State != StateResolved || after.Attempts != 0 {
		t.Errorf("record after a successful retry = %+v, want resolved with 0 attempts", after)
	}
}

// TestCatchUpWritesInventoryReport proves each pass writes the needs-repair
// report beside the index, with the entry's path, attempts and error.
func TestCatchUpWritesInventoryReport(t *testing.T) {
	root, _ := catchUpCorpus(t, 1)
	cache := metaCacheDir(t)
	index := filepath.Join(t.TempDir(), "scanindex.json")
	ctx := context.Background()

	orig := newResolver
	defer func() { newResolver = orig }()
	failing := &honestSource{}
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{failing}})
	}
	// "ShowA" is answered by honestSource, so rename the file to the miss arm.
	if err := os.Rename(filepath.Join(root, "ShowA.S01E01.mkv"), filepath.Join(root, "Missing.S01E01.mkv")); err != nil {
		t.Fatal(err)
	}
	if _, err := ScanWithOptions(ctx, Options{Dirs: []string{root}, CacheDir: cache, IndexPath: index, UseIndex: true, SkipCatchUp: true}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	cfg := CatchUpConfig{Dirs: []string{root}, CacheDir: cache, IndexPath: index, AppVersion: "test"}
	for i := 0; i < maxRepairAttempts; i++ {
		StartCatchUp(cfg, nil).Wait()
	}
	r, err := repair.ReadReport(filepath.Join(filepath.Dir(index), "inventory", repair.ReportJSONName))
	if err != nil {
		t.Fatalf("read inventory report: %v", err)
	}
	if r.AppVersion != "test" {
		t.Errorf("report app version = %q, want %q", r.AppVersion, "test")
	}
	if r.Totals.Files != 1 || r.Totals.Indexed != 1 {
		t.Errorf("report totals = %+v, want 1 file indexed", r.Totals)
	}
	if r.Totals.NeedsRepair != len(r.Entries) || len(r.Entries) != 1 {
		t.Fatalf("report entries = %d (totals %+v), want exactly 1", len(r.Entries), r.Totals)
	}
	e := r.Entries[0]
	if e.Path != filepath.Join(root, "Missing.S01E01.mkv") {
		t.Errorf("entry path = %q, want the local path", e.Path)
	}
	if e.Attempts != maxRepairAttempts {
		t.Errorf("entry attempts = %d, want %d", e.Attempts, maxRepairAttempts)
	}
	if e.LastError == "" {
		t.Errorf("entry last error is empty")
	}
	if e.Reason != repair.ReasonNoMetadata {
		t.Errorf("entry reason = %q, want %q", e.Reason, repair.ReasonNoMetadata)
	}
	// On demand form, same data.
	if _, _, err := WriteInventory(index, "test"); err != nil {
		t.Fatalf("WriteInventory: %v", err)
	}
}

// TestCatchUpCancelStopsPass proves the worker stops cleanly and keeps what it
// applied, with no invented failures.
func TestCatchUpCancelStopsPass(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"ShowA.S01E01.mkv", "ShowB.S01E01.mkv", "ShowC.S01E01.mkv"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cache := metaCacheDir(t)
	index := filepath.Join(t.TempDir(), "scanindex.json")

	orig := newResolver
	defer func() { newResolver = orig }()
	split := &splitSpeedSource{name: "fake", tag: "V1", release: make(chan struct{})}
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{split}})
	}
	if _, err := ScanWithOptions(context.Background(), Options{Dirs: []string{root}, CacheDir: cache, IndexPath: index, UseIndex: true, SkipCatchUp: true}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	c := StartCatchUp(CatchUpConfig{Dirs: []string{root}, CacheDir: cache, IndexPath: index}, nil)
	deadline := time.Now().Add(5 * time.Second)
	for LoadScanIndex(index).State().KeysResolved == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		StopCatchUp(index)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(split.release)
		t.Fatal("StopCatchUp did not return within 5s")
	}
	close(split.release)
	c.Wait()

	stats := c.Stats()
	if stats.Failed != 0 || stats.Missed != 0 {
		t.Errorf("cancelled pass counts = failed %d missed %d, want 0/0", stats.Failed, stats.Missed)
	}
	state := LoadScanIndex(index).State()
	if state.KeysResolved == 0 {
		t.Errorf("cancelled pass applied nothing (%+v)", state)
	}
	if state.FilesNeedsRepair != 0 {
		t.Errorf("cancelled pass parked %d records as needs_repair, want 0", state.FilesNeedsRepair)
	}
	// Cancelled keys stay pending: no verdict was written for them.
	for _, rec := range LoadScanIndex(index).ix.filesSnapshot() {
		if rec.MetaKey == "" {
			continue
		}
		if rec.State == StateNeedsRepair {
			t.Errorf("record %q parked after a cancel", rec.Path)
		}
		if rec.Attempts > 0 && rec.State == StatePending {
			t.Errorf("record %q counted an attempt for a cancelled lookup", rec.Path)
		}
	}
}

// TestCatchUpCancelsWithScanContext proves the scan's context stops the
// catch-up it started: a UI Cancel ends the metadata work too.
func TestCatchUpCancelsWithScanContext(t *testing.T) {
	root, _ := catchUpCorpus(t, 2)
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
	c := StartCatchUp(CatchUpConfig{Dirs: []string{root}, CacheDir: cache, IndexPath: index, Ctx: ctx}, nil)
	select {
	case <-slow.started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("slow provider never started")
	}
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Wait()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the scan's cancel did not stop the catch-up")
	}
	if c.Stats().Failed != 0 {
		t.Errorf("cancelled pass recorded %d failures, want 0", c.Stats().Failed)
	}
}

// TestApplyVerdictTransientFailureIsNotAResolution pins the reason text a
// provider error leaves behind (the inventory report's last column).
func TestApplyVerdictTransientFailureIsNotAResolution(t *testing.T) {
	rec := FileRecord{Path: `D:\lib\Show.S01E01.mkv`, MetaKey: "series|show|0|1", State: StatePending}
	key := normalizeScanDir(rec.Path)
	ix := &Index{
		files:        map[string]*FileRecord{key: &rec},
		folders:      map[string]FolderRecord{},
		dirtyFiles:   map[string]bool{},
		dirtyFolders: map[string]bool{},
		goneFiles:    map[string]bool{},
	}
	byPath := map[string]string{key: rec.MetaKey}
	applied, _, _ := ix.applyVerdict(rec.MetaKey, byPath, metadata.Resolution{}, errors.New("boom"), time.Now())
	if applied != 1 {
		t.Fatalf("applied = %d, want 1", applied)
	}
	got := ix.files[key]
	if got.LastError != "boom" {
		t.Errorf("last error = %q, want the provider error", got.LastError)
	}
	if got.Attempts != 1 || got.State != StatePending {
		t.Errorf("record = %+v, want pending with 1 attempt", got)
	}
}

// gateSource answers only lookups whose context is still live, and only after
// the test closes proceed. That makes the two behaviours deterministic in one
// run: a lookup that inherited a cancelled context is refused exactly as a real
// provider's in-flight HTTP call would be (the post-select ctx check decides,
// never the select's random branch), while a lookup running on the pass's own
// live context is answered.
type gateSource struct {
	name    string
	tag     string
	calls   atomic.Int32
	started chan struct{}
	proceed chan struct{}
	once    sync.Once
}

func (s *gateSource) Name() string { return s.name }

func (s *gateSource) Find(ctx context.Context, q metadata.Query) (*metadata.Media, error) {
	s.calls.Add(1)
	s.once.Do(func() { close(s.started) })
	select {
	case <-s.proceed:
	case <-ctx.Done():
	}
	if err := ctx.Err(); err != nil {
		return nil, err
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

// TestScanHandoffResolvesAfterTheScanContextEnds models the live app sequence:
// the index phase (window.go startLibraryScan -> ScanWithOptions with no
// SkipCatchUp) hands its pending keys to the catch-up, and the window cancels
// the scan's context the moment the run lands (onScanDone). The hand-off must
// outlive that context: the pass it starts has to reach the provider and settle
// the keys the scan just recorded, and the metadata cache store must appear.
//
// Measured live on 2026-10-13 00:37:41: the scan indexed 7,806 files / 2,302
// unique keys / 7,303 pending and handed them over, the pass was cancelled
// microseconds later, and no lookup, no cache store and no report followed.
func TestScanHandoffResolvesAfterTheScanContextEnds(t *testing.T) {
	root, names := catchUpCorpus(t, 3)
	cache := metaCacheDir(t)
	index := filepath.Join(t.TempDir(), "scanindex.json")

	orig := newResolver
	defer func() { newResolver = orig }()
	// The provider holds every lookup until the test lets it answer, so the
	// cancel below is guaranteed to land while the handed-over pass is in flight.
	gate := &gateSource{name: "fake", tag: "V1", started: make(chan struct{}), proceed: make(chan struct{})}
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{gate}})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := ScanWithOptions(ctx, Options{Dirs: []string{root}, CacheDir: cache, IndexPath: index, UseIndex: true, AppVersion: "test"})
	if err != nil {
		t.Fatalf("indexed Scan: %v", err)
	}
	if st.Keys != len(names) || st.KeysDone != 0 {
		t.Fatalf("index phase reported keys=%d resolved=%d, want %d pending keys handed to the catch-up", st.Keys, st.KeysDone, len(names))
	}

	select {
	case <-gate.started:
	case <-time.After(10 * time.Second):
		close(gate.proceed)
		StopCatchUp(index)
		t.Fatal("the handed-over pass never reached the provider")
	}
	// window.go onScanDone: the scan's context dies with the run, every time.
	cancel()
	close(gate.proceed)

	deadline := time.Now().Add(10 * time.Second)
	for LoadScanIndex(index).State().KeysResolved < len(names) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	state := LoadScanIndex(index).State()
	StopCatchUp(index)
	if state.KeysResolved != len(names) {
		t.Errorf("after the scan context ended KeysResolved = %d, want %d (the handed-over keys were never settled)", state.KeysResolved, len(names))
	}
	if got := int(gate.calls.Load()); got != len(names) {
		t.Errorf("provider calls = %d, want %d (one per handed-over key)", got, len(names))
	}
	if fi, err := os.Stat(cache + ".idx"); err != nil {
		t.Errorf("metadata cache store %q was never created: %v", cache+".idx", err)
	} else if fi.Size() == 0 {
		t.Errorf("metadata cache store %q is empty", cache+".idx")
	}
}

// TestCatchUpInventoryReportNeedsAnEnumeratedIndex proves the needs-repair
// report is never written from an index that holds no per-file records - the
// folder-rollup-only state the legacy migration leaves, before the first
// index-phase scan - because such a report claims "0 files, 0 indexed" for a
// library whose files were merely not enumerated yet. Measured live
// 2026-10-13 00:37:34: exactly that report was written at launch (0 files, 0
// indexed) while 7,806 files were on disk, and it was never refreshed.
// The positive control pins the other half: with file records in the index the
// pass writes the report, with real totals.
func TestCatchUpInventoryReportNeedsAnEnumeratedIndex(t *testing.T) {
	index := filepath.Join(t.TempDir(), "scanindex.json")
	cache := metaCacheDir(t)
	reportPath := filepath.Join(filepath.Dir(index), "inventory", repair.ReportJSONName)

	// Nothing has enumerated the library: no per-file records at all.
	StartCatchUp(CatchUpConfig{IndexPath: index, CacheDir: cache, AppVersion: "test"}, nil).Wait()
	if _, err := os.Stat(reportPath); err == nil {
		t.Errorf("a report was written from an index with no file records: %s", reportPath)
	}

	// Positive control: the index-phase scan enumerates files, so the pass has
	// real totals to report.
	root, names := catchUpCorpus(t, 3)
	orig := newResolver
	defer func() { newResolver = orig }()
	fake := &fakeSource{name: "fake", tag: "V1"}
	newResolver = func(dir string) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Sources: []metadata.Source{fake}})
	}
	if _, err := ScanWithOptions(context.Background(), Options{Dirs: []string{root}, CacheDir: cache, IndexPath: index, UseIndex: true, SkipCatchUp: true}); err != nil {
		t.Fatalf("indexed Scan: %v", err)
	}
	StartCatchUp(CatchUpConfig{Dirs: []string{root}, CacheDir: cache, IndexPath: index, AppVersion: "test"}, nil).Wait()
	r, err := repair.ReadReport(reportPath)
	if err != nil {
		t.Fatalf("no report after an enumerated index: %v", err)
	}
	if r.Totals.Files != len(names) || r.Totals.Indexed != len(names) {
		t.Errorf("report totals = %+v, want %d files indexed", r.Totals, len(names))
	}
}
