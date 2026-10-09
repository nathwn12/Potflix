package metadata

import (
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
