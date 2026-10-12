package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"potflix/internal/store"
)

func TestCacheKeyShapes(t *testing.T) {
	if k := cacheKey(Query{Kind: KindSeries, Title: "Breaking Bad", Year: 2008, Season: 1}); k != "series|breaking bad|2008|1" {
		t.Fatalf("series key = %q", k)
	}
	if k := cacheKey(Query{Kind: KindMovie, Title: "Inception", Year: 2010}); k != "movie|inception|2010" {
		t.Fatalf("movie key = %q", k)
	}
	if k := cacheKey(Query{Kind: KindSeries, Title: "  Breaking   BAD ", Year: 2008, Season: 1}); k != "series|breaking bad|2008|1" {
		t.Fatalf("collapsed key = %q", k)
	}
}

func TestCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	q := Query{Kind: KindSeries, Title: "Breaking Bad", Year: 2008, Season: 1}
	key := cacheKey(q)
	media := &Media{
		Source: "tvmaze", SourceID: "169",
		Kind: KindSeries, Title: "Breaking Bad", Year: 2008,
		Episodes:  []Episode{{Season: 1, Number: 1, Title: "Pilot"}},
		FetchedAt: time.Now().UTC().Truncate(time.Millisecond),
	}
	if err := cacheStore(dir, key, media); err != nil {
		t.Fatalf("cacheStore: %v", err)
	}
	got, ok := cacheLoad(dir, key)
	if !ok {
		t.Fatal("cacheLoad missed after store")
	}
	if !reflect.DeepEqual(got, media) {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, media)
	}
}

// TestCacheCorruptIgnored proves a corrupt legacy file imports nothing (it is
// not a record) and that a key with no record is a miss.
func TestCacheCorruptIgnored(t *testing.T) {
	dir := t.TempDir()
	q := Query{Kind: KindSeries, Title: "Show", Season: 1}
	key := cacheKey(q)
	if err := os.WriteFile(filepath.Join(dir, cacheFileName(key)), []byte("not json{{{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := cacheLoad(dir, key); ok {
		t.Fatal("corrupt file should be a miss")
	}
	if _, ok := cacheLoad(dir, "series|missing|0|0"); ok {
		t.Fatal("missing file should be a miss")
	}
}

// TestCacheSchema2IsMiss proves schema-2 entries (pre-enrichment) are a miss
// under schema 3 while fresh stores hit.
func TestCacheSchema2IsMiss(t *testing.T) {
	dir := t.TempDir()
	q := Query{Kind: KindSeries, Title: "Show", Season: 1}
	key := cacheKey(q)
	legacy2 := `{"key":` + strconv.Quote(key) + `,"schema":2,"media":{"source":"tvmaze","sourceId":"1","kind":1,"title":"Show","fetchedAt":"2026-01-01T00:00:00Z"}}`
	if err := os.WriteFile(filepath.Join(dir, cacheFileName(key)), []byte(legacy2), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := cacheLoad(dir, key); ok {
		t.Fatal("Schema:2 entry should be a miss under schema 3")
	}
	media := &Media{
		Source: "tvmaze", SourceID: "1",
		Kind: KindSeries, Title: "Show",
		Episodes:  []Episode{{Season: 1, Number: 1, Title: "Pilot"}},
		FetchedAt: time.Now().UTC().Truncate(time.Millisecond),
	}
	if err := cacheStore(dir, key, media); err != nil {
		t.Fatalf("cacheStore: %v", err)
	}
	if _, ok := cacheLoad(dir, key); !ok {
		t.Fatal("current-schema entry should hit")
	}
}

// storeRawRecord writes one encoded cache record straight into dir's store.
// The cache is a single store file keyed by the raw cache key, so a test that
// needs a hand-built record (a backdated negative, a record for a key nothing
// else touches) stores it instead of writing a per-key .json file.
func storeRawRecord(t *testing.T, dir, key string, data []byte) {
	t.Helper()
	if err := withCacheWrite(dir, func(db *store.DB) error { return db.Put(key, data) }); err != nil {
		t.Fatalf("store record %q: %v", key, err)
	}
}

// TestCacheHasIsPresenceOnly proves CacheHas is a presence check that never
// parses a stored body: a stored record answers true whatever its schema, an
// empty key and a key with no record answer false, and a corrupt legacy file
// (which imports nothing, so no record exists) answers false too. (This
// replaces TestCacheHasStatOnly, whose premise was a stat on the key's own
// file: the live cache is one store file, so presence is a stored record.)
func TestCacheHasIsPresenceOnly(t *testing.T) {
	dir := t.TempDir()
	key := cacheKey(Query{Kind: KindSeries, Title: "Stat Only", Year: 2001, Season: 1})
	if CacheHas(dir, key) {
		t.Fatal("CacheHas on a missing entry = true, want false")
	}
	if CacheHas(dir, "") {
		t.Fatal("CacheHas on an empty key = true, want false")
	}
	// A corrupt legacy file is not a record: nothing imports, so nothing is
	// present, and the probe still never parses anything on the read path.
	if err := os.WriteFile(filepath.Join(dir, cacheFileName(key)), []byte("not json{{{corrupt"), 0o644); err != nil {
		t.Fatalf("write corrupt fixture: %v", err)
	}
	if CacheHas(dir, key) {
		t.Fatal("CacheHas on a corrupt legacy file = true, want false (it never was a record)")
	}
	// A stored entry reports present.
	key2 := cacheKey(Query{Kind: KindMovie, Title: "Stat Only Film", Year: 2002})
	media := &Media{
		Source: "tvmaze", SourceID: "7",
		Kind: KindMovie, Title: "Stat Only Film", Year: 2002,
		FetchedAt: time.Now().UTC().Truncate(time.Millisecond),
	}
	if err := cacheStore(dir, key2, media); err != nil {
		t.Fatalf("cacheStore: %v", err)
	}
	if !CacheHas(dir, key2) {
		t.Fatal("CacheHas on a stored entry = false, want true")
	}
}

// TestCacheNegativeRoundTrip proves a persisted miss loads as a negative
// hit (not a positive), stays invisible to the positive-only cacheLoad,
// and still satisfies the presence probe.
func TestCacheNegativeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	key := cacheKey(Query{Kind: KindSeries, Title: "No Such Show", Year: 2031, Season: 1})
	if err := cacheStoreNegative(dir, key); err != nil {
		t.Fatalf("cacheStoreNegative: %v", err)
	}
	e, ok := cacheLoadEntry(dir, key)
	if !ok || !e.NotFound || e.Media != nil {
		t.Fatalf("cacheLoadEntry = %+v,%v; want negative hit", e, ok)
	}
	if _, ok := cacheLoad(dir, key); ok {
		t.Fatal("cacheLoad hit on a negative entry, want miss (positive-only)")
	}
	if !CacheHas(dir, key) {
		t.Fatal("CacheHas on a negative entry = false, want true")
	}
}

// TestCacheNegativeExpiredIsAbsent proves a negative entry older than the
// TTL is treated as absent so the caller re-attempts.
func TestCacheNegativeExpiredIsAbsent(t *testing.T) {
	dir := t.TempDir()
	key := cacheKey(Query{Kind: KindSeries, Title: "Stale Miss", Season: 1})
	old := time.Now().UTC().Add(-negativeTTL - time.Hour).Truncate(time.Millisecond)
	data, err := json.Marshal(cacheFile{Key: key, Schema: cacheSchema, NotFound: true, CheckedAt: old})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, cacheFileName(key)), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := cacheLoadEntry(dir, key); ok {
		t.Fatal("expired negative should be absent, got hit")
	}
}

// TestCacheLivePositive proves a valid positive entry (any age) reports live.
func TestCacheLivePositive(t *testing.T) {
	dir := t.TempDir()
	q := Query{Kind: KindSeries, Title: "Live Show", Year: 2008, Season: 1}
	key := cacheKey(q)
	media := &Media{
		Source: "tvmaze", SourceID: "169",
		Kind: KindSeries, Title: "Live Show", Year: 2008,
		Episodes:  []Episode{{Season: 1, Number: 1, Title: "Pilot"}},
		FetchedAt: time.Now().UTC().Truncate(time.Millisecond),
	}
	if err := cacheStore(dir, key, media); err != nil {
		t.Fatalf("cacheStore: %v", err)
	}
	if !CacheLive(dir, key) {
		t.Fatal("CacheLive on a positive entry = false, want true")
	}
}

// TestCacheLiveFreshNegative proves a fresh negative entry reports live.
func TestCacheLiveFreshNegative(t *testing.T) {
	dir := t.TempDir()
	key := cacheKey(Query{Kind: KindSeries, Title: "Fresh Miss", Season: 1})
	if err := cacheStoreNegative(dir, key); err != nil {
		t.Fatalf("cacheStoreNegative: %v", err)
	}
	if !CacheLive(dir, key) {
		t.Fatal("CacheLive on a fresh negative = false, want true")
	}
}

// TestCacheLiveExpiredNegative proves an expired negative reports not-live.
func TestCacheLiveExpiredNegative(t *testing.T) {
	dir := t.TempDir()
	key := cacheKey(Query{Kind: KindSeries, Title: "Stale Miss Live", Season: 1})
	old := time.Now().UTC().Add(-negativeTTL - time.Hour).Truncate(time.Millisecond)
	data, err := json.Marshal(cacheFile{Key: key, Schema: cacheSchema, NotFound: true, CheckedAt: old})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, cacheFileName(key)), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if CacheLive(dir, key) {
		t.Fatal("CacheLive on an expired negative = true, want false")
	}
}

// TestCacheLiveCorrupt proves corrupt / truncated / wrong-schema / oversize /
// missing records report not-live (unlike the presence-only CacheHas). Every
// fixture here is a LEGACY file, so it also proves the schema and decode gates
// still reject exactly what they rejected before the store.
func TestCacheLiveCorrupt(t *testing.T) {
	dir := t.TempDir()
	key := cacheKey(Query{Kind: KindSeries, Title: "Corrupt Live", Season: 1})
	if CacheLive(dir, key) {
		t.Fatal("CacheLive on a missing entry = true, want false")
	}
	if CacheLive(dir, "") {
		t.Fatal("CacheLive on an empty key = true, want false")
	}
	if err := os.WriteFile(filepath.Join(dir, cacheFileName(key)), []byte("not json{{{corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if CacheLive(dir, key) {
		t.Fatal("CacheLive on a corrupt file = true, want false")
	}
	// Truncated JSON.
	if err := os.WriteFile(filepath.Join(dir, cacheFileName(key)), []byte(`{"key":`), 0o644); err != nil {
		t.Fatal(err)
	}
	if CacheLive(dir, key) {
		t.Fatal("CacheLive on a truncated file = true, want false")
	}
	// Wrong schema.
	legacy := `{"key":"` + key + `","schema":2,"media":{"source":"tvmaze","sourceId":"1","kind":1,"title":"Show","fetchedAt":"2026-01-01T00:00:00Z"}}`
	if err := os.WriteFile(filepath.Join(dir, cacheFileName(key)), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	if CacheLive(dir, key) {
		t.Fatal("CacheLive on a wrong-schema file = true, want false")
	}
	// Oversize garbage is rejected by the decode, not by a size cap: size is
	// not a validity gate any more (see TestCacheLargeEntryRoundTrip).
	big := make([]byte, 512*1024)
	for i := range big {
		big[i] = 'x'
	}
	if err := os.WriteFile(filepath.Join(dir, cacheFileName(key)), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if CacheLive(dir, key) {
		t.Fatal("CacheLive on oversize garbage = true, want false")
	}
}

// TestCacheLiveFutureCheckedAt proves a far-future CheckedAt (clock jump)
// reports not-live, while a near-future one within the skew window stays live.
func TestCacheLiveFutureCheckedAt(t *testing.T) {
	dir := t.TempDir()
	key := cacheKey(Query{Kind: KindSeries, Title: "Future Miss", Season: 1})
	far := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Millisecond)
	data, err := json.Marshal(cacheFile{Key: key, Schema: cacheSchema, NotFound: true, CheckedAt: far})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, cacheFileName(key)), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if CacheLive(dir, key) {
		t.Fatal("CacheLive on a far-future CheckedAt = true, want false (clock jump = expired)")
	}
	if e, ok := cacheLoadEntry(dir, key); ok {
		t.Fatalf("cacheLoadEntry on far-future negative = %+v,true; want absent", e)
	}
	near := time.Now().UTC().Add(2 * time.Minute).Truncate(time.Millisecond)
	// A second directory: a rewrite of an already-imported legacy file is not
	// a flow migration covers, so the fresh record is written where it is read.
	dir2 := t.TempDir()
	data, err = json.Marshal(cacheFile{Key: key, Schema: cacheSchema, NotFound: true, CheckedAt: near})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir2, cacheFileName(key)), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if !CacheLive(dir2, key) {
		t.Fatal("CacheLive on a near-future CheckedAt (within skew) = false, want true")
	}
}

// TestCacheLegacyPositiveNoNewFields proves a pre-existing positive file
// with no notFound/checkedAt fields still loads as a positive hit.
func TestCacheLegacyPositiveNoNewFields(t *testing.T) {
	dir := t.TempDir()
	q := Query{Kind: KindSeries, Title: "Legacy Show", Year: 2005, Season: 2}
	key := cacheKey(q)
	legacy := `{"key":` + strconv.Quote(key) + `,"schema":3,"media":{"source":"tvmaze","sourceId":"9","kind":1,"title":"Legacy Show","episodes":[{"season":2,"number":1,"title":"Pilot"}],"fetchedAt":"2026-01-01T00:00:00Z"}}`
	if err := os.WriteFile(filepath.Join(dir, cacheFileName(key)), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	m, ok := cacheLoad(dir, key)
	if !ok || m == nil || m.Source != "tvmaze" {
		t.Fatalf("legacy positive cacheLoad = %+v,%v; want hit", m, ok)
	}
	e, ok := cacheLoadEntry(dir, key)
	if !ok || e.NotFound || e.Media == nil {
		t.Fatalf("legacy positive cacheLoadEntry = %+v,%v; want positive hit", e, ok)
	}
}

// TestCacheLargeEntryRoundTrip proves the 64 KiB read cap is gone: an entry of
// 256 KiB is stored, reports live, reads back, and round-trips exactly.
func TestCacheLargeEntryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	q := Query{Kind: KindSeries, Title: "Huge Show", Year: 2008, Season: 1}
	key := cacheKey(q)
	media := &Media{
		Source: "tvmaze", SourceID: "41",
		Kind: KindSeries, Title: "Huge Show", Year: 2008,
		Episodes:  []Episode{{Season: 1, Number: 1, Title: "Pilot", Synopsis: strings.Repeat("s", 256*1024)}},
		FetchedAt: time.Now().UTC().Truncate(time.Millisecond),
	}
	encoded, err := json.Marshal(cacheFile{Key: key, Schema: cacheSchema, Media: *media})
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) <= 64*1024 {
		t.Fatalf("fixture is %d bytes, want more than the old 64 KiB read cap", len(encoded))
	}
	if err := cacheStore(dir, key, media); err != nil {
		t.Fatalf("cacheStore of a 256 KiB entry: %v", err)
	}
	if !CacheLive(dir, key) {
		t.Fatalf("CacheLive on a %d-byte entry = false, want true (no size cap)", len(encoded))
	}
	got, ok := cacheLoad(dir, key)
	if !ok {
		t.Fatalf("cacheLoad missed a %d-byte entry, want a hit", len(encoded))
	}
	if !reflect.DeepEqual(got, media) {
		t.Fatalf("large round-trip mismatch: got %d synopsis bytes, want %d",
			len(got.Synopsis), len(media.Synopsis))
	}
}

// TestProbeMatchesCacheLiveLarge proves ProbeCache and CacheLive agree case by
// case without a size gate: a valid 256 KiB entry is live for both (the old
// cap made every entry over 64 KiB permanently non-live), and a corrupt record
// is live for neither.
func TestProbeMatchesCacheLiveLarge(t *testing.T) {
	dir := t.TempDir()
	q := Query{Kind: KindSeries, Title: "Cap Show", Season: 1, Episode: 1}
	key := cacheKey(q)
	media := &Media{
		Source: "tvmaze", SourceID: "1",
		Kind: KindSeries, Title: "Cap Show",
		Episodes:  []Episode{{Season: 1, Number: 1, Title: "Pilot", Synopsis: strings.Repeat("s", 256*1024)}},
		FetchedAt: time.Now().UTC().Truncate(time.Millisecond),
	}
	if err := cacheStore(dir, key, media); err != nil {
		t.Fatalf("cacheStore: %v", err)
	}
	if !CacheLive(dir, key) {
		t.Fatal("CacheLive on a valid 256 KiB entry = false, want true")
	}
	if r := New(Options{Dir: dir}); !r.ProbeCache(key) {
		t.Fatal("ProbeCache on a valid 256 KiB entry = false, want true (must match CacheLive)")
	}
	// A corrupt record is a miss for both, whatever its size.
	dir2 := t.TempDir()
	key2 := cacheKey(Query{Kind: KindSeries, Title: "Broken Show", Season: 1, Episode: 1})
	if err := os.WriteFile(filepath.Join(dir2, cacheFileName(key2)), []byte("not json{{{corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if CacheLive(dir2, key2) {
		t.Fatal("CacheLive on a corrupt entry = true, want false")
	}
	if r := New(Options{Dir: dir2}); r.ProbeCache(key2) {
		t.Fatal("ProbeCache on a corrupt entry = true, want false (must match CacheLive)")
	}
}

// TestNegativeTTLGovernsRefetch proves the persisted semantics are unchanged:
// a not-found entry inside its 30-day TTL serves the miss with zero provider
// calls from a cold resolver, and one backdated past the TTL is re-attempted.
func TestNegativeTTLGovernsRefetch(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	q := Query{Kind: KindSeries, Title: "Never Aired Nowhere", Season: 1, Episode: 1}
	key := cacheKey(q)

	first := &fakeSource{name: "fake"} // nil media = not found
	r1 := New(Options{Dir: dir, Sources: []Source{first}, Timeout: 5 * time.Second})
	if res, err := r1.LookupNow(ctx, q); err != nil || res.Found {
		t.Fatalf("run 1 = %+v,%v; want a miss", res, err)
	}
	if n := first.calls.Load(); n != 1 {
		t.Fatalf("run 1 provider calls = %d, want 1", n)
	}
	if e, ok := cacheLoadEntry(dir, key); !ok || !e.NotFound {
		t.Fatalf("run 1 entry = %+v,%v; want a persisted negative", e, ok)
	}

	// Inside the TTL: a cold resolver (new memory) serves the miss with no
	// provider call.
	second := &fakeSource{name: "fake"}
	r2 := New(Options{Dir: dir, Sources: []Source{second}, Timeout: 5 * time.Second})
	if res, err := r2.LookupNow(ctx, q); err != nil || res.Found {
		t.Fatalf("run 2 = %+v,%v; want the cached miss", res, err)
	}
	if n := second.calls.Load(); n != 0 {
		t.Fatalf("run 2 provider calls = %d, want 0 (negative inside its TTL)", n)
	}

	// Backdate the stored record past the TTL, as a month-old entry reads.
	backdated, err := json.Marshal(cacheFile{Key: key, Schema: cacheSchema, NotFound: true,
		CheckedAt: time.Now().UTC().Add(-negativeTTL - time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := withCache(dir, func(db *store.DB) error { return db.Put(key, backdated) }); err != nil {
		t.Fatalf("backdate stored negative: %v", err)
	}
	if CacheLive(dir, key) {
		t.Fatal("CacheLive on a backdated negative = true, want false")
	}

	// Past the TTL: a third cold resolver re-attempts the provider.
	third := &fakeSource{name: "fake"}
	r3 := New(Options{Dir: dir, Sources: []Source{third}, Timeout: 5 * time.Second})
	if res, err := r3.LookupNow(ctx, q); err != nil || res.Found {
		t.Fatalf("run 3 = %+v,%v; want a miss", res, err)
	}
	if n := third.calls.Load(); n != 1 {
		t.Fatalf("run 3 provider calls = %d, want 1 (expired negative must be re-attempted)", n)
	}
}

// TestClearCacheEmptiesStoreAndLegacyDir proves ClearCache drops every stored
// entry, removes the legacy directory, stays idempotent, and leaves the store
// usable for the next write.
func TestClearCacheEmptiesStoreAndLegacyDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "meta")
	// The store file is <dir>.idx, inside the temp root: release the held
	// handle before t.TempDir removes it. Production never closes a handle;
	// only tests do.
	t.Cleanup(closeCacheHandles)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	posKey := cacheKey(Query{Kind: KindSeries, Title: "Kept Show", Season: 1})
	negKey := cacheKey(Query{Kind: KindSeries, Title: "Dead Show", Season: 1})
	media := &Media{Source: "tvmaze", SourceID: "5", Kind: KindSeries, Title: "Kept Show",
		FetchedAt: time.Now().UTC().Truncate(time.Millisecond)}
	if err := cacheStore(dir, posKey, media); err != nil {
		t.Fatalf("cacheStore: %v", err)
	}
	if err := cacheStoreNegative(dir, negKey); err != nil {
		t.Fatalf("cacheStoreNegative: %v", err)
	}
	if !CacheHas(dir, posKey) || CountNegatives(dir) != 1 {
		t.Fatalf("seed state = has %t negatives %d; want true, 1", CacheHas(dir, posKey), CountNegatives(dir))
	}

	if err := ClearCache(dir); err != nil {
		t.Fatalf("ClearCache: %v", err)
	}
	if CacheHas(dir, posKey) || CacheLive(dir, negKey) {
		t.Fatal("cleared cache still reports entries, want none")
	}
	if got := CountNegatives(dir); got != 0 {
		t.Fatalf("CountNegatives after ClearCache = %d, want 0", got)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("legacy dir stat = %v, want removed", err)
	}
	// Idempotent: a second call on an already-empty cache is not an error.
	if err := ClearCache(dir); err != nil {
		t.Fatalf("second ClearCache: %v", err)
	}
	// The store file itself survives, so the next lookup caches again.
	if err := cacheStore(dir, posKey, media); err != nil {
		t.Fatalf("cacheStore after ClearCache: %v", err)
	}
	if !CacheHas(dir, posKey) {
		t.Fatal("store unusable after ClearCache, want the next write to land")
	}
	if err := ClearCache(dir); err != nil {
		t.Fatalf("final ClearCache: %v", err)
	}
}

// TestCacheMigrationParityOnRealDir copies the machine's legacy cache
// directory into a temp dir (the live directory is only read, never written)
// and proves the first open imports exactly one record per legacy .json file
// and that every key resolves to the same answer it had as a file.
func TestCacheMigrationParityOnRealDir(t *testing.T) {
	live := defaultCacheDir()
	files, err := filepath.Glob(filepath.Join(live, "*.json"))
	if err != nil {
		t.Fatalf("glob %s: %v", live, err)
	}
	if len(files) == 0 {
		t.Skipf("no legacy cache at %s to migrate", live)
	}
	before := dirSnapshot(t, live)

	dir := filepath.Join(t.TempDir(), "meta")
	// The store file <dir>.idx lives inside the temp root (see
	// TestClearCacheEmptiesStoreAndLegacyDir).
	t.Cleanup(closeCacheHandles)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// What each file answers on its own, under the gates the file reader
	// applied; migration must reproduce it for the same key.
	type wantEntry struct {
		key string
		e   cacheEntry
		ok  bool
	}
	legacy := make([]wantEntry, 0, len(files))
	seen := make(map[string]bool, len(files))
	for _, src := range files {
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read legacy %s: %v", src, err)
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(src)), data, 0o644); err != nil {
			t.Fatalf("copy legacy %s: %v", src, err)
		}
		var cf cacheFile
		if err := json.Unmarshal(data, &cf); err != nil || cf.Key == "" {
			t.Fatalf("legacy file %s is not a cache record (key %q)", filepath.Base(src), cf.Key)
		}
		if seen[cf.Key] {
			t.Fatalf("legacy dir has a duplicate key %q; the parity check needs unique keys", cf.Key)
		}
		seen[cf.Key] = true
		e, ok := decodeCacheEntry(data, cf.Key)
		legacy = append(legacy, wantEntry{key: cf.Key, e: e, ok: ok})
	}

	// The first read opens the store and migrates the directory into it.
	var records, imported int
	err = withCache(dir, func(db *store.DB) error {
		records = db.Len()
		// The migration counter itself reports one import per file (a re-run
		// over the imported records overwrites them with identical bytes).
		imported = migrateLegacyCache(db, legacyCacheFiles(dir))
		return nil
	})
	if err != nil {
		t.Fatalf("withCache: %v", err)
	}
	if records != len(files) {
		t.Fatalf("imported %d records, want %d (one per legacy .json file)", records, len(files))
	}
	t.Logf("migrated %d records from %d legacy .json files under %s", records, len(files), live)
	if imported != len(files) {
		t.Fatalf("migrateLegacyCache reported %d imports, want %d", imported, len(files))
	}
	// Every key resolves exactly as its legacy file did.
	for _, w := range legacy {
		got, ok := cacheLoadEntry(dir, w.key)
		if ok != w.ok || (ok && !reflect.DeepEqual(got, w.e)) {
			t.Fatalf("key %q = %+v,%v after migration, want %+v,%v (the legacy answer)",
				w.key, got, ok, w.e, w.ok)
		}
	}
	// Migration is never destructive: the recovery copy is untouched.
	if got := len(legacyCacheFiles(dir)); got != len(files) {
		t.Fatalf("recovery copy holds %d files, want %d kept in place", got, len(files))
	}
	if after := dirSnapshot(t, live); !reflect.DeepEqual(before, after) {
		t.Fatalf("live legacy dir changed during the test, want it untouched")
	}
}

// dirSnapshot is a comparable (name, size, mtime) snapshot of a directory.
func dirSnapshot(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			t.Fatalf("stat %s: %v", e.Name(), err)
		}
		out = append(out, fmt.Sprintf("%s|%d|%d", e.Name(), fi.Size(), fi.ModTime().UnixNano()))
	}
	return out
}

// TestCacheConcurrentAccess proves concurrent readers and writers on one cache
// directory are safe (run it under -race to check for data races): the store
// and the handle registry both serialize.
func TestCacheConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	shared := cacheKey(Query{Kind: KindSeries, Title: "Shared Show", Season: 1, Episode: 1})
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			key := cacheKey(Query{Kind: KindSeries, Title: "Show " + strconv.Itoa(w), Season: 1, Episode: 1})
			for i := 0; i < 20; i++ {
				m := &Media{Source: "fake", SourceID: strconv.Itoa(w), Kind: KindSeries,
					Title: "Show " + strconv.Itoa(w)}
				if err := cacheStore(dir, key, m); err != nil {
					t.Errorf("cacheStore: %v", err)
					return
				}
				if err := cacheStoreNegative(dir, shared); err != nil {
					t.Errorf("cacheStoreNegative: %v", err)
					return
				}
				cacheLoad(dir, key)
				cacheLoadEntry(dir, shared)
				CacheLive(dir, key)
				CacheHas(dir, key)
				CountNegatives(dir)
			}
		}(w)
	}
	wg.Wait()
	if !CacheHas(dir, shared) {
		t.Fatal("shared key missing after concurrent writes, want present")
	}
	if got := CountNegatives(dir); got != 1 {
		t.Fatalf("CountNegatives after concurrent writes = %d, want 1 (one shared negative key)", got)
	}
}

// TestCacheForEachEnumeratesLiveStore proves the exported enumeration reads the
// store's live records - the whole cache - with each value the record's stored
// bytes, that a legacy-only directory is imported first, that fn can stop early,
// and that a directory with no cache calls fn zero times and is not an error.
func TestCacheForEachEnumeratesLiveStore(t *testing.T) {
	t.Cleanup(closeCacheHandles)
	dir := t.TempDir()
	keys := []string{
		cacheKey(Query{Kind: KindSeries, Title: "Alpha", Season: 1}),
		cacheKey(Query{Kind: KindMovie, Title: "Beta", Year: 1999}),
		cacheKey(Query{Kind: KindSeries, Title: "Gamma", Season: 2}),
	}
	for _, k := range keys {
		if err := cacheStore(dir, k, &Media{Source: "fake", Kind: KindSeries, Title: k}); err != nil {
			t.Fatalf("cacheStore %q: %v", k, err)
		}
	}
	neg := cacheKey(Query{Kind: KindSeries, Title: "Dead", Season: 1})
	if err := cacheStoreNegative(dir, neg); err != nil {
		t.Fatalf("cacheStoreNegative: %v", err)
	}

	seen := map[string]int{}
	if err := CacheForEach(dir, func(key string, value []byte) bool {
		seen[key]++
		var cf cacheFile
		if err := json.Unmarshal(value, &cf); err != nil {
			t.Errorf("value for %q does not decode as a record: %v", key, err)
			return true
		}
		if cf.Key != key {
			t.Errorf("value for %q carries key %q, want the store key", key, cf.Key)
		}
		return true
	}); err != nil {
		t.Fatalf("CacheForEach: %v", err)
	}
	for _, k := range append(append([]string{}, keys...), neg) {
		if seen[k] != 1 {
			t.Fatalf("key %q seen %d times, want exactly 1 (seen: %v)", k, seen[k], seen)
		}
	}
	if len(seen) != 4 {
		t.Fatalf("enumeration saw %d keys, want 4", len(seen))
	}

	// Early stop: fn returning false ends the walk.
	n := 0
	if err := CacheForEach(dir, func(string, []byte) bool { n++; return false }); err != nil {
		t.Fatalf("CacheForEach (early stop): %v", err)
	}
	if n != 1 {
		t.Fatalf("early stop called fn %d times, want 1", n)
	}

	// A legacy-only directory is imported by the enumeration, exactly as any
	// other first touch does.
	legacy := t.TempDir()
	lkey := cacheKey(Query{Kind: KindMovie, Title: "Legacy", Year: 2001})
	payload, err := json.Marshal(cacheFile{Key: lkey, Schema: cacheSchema,
		Media: Media{Source: "fake", Title: "Legacy"}})
	if err != nil {
		t.Fatalf("marshal legacy record: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacy, cacheFileName(lkey)), payload, 0o644); err != nil {
		t.Fatalf("write legacy file: %v", err)
	}
	got := []string{}
	if err := CacheForEach(legacy, func(key string, _ []byte) bool {
		got = append(got, key)
		return true
	}); err != nil {
		t.Fatalf("CacheForEach(legacy): %v", err)
	}
	if len(got) != 1 || got[0] != lkey {
		t.Fatalf("legacy enumeration = %v, want [%q]", got, lkey)
	}

	// No cache on disk: no calls, no error, and no cache is created.
	empty := t.TempDir()
	calls := 0
	if err := CacheForEach(empty, func(string, []byte) bool { calls++; return true }); err != nil {
		t.Fatalf("CacheForEach(empty): %v", err)
	}
	if calls != 0 {
		t.Fatalf("empty directory called fn %d times, want 0", calls)
	}
	if _, err := os.Stat(cacheIndexPath(empty)); !os.IsNotExist(err) {
		t.Fatalf("a read created a cache file: %v", err)
	}
	if err := CacheForEach(empty, nil); err != nil {
		t.Fatalf("CacheForEach with a nil callback: %v", err)
	}
}

// TestCacheForEachUsesTheHeldHandle proves the enumeration runs against the
// process's ONE held store handle rather than opening (or replacing) one: the
// handle is the same before and after, it is still held, its records are
// untouched, and a second walk still sees them.
func TestCacheForEachUsesTheHeldHandle(t *testing.T) {
	t.Cleanup(closeCacheHandles)
	dir := t.TempDir()
	key := cacheKey(Query{Kind: KindSeries, Title: "Held", Season: 1})
	if err := cacheStore(dir, key, &Media{Source: "fake", Title: "Held"}); err != nil {
		t.Fatalf("cacheStore: %v", err)
	}
	before, err := cacheHandleFor(dir)
	if err != nil {
		t.Fatalf("cacheHandleFor: %v", err)
	}
	if !cacheHandleHeld(dir) {
		t.Fatal("cache handle is not held after a store write")
	}
	for i := 0; i < 2; i++ {
		n := 0
		if err := CacheForEach(dir, func(string, []byte) bool { n++; return true }); err != nil {
			t.Fatalf("CacheForEach run %d: %v", i, err)
		}
		if n != 1 {
			t.Fatalf("run %d saw %d records, want 1", i, n)
		}
		after, err := cacheHandleFor(dir)
		if err != nil {
			t.Fatalf("cacheHandleFor after run %d: %v", i, err)
		}
		if after != before {
			t.Fatalf("run %d replaced the held handle", i)
		}
		if before.db.Len() != 1 {
			t.Fatalf("run %d left the store with %d records, want 1", i, before.db.Len())
		}
	}
	if !CacheLive(dir, key) {
		t.Fatal("the enumerated entry stopped being live")
	}
}

// TestClearCacheEntriesClearsEntriesOnly proves the entry-level clear drops every
// stored record, reports how many titles it cleared, and leaves the store file,
// the directory and every non-entry file exactly where they were - the split
// ClearCache (which also removes the directory) depends on.
func TestClearCacheEntriesClearsEntriesOnly(t *testing.T) {
	t.Cleanup(closeCacheHandles)
	dir := t.TempDir()
	posKey := cacheKey(Query{Kind: KindSeries, Title: "Pos", Season: 1})
	negKey := cacheKey(Query{Kind: KindSeries, Title: "Neg", Season: 1})
	if err := cacheStore(dir, posKey, &Media{Source: "fake", Title: "Pos"}); err != nil {
		t.Fatalf("cacheStore: %v", err)
	}
	if err := cacheStoreNegative(dir, negKey); err != nil {
		t.Fatalf("cacheStoreNegative: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.json"), []byte(`{"keep":true}`), 0o644); err != nil {
		t.Fatalf("write notes.json: %v", err)
	}

	n, err := ClearCacheEntries(dir)
	if err != nil {
		t.Fatalf("ClearCacheEntries: %v", err)
	}
	if n != 2 {
		t.Fatalf("ClearCacheEntries cleared %d, want 2", n)
	}
	if CacheHas(dir, posKey) || CacheLive(dir, negKey) || CountNegatives(dir) != 0 {
		t.Fatal("cleared cache still reports entries")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("cache directory was removed: %v", err)
	}
	if _, err := os.Stat(cacheIndexPath(dir)); err != nil {
		t.Fatalf("store file was removed: %v", err)
	}
	if keep, err := os.ReadFile(filepath.Join(dir, "notes.json")); err != nil || string(keep) != `{"keep":true}` {
		t.Fatalf("non-entry file touched: %q, %v", keep, err)
	}
	// Idempotent, and the store stays usable for the next write.
	if n, err := ClearCacheEntries(dir); err != nil || n != 0 {
		t.Fatalf("second ClearCacheEntries = (%d, %v), want (0, nil)", n, err)
	}
	if err := cacheStore(dir, posKey, &Media{Source: "fake", Title: "Pos"}); err != nil {
		t.Fatalf("cacheStore after clear: %v", err)
	}
	if !CacheHas(dir, posKey) {
		t.Fatal("store unusable after ClearCacheEntries")
	}
}

// TestClearCacheEntriesCountsImportedLegacyTitles proves the clear counts the
// titles a legacy directory still holds, by importing it as any first touch
// does - and that the legacy files themselves are left alone: removing them is
// ClearCache's (and bundle.Clear's) job, never this one's.
func TestClearCacheEntriesCountsImportedLegacyTitles(t *testing.T) {
	t.Cleanup(closeCacheHandles)
	dir := t.TempDir()
	keys := []string{
		cacheKey(Query{Kind: KindSeries, Title: "One", Season: 1}),
		cacheKey(Query{Kind: KindMovie, Title: "Two", Year: 1994}),
	}
	for _, k := range keys {
		payload, err := json.Marshal(cacheFile{Key: k, Schema: cacheSchema, Media: Media{Source: "fake", Title: k}})
		if err != nil {
			t.Fatalf("marshal legacy record: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, cacheFileName(k)), payload, 0o644); err != nil {
			t.Fatalf("write legacy file: %v", err)
		}
	}

	n, err := ClearCacheEntries(dir)
	if err != nil {
		t.Fatalf("ClearCacheEntries: %v", err)
	}
	if n != 2 {
		t.Fatalf("ClearCacheEntries cleared %d titles, want 2", n)
	}
	// The store itself holds nothing now. Read it through the held handle:
	// any read path re-imports the legacy files this call deliberately left
	// behind, which is exactly why ClearCache (and bundle.Clear) remove them
	// - an emptied store beside an untouched recovery directory refills on the
	// next touch.
	h, err := cacheHandleFor(dir)
	if err != nil {
		t.Fatalf("cacheHandleFor: %v", err)
	}
	if got := h.db.Len(); got != 0 {
		t.Fatalf("store still holds %d records after the clear, want 0", got)
	}
	for _, k := range keys {
		if _, err := os.Stat(filepath.Join(dir, cacheFileName(k))); err != nil {
			t.Fatalf("legacy file for %q was removed: %v", k, err)
		}
	}
}

// TestCachePutRawStoresBytesVerbatim proves the bulk-write seam stores what it
// is given, that the record is immediately live, and that it refuses what the
// store cannot hold (an over-size value, an empty key, no directory).
func TestCachePutRawStoresBytesVerbatim(t *testing.T) {
	t.Cleanup(closeCacheHandles)
	dir := t.TempDir()
	key := cacheKey(Query{Kind: KindSeries, Title: "Raw", Season: 1})
	payload, err := json.Marshal(cacheFile{Key: key, Schema: cacheSchema,
		Media: Media{Source: "fake", Title: "Raw"}, CheckedAt: time.Now().UTC()})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := CachePutRaw(dir, key, payload); err != nil {
		t.Fatalf("CachePutRaw: %v", err)
	}
	if !CacheHas(dir, key) || !CacheLive(dir, key) {
		t.Fatal("a raw put is not visible to CacheHas/CacheLive")
	}
	stored, ok := cacheRecord(dir, key)
	if !ok {
		t.Fatal("stored record missing")
	}
	if string(stored) != string(payload) {
		t.Fatalf("stored bytes differ from the payload:\n%s\n%s", stored, payload)
	}

	big := make([]byte, cacheMaxValueSize+1)
	if err := CachePutRaw(dir, "movie|huge|2001", big); err == nil {
		t.Fatal("over-size value accepted, want a refusal")
	}
	if CacheHas(dir, "movie|huge|2001") {
		t.Fatal("refused over-size value was stored anyway")
	}
	if err := CachePutRaw(dir, "", []byte("{}")); err == nil {
		t.Fatal("empty key accepted, want an error")
	}
	if err := CachePutRaw("", key, []byte("{}")); err == nil {
		t.Fatal("empty directory accepted, want an error")
	}
}
