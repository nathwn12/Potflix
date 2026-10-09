package metadata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// countingSource is a fake Source that records every Find call by source
// name so the resolver chain cost can be asserted per query kind.
type countingSource struct {
	name  string
	mu    *sync.Mutex
	calls map[string]int
	find  func(q Query) *Media
}

func (s *countingSource) Name() string { return s.name }

func (s *countingSource) Find(_ context.Context, q Query) (*Media, error) {
	s.mu.Lock()
	s.calls[s.name]++
	s.mu.Unlock()
	if s.find != nil {
		return s.find(q), nil
	}
	return nil, nil
}

// countingResolver builds a Resolver whose providers are fakes named exactly
// like the defaults, so orderedSources ranks them as in production.
func countingResolver(t *testing.T, mu *sync.Mutex, calls map[string]int, find func(name string, q Query) *Media) *Resolver {
	t.Helper()
	mk := func(name string) *countingSource {
		return &countingSource{name: name, mu: mu, calls: calls, find: func(q Query) *Media {
			return find(name, q)
		}}
	}
	return New(Options{
		Dir:     t.TempDir(),
		Timeout: defaultHTTPTimeout(),
		Sources: []Source{mk("tvmaze"), mk("anilist"), mk("jikan"), mk("itunes")},
	})
}

func lookupCounts(t *testing.T, r *Resolver, q Query, mu *sync.Mutex, calls map[string]int) map[string]int {
	t.Helper()
	mu.Lock()
	for k := range calls {
		delete(calls, k)
	}
	mu.Unlock()
	if _, err := r.LookupNowForce(context.Background(), q, true); err != nil {
		t.Fatalf("LookupNowForce: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	out := make(map[string]int, len(calls))
	for k, v := range calls {
		out[k] = v
	}
	return out
}

func seriesAnswer(q Query) *Media {
	return &Media{Title: q.Title, Episodes: []Episode{{Season: q.Season, Number: q.Episode, Title: "Pilot"}}}
}

// TestChainCallCounts pins how many provider Find calls one lookup costs
// per query kind. NOTE: these are Find invocations, not network calls.
// Each source self-gates ruled-out kinds before touching the wire
// (itunes.go:59 for series; tvmaze.go:116, anilist.go:114, jikan.go:73
// for movies), pinned by TestKindGateNoNetwork below. So a series miss
// still INVOKES itunes.Find (counted here) but pays no rate-limiter wait;
// only movie/unknown queries reach the iTunes wire, which is legitimate
// movie-fallback coverage, not avoidable waste.
func TestChainCallCounts(t *testing.T) {
	tvQ := ResolveQuery("Breaking Bad S01E01.mkv", "")
	if tvQ.Kind != KindSeries {
		t.Fatalf("tv fixture kind = %v, want KindSeries (%+v)", tvQ.Kind, tvQ)
	}
	t.Logf("tv fixture: kind=%v title=%q season=%d episode=%d", tvQ.Kind, tvQ.Title, tvQ.Season, tvQ.Episode)
	animeQ := ResolveQuery("[Anime Time] Re Zero - 01.mkv", "")
	if animeQ.Kind != KindSeries || !animeQ.Absolute {
		t.Fatalf("anime fixture = %+v, want KindSeries absolute", animeQ)
	}
	t.Logf("anime fixture: kind=%v title=%q season=%d episode=%d", animeQ.Kind, animeQ.Title, animeQ.Season, animeQ.Episode)
	movieQ := Query{Kind: KindMovie, Title: "Inception"}
	missQ := ResolveQuery("Xyzzy Qqq S01E01.mkv", "")
	if missQ.Kind != KindSeries {
		t.Fatalf("miss fixture kind = %v, want KindSeries (%+v)", missQ.Kind, missQ)
	}
	bareQ := ResolveQuery("Sample.mkv", "")
	if bareQ.Kind != KindUnknown {
		t.Fatalf("bare fixture kind = %v, want KindUnknown (%+v)", bareQ.Kind, bareQ)
	}

	cases := []struct {
		name string
		q    Query
		// answerAt names the fake that returns answering media ("" = total miss).
		answerAt string
		// want maps source name to expected Find-invocation count (absent = 0).
		want map[string]int
	}{
		{"tv hit at first provider", tvQ, "tvmaze", map[string]int{"tvmaze": 1}},
		{"movie hit at itunes", movieQ, "itunes", nil}, // want set below by kind order
		{"anime hit at anilist", animeQ, "anilist", map[string]int{"tvmaze": 1, "anilist": 1}},
		// itunes IS invoked for the series miss (the resolver has no
		// kind filter) but returns nil before the wire (itunes.go:59),
		// so no 3s limiter wait is paid. See TestKindGateNoNetwork.
		{"tv definitive miss invokes itunes pre-wire only", missQ, "", map[string]int{"tvmaze": 1, "anilist": 1, "jikan": 1, "itunes": 1}},
		{"unknown definitive miss keeps full chain", bareQ, "", map[string]int{"tvmaze": 1, "anilist": 1, "jikan": 1, "itunes": 1}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			calls := map[string]int{}
			r := countingResolver(t, &mu, calls, func(name string, q Query) *Media {
				if name != tc.answerAt {
					return nil
				}
				if tc.q.Kind == KindMovie || q.Kind == KindMovie {
					return &Media{Title: q.Title}
				}
				return seriesAnswer(q)
			})
			got := lookupCounts(t, r, tc.q, &mu, calls)
			want := tc.want
			if tc.name == "movie hit at itunes" {
				// Movie order ranks itunes first: exactly one call, to itunes.
				want = map[string]int{"itunes": 1}
			}
			t.Logf("counts: tvmaze=%d anilist=%d jikan=%d itunes=%d (kind=%v)",
				got["tvmaze"], got["anilist"], got["jikan"], got["itunes"], tc.q.Kind)
			for _, name := range []string{"tvmaze", "anilist", "jikan", "itunes"} {
				if got[name] != want[name] {
					t.Errorf("source %s: got %d calls, want %d (full table %v)", name, got[name], want[name], got)
				}
			}
		})
	}
}

// TestKindGateNoNetwork proves each source self-gates ruled-out kinds
// BEFORE touching the wire: pointed at a dead server, a ruled-out query
// must return (nil, nil), while an allowed query must fail with a network
// error (positive control - proves the dead server would have been hit,
// so a (nil, nil) is a real gate and not a broken test harness).
func TestKindGateNoNetwork(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // every dial now refuses; any wire attempt surfaces as an error
	cli := dead.Client()

	seriesQ := Query{Kind: KindSeries, Title: "Breaking Bad", Season: 1, Episode: 1}
	movieQ := Query{Kind: KindMovie, Title: "Inception"}

	cases := []struct {
		name     string
		src      Source
		ruledOut Query // must yield (nil, nil) with no error
		allowed  Query // must yield an error (wire was attempted)
	}{
		{"itunes", &itunesSource{base: deadURL, client: cli}, seriesQ, movieQ},
		{"tvmaze", &tvmazeSource{base: deadURL, client: cli}, movieQ, seriesQ},
		{"anilist", &anilistSource{base: deadURL, client: cli}, movieQ, seriesQ},
		{"jikan", &jikanSource{base: deadURL, client: cli}, movieQ, seriesQ},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if m, err := tc.src.Find(ctx, tc.ruledOut); err != nil || m != nil {
				t.Errorf("ruled-out query: got (%v, %v), want (nil, nil)", m, err)
			}
			if _, err := tc.src.Find(ctx, tc.allowed); err == nil {
				t.Errorf("allowed query against dead server: got nil error, positive control failed")
			} else {
				t.Logf("allowed query error (proves wire would be hit): %v", err)
			}
		})
	}
}
