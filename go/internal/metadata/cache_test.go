package metadata

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"
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

// TestCacheHasStatOnly proves CacheHas is a stat-only existence check: it
// reports true for an existing entry even when the body is corrupt
// (callers needing the schema use the index's own Schema field), and false
// for a missing one - without ever reading or parsing the file body.
func TestCacheHasStatOnly(t *testing.T) {
	dir := t.TempDir()
	key := cacheKey(Query{Kind: KindSeries, Title: "Stat Only", Year: 2001, Season: 1})
	if CacheHas(dir, key) {
		t.Fatal("CacheHas on a missing entry = true, want false")
	}
	if CacheHas(dir, "") {
		t.Fatal("CacheHas on an empty key = true, want false")
	}
	// Invalid JSON: an existence check still reports present (a read+parse
	// check would report absent), proving the body is never read.
	if err := os.WriteFile(filepath.Join(dir, cacheFileName(key)), []byte("not json{{{corrupt"), 0o644); err != nil {
		t.Fatalf("write corrupt fixture: %v", err)
	}
	if !CacheHas(dir, key) {
		t.Fatal("CacheHas on an existing (corrupt) entry = false, want true (stat-only, no read/parse)")
	}
	// A valid stored entry still reports present.
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
// and still satisfies the stat-based CacheHas guard.
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
		t.Fatal("CacheHas on a negative entry = false, want true (same filename)")
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

// TestCacheLiveCorrupt proves corrupt / truncated / wrong-schema /
// oversize / missing files report not-live (unlike stat-only CacheHas).
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
	// Oversize file is rejected without slurping.
	big := make([]byte, cacheLiveMaxBytes+1024)
	for i := range big {
		big[i] = 'x'
	}
	if err := os.WriteFile(filepath.Join(dir, cacheFileName(key)), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if CacheLive(dir, key) {
		t.Fatal("CacheLive on an oversize file = true, want false")
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
	// Near-future within the skew window is tolerated as fresh.
	near := time.Now().UTC().Add(2 * time.Minute).Truncate(time.Millisecond)
	data, err = json.Marshal(cacheFile{Key: key, Schema: cacheSchema, NotFound: true, CheckedAt: near})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, cacheFileName(key)), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if !CacheLive(dir, key) {
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
