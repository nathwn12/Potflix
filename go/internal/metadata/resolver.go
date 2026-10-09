package metadata

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// negativeBackoff keeps a failed key from being hammered. In-memory only;
// the on-disk cache stays additive forever.
const negativeBackoff = 10 * time.Minute

// Resolver resolves Queries to episode titles without ever blocking on the
// network. It is safe for concurrent use.
type Resolver struct {
	mu       sync.Mutex
	mem      map[string]*Media
	inflight map[string]chan struct{}
	neg      map[string]time.Time
	dir      string
	timeout  time.Duration
	sources  []Source
	logf     func(format string, args ...any)
	offline  bool
}

func makeDefaultSources(timeout time.Duration) []Source {
	c := newHTTPClient(timeout)
	return []Source{
		&tvmazeSource{base: "https://api.tvmaze.com", client: c},
		&anilistSource{base: "https://graphql.anilist.co", client: c},
		&jikanSource{base: "https://api.jikan.moe/v4", client: c},
		&itunesSource{base: "https://itunes.apple.com", client: c},
	}
}

// makeDefaultSourcesWithKeys prepends the keyed TMDB source at the FRONT
// when KeyProvider yields a non-blank tmdb key; with a nil provider or a
// blank key the list is exactly makeDefaultSources (byte-identical order).
func makeDefaultSourcesWithKeys(timeout time.Duration, kp func(provider string) string) []Source {
	key := ""
	if kp != nil {
		key = strings.TrimSpace(kp("tmdb"))
	}
	if key == "" {
		return makeDefaultSources(timeout)
	}
	c := newHTTPClient(timeout)
	return []Source{
		&tmdbSource{base: "https://api.themoviedb.org", key: key, client: c},
		&tvmazeSource{base: "https://api.tvmaze.com", client: c},
		&anilistSource{base: "https://graphql.anilist.co", client: c},
		&jikanSource{base: "https://api.jikan.moe/v4", client: c},
		&itunesSource{base: "https://itunes.apple.com", client: c},
	}
}

// DefaultSources returns the built-in providers. Ordering by Kind is handled
// by the Resolver. An optional KeyProvider prepends the keyed TMDB source
// when it yields a non-blank tmdb key; without one the list is exactly the
// historical keyless order.
func DefaultSources(keyProvider ...func(provider string) string) []Source {
	var kp func(provider string) string
	if len(keyProvider) > 0 {
		kp = keyProvider[0]
	}
	return makeDefaultSourcesWithKeys(0, kp)
}

// New builds a Resolver. A nil Sources list selects DefaultSources() with
// the Options KeyProvider applied.
func New(opts Options) *Resolver {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultHTTPTimeout()
	}
	srcs := opts.Sources
	if srcs == nil {
		srcs = makeDefaultSourcesWithKeys(timeout, opts.KeyProvider)
	}
	cp := make([]Source, len(srcs))
	copy(cp, srcs)
	return &Resolver{
		mem:      make(map[string]*Media),
		inflight: make(map[string]chan struct{}),
		neg:      make(map[string]time.Time),
		dir:      resolveCacheDir(opts.Dir),
		timeout:  timeout,
		sources:  cp,
		logf:     opts.Logf,
		offline:  opts.Offline,
	}
}

func (r *Resolver) log(format string, args ...any) {
	if r.logf != nil {
		r.logf(format, args...)
	}
}

// orderedSources ranks providers per Kind; unknown names keep their relative
// order at the end so injected test sources are still tried.
func (r *Resolver) orderedSources(q Query) []Source {
	var pref map[string]int
	switch {
	case q.Kind == KindMovie:
		// TMDB first when keyed (it is only ever present then); the
		// keyless order behind it is untouched.
		pref = map[string]int{"tmdb": 0, "itunes": 1}
	default:
		// TVmaze first for every series query: its season/episode tables
		// are authoritative, while AniList streamingEpisodes are mislabeled
		// for absolute anime numbering (Re:ZERO media 21355 serves 51-66
		// for an episode-1 query - a wrong title is worse than no title)
		// and Jikan is rate-limited. This holds for Absolute and for
		// !Absolute alike. A keyed TMDB source outranks all of them.
		pref = map[string]int{"tmdb": 0, "tvmaze": 1, "anilist": 2, "jikan": 3}
	}
	out := make([]Source, len(r.sources))
	copy(out, r.sources)
	sort.SliceStable(out, func(i, j int) bool {
		ri, ok := pref[out[i].Name()]
		if !ok {
			ri = 100
		}
		rj, ok := pref[out[j].Name()]
		if !ok {
			rj = 100
		}
		return ri < rj
	})
	return out
}

// mediaAnswers reports whether m can answer q. Movies answer on presence;
// series require a number-exact episode match.
func mediaAnswers(m *Media, q Query) bool {
	if m == nil {
		return false
	}
	if m.Kind == KindMovie || q.Kind == KindMovie {
		return true
	}
	_, ok := matchEpisode(m.Episodes, q)
	return ok
}

func (r *Resolver) fetchMedia(ctx context.Context, q Query) (*Media, bool) {
	sawError := false
	for _, s := range r.orderedSources(q) {
		m, err := s.Find(ctx, q)
		if err != nil {
			r.log("metadata: source %s error: %v", s.Name(), err)
			sawError = true
			continue
		}
		if m == nil {
			continue
		}
		if m.Source == "" {
			m.Source = s.Name()
		}
		if m.Kind == KindMovie || q.Kind == KindMovie {
			return m, true
		}
		if _, ok := matchEpisode(m.Episodes, q); ok {
			return m, true
		}
		// The show matched but its Numbers lack the requested episode:
		// fall through to the next source rather than caching/returning
		// a wrong (positional) title.
		r.log("metadata: source %s lacks episode %d for %q, trying next", s.Name(), q.Episode, q.Title)
	}
	if sawError {
		return nil, false
	}
	if ctx.Err() != nil {
		return nil, false
	}
	return nil, true
}

// matchEpisode is number-exact: only Number == q.Episode (plus
// Season == q.Season when q.Season > 0) answers. It never returns a
// different episode via positional index - a wrong title is worse than no
// title (AniList 21355 serves Numbers 51-66 for an episode-1 query).
func matchEpisode(eps []Episode, q Query) (Episode, bool) {
	if len(eps) == 0 || q.Episode <= 0 {
		return Episode{}, false
	}
	if q.Season > 0 {
		for _, e := range eps {
			if e.Season == q.Season && e.Number == q.Episode {
				return e, true
			}
		}
		return Episode{}, false
	}
	for _, e := range eps {
		if e.Number == q.Episode {
			return e, true
		}
	}
	return Episode{}, false
}

func resolveFromMedia(m *Media, q Query) Resolution {
	isMovie := m.Kind == KindMovie || q.Kind == KindMovie
	res := Resolution{Series: m.Title, Season: q.Season, Number: q.Episode, Artwork: m.Artwork, Synopsis: capSynopsis(stripHTML(m.Synopsis)), Genres: m.Genres, Length: m.Length, Year: m.Year, IsMovie: isMovie}
	if isMovie {
		res.Season = 0
		res.Number = 0
		res.Found = true
		res.Buttons = resolutionButtons(m, Episode{}, false, true)
		return res
	}
	if ep, ok := matchEpisode(m.Episodes, q); ok {
		res.Season = ep.Season
		res.Number = ep.Number
		res.Episode = ep.Title
		// Artwork is ALWAYS the series poster: episode stills never override
		// (cover rework - stills look awkward/unofficial).
		if s := firstNonEmpty(ep.Synopsis, m.Synopsis); s != "" {
			res.Synopsis = capSynopsis(stripHTML(s))
		}
		if strings.TrimSpace(ep.Length) != "" {
			res.Length = strings.TrimSpace(ep.Length)
		}
		res.Buttons = resolutionButtons(m, ep, true, false)
		res.Found = true
		return res
	}
	// Every source lacked the requested Number: not found, so the play
	// path falls back to today's clean card. Never report a wrong title.
	res.Found = false
	return res
}

// buttonOK validates one card button: non-empty label (<=32 runes) plus an
// http(s) URL forced to https. Invalid pairs are dropped.
func buttonOK(label, rawURL string) (Button, bool) {
	label = strings.TrimSpace(label)
	if label == "" || len([]rune(label)) > 32 {
		return Button{}, false
	}
	u := normalizeURL(rawURL)
	if u == "" {
		return Button{}, false
	}
	return Button{Label: label, URL: u}, true
}

// resolutionButtons builds the max-2 card buttons:
//   - movie: [{View Movie, MovieURL}] + [{View Trailer, TrailerURL}]
//   - anime (anilist/jikan): [{View Episode, streaming URL if any}] + [{View Anime, ShowURL}]
//   - series (tvmaze + others): [{View Episode, episode URL}] + [{View Show, show URL}]
func resolutionButtons(m *Media, ep Episode, hasEp, isMovie bool) []Button {
	var cands [][2]string
	switch {
	case isMovie:
		cands = [][2]string{
			{"View Movie", m.MovieURL},
			{"View Trailer", m.TrailerURL},
		}
	case m.Source == "anilist" || m.Source == "jikan":
		epURL := ""
		if hasEp {
			epURL = ep.URL
		}
		cands = [][2]string{
			{"View Episode", epURL},
			{"View Anime", m.ShowURL},
		}
	default:
		epURL := m.EpisodeURL
		if hasEp && strings.TrimSpace(ep.URL) != "" {
			epURL = ep.URL
		}
		cands = [][2]string{
			{"View Episode", epURL},
			{"View Show", m.ShowURL},
		}
	}
	out := make([]Button, 0, 2)
	for _, c := range cands {
		if b, ok := buttonOK(c[0], c[1]); ok {
			out = append(out, b)
			if len(out) == 2 {
				break
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Resolve NEVER blocks on network: it returns from the memory/disk cache
// when present, else fires ONE background fetch per cache key
// (single-flight) and returns Found=false.
func (r *Resolver) Resolve(ctx context.Context, q Query) (Resolution, error) {
	key := cacheKey(q)
	r.mu.Lock()
	if m, ok := r.mem[key]; ok && mediaAnswers(m, q) {
		r.mu.Unlock()
		return resolveFromMedia(m, q), nil
	}
	// A cached entry that cannot answer q is treated as a miss so a
	// stale/wrong source never pins the key; the fetch below may overwrite
	// it with the source that does answer. The stale entry stays in mem
	// for other episodes sharing this key.
	if exp, ok := r.neg[key]; ok {
		if time.Now().Before(exp) {
			r.mu.Unlock()
			return Resolution{}, nil
		}
		delete(r.neg, key)
	}
	if e, ok := cacheLoadEntry(r.dir, key); ok {
		if e.NotFound {
			// Fresh persisted miss: no network, same observable
			// result as a fresh not-found lookup. Repopulate the
			// in-memory fast path for the rest of this process,
			// bounded by the stored TTL so a near-expiry miss is
			// not extended past its on-disk expiry.
			r.neg[key] = negativeMemExpiry(e.CheckedAt, time.Now())
			r.mu.Unlock()
			return Resolution{}, nil
		}
		if mediaAnswers(e.Media, q) {
			r.mem[key] = e.Media
			r.mu.Unlock()
			return resolveFromMedia(e.Media, q), nil
		}
		// Stale on-disk entry for this episode: fall through to fetch
		// (which overwrites it on success) rather than pinning a miss.
	}
	if r.offline {
		// Cache-only: never touch the network, never spawn a fetch.
		r.mu.Unlock()
		return Resolution{}, nil
	}
	if _, ok := r.inflight[key]; ok {
		r.mu.Unlock()
		return Resolution{}, nil
	}
	done := make(chan struct{})
	r.inflight[key] = done
	r.mu.Unlock()
	go r.backgroundFetch(q, key, done)
	return Resolution{}, nil
}

func (r *Resolver) backgroundFetch(q Query, key string, done chan struct{}) {
	fetchCtx, cancel := context.WithTimeout(context.Background(), r.timeout*4)
	defer cancel()
	m, definitive := r.fetchMedia(fetchCtx, q)
	r.mu.Lock()
	defer r.mu.Unlock()
	defer close(done)
	delete(r.inflight, key)
	if m == nil {
		// A cancelled/timed-out fetch or any provider error is
		// transient, never a confirmed not-found: persist nothing
		// and suppress nothing.
		if fetchCtx.Err() != nil {
			return
		}
		if !definitive {
			return
		}
		r.neg[key] = time.Now().Add(negativeBackoff)
		if err := cacheStoreNegative(r.dir, key); err != nil {
			r.log("metadata: cache store negative %q: %v", key, err)
		}
		return
	}
	r.mem[key] = m
	delete(r.neg, key)
	if err := cacheStore(r.dir, key, m); err != nil {
		r.log("metadata: cache store %q: %v", key, err)
	}
}

// LookupNow is the blocking form for CLI/tests: fetch if needed, then resolve.
func (r *Resolver) LookupNow(ctx context.Context, q Query) (Resolution, error) {
	return r.LookupNowForce(ctx, q, false)
}

// LookupNowForce is LookupNow with an explicit force flag. force=true
// bypasses the memory/disk cache and the negative backoff for this query,
// fetches fresh from the sources, and rewrites the cache entry on success.
// force=false is exactly the historical LookupNow behavior. Offline still
// serves a miss: a forced fetch can never touch the network in offline mode.
func (r *Resolver) LookupNowForce(ctx context.Context, q Query, force bool) (Resolution, error) {
	res, _, err := r.LookupNowForceSrc(ctx, q, force)
	return res, err
}

// LookupNowForceSrc is LookupNowForce with provenance: fromCache reports
// whether a FOUND resolution was served from the memory/disk cache without
// a fetch. force=true never reports fromCache (it always fetches); a miss
// (Found=false, including the negative-backoff and offline paths) reports
// fromCache=false. LookupNow and LookupNowForce delegate to it so all
// current callers and signatures keep working unchanged.
func (r *Resolver) LookupNowForceSrc(ctx context.Context, q Query, force bool) (Resolution, bool, error) {
	key := cacheKey(q)
	var prevNeg time.Time
	var hadPrevNeg bool
	r.mu.Lock()
	if !force {
		if m, ok := r.mem[key]; ok && mediaAnswers(m, q) {
			r.mu.Unlock()
			return resolveFromMedia(m, q), true, nil
		}
		if exp, ok := r.neg[key]; ok {
			if time.Now().Before(exp) {
				r.mu.Unlock()
				return Resolution{}, false, nil
			}
			delete(r.neg, key)
		}
		if e, ok := cacheLoadEntry(r.dir, key); ok {
			if e.NotFound {
				// Fresh persisted miss: no network, exactly the
				// observable result of a fresh not-found lookup.
				// Repopulate the in-memory fast path, bounded by
				// the stored TTL so a near-expiry miss is not
				// extended past its on-disk expiry.
				r.neg[key] = negativeMemExpiry(e.CheckedAt, time.Now())
				r.mu.Unlock()
				return Resolution{}, false, nil
			}
			if mediaAnswers(e.Media, q) {
				r.mem[key] = e.Media
				r.mu.Unlock()
				return resolveFromMedia(e.Media, q), true, nil
			}
			// Stale on-disk entry for this episode: fall through
			// to the fetch below (which overwrites it on success)
			// rather than pinning a miss.
		}
	} else {
		// A forced refetch must never be pinned by a stale answer or a
		// recent miss; the fetch below overwrites both on success.
		// Save the cleared negative so a transient failure below can
		// restore it instead of dropping suppression early. Memory
		// positives are left intact so a failed refresh never clobbers
		// them in memory either.
		if exp, ok := r.neg[key]; ok {
			prevNeg, hadPrevNeg = exp, true
		}
		delete(r.neg, key)
	}
	if r.offline {
		// Cache-only: never touch the network.
		r.mu.Unlock()
		return Resolution{}, false, nil
	}
	if ch, ok := r.inflight[key]; ok {
		r.mu.Unlock()
		select {
		case <-ch:
			r.mu.Lock()
			defer r.mu.Unlock()
			if m, ok := r.mem[key]; ok {
				// Served by a concurrent fetch, not the cache.
				return resolveFromMedia(m, q), false, nil
			}
			return Resolution{}, false, nil
		case <-ctx.Done():
			return Resolution{}, false, ctx.Err()
		}
	}
	done := make(chan struct{})
	r.inflight[key] = done
	r.mu.Unlock()

	m, definitive := r.fetchMedia(ctx, q)

	r.mu.Lock()
	defer r.mu.Unlock()
	defer close(done)
	delete(r.inflight, key)
	if m == nil {
		if err := ctx.Err(); err != nil {
			// A cancelled fetch persists nothing; restore the
			// negative cleared above so a failed forced refresh
			// leaves prior suppression intact.
			if force && hadPrevNeg {
				r.neg[key] = prevNeg
			}
			return Resolution{}, false, err
		}
		if !definitive {
			// Transient provider error/timeout: not a confirmed
			// not-found, so persist nothing and suppress nothing.
			// A valid positive on disk is left intact (F3).
			if force && hadPrevNeg {
				r.neg[key] = prevNeg
			}
			return Resolution{}, false, nil
		}
		r.neg[key] = time.Now().Add(negativeBackoff)
		if err := cacheStoreNegative(r.dir, key); err != nil {
			r.log("metadata: cache store negative %q: %v", key, err)
		}
		return Resolution{}, false, nil
	}
	r.mem[key] = m
	delete(r.neg, key)
	if err := cacheStore(r.dir, key, m); err != nil {
		r.log("metadata: cache store %q: %v", key, err)
	}
	return resolveFromMedia(m, q), false, nil
}
