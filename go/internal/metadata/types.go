// Package metadata identifies TV/anime/movie media from a cleaned PotPlayer
// title and resolves real episode titles from free, keyless APIs, with a
// permanent on-disk cache.
package metadata

import (
	"context"
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
}

// Media is a cached provider record for one show/season or movie.
type Media struct {
	Source   string `json:"source"` // "tvmaze"|"anilist"|"jikan"|"itunes"
	SourceID string `json:"sourceId"`
	Kind     Kind   `json:"kind"`
	Title    string `json:"title"` // canonical series/movie title
	Year     int    `json:"year,omitempty"`
	// Artwork/Synopsis are series/movie-level (show poster, show summary).
	// Per-episode art/summary live on Episode; the resolver prefers those.
	Artwork   string    `json:"artwork,omitempty"`
	Synopsis  string    `json:"synopsis,omitempty"`
	Episodes  []Episode `json:"episodes,omitempty"`
	FetchedAt time.Time `json:"fetchedAt"`
}

// Resolution is the user-facing answer for a Query.
type Resolution struct {
	Series   string
	Episode  string
	Season   int
	Number   int
	Found    bool
	Artwork  string // episode art when known, else series/movie art, else ""
	Synopsis string // plain-text one-line summary, capped, "" if unknown
}

// Source is one metadata provider.
type Source interface {
	Name() string
	Find(ctx context.Context, q Query) (*Media, error)
}

// Options configures a Resolver.
type Options struct {
	Dir     string        // cache dir; "" -> default
	Timeout time.Duration // per HTTP request; 0 -> 8s
	Sources []Source      // nil -> DefaultSources()
	Logf    func(format string, args ...any)
	// Offline, when true, serves the memory/disk cache only: Resolve and
	// LookupNow never touch the network and never spawn a background fetch.
	Offline bool
}
