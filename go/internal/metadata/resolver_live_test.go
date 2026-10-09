package metadata

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveAbsoluteNoWrongTitle proves the Re:ZERO absolute-1 regression is
// gone on live providers: episode 1 must resolve to the true S1E1 title or
// to not-found, and never to the mislabeled later-season title served by
// AniList media 21355 (Numbers 51-66).
//
// Run: $env:POTFLIX_LIVE='1'; go test ./internal/metadata -run TestLive -v
func TestLiveAbsoluteNoWrongTitle(t *testing.T) {
	if os.Getenv("POTFLIX_LIVE") != "1" {
		t.Skip("live: set POTFLIX_LIVE=1 to run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	r := New(Options{Dir: t.TempDir(), Timeout: 20 * time.Second})
	q := Query{Kind: KindSeries, Title: "Re Zero", Episode: 1, Absolute: true}
	res, err := r.LookupNow(ctx, q)
	if err != nil {
		t.Fatalf("LookupNow Re Zero ep1: %v", err)
	}
	t.Logf("live Re Zero ep1 -> found=%v series=%q season=%d number=%d title=%q", res.Found, res.Series, res.Season, res.Number, res.Episode)
	const wrong = "The Result of the Battle for Priestella"
	if res.Episode == wrong {
		t.Fatalf("BUG PRESENT: episode 1 resolved to wrong title %q", wrong)
	}
	if strings.Contains(res.Episode, "Result of the Battle for Priestella") {
		t.Fatalf("BUG PRESENT: episode 1 resolved to wrong title %q", res.Episode)
	}
	// Either the true S1E1 title or not-found is acceptable; only the wrong
	// title fails. Log which outcome held for the proof tail.
	if !res.Found {
		t.Logf("live Re Zero ep1 not-found: acceptable fallthrough (clean card)")
	}
}

// TestLiveNarutoEp1 locks the Naruto absolute-1 title on live providers.
func TestLiveNarutoEp1(t *testing.T) {
	if os.Getenv("POTFLIX_LIVE") != "1" {
		t.Skip("live: set POTFLIX_LIVE=1 to run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	r := New(Options{Dir: t.TempDir(), Timeout: 20 * time.Second})
	q := Query{Kind: KindSeries, Title: "Naruto", Episode: 1, Absolute: true}
	res, err := r.LookupNow(ctx, q)
	if err != nil {
		t.Fatalf("LookupNow Naruto ep1: %v", err)
	}
	t.Logf("live Naruto ep1 -> found=%v series=%q season=%d number=%d title=%q", res.Found, res.Series, res.Season, res.Number, res.Episode)
	const want = "Enter: Naruto Uzumaki!"
	if !res.Found || res.Episode != want {
		t.Fatalf("Naruto ep1 = %+v, want title %q", res, want)
	}
}
