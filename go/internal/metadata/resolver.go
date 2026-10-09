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

// DefaultSources returns the built-in providers. Ordering by Kind is handled
// by the Resolver.
func DefaultSources() []Source { return makeDefaultSources(0) }

// New builds a Resolver. A nil Sources list selects DefaultSources().
func New(opts Options) *Resolver {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultHTTPTimeout()
	}
	srcs := opts.Sources
	if srcs == nil {
		srcs = makeDefaultSources(timeout)
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
		pref = map[string]int{"itunes": 0}
	default:
		// TVmaze first for every series query: its season/episode tables
		// are authoritative, while AniList streamingEpisodes are mislabeled
		// for absolute anime numbering (Re:ZERO media 21355 serves 51-66
		// for an episode-1 query - a wrong title is worse than no title)
		// and Jikan is rate-limited. This holds for Absolute and for
		// !Absolute alike.
		pref = map[string]int{"tvmaze": 0, "anilist": 1, "jikan": 2}
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

func (r *Resolver) fetchMedia(ctx context.Context, q Query) *Media {
	for _, s := range r.orderedSources(q) {
		m, err := s.Find(ctx, q)
		if err != nil {
			r.log("metadata: source %s error: %v", s.Name(), err)
			continue
		}
		if m == nil {
			continue
		}
		if m.Source == "" {
			m.Source = s.Name()
		}
		if m.Kind == KindMovie || q.Kind == KindMovie {
			return m
		}
		if _, ok := matchEpisode(m.Episodes, q); ok {
			return m
		}
		// The show matched but its Numbers lack the requested episode:
		// fall through to the next source rather than caching/returning
		// a wrong (positional) title.
		r.log("metadata: source %s lacks episode %d for %q, trying next", s.Name(), q.Episode, q.Title)
	}
	return nil
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
	res := Resolution{Series: m.Title, Season: q.Season, Number: q.Episode, Artwork: m.Artwork, Synopsis: capSynopsis(stripHTML(m.Synopsis))}
	if m.Kind == KindMovie || q.Kind == KindMovie {
		res.Season = 0
		res.Number = 0
		res.Found = true
		return res
	}
	if ep, ok := matchEpisode(m.Episodes, q); ok {
		res.Season = ep.Season
		res.Number = ep.Number
		res.Episode = ep.Title
		// Episode art/summary win when present, else the series-level ones
		// already on res.
		if strings.TrimSpace(ep.Artwork) != "" {
			res.Artwork = strings.TrimSpace(ep.Artwork)
		}
		if s := firstNonEmpty(ep.Synopsis, m.Synopsis); s != "" {
			res.Synopsis = capSynopsis(stripHTML(s))
		}
		res.Found = true
		return res
	}
	// Every source lacked the requested Number: not found, so the play
	// path falls back to today's clean card. Never report a wrong title.
	res.Found = false
	return res
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
	if m, ok := cacheLoad(r.dir, key); ok {
		if mediaAnswers(m, q) {
			r.mem[key] = m
			r.mu.Unlock()
			return resolveFromMedia(m, q), nil
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
	m := r.fetchMedia(fetchCtx, q)
	r.mu.Lock()
	defer r.mu.Unlock()
	defer close(done)
	delete(r.inflight, key)
	if m == nil {
		r.neg[key] = time.Now().Add(negativeBackoff)
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
	key := cacheKey(q)
	r.mu.Lock()
	if m, ok := r.mem[key]; ok && mediaAnswers(m, q) {
		r.mu.Unlock()
		return resolveFromMedia(m, q), nil
	}
	if m, ok := cacheLoad(r.dir, key); ok && mediaAnswers(m, q) {
		r.mem[key] = m
		r.mu.Unlock()
		return resolveFromMedia(m, q), nil
	}
	if exp, ok := r.neg[key]; ok {
		if time.Now().Before(exp) {
			r.mu.Unlock()
			return Resolution{}, nil
		}
		delete(r.neg, key)
	}
	if r.offline {
		// Cache-only: never touch the network.
		r.mu.Unlock()
		return Resolution{}, nil
	}
	if ch, ok := r.inflight[key]; ok {
		r.mu.Unlock()
		select {
		case <-ch:
			r.mu.Lock()
			defer r.mu.Unlock()
			if m, ok := r.mem[key]; ok {
				return resolveFromMedia(m, q), nil
			}
			return Resolution{}, nil
		case <-ctx.Done():
			return Resolution{}, ctx.Err()
		}
	}
	done := make(chan struct{})
	r.inflight[key] = done
	r.mu.Unlock()

	m := r.fetchMedia(ctx, q)

	r.mu.Lock()
	defer r.mu.Unlock()
	defer close(done)
	delete(r.inflight, key)
	if m == nil {
		if err := ctx.Err(); err != nil {
			return Resolution{}, err
		}
		r.neg[key] = time.Now().Add(negativeBackoff)
		return Resolution{}, nil
	}
	r.mem[key] = m
	delete(r.neg, key)
	if err := cacheStore(r.dir, key, m); err != nil {
		r.log("metadata: cache store %q: %v", key, err)
	}
	return resolveFromMedia(m, q), nil
}
