package metadata

import (
	"testing"
	"time"
)

// TestRateProfileMapping proves the three named profiles map onto the five
// known hosts with the published intervals as the floor: turbo is exactly
// the published values, balanced is 2x, gentle is 4x. Turbo is the default:
// unknown or empty profiles yield turbo, and an unknown host stays ungated
// in every profile.
func TestRateProfileMapping(t *testing.T) {
	defer SetRateProfile("turbo")
	cases := map[string]map[string]time.Duration{
		"turbo": {
			"api.tvmaze.com":     500 * time.Millisecond,
			"graphql.anilist.co": 700 * time.Millisecond,
			"api.jikan.moe":      350 * time.Millisecond,
			"itunes.apple.com":   3000 * time.Millisecond,
			"en.wikipedia.org":   1000 * time.Millisecond,
		},
		"balanced": {
			"api.tvmaze.com":     1000 * time.Millisecond,
			"graphql.anilist.co": 1400 * time.Millisecond,
			"api.jikan.moe":      700 * time.Millisecond,
			"itunes.apple.com":   6000 * time.Millisecond,
			"en.wikipedia.org":   2000 * time.Millisecond,
		},
		"gentle": {
			"api.tvmaze.com":     2000 * time.Millisecond,
			"graphql.anilist.co": 2800 * time.Millisecond,
			"api.jikan.moe":      1400 * time.Millisecond,
			"itunes.apple.com":   12000 * time.Millisecond,
			"en.wikipedia.org":   4000 * time.Millisecond,
		},
	}
	for _, profile := range []string{"turbo", "balanced", "gentle"} {
		SetRateProfile(profile)
		if got := CurrentRateProfile(); got != profile {
			t.Fatalf("profile %q: CurrentRateProfile = %q, want %q", profile, got, profile)
		}
		for host, want := range cases[profile] {
			if got := RateIntervalForHost(host); got != want {
				t.Fatalf("profile %q host %q = %v, want %v", profile, host, got, want)
			}
		}
		if got := RateIntervalForHost("unknown-host-127-0-0-1.example"); got != 0 {
			t.Fatalf("profile %q unknown host = %v, want 0 (ungated)", profile, got)
		}
	}
	// Unknown/empty profiles fall back to turbo.
	for _, profile := range []string{"", "  ", "ludicrous", "TURBO-fast"} {
		SetRateProfile(profile)
		if got := CurrentRateProfile(); got != "turbo" {
			t.Fatalf("profile %q: CurrentRateProfile = %q, want turbo", profile, got)
		}
		for host, want := range cases["turbo"] {
			if got := RateIntervalForHost(host); got != want {
				t.Fatalf("profile %q host %q = %v, want turbo %v", profile, host, got, want)
			}
		}
		if got := RateIntervalForHost("unknown-host-127-0-0-1.example"); got != 0 {
			t.Fatalf("profile %q unknown host = %v, want 0 (ungated)", profile, got)
		}
	}
	// Case-insensitivity: named profiles normalise.
	SetRateProfile("Turbo")
	if got := RateIntervalForHost("api.tvmaze.com"); got != 500*time.Millisecond {
		t.Fatalf("profile Turbo host tvmaze = %v, want 500ms", got)
	}
	SetRateProfile("GENTLE")
	if got := RateIntervalForHost("api.tvmaze.com"); got != 2000*time.Millisecond {
		t.Fatalf("profile GENTLE host tvmaze = %v, want 2000ms", got)
	}
}
