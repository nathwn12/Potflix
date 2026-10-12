package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"potflix/internal/store"
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

// TestResolutionButtonsSeriesAndMovie proves button labels, https forcing,
// the <=2 cap, and anime/movie variants.
func TestResolutionButtonsSeriesAndMovie(t *testing.T) {
	series := &fakeSource{name: "tvmaze", media: &Media{
		Source: "tvmaze", SourceID: "1",
		Kind: KindSeries, Title: "Show", Year: 2008,
		Genres: "Drama " + string(rune(0xb7)) + " Crime", Length: "45 min",
		ShowURL: "http://example.com/show", EpisodeURL: "http://example.com/ep1",
		Artwork:   "https://example.com/poster.jpg",
		Episodes:  []Episode{{Season: 1, Number: 1, Title: "Pilot", URL: "http://example.com/ep1", Length: "45 min", Artwork: "https://example.com/still.jpg"}},
		FetchedAt: time.Now().UTC(),
	}}
	r := New(Options{Dir: t.TempDir(), Sources: []Source{series}, Timeout: 5 * time.Second})
	res, err := r.LookupNow(context.Background(), Query{Kind: KindSeries, Title: "Show", Season: 1, Episode: 1})
	if err != nil || !res.Found {
		t.Fatalf("series LookupNow = %+v, %v", res, err)
	}
	if res.Genres != "Drama "+string(rune(0xb7))+" Crime" || res.Length != "45 min" || res.Year != 2008 || res.IsMovie {
		t.Fatalf("series fields = %+v", res)
	}
	if len(res.Buttons) != 2 || res.Buttons[0].Label != "View Episode" || res.Buttons[1].Label != "View Show" {
		t.Fatalf("series buttons = %+v", res.Buttons)
	}
	for _, b := range res.Buttons {
		if len(b.URL) < len("https://") || b.URL[:len("https://")] != "https://" {
			t.Fatalf("series button not https: %+v", b)
		}
	}
	// Cover rework: series poster wins, episode still never overrides.
	if res.Artwork != "https://example.com/poster.jpg" {
		t.Fatalf("series artwork = %q, want poster", res.Artwork)
	}

	anime := &fakeSource{name: "anilist", media: &Media{
		Source: "anilist", SourceID: "21",
		Kind: KindSeries, Title: "Anime", Year: 2016,
		Genres: "Action", Length: "24 min", ShowURL: "https://myanimelist.net/anime/21",
		Episodes:  []Episode{{Number: 1, Title: "First", URL: "http://example.com/stream1"}},
		FetchedAt: time.Now().UTC(),
	}}
	r2 := New(Options{Dir: t.TempDir(), Sources: []Source{anime}, Timeout: 5 * time.Second})
	res2, err := r2.LookupNow(context.Background(), Query{Kind: KindSeries, Title: "Anime", Episode: 1, Absolute: true})
	if err != nil || !res2.Found {
		t.Fatalf("anime LookupNow = %+v, %v", res2, err)
	}
	if len(res2.Buttons) != 2 || res2.Buttons[0].Label != "View Episode" || res2.Buttons[1].Label != "View Anime" {
		t.Fatalf("anime buttons = %+v", res2.Buttons)
	}

	movie := &fakeSource{name: "itunes", media: &Media{
		Source: "itunes", SourceID: "Blade Runner 2049",
		Kind: KindMovie, Title: "Blade Runner 2049", Year: 2017,
		Genres: "Sci-Fi", Length: "2h 19m",
		MovieURL: "http://itunes.apple.com/movie/id123", TrailerURL: "ftp://example.com/trailer",
		Artwork:   "https://example.com/movie.jpg",
		FetchedAt: time.Now().UTC(),
	}}
	r3 := New(Options{Dir: t.TempDir(), Sources: []Source{movie}, Timeout: 5 * time.Second})
	res3, err := r3.LookupNow(context.Background(), Query{Kind: KindMovie, Title: "Blade Runner 2049", Year: 2017})
	if err != nil || !res3.Found {
		t.Fatalf("movie LookupNow = %+v, %v", res3, err)
	}
	if !res3.IsMovie || res3.Year != 2017 {
		t.Fatalf("movie fields = %+v", res3)
	}
	// ftp trailer drops: only the https-forced movie button remains.
	if len(res3.Buttons) != 1 || res3.Buttons[0].Label != "View Movie" {
		t.Fatalf("movie buttons = %+v, want single View Movie", res3.Buttons)
	}
	if res3.Buttons[0].URL != "https://itunes.apple.com/movie/id123" {
		t.Fatalf("movie button URL = %q", res3.Buttons[0].URL)
	}
}

// TestNegativePersistedAndColdHitNoNetwork proves a not-found lookup
// persists a negative entry (file exists, parses, NotFound=true) and that
// a second lookup in a NEW resolver (cold mem) with the same cache dir
// serves the miss with zero provider calls.
func TestNegativePersistedAndColdHitNoNetwork(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	q := Query{Kind: KindSeries, Title: "No Such Show Ever", Season: 1, Episode: 1}
	miss := &fakeSource{name: "fake"} // nil media = not found
	r1 := New(Options{Dir: dir, Sources: []Source{miss}, Timeout: 5 * time.Second})
	res, err := r1.LookupNow(ctx, q)
	if err != nil {
		t.Fatalf("run 1 LookupNow: %v", err)
	}
	if res.Found {
		t.Fatalf("run 1 = %+v, want miss", res)
	}
	if n := miss.calls.Load(); n != 1 {
		t.Fatalf("run 1 provider calls = %d, want 1", n)
	}
	// The negative record is stored under the same key.
	key := cacheKey(q)
	data, ok := cacheRecord(dir, key)
	if !ok {
		t.Fatal("negative entry missing from the store, want a stored negative")
	}
	var cf cacheFile
	if err := json.Unmarshal(data, &cf); err != nil {
		t.Fatalf("negative entry does not parse: %v", err)
	}
	if !cf.NotFound {
		t.Fatalf("negative entry NotFound = false, want true (%s)", string(data))
	}
	if cf.Key != key || cf.Schema != cacheSchema {
		t.Fatalf("negative entry identity = %+v, want key %q schema %d", cf, key, cacheSchema)
	}

	// Cold mem: a NEW resolver over the same dir must not touch the network.
	counting := &fakeSource{name: "fake"}
	r2 := New(Options{Dir: dir, Sources: []Source{counting}, Timeout: 5 * time.Second})
	res2, err := r2.LookupNow(ctx, q)
	if err != nil {
		t.Fatalf("run 2 LookupNow: %v", err)
	}
	if res2.Found {
		t.Fatalf("run 2 = %+v, want cached miss", res2)
	}
	if n := counting.calls.Load(); n != 0 {
		t.Fatalf("run 2 provider calls = %d, want 0 (negative-cache hit, no network)", n)
	}
	// The blocking miss reports fromCache=false, like every other miss.
	if _, fromCache, err := r2.LookupNowForceSrc(ctx, q, false); err != nil || fromCache {
		t.Fatalf("negative hit fromCache = %t,%v; want false,nil", fromCache, err)
	}
	if n := counting.calls.Load(); n != 0 {
		t.Fatalf("provenance probe provider calls = %d, want still 0", n)
	}
}

// TestNegativeForceReattemptsAndReplaces proves force=true bypasses the
// persisted miss, re-attempts the provider, and replaces the negative
// entry with the positive one on success.
func TestNegativeForceReattemptsAndReplaces(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	q := Query{Kind: KindSeries, Title: "Late Bloomer", Season: 1, Episode: 2}
	miss := &fakeSource{name: "fake"}
	r1 := New(Options{Dir: dir, Sources: []Source{miss}, Timeout: 5 * time.Second})
	if res, err := r1.LookupNow(ctx, q); err != nil || res.Found {
		t.Fatalf("seed miss = %+v,%v; want miss", res, err)
	}
	key := cacheKey(q)
	if e, ok := cacheLoadEntry(dir, key); !ok || !e.NotFound {
		t.Fatalf("seed negative entry = %+v,%v; want negative hit", e, ok)
	}

	now := &fakeSource{name: "fake", media: seriesMedia()}
	r2 := New(Options{Dir: dir, Sources: []Source{now}, Timeout: 5 * time.Second})
	res, err := r2.LookupNowForce(ctx, q, true)
	if err != nil {
		t.Fatalf("force LookupNow: %v", err)
	}
	if !res.Found || res.Episode != "Pilot" {
		t.Fatalf("force = %+v, want Pilot found", res)
	}
	if n := now.calls.Load(); n != 1 {
		t.Fatalf("force provider calls = %d, want 1 (re-attempted)", n)
	}
	// The negative file is now a positive entry.
	e, ok := cacheLoadEntry(dir, key)
	if !ok || e.NotFound || e.Media == nil {
		t.Fatalf("replaced entry = %+v,%v; want positive hit", e, ok)
	}
	if _, ok := cacheLoad(dir, key); !ok {
		t.Fatal("replaced entry should load as a positive hit")
	}
}

// TestNegativeExpiredReattempted proves an expired negative entry is
// treated as absent: the provider IS called again.
func TestNegativeExpiredReattempted(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	q := Query{Kind: KindSeries, Title: "Stale Miss Show", Season: 1, Episode: 1}
	key := cacheKey(q)
	old := time.Now().UTC().Add(-negativeTTL - time.Hour).Truncate(time.Millisecond)
	data, err := json.Marshal(cacheFile{Key: key, Schema: cacheSchema, NotFound: true, CheckedAt: old})
	if err != nil {
		t.Fatal(err)
	}
	storeRawRecord(t, dir, key, data)
	fake := &fakeSource{name: "fake"}
	r := New(Options{Dir: dir, Sources: []Source{fake}, Timeout: 5 * time.Second})
	res, err := r.LookupNow(ctx, q)
	if err != nil {
		t.Fatalf("LookupNow: %v", err)
	}
	if res.Found {
		t.Fatalf("expired-negative lookup = %+v, want miss (re-attempted, still missing)", res)
	}
	if n := fake.calls.Load(); n != 1 {
		t.Fatalf("provider calls = %d, want 1 (expired entry re-attempted)", n)
	}
}

// TestProviderErrorPersistsNothing proves a transport error is transient,
// never a confirmed not-found: it returns ErrLookupUnavailable (not nil),
// persists no negative file and sets no in-memory suppression, so the next
// lookup re-attempts the provider.
func TestProviderErrorPersistsNothing(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	q := Query{Kind: KindSeries, Title: "Flaky Show", Season: 1, Episode: 2}
	key := cacheKey(q)
	boom := &fakeSource{name: "fake", err: errors.New("boom")}
	r := New(Options{Dir: dir, Sources: []Source{boom}, Timeout: 5 * time.Second})
	res, err := r.LookupNow(ctx, q)
	if !errors.Is(err, ErrLookupUnavailable) {
		t.Fatalf("LookupNow err = %v, want ErrLookupUnavailable", err)
	}
	if res.Found {
		t.Fatalf("error lookup = %+v, want miss", res)
	}
	if CacheHas(dir, key) {
		t.Fatalf("provider error persisted a record, want nothing")
	}
	if CacheLive(dir, key) {
		t.Fatal("CacheLive after provider error = true, want false")
	}
	r.mu.Lock()
	_, suppressed := r.neg[key]
	r.mu.Unlock()
	if suppressed {
		t.Fatal("provider error set in-memory suppression, want none")
	}
	// A second lookup must re-attempt (no suppression anywhere).
	if _, err := r.LookupNow(ctx, q); !errors.Is(err, ErrLookupUnavailable) {
		t.Fatalf("second LookupNow err = %v, want ErrLookupUnavailable", err)
	}
	if n := boom.calls.Load(); n != 2 {
		t.Fatalf("provider calls = %d, want 2 (no negative cached)", n)
	}
}

// TestProviderTimeoutPersistsNothing proves a timed-out fetch persists
// nothing: no record is stored and the next lookup re-attempts.
func TestProviderTimeoutPersistsNothing(t *testing.T) {
	dir := t.TempDir()
	q := Query{Kind: KindSeries, Title: "Slow Show", Season: 1, Episode: 2}
	key := cacheKey(q)
	slow := &fakeSource{name: "fake", delay: 500 * time.Millisecond}
	r := New(Options{Dir: dir, Sources: []Source{slow}, Timeout: 5 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := r.LookupNow(ctx, q)
	if err == nil {
		t.Fatal("timed-out LookupNow err = nil, want ctx error")
	}
	if CacheHas(dir, key) {
		t.Fatalf("timed-out fetch stored a record, want nothing")
	}
	if CacheLive(dir, key) {
		t.Fatal("CacheLive after timeout = true, want false")
	}
}

// TestFailedForcedRefreshPreservesPositive proves a failed forced refresh
// (provider error) leaves an existing stored positive byte-identical and the
// positive still serves afterwards.
func TestFailedForcedRefreshPreservesPositive(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	q := Query{Kind: KindSeries, Title: "Keep Me", Season: 1, Episode: 2}
	key := cacheKey(q)
	media := seriesMedia()
	if err := cacheStore(dir, key, media); err != nil {
		t.Fatalf("seed cacheStore: %v", err)
	}
	before, ok := cacheRecord(dir, key)
	if !ok {
		t.Fatal("seed record missing from the store")
	}
	boom := &fakeSource{name: "fake", err: errors.New("transport down")}
	r := New(Options{Dir: dir, Sources: []Source{boom}, Timeout: 5 * time.Second})
	// Warm the in-memory positive from disk (no provider call).
	if res, err := r.LookupNow(ctx, q); err != nil || !res.Found {
		t.Fatalf("warm LookupNow = %+v,%v; want found (from disk)", res, err)
	}
	// Failed forced refresh: transient, must write nothing. The error is
	// ErrLookupUnavailable, never a silent miss.
	res, err := r.LookupNowForce(ctx, q, true)
	if !errors.Is(err, ErrLookupUnavailable) {
		t.Fatalf("force LookupNow err = %v, want ErrLookupUnavailable", err)
	}
	if res.Found {
		t.Fatalf("failed force = %+v, want miss", res)
	}
	after, ok := cacheRecord(dir, key)
	if !ok {
		t.Fatal("stored record vanished from the store")
	}
	if string(after) != string(before) {
		t.Fatalf("stored positive clobbered by failed refresh:\nbefore %s\nafter  %s", string(before), string(after))
	}
	if !CacheLive(dir, key) {
		t.Fatal("CacheLive after failed force = false, want true (positive intact)")
	}
	// The positive still serves (memory restored/intact).
	res2, err := r.LookupNow(ctx, q)
	if err != nil || !res2.Found || res2.Episode != "Pilot" {
		t.Fatalf("post-force LookupNow = %+v,%v; want Pilot found", res2, err)
	}
}

// TestCancelledBackgroundFetchStoresNothing proves a cancelled/timed-out
// background fetch stores no negative record and sets no suppression.
func TestCancelledBackgroundFetchStoresNothing(t *testing.T) {
	dir := t.TempDir()
	q := Query{Kind: KindSeries, Title: "Bg Cancel", Season: 1, Episode: 2}
	key := cacheKey(q)
	slow := &fakeSource{name: "fake", delay: 500 * time.Millisecond}
	// fetchCtx timeout = timeout*4 = 80ms, well under the 500ms delay.
	r := New(Options{Dir: dir, Sources: []Source{slow}, Timeout: 20 * time.Millisecond})
	done := make(chan struct{})
	r.mu.Lock()
	r.inflight[key] = done
	r.mu.Unlock()
	go r.backgroundFetch(context.Background(), q, key, done)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("backgroundFetch never finished")
	}
	if CacheHas(dir, key) {
		t.Fatalf("cancelled backgroundFetch stored a record, want nothing")
	}
	r.mu.Lock()
	_, suppressed := r.neg[key]
	r.mu.Unlock()
	if suppressed {
		t.Fatal("cancelled backgroundFetch set suppression, want none")
	}
}

// TestNegativeBackoffBoundedByTTL proves the in-memory suppression never
// outlives the stored TTL expiry: a near-expiry miss repopulates mem only
// to CheckedAt+TTL, not now+backoff.
func TestNegativeBackoffBoundedByTTL(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	q := Query{Kind: KindSeries, Title: "Near Expiry", Season: 1, Episode: 1}
	key := cacheKey(q)
	checkedAt := time.Now().UTC().Add(-negativeTTL + 2*time.Minute).Truncate(time.Millisecond)
	data, err := json.Marshal(cacheFile{Key: key, Schema: cacheSchema, NotFound: true, CheckedAt: checkedAt})
	if err != nil {
		t.Fatal(err)
	}
	storeRawRecord(t, dir, key, data)
	counting := &fakeSource{name: "fake"}
	r := New(Options{Dir: dir, Sources: []Source{counting}, Timeout: 5 * time.Second})
	if res, err := r.LookupNow(ctx, q); err != nil || res.Found {
		t.Fatalf("near-expiry LookupNow = %+v,%v; want cached miss", res, err)
	}
	if n := counting.calls.Load(); n != 0 {
		t.Fatalf("provider calls = %d, want 0 (fresh negative, no network)", n)
	}
	r.mu.Lock()
	exp, ok := r.neg[key]
	r.mu.Unlock()
	if !ok {
		t.Fatal("near-expiry miss set no in-memory suppression, want bounded one")
	}
	ttlExp := checkedAt.Add(negativeTTL)
	if exp.After(ttlExp.Add(5 * time.Second)) {
		t.Fatalf("in-memory expiry %v outlives TTL expiry %v", exp, ttlExp)
	}
	if !exp.Before(time.Now().Add(negativeBackoff)) {
		t.Fatalf("in-memory expiry %v not bounded (want well under now+10m)", exp)
	}
}

// TestTransientVsDefinitiveMissDistinguishable proves the honest-counters
// contract at the resolver boundary: a transient provider failure returns
// ErrLookupUnavailable (wrapping the underlying cause) and stores nothing,
// while a definitive miss (every source answered, none has the title)
// returns a nil error and stores exactly one negative.
func TestTransientVsDefinitiveMissDistinguishable(t *testing.T) {
	dir := t.TempDir()
	countRecords := func() int {
		if !cacheExists(dir) {
			return 0
		}
		n := 0
		if err := withCache(dir, func(db *store.DB) error {
			n = db.Len()
			return nil
		}); err != nil {
			t.Fatalf("count stored records: %v", err)
		}
		return n
	}
	ctx := context.Background()

	// Transient: the provider errored, so the outcome is unknown.
	boom := errors.New("boom")
	flaky := New(Options{Dir: dir, Sources: []Source{&fakeSource{name: "fake", err: boom}}, Timeout: 5 * time.Second})
	qFlaky := Query{Kind: KindSeries, Title: "Flaky Show", Season: 1, Episode: 2}
	res, err := flaky.LookupNow(ctx, qFlaky)
	if !errors.Is(err, ErrLookupUnavailable) {
		t.Fatalf("transient err = %v, want ErrLookupUnavailable", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("transient err = %q, want underlying cause propagated", err.Error())
	}
	if res.Found {
		t.Fatalf("transient res = %+v, want miss", res)
	}
	if n := countRecords(); n != 0 {
		t.Fatalf("transient stored %d records, want 0", n)
	}

	// Definitive: every source answered and none has the title.
	empty := New(Options{Dir: dir, Sources: []Source{}, Timeout: 5 * time.Second})
	qMissing := Query{Kind: KindSeries, Title: "Missing Show", Season: 1, Episode: 2}
	res, err = empty.LookupNow(ctx, qMissing)
	if err != nil {
		t.Fatalf("definitive miss err = %v, want nil", err)
	}
	if res.Found {
		t.Fatalf("definitive miss res = %+v, want miss", res)
	}
	if n := countRecords(); n != 1 {
		t.Fatalf("definitive miss left %d records, want exactly 1 negative", n)
	}
	if !CacheLive(dir, cacheKey(qMissing)) {
		t.Fatal("CacheLive after definitive miss = false, want true")
	}
}

// TestProbedEntryDeletedNotServed proves an entry invalidated after
// ProbeCache warmed memory is never served from that memory. ClearCache is the
// invalidation path now (the cache is one store file keyed by the raw key, so
// there is no per-key file to delete), and it removes the record the probe
// warmed: an invalidated positive resolves Found=false (never the warmed
// title) and its lookup re-attempts the fetch, and an invalidated fresh
// negative does not suppress its fetch (the lookup proceeds to the sources
// instead of reporting a warmed miss).
func TestProbedEntryDeletedNotServed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	// Invalidated positive: Resolve must not serve the warmed title.
	q := Query{Kind: KindSeries, Title: "Probe Del", Season: 1, Episode: 1}
	key := cacheKey(q)
	m := &Media{Source: "tvmaze", SourceID: "1", Kind: KindSeries, Title: "Probe Del",
		Episodes: []Episode{{Season: 1, Number: 1, Title: "Pilot"}}, FetchedAt: time.Now().UTC()}
	if err := cacheStore(dir, key, m); err != nil {
		t.Fatal(err)
	}
	r := New(Options{Dir: dir, Sources: []Source{&fakeSource{name: "fake"}}, Offline: true})
	if !r.ProbeCache(key) {
		t.Fatal("ProbeCache on a live positive = false, want true")
	}
	if err := ClearCache(dir); err != nil {
		t.Fatalf("ClearCache: %v", err)
	}
	if CacheHas(dir, key) {
		t.Fatal("cleared cache still holds the probed record, want it gone")
	}
	if res, err := r.Resolve(ctx, q); err != nil || res.Found {
		t.Fatalf("Resolve after invalidation = %+v,%v; want not-found (never warmed memory)", res, err)
	}
	// Same, via the blocking lookup on a fresh resolver: the fetch is
	// re-attempted instead of serving warmed memory.
	if err := cacheStore(dir, key, m); err != nil {
		t.Fatal(err)
	}
	fake := &fakeSource{name: "fake", err: errors.New("boom")}
	r2 := New(Options{Dir: dir, Sources: []Source{fake}})
	if !r2.ProbeCache(key) {
		t.Fatal("ProbeCache on a live positive = false, want true")
	}
	if err := ClearCache(dir); err != nil {
		t.Fatalf("ClearCache: %v", err)
	}
	if res, _, err := r2.LookupNowForceSrc(ctx, q, false); err == nil || res.Found {
		t.Fatalf("LookupNowForceSrc after invalidation = %+v,%v; want a transient error (fetch re-attempted)", res, err)
	}
	if got := fake.calls.Load(); got == 0 {
		t.Fatal("lookup after invalidation made no source call; want the miss re-attempted from store/network")
	}
	// Invalidated fresh negative: the lookup must fetch, not report a warmed
	// miss.
	nq := Query{Kind: KindSeries, Title: "Probe Neg", Season: 1, Episode: 1}
	nkey := cacheKey(nq)
	if err := cacheStoreNegative(dir, nkey); err != nil {
		t.Fatal(err)
	}
	nfake := &fakeSource{name: "fake"}
	r3 := New(Options{Dir: dir, Sources: []Source{nfake}})
	if !r3.ProbeCache(nkey) {
		t.Fatal("ProbeCache on a fresh negative = false, want true")
	}
	if err := ClearCache(dir); err != nil {
		t.Fatalf("ClearCache: %v", err)
	}
	if res, _, err := r3.LookupNowForceSrc(ctx, nq, false); err != nil || res.Found {
		t.Fatalf("LookupNowForceSrc after negative invalidation = %+v,%v; want a clean miss after a real fetch", res, err)
	}
	if got := nfake.calls.Load(); got == 0 {
		t.Fatal("lookup after negative invalidation made no source call; want the fetch, not a warmed miss")
	}
}
