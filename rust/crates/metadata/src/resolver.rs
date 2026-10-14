//! `metadata::resolver` -- mirrors `go\internal\metadata\resolver.go`.
//!
//! The provider chain. The poll loop uses the NON-BLOCKING [`Resolver::resolve`]
//! (a cache miss fires one background fetch per cache key, single-flight, and the
//! card falls back to the clean title until it lands); the CLIs use the BLOCKING
//! [`Resolver::lookup_now`]. Order: TVmaze for TV, AniList + Jikan for anime,
//! iTunes then Wikipedia for movies. Offline safe: with no cache entry and no
//! network the card is exactly the clean card, and [`Options::offline`] serves
//! cache-only. `force` bypasses and re-attempts.
//!
//! # The chain is behaviour, not detail
//!
//! * [`Resolver::ordered_sources`] ranks providers per kind. TVmaze is first for
//!   EVERY series query including `Absolute` (its season/episode tables are
//!   authoritative, while AniList `streamingEpisodes` are mislabeled for
//!   absolute numbering -- Re:ZERO media 21355 serves 51-66 for an episode-1
//!   query, and a wrong title is worse than no title). Unranked names keep their
//!   relative order at rank 100, so an injected test source is still tried.
//! * `fetchChain` keeps going past an error (`sawError`, first error kept) and
//!   past a source that answered but lacks the requested episode. A movie
//!   answer returns immediately; a series answer needs a number-exact episode
//!   match ([`match_episode`], NEVER a positional index).
//! * `fetchMedia` retries the SAME chain once with the separator-normalized
//!   alternate title, and ONLY after a definitive primary miss: a transient
//!   primary is unknown, and substituting an alternate match would overwrite a
//!   correct cached positive under the ORIGINAL key.
//! * Only a definitive miss is cached negative (10 minutes in memory, 30 days on
//!   disk). A transient failure -- including cancellation -- persists NOTHING.

use std::collections::{HashMap, HashSet};
use std::sync::{Arc, Condvar, Mutex, MutexGuard};
use std::time::{Duration, SystemTime};

use metadata_core::alternate::alternate_query;
use metadata_core::types::{Button, Episode, Kind, Media, Query, Resolution, normalize_url};
use metadata_http::cancel::Cancel;
use metadata_http::http::{DEFAULT_HTTP_TIMEOUT, HttpClient};

use crate::artwork::{cap_synopsis, first_non_empty, strip_html};
use crate::cache::{
    cache_has, cache_key, cache_live, cache_load_entry, cache_store, cache_store_negative,
    negative_mem_expiry, resolve_cache_dir,
};
use crate::tvmaze::TvmazeSource;
use crate::wikipedia::{WikiClient, WikipediaSource};
use crate::{Error, anilist::AnilistSource, itunes::ItunesSource, jikan::JikanSource};

/// `negativeBackoff` keeps a failed key from being hammered. In-memory only; the
/// on-disk cache stays additive forever.
pub const NEGATIVE_BACKOFF: Duration = Duration::from_secs(10 * 60);

/// `Source` is one keyless metadata provider. Go's `Source` interface:
/// `Name() string` and `Find(ctx, q) (*Media, error)`.
///
/// `ctx` is [`Cancel`] (`metadata-http`'s `context.Context` stand-in): a source
/// must abort a wait when the scope ends and must return the scope's error
/// rather than a miss.
pub trait Source: Send + Sync {
    /// The provider's name, as it appears in a [`Media::source`] and in the
    /// chain ranking.
    fn name(&self) -> &str;

    /// `Find`: `Ok(None)` is a definitive miss for this source, `Err` is a
    /// transient failure, `Ok(Some)` is an answer the chain still has to check
    /// against the query.
    fn find(&self, cancel: &Cancel, q: &Query) -> Result<Option<Media>, Error>;
}

/// `Logf`: Go's `Logf func(format string, args ...any)`, with the message
/// already formatted.
pub type LogFn = Arc<dyn Fn(&str) + Send + Sync>;

/// `Options` for [`Resolver::new`]. The zero value is usable: no dir means the
/// default cache dir, no timeout means [`DEFAULT_HTTP_TIMEOUT`], and no sources
/// means [`default_sources`].
pub struct Options {
    /// Cache directory; empty resolves to `%LOCALAPPDATA%\Potflix\meta`.
    pub dir: String,
    /// Per-request HTTP budget; zero means [`DEFAULT_HTTP_TIMEOUT`].
    pub timeout: Duration,
    /// The provider set; `None` selects the built-in five. `Some(vec![])` is a
    /// deliberately empty chain (every lookup is a definitive miss).
    pub sources: Option<Vec<Arc<dyn Source>>>,
    /// A sink for the resolver's diagnostics (`metadata: source ... error: ...`).
    pub logf: Option<LogFn>,
    /// Cache-only: never touch the network, never spawn a fetch.
    pub offline: bool,
}

impl Default for Options {
    fn default() -> Options {
        Options {
            dir: String::new(),
            timeout: Duration::ZERO,
            sources: None,
            logf: None,
            offline: false,
        }
    }
}

/// `makeDefaultSources`: the built-in keyless providers in their historical
/// order, sharing one connection pool.
fn make_default_sources(timeout: Duration) -> Vec<Arc<dyn Source>> {
    let client = HttpClient::new(timeout);
    vec![
        Arc::new(TvmazeSource::new("https://api.tvmaze.com", client.clone())),
        Arc::new(AnilistSource::new(
            "https://graphql.anilist.co",
            client.clone(),
        )),
        Arc::new(JikanSource::new("https://api.jikan.moe/v4", client.clone())),
        Arc::new(ItunesSource::new(
            "https://itunes.apple.com",
            client.clone(),
        )),
        // Wikipedia sends its own User-Agent, so it carries its own agent with
        // the same timeout instead of the shared client.
        Arc::new(WikipediaSource::new(
            "https://en.wikipedia.org",
            WikiClient::new(timeout),
        )),
    ]
}

/// `DefaultSources`: the built-in keyless providers (TVmaze, AniList, Jikan,
/// iTunes, Wikipedia) in their historical order.
pub fn default_sources() -> Vec<Arc<dyn Source>> {
    make_default_sources(Duration::ZERO)
}

/// A one-shot completion flag, Go's `chan struct{}`: `close` wakes every waiter.
#[derive(Debug, Default)]
pub(crate) struct Done {
    done: Mutex<bool>,
    signal: Condvar,
}

impl Done {
    pub(crate) fn new() -> Arc<Done> {
        Arc::new(Done::default())
    }

    /// `close(ch)`.
    pub(crate) fn close(&self) {
        let mut done = self.lock();
        *done = true;
        self.signal.notify_all();
    }

    /// Go's `select { case <-ch: ...; case <-ctx.Done(): ... }`: returns when the
    /// flag is closed or the scope ends, whichever happens first.
    pub(crate) fn wait(&self, cancel: &Cancel) {
        let mut done = self.lock();
        loop {
            if *done || cancel.is_cancelled() {
                return;
            }
            let (guard, _) = self
                .signal
                .wait_timeout(done, Duration::from_millis(25))
                .unwrap_or_else(|poisoned| poisoned.into_inner());
            done = guard;
        }
    }

    fn lock(&self) -> MutexGuard<'_, bool> {
        self.done
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
    }
}

/// Closes the flag on every exit path, Go's `defer close(done)`.
struct DoneGuard(Arc<Done>);

impl Drop for DoneGuard {
    fn drop(&mut self) {
        self.0.close();
    }
}

#[derive(Default)]
struct State {
    mem: HashMap<String, Media>,
    inflight: HashMap<String, Arc<Done>>,
    neg: HashMap<String, SystemTime>,
    probed: HashSet<String>,
}

struct Inner {
    mu: Mutex<State>,
    dir: String,
    timeout: Duration,
    sources: Vec<Arc<dyn Source>>,
    logf: Option<LogFn>,
    offline: bool,
}

/// `Resolver` resolves `Query`s to episode titles without ever blocking on the
/// network in [`Resolver::resolve`]. Safe for concurrent use; cloneable, and
/// every clone shares the same memory, in-flight set and suppression map.
#[derive(Clone)]
pub struct Resolver {
    inner: Arc<Inner>,
}

impl Resolver {
    /// `New`: builds a resolver. A zero timeout takes
    /// [`DEFAULT_HTTP_TIMEOUT`]; `None` sources selects [`default_sources`].
    pub fn new(opts: Options) -> Resolver {
        let timeout = if opts.timeout.is_zero() {
            DEFAULT_HTTP_TIMEOUT
        } else {
            opts.timeout
        };
        let sources = match opts.sources {
            Some(sources) => sources,
            None => make_default_sources(timeout),
        };
        Resolver {
            inner: Arc::new(Inner {
                mu: Mutex::new(State::default()),
                dir: resolve_cache_dir(&opts.dir),
                timeout,
                sources,
                logf: opts.logf,
                offline: opts.offline,
            }),
        }
    }

    fn lock(&self) -> MutexGuard<'_, State> {
        self.inner
            .mu
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
    }

    fn log(&self, message: &str) {
        if let Some(logf) = &self.inner.logf {
            logf(message);
        }
    }

    /// The resolver's cache directory.
    pub fn dir(&self) -> &str {
        &self.inner.dir
    }

    /// `orderedSources`: ranks providers per Kind; unknown names keep their
    /// relative order at the end so injected test sources are still tried.
    pub fn ordered_sources(&self, q: &Query) -> Vec<Arc<dyn Source>> {
        let pref: &[(&str, i32)] = if q.kind == Kind::MOVIE {
            &[("itunes", 0), ("wikipedia", 1)]
        } else {
            &[("tvmaze", 0), ("anilist", 1), ("jikan", 2)]
        };
        let mut out = self.inner.sources.clone();
        out.sort_by_key(|source| {
            pref.iter()
                .find(|(name, _)| *name == source.name())
                .map_or(100, |(_, rank)| *rank)
        });
        out
    }

    /// `ResolveCached`: [`Resolver::resolve`] without the fetch. `Some` when
    /// memory or disk can answer `q`, `None` otherwise -- never touching the
    /// network and never spawning a background fetch. A stale key (a live entry
    /// that cannot answer `q`) reports `None` so the caller routes it through its
    /// bounded fetch path instead of fanning out one uncancellable fetch per
    /// stale key.
    pub fn resolve_cached(&self, q: &Query) -> Option<Resolution> {
        let key = cache_key(q);
        let mut state = self.lock();
        if let Some(media) = cached_answer(&mut state, &self.inner.dir, &key, q) {
            return Some(resolve_from_media(&media, q));
        }
        if let Some(expiry) = state.neg.get(&key).copied() {
            if SystemTime::now() < expiry {
                if probed_live(&mut state, &self.inner.dir, &key) {
                    return Some(Resolution::default());
                }
            } else {
                state.neg.remove(&key);
            }
        }
        if let Some(entry) = cache_load_entry(&self.inner.dir, &key) {
            if entry.not_found {
                let expiry =
                    negative_mem_expiry(&entry.checked_at, SystemTime::now(), NEGATIVE_BACKOFF);
                state.neg.insert(key, expiry);
                return Some(Resolution::default());
            }
            if let Some(media) = entry.media
                && media_answers(&media, q)
            {
                state.mem.insert(key, media.clone());
                return Some(resolve_from_media(&media, q));
            }
        }
        None
    }

    /// `Resolve` NEVER blocks on network: it returns from the memory/disk cache
    /// when present, else fires ONE background fetch per cache key
    /// (single-flight) and returns `Found=false`.
    pub fn resolve(&self, cancel: &Cancel, q: &Query) -> Result<Resolution, Error> {
        let key = cache_key(q);
        let mut state = self.lock();
        if let Some(media) = cached_answer(&mut state, &self.inner.dir, &key, q) {
            return Ok(resolve_from_media(&media, q));
        }
        // A cached entry that cannot answer q is treated as a miss so a
        // stale/wrong source never pins the key; the fetch below may overwrite it
        // with the source that does answer. The stale entry stays in mem for
        // other episodes sharing this key.
        if let Some(expiry) = state.neg.get(&key).copied() {
            if SystemTime::now() < expiry {
                if probed_live(&mut state, &self.inner.dir, &key) {
                    return Ok(Resolution::default());
                }
            } else {
                state.neg.remove(&key);
            }
        }
        if let Some(entry) = cache_load_entry(&self.inner.dir, &key) {
            if entry.not_found {
                // Fresh persisted miss: no network, same observable result as a
                // fresh not-found lookup. Repopulate the in-memory fast path,
                // bounded by the stored TTL.
                let expiry =
                    negative_mem_expiry(&entry.checked_at, SystemTime::now(), NEGATIVE_BACKOFF);
                state.neg.insert(key, expiry);
                return Ok(Resolution::default());
            }
            if let Some(media) = entry.media
                && media_answers(&media, q)
            {
                state.mem.insert(key, media.clone());
                return Ok(resolve_from_media(&media, q));
            }
            // Stale on-disk entry for this episode: fall through to the fetch.
        }
        if self.inner.offline {
            return Ok(Resolution::default());
        }
        if state.inflight.contains_key(&key) {
            return Ok(Resolution::default());
        }
        let done = Done::new();
        state.inflight.insert(key.clone(), Arc::clone(&done));
        drop(state);
        let me = self.clone();
        let cancel = cancel.clone();
        let q = q.clone();
        std::thread::spawn(move || me.background_fetch(cancel, q, &key, done));
        Ok(Resolution::default())
    }

    /// `backgroundFetch`: one fetch per key, in its own scope
    /// (`timeout * 4`), persisting a negative only for a definitive miss.
    pub(crate) fn background_fetch(&self, cancel: Cancel, q: Query, key: &str, done: Arc<Done>) {
        let _guard = DoneGuard(Arc::clone(&done));
        let fetch_cancel = child_scope(&cancel, self.inner.timeout * 4);
        let chain = self.fetch_media(&fetch_cancel, &q);
        let mut state = self.lock();
        state.inflight.remove(key);
        let timed_out = fetch_cancel.err().is_some();
        // Release the propagation watcher: this scope is finished.
        fetch_cancel.cancel();
        let Some(media) = chain.media else {
            // A cancelled/timed-out fetch or any provider error is transient,
            // never a confirmed not-found: persist nothing, suppress nothing.
            if timed_out || !chain.definitive {
                return;
            }
            state
                .neg
                .insert(key.to_string(), SystemTime::now() + NEGATIVE_BACKOFF);
            drop(state);
            if let Err(err) = cache_store_negative(&self.inner.dir, key) {
                self.log(&format!("metadata: cache store negative {key:?}: {err}"));
            }
            return;
        };
        state.mem.insert(key.to_string(), media.clone());
        state.neg.remove(key);
        drop(state);
        if let Err(err) = cache_store(&self.inner.dir, key, &media) {
            self.log(&format!("metadata: cache store {key:?}: {err}"));
        }
    }

    /// `LookupNow`: the blocking form for CLI/tests -- fetch if needed, then
    /// resolve.
    pub fn lookup_now(&self, cancel: &Cancel, q: &Query) -> Result<Resolution, Error> {
        self.lookup_now_force(cancel, q, false)
    }

    /// `LookupNowForce`: `force=true` bypasses the memory/disk cache and the
    /// negative backoff for this query, fetches fresh, and rewrites the cache
    /// entry on success. Offline still serves a miss.
    pub fn lookup_now_force(
        &self,
        cancel: &Cancel,
        q: &Query,
        force: bool,
    ) -> Result<Resolution, Error> {
        self.lookup_now_force_src(cancel, q, force)
            .map(|(resolution, _)| resolution)
    }

    /// `LookupNowForceSrc` with provenance: the flag reports whether a FOUND
    /// resolution was served from the memory/disk cache without a fetch.
    /// `force=true` never reports `true`; a miss reports `false`.
    pub fn lookup_now_force_src(
        &self,
        cancel: &Cancel,
        q: &Query,
        force: bool,
    ) -> Result<(Resolution, bool), Error> {
        let key = cache_key(q);
        let mut prev_neg: Option<SystemTime> = None;
        let mut state = self.lock();
        if !force {
            if let Some(media) = cached_answer(&mut state, &self.inner.dir, &key, q) {
                return Ok((resolve_from_media(&media, q), true));
            }
            if let Some(expiry) = state.neg.get(&key).copied() {
                if SystemTime::now() < expiry {
                    if probed_live(&mut state, &self.inner.dir, &key) {
                        return Ok((Resolution::default(), false));
                    }
                } else {
                    state.neg.remove(&key);
                }
            }
            if let Some(entry) = cache_load_entry(&self.inner.dir, &key) {
                if entry.not_found {
                    // Fresh persisted miss: no network, exactly the observable
                    // result of a fresh not-found lookup.
                    let expiry =
                        negative_mem_expiry(&entry.checked_at, SystemTime::now(), NEGATIVE_BACKOFF);
                    state.neg.insert(key.clone(), expiry);
                    return Ok((Resolution::default(), false));
                }
                if let Some(media) = entry.media
                    && media_answers(&media, q)
                {
                    state.mem.insert(key.clone(), media.clone());
                    return Ok((resolve_from_media(&media, q), true));
                }
                // Stale on-disk entry: fall through to the fetch below.
            }
        } else {
            // A forced refetch must never be pinned by a stale answer or a
            // recent miss; the fetch below overwrites both on success. Save the
            // cleared negative so a transient failure can restore it.
            prev_neg = state.neg.get(&key).copied();
            state.neg.remove(&key);
        }
        if self.inner.offline {
            return Ok((Resolution::default(), false));
        }
        if let Some(done) = state.inflight.get(&key).cloned() {
            drop(state);
            done.wait(cancel);
            if let Some(reason) = cancel.err() {
                return Err(Error::Cancelled(reason));
            }
            let state = self.lock();
            if let Some(media) = state.mem.get(&key) {
                // Served by a concurrent fetch, not the cache.
                return Ok((resolve_from_media(media, q), false));
            }
            if let Some(expiry) = state.neg.get(&key).copied()
                && SystemTime::now() < expiry
            {
                // The owning fetch confirmed the miss (it recorded suppression
                // before releasing the joiners).
                return Ok((Resolution::default(), false));
            }
            // The owning fetch ended without an answer and without recording a
            // miss: its outcome was transient, so this join is transient too.
            return Err(Error::unavailable(format!(
                "metadata: concurrent fetch failed transiently for {key:?}: {}",
                crate::LOOKUP_UNAVAILABLE_MESSAGE
            )));
        }
        let done = Done::new();
        state.inflight.insert(key.clone(), Arc::clone(&done));
        drop(state);

        let chain = self.fetch_media(cancel, q);

        let mut state = self.lock();
        state.inflight.remove(&key);
        let _guard = DoneGuard(done);
        let Some(media) = chain.media else {
            if let Some(reason) = cancel.err() {
                // A cancelled fetch persists nothing; restore the negative
                // cleared above so a failed forced refresh leaves prior
                // suppression intact.
                if force && let Some(prev) = prev_neg {
                    state.neg.insert(key.clone(), prev);
                }
                return Err(Error::Cancelled(reason));
            }
            if !chain.definitive {
                // Transient provider error/timeout: not a confirmed not-found,
                // so persist nothing and suppress nothing. A valid positive on
                // disk is left intact. The sentinel keeps this distinguishable
                // from a miss.
                if force && let Some(prev) = prev_neg {
                    state.neg.insert(key.clone(), prev);
                }
                return Err(match chain.err {
                    Some(fetch_err) => Error::unavailable(format!(
                        "metadata: transient lookup failure for {key:?} ({fetch_err}): {}",
                        crate::LOOKUP_UNAVAILABLE_MESSAGE
                    )),
                    None => Error::unavailable(format!(
                        "metadata: transient lookup failure for {key:?}: {}",
                        crate::LOOKUP_UNAVAILABLE_MESSAGE
                    )),
                });
            }
            state
                .neg
                .insert(key.clone(), SystemTime::now() + NEGATIVE_BACKOFF);
            drop(state);
            if let Err(err) = cache_store_negative(&self.inner.dir, &key) {
                self.log(&format!("metadata: cache store negative {key:?}: {err}"));
            }
            return Ok((Resolution::default(), false));
        };
        state.mem.insert(key.clone(), media.clone());
        state.neg.remove(&key);
        drop(state);
        if let Err(err) = cache_store(&self.inner.dir, &key, &media) {
            self.log(&format!("metadata: cache store {key:?}: {err}"));
        }
        Ok((resolve_from_media(&media, q), false))
    }

    /// `(*Resolver).CacheLive`: probes the resolver's own cache directory for a
    /// valid live answer to `key`.
    pub fn cache_live(&self, key: &str) -> bool {
        cache_live(&self.inner.dir, key)
    }

    /// `ProbeCache`: whether `key` has a live cache answer without ever fetching,
    /// warming the in-memory fast path so a later lookup for the same key is
    /// served without another store read. The verdict matches [`cache_live`]
    /// exactly; only the side effect differs. Warmed entries are marked probed so
    /// an entry removed after the probe is never served from memory.
    pub fn probe_cache(&self, key: &str) -> bool {
        if key.is_empty() {
            return false;
        }
        let Some(entry) = cache_load_entry(&self.inner.dir, key) else {
            return false;
        };
        let mut state = self.lock();
        if entry.not_found {
            let expiry =
                negative_mem_expiry(&entry.checked_at, SystemTime::now(), NEGATIVE_BACKOFF);
            state.neg.insert(key.to_string(), expiry);
            state.probed.insert(key.to_string());
            return true;
        }
        match entry.media {
            Some(media) => {
                state.mem.insert(key.to_string(), media);
                state.probed.insert(key.to_string());
                true
            }
            None => false,
        }
    }

    /// Registers an in-flight fetch for `key` (the test seam Go's direct
    /// `r.inflight[key] = done` provides).
    #[cfg(test)]
    pub(crate) fn set_inflight(&self, key: &str, done: Arc<Done>) {
        self.lock().inflight.insert(key.to_string(), done);
    }

    /// Whether `key` is suppressed in memory (Go's `_, ok := r.neg[key]`).
    #[cfg(test)]
    pub(crate) fn neg_suppressed(&self, key: &str) -> bool {
        self.lock().neg.contains_key(key)
    }

    /// The in-memory suppression expiry for `key`.
    #[cfg(test)]
    pub(crate) fn neg_expiry(&self, key: &str) -> Option<SystemTime> {
        self.lock().neg.get(key).copied()
    }
}

/// The cached positive that can answer `q`, with a probe-warmed entry
/// revalidated exactly as Go's `probedLive` does: an entry invalidated after the
/// probe must never be served from warmed memory.
fn cached_answer(state: &mut State, dir: &str, key: &str, q: &Query) -> Option<Media> {
    if !state
        .mem
        .get(key)
        .is_some_and(|media| media_answers(media, q))
    {
        return None;
    }
    if !probed_live(state, dir, key) {
        return None;
    }
    state.mem.get(key).cloned()
}

/// `probedLive`: revalidates a probe-warmed entry -- an entry invalidated after
/// the probe must never be served from warmed memory. Keys never warmed by
/// `ProbeCache` report true without any I/O.
fn probed_live(state: &mut State, dir: &str, key: &str) -> bool {
    if !state.probed.contains(key) {
        return true;
    }
    if cache_has(dir, key) {
        return true;
    }
    state.mem.remove(key);
    state.neg.remove(key);
    state.probed.remove(key);
    false
}

/// The outcome of one chain walk: Go's `(media, definitive, error)` triple.
struct Chain {
    media: Option<Media>,
    definitive: bool,
    err: Option<Error>,
}

impl Chain {
    fn found(media: Media) -> Chain {
        Chain {
            media: Some(media),
            definitive: true,
            err: None,
        }
    }

    fn missed() -> Chain {
        Chain {
            media: None,
            definitive: true,
            err: None,
        }
    }

    fn transient(err: Option<Error>) -> Chain {
        Chain {
            media: None,
            definitive: false,
            err,
        }
    }
}

impl Resolver {
    /// `fetchMedia`: the chain, then ONE bounded retry under the alternate title
    /// and ONLY after a definitive primary miss.
    fn fetch_media(&self, cancel: &Cancel, q: &Query) -> Chain {
        let primary = self.fetch_chain(cancel, q);
        if let Some(media) = primary.media {
            return Chain::found(media);
        }
        // An unknown primary outcome (transient failure) must never be replaced
        // by a substituted alternate match that the caller then persists under
        // the ORIGINAL key, overwriting a correct cached positive.
        if !primary.definitive {
            return Chain::transient(primary.err);
        }
        let Some(alt) = alternate_query(q) else {
            return Chain {
                media: None,
                definitive: true,
                err: primary.err,
            };
        };
        if cancel.err().is_some() {
            return Chain {
                media: None,
                definitive: true,
                err: primary.err,
            };
        }
        // One bounded retry of the SAME source chain under the
        // separator-normalized title ("Show - Arc" -> "Show: Arc"). The sources
        // match against the alternate title, while the caller caches and
        // resolves under the ORIGINAL query key.
        let secondary = self.fetch_chain(cancel, &alt);
        if let Some(media) = secondary.media {
            return Chain::found(media);
        }
        // The primary was a definitive miss, so a transient alternate is still
        // unknown and surfaces the alternate's error; the primary error is nil
        // here.
        if !secondary.definitive {
            return Chain::transient(secondary.err);
        }
        Chain::missed()
    }

    /// `fetchChain`: iterate the ordered sources; an error is recorded and the
    /// walk continues (the FIRST error is kept), a `None` answer continues, a
    /// movie answer returns immediately, and a series answer returns only on a
    /// number-exact episode match.
    fn fetch_chain(&self, cancel: &Cancel, q: &Query) -> Chain {
        let mut saw_error = false;
        let mut first_err: Option<Error> = None;
        for source in self.ordered_sources(q) {
            match source.find(cancel, q) {
                Err(err) => {
                    self.log(&format!("metadata: source {} error: {err}", source.name()));
                    saw_error = true;
                    if first_err.is_none() {
                        first_err = Some(err);
                    }
                    continue;
                }
                Ok(None) => continue,
                Ok(Some(mut media)) => {
                    if media.source.is_empty() {
                        media.source = source.name().to_string();
                    }
                    if media.kind == Kind::MOVIE || q.kind == Kind::MOVIE {
                        return Chain::found(media);
                    }
                    if match_episode(&media.episodes, q).is_some() {
                        return Chain::found(media);
                    }
                    // The show matched but its Numbers lack the requested
                    // episode: fall through to the next source rather than
                    // caching/returning a wrong (positional) title.
                    self.log(&format!(
                        "metadata: source {} lacks episode {} for {:?}, trying next",
                        source.name(),
                        q.episode,
                        q.title
                    ));
                }
            }
        }
        if saw_error {
            return Chain::transient(first_err);
        }
        if let Some(reason) = cancel.err() {
            return Chain::transient(Some(Error::Cancelled(reason)));
        }
        Chain::missed()
    }
}

/// `mediaAnswers`: movies answer on presence; series require a number-exact
/// episode match.
pub(crate) fn media_answers(media: &Media, q: &Query) -> bool {
    if media.kind == Kind::MOVIE || q.kind == Kind::MOVIE {
        return true;
    }
    match_episode(&media.episodes, q).is_some()
}

/// `matchEpisode` is number-exact: only `Number == q.Episode` (plus
/// `Season == q.Season` when `q.Season > 0`) answers. It never returns a
/// different episode via a positional index -- a wrong title is worse than no
/// title (AniList 21355 serves Numbers 51-66 for an episode-1 query).
pub(crate) fn match_episode(eps: &[Episode], q: &Query) -> Option<Episode> {
    if eps.is_empty() || q.episode <= 0 {
        return None;
    }
    if q.season > 0 {
        return eps
            .iter()
            .find(|e| i64::from(e.season) == q.season && e.number == q.episode)
            .cloned();
    }
    eps.iter().find(|e| e.number == q.episode).cloned()
}

/// `resolveFromMedia`: the user-facing [`Resolution`]. Artwork is ALWAYS the
/// series/movie poster -- episode stills never override (they look
/// unofficial) -- while the synopsis and length prefer the matched episode.
pub(crate) fn resolve_from_media(media: &Media, q: &Query) -> Resolution {
    let is_movie = media.kind == Kind::MOVIE || q.kind == Kind::MOVIE;
    let mut resolution = Resolution {
        series: media.title.clone(),
        episode: String::new(),
        season: q.season,
        number: q.episode,
        found: false,
        artwork: media.artwork.clone(),
        synopsis: cap_synopsis(&strip_html(&media.synopsis)),
        genres: media.genres.clone(),
        length: media.length.clone(),
        year: media.year,
        is_movie,
        buttons: Vec::new(),
    };
    if is_movie {
        resolution.season = 0;
        resolution.number = 0;
        resolution.found = true;
        resolution.buttons = resolution_buttons(media, None, false, true);
        return resolution;
    }
    if let Some(ep) = match_episode(&media.episodes, q) {
        resolution.season = i64::from(ep.season);
        resolution.number = ep.number;
        resolution.episode = ep.title.clone();
        let synopsis = first_non_empty(&[&ep.synopsis, &media.synopsis]);
        if !synopsis.is_empty() {
            resolution.synopsis = cap_synopsis(&strip_html(&synopsis));
        }
        if !ep.length.trim().is_empty() {
            resolution.length = ep.length.trim().to_string();
        }
        resolution.buttons = resolution_buttons(media, Some(&ep), true, false);
        resolution.found = true;
        return resolution;
    }
    // Every source lacked the requested Number: not found, so the play path
    // falls back to today's clean card. Never report a wrong title.
    resolution
}

/// `buttonOK`: a non-empty label (<= 32 runes) plus an http(s) URL forced to
/// https. Invalid pairs are dropped.
fn button_ok(label: &str, raw_url: &str) -> Option<Button> {
    let label = label.trim();
    if label.is_empty() || label.chars().count() > 32 {
        return None;
    }
    let url = normalize_url(raw_url);
    if url.is_empty() {
        return None;
    }
    Some(Button {
        label: label.to_string(),
        url,
    })
}

/// `resolutionButtons`: the max-2 card buttons.
///
/// * movie: `[View Movie, MovieURL]` + `[View Trailer, TrailerURL]`
/// * anime (anilist/jikan): `[View Episode, streaming URL]` + `[View Anime, ShowURL]`
/// * series (tvmaze + others): `[View Episode, episode URL]` + `[View Show, ShowURL]`
fn resolution_buttons(
    media: &Media,
    ep: Option<&Episode>,
    has_ep: bool,
    is_movie: bool,
) -> Vec<Button> {
    let candidates: [(&str, String); 2] = if is_movie {
        [
            ("View Movie", media.movie_url.clone()),
            ("View Trailer", media.trailer_url.clone()),
        ]
    } else if media.source == "anilist" || media.source == "jikan" {
        let episode_url = if has_ep {
            ep.map(|ep| ep.url.clone()).unwrap_or_default()
        } else {
            String::new()
        };
        [
            ("View Episode", episode_url),
            ("View Anime", media.show_url.clone()),
        ]
    } else {
        let mut episode_url = media.episode_url.clone();
        if let Some(ep) = ep
            && has_ep
            && !ep.url.trim().is_empty()
        {
            episode_url = ep.url.clone();
        }
        [
            ("View Episode", episode_url),
            ("View Show", media.show_url.clone()),
        ]
    };
    let mut out: Vec<Button> = Vec::with_capacity(2);
    for (label, url) in candidates {
        if let Some(button) = button_ok(label, &url) {
            out.push(button);
            if out.len() == 2 {
                break;
            }
        }
    }
    out
}

/// Go's `context.WithTimeout(parent, d)`: a scope that ends when the parent is
/// cancelled or when its own deadline passes.
///
/// `metadata-http`'s [`Cancel`] has no child constructor, so the propagation is a
/// bounded watcher thread that exits as soon as either scope ends -- the same
/// work Go's `propagateCancel` goroutine does. The caller cancels the child when
/// the fetch finishes, which ends the watcher immediately.
fn child_scope(parent: &Cancel, timeout: Duration) -> Cancel {
    let child = Cancel::with_deadline(timeout);
    if parent.is_cancelled() {
        child.cancel();
        return child;
    }
    let parent = parent.clone();
    let watcher = child.clone();
    std::thread::spawn(move || {
        loop {
            if parent.is_cancelled() {
                watcher.cancel();
                return;
            }
            if watcher.wait(Duration::from_millis(25)).is_err() {
                return;
            }
        }
    });
    child
}
