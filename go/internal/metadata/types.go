// Package metadata identifies TV/anime/movie media from a cleaned PotPlayer
// title and resolves real episode titles from free, keyless APIs, with a
// permanent on-disk cache.
package metadata

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Kind classifies a query or media entry.
type Kind int

const (
	KindUnknown Kind = iota
	KindSeries
	KindMovie
)

// Query is a parsed identification request.
type Query struct {
	Kind     Kind
	Title    string // cleaned series/movie name
	Year     int    // 0 if unknown
	Season   int    // 0 if unknown
	Episode  int
	Absolute bool // true when Episode is an absolute anime number
}

// Episode is a single resolved episode title.
type Episode struct {
	Season   int    `json:"season"`
	Number   int    `json:"number"`
	Title    string `json:"title"`
	Artwork  string `json:"artwork,omitempty"`  // episode still/poster URL, "" if unknown
	Synopsis string `json:"synopsis,omitempty"` // plain-text episode summary, "" if unknown
	URL      string `json:"url,omitempty"`      // episode page URL (TVmaze episode.url, AniList streaming url), "" if unknown
	Length   string `json:"length,omitempty"`   // per-episode length ("45 min"), "" if unknown
}

// Button is one card page button (label + https page URL).
type Button struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// Media is a cached provider record for one show/season or movie.
type Media struct {
	Source   string `json:"source"` // "tvmaze"|"anilist"|"jikan"|"itunes"
	SourceID string `json:"sourceId"`
	Kind     Kind   `json:"kind"`
	Title    string `json:"title"` // canonical series/movie title
	Year     int    `json:"year,omitempty"`
	// Artwork/Synopsis are series/movie-level (show poster, show summary).
	// Per-episode art/summary live on Episode; the resolver prefers episode
	// synopsis but ALWAYS uses the series/movie poster for artwork (episode
	// stills look unofficial).
	Artwork    string    `json:"artwork,omitempty"`
	Synopsis   string    `json:"synopsis,omitempty"`
	Genres     string    `json:"genres,omitempty"`     // "Drama . Crime", "" if unknown
	Length     string    `json:"length,omitempty"`     // show/movie length ("45 min", "2h 19m"), "" if unknown
	ShowURL    string    `json:"showUrl,omitempty"`    // series/show page (TVmaze show.url, MAL/AniList), "" if unknown
	EpisodeURL string    `json:"episodeUrl,omitempty"` // TVmaze matched-episode page, "" if unknown
	MovieURL   string    `json:"movieUrl,omitempty"`   // iTunes trackViewUrl, "" if unknown
	TrailerURL string    `json:"trailerUrl,omitempty"` // iTunes previewUrl, "" if unknown
	Episodes   []Episode `json:"episodes,omitempty"`
	FetchedAt  time.Time `json:"fetchedAt"`
}

// Resolution is the user-facing answer for a Query.
type Resolution struct {
	Series   string
	Episode  string
	Season   int
	Number   int
	Found    bool
	Artwork  string // series/movie poster only, else ""
	Synopsis string // plain-text one-line summary, capped, "" if unknown
	Genres   string // "Drama . Crime", "" if unknown
	Length   string // "45 min" / "2h 19m", "" if unknown
	Year     int
	IsMovie  bool
	Buttons  []Button // max 2, https-only page buttons
}

// Source is one metadata provider.
type Source interface {
	Name() string
	Find(ctx context.Context, q Query) (*Media, error)
}

// joinGenres joins non-blank genre names with " . " (U+00B7).
func joinGenres(gs []string) string {
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		if t := strings.TrimSpace(g); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, " \u00b7 ")
}

// normalizeURL forces provider page URLs to https and drops non-http(s).
// "" when empty or not http(s).
func normalizeURL(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" {
		return ""
	}
	if strings.HasPrefix(u, "http://") {
		u = "https://" + strings.TrimPrefix(u, "http://")
	}
	if strings.HasPrefix(u, "https://") {
		if len(u) > len("https://") {
			return u
		}
		return ""
	}
	return ""
}

// runtimeLength formats whole minutes as "45 min", "" when <= 0.
func runtimeLength(mins int) string {
	if mins <= 0 {
		return ""
	}
	return fmt.Sprintf("%d min", mins)
}

// millisLength formats iTunes trackTimeMillis as "2h 19m" / "2h" / "45 min".
func millisLength(millis int64) string {
	if millis <= 0 {
		return ""
	}
	totalMin := int((millis + 30000) / 60000)
	if totalMin <= 0 {
		return ""
	}
	h := totalMin / 60
	m := totalMin % 60
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%d min", totalMin)
	}
}

// Options configures a Resolver.
type Options struct {
	Dir     string        // cache dir; "" -> default
	Timeout time.Duration // per HTTP request; 0 -> 8s
	Sources []Source      // nil -> DefaultSources()
	Logf    func(format string, args ...any)
	// KeyProvider yields the stored API key for a provider name ("tmdb");
	// nil (or a blank return) means keyless, exactly today's behaviour: no
	// keyed source is built and the provider order is unchanged.
	KeyProvider func(provider string) string
	// Offline, when true, serves the memory/disk cache only: Resolve and
	// LookupNow never touch the network and never spawn a background fetch.
	Offline bool
}
