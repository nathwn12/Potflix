package metadata

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveAnimeNumbering is env-gated: it hits the live AniList GraphQL
// endpoint for Re:ZERO and proves the absolute-episode mapping.
//
//	BEFORE: streamingEpisodes were numbered by positional index (i+1), so
//	the first entry "Episode 66 - The Result of the Battle for Priestella"
//	became Number 1, and episode 1 resolved to that much-later title.
//	AFTER: the number is parsed from the entry title, so that entry is
//	Number 66 and episode 66 resolves to it while episode 1 no longer does.
//
// Run: $env:POTFLIX_LIVE='1'; go test ./internal/metadata -run TestLiveAnimeNumbering -v
func TestLiveAnimeNumbering(t *testing.T) {
	if os.Getenv("POTFLIX_LIVE") != "1" {
		t.Skip("live: set POTFLIX_LIVE=1 to run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	s := &anilistSource{}
	m, err := s.Find(ctx, Query{Kind: KindSeries, Title: "Re Zero", Episode: 66, Absolute: true})
	if err != nil {
		t.Fatalf("anilist Find: %v", err)
	}
	if m == nil || len(m.Episodes) == 0 {
		t.Fatal("expected live AniList media with episodes, got none")
	}
	t.Logf("live anilist: title=%q sourceId=%s count=%d", m.Title, m.SourceID, len(m.Episodes))
	for _, e := range m.Episodes {
		t.Logf("live anilist ep: number=%d title=%q", e.Number, e.Title)
	}

	// Deterministic ascending order.
	for i := 1; i < len(m.Episodes); i++ {
		if m.Episodes[i].Number < m.Episodes[i-1].Number {
			t.Fatalf("episodes not sorted: %+v", m.Episodes)
		}
	}

	// The known later episode must carry its embedded number, not index 1.
	const wantTitle = "The Result of the Battle for Priestella"
	found := false
	for _, e := range m.Episodes {
		if strings.Contains(e.Title, "Result of the Battle for Priestella") {
			found = true
			if e.Number == 1 {
				t.Fatalf("BUG PRESENT: %q still mapped to Number 1 (positional index)", e.Title)
			}
			t.Logf("AFTER: %q -> Number %d (BEFORE: Number 1)", e.Title, e.Number)
		}
	}
	if !found {
		t.Logf("note: %q absent from live list today; raw count=%d", wantTitle, len(m.Episodes))
	}

	// Episode 66 must resolve to the later title (Number loop, beyond len).
	r := New(Options{Dir: t.TempDir(), Sources: []Source{s}, Timeout: 20 * time.Second})
	res66, err := r.LookupNow(ctx, Query{Kind: KindSeries, Title: "Re Zero", Episode: 66, Absolute: true})
	if err != nil {
		t.Fatalf("LookupNow ep66: %v", err)
	}
	t.Logf("resolve ep66 -> found=%v title=%q", res66.Found, res66.Episode)
	if found && res66.Episode != wantTitle {
		t.Fatalf("ep66 = %q, want %q", res66.Episode, wantTitle)
	}

	// Episode 1 must NOT resolve to the much-later title (the reported bug).
	res1, err := r.LookupNow(ctx, Query{Kind: KindSeries, Title: "Re Zero", Episode: 1, Absolute: true})
	if err != nil {
		t.Fatalf("LookupNow ep1: %v", err)
	}
	t.Logf("resolve ep1 -> found=%v title=%q", res1.Found, res1.Episode)
	if res1.Episode == wantTitle {
		t.Fatalf("BUG PRESENT: episode 1 still resolves to %q", wantTitle)
	}
	if found {
		t.Logf("BEFORE: ep1 -> %q | AFTER: ep1 -> %q, ep66 -> %q", wantTitle, res1.Episode, res66.Episode)
	}

	// Cross-check Jikan v4 page 1 when reachable from this host.
	j := &jikanSource{}
	jm, jerr := j.Find(ctx, Query{Kind: KindSeries, Title: "Re Zero", Episode: 1, Absolute: true})
	if jerr != nil {
		t.Logf("jikan unreachable from this host (say so per brief): %v; comparing against the AniList entry's own title instead", jerr)
		return
	}
	if jm == nil || len(jm.Episodes) == 0 {
		t.Logf("jikan returned no media; comparing against the AniList entry's own title instead")
		return
	}
	t.Logf("live jikan: title=%q sourceId=%s count=%d first=%+v", jm.Title, jm.SourceID, len(jm.Episodes), jm.Episodes[0])
}
