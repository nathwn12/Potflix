package metadata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestJikan(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/anime", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"mal_id":21,"title":"One Piece","title_english":"One Piece","year":1999}]}`))
	})
	mux.HandleFunc("/anime/21/episodes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`{"data":[{"episode":2,"title":"Second"}],"pagination":{"last_visible_page":2}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"episode":1,"title":"First"}],"pagination":{"last_visible_page":2}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s := &jikanSource{base: srv.URL, client: srv.Client()}
	m, err := s.Find(context.Background(), Query{Kind: KindSeries, Title: "One Piece", Episode: 2, Absolute: true})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if m == nil {
		t.Fatal("expected media, got nil")
	}
	if m.Title != "One Piece" || m.Year != 1999 || m.SourceID != "21" {
		t.Fatalf("media = %+v", m)
	}
	if len(m.Episodes) != 2 {
		t.Fatalf("episodes = %+v", m.Episodes)
	}
	if m.Episodes[0] != (Episode{Number: 1, Title: "First"}) {
		t.Fatalf("ep0 = %+v", m.Episodes[0])
	}
	if m.Episodes[1] != (Episode{Number: 2, Title: "Second"}) {
		t.Fatalf("ep1 = %+v", m.Episodes[1])
	}

	r := New(Options{Dir: t.TempDir(), Sources: []Source{s}, Timeout: 5 * time.Second})
	res, err := r.LookupNow(context.Background(), Query{Kind: KindSeries, Title: "One Piece", Episode: 2, Absolute: true})
	if err != nil {
		t.Fatalf("LookupNow: %v", err)
	}
	if !res.Found || res.Episode != "Second" {
		t.Fatalf("resolution = %+v", res)
	}
}

// Number must come from the authoritative data[].episode field, never the
// slice index: gapped, offset, and unordered pages keep their own numbers.
func TestJikanUsesEpisodeField(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/anime", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"mal_id":7,"title":"Show","title_english":"Show","year":2020}]}`))
	})
	mux.HandleFunc("/anime/7/episodes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Deliberately unordered with a gap and an offset start: index order
		// is 51, 66, 52, so index+1 would yield 1, 2, 3.
		_, _ = w.Write([]byte(`{"data":[{"episode":66,"title":"Sixty Six"},{"episode":51,"title":"Fifty One"},{"episode":52,"title":"Fifty Two"}],"pagination":{"last_visible_page":1}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s := &jikanSource{base: srv.URL, client: srv.Client()}
	m, err := s.Find(context.Background(), Query{Kind: KindSeries, Title: "Show", Episode: 66, Absolute: true})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if m == nil {
		t.Fatal("expected media, got nil")
	}
	if len(m.Episodes) != 3 {
		t.Fatalf("episodes = %+v", m.Episodes)
	}
	// Deterministic ascending order by the episode field.
	want := []Episode{
		{Number: 51, Title: "Fifty One"},
		{Number: 52, Title: "Fifty Two"},
		{Number: 66, Title: "Sixty Six"},
	}
	for i, w := range want {
		if m.Episodes[i] != w {
			t.Fatalf("episodes[%d] = %+v, want %+v (full: %+v)", i, m.Episodes[i], w, m.Episodes)
		}
	}
}

// TestJikanEnrichment proves genres/duration/url populate Media plus the
// View-Anime button path.
func TestJikanEnrichment(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/anime", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"mal_id":21,"title":"One Piece","title_english":"One Piece","year":1999,"duration":"24 min per ep","url":"http://myanimelist.net/anime/21/One_Piece","genres":[{"name":"Action"},{"name":"Adventure"}]}]}`))
	})
	mux.HandleFunc("/anime/21/episodes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"episode":1,"title":"First"}],"pagination":{"last_visible_page":1}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s := &jikanSource{base: srv.URL, client: srv.Client()}
	m, err := s.Find(context.Background(), Query{Kind: KindSeries, Title: "One Piece", Episode: 1, Absolute: true})
	if err != nil || m == nil {
		t.Fatalf("Find = %+v, %v", m, err)
	}
	if m.Genres != "Action "+string(rune(0xb7))+" Adventure" {
		t.Fatalf("genres = %q", m.Genres)
	}
	if m.Length != "24 min per ep" {
		t.Fatalf("length = %q", m.Length)
	}
	if m.ShowURL != "https://myanimelist.net/anime/21/One_Piece" {
		t.Fatalf("showURL = %q", m.ShowURL)
	}

	r := New(Options{Dir: t.TempDir(), Sources: []Source{s}, Timeout: 5 * time.Second})
	res, err := r.LookupNow(context.Background(), Query{Kind: KindSeries, Title: "One Piece", Episode: 1, Absolute: true})
	if err != nil {
		t.Fatalf("LookupNow: %v", err)
	}
	if !res.Found {
		t.Fatalf("resolution = %+v, want found", res)
	}
	if len(res.Buttons) == 0 || res.Buttons[len(res.Buttons)-1].Label != "View Anime" {
		t.Fatalf("buttons = %+v, want trailing View Anime", res.Buttons)
	}
}
