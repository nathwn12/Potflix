package metadata

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// wikiFake serves canned search+detail JSON and records hits and the
// User-Agent it saw, so tests prove the wire shape without the network.
// images/imageinfo bodies serve the poster-fallback calls (prop=images,
// prop=imageinfo); "" serves an empty image list / empty imageinfo.
type wikiFake struct {
	t         *testing.T
	hits      int
	sawUA     string
	search    string // body for list=search; "" serves {"query":{"search":[]}}
	detail    string // body for prop=extracts|pageimages
	images    string // body for prop=images
	imageinfo string // body for prop=imageinfo
	failMode  string // "search500" | "detail500" | "badjson" | "images500" | "imageinfo500" | ""
}

func (f *wikiFake) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.hits++
		f.sawUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		if q.Get("list") == "search" {
			if f.failMode == "search500" {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			if f.failMode == "badjson" {
				_, _ = w.Write([]byte(`{not json`))
				return
			}
			body := f.search
			if body == "" {
				body = `{"query":{"search":[]}}`
			}
			_, _ = w.Write([]byte(body))
			return
		}
		if q.Get("prop") == "images" {
			if f.failMode == "images500" {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			body := f.images
			if body == "" {
				body = `{"query":{"pages":{"12345":{"pageid":12345,"images":[]}}}}`
			}
			_, _ = w.Write([]byte(body))
			return
		}
		if strings.Contains(q.Get("prop"), "imageinfo") {
			if f.failMode == "imageinfo500" {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			body := f.imageinfo
			if body == "" {
				body = `{"query":{"pages":{"-1":{"missing":true}}}}`
			}
			_, _ = w.Write([]byte(body))
			return
		}
		if f.failMode == "detail500" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(f.detail))
	}
}

const wikiHitSearch = `{"query":{"search":[{"title":"Dune (2021 film)"},{"title":"Dune (novel)"}]}}`

const wikiHitDetail = `{"query":{"pages":{"12345":{"pageid":12345,"title":"Dune (2021 film)",` +
	`"extract":"Dune is a 2021 American epic science fiction film directed by Denis Villeneuve.",` +
	`"thumbnail":{"source":"https://upload.wikimedia.org/wikipedia/en/thumb/8/8e/Dune_poster.jpg","width":600,"height":900}}}}}`

// TestWikipediaHit proves the two-call path yields the card fields:
// title, year from "(2021 film)", thumbnail poster, extract synopsis.
func TestWikipediaHit(t *testing.T) {
	f := &wikiFake{t: t, search: wikiHitSearch, detail: wikiHitDetail}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	s := &wikipediaSource{base: srv.URL, client: srv.Client()}
	m, err := s.Find(context.Background(), Query{Kind: KindMovie, Title: "Dune", Year: 2021})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if m == nil {
		t.Fatal("expected media, got nil")
	}
	if m.Title != "Dune (2021 film)" {
		t.Fatalf("title = %q", m.Title)
	}
	if m.Year != 2021 {
		t.Fatalf("year = %d, want 2021", m.Year)
	}
	if m.Kind != KindMovie {
		t.Fatalf("kind = %v, want KindMovie", m.Kind)
	}
	if !strings.HasPrefix(m.Artwork, "https://upload.wikimedia.org/") {
		t.Fatalf("artwork = %q, want upload.wikimedia.org poster", m.Artwork)
	}
	if !strings.Contains(m.Synopsis, "2021 American epic science fiction film") {
		t.Fatalf("synopsis = %q", m.Synopsis)
	}
	if m.Source != "wikipedia" {
		t.Fatalf("source = %q", m.Source)
	}
	if len(m.Episodes) != 0 {
		t.Fatalf("movie must have no episodes: %+v", m.Episodes)
	}
	if f.hits != 2 {
		t.Fatalf("hits = %d, want exactly 2 (search+detail)", f.hits)
	}
	if !strings.Contains(f.sawUA, "Potflix") {
		t.Fatalf("User-Agent = %q, want descriptive Potflix identifier per Wikimedia policy", f.sawUA)
	}

	r := New(Options{Dir: t.TempDir(), Sources: []Source{s}, Timeout: 5 * time.Second})
	res, err := r.LookupNow(context.Background(), Query{Kind: KindMovie, Title: "Dune", Year: 2021})
	if err != nil {
		t.Fatalf("LookupNow: %v", err)
	}
	if !res.Found || !res.IsMovie || res.Year != 2021 {
		t.Fatalf("resolution = %+v", res)
	}
	if res.Artwork == "" || res.Synopsis == "" {
		t.Fatalf("resolution lacks card fields: %+v", res)
	}
}

// TestWikipediaDefinitiveMiss proves every "no such page" shape is a
// definitive miss (nil, nil) - never the transient sentinel.
func TestWikipediaDefinitiveMiss(t *testing.T) {
	cases := []struct {
		name   string
		search string
		detail string
	}{
		{"empty search results", "", wikiHitDetail},
		{"blank search title", `{"query":{"search":[{"title":"  "}]}}`, wikiHitDetail},
		{"missing page", wikiHitSearch, `{"query":{"pages":{"-1":{"title":"Dune (2021 film)","missing":true}}}}`},
		{"no pages", wikiHitSearch, `{"query":{"pages":{}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &wikiFake{search: tc.search, detail: tc.detail}
			srv := httptest.NewServer(f.handler())
			defer srv.Close()
			s := &wikipediaSource{base: srv.URL, client: srv.Client()}
			m, err := s.Find(context.Background(), Query{Kind: KindMovie, Title: "Xyzzy Qqq"})
			if err != nil || m != nil {
				t.Fatalf("Find = (%v, %v), want (nil, nil)", m, err)
			}
		})
	}
}

// TestWikipediaTransientIsSentinel proves transport and API failures are
// distinguishable from a miss: they return ErrLookupUnavailable.
func TestWikipediaTransientIsSentinel(t *testing.T) {
	cases := []struct {
		name string
		fail string
	}{
		{"search 500", "search500"},
		{"detail 500", "detail500"},
		{"search bad json", "badjson"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &wikiFake{search: wikiHitSearch, detail: wikiHitDetail, failMode: tc.fail}
			srv := httptest.NewServer(f.handler())
			defer srv.Close()
			s := &wikipediaSource{base: srv.URL, client: srv.Client()}
			m, err := s.Find(context.Background(), Query{Kind: KindMovie, Title: "Dune"})
			if m != nil {
				t.Fatalf("Find media = %+v, want nil on transient", m)
			}
			if !errors.Is(err, ErrLookupUnavailable) {
				t.Fatalf("Find err = %v, want ErrLookupUnavailable", err)
			}
		})
	}

	// Dead server (connection refused) is transient too.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	s := &wikipediaSource{base: deadURL, client: dead.Client()}
	if _, err := s.Find(context.Background(), Query{Kind: KindMovie, Title: "Dune"}); !errors.Is(err, ErrLookupUnavailable) {
		t.Fatalf("dead-server err = %v, want ErrLookupUnavailable", err)
	}
}

// TestWikipediaKindGate proves a series key never reaches the wire: against
// a hit-counting server it returns (nil, nil) with zero hits, and a blank
// title gates the same way.
func TestWikipediaKindGate(t *testing.T) {
	f := &wikiFake{search: wikiHitSearch, detail: wikiHitDetail}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	s := &wikipediaSource{base: srv.URL, client: srv.Client()}
	ctx := context.Background()

	for _, q := range []Query{
		{Kind: KindSeries, Title: "Breaking Bad", Season: 1, Episode: 1},
		{Kind: KindMovie, Title: ""},
	} {
		if m, err := s.Find(ctx, q); err != nil || m != nil {
			t.Fatalf("gated query %+v: Find = (%v, %v), want (nil, nil)", q, m, err)
		}
	}
	if f.hits != 0 {
		t.Fatalf("gated queries reached the wire %d times, want 0", f.hits)
	}

	// Positive control: a movie query DOES reach the wire.
	if _, err := s.Find(ctx, Query{Kind: KindMovie, Title: "Dune"}); err != nil {
		t.Fatalf("movie Find: %v", err)
	}
	if f.hits == 0 {
		t.Fatal("movie query made no wire calls, positive control failed")
	}
}

// TestWikipediaNoPosterStillFound pins the common live shape (measured
// 2026-10-10: PageImages serves no thumbnail for film articles, since
// posters are fair-use): a poster-less page still yields a FOUND movie
// with title, year and synopsis, Artwork honestly empty - never invented.
func TestWikipediaNoPosterStillFound(t *testing.T) {
	f := &wikiFake{search: wikiHitSearch, detail: `{"query":{"pages":{"12345":{"pageid":12345,` +
		`"title":"Dune (2021 film)",` +
		`"extract":"Dune is a 2021 American epic science fiction film directed by Denis Villeneuve."}}}}`}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	s := &wikipediaSource{base: srv.URL, client: srv.Client()}
	m, err := s.Find(context.Background(), Query{Kind: KindMovie, Title: "Dune"})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if m == nil {
		t.Fatal("expected media, got nil")
	}
	if m.Artwork != "" {
		t.Fatalf("artwork = %q, want empty (no thumbnail served)", m.Artwork)
	}
	if m.Title != "Dune (2021 film)" || m.Year != 2021 {
		t.Fatalf("media = %+v, want title+year", m)
	}
	if m.Synopsis == "" {
		t.Fatal("expected synopsis, got empty")
	}
	if m.MovieURL == "" || !strings.HasPrefix(m.MovieURL, "https://") {
		t.Fatalf("movieURL = %q, want article URL", m.MovieURL)
	}
}

// TestWikipediaPosterFromImageList proves the poster fallback: a resolved
// page with no PageImages thumbnail lists the article files via
// prop=images, picks the poster-like file, and resolves it to a direct
// URL via prop=imageinfo - exactly 4 calls (search+detail+images+
// imageinfo), Artwork set to the expected URL. The fixture mixes in a
// logo and an svg "poster" to prove the pick rule rejects them.
func TestWikipediaPosterFromImageList(t *testing.T) {
	f := &wikiFake{
		search: wikiHitSearch,
		detail: `{"query":{"pages":{"12345":{"pageid":12345,` +
			`"title":"Dune (2021 film)",` +
			`"extract":"Dune is a 2021 American epic science fiction film directed by Denis Villeneuve."}}}}`,
		images: `{"query":{"pages":{"12345":{"pageid":12345,"images":[` +
			`{"title":"File:Dune logo.png"},` +
			`{"title":"File:Dune poster guidelines.svg"},` +
			`{"title":"File:Dune theatrical poster.jpg"}]}}}}`,
		imageinfo: `{"query":{"pages":{"999":{"pageid":999,` +
			`"title":"File:Dune theatrical poster.jpg",` +
			`"imageinfo":[{"url":"https://upload.wikimedia.org/wikipedia/en/8/8e/Dune_theatrical_poster.jpg"}]}}}}`,
	}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	s := &wikipediaSource{base: srv.URL, client: srv.Client()}
	m, err := s.Find(context.Background(), Query{Kind: KindMovie, Title: "Dune"})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if m == nil {
		t.Fatal("expected media, got nil")
	}
	want := "https://upload.wikimedia.org/wikipedia/en/8/8e/Dune_theatrical_poster.jpg"
	if m.Artwork != want {
		t.Fatalf("artwork = %q, want %q", m.Artwork, want)
	}
	if m.Title != "Dune (2021 film)" || m.Year != 2021 || m.Synopsis == "" {
		t.Fatalf("media lost card fields: %+v", m)
	}
	if f.hits != 4 {
		t.Fatalf("hits = %d, want exactly 4 (search+detail+images+imageinfo)", f.hits)
	}
}

// TestWikipediaPosterAbsentStillFound proves a poster-less image list
// keeps today's behavior: the lookup still succeeds with title, year and
// synopsis, and Artwork stays honestly empty - never invented.
func TestWikipediaPosterAbsentStillFound(t *testing.T) {
	f := &wikiFake{
		search: wikiHitSearch,
		detail: `{"query":{"pages":{"12345":{"pageid":12345,` +
			`"title":"Dune (2021 film)",` +
			`"extract":"Dune is a 2021 American epic science fiction film directed by Denis Villeneuve."}}}}`,
		images: `{"query":{"pages":{"12345":{"pageid":12345,"images":[]}}}}`,
	}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	s := &wikipediaSource{base: srv.URL, client: srv.Client()}
	m, err := s.Find(context.Background(), Query{Kind: KindMovie, Title: "Dune"})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if m == nil {
		t.Fatal("expected media, got nil")
	}
	if m.Artwork != "" {
		t.Fatalf("artwork = %q, want empty (no poster-like file)", m.Artwork)
	}
	if m.Title != "Dune (2021 film)" || m.Year != 2021 || m.Synopsis == "" {
		t.Fatalf("media lost card fields: %+v", m)
	}
	if f.hits != 3 {
		t.Fatalf("hits = %d, want exactly 3 (search+detail+images, no imageinfo call)", f.hits)
	}
}

// TestWikipediaNonPosterImageIgnored proves a non-poster image (a logo)
// is never substituted: the lookup succeeds and Artwork stays empty.
func TestWikipediaNonPosterImageIgnored(t *testing.T) {
	f := &wikiFake{
		search: wikiHitSearch,
		detail: `{"query":{"pages":{"12345":{"pageid":12345,` +
			`"title":"Dune (2021 film)",` +
			`"extract":"Dune is a 2021 American epic science fiction film directed by Denis Villeneuve."}}}}`,
		images: `{"query":{"pages":{"12345":{"pageid":12345,"images":[` +
			`{"title":"File:Dune logo.png"},` +
			`{"title":"File:Dune cast at premiere.jpg"}]}}}}`,
		imageinfo: `{"query":{"pages":{"999":{"pageid":999,` +
			`"title":"File:Dune logo.png",` +
			`"imageinfo":[{"url":"https://upload.wikimedia.org/wikipedia/en/9/9e/Dune_logo.png"}]}}}}`,
	}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	s := &wikipediaSource{base: srv.URL, client: srv.Client()}
	m, err := s.Find(context.Background(), Query{Kind: KindMovie, Title: "Dune"})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if m == nil {
		t.Fatal("expected media, got nil")
	}
	if m.Artwork != "" {
		t.Fatalf("artwork = %q, want empty (logo/cast photo must never substitute)", m.Artwork)
	}
	if m.Title != "Dune (2021 film)" || m.Synopsis == "" {
		t.Fatalf("media lost card fields: %+v", m)
	}
	if f.hits != 3 {
		t.Fatalf("hits = %d, want exactly 3 (no imageinfo call for a non-poster list)", f.hits)
	}
}

// TestWikipediaPosterFailureStillFound proves the fallback is best-effort:
// a 500 on either poster call still yields the resolved movie with empty
// Artwork - never the transient sentinel for an already-resolved page.
func TestWikipediaPosterFailureStillFound(t *testing.T) {
	for _, mode := range []string{"images500", "imageinfo500"} {
		t.Run(mode, func(t *testing.T) {
			f := &wikiFake{
				search: wikiHitSearch,
				detail: `{"query":{"pages":{"12345":{"pageid":12345,` +
					`"title":"Dune (2021 film)",` +
					`"extract":"Dune is a 2021 American epic science fiction film directed by Denis Villeneuve."}}}}`,
				images: `{"query":{"pages":{"12345":{"pageid":12345,"images":[` +
					`{"title":"File:Dune theatrical poster.jpg"}]}}}}`,
				imageinfo: `{"query":{"pages":{"999":{"pageid":999,` +
					`"title":"File:Dune theatrical poster.jpg",` +
					`"imageinfo":[{"url":"https://upload.wikimedia.org/wikipedia/en/8/8e/Dune_theatrical_poster.jpg"}]}}}}`,
				failMode: mode,
			}
			srv := httptest.NewServer(f.handler())
			defer srv.Close()

			s := &wikipediaSource{base: srv.URL, client: srv.Client()}
			m, err := s.Find(context.Background(), Query{Kind: KindMovie, Title: "Dune"})
			if err != nil {
				t.Fatalf("Find err = %v, want nil (poster failure is best-effort)", err)
			}
			if m == nil {
				t.Fatal("expected media, got nil")
			}
			if m.Artwork != "" {
				t.Fatalf("artwork = %q, want empty on poster-call failure", m.Artwork)
			}
		})
	}
}

// TestWikiPickPoster proves the pick rule: "poster" case-insensitive plus
// a photo extension wins; .jpg/.jpeg outrank .png outrank .webp; .svg
// and extensionless names are rejected even with "poster" in them.
func TestWikiPickPoster(t *testing.T) {
	cases := []struct {
		name   string
		titles []string
		want   string
	}{
		{"jpg wins over png", []string{"File:Film poster.png", "File:Film poster.jpg"}, "File:Film poster.jpg"},
		{"case insensitive", []string{"File:Film THEATRICAL POSTER.JPEG"}, "File:Film THEATRICAL POSTER.JPEG"},
		{"png accepted", []string{"File:Film poster.png"}, "File:Film poster.png"},
		{"webp last resort", []string{"File:Film poster.webp"}, "File:Film poster.webp"},
		{"svg rejected", []string{"File:Film poster.svg"}, ""},
		{"logo rejected", []string{"File:Film logo.png"}, ""},
		{"cast photo rejected", []string{"File:Film cast at premiere.jpg"}, ""},
		{"empty list", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := wikiPickPoster(tc.titles); got != tc.want {
				t.Fatalf("wikiPickPoster(%q) = %q, want %q", tc.titles, got, tc.want)
			}
		})
	}
}

// TestWikiYear proves year extraction: paren year in the page title wins,
// else the first plausible year in the extract, else empty (never invented).
func TestWikiYear(t *testing.T) {
	cases := []struct {
		name    string
		title   string
		extract string
		want    int
	}{
		{"paren film year", "Dune (2021 film)", "A science fiction film.", 2021},
		{"paren bare year", "Heat (1995)", "Crime film.", 1995},
		{"extract fallback", "Some Film", "Released in 2019 to acclaim.", 2019},
		{"no year stays empty", "Some Film", "A film of unknown vintage.", 0},
		{"far future not a year", "Future Film", "Set in the year 2200.", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := wikiYear(tc.title, tc.extract); got != tc.want {
				t.Fatalf("wikiYear(%q, %q) = %d, want %d", tc.title, tc.extract, got, tc.want)
			}
		})
	}
}

// TestWikipediaMovieOrder proves registration: for a movie query the
// default chain ranks iTunes first with Wikipedia immediately after, so
// Wikipedia is consulted only after iTunes definitively misses and never
// on the happy path.
func TestWikipediaMovieOrder(t *testing.T) {
	r := New(Options{Dir: t.TempDir(), Timeout: 5 * time.Second})
	got := []string{}
	for _, s := range r.orderedSources(Query{Kind: KindMovie, Title: "Dune"}) {
		got = append(got, s.Name())
	}
	if len(got) < 2 || got[0] != "itunes" || got[1] != "wikipedia" {
		t.Fatalf("movie order = %v, want [itunes wikipedia ...]", got)
	}

	// Series order is untouched: wikipedia self-gates there anyway.
	for _, s := range r.orderedSources(Query{Kind: KindSeries, Title: "Show", Season: 1, Episode: 1}) {
		if s.Name() == "wikipedia" {
			t.Logf("note: wikipedia present in series chain (self-gates pre-wire)")
		}
	}
}
