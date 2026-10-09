package metadata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestStripHTML(t *testing.T) {
	got := stripHTML(`<p>Hello <b>World</b></p><br>next line`)
	if got != "Hello World next line" {
		t.Fatalf("stripHTML tags = %q", got)
	}
	got = stripHTML(`A &amp; B &lt;C&gt; &quot;Q&quot; &#39;S&#39;&nbsp;&nbsp;end`)
	if got != `A & B <C> "Q" 'S' end` {
		t.Fatalf("stripHTML entities = %q", got)
	}
	got = stripHTML("  line one\n\nline   two\r\nline three  ")
	if got != "line one line two line three" {
		t.Fatalf("stripHTML whitespace = %q", got)
	}
	if got := stripHTML(""); got != "" {
		t.Fatalf("stripHTML empty = %q", got)
	}
	// A bare "<" with no closing ">" is literal text, not a tag.
	if got := stripHTML("a < b"); got != "a < b" {
		t.Fatalf("stripHTML bare angle = %q", got)
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "  ", "b", "c"); got != "b" {
		t.Fatalf("firstNonEmpty = %q", got)
	}
	if got := firstNonEmpty("", " "); got != "" {
		t.Fatalf("firstNonEmpty all-blank = %q", got)
	}
}

func TestCapSynopsis(t *testing.T) {
	short := "A short synopsis."
	if got := capSynopsis(short); got != short {
		t.Fatalf("capSynopsis short = %q", got)
	}
	// Long single-word-run-free text: capped at a word boundary <= 300 runes.
	long := strings.TrimSpace(strings.Repeat("word ", 100))
	got := capSynopsis(long)
	if n := len([]rune(got)); n > maxSynopsisRunes {
		t.Fatalf("capSynopsis runes = %d, want <= %d", n, maxSynopsisRunes)
	}
	if strings.HasSuffix(got, "wor") || strings.HasSuffix(got, "w") {
		t.Fatalf("capSynopsis cut mid-word: %q", got)
	}
	if !strings.HasSuffix(got, "word") {
		t.Fatalf("capSynopsis word boundary = %q", got)
	}
	// Sentence boundary wins when one exists in the back half.
	sentences := "First sentence ends here. " + strings.Repeat("filler text here. ", 30)
	got = capSynopsis(sentences)
	if n := len([]rune(got)); n > maxSynopsisRunes {
		t.Fatalf("capSynopsis sentences runes = %d", n)
	}
	if !strings.HasSuffix(got, ".") {
		t.Fatalf("capSynopsis sentence boundary = %q", got)
	}
}

// TestCacheSchemaMismatchIsMiss proves pre-art entries (Schema 1 or absent)
// are treated as a MISS while current-schema entries still hit.
func TestCacheSchemaMismatchIsMiss(t *testing.T) {
	dir := t.TempDir()
	q := Query{Kind: KindSeries, Title: "Show", Season: 1}
	key := cacheKey(q)

	legacy := `{"key":` + strconv.Quote(key) + `,"media":{"source":"tvmaze","sourceId":"1","kind":1,"title":"Show","fetchedAt":"2026-01-01T00:00:00Z"}}`
	if err := os.WriteFile(filepath.Join(dir, cacheFileName(key)), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := cacheLoad(dir, key); ok {
		t.Fatal("schema-less legacy entry should be a miss")
	}

	schema1 := `{"key":` + strconv.Quote(key) + `,"schema":1,"media":{"source":"tvmaze","sourceId":"1","kind":1,"title":"Show","fetchedAt":"2026-01-01T00:00:00Z"}}`
	if err := os.WriteFile(filepath.Join(dir, cacheFileName(key)), []byte(schema1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := cacheLoad(dir, key); ok {
		t.Fatal("Schema:1 entry should be a miss")
	}

	media := &Media{
		Source: "tvmaze", SourceID: "1",
		Kind: KindSeries, Title: "Show",
		Artwork: "https://example.com/poster.jpg", Synopsis: "A show.",
		Episodes:  []Episode{{Season: 1, Number: 1, Title: "Pilot"}},
		FetchedAt: time.Now().UTC().Truncate(time.Millisecond),
	}
	if err := cacheStore(dir, key, media); err != nil {
		t.Fatalf("cacheStore: %v", err)
	}
	got, ok := cacheLoad(dir, key)
	if !ok {
		t.Fatal("current-schema entry should hit")
	}
	if got.Artwork != media.Artwork || got.Synopsis != media.Synopsis {
		t.Fatalf("art/synopsis round-trip = %+v", got)
	}
}

// TestTVMazeArtworkFixture proves a provider fixture yields non-empty
// Artwork+Synopsis at both show and episode level, and that Resolution
// carries the SERIES poster (cover rework: episode stills never override)
// plus the episode-preferred synopsis.
func TestTVMazeArtworkFixture(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/shows", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"score":1.0,"show":{"id":169,"name":"Breaking Bad","premiered":"2008-01-20","weight":99,"image":{"medium":"https://example.com/show-medium.jpg","original":"https://example.com/show-original.jpg"},"summary":"<p>A chemistry teacher turns cook.</p>"}}]`))
	})
	mux.HandleFunc("/shows/169/seasons", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":753,"number":1}]`))
	})
	mux.HandleFunc("/seasons/753/episodes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":12192,"name":"Pilot","season":1,"number":1,"image":{"medium":"https://example.com/ep1-medium.jpg","original":"https://example.com/ep1-original.jpg"},"summary":"<p>Walter faces episode one.</p>"},{"id":12193,"name":"Second","season":1,"number":2}]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s := &tvmazeSource{base: srv.URL, client: srv.Client()}
	m, err := s.Find(context.Background(), Query{Kind: KindSeries, Title: "Breaking Bad", Season: 1, Episode: 1})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if m == nil {
		t.Fatal("expected media, got nil")
	}
	if m.Artwork != "https://example.com/show-original.jpg" {
		t.Fatalf("show artwork = %q", m.Artwork)
	}
	if m.Synopsis == "" || strings.ContainsAny(m.Synopsis, "<>") {
		t.Fatalf("show synopsis not cleaned: %q", m.Synopsis)
	}
	if len(m.Episodes) == 0 || m.Episodes[0].Artwork != "https://example.com/ep1-original.jpg" {
		t.Fatalf("episode artwork = %+v", m.Episodes)
	}
	if m.Episodes[0].Synopsis == "" {
		t.Fatalf("episode synopsis empty: %+v", m.Episodes[0])
	}

	r := New(Options{Dir: t.TempDir(), Sources: []Source{s}, Timeout: 5 * time.Second})
	res, err := r.LookupNow(context.Background(), Query{Kind: KindSeries, Title: "Breaking Bad", Season: 1, Episode: 1})
	if err != nil {
		t.Fatalf("LookupNow: %v", err)
	}
	if !res.Found {
		t.Fatalf("resolution = %+v, want found", res)
	}
	// Series poster always wins over episode stills; episode summary wins over show summary.
	if res.Artwork != "https://example.com/show-original.jpg" {
		t.Fatalf("resolution artwork = %q", res.Artwork)
	}
	if !strings.Contains(res.Synopsis, "episode one") {
		t.Fatalf("resolution synopsis = %q, want episode-level text", res.Synopsis)
	}
}

// TestResolutionFallsBackToSeriesArt proves the series-level pair is used
// when the matched episode carries none.
func TestResolutionFallsBackToSeriesArt(t *testing.T) {
	fake := &fakeSource{name: "fake", media: &Media{
		Source: "fake", SourceID: "1",
		Kind: KindSeries, Title: "Show",
		Artwork: "https://example.com/show.jpg", Synopsis: "<p>Show summary.</p>",
		Episodes:  []Episode{{Season: 1, Number: 2, Title: "Pilot"}},
		FetchedAt: time.Now().UTC(),
	}}
	r := New(Options{Dir: t.TempDir(), Sources: []Source{fake}, Timeout: 5 * time.Second})
	res, err := r.LookupNow(context.Background(), Query{Kind: KindSeries, Title: "Show", Season: 1, Episode: 2})
	if err != nil {
		t.Fatalf("LookupNow: %v", err)
	}
	if !res.Found || res.Artwork != "https://example.com/show.jpg" {
		t.Fatalf("resolution = %+v, want series art fallback", res)
	}
	if res.Synopsis != "Show summary." {
		t.Fatalf("resolution synopsis = %q, want stripped series summary", res.Synopsis)
	}
}
