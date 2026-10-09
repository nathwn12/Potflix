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
