package metadata

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"
)

// Per-host fetch spacing, chosen from each provider's published limits:
// TVmaze ~2/s, AniList ~1.4/s, Jikan ~2.8/s, iTunes Search 20/min, TMDB
// gated at a 250 ms base. Any host NOT listed here gets NO gate, so
// tests, the loopback fake provider, and any future source are never
// silently throttled by a wrong guess.
var defaultHostIntervals = map[string]time.Duration{
	"api.tvmaze.com":     500 * time.Millisecond,
	"graphql.anilist.co": 700 * time.Millisecond,
	"api.jikan.moe":      350 * time.Millisecond,
	"itunes.apple.com":   3000 * time.Millisecond,
	"api.themoviedb.org": 250 * time.Millisecond,
}

var (
	rlMu        sync.Mutex
	rlIntervals = func() map[string]time.Duration {
		m := make(map[string]time.Duration, len(defaultHostIntervals))
		for h, d := range defaultHostIntervals {
			m[h] = d
		}
		return m
	}()
	rlGates   = make(map[string]*hostGate)
	rlEnabled = true
	rlProfile = ScanSpeedTurbo
)

// Scan-speed profile names. Turbo (the published intervals exactly) is the
// default (fastest, never exceeded); balanced is 2x; gentle is 4x.
// Unknown/empty normalises to turbo.
const (
	ScanSpeedTurbo    = "turbo"
	ScanSpeedBalanced = "balanced"
	ScanSpeedGentle   = "gentle"
)

// NormalizeRateProfile maps any input onto a known profile: turbo,
// balanced, and gentle pass through (case-insensitive); empty and unknown
// yield turbo.
func NormalizeRateProfile(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case ScanSpeedBalanced:
		return ScanSpeedBalanced
	case ScanSpeedGentle:
		return ScanSpeedGentle
	default:
		return ScanSpeedTurbo
	}
}

// SetRateProfile maps a named profile onto the five known hosts, leaving
// any other (test/fake) host entries untouched so SetHostInterval keeps
// working. Unknown/empty profiles yield turbo.
func SetRateProfile(profile string) {
	profile = NormalizeRateProfile(profile)
	mult := 1
	switch profile {
	case ScanSpeedBalanced:
		mult = 2
	case ScanSpeedGentle:
		mult = 4
	}
	rlMu.Lock()
	defer rlMu.Unlock()
	if rlIntervals == nil {
		rlIntervals = make(map[string]time.Duration)
	}
	for h, d := range defaultHostIntervals {
		rlIntervals[h] = d * time.Duration(mult)
	}
	rlProfile = profile
}

// CurrentRateProfile reports the last profile applied via SetRateProfile
// (turbo until the first call).
func CurrentRateProfile() string {
	rlMu.Lock()
	defer rlMu.Unlock()
	if rlProfile == "" {
		return ScanSpeedTurbo
	}
	return rlProfile
}

// RateIntervalForHost is the exported read-back of the effective spacing
// for host (0 when ungated, unknown, or disabled). Tests assert profile
// mapping through it.
func RateIntervalForHost(host string) time.Duration {
	return rateIntervalForHost(host)
}

// hostGate spaces fetches to one host; the first call goes immediately.
type hostGate struct {
	mu   sync.Mutex
	last time.Time
}

// normalizeRateHost lowercases, trims, and strips any :port (including
// httptest's 127.0.0.1:port and bracketed IPv6) so map lookups compare
// bare hostnames.
func normalizeRateHost(h string) string {
	h = strings.TrimSpace(h)
	if h == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	h = strings.TrimSuffix(h, ".")
	return strings.ToLower(h)
}

// rateIntervalForHost reports the spacing for host, or 0 when the host is
// unknown (no gate) or rate limiting is disabled.
func rateIntervalForHost(host string) time.Duration {
	host = normalizeRateHost(host)
	if host == "" {
		return 0
	}
	rlMu.Lock()
	defer rlMu.Unlock()
	if !rlEnabled {
		return 0
	}
	return rlIntervals[host]
}

// SetHostInterval injects or overrides one host's spacing (tests use a
// fake host). d <= 0 removes the entry, leaving the host ungated.
func SetHostInterval(host string, d time.Duration) {
	host = normalizeRateHost(host)
	if host == "" {
		return
	}
	rlMu.Lock()
	defer rlMu.Unlock()
	if rlIntervals == nil {
		rlIntervals = make(map[string]time.Duration)
	}
	if d <= 0 {
		delete(rlIntervals, host)
		return
	}
	rlIntervals[host] = d
}

// SetRateLimitingEnabled toggles all per-host gating (tests may disable
// it outright). Unknown hosts are ungated either way.
func SetRateLimitingEnabled(en bool) {
	rlMu.Lock()
	defer rlMu.Unlock()
	rlEnabled = en
}

// resetRateLimiter clears per-host last-fire times (timing state only;
// configured intervals are kept).
func resetRateLimiter() {
	rlMu.Lock()
	defer rlMu.Unlock()
	rlGates = make(map[string]*hostGate)
}

func gateForHost(host string) *hostGate {
	rlMu.Lock()
	defer rlMu.Unlock()
	g, ok := rlGates[host]
	if !ok {
		g = &hostGate{}
		rlGates[host] = g
	}
	return g
}

// waitForHost sleeps until host's interval has elapsed since the last
// acquisition, then records now. Unknown hosts and disabled limiting
// return immediately. A cancelled ctx aborts the wait. Safe for
// concurrent use; waiters for the same host serialize on that host's
// gate while other hosts proceed independently.
func waitForHost(ctx context.Context, host string) error {
	host = normalizeRateHost(host)
	if host == "" {
		return nil
	}
	rlMu.Lock()
	if !rlEnabled {
		rlMu.Unlock()
		return nil
	}
	interval := rlIntervals[host]
	rlMu.Unlock()
	if interval <= 0 {
		return nil
	}
	g := gateForHost(host)
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.last.IsZero() {
		if d := time.Until(g.last.Add(interval)); d > 0 {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
			}
		}
	}
	g.last = time.Now()
	return nil
}
