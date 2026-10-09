package metadata

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeSource struct {
	name  string
	calls atomic.Int32
	media *Media
	err   error
	block chan struct{} // when non-nil, Find waits here (or ctx)
	delay time.Duration
}

func (f *fakeSource) Name() string { return f.name }

func (f *fakeSource) Find(ctx context.Context, q Query) (*Media, error) {
	f.calls.Add(1)
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.media, f.err
}

func seriesMedia() *Media {
	return &Media{
		Source: "fake", SourceID: "1",
		Kind: KindSeries, Title: "Show",
		Episodes:  []Episode{{Season: 1, Number: 2, Title: "Pilot"}},
		FetchedAt: time.Now().UTC(),
	}
}

// Resolve must return at once on a miss even while the fetch is stuck, then
// serve from memory once the fetch lands without a second fetch.
func TestResolveNonBlockingThenMemory(t *testing.T) {
	fake := &fakeSource{name: "fake", media: seriesMedia(), block: make(chan struct{})}
	r := New(Options{Dir: t.TempDir(), Sources: []Source{fake}, Timeout: 5 * time.Second})
	q := Query{Kind: KindSeries, Title: "Show", Season: 1, Episode: 2}
	ctx := context.Background()

	res, err := r.Resolve(ctx, q)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Found {
		t.Fatal("expected Found=false on cache miss")
	}

	// The background fetch is stuck in fake.Find; LookupNow must wait for it.
	type result struct {
		res Resolution
		err error
	}
	nowCh := make(chan result, 1)
	go func() {
		res, err := r.LookupNow(ctx, q)
		nowCh <- result{res, err}
	}()
	// Give the waiter a moment to attach to the in-flight fetch, then release.
	time.Sleep(100 * time.Millisecond)
	close(fake.block)
	select {
	case out := <-nowCh:
		if out.err != nil {
			t.Fatalf("LookupNow: %v", out.err)
		}
		if !out.res.Found || out.res.Series != "Show" || out.res.Episode != "Pilot" {
			t.Fatalf("LookupNow = %+v, want Show/Pilot found", out.res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("LookupNow did not return after fetch released")
	}

	res2, err := r.Resolve(ctx, q)
	if err != nil {
		t.Fatalf("Resolve after fetch: %v", err)
	}
	if !res2.Found || res2.Episode != "Pilot" {
		t.Fatalf("Resolve after fetch = %+v, want Pilot found", res2)
	}
	if n := fake.calls.Load(); n != 1 {
		t.Fatalf("HTTP calls = %d, want exactly 1 (served from memory)", n)
	}
}

// Two concurrent Resolves on a miss must fire exactly one fetch.
func TestResolveSingleFlight(t *testing.T) {
	fake := &fakeSource{name: "fake", media: seriesMedia(), delay: 300 * time.Millisecond}
	r := New(Options{Dir: t.TempDir(), Sources: []Source{fake}, Timeout: 5 * time.Second})
	q := Query{Kind: KindSeries, Title: "Show", Season: 1, Episode: 2}
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Resolve(ctx, q); err != nil {
				t.Errorf("Resolve: %v", err)
			}
		}()
	}
	wg.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if n := fake.calls.Load(); n == 1 {
			break
		} else if n > 1 {
			t.Fatalf("HTTP calls = %d, want 1", n)
		}
		if time.Now().After(deadline) {
			t.Fatal("background fetch never ran")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// After the single fetch lands, Resolve serves from memory.
	deadline = time.Now().Add(5 * time.Second)
	for {
		res, _ := r.Resolve(ctx, q)
		if res.Found {
			if res.Episode != "Pilot" {
				t.Fatalf("Resolve = %+v, want Pilot", res)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Resolve never served from memory")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := fake.calls.Load(); n != 1 {
		t.Fatalf("HTTP calls = %d, want exactly 1", n)
	}
}

// mislabeledMedia mimics live AniList 21355: streamingEpisodes numbered
// 51..66 (a later season) served for an absolute episode-1 query.
func mislabeledMedia() *Media {
	return &Media{
		Source: "fake-first", SourceID: "21355",
		Kind: KindSeries, Title: "Re Zero",
		Episodes: []Episode{
			{Season: 0, Number: 51, Title: "Theatrical Malice"},
			{Season: 0, Number: 66, Title: "The Result of the Battle for Priestella"},
		},
		FetchedAt: time.Now().UTC(),
	}
}

func correctMedia() *Media {
	return &Media{
		Source: "fake-second", SourceID: "1",
		Kind: KindSeries, Title: "Re Zero",
		Episodes:  []Episode{{Season: 0, Number: 1, Title: "True S1E1"}},
		FetchedAt: time.Now().UTC(),
	}
}

// Number-exact: Numbers 51..66 must never answer Number 1 via positional
// index; Number 51 itself must still match.
func TestMatchEpisodeNeverPositional(t *testing.T) {
	eps := mislabeledMedia().Episodes
	if _, ok := matchEpisode(eps, Query{Kind: KindSeries, Title: "Re Zero", Episode: 1, Absolute: true}); ok {
		t.Fatal("matchEpisode(51..66, ep1) matched: positional short-circuit present")
	}
	if ep, ok := matchEpisode(eps, Query{Kind: KindSeries, Title: "Re Zero", Episode: 51, Absolute: true}); !ok || ep.Title != "Theatrical Malice" {
		t.Fatalf("matchEpisode(51..66, ep51) = %+v,%v; want Theatrical Malice", ep, ok)
	}
}

// Cross-source fallthrough: the mislabeled first source must be skipped and
// the second source having Number 1 must answer.
func TestCrossSourceFallthrough(t *testing.T) {
	first := &fakeSource{name: "fake-first", media: mislabeledMedia()}
	second := &fakeSource{name: "fake-second", media: correctMedia()}
	r := New(Options{Dir: t.TempDir(), Sources: []Source{first, second}, Timeout: 5 * time.Second})
	q := Query{Kind: KindSeries, Title: "Re Zero", Episode: 1, Absolute: true}
	res, err := r.LookupNow(context.Background(), q)
	if err != nil {
		t.Fatalf("LookupNow: %v", err)
	}
	if !res.Found || res.Episode != "True S1E1" {
		t.Fatalf("resolution = %+v, want True S1E1 found", res)
	}
	if res.Episode == "The Result of the Battle for Priestella" {
		t.Fatal("wrong title: fell back to positional Number 51+ entry")
	}
	if n := first.calls.Load(); n != 1 {
		t.Fatalf("first source calls = %d, want 1", n)
	}
	if n := second.calls.Load(); n != 1 {
		t.Fatalf("second source calls = %d, want 1 (fallthrough)", n)
	}
}

// No source having the Number must return Found=false (play path falls back
// to today's clean card), never a wrong title.
func TestCrossSourceNotFound(t *testing.T) {
	first := &fakeSource{name: "fake-first", media: mislabeledMedia()}
	second := &fakeSource{name: "fake-second", media: mislabeledMedia()}
	r := New(Options{Dir: t.TempDir(), Sources: []Source{first, second}, Timeout: 5 * time.Second})
	q := Query{Kind: KindSeries, Title: "Re Zero", Episode: 1, Absolute: true}
	res, err := r.LookupNow(context.Background(), q)
	if err != nil {
		t.Fatalf("LookupNow: %v", err)
	}
	if res.Found {
		t.Fatalf("resolution = %+v, want Found=false", res)
	}
	if res.Episode == "The Result of the Battle for Priestella" {
		t.Fatal("wrong title cached/returned for missing episode")
	}
}

// Source order: TVmaze first for Absolute and for !Absolute series queries.
func TestOrderedSourcesTVMazeFirst(t *testing.T) {
	mk := func(name string) *fakeSource { return &fakeSource{name: name} }
	r := New(Options{
		Dir:     t.TempDir(),
		Sources: []Source{mk("anilist"), mk("jikan"), mk("tvmaze"), mk("itunes")},
		Timeout: 5 * time.Second,
	})
	for _, q := range []Query{
		{Kind: KindSeries, Title: "Show", Episode: 1, Absolute: true},
		{Kind: KindSeries, Title: "Show", Season: 1, Episode: 1},
		{Kind: KindSeries, Title: "Show", Episode: 3, Absolute: true},
	} {
		got := r.orderedSources(q)
		if len(got) < 3 || got[0].Name() != "tvmaze" || got[1].Name() != "anilist" || got[2].Name() != "jikan" {
			names := make([]string, len(got))
			for i, s := range got {
				names[i] = s.Name()
			}
			t.Fatalf("orderedSources(%+v) = %v, want [tvmaze anilist jikan ...]", q, names)
		}
	}
}
