package metadata

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// titleFuncSource is a fake Source whose answer depends on the queried title,
// so the primary (polluted) and alternate (head) attempts can miss, hit, or
// fail independently.
type titleFuncSource struct {
	name  string
	mu    *sync.Mutex
	calls map[string]int
	find  func(q Query) (*Media, error)
}

func (s *titleFuncSource) Name() string { return s.name }

func (s *titleFuncSource) Find(_ context.Context, q Query) (*Media, error) {
	if s.mu != nil {
		s.mu.Lock()
		s.calls[q.Title]++
		s.mu.Unlock()
	}
	return s.find(q)
}

func TestAlternateQuery(t *testing.T) {
	cases := []struct {
		name      string
		q         Query
		wantTitle string
		wantOK    bool
	}{
		{
			name:      "arc title yields colon form",
			q:         Query{Kind: KindSeries, Title: "Dragon Ball Super - Beerus", Episode: 1, Absolute: true},
			wantTitle: "Dragon Ball Super: Beerus",
			wantOK:    true,
		},
		{
			name:      "arc suffix with slashes",
			q:         Query{Kind: KindSeries, Title: "Fate/stay night - Unlimited Blade Works", Episode: 1, Absolute: true},
			wantTitle: "Fate/stay night: Unlimited Blade Works",
			wantOK:    true,
		},
		{
			name:      "every separator normalized",
			q:         Query{Kind: KindSeries, Title: "Show - Arc - Part", Season: 1, Episode: 2},
			wantTitle: "Show: Arc: Part",
			wantOK:    true,
		},
		{
			name:   "no separator yields none",
			q:      Query{Kind: KindSeries, Title: "Dragon Ball Super", Episode: 1, Absolute: true},
			wantOK: false,
		},
		{
			name:   "movie never alternates",
			q:      Query{Kind: KindMovie, Title: "Dune - Part Two"},
			wantOK: false,
		},
		{
			name:   "unknown never alternates",
			q:      Query{Kind: KindUnknown, Title: "Show - Arc"},
			wantOK: false,
		},
		{
			name:   "empty title yields none",
			q:      Query{Kind: KindSeries, Title: "", Episode: 1, Absolute: true},
			wantOK: false,
		},
		{
			name:   "whitespace title yields none",
			q:      Query{Kind: KindSeries, Title: "   ", Episode: 1, Absolute: true},
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			alt, ok := alternateQuery(tc.q)
			if ok != tc.wantOK {
				t.Fatalf("alternateQuery(%+v) ok = %v, want %v", tc.q, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if alt.Title != tc.wantTitle {
				t.Errorf("alternate title = %q, want %q", alt.Title, tc.wantTitle)
			}
			// The retry must still answer the ORIGINAL request: every
			// field but the title carries over.
			if alt.Kind != tc.q.Kind || alt.Year != tc.q.Year || alt.Season != tc.q.Season ||
				alt.Episode != tc.q.Episode || alt.Absolute != tc.q.Absolute {
				t.Errorf("alternate = %+v, want non-title fields of %+v", alt, tc.q)
			}
		})
	}
}

// TestAlternateFallbackReturnsShow proves the retry answers a polluted
// dash title when the primary title misses: the separator-normalized query
// hits the same source chain, the resolution carries the canonical series
// title and episode, and the media is cached under the ORIGINAL query key
// so the next poll's non-blocking Resolve serves it without touching the
// network.
func TestAlternateFallbackReturnsShow(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	polluted := Query{Kind: KindSeries, Title: "Dragon Ball Super - Beerus", Episode: 1, Absolute: true}
	var mu sync.Mutex
	calls := map[string]int{}
	src := &titleFuncSource{name: "tvmaze", mu: &mu, calls: calls, find: func(q Query) (*Media, error) {
		if q.Title == "Dragon Ball Super: Beerus" {
			return &Media{
				Source: "tvmaze", SourceID: "93969",
				Kind: KindSeries, Title: "Dragon Ball Super: Beerus",
				Artwork:  "https://example.com/beerus.jpg",
				Episodes: []Episode{{Season: 1, Number: 1, Title: "The God Who Destroys"}},
			}, nil
		}
		return nil, nil
	}}
	r := New(Options{Dir: dir, Sources: []Source{src}, Timeout: 5 * time.Second})

	res, err := r.LookupNow(ctx, polluted)
	if err != nil {
		t.Fatalf("LookupNow: %v", err)
	}
	if !res.Found {
		t.Fatalf("resolution = %+v, want found via alternate", res)
	}
	if res.Series != "Dragon Ball Super: Beerus" {
		t.Errorf("series = %q, want canonical colon title", res.Series)
	}
	if res.Number != 1 || res.Episode != "The God Who Destroys" {
		t.Errorf("episode = S%d E%d %q, want S1 E1 %q", res.Season, res.Number, res.Episode, "The God Who Destroys")
	}
	mu.Lock()
	primaryCalls, altCalls := calls["Dragon Ball Super - Beerus"], calls["Dragon Ball Super: Beerus"]
	mu.Unlock()
	if primaryCalls != 1 || altCalls != 1 {
		t.Errorf("chain calls primary=%d alternate=%d, want 1 and 1", primaryCalls, altCalls)
	}

	// Cached under the ORIGINAL key: a cold resolver whose sources always
	// fail still serves the next poll's non-blocking Resolve from disk.
	key := cacheKey(polluted)
	if e, ok := cacheLoadEntry(dir, key); !ok || e.NotFound || e.Media == nil {
		t.Fatalf("original-key entry = %+v,%v; want positive hit", e, ok)
	}
	boom := &fakeSource{name: "tvmaze", err: errors.New("must not be called")}
	cold := New(Options{Dir: dir, Sources: []Source{boom}, Timeout: 5 * time.Second})
	got, err := cold.Resolve(ctx, polluted)
	if err != nil {
		t.Fatalf("cold Resolve: %v", err)
	}
	if !got.Found || got.Series != "Dragon Ball Super: Beerus" || got.Episode != "The God Who Destroys" {
		t.Fatalf("cold Resolve = %+v, want cached Found", got)
	}
	if n := boom.calls.Load(); n != 0 {
		t.Fatalf("cold Resolve provider calls = %d, want 0 (served from original-key cache)", n)
	}
}

// TestAlternateNeverReturnsHeadShow is the regression test for the
// head-strip fallback defect: with a source that answers the stripped head
// title "Dragon Ball Super" (the 2015 series) to ANY query and the true
// "Dragon Ball Super: Beerus" only to the normalized query, the resolver
// must return the Beerus media and never the 2015 show. The stripped head
// must never be queried at all.
func TestAlternateNeverReturnsHeadShow(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	polluted := Query{Kind: KindSeries, Title: "Dragon Ball Super - Beerus", Episode: 1, Absolute: true}
	var mu sync.Mutex
	calls := map[string]int{}
	src := &titleFuncSource{name: "tvmaze", mu: &mu, calls: calls, find: func(q Query) (*Media, error) {
		if q.Title == "Dragon Ball Super: Beerus" {
			return &Media{
				Source: "tvmaze", SourceID: "93969",
				Kind: KindSeries, Title: "Dragon Ball Super: Beerus",
				Artwork:  "https://example.com/beerus.jpg",
				Episodes: []Episode{{Season: 1, Number: 1, Title: "The God Who Destroys"}},
			}, nil
		}
		if q.Title == "Dragon Ball Super" {
			return &Media{
				Source: "tvmaze", SourceID: "2368",
				Kind: KindSeries, Title: "Dragon Ball Super",
				Artwork:  "https://example.com/dbs-2015.jpg",
				Episodes: []Episode{{Season: 1, Number: 1, Title: "The Peace Prize. Who'll Get the 100 Million Zeny!"}},
			}, nil
		}
		return nil, nil
	}}
	r := New(Options{Dir: dir, Sources: []Source{src}, Timeout: 5 * time.Second})

	res, err := r.LookupNow(ctx, polluted)
	if err != nil {
		t.Fatalf("LookupNow: %v", err)
	}
	if !res.Found {
		t.Fatalf("resolution = %+v, want found via normalized alternate", res)
	}
	if res.Series != "Dragon Ball Super: Beerus" {
		t.Errorf("series = %q, want the 2026 Beerus show, never the 2015 head title", res.Series)
	}
	if res.Episode == "The Peace Prize. Who'll Get the 100 Million Zeny!" {
		t.Errorf("episode = %q, the 2015 show's episode 1 leaked through", res.Episode)
	}
	mu.Lock()
	headCalls := calls["Dragon Ball Super"]
	mu.Unlock()
	if headCalls != 0 {
		t.Errorf("head-title queries = %d, want 0 (the strip must never be sent)", headCalls)
	}
}

// TestAlternateTransientMerging proves a transient on EITHER attempt is
// never reported as a confirmed not-found: both-error and mixed
// miss/error pairs yield ErrLookupUnavailable and persist nothing, while a
// double definitive miss stays a nil-error not-found with one negative.
func TestAlternateTransientMerging(t *testing.T) {
	primaryBoom := errors.New("primary-boom")
	altBoom := errors.New("alt-boom")
	cases := []struct {
		name string
		find func(q Query) (*Media, error)
		// wantErr is "" for a definitive miss (nil error), or a substring
		// the transient error must carry (and wrap ErrLookupUnavailable).
		wantErr string
		// wantFile reports whether a negative file must exist afterwards.
		wantFile bool
	}{
		{
			name: "both transient",
			find: func(q Query) (*Media, error) {
				return nil, altBoom
			},
			wantErr: "alt-boom",
		},
		{
			name: "primary miss alternate transient",
			find: func(q Query) (*Media, error) {
				if q.Title == "Dragon Ball Super: Beerus" {
					return nil, altBoom
				}
				return nil, nil
			},
			wantErr: "alt-boom",
		},
		{
			name: "primary transient surfaces primary error",
			find: func(q Query) (*Media, error) {
				if q.Title == "Dragon Ball Super: Beerus" {
					return nil, nil
				}
				return nil, primaryBoom
			},
			wantErr: "primary-boom",
		},
		{
			name: "both definitive miss",
			find: func(q Query) (*Media, error) {
				return nil, nil
			},
			wantErr:  "",
			wantFile: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := &titleFuncSource{name: "fake", find: tc.find}
			r := New(Options{Dir: dir, Sources: []Source{src}, Timeout: 5 * time.Second})
			q := Query{Kind: KindSeries, Title: "Dragon Ball Super - Beerus", Episode: 1, Absolute: true}
			res, err := r.LookupNowForce(context.Background(), q, true)
			if res.Found {
				t.Fatalf("resolution = %+v, want miss", res)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("definitive miss err = %v, want nil", err)
				}
			} else {
				if !errors.Is(err, ErrLookupUnavailable) {
					t.Fatalf("err = %v, want ErrLookupUnavailable", err)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("err = %q, want underlying cause %q", err.Error(), tc.wantErr)
				}
			}
			key := cacheKey(q)
			_, statErr := os.Stat(filepath.Join(dir, cacheFileName(key)))
			if tc.wantFile && os.IsNotExist(statErr) {
				t.Errorf("definitive miss persisted no file, want one negative")
			}
			if !tc.wantFile && statErr == nil {
				t.Errorf("transient lookup persisted a file, want nothing")
			}
		})
	}
}
