package metadata

import (
	"testing"
	"time"
)

// TestRateLimiterInitialProfileIsTurbo proves the package default is 1x
// (turbo): a resolver constructed without an explicit profile runs at the
// published intervals. It reads only, never calls SetRateProfile, and must
// stay in this file so it runs before the profile tests that mutate the
// global limiter state.
func TestRateLimiterInitialProfileIsTurbo(t *testing.T) {
	if got := CurrentRateProfile(); got != "turbo" {
		t.Fatalf("initial CurrentRateProfile = %q, want turbo", got)
	}
	for host, want := range map[string]time.Duration{
		"api.tvmaze.com":     500 * time.Millisecond,
		"graphql.anilist.co": 700 * time.Millisecond,
		"api.jikan.moe":      350 * time.Millisecond,
		"itunes.apple.com":   3000 * time.Millisecond,
		"en.wikipedia.org":   1000 * time.Millisecond,
	} {
		if got := RateIntervalForHost(host); got != want {
			t.Fatalf("initial host %q = %v, want published %v", host, got, want)
		}
	}
}
