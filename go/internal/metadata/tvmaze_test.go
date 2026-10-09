package metadata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTVMazeSeason(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/shows", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "breaking bad" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"score":1.0,"show":{"id":169,"name":"Breaking Bad","premiered":"2008-01-20","weight":99}}]`))
	})
	mux.HandleFunc("/shows/169/seasons", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":753,"number":1},{"id":754,"number":2}]`))
	})
	mux.HandleFunc("/seasons/753/episodes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":12192,"name":"Pilot","season":1,"number":1},{"id":12193,"name":"Cat's in the Bag...","season":1,"number":2}]`))
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
	if m.Title != "Breaking Bad" || m.Year != 2008 || m.SourceID != "169" {
		t.Fatalf("media = %+v", m)
	}
	if len(m.Episodes) != 2 || m.Episodes[0] != (Episode{Season: 1, Number: 1, Title: "Pilot"}) {
		t.Fatalf("episodes = %+v", m.Episodes)
	}

	if _, err := s.Find(context.Background(), Query{Kind: KindMovie, Title: "Inception"}); err != nil {
		t.Fatalf("movie Find: %v", err)
	}

	r := New(Options{Dir: t.TempDir(), Sources: []Source{s}, Timeout: 5 * time.Second})
	res, err := r.LookupNow(context.Background(), Query{Kind: KindSeries, Title: "Breaking Bad", Season: 1, Episode: 1})
	if err != nil {
		t.Fatalf("LookupNow: %v", err)
	}
	if !res.Found || res.Series != "Breaking Bad" || res.Episode != "Pilot" {
		t.Fatalf("resolution = %+v", res)
	}
}

func TestTVMazeAbsoluteAndNotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/shows", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "Missing") || strings.Contains(r.URL.RawQuery, "missing") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"score":1.0,"show":{"id":169,"name":"Show","premiered":"2008-01-20","weight":99}}]`))
	})
	mux.HandleFunc("/shows/169/episodes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":1,"name":"S1E1","season":1,"number":1},{"id":2,"name":"S1E2","season":1,"number":2},{"id":3,"name":"S2E1","season":2,"number":1}]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s := &tvmazeSource{base: srv.URL, client: srv.Client()}
	m, err := s.Find(context.Background(), Query{Kind: KindSeries, Title: "Show", Episode: 3, Absolute: true})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if m == nil || len(m.Episodes) != 3 {
		t.Fatalf("absolute media = %+v", m)
	}
	r := New(Options{Dir: t.TempDir(), Sources: []Source{s}, Timeout: 5 * time.Second})
	res, err := r.LookupNow(context.Background(), Query{Kind: KindSeries, Title: "Show", Episode: 3, Absolute: true})
	if err != nil {
		t.Fatalf("LookupNow: %v", err)
	}
	if !res.Found || res.Episode != "S2E1" {
		t.Fatalf("absolute resolution = %+v, want S2E1", res)
	}

	miss, err := s.Find(context.Background(), Query{Kind: KindSeries, Title: "Missing Show"})
	if err != nil || miss != nil {
		t.Fatalf("not-found = %+v, %v; want nil, nil", miss, err)
	}
}

// TestTVMazeSearchRankingReZero replays the live misranking: /search/shows
// lists the "Break Time" spinoff (weight 80) above the main show (weight
// 98). Find must still resolve to the main show (id 14459).
func TestTVMazeSearchRankingReZero(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/shows", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"score":0.6979369,"show":{"id":82431,"name":"Re:Zero kara Hajimeru Break Time","premiered":"2016-04-08","weight":80}},{"score":0.669693,"show":{"id":14459,"name":"Re:ZERO -Starting Life in Another World-","premiered":"2016-04-04","weight":98}}]`))
	})
	mux.HandleFunc("/shows/14459/seasons", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":14459,"number":1}]`))
	})
	mux.HandleFunc("/seasons/14459/episodes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":1,"name":"The End of the Beginning and the Beginning of the End - 1","season":1,"number":1}]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s := &tvmazeSource{base: srv.URL, client: srv.Client()}
	m, err := s.Find(context.Background(), Query{Kind: KindSeries, Title: "Re Zero", Season: 1, Episode: 1})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if m == nil {
		t.Fatal("expected media, got nil (low-confidence fallthrough misfired)")
	}
	if m.SourceID != "14459" || m.Title != "Re:ZERO -Starting Life in Another World-" {
		t.Fatalf("media = %+v, want the main show id 14459", m)
	}
}

// TestTVMazeLowConfidenceFallsThrough proves a best similarity below ~0.5
// returns not-found so the resolver tries the next source.
func TestTVMazeLowConfidenceFallsThrough(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/shows", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"score":0.9,"show":{"id":14459,"name":"Re:ZERO -Starting Life in Another World-","premiered":"2016-04-04","weight":98}}]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s := &tvmazeSource{base: srv.URL, client: srv.Client()}
	m, err := s.Find(context.Background(), Query{Kind: KindSeries, Title: "Breaking Bad", Season: 1, Episode: 1})
	if err != nil || m != nil {
		t.Fatalf("low-confidence = %+v, %v; want nil, nil", m, err)
	}
}

// TestTVMazeEnrichment proves the fixture yields genres/length/urls and the
// resolver carries them plus View-Episode/View-Show buttons.
func TestTVMazeEnrichment(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/shows", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"score":1.0,"show":{"id":169,"name":"Breaking Bad","premiered":"2008-01-20","weight":99,"genres":["Drama","Crime"],"averageRuntime":45,"runtime":60,"url":"http://www.tvmaze.com/shows/169/breaking-bad"}}]`))
	})
	mux.HandleFunc("/shows/169/seasons", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":753,"number":1}]`))
	})
	mux.HandleFunc("/seasons/753/episodes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":12192,"name":"Pilot","season":1,"number":1,"url":"http://www.tvmaze.com/episodes/12192/breaking-bad-1x01-pilot","runtime":45}]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s := &tvmazeSource{base: srv.URL, client: srv.Client()}
	q := Query{Kind: KindSeries, Title: "Breaking Bad", Season: 1, Episode: 1}
	m, err := s.Find(context.Background(), q)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if m == nil {
		t.Fatal("expected media, got nil")
	}
	if m.Genres != "Drama "+string(rune(0xb7))+" Crime" {
		t.Fatalf("genres = %q, want %q", m.Genres, "Drama "+string(rune(0xb7))+" Crime")
	}
	if m.Length != "45 min" {
		t.Fatalf("length = %q, want 45 min", m.Length)
	}
	if m.ShowURL != "https://www.tvmaze.com/shows/169/breaking-bad" {
		t.Fatalf("showURL = %q", m.ShowURL)
	}
	if len(m.Episodes) != 1 || m.Episodes[0].URL != "https://www.tvmaze.com/episodes/12192/breaking-bad-1x01-pilot" {
		t.Fatalf("episode URL = %+v", m.Episodes)
	}
	if m.Episodes[0].Length != "45 min" {
		t.Fatalf("episode length = %q", m.Episodes[0].Length)
	}
	if m.EpisodeURL != "https://www.tvmaze.com/episodes/12192/breaking-bad-1x01-pilot" {
		t.Fatalf("media episodeURL = %q", m.EpisodeURL)
	}

	r := New(Options{Dir: t.TempDir(), Sources: []Source{s}, Timeout: 5 * time.Second})
	res, err := r.LookupNow(context.Background(), q)
	if err != nil {
		t.Fatalf("LookupNow: %v", err)
	}
	if !res.Found || res.Genres != "Drama "+string(rune(0xb7))+" Crime" || res.Length != "45 min" || res.Year != 2008 || res.IsMovie {
		t.Fatalf("resolution = %+v, want genres/length/year", res)
	}
	if len(res.Buttons) != 2 || res.Buttons[0].Label != "View Episode" || res.Buttons[1].Label != "View Show" {
		t.Fatalf("buttons = %+v, want [View Episode View Show]", res.Buttons)
	}
	for _, b := range res.Buttons {
		if !strings.HasPrefix(b.URL, "https://") {
			t.Fatalf("button not https: %+v", b)
		}
	}
}
