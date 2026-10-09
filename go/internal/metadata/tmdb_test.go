package metadata

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	tmdbTestKey = "REDACTED_TEST_KEY_2"
	tmdbLeakKey = "REDACTED_TEST_KEY"
)

const (
	tmdbTVSearchJSON = `{"page":1,"results":[{"id":1396,"name":"Breaking Bad","first_air_date":"2008-01-20","vote_count":12000,"poster_path":"/ggFHVNu6YYI5L9pCfOacjizRGt.jpg","overview":"A teacher."}],"total_results":1}`
	tmdbTVDetailJSON = `{"id":1396,"name":"Breaking Bad","first_air_date":"2008-01-20","poster_path":"/ggFHVNu6YYI5L9pCfOacjizRGt.jpg","overview":"<p>A high-school chemistry teacher turned meth cook.</p>","genres":[{"id":18,"name":"Drama"},{"id":80,"name":"Crime"}],"episode_run_time":[45],"seasons":[{"season_number":0},{"season_number":1},{"season_number":2}]}`
	tmdbSeason1JSON  = `{"season_number":1,"episodes":[{"episode_number":1,"season_number":1,"name":"Pilot","overview":"Walter White learns he has cancer.","still_path":"/still1.jpg"},{"episode_number":2,"season_number":1,"name":"Cat's in the Bag...","overview":"Walt and Jesse deal with a body.","still_path":"/still2.jpg"}]}`
	tmdbMovieSrJSON  = `{"page":1,"results":[{"id":27205,"title":"Inception","release_date":"2010-07-16","vote_count":30000}],"total_results":1}`
	tmdbMovieDetJSON = `{"id":27205,"title":"Inception","release_date":"2010-07-16","poster_path":"/9gk7adHYeDvHkCSEqAvQNLV5Uge.jpg","overview":"A thief who steals corporate secrets.","genres":[{"id":28,"name":"Action"},{"id":878,"name":"Sci-Fi"}],"runtime":148}`
)

// tmdbKeyedMux serves the happy-path fixtures and enforces the api_key,
// returning 401 for anything else (like the real API).
func tmdbKeyedMux(t *testing.T, key string) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	guard := func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("api_key") != key {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"status_code":7,"status_message":"Invalid API key"}`))
			return false
		}
		return true
	}
	write := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
	mux.HandleFunc("/3/search/tv", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		write(w, tmdbTVSearchJSON)
	})
	mux.HandleFunc("/3/tv/1396", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		write(w, tmdbTVDetailJSON)
	})
	mux.HandleFunc("/3/tv/1396/season/1", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		write(w, tmdbSeason1JSON)
	})
	mux.HandleFunc("/3/search/movie", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		write(w, tmdbMovieSrJSON)
	})
	mux.HandleFunc("/3/movie/27205", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		write(w, tmdbMovieDetJSON)
	})
	return mux
}

func sourceNames(ss []Source) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.Name()
	}
	return out
}

func equalNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Keyed-first: with a key + a fake TMDB server, a series query is answered
// by TMDB and the keyless sources are NOT called.
func TestTMDBKeyedFirst(t *testing.T) {
	srv := httptest.NewServer(tmdbKeyedMux(t, tmdbTestKey))
	defer srv.Close()

	tmdb := &tmdbSource{base: srv.URL, key: tmdbTestKey, client: srv.Client()}
	keyless := &fakeSource{name: "fake-keyless", media: &Media{
		Source: "fake-keyless", SourceID: "9",
		Kind: KindSeries, Title: "Breaking Bad", Year: 2008,
		Episodes:  []Episode{{Season: 1, Number: 1, Title: "Wrong Title"}},
		FetchedAt: time.Now().UTC(),
	}}
	r := New(Options{Dir: t.TempDir(), Sources: []Source{tmdb, keyless}, Timeout: 5 * time.Second})
	res, err := r.LookupNow(context.Background(), Query{Kind: KindSeries, Title: "Breaking Bad", Season: 1, Episode: 1})
	if err != nil {
		t.Fatalf("LookupNow: %v", err)
	}
	if !res.Found || res.Episode != "Pilot" {
		t.Fatalf("resolution = %+v, want Pilot found", res)
	}
	if n := keyless.calls.Load(); n != 0 {
		t.Fatalf("keyless calls = %d, want 0 (TMDB answered first)", n)
	}
}

// Fallthrough: TMDB 401/500/empty all let the keyless source answer.
func TestTMDBFallthrough(t *testing.T) {
	fakeMedia := func() *Media {
		return &Media{
			Source: "fake-keyless", SourceID: "9",
			Kind: KindSeries, Title: "Breaking Bad", Year: 2008,
			Episodes:  []Episode{{Season: 1, Number: 1, Title: "Pilot"}},
			FetchedAt: time.Now().UTC(),
		}
	}
	q := Query{Kind: KindSeries, Title: "Breaking Bad", Season: 1, Episode: 1}
	cases := map[string]http.HandlerFunc{
		"401": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"status_code":7,"status_message":"Invalid API key"}`))
		},
		"500": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`boom`))
		},
		"empty": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"page":1,"results":[],"total_results":0}`))
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.Handle("/3/search/tv", h)
			srv := httptest.NewServer(mux)
			defer srv.Close()
			tmdb := &tmdbSource{base: srv.URL, key: tmdbTestKey, client: srv.Client()}
			keyless := &fakeSource{name: "fake-keyless", media: fakeMedia()}
			r := New(Options{Dir: t.TempDir(), Sources: []Source{tmdb, keyless}, Timeout: 5 * time.Second})
			res, err := r.LookupNow(context.Background(), q)
			if err != nil {
				t.Fatalf("LookupNow: %v", err)
			}
			if !res.Found || res.Episode != "Pilot" {
				t.Fatalf("resolution = %+v, want keyless Pilot found", res)
			}
			if n := keyless.calls.Load(); n != 1 {
				t.Fatalf("keyless calls = %d, want 1 (fallthrough)", n)
			}
		})
	}
}

// No key (nil provider, blank, or whitespace) -> the source list is
// byte-identical to today's keyless order; a real key prepends tmdb.
func TestTMDBNoKey(t *testing.T) {
	want := []string{"tvmaze", "anilist", "jikan", "itunes"}
	if got := sourceNames(DefaultSources()); !equalNames(got, want) {
		t.Fatalf("DefaultSources() = %v, want %v", got, want)
	}
	if got := sourceNames(DefaultSources(nil)); !equalNames(got, want) {
		t.Fatalf("DefaultSources(nil) = %v, want %v", got, want)
	}
	for _, k := range []string{"", "   ", "\t\n "} {
		if got := sourceNames(DefaultSources(func(string) string { return k })); !equalNames(got, want) {
			t.Fatalf("DefaultSources(blank) = %v, want %v", got, want)
		}
	}
	if got := sourceNames(DefaultSources(func(string) string { return "k" })); !equalNames(got, append([]string{"tmdb"}, want...)) {
		t.Fatalf("DefaultSources(key) = %v, want tmdb first", got)
	}

	// The resolver applies the same rule when Sources is nil.
	r := New(Options{Dir: t.TempDir(), Timeout: 5 * time.Second})
	if got := sourceNames(r.orderedSources(Query{Kind: KindSeries, Title: "Show", Season: 1, Episode: 1})); !equalNames(got, want) {
		t.Fatalf("keyless orderedSources = %v, want %v", got, want)
	}
	rk := New(Options{Dir: t.TempDir(), Timeout: 5 * time.Second, KeyProvider: func(string) string { return "k" }})
	if got := sourceNames(rk.orderedSources(Query{Kind: KindSeries, Title: "Show", Season: 1, Episode: 1})); !equalNames(got, append([]string{"tmdb"}, want...)) {
		t.Fatalf("keyed orderedSources = %v, want tmdb first", got)
	}
	if got := sourceNames(rk.orderedSources(Query{Kind: KindMovie, Title: "Film"})); len(got) == 0 || got[0] != "tmdb" {
		t.Fatalf("keyed movie orderedSources = %v, want tmdb first", got)
	}
}

// Mapping: poster, genres, runtime, year, overview, episode title, and the
// TMDB page button URL for both series and movie.
func TestTMDBMapping(t *testing.T) {
	srv := httptest.NewServer(tmdbKeyedMux(t, tmdbTestKey))
	defer srv.Close()
	s := &tmdbSource{base: srv.URL, key: tmdbTestKey, client: srv.Client()}
	ctx := context.Background()

	m, err := s.Find(ctx, Query{Kind: KindSeries, Title: "Breaking Bad", Season: 1, Episode: 1})
	if err != nil {
		t.Fatalf("series Find: %v", err)
	}
	if m == nil {
		t.Fatal("series Find = nil, want media")
	}
	if m.Title != "Breaking Bad" || m.Source != "tmdb" || m.SourceID != "1396" || m.Kind != KindSeries {
		t.Fatalf("series identity = %+v", m)
	}
	if m.Year != 2008 {
		t.Fatalf("series year = %d, want 2008", m.Year)
	}
	if m.Artwork != "https://image.tmdb.org/t/p/w500/ggFHVNu6YYI5L9pCfOacjizRGt.jpg" {
		t.Fatalf("series artwork = %q", m.Artwork)
	}
	if m.Genres != "Drama \u00b7 Crime" {
		t.Fatalf("series genres = %q", m.Genres)
	}
	if m.Length != "45 min" {
		t.Fatalf("series length = %q", m.Length)
	}
	if m.Synopsis != "A high-school chemistry teacher turned meth cook." {
		t.Fatalf("series synopsis = %q", m.Synopsis)
	}
	if m.ShowURL != "https://www.themoviedb.org/tv/1396" {
		t.Fatalf("series show URL = %q", m.ShowURL)
	}
	if len(m.Episodes) != 2 || m.Episodes[0].Title != "Pilot" || m.Episodes[0].Season != 1 || m.Episodes[0].Number != 1 {
		t.Fatalf("series episodes = %+v", m.Episodes)
	}
	// still_path is never artwork: episode art stays empty, the series
	// poster wins at resolution.
	if m.Episodes[0].Artwork != "" {
		t.Fatalf("episode artwork = %q, want empty (poster wins)", m.Episodes[0].Artwork)
	}

	r := New(Options{Dir: t.TempDir(), Sources: []Source{s}, Timeout: 5 * time.Second})
	res, err := r.LookupNow(ctx, Query{Kind: KindSeries, Title: "Breaking Bad", Season: 1, Episode: 1})
	if err != nil || !res.Found {
		t.Fatalf("series LookupNow = %+v, %v", res, err)
	}
	if res.Series != "Breaking Bad" || res.Episode != "Pilot" || res.Year != 2008 || res.IsMovie {
		t.Fatalf("series resolution = %+v", res)
	}
	if res.Artwork != "https://image.tmdb.org/t/p/w500/ggFHVNu6YYI5L9pCfOacjizRGt.jpg" {
		t.Fatalf("series resolution artwork = %q", res.Artwork)
	}
	if len(res.Buttons) != 1 || res.Buttons[0].Label != "View Show" || res.Buttons[0].URL != "https://www.themoviedb.org/tv/1396" {
		t.Fatalf("series buttons = %+v, want single View Show", res.Buttons)
	}

	mv, err := s.Find(ctx, Query{Kind: KindMovie, Title: "Inception", Year: 2010})
	if err != nil {
		t.Fatalf("movie Find: %v", err)
	}
	if mv == nil {
		t.Fatal("movie Find = nil, want media")
	}
	if mv.Title != "Inception" || mv.Kind != KindMovie || mv.Year != 2010 || mv.SourceID != "27205" {
		t.Fatalf("movie identity = %+v", mv)
	}
	if mv.Artwork != "https://image.tmdb.org/t/p/w500/9gk7adHYeDvHkCSEqAvQNLV5Uge.jpg" {
		t.Fatalf("movie artwork = %q", mv.Artwork)
	}
	if mv.Genres != "Action \u00b7 Sci-Fi" {
		t.Fatalf("movie genres = %q", mv.Genres)
	}
	if mv.Length != "148 min" {
		t.Fatalf("movie length = %q", mv.Length)
	}
	if mv.Synopsis != "A thief who steals corporate secrets." {
		t.Fatalf("movie synopsis = %q", mv.Synopsis)
	}
	if mv.MovieURL != "https://www.themoviedb.org/movie/27205" {
		t.Fatalf("movie URL = %q", mv.MovieURL)
	}
	res2, err := r.LookupNow(ctx, Query{Kind: KindMovie, Title: "Inception", Year: 2010})
	if err != nil || !res2.Found || !res2.IsMovie {
		t.Fatalf("movie LookupNow = %+v, %v", res2, err)
	}
	if len(res2.Buttons) != 1 || res2.Buttons[0].Label != "View Movie" || res2.Buttons[0].URL != "https://www.themoviedb.org/movie/27205" {
		t.Fatalf("movie buttons = %+v, want single View Movie", res2.Buttons)
	}
}

// No-leak: a failing TMDB request with a known key produces error and log
// strings that do NOT contain the key - for HTTP failures and for transport
// failures (whose url.Error would otherwise carry the full query).
func TestTMDBNoLeak(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/3/search/tv", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`boom`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	q := Query{Kind: KindSeries, Title: "Breaking Bad", Season: 1, Episode: 1}
	s := &tmdbSource{base: srv.URL, key: tmdbLeakKey, client: srv.Client()}
	if _, err := s.Find(context.Background(), q); err == nil {
		t.Fatal("HTTP-500 Find = nil error, want failure")
	} else if strings.Contains(err.Error(), tmdbLeakKey) {
		t.Fatalf("HTTP-500 error leaks key: %q", err)
	}

	var logs []string
	r := New(Options{
		Dir: t.TempDir(), Sources: []Source{s}, Timeout: 5 * time.Second,
		Logf: func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
	})
	if _, err := r.LookupNow(context.Background(), q); err != nil {
		t.Fatalf("LookupNow: %v", err)
	}
	for _, l := range logs {
		if strings.Contains(l, tmdbLeakKey) {
			t.Fatalf("resolver log leaks key: %q", l)
		}
	}

	// Transport failure: the server is gone, so client.Do reports a
	// url.Error embedding the request URL (api_key included).
	dead := httptest.NewServer(mux)
	deadURL := dead.URL
	dead.Close()
	d := &tmdbSource{key: tmdbLeakKey, client: dead.Client()}
	d.base = deadURL
	if _, err := d.Find(context.Background(), q); err == nil {
		t.Fatal("transport Find = nil error, want failure")
	} else if strings.Contains(err.Error(), tmdbLeakKey) {
		t.Fatalf("transport error leaks key: %q", err)
	}
}

// Confidence: a same-name hit from the wrong year, a low-similarity title,
// and a missing season are all misses - never positional answers.
func TestTMDBConfidence(t *testing.T) {
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
	mux.HandleFunc("/3/search/tv", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"results":[{"id":999,"name":"Breaking Bad","first_air_date":"2008-01-20","vote_count":5}]}`)
	})
	mux.HandleFunc("/3/tv/999", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"id":999,"name":"Breaking Bad","first_air_date":"2008-01-20","genres":[],"seasons":[{"season_number":1}]}`)
	})
	mux.HandleFunc("/3/tv/999/season/1", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"season_number":1,"episodes":[{"episode_number":1,"season_number":1,"name":"Pilot","overview":""}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	s := &tmdbSource{base: srv.URL, key: tmdbTestKey, client: srv.Client()}
	ctx := context.Background()

	// Wrong year: the only hit is 2008, the query wants 2020.
	if m, err := s.Find(ctx, Query{Kind: KindSeries, Title: "Breaking Bad", Year: 2020, Season: 1, Episode: 1}); err != nil || m != nil {
		t.Fatalf("year-mismatch = %+v, %v; want nil, nil", m, err)
	}

	// Low title confidence: no positional acceptance of a stranger.
	if m, err := s.Find(ctx, Query{Kind: KindSeries, Title: "Zebra Quilts Unite", Season: 1, Episode: 1}); err != nil || m != nil {
		t.Fatalf("low-confidence = %+v, %v; want nil, nil", m, err)
	}

	// Missing season: only season 1 exists, season 5 must miss.
	if m, err := s.Find(ctx, Query{Kind: KindSeries, Title: "Breaking Bad", Season: 5, Episode: 1}); err != nil || m != nil {
		t.Fatalf("missing-season = %+v, %v; want nil, nil", m, err)
	}
}
