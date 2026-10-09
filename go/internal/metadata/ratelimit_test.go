package metadata

import (
	"context"
	"testing"
	"time"
)

// TestRateLimiterSpacing proves the per-host limiter still throttles real
// provider hosts: N acquisitions for a fake host with a known interval
// must be spaced by (almost) the full interval.
func TestRateLimiterSpacing(t *testing.T) {
	const host = "test-ratelimit.example"
	const interval = 100 * time.Millisecond
	const n = 4
	// Tolerance for timer/scheduling granularity (Windows sleep
	// granularity is coarse; the limiter must never fire early by more
	// than this).
	const tolerance = 25 * time.Millisecond

	SetHostInterval(host, interval)
	defer SetHostInterval(host, 0)
	resetRateLimiter()
	defer resetRateLimiter()

	ctx := context.Background()
	times := make([]time.Time, 0, n)
	for i := 0; i < n; i++ {
		if err := waitForHost(ctx, host); err != nil {
			t.Fatalf("waitForHost %d: %v", i, err)
		}
		times = append(times, time.Now())
	}
	for i := 1; i < n; i++ {
		if gap := times[i].Sub(times[i-1]); gap < interval-tolerance {
			t.Errorf("gap %d = %v, want >= %v (interval %v)", i, gap, interval-tolerance, interval)
		}
	}
	if total := times[n-1].Sub(times[0]); total < time.Duration(n-1)*interval-tolerance {
		t.Errorf("total = %v, want >= %v for %d spaced acquisitions", total, time.Duration(n-1)*interval-tolerance, n)
	}
}

// TestRateLimiterUnknownHostUngated proves any host without a configured
// interval (tests, loopback fakes, future sources) is never throttled.
func TestRateLimiterUnknownHostUngated(t *testing.T) {
	resetRateLimiter()
	defer resetRateLimiter()
	ctx := context.Background()
	start := time.Now()
	for i := 0; i < 10; i++ {
		if err := waitForHost(ctx, "unknown-host-127-0-0-1.example"); err != nil {
			t.Fatalf("waitForHost: %v", err)
		}
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("10 ungated acquisitions took %v, want < 100ms", d)
	}
}

// TestRateLimiterDisabled proves the limiter can be switched off outright.
func TestRateLimiterDisabled(t *testing.T) {
	const host = "test-ratelimit-disabled.example"
	SetHostInterval(host, 500*time.Millisecond)
	defer SetHostInterval(host, 0)
	resetRateLimiter()
	defer resetRateLimiter()
	SetRateLimitingEnabled(false)
	defer SetRateLimitingEnabled(true)
	if got := rateIntervalForHost(host); got != 0 {
		t.Fatalf("rateIntervalForHost with limiter disabled = %v, want 0", got)
	}
	ctx := context.Background()
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := waitForHost(ctx, host); err != nil {
			t.Fatalf("waitForHost: %v", err)
		}
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("3 disabled acquisitions took %v, want < 100ms", d)
	}
}

// TestRateLimiterTMDBHostGated proves api.themoviedb.org is a known gated
// host in every profile, scaling with the turbo/balanced/gentle multipliers
// (250/500/1000 ms).
func TestRateLimiterTMDBHostGated(t *testing.T) {
	defer SetRateProfile("turbo")
	for profile, want := range map[string]time.Duration{
		"turbo":    250 * time.Millisecond,
		"balanced": 500 * time.Millisecond,
		"gentle":   1000 * time.Millisecond,
	} {
		SetRateProfile(profile)
		if got := RateIntervalForHost("api.themoviedb.org"); got != want {
			t.Fatalf("profile %q: api.themoviedb.org = %v, want %v", profile, got, want)
		}
	}
}
