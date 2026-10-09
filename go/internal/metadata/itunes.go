package metadata

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type itunesSource struct {
	base   string
	client *http.Client
}

func (s *itunesSource) Name() string { return "itunes" }

func (s *itunesSource) baseURL() string {
	if s.base != "" {
		return s.base
	}
	return "https://itunes.apple.com"
}

func (s *itunesSource) httpClient() *http.Client {
	if s.client != nil {
		return s.client
	}
	return newHTTPClient(0)
}

type itunesSearchResp struct {
	Results []struct {
		Kind             string `json:"kind"`
		TrackName        string `json:"trackName"`
		ReleaseDate      string `json:"releaseDate"`
		ArtworkURL100    string `json:"artworkUrl100"`
		ShortDescription string `json:"shortDescription"`
		LongDescription  string `json:"longDescription"`
		PrimaryGenreName string `json:"primaryGenreName"`
		TrackTimeMillis  int64  `json:"trackTimeMillis"`
		TrackViewURL     string `json:"trackViewUrl"`
		PreviewURL       string `json:"previewUrl"`
	} `json:"results"`
}

// itunesArt scales the 100x100 store thumbnail up to 600x600.
func itunesArt(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	return strings.Replace(raw, "100x100", "600x600", 1)
}

// Find is movie-only. It deliberately omits media=movie (which returns 0
// rows) and takes the first results[] entry with kind=="feature-movie".
func (s *itunesSource) Find(ctx context.Context, q Query) (*Media, error) {
	if q.Kind == KindSeries || q.Title == "" {
		return nil, nil
	}
	searchURL := s.baseURL() + "/search?term=" + url.QueryEscape(q.Title) + "&limit=5"
	resp, err := fetchGet(ctx, s.httpClient(), searchURL)
	if err != nil {
		return nil, err
	}
	var sr itunesSearchResp
	if err := decodeCapped(resp, &sr); err != nil {
		return nil, err
	}
	for _, r := range sr.Results {
		if r.Kind != "feature-movie" {
			continue
		}
		title := strings.TrimSpace(r.TrackName)
		if title == "" {
			continue
		}
		return &Media{
			Source: "itunes", SourceID: title,
			Kind: KindMovie, Title: title, Year: dateYear(r.ReleaseDate),
			Artwork:    itunesArt(r.ArtworkURL100),
			Synopsis:   capSynopsis(stripHTML(firstNonEmpty(r.LongDescription, r.ShortDescription))),
			Genres:     strings.TrimSpace(r.PrimaryGenreName),
			Length:     millisLength(r.TrackTimeMillis),
			MovieURL:   normalizeURL(r.TrackViewURL),
			TrailerURL: normalizeURL(r.PreviewURL),
			FetchedAt:  time.Now().UTC(),
		}, nil
	}
	return nil, nil
}
