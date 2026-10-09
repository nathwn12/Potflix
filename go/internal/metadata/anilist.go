package metadata

import (
	"context"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type anilistSource struct {
	base   string
	client *http.Client
}

func (s *anilistSource) Name() string { return "anilist" }

func (s *anilistSource) baseURL() string {
	if s.base != "" {
		return s.base
	}
	return "https://graphql.anilist.co"
}

func (s *anilistSource) httpClient() *http.Client {
	if s.client != nil {
		return s.client
	}
	return newHTTPClient(0)
}

const anilistQuery = `query ($search:String){Media(search:$search,type:ANIME){id idMal title{romaji english} season seasonYear episodes genres duration siteUrl description coverImage{extraLarge large} bannerImage streamingEpisodes{title url site thumbnail}}}`

type anilistResp struct {
	Data *struct {
		Media *struct {
			ID    int `json:"id"`
			IdMal int `json:"idMal"`
			Title struct {
				Romaji  string `json:"romaji"`
				English string `json:"english"`
			} `json:"title"`
			SeasonYear  *int     `json:"seasonYear"`
			Genres      []string `json:"genres"`
			Duration    *int     `json:"duration"`
			SiteURL     string   `json:"siteUrl"`
			Description string   `json:"description"`
			CoverImage  *struct {
				ExtraLarge string `json:"extraLarge"`
				Large      string `json:"large"`
			} `json:"coverImage"`
			BannerImage string `json:"bannerImage"`
			Streaming   []struct {
				Title     string `json:"title"`
				URL       string `json:"url"`
				Site      string `json:"site"`
				Thumbnail string `json:"thumbnail"`
			} `json:"streamingEpisodes"`
		} `json:"Media"`
	} `json:"data"`
}

// anilistEpNumRe spots an embedded "Episode N" number, with an optional
// ".5" recap suffix and an optional " - title" tail.
var anilistEpNumRe = regexp.MustCompile(`(?i)^episode\s+(\d+)(?:\.\d+)?\s*(?:-\s*(.*))?$`)

// anilistLeadingNumRe spots a bare leading "N - title" number.
var anilistLeadingNumRe = regexp.MustCompile(`^(\d{1,4})(?:\.\d+)?\s*-\s*(.+)$`)

// stripAnilistEpTitle drops a leading "Episode N - " (or "N - ") prefix
// when present.
func stripAnilistEpTitle(s string) string {
	_, title := parseAnilistEpisode(s, 0)
	return title
}

// parseAnilistEpisode derives the episode number from the entry's title
// when it carries one ("Episode N - ...", "Episode N", "Episode N.5", or a
// leading "N - ...") and strips that prefix for the display title. It
// returns fallback (the running index) only when no number is present.
func parseAnilistEpisode(s string, fallback int) (int, string) {
	trimmed := strings.TrimSpace(s)
	if m := anilistEpNumRe.FindStringSubmatch(trimmed); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			rest := ""
			if len(m) > 2 {
				rest = strings.TrimSpace(m[2])
			}
			if rest == "" {
				return n, trimmed
			}
			return n, rest
		}
	}
	if m := anilistLeadingNumRe.FindStringSubmatch(trimmed); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return n, strings.TrimSpace(m[2])
		}
	}
	if fallback > 0 {
		return fallback, trimmed
	}
	return 0, trimmed
}

// Find queries the keyless AniList GraphQL endpoint and builds absolute
// episodes from streamingEpisodes. The episode Number is parsed from the
// entry's title ("Episode N - ...", "Episode N[.5]", or leading "N - ...");
// the positional index (i+1) is only the fallback when no number is
// present. Entries are sorted by Number for deterministic ordering.
func (s *anilistSource) Find(ctx context.Context, q Query) (*Media, error) {
	if q.Kind == KindMovie || q.Title == "" {
		return nil, nil
	}
	resp, err := fetchPostJSON(ctx, s.httpClient(), s.baseURL(), map[string]any{
		"query":     anilistQuery,
		"variables": map[string]any{"search": q.Title},
	})
	if err != nil {
		return nil, err
	}
	var ar anilistResp
	if err := decodeCapped(resp, &ar); err != nil {
		return nil, err
	}
	if ar.Data == nil || ar.Data.Media == nil || ar.Data.Media.ID == 0 {
		return nil, nil
	}
	m := ar.Data.Media
	title := strings.TrimSpace(m.Title.English)
	if title == "" {
		title = strings.TrimSpace(m.Title.Romaji)
	}
	if title == "" {
		return nil, nil
	}
	year := 0
	if m.SeasonYear != nil {
		year = *m.SeasonYear
	}
	genres := joinGenres(m.Genres)
	length := ""
	if m.Duration != nil {
		length = runtimeLength(*m.Duration)
	}
	showURL := ""
	if m.IdMal > 0 {
		showURL = "https://myanimelist.net/anime/" + strconv.Itoa(m.IdMal)
	} else {
		showURL = normalizeURL(m.SiteURL)
	}
	eps := make([]Episode, 0, len(m.Streaming))
	for i, se := range m.Streaming {
		num, title := parseAnilistEpisode(se.Title, i+1)
		eps = append(eps, Episode{Season: 0, Number: num, Title: title, Artwork: strings.TrimSpace(se.Thumbnail), URL: normalizeURL(se.URL)})
	}
	sort.SliceStable(eps, func(a, b int) bool { return eps[a].Number < eps[b].Number })
	artwork := m.BannerImage
	if m.CoverImage != nil {
		artwork = firstNonEmpty(m.CoverImage.ExtraLarge, m.CoverImage.Large, m.BannerImage)
	} else {
		artwork = strings.TrimSpace(m.BannerImage)
	}
	return &Media{
		Source: "anilist", SourceID: strconv.Itoa(m.ID),
		Kind: KindSeries, Title: title, Year: year,
		Artwork: artwork, Synopsis: capSynopsis(stripHTML(m.Description)),
		Genres: genres, Length: length, ShowURL: showURL,
		Episodes: eps, FetchedAt: time.Now().UTC(),
	}, nil
}
