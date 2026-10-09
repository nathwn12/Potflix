package metadata

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type tvmazeSource struct {
	base   string
	client *http.Client
}

func (s *tvmazeSource) Name() string { return "tvmaze" }

func (s *tvmazeSource) baseURL() string {
	if s.base != "" {
		return s.base
	}
	return "https://api.tvmaze.com"
}

func (s *tvmazeSource) httpClient() *http.Client {
	if s.client != nil {
		return s.client
	}
	return newHTTPClient(0)
}

type tvmazeImage struct {
	Medium   string `json:"medium"`
	Original string `json:"original"`
}

// tvmazeArt prefers the full-size original, falling back to medium.
func tvmazeArt(img *tvmazeImage) string {
	if img == nil {
		return ""
	}
	return firstNonEmpty(img.Original, img.Medium)
}

type tvmazeShow struct {
	ID             int          `json:"id"`
	Name           string       `json:"name"`
	Premiered      string       `json:"premiered"`
	Weight         int          `json:"weight"`
	Image          *tvmazeImage `json:"image"`
	Summary        string       `json:"summary"`
	Genres         []string     `json:"genres"`
	Runtime        *int         `json:"runtime"`
	AverageRuntime *int         `json:"averageRuntime"`
	URL            string       `json:"url"`
}

// tvmazeSearchResult is one entry of GET /search/shows?q=...; the show
// fields match the singlesearch shape.
type tvmazeSearchResult struct {
	Score float64    `json:"score"`
	Show  tvmazeShow `json:"show"`
}

type tvmazeSeason struct {
	ID     int `json:"id"`
	Number int `json:"number"`
}

type tvmazeEpisode struct {
	ID      int          `json:"id"`
	Name    string       `json:"name"`
	Season  int          `json:"season"`
	Number  *int         `json:"number"`
	Image   *tvmazeImage `json:"image"`
	Summary string       `json:"summary"`
	URL     string       `json:"url"`
	Runtime *int         `json:"runtime"`
}

// tvmazeShowLength prefers averageRuntime, else runtime.
func tvmazeShowLength(show tvmazeShow) string {
	if show.AverageRuntime != nil && *show.AverageRuntime > 0 {
		return runtimeLength(*show.AverageRuntime)
	}
	if show.Runtime != nil && *show.Runtime > 0 {
		return runtimeLength(*show.Runtime)
	}
	return ""
}

func tvmazeEpLength(runtime *int) string {
	if runtime == nil {
		return ""
	}
	return runtimeLength(*runtime)
}

// tvmazeMatchedURL returns the matched episode's page URL for q, "" when
// the table lacks it. Stored on Media.EpisodeURL so the resolver can build
// the View-Episode button even from cache.
func tvmazeMatchedURL(out []Episode, q Query) string {
	if ep, ok := matchEpisode(out, q); ok {
		return ep.URL
	}
	return ""
}

// Find resolves one season (Season>0) via /shows/<id>/seasons +
// /seasons/<seasonId>/episodes, or the whole show in air order for
// absolute/Season==0 queries via /shows/<id>/episodes.
func (s *tvmazeSource) Find(ctx context.Context, q Query) (*Media, error) {
	if q.Kind == KindMovie || q.Title == "" {
		return nil, nil
	}
	base := s.baseURL()
	client := s.httpClient()

	// Ranked search: /search/shows returns score order, which misranks
	// (singlesearch picked the "Re:Zero kara Hajimeru Break Time" spinoff
	// over the main show), so pickBestShow re-ranks by title similarity
	// with TVmaze-weight tie-breaks. A low-confidence best returns
	// not-found so the resolver falls through to AniList/Jikan.
	searchURL := base + "/search/shows?q=" + url.QueryEscape(strings.ToLower(q.Title))
	resp, err := fetchGet(ctx, client, searchURL)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		drainAndClose(resp)
		return nil, nil
	}
	var results []tvmazeSearchResult
	if err := decodeCapped(resp, &results); err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, nil
	}
	cands := make([]ShowCand, 0, len(results))
	shows := make([]tvmazeShow, 0, len(results))
	for _, r := range results {
		if r.Show.ID == 0 || r.Show.Name == "" {
			continue
		}
		cands = append(cands, ShowCand{Name: r.Show.Name, Weight: r.Show.Weight})
		shows = append(shows, r.Show)
	}
	if len(cands) == 0 {
		return nil, nil
	}
	winner, ok := pickBestShow(q.Title, cands)
	if !ok {
		return nil, nil
	}
	var show tvmazeShow
	found := false
	for i, c := range cands {
		if c == winner {
			show = shows[i]
			found = true
			break
		}
	}
	if !found {
		return nil, nil
	}
	year := dateYear(show.Premiered)
	genres := joinGenres(show.Genres)
	length := tvmazeShowLength(show)
	showURL := normalizeURL(show.URL)

	if q.Season > 0 && !q.Absolute {
		seasonsURL := fmt.Sprintf("%s/shows/%d/seasons", base, show.ID)
		resp, err := fetchGet(ctx, client, seasonsURL)
		if err != nil {
			return nil, err
		}
		var seasons []tvmazeSeason
		if err := decodeCapped(resp, &seasons); err != nil {
			return nil, err
		}
		seasonID := -1
		for _, sn := range seasons {
			if sn.Number == q.Season {
				seasonID = sn.ID
				break
			}
		}
		if seasonID < 0 {
			return nil, nil
		}
		epsURL := fmt.Sprintf("%s/seasons/%d/episodes", base, seasonID)
		resp, err = fetchGet(ctx, client, epsURL)
		if err != nil {
			return nil, err
		}
		var eps []tvmazeEpisode
		if err := decodeCapped(resp, &eps); err != nil {
			return nil, err
		}
		out := make([]Episode, 0, len(eps))
		for _, e := range eps {
			if e.Number == nil {
				continue
			}
			out = append(out, Episode{Season: e.Season, Number: *e.Number, Title: e.Name, Artwork: tvmazeArt(e.Image), Synopsis: capSynopsis(stripHTML(e.Summary)), URL: normalizeURL(e.URL), Length: tvmazeEpLength(e.Runtime)})
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Season != out[j].Season {
				return out[i].Season < out[j].Season
			}
			return out[i].Number < out[j].Number
		})
		return &Media{
			Source: "tvmaze", SourceID: strconv.Itoa(show.ID),
			Kind: KindSeries, Title: show.Name, Year: year,
			Artwork: tvmazeArt(show.Image), Synopsis: capSynopsis(stripHTML(show.Summary)),
			Genres: genres, Length: length, ShowURL: showURL, EpisodeURL: tvmazeMatchedURL(out, q),
			Episodes: out, FetchedAt: time.Now().UTC(),
		}, nil
	}

	epsURL := fmt.Sprintf("%s/shows/%d/episodes", base, show.ID)
	resp, err = fetchGet(ctx, client, epsURL)
	if err != nil {
		return nil, err
	}
	var eps []tvmazeEpisode
	if err := decodeCapped(resp, &eps); err != nil {
		return nil, err
	}
	sort.Slice(eps, func(i, j int) bool {
		if eps[i].Season != eps[j].Season {
			return eps[i].Season < eps[j].Season
		}
		ni, nj := 0, 0
		if eps[i].Number != nil {
			ni = *eps[i].Number
		}
		if eps[j].Number != nil {
			nj = *eps[j].Number
		}
		return ni < nj
	})
	out := make([]Episode, 0, len(eps))
	for _, e := range eps {
		if e.Number == nil {
			continue
		}
		out = append(out, Episode{Season: e.Season, Number: *e.Number, Title: e.Name, Artwork: tvmazeArt(e.Image), Synopsis: capSynopsis(stripHTML(e.Summary)), URL: normalizeURL(e.URL), Length: tvmazeEpLength(e.Runtime)})
	}
	if q.Absolute {
		// Absolute N maps to the Nth episode in (season, number) air order:
		// renumber so number-exact matching holds. Season is preserved for
		// display. An empty/short table simply lacks the Number, so the
		// resolver falls through to the next source.
		for i := range out {
			out[i].Number = i + 1
		}
	}
	return &Media{
		Source: "tvmaze", SourceID: strconv.Itoa(show.ID),
		Kind: KindSeries, Title: show.Name, Year: year,
		Artwork: tvmazeArt(show.Image), Synopsis: capSynopsis(stripHTML(show.Summary)),
		Genres: genres, Length: length, ShowURL: showURL, EpisodeURL: tvmazeMatchedURL(out, q),
		Episodes: out, FetchedAt: time.Now().UTC(),
	}, nil
}
