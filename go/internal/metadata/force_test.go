package metadata

import (
	"context"
	"os"
	"testing"
	"time"
)

func cacheFileModTime(t *testing.T, dir string) time.Time {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("cache files = %d, want 1", len(entries))
	}
	fi, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime()
}

// TestLookupNowForceRewritesCache proves the force path bypasses the cached
// entry and rewrites it, while a non-force second run is a strict no-op
// (served from disk: zero fetches, cache file untouched).
func TestLookupNowForceRewritesCache(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	q := Query{Kind: KindSeries, Title: "Show", Season: 1, Episode: 1}
	mediaWith := func(tag string) *Media {
		return &Media{
			Source: "fake", SourceID: "1",
			Kind: KindSeries, Title: "Show",
			Episodes:  []Episode{{Season: 1, Number: 1, Title: tag}},
			FetchedAt: time.Now().UTC(),
		}
	}

	fake1 := &fakeSource{name: "fake", media: mediaWith("V1")}
	r1 := New(Options{Dir: dir, Sources: []Source{fake1}, Timeout: 5 * time.Second})
	res1, err := r1.LookupNow(ctx, q)
	if err != nil || !res1.Found || res1.Episode != "V1" {
		t.Fatalf("run 1 = %+v,%v; want V1 found", res1, err)
	}
	if n := fake1.calls.Load(); n != 1 {
		t.Fatalf("run 1 fetches = %d, want 1", n)
	}
	mtime1 := cacheFileModTime(t, dir)

	// Run 2, non-force, through a FRESH resolver (proves the disk entry is
	// the no-op source): still V1, zero fetches, file untouched.
	fake2 := &fakeSource{name: "fake", media: mediaWith("V2")}
	r2 := New(Options{Dir: dir, Sources: []Source{fake2}, Timeout: 5 * time.Second})
	res2, err := r2.LookupNow(ctx, q)
	if err != nil || !res2.Found || res2.Episode != "V1" {
		t.Fatalf("run 2 (non-force) = %+v,%v; want cached V1 found", res2, err)
	}
	if n := fake2.calls.Load(); n != 0 {
		t.Errorf("run 2 fetches = %d, want 0 (disk no-op)", n)
	}
	if mt := cacheFileModTime(t, dir); !mt.Equal(mtime1) {
		t.Errorf("run 2 rewrote the cache entry without force (mtime %v != %v)", mt, mtime1)
	}

	// Run 3, force: refetch and rewrite.
	time.Sleep(60 * time.Millisecond) // separate mtimes on coarse filesystems
	res3, err := r2.LookupNowForce(ctx, q, true)
	if err != nil || !res3.Found || res3.Episode != "V2" {
		t.Fatalf("run 3 (force) = %+v,%v; want refetched V2 found", res3, err)
	}
	if n := fake2.calls.Load(); n != 1 {
		t.Errorf("run 3 fetches = %d, want 1 (forced refetch)", n)
	}
	if mt := cacheFileModTime(t, dir); !mt.After(mtime1) {
		t.Errorf("run 3 did not rewrite the cache entry (mtime %v, want after %v)", mt, mtime1)
	}

	// force=false stays exactly LookupNow: served from memory, no fetch.
	if res4, err := r2.LookupNowForce(ctx, q, false); err != nil || !res4.Found || res4.Episode != "V2" {
		t.Fatalf("force=false = %+v,%v; want V2 found", res4, err)
	}
	if n := fake2.calls.Load(); n != 1 {
		t.Errorf("fetches after force=false = %d, want still 1", n)
	}
}

// TestLookupNowForceSrcProvenance proves the provenance bit: a first
// non-force lookup fetches (fromCache=false), a repeat is served from
// cache (fromCache=true), and force always fetches (fromCache=false),
// while LookupNow/LookupNowForce keep their historical results.
func TestLookupNowForceSrcProvenance(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	q := Query{Kind: KindSeries, Title: "Show", Season: 1, Episode: 1}
	media := &Media{
		Source: "fake", SourceID: "1",
		Kind: KindSeries, Title: "Show",
		Episodes:  []Episode{{Season: 1, Number: 1, Title: "V1"}},
		FetchedAt: time.Now().UTC(),
	}
	fake := &fakeSource{name: "fake", media: media}
	r := New(Options{Dir: dir, Sources: []Source{fake}, Timeout: 5 * time.Second})

	res1, cached1, err := r.LookupNowForceSrc(ctx, q, false)
	if err != nil || !res1.Found || cached1 {
		t.Errorf("first lookup = %+v cached=%t %v; want found, cached=false", res1, cached1, err)
	}
	res2, cached2, err := r.LookupNowForceSrc(ctx, q, false)
	if err != nil || !res2.Found || !cached2 {
		t.Errorf("repeat lookup = %+v cached=%t %v; want found, cached=true", res2, cached2, err)
	}
	if n := fake.calls.Load(); n != 1 {
		t.Errorf("fetches after repeat = %d, want 1 (repeat served from cache)", n)
	}
	_, cached3, err := r.LookupNowForceSrc(ctx, q, true)
	if err != nil || cached3 {
		t.Errorf("force lookup cached=%t %v; want cached=false", cached3, err)
	}
	if n := fake.calls.Load(); n != 2 {
		t.Errorf("fetches after force = %d, want 2 (forced refetch)", n)
	}
	// Delegating wrappers keep their historical behavior.
	if res, err := r.LookupNow(ctx, q); err != nil || !res.Found {
		t.Errorf("LookupNow = %+v,%v; want found", res, err)
	}
	if res, err := r.LookupNowForce(ctx, q, false); err != nil || !res.Found {
		t.Errorf("LookupNowForce(false) = %+v,%v; want found", res, err)
	}
}
