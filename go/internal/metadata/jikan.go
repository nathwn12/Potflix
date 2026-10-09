package metadata

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type jikanSource struct {
	base   string
	client *http.Client
}

func (s *jikanSource) Name() string { return "jikan" }

func (s *jikanSource) baseURL() string {
	if s.base != "" {
		return s.base
	}
	return "https://api.jikan.moe/v4"
}

func (s *jikanSource) httpClient() *http.Client {
	if s.client != nil {
		return s.client
	}
	return newHTTPClient(0)
}

// maxJikanPages bounds the episode pagination loop.
const maxJikanPages = 50

type jikanSearchResp struct {
	Data []struct {
		MalID        int     `json:"mal_id"`
		Title        string  `json:"title"`
		TitleEnglish *string `json:"title_english"`
		Synopsis     string  `json:"synopsis"`
		Images       *struct {
			JPG struct {
				ImageURL      string `json:"image_url"`
				LargeImageURL string `json:"large_image_url"`
			} `json:"jpg"`
		} `json:"images"`
		Year  *int `json:"year"`
		Aired *struct {
			From *string `json:"from"`
		} `json:"aired"`
	} `json:"data"`
}

type jikanEpsResp struct {
	Data []struct {
		Episode *int   `json:"episode"`
		Title   string `json:"title"`
	} `json:"data"`
	Pagination struct {
		LastVisiblePage *int `json:"last_visible_page"`
	} `json:"pagination"`
}

// Find searches /anime then walks /anime/<id>/episodes pages in order.
func (s *jikanSource) Find(ctx context.Context, q Query) (*Media, error) {
	if q.Kind == KindMovie || q.Title == "" {
		return nil, nil
	}
	base := s.baseURL()
	client := s.httpClient()

	searchURL := base + "/anime?q=" + url.QueryEscape(q.Title) + "&limit=5"
	resp, err := fetchGet(ctx, client, searchURL)
	if err != nil {
		return nil, err
	}
	var sr jikanSearchResp
	if err := decodeCapped(resp, &sr); err != nil {
		return nil, err
	}
	if len(sr.Data) == 0 || sr.Data[0].MalID == 0 {
		return nil, nil
	}
	top := sr.Data[0]
	title := strings.TrimSpace(top.Title)
	if top.TitleEnglish != nil && strings.TrimSpace(*top.TitleEnglish) != "" {
		title = strings.TrimSpace(*top.TitleEnglish)
	}
	year := 0
	if top.Year != nil {
		year = *top.Year
	} else if top.Aired != nil && top.Aired.From != nil {
		year = dateYear(*top.Aired.From)
	}

	eps := make([]Episode, 0, 64)
	lastPage := 1
	for page := 1; page <= lastPage && page <= maxJikanPages; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pageURL := base + "/anime/" + strconv.Itoa(top.MalID) + "/episodes?page=" + strconv.Itoa(page)
		resp, err := fetchGet(ctx, client, pageURL)
		if err != nil {
			return nil, err
		}
		var er jikanEpsResp
		if err := decodeCapped(resp, &er); err != nil {
			return nil, err
		}
		for _, e := range er.Data {
			if e.Episode == nil {
				continue
			}
			// Number is the authoritative data[].episode field, never the
			// slice index: pages may be unordered or gap-filled.
			eps = append(eps, Episode{Season: 0, Number: *e.Episode, Title: strings.TrimSpace(e.Title)})
		}
		if er.Pagination.LastVisiblePage != nil && *er.Pagination.LastVisiblePage > lastPage {
			lastPage = *er.Pagination.LastVisiblePage
		}
	}
	sort.Slice(eps, func(i, j int) bool { return eps[i].Number < eps[j].Number })
	artwork := ""
	if top.Images != nil {
		artwork = firstNonEmpty(top.Images.JPG.LargeImageURL, top.Images.JPG.ImageURL)
	}
	return &Media{
		Source: "jikan", SourceID: strconv.Itoa(top.MalID),
		Kind: KindSeries, Title: title, Year: year,
		Artwork: artwork, Synopsis: capSynopsis(stripHTML(top.Synopsis)),
		Episodes: eps, FetchedAt: time.Now().UTC(),
	}, nil
}
