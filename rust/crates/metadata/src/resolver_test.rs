//! Ports `go\internal\metadata\resolver_test.go`.

use std::sync::atomic::Ordering;
use std::sync::Arc;
use std::time::{Duration, Instant, SystemTime};

use metadata_core::types::{Episode, Kind, Media, Query};
use metadata_http::cancel::Cancel;

use crate::cache::{
    cache_exists, cache_key, cache_live, cache_load, cache_load_entry, cache_record, cache_store,
    cache_store_negative, with_cache, CacheFile, CACHE_SCHEMA, NEGATIVE_TTL,
};
use crate::resolver::{match_episode, Resolver, Source, NEGATIVE_BACKOFF};
use crate::testsupport::{
    correct_media, mislabeled_media, resolver_with, series_media, FakeSource, TempDir,
};
use crate::timefmt::{format_rfc3339, now_rfc3339};

fn show_query(season: i64, episode: i32) -> Query {
    Query {
        kind: Kind::SERIES,
        title: "Show".to_string(),
        season,
        episode,
        ..Query::default()
    }
}

/// `TestResolveNonBlockingThenMemory`: `resolve` returns at once on a miss even
/// while the fetch is stuck, then serves from memory once the fetch lands
/// without a second fetch.
#[test]
fn resolve_non_blocking_then_memory() {
    let block = crate::resolver::Done::new();
    let fake = FakeSource {
        block: Some(Arc::clone(&block)),
        media: Some(series_media()),
        ..FakeSource::new("fake")
    };
    let calls = fake.counter();
    let dir = TempDir::new();
    let resolver = resolver_with(&dir.path(), vec![fake.shared()]);
    let q = show_query(1, 2);

    let res = resolver
        .resolve(&Cancel::background(), &q)
        .expect("Resolve");
    assert!(!res.found, "expected Found=false on cache miss");

    // The background fetch is stuck in the fake; LookupNow must wait for it.
    let waiter = resolver.clone();
    let waiter_q = q.clone();
    let handle = std::thread::spawn(move || waiter.lookup_now(&Cancel::background(), &waiter_q));
    std::thread::sleep(Duration::from_millis(100));
    block.close();
    let out = handle
        .join()
        .expect("LookupNow thread")
        .expect("LookupNow after fetch released");
    assert!(
        out.found && out.series == "Show" && out.episode == "Pilot",
        "LookupNow = {out:?}, want Show/Pilot found"
    );

    let res2 = resolver
        .resolve(&Cancel::background(), &q)
        .expect("Resolve after fetch");
    assert!(res2.found && res2.episode == "Pilot", "resolve = {res2:?}");
    assert_eq!(
        calls.load(Ordering::SeqCst),
        1,
        "exactly one fetch (served from memory afterwards)"
    );
}

/// `TestResolveSingleFlight`: two concurrent Resolves on a miss fire exactly one
/// fetch.
#[test]
fn resolve_single_flight() {
    let fake = FakeSource {
        delay: Duration::from_millis(300),
        media: Some(series_media()),
        ..FakeSource::new("fake")
    };
    let calls = fake.counter();
    let dir = TempDir::new();
    let resolver = resolver_with(&dir.path(), vec![fake.shared()]);
    let q = show_query(1, 2);

    let mut handles = Vec::new();
    for _ in 0..2 {
        let resolver = resolver.clone();
        let q = q.clone();
        handles.push(std::thread::spawn(move || {
            resolver
                .resolve(&Cancel::background(), &q)
                .expect("Resolve")
        }));
    }
    for handle in handles {
        handle.join().expect("Resolve thread");
    }

    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        match calls.load(Ordering::SeqCst) {
            1 => break,
            n if n > 1 => panic!("HTTP calls = {n}, want 1"),
            _ => {}
        }
        assert!(Instant::now() < deadline, "background fetch never ran");
        std::thread::sleep(Duration::from_millis(20));
    }

    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        let res = resolver
            .resolve(&Cancel::background(), &q)
            .expect("Resolve after fetch");
        if res.found {
            assert_eq!(res.episode, "Pilot");
            break;
        }
        assert!(
            Instant::now() < deadline,
            "Resolve never served from memory"
        );
        std::thread::sleep(Duration::from_millis(20));
    }
    assert_eq!(calls.load(Ordering::SeqCst), 1, "exactly one fetch");
}

/// `TestMatchEpisodeNeverPositional`: Numbers 51..66 must never answer Number 1
/// via a positional index; Number 51 itself must still match.
#[test]
fn match_episode_never_positional() {
    let eps = mislabeled_media().episodes;
    assert!(
        match_episode(
            &eps,
            &Query {
                kind: Kind::SERIES,
                title: "Re Zero".to_string(),
                episode: 1,
                absolute: true,
                ..Query::default()
            }
        )
        .is_none(),
        "matchEpisode(51..66, ep1) matched: positional short-circuit present"
    );
    let matched = match_episode(
        &eps,
        &Query {
            kind: Kind::SERIES,
            title: "Re Zero".to_string(),
            episode: 51,
            absolute: true,
            ..Query::default()
        },
    )
    .expect("ep51 must match");
    assert_eq!(matched.title, "Theatrical Malice");
}

/// `TestCrossSourceFallthrough`: the mislabeled first source is skipped and the
/// second source carrying Number 1 answers.
#[test]
fn cross_source_fallthrough() {
    let first = FakeSource::with_media("fake-first", mislabeled_media());
    let second = FakeSource::with_media("fake-second", correct_media());
    let first_calls = first.counter();
    let second_calls = second.counter();
    let dir = TempDir::new();
    let resolver = resolver_with(&dir.path(), vec![first.shared(), second.shared()]);
    let q = Query {
        kind: Kind::SERIES,
        title: "Re Zero".to_string(),
        episode: 1,
        absolute: true,
        ..Query::default()
    };
    let res = resolver
        .lookup_now(&Cancel::background(), &q)
        .expect("LookupNow");
    assert!(
        res.found && res.episode == "True S1E1",
        "resolution = {res:?}, want True S1E1 found"
    );
    assert_eq!(first_calls.load(Ordering::SeqCst), 1);
    assert_eq!(second_calls.load(Ordering::SeqCst), 1, "fallthrough");
}

/// `TestCrossSourceNotFound`: no source having the Number must return
/// Found=false (never a wrong title).
#[test]
fn cross_source_not_found() {
    let first = FakeSource::with_media("fake-first", mislabeled_media());
    let second = FakeSource::with_media("fake-second", mislabeled_media());
    let dir = TempDir::new();
    let resolver = resolver_with(&dir.path(), vec![first.shared(), second.shared()]);
    let res = resolver
        .lookup_now(
            &Cancel::background(),
            &Query {
                kind: Kind::SERIES,
                title: "Re Zero".to_string(),
                episode: 1,
                absolute: true,
                ..Query::default()
            },
        )
        .expect("LookupNow");
    assert!(!res.found, "resolution = {res:?}, want Found=false");
    assert_ne!(res.episode, "The Result of the Battle for Priestella");
}

/// `TestOrderedSourcesTVMazeFirst`: TVmaze first for Absolute and for
/// !Absolute series queries.
#[test]
fn ordered_sources_tvmaze_first() {
    let dir = TempDir::new();
    let resolver = resolver_with(
        &dir.path(),
        vec![
            FakeSource::new("anilist").shared(),
            FakeSource::new("jikan").shared(),
            FakeSource::new("tvmaze").shared(),
            FakeSource::new("itunes").shared(),
        ],
    );
    for q in [
        Query {
            kind: Kind::SERIES,
            title: "Show".to_string(),
            episode: 1,
            absolute: true,
            ..Query::default()
        },
        show_query(1, 1),
        Query {
            kind: Kind::SERIES,
            title: "Show".to_string(),
            episode: 3,
            absolute: true,
            ..Query::default()
        },
    ] {
        let names: Vec<String> = resolver
            .ordered_sources(&q)
            .iter()
            .map(|source| source.name().to_string())
            .collect();
        assert!(
            names.len() >= 3
                && names[0] == "tvmaze"
                && names[1] == "anilist"
                && names[2] == "jikan",
            "orderedSources({q:?}) = {names:?}, want [tvmaze anilist jikan ...]"
        );
    }
}

/// `TestResolutionButtonsSeriesAndMovie`: button labels, https forcing, the <=2
/// cap, and the anime/movie variants.
#[test]
fn resolution_buttons_series_and_movie() {
    let series = FakeSource::with_media(
        "tvmaze",
        Media {
            source: "tvmaze".to_string(),
            source_id: "1".to_string(),
            kind: Kind::SERIES,
            title: "Show".to_string(),
            year: 2008,
            genres: "Drama \u{00b7} Crime".to_string(),
            length: "45 min".to_string(),
            show_url: "http://example.com/show".to_string(),
            episode_url: "http://example.com/ep1".to_string(),
            artwork: "https://example.com/poster.jpg".to_string(),
            episodes: vec![Episode {
                season: 1,
                number: 1,
                title: "Pilot".to_string(),
                url: "http://example.com/ep1".to_string(),
                length: "45 min".to_string(),
                artwork: "https://example.com/still.jpg".to_string(),
                ..Episode::default()
            }],
            fetched_at: now_rfc3339(),
            ..Media::default()
        },
    );
    let dir = TempDir::new();
    let resolver = resolver_with(&dir.path(), vec![series.shared()]);
    let res = resolver
        .lookup_now(&Cancel::background(), &show_query(1, 1))
        .expect("series LookupNow");
    assert!(res.found, "series LookupNow = {res:?}");
    assert_eq!(res.genres, "Drama \u{00b7} Crime");
    assert_eq!(res.length, "45 min");
    assert_eq!(res.year, 2008);
    assert!(!res.is_movie);
    assert_eq!(res.buttons.len(), 2, "series buttons = {:?}", res.buttons);
    assert_eq!(res.buttons[0].label, "View Episode");
    assert_eq!(res.buttons[1].label, "View Show");
    for button in &res.buttons {
        assert!(
            button.url.starts_with("https://"),
            "series button not https: {button:?}"
        );
    }
    // Cover rework: the series poster wins, the episode still never overrides.
    assert_eq!(res.artwork, "https://example.com/poster.jpg");

    let anime = FakeSource::with_media(
        "anilist",
        Media {
            source: "anilist".to_string(),
            source_id: "21".to_string(),
            kind: Kind::SERIES,
            title: "Anime".to_string(),
            year: 2016,
            genres: "Action".to_string(),
            length: "24 min".to_string(),
            show_url: "https://myanimelist.net/anime/21".to_string(),
            episodes: vec![Episode {
                number: 1,
                title: "First".to_string(),
                url: "http://example.com/stream1".to_string(),
                ..Episode::default()
            }],
            fetched_at: now_rfc3339(),
            ..Media::default()
        },
    );
    let dir2 = TempDir::new();
    let resolver2 = resolver_with(&dir2.path(), vec![anime.shared()]);
    let res2 = resolver2
        .lookup_now(
            &Cancel::background(),
            &Query {
                kind: Kind::SERIES,
                title: "Anime".to_string(),
                episode: 1,
                absolute: true,
                ..Query::default()
            },
        )
        .expect("anime LookupNow");
    assert!(res2.found, "anime LookupNow = {res2:?}");
    assert_eq!(res2.buttons.len(), 2, "anime buttons = {:?}", res2.buttons);
    assert_eq!(res2.buttons[0].label, "View Episode");
    assert_eq!(res2.buttons[1].label, "View Anime");

    let movie = FakeSource::with_media(
        "itunes",
        Media {
            source: "itunes".to_string(),
            source_id: "Blade Runner 2049".to_string(),
            kind: Kind::MOVIE,
            title: "Blade Runner 2049".to_string(),
            year: 2017,
            genres: "Sci-Fi".to_string(),
            length: "2h 19m".to_string(),
            movie_url: "http://itunes.apple.com/movie/id123".to_string(),
            trailer_url: "ftp://example.com/trailer".to_string(),
            artwork: "https://example.com/movie.jpg".to_string(),
            fetched_at: now_rfc3339(),
            ..Media::default()
        },
    );
    let dir3 = TempDir::new();
    let resolver3 = resolver_with(&dir3.path(), vec![movie.shared()]);
    let res3 = resolver3
        .lookup_now(
            &Cancel::background(),
            &Query {
                kind: Kind::MOVIE,
                title: "Blade Runner 2049".to_string(),
                year: 2017,
                ..Query::default()
            },
        )
        .expect("movie LookupNow");
    assert!(res3.found && res3.is_movie && res3.year == 2017);
    // An ftp trailer drops: only the https-forced movie button remains.
    assert_eq!(
        res3.buttons.len(),
        1,
        "movie buttons = {:?}, want a single View Movie",
        res3.buttons
    );
    assert_eq!(res3.buttons[0].label, "View Movie");
    assert_eq!(res3.buttons[0].url, "https://itunes.apple.com/movie/id123");
}

/// `TestNegativePersistedAndColdHitNoNetwork`: a not-found lookup persists a
/// negative entry and a second lookup in a NEW resolver serves the miss with
/// zero provider calls.
#[test]
fn negative_persisted_and_cold_hit_no_network() {
    let dir = TempDir::new();
    let d = dir.path();
    let q = Query {
        kind: Kind::SERIES,
        title: "No Such Show Ever".to_string(),
        season: 1,
        episode: 1,
        ..Query::default()
    };
    let miss = FakeSource::new("fake");
    let miss_calls = miss.counter();
    let r1 = resolver_with(&d, vec![miss.shared()]);
    let res = r1
        .lookup_now(&Cancel::background(), &q)
        .expect("run 1 LookupNow");
    assert!(!res.found, "run 1 = {res:?}, want miss");
    assert_eq!(miss_calls.load(Ordering::SeqCst), 1);

    let key = cache_key(&q);
    let data = cache_record(&d, &key).expect("negative entry missing from the store");
    let record: CacheFile = serde_json::from_slice(&data).expect("negative entry must parse");
    assert!(record.not_found, "negative entry NotFound = false");
    assert_eq!(record.key, key);
    assert_eq!(record.schema, CACHE_SCHEMA);

    let counting = FakeSource::new("fake");
    let counting_calls = counting.counter();
    let r2 = resolver_with(&d, vec![counting.shared()]);
    let res2 = r2
        .lookup_now(&Cancel::background(), &q)
        .expect("run 2 LookupNow");
    assert!(!res2.found, "run 2 = {res2:?}, want cached miss");
    assert_eq!(
        counting_calls.load(Ordering::SeqCst),
        0,
        "a cached negative must not touch the network"
    );
    let (_resolution, from_cache) = r2
        .lookup_now_force_src(&Cancel::background(), &q, false)
        .expect("provenance probe");
    assert!(!from_cache, "a blocking miss reports fromCache=false");
    assert_eq!(counting_calls.load(Ordering::SeqCst), 0);
}

/// `TestNegativeForceReattemptsAndReplaces`: force=true bypasses the persisted
/// miss, re-attempts the provider, and replaces the negative with the positive.
#[test]
fn negative_force_reattempts_and_replaces() {
    let dir = TempDir::new();
    let d = dir.path();
    let q = Query {
        kind: Kind::SERIES,
        title: "Late Bloomer".to_string(),
        season: 1,
        episode: 2,
        ..Query::default()
    };
    let miss = FakeSource::new("fake");
    let r1 = resolver_with(&d, vec![miss.shared()]);
    let res = r1.lookup_now(&Cancel::background(), &q).expect("seed miss");
    assert!(!res.found, "seed miss = {res:?}");
    let key = cache_key(&q);
    let entry = cache_load_entry(&d, &key).expect("seed negative entry");
    assert!(entry.not_found);

    let now = FakeSource::with_media("fake", series_media());
    let now_calls = now.counter();
    let r2 = resolver_with(&d, vec![now.shared()]);
    let res = r2
        .lookup_now_force(&Cancel::background(), &q, true)
        .expect("force LookupNow");
    assert!(res.found && res.episode == "Pilot", "force = {res:?}");
    assert_eq!(now_calls.load(Ordering::SeqCst), 1, "re-attempted");
    let entry = cache_load_entry(&d, &key).expect("replaced entry");
    assert!(!entry.not_found && entry.media.is_some());
    assert!(cache_load(&d, &key).is_some());
}

/// `TestNegativeExpiredReattempted`: an expired negative entry is treated as
/// absent, so the provider IS called again.
#[test]
fn negative_expired_reattempted() {
    let dir = TempDir::new();
    let d = dir.path();
    let q = Query {
        kind: Kind::SERIES,
        title: "Stale Miss Show".to_string(),
        season: 1,
        episode: 1,
        ..Query::default()
    };
    let key = cache_key(&q);
    let data = serde_json::to_vec(&CacheFile {
        key: key.clone(),
        schema: CACHE_SCHEMA,
        not_found: true,
        checked_at: format_rfc3339(SystemTime::now() - NEGATIVE_TTL - Duration::from_secs(3600)),
        ..CacheFile::default()
    })
    .unwrap();
    crate::cache_test::store_raw_record(&d, &key, &data);
    let fake = FakeSource::new("fake");
    let calls = fake.counter();
    let resolver = resolver_with(&d, vec![fake.shared()]);
    let res = resolver
        .lookup_now(&Cancel::background(), &q)
        .expect("LookupNow");
    assert!(!res.found, "expired-negative lookup = {res:?}, want miss");
    assert_eq!(
        calls.load(Ordering::SeqCst),
        1,
        "expired entry re-attempted"
    );
}

/// `TestProviderErrorPersistsNothing`: a transport error is transient, never a
/// confirmed not-found.
#[test]
fn provider_error_persists_nothing() {
    let dir = TempDir::new();
    let d = dir.path();
    let q = Query {
        kind: Kind::SERIES,
        title: "Flaky Show".to_string(),
        season: 1,
        episode: 2,
        ..Query::default()
    };
    let key = cache_key(&q);
    let boom = FakeSource::with_error("fake", "boom");
    let calls = boom.counter();
    let resolver = resolver_with(&d, vec![boom.shared()]);
    let err = resolver
        .lookup_now(&Cancel::background(), &q)
        .expect_err("LookupNow err = nil, want ErrLookupUnavailable");
    assert!(
        err.is_lookup_unavailable(),
        "LookupNow err = {err}, want ErrLookupUnavailable"
    );
    assert!(!crate::cache::cache_has(&d, &key));
    assert!(!cache_live(&d, &key));
    assert!(
        !resolver.neg_suppressed(&key),
        "provider error set in-memory suppression, want none"
    );
    let err2 = resolver
        .lookup_now(&Cancel::background(), &q)
        .expect_err("second LookupNow err = nil");
    assert!(err2.is_lookup_unavailable());
    assert_eq!(calls.load(Ordering::SeqCst), 2, "no negative cached");
}

/// `TestProviderTimeoutPersistsNothing`: a timed-out fetch persists nothing.
#[test]
fn provider_timeout_persists_nothing() {
    let dir = TempDir::new();
    let d = dir.path();
    let q = Query {
        kind: Kind::SERIES,
        title: "Slow Show".to_string(),
        season: 1,
        episode: 2,
        ..Query::default()
    };
    let key = cache_key(&q);
    let slow = FakeSource {
        delay: Duration::from_millis(500),
        ..FakeSource::new("fake")
    };
    let resolver = resolver_with(&d, vec![slow.shared()]);
    let cancel = Cancel::with_deadline(Duration::from_millis(30));
    let err = resolver
        .lookup_now(&cancel, &q)
        .expect_err("timed-out LookupNow err = nil");
    assert!(!err.message().is_empty());
    assert!(!crate::cache::cache_has(&d, &key));
    assert!(!cache_live(&d, &key));
}

/// `TestFailedForcedRefreshPreservesPositive`: a failed forced refresh leaves an
/// existing stored positive byte-identical, and the positive still serves.
#[test]
fn failed_forced_refresh_preserves_positive() {
    let dir = TempDir::new();
    let d = dir.path();
    let q = show_query(1, 2);
    let key = cache_key(&q);
    cache_store(&d, &key, &series_media()).expect("seed cacheStore");
    let before = cache_record(&d, &key).expect("seed record missing");

    let boom = FakeSource::with_error("fake", "transport down");
    let resolver = resolver_with(&d, vec![boom.shared()]);
    // Warm the in-memory positive from disk (no provider call).
    let res = resolver
        .lookup_now(&Cancel::background(), &q)
        .expect("warm LookupNow");
    assert!(res.found, "warm LookupNow = {res:?}");
    let err = resolver
        .lookup_now_force(&Cancel::background(), &q, true)
        .expect_err("failed force must be transient");
    assert!(err.is_lookup_unavailable(), "err = {err}");
    let after = cache_record(&d, &key).expect("stored record vanished");
    assert_eq!(&after[..], &before[..], "stored positive clobbered");
    assert!(cache_live(&d, &key));
    let res2 = resolver
        .lookup_now(&Cancel::background(), &q)
        .expect("post-force LookupNow");
    assert!(res2.found && res2.episode == "Pilot", "{res2:?}");
}

/// `TestCancelledBackgroundFetchStoresNothing`: a cancelled/timed-out background
/// fetch stores no negative record and sets no suppression.
#[test]
fn cancelled_background_fetch_stores_nothing() {
    let dir = TempDir::new();
    let d = dir.path();
    let q = show_query(1, 2);
    let key = cache_key(&q);
    let slow = FakeSource {
        delay: Duration::from_millis(500),
        ..FakeSource::new("fake")
    };
    // fetchCtx timeout = timeout * 4 = 80 ms, well under the 500 ms delay.
    let resolver = Resolver::new(crate::resolver::Options {
        dir: d.clone(),
        timeout: Duration::from_millis(20),
        sources: Some(vec![slow.shared()]),
        ..crate::resolver::Options::default()
    });
    let done = crate::resolver::Done::new();
    resolver.set_inflight(&key, Arc::clone(&done));
    let worker = resolver.clone();
    let worker_q = q.clone();
    let worker_key = key.clone();
    let worker_done = Arc::clone(&done);
    std::thread::spawn(move || {
        worker.background_fetch(Cancel::background(), worker_q, &worker_key, worker_done)
    });

    // The fetch must finish inside this gate; a gate that fires instead means
    // the background fetch never completed.
    let gate = Cancel::with_deadline(Duration::from_secs(5));
    done.wait(&gate);
    assert!(!gate.is_cancelled(), "backgroundFetch never finished");
    assert!(
        !crate::cache::cache_has(&d, &key),
        "cancelled backgroundFetch stored a record, want nothing"
    );
    assert!(
        !resolver.neg_suppressed(&key),
        "cancelled backgroundFetch set suppression, want none"
    );
}

/// `TestNegativeBackoffBoundedByTTL`: the in-memory suppression never outlives
/// the stored TTL expiry.
#[test]
fn negative_backoff_bounded_by_ttl() {
    let dir = TempDir::new();
    let d = dir.path();
    let q = Query {
        kind: Kind::SERIES,
        title: "Near Expiry".to_string(),
        season: 1,
        episode: 1,
        ..Query::default()
    };
    let key = cache_key(&q);
    let checked_at = SystemTime::now() - NEGATIVE_TTL + Duration::from_secs(120);
    let data = serde_json::to_vec(&CacheFile {
        key: key.clone(),
        schema: CACHE_SCHEMA,
        not_found: true,
        checked_at: format_rfc3339(checked_at),
        ..CacheFile::default()
    })
    .unwrap();
    crate::cache_test::store_raw_record(&d, &key, &data);
    let counting = FakeSource::new("fake");
    let calls = counting.counter();
    let resolver = resolver_with(&d, vec![counting.shared()]);
    let res = resolver
        .lookup_now(&Cancel::background(), &q)
        .expect("near-expiry LookupNow");
    assert!(
        !res.found,
        "near-expiry LookupNow = {res:?}, want cached miss"
    );
    assert_eq!(
        calls.load(Ordering::SeqCst),
        0,
        "fresh negative, no network"
    );
    let expiry = resolver
        .neg_expiry(&key)
        .expect("near-expiry miss set no in-memory suppression");
    let ttl_expiry = checked_at + NEGATIVE_TTL;
    assert!(
        expiry <= ttl_expiry + Duration::from_secs(5),
        "in-memory expiry {expiry:?} outlives TTL expiry {ttl_expiry:?}"
    );
    assert!(
        expiry < SystemTime::now() + NEGATIVE_BACKOFF,
        "in-memory expiry must be well under now+10m"
    );
}

/// `TestTransientVsDefinitiveMissDistinguishable`: a transient failure returns
/// the sentinel (wrapping the cause) and stores nothing, while a definitive miss
/// returns a nil error and stores exactly one negative.
#[test]
fn transient_vs_definitive_miss_distinguishable() {
    let dir = TempDir::new();
    let d = dir.path();
    let count_records = || -> i32 {
        if !cache_exists(&d) {
            return 0;
        }
        let mut n = 0;
        with_cache(&d, |db| {
            n = db.len() as i32;
            Ok(())
        })
        .expect("count stored records");
        n
    };

    let flaky = resolver_with(&d, vec![FakeSource::with_error("fake", "boom").shared()]);
    let q_flaky = Query {
        kind: Kind::SERIES,
        title: "Flaky Show".to_string(),
        season: 1,
        episode: 2,
        ..Query::default()
    };
    let err = flaky
        .lookup_now(&Cancel::background(), &q_flaky)
        .expect_err("transient err = nil");
    assert!(err.is_lookup_unavailable(), "transient err = {err}");
    assert!(
        err.message().contains("boom"),
        "transient err = {err:?}, want the underlying cause propagated"
    );
    assert_eq!(count_records(), 0, "transient stored records");

    let empty = resolver_with(&d, Vec::new());
    let q_missing = Query {
        kind: Kind::SERIES,
        title: "Missing Show".to_string(),
        season: 1,
        episode: 2,
        ..Query::default()
    };
    let res = empty
        .lookup_now(&Cancel::background(), &q_missing)
        .expect("definitive miss err must be nil");
    assert!(!res.found, "definitive miss = {res:?}, want miss");
    assert_eq!(count_records(), 1, "exactly one negative");
    assert!(cache_live(&d, &cache_key(&q_missing)));
}

/// `TestProbedEntryDeletedNotServed`: an entry invalidated after `ProbeCache`
/// warmed memory is never served from that memory.
#[test]
fn probed_entry_deleted_not_served() {
    let dir = TempDir::new();
    let d = dir.path();
    let q = Query {
        kind: Kind::SERIES,
        title: "Probe Del".to_string(),
        season: 1,
        episode: 1,
        ..Query::default()
    };
    let key = cache_key(&q);
    let media = Media {
        source: "tvmaze".to_string(),
        source_id: "1".to_string(),
        kind: Kind::SERIES,
        title: "Probe Del".to_string(),
        episodes: vec![Episode {
            season: 1,
            number: 1,
            title: "Pilot".to_string(),
            ..Episode::default()
        }],
        fetched_at: now_rfc3339(),
        ..Media::default()
    };
    cache_store(&d, &key, &media).expect("cacheStore");
    let resolver = Resolver::new(crate::resolver::Options {
        dir: d.clone(),
        sources: Some(vec![FakeSource::new("fake").shared()]),
        offline: true,
        ..crate::resolver::Options::default()
    });
    assert!(resolver.probe_cache(&key), "ProbeCache on a live positive");
    crate::cache::clear_cache(&d).expect("ClearCache");
    assert!(!crate::cache::cache_has(&d, &key));
    let res = resolver
        .resolve(&Cancel::background(), &q)
        .expect("Resolve after invalidation");
    assert!(!res.found, "want not-found, never warmed memory: {res:?}");

    // Same, via the blocking lookup on a fresh resolver: the fetch is
    // re-attempted instead of serving warmed memory.
    cache_store(&d, &key, &media).expect("cacheStore");
    let fake = FakeSource::with_error("fake", "boom");
    let calls = fake.counter();
    let r2 = resolver_with(&d, vec![fake.shared()]);
    assert!(r2.probe_cache(&key));
    crate::cache::clear_cache(&d).expect("ClearCache");
    let err = r2
        .lookup_now_force_src(&Cancel::background(), &q, false)
        .expect_err("want a transient error (fetch re-attempted)");
    assert!(err.is_lookup_unavailable());
    assert!(
        calls.load(Ordering::SeqCst) > 0,
        "the miss must be re-attempted"
    );

    // Invalidated fresh negative: the lookup must fetch, not report a warmed
    // miss.
    let nq = Query {
        kind: Kind::SERIES,
        title: "Probe Neg".to_string(),
        season: 1,
        episode: 1,
        ..Query::default()
    };
    let nkey = cache_key(&nq);
    cache_store_negative(&d, &nkey).expect("cacheStoreNegative");
    let nfake = FakeSource::new("fake");
    let ncalls = nfake.counter();
    let r3 = resolver_with(&d, vec![nfake.shared()]);
    assert!(r3.probe_cache(&nkey), "ProbeCache on a fresh negative");
    crate::cache::clear_cache(&d).expect("ClearCache");
    let res = r3
        .lookup_now_force_src(&Cancel::background(), &nq, false)
        .expect("clean miss after a real fetch");
    assert!(!res.0.found);
    assert!(ncalls.load(Ordering::SeqCst) > 0, "want the fetch");
}

/// `Source` is object-safe: the chain holds `Arc<dyn Source>`.
#[test]
fn source_trait_is_object_safe() {
    let sources: Vec<Arc<dyn Source>> = vec![FakeSource::new("x").shared()];
    assert_eq!(sources[0].name(), "x");
}
