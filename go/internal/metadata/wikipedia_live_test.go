package metadata

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path"
	"strings"
	"testing"
	"time"
)

// TestLiveWikipediaMovies is env-gated: it hits the real Wikipedia Action
// API for movie titles the owner supplies at runtime and proves the
// keyless fallback returns a title, year, poster and synopsis.
//
// Run: $env:POTFLIX_LIVE='1'; $env:POTFLIX_LIVE_TITLES='Title One;Title Two';
//
//	go test ./internal/metadata -run TestLiveWikipediaMovies -v
//
// Titles come ONLY from the environment so no library filename is ever
// committed. A transient network failure skips (reported plainly) rather
// than failing: it proves nothing about the source either way.
func TestLiveWikipediaMovies(t *testing.T) {
	if os.Getenv("POTFLIX_LIVE") != "1" {
		t.Skip("live: set POTFLIX_LIVE=1 to run")
	}
	raw := os.Getenv("POTFLIX_LIVE_TITLES")
	var titles []string
	for _, part := range strings.Split(raw, ";") {
		if s := strings.TrimSpace(part); s != "" {
			titles = append(titles, s)
		}
	}
	if len(titles) == 0 {
		t.Skip("live: set POTFLIX_LIVE_TITLES='Title One;Title Two' to run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	s := &wikipediaSource{}
	for _, title := range titles {
		m, err := s.Find(ctx, Query{Kind: KindMovie, Title: title})
		if err != nil {
			if errors.Is(err, ErrLookupUnavailable) {
				t.Logf("live wikipedia %q: network unavailable from this host (%v), saying so plainly", title, err)
				continue
			}
			t.Errorf("live wikipedia %q: Find: %v", title, err)
			continue
		}
		if m == nil {
			t.Logf("live wikipedia %q: definitive miss (no page)", title)
			continue
		}
		t.Logf("live wikipedia query=%q -> title=%q year=%d poster=%v host=%q ext=%q synopsisLen=%d",
			title, m.Title, m.Year, m.Artwork != "", liveArtHost(m.Artwork), liveArtExt(m.Artwork), len([]rune(m.Synopsis)))
		if m.Title == "" {
			t.Errorf("live wikipedia %q: empty title", title)
		}
		if m.Artwork != "" && !strings.HasPrefix(m.Artwork, "https://") {
			t.Errorf("live wikipedia %q: artwork = %q, want https", title, m.Artwork)
		}
	}
}

// liveArtHost/liveArtExt report where a poster URL points (host and file
// extension only, never the full URL) for the live-test log line.
func liveArtHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

func liveArtExt(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(path.Ext(u.Path))
}
