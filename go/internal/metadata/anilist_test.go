package metadata

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAniList(t *testing.T) {
	var gotSearch string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		var payload struct {
			Query     string `json:"query"`
			Variables struct {
				Search string `json:"search"`
			} `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode payload: %v", err)
		}
		gotSearch = payload.Variables.Search
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"Media":{"id":21,"title":{"romaji":"One Piece","english":""},"season":"SPRING","seasonYear":1999,"episodes":2,"streamingEpisodes":[{"title":"Episode 1 - Enter: Naruto Uzumaki!","url":"http://x","site":"Crunchyroll"},{"title":"Episode 2 - Second","url":"http://y","site":"Crunchyroll"}]}}}`))
	}))
	defer srv.Close()

	s := &anilistSource{base: srv.URL, client: srv.Client()}
	m, err := s.Find(context.Background(), Query{Kind: KindSeries, Title: "One Piece", Episode: 1, Absolute: true})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if m == nil {
		t.Fatal("expected media, got nil")
	}
	if gotSearch != "One Piece" {
		t.Fatalf("search = %q", gotSearch)
	}
	// english is empty so canonical falls back to romaji.
	if m.Title != "One Piece" || m.Year != 1999 || m.SourceID != "21" {
		t.Fatalf("media = %+v", m)
	}
	if len(m.Episodes) != 2 {
		t.Fatalf("episodes = %+v", m.Episodes)
	}
	if m.Episodes[0] != (Episode{Number: 1, Title: "Enter: Naruto Uzumaki!", URL: "https://x"}) {
		t.Fatalf("ep0 = %+v", m.Episodes[0])
	}
	if m.Episodes[1] != (Episode{Number: 2, Title: "Second", URL: "https://y"}) {
		t.Fatalf("ep1 = %+v", m.Episodes[1])
	}

	r := New(Options{Dir: t.TempDir(), Sources: []Source{s}, Timeout: 5 * time.Second})
	res, err := r.LookupNow(context.Background(), Query{Kind: KindSeries, Title: "One Piece", Episode: 1, Absolute: true})
	if err != nil {
		t.Fatalf("LookupNow: %v", err)
	}
	if !res.Found || res.Episode != "Enter: Naruto Uzumaki!" {
		t.Fatalf("resolution = %+v", res)
	}
}

// anilistStub serves one canned streamingEpisodes list.
func anilistStub(t *testing.T, streaming string) *anilistSource {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"Media":{"id":1,"title":{"romaji":"Show","english":"Show"},"seasonYear":2020,"streamingEpisodes":[` + streaming + `]}}}`))
	}))
	t.Cleanup(srv.Close)
	return &anilistSource{base: srv.URL, client: srv.Client()}
}

func anilistNumbers(m *Media) []int {
	nums := make([]int, len(m.Episodes))
	for i, e := range m.Episodes {
		nums[i] = e.Number
	}
	return nums
}

// Numbers must come from the embedded "Episode N" title, not the slice
// index: offset, gapped, and reordered lists map by parsed number.
func TestAniListNumberParsing(t *testing.T) {
	cases := []struct {
		name      string
		streaming string
		wantNums  []int
		wantTitle map[int]string
	}{
		{
			name:      "episode prefix at index zero",
			streaming: `{"title":"Episode 1 - Pilot","url":"http://x","site":"C"}`,
			wantNums:  []int{1},
			wantTitle: map[int]string{1: "Pilot"},
		},
		{
			name: "offset start maps by parsed number",
			streaming: `{"title":"Episode 51 - Theatrical Malice","url":"http://x","site":"C"},` +
				`{"title":"Episode 52 - A Showdown of Fire and Ice","url":"http://x","site":"C"}`,
			wantNums:  []int{51, 52},
			wantTitle: map[int]string{51: "Theatrical Malice", 52: "A Showdown of Fire and Ice"},
		},
		{
			name: "gap is preserved not closed",
			streaming: `{"title":"Episode 1 - A","url":"http://x","site":"C"},` +
				`{"title":"Episode 3 - C","url":"http://x","site":"C"},` +
				`{"title":"Episode 4 - D","url":"http://x","site":"C"}`,
			wantNums:  []int{1, 3, 4},
			wantTitle: map[int]string{1: "A", 3: "C", 4: "D"},
		},
		{
			name: "descending live shape sorts ascending",
			streaming: `{"title":"Episode 66 - The Result of the Battle for Priestella","url":"http://x","site":"C"},` +
				`{"title":"Episode 65 - A Hideous Dinner Party","url":"http://x","site":"C"},` +
				`{"title":"Episode 51 - Theatrical Malice","url":"http://x","site":"C"}`,
			wantNums:  []int{51, 65, 66},
			wantTitle: map[int]string{51: "Theatrical Malice", 65: "A Hideous Dinner Party", 66: "The Result of the Battle for Priestella"},
		},
		{
			name: "leading number dash prefix",
			streaming: `{"title":"01 - Pilot","url":"http://x","site":"C"},` +
				`{"title":"02 - Second","url":"http://x","site":"C"}`,
			wantNums:  []int{1, 2},
			wantTitle: map[int]string{1: "Pilot", 2: "Second"},
		},
		{
			name:      "recap dot-five keeps base number",
			streaming: `{"title":"Episode 2.5 - Recap","url":"http://x","site":"C"}`,
			wantNums:  []int{2},
			wantTitle: map[int]string{2: "Recap"},
		},
		{
			name: "fallback keeps running index",
			streaming: `{"title":"Episode 10 - Tenth","url":"http://x","site":"C"},` +
				`{"title":"Behind the Scenes","url":"http://x","site":"C"}`,
			wantNums:  []int{2, 10},
			wantTitle: map[int]string{10: "Tenth", 2: "Behind the Scenes"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := anilistStub(t, tc.streaming)
			m, err := s.Find(context.Background(), Query{Kind: KindSeries, Title: "Show", Absolute: true})
			if err != nil {
				t.Fatalf("Find: %v", err)
			}
			if m == nil {
				t.Fatal("expected media, got nil")
			}
			got := anilistNumbers(m)
			if len(got) != len(tc.wantNums) {
				t.Fatalf("numbers = %v, want %v", got, tc.wantNums)
			}
			for i, n := range tc.wantNums {
				if got[i] != n {
					t.Fatalf("numbers = %v, want %v", got, tc.wantNums)
				}
			}
			for n, title := range tc.wantTitle {
				found := false
				for _, e := range m.Episodes {
					if e.Number == n && e.Title == title {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("episodes = %+v, want number %d title %q", m.Episodes, n, title)
				}
			}
		})
	}
}

// TestAniListShowURL proves idMal present -> myanimelist.net ShowURL (with
// genres/duration/streaming URL), and absent -> AniList siteUrl fallback.
func TestAniListShowURL(t *testing.T) {
	mk := func(body string) *anilistSource {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return &anilistSource{base: srv.URL, client: srv.Client()}
	}

	withMal := mk(`{"data":{"Media":{"id":21355,"idMal":12189,"title":{"romaji":"Re Zero","english":"Re:ZERO"},"seasonYear":2016,"genres":["Action","Adventure"],"duration":24,"siteUrl":"https://anilist.co/anime/21355","streamingEpisodes":[{"title":"Episode 1 - Pilot","url":"http://example.com/ep1","site":"Crunchyroll","thumbnail":""}]}}}`)
	m, err := withMal.Find(context.Background(), Query{Kind: KindSeries, Title: "Re Zero", Episode: 1, Absolute: true})
	if err != nil || m == nil {
		t.Fatalf("Find idMal = %+v, %v", m, err)
	}
	if m.ShowURL != "https://myanimelist.net/anime/12189" {
		t.Fatalf("showURL = %q, want MAL idMal URL", m.ShowURL)
	}
	if m.Genres != "Action "+string(rune(0xb7))+" Adventure" {
		t.Fatalf("genres = %q", m.Genres)
	}
	if m.Length != "24 min" {
		t.Fatalf("length = %q, want 24 min", m.Length)
	}
	if len(m.Episodes) != 1 || m.Episodes[0].URL != "https://example.com/ep1" {
		t.Fatalf("episode URL = %+v, want https normalized", m.Episodes)
	}

	withoutMal := mk(`{"data":{"Media":{"id":1,"title":{"romaji":"Show","english":"Show"},"seasonYear":2020,"genres":["Drama"],"siteUrl":"http://anilist.co/anime/1","streamingEpisodes":[]}}}`)
	m2, err := withoutMal.Find(context.Background(), Query{Kind: KindSeries, Title: "Show", Episode: 1, Absolute: true})
	if err != nil || m2 == nil {
		t.Fatalf("Find no-idMal = %+v, %v", m2, err)
	}
	if m2.ShowURL != "https://anilist.co/anime/1" {
		t.Fatalf("fallback showURL = %q, want https siteUrl", m2.ShowURL)
	}
}
