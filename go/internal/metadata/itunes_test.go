package metadata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestITunes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/search") {
			t.Errorf("path = %s, want /search", r.URL.Path)
		}
		if r.URL.Query().Get("media") == "movie" {
			t.Error("must NOT send media=movie (it returns 0 rows)")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resultCount":2,"results":[{"kind":"song","trackName":"Noise","releaseDate":"2020-01-01T08:00:00Z"},{"kind":"feature-movie","trackName":"Inception","releaseDate":"2010-07-16T07:00:00Z"}]}`))
	}))
	defer srv.Close()

	s := &itunesSource{base: srv.URL, client: srv.Client()}
	m, err := s.Find(context.Background(), Query{Kind: KindMovie, Title: "Inception", Year: 2010})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if m == nil {
		t.Fatal("expected media, got nil")
	}
	if m.Title != "Inception" || m.Year != 2010 || m.Kind != KindMovie {
		t.Fatalf("media = %+v", m)
	}
	if len(m.Episodes) != 0 {
		t.Fatalf("movie must have no episodes: %+v", m.Episodes)
	}

	r := New(Options{Dir: t.TempDir(), Sources: []Source{s}, Timeout: 5 * time.Second})
	res, err := r.LookupNow(context.Background(), Query{Kind: KindMovie, Title: "Inception", Year: 2010})
	if err != nil {
		t.Fatalf("LookupNow: %v", err)
	}
	if !res.Found || res.Series != "Inception" || res.Episode != "" {
		t.Fatalf("resolution = %+v", res)
	}
}
