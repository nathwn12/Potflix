//! Ports `go\internal\metadata\chaincount_test.go`.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use metadata_core::context::resolve_query;
use metadata_core::types::{Episode, Kind, Media, Query};
use metadata_http::cancel::Cancel;

use crate::Error;
use crate::resolver::{Options, Resolver, Source};
use crate::testsupport::{TempDir, dead_url, http_client, wiki_client};
use crate::{anilist::AnilistSource, itunes::ItunesSource, jikan::JikanSource};
use crate::{tvmaze::TvmazeSource, wikipedia::WikipediaSource};

/// The answering callback shared by every fake provider in this module.
type FindFn = Arc<dyn Fn(&str, &Query) -> Option<Media> + Send + Sync>;

/// `countingSource`: a fake [`Source`] that records every `find` call by source
/// name so the resolver chain cost can be asserted per query kind.
struct CountingSource {
    name: String,
    counts: Arc<Mutex<HashMap<String, i32>>>,
    find: FindFn,
}

impl Source for CountingSource {
    fn name(&self) -> &str {
        &self.name
    }

    fn find(&self, _cancel: &Cancel, q: &Query) -> Result<Option<Media>, Error> {
        let name = self.name.clone();
        *self.counts.lock().unwrap().entry(name.clone()).or_insert(0) += 1;
        Ok((self.find)(&name, q))
    }
}

/// `countingResolver`: a Resolver whose providers are fakes named exactly like
/// the defaults, so `orderedSources` ranks them as in production.
fn counting_resolver(
    dir: &str,
    counts: &Arc<Mutex<HashMap<String, i32>>>,
    find: impl Fn(&str, &Query) -> Option<Media> + Send + Sync + 'static,
) -> Resolver {
    let find: FindFn = Arc::new(find);
    let make = |name: &str| {
        Arc::new(CountingSource {
            name: name.to_string(),
            counts: Arc::clone(counts),
            find: Arc::clone(&find),
        }) as Arc<dyn Source>
    };
    Resolver::new(Options {
        dir: dir.to_string(),
        timeout: Duration::from_secs(8),
        sources: Some(vec![
            make("tvmaze"),
            make("anilist"),
            make("jikan"),
            make("itunes"),
        ]),
        ..Options::default()
    })
}

/// `lookupCounts`: clears the tally, forces one lookup, and returns the counts.
fn lookup_counts(
    resolver: &Resolver,
    q: &Query,
    counts: &Arc<Mutex<HashMap<String, i32>>>,
) -> HashMap<String, i32> {
    counts.lock().unwrap().clear();
    resolver
        .lookup_now_force(&Cancel::background(), q, true)
        .expect("LookupNowForce");
    counts.lock().unwrap().clone()
}

/// `seriesAnswer`: an answering series media for the query's own episode.
fn series_answer(q: &Query) -> Media {
    Media {
        title: q.title.clone(),
        episodes: vec![Episode {
            season: q.season,
            number: q.episode,
            title: "Pilot".to_string(),
            ..Episode::default()
        }],
        ..Media::default()
    }
}

fn count(counts: &HashMap<String, i32>, name: &str) -> i32 {
    counts.get(name).copied().unwrap_or(0)
}

/// `TestChainCallCounts`: how many provider `Find` calls one lookup costs per
/// query kind. These are Find invocations, not network calls: each source
/// self-gates a ruled-out kind before touching the wire, so a series miss still
/// INVOKES itunes.Find but pays no rate-limiter wait.
#[test]
fn chain_call_counts() {
    let tv_q = resolve_query("Breaking Bad S01E01.mkv", "");
    assert_eq!(tv_q.kind, Kind::SERIES, "tv fixture kind");
    let anime_q = resolve_query("[Anime Time] Re Zero - 01.mkv", "");
    assert!(
        anime_q.kind == Kind::SERIES && anime_q.absolute,
        "anime fixture must be KindSeries absolute"
    );
    let movie_q = Query {
        kind: Kind::MOVIE,
        title: "Inception".to_string(),
        ..Query::default()
    };
    let miss_q = resolve_query("Xyzzy Qqq S01E01.mkv", "");
    assert_eq!(miss_q.kind, Kind::SERIES, "miss fixture kind");
    let bare_q = resolve_query("Sample.mkv", "");
    assert_eq!(bare_q.kind, Kind::UNKNOWN, "bare fixture kind");

    struct Case {
        name: &'static str,
        q: Query,
        answer_at: &'static str,
        want: Vec<(&'static str, i32)>,
    }
    let cases = vec![
        Case {
            name: "tv hit at first provider",
            q: tv_q.clone(),
            answer_at: "tvmaze",
            want: vec![("tvmaze", 1)],
        },
        Case {
            name: "movie hit at itunes",
            q: movie_q.clone(),
            answer_at: "itunes",
            want: vec![("itunes", 1)],
        },
        Case {
            name: "anime hit at anilist",
            q: anime_q.clone(),
            answer_at: "anilist",
            want: vec![("tvmaze", 1), ("anilist", 1)],
        },
        Case {
            name: "tv definitive miss invokes itunes pre-wire only",
            q: miss_q.clone(),
            answer_at: "",
            want: vec![("tvmaze", 1), ("anilist", 1), ("jikan", 1), ("itunes", 1)],
        },
        Case {
            name: "unknown definitive miss keeps full chain",
            q: bare_q.clone(),
            answer_at: "",
            want: vec![("tvmaze", 1), ("anilist", 1), ("jikan", 1), ("itunes", 1)],
        },
    ];

    for case in cases {
        let dir = TempDir::new();
        let counts: Arc<Mutex<HashMap<String, i32>>> = Arc::new(Mutex::new(HashMap::new()));
        let answer_at = case.answer_at;
        let resolver = counting_resolver(&dir.path(), &counts, move |name, q| {
            if name != answer_at {
                return None;
            }
            if q.kind == Kind::MOVIE {
                return Some(Media {
                    title: q.title.clone(),
                    ..Media::default()
                });
            }
            Some(series_answer(q))
        });
        let got = lookup_counts(&resolver, &case.q, &counts);
        for (name, expected) in &case.want {
            assert_eq!(
                count(&got, name),
                *expected,
                "{}: source {name} count (full table {got:?})",
                case.name
            );
        }
    }
}

/// `TestKindGateNoNetwork`: each source self-gates a ruled-out kind BEFORE
/// touching the wire -- against a dead server a ruled-out query returns
/// `(nil, nil)`, while an allowed query fails with a network error (the positive
/// control that proves the dead server would have been hit).
#[test]
fn kind_gate_no_network() {
    let dead = dead_url();
    let series_q = Query {
        kind: Kind::SERIES,
        title: "Breaking Bad".to_string(),
        season: 1,
        episode: 1,
        ..Query::default()
    };
    let movie_q = Query {
        kind: Kind::MOVIE,
        title: "Inception".to_string(),
        ..Query::default()
    };

    let cases: Vec<(&str, Box<dyn Source>, &Query, &Query)> = vec![
        (
            "itunes",
            Box::new(ItunesSource::new(dead.clone(), http_client())),
            &series_q,
            &movie_q,
        ),
        (
            "wikipedia",
            Box::new(WikipediaSource::new(dead.clone(), wiki_client())),
            &series_q,
            &movie_q,
        ),
        (
            "tvmaze",
            Box::new(TvmazeSource::new(dead.clone(), http_client())),
            &movie_q,
            &series_q,
        ),
        (
            "anilist",
            Box::new(AnilistSource::new(dead.clone(), http_client())),
            &movie_q,
            &series_q,
        ),
        (
            "jikan",
            Box::new(JikanSource::new(dead.clone(), http_client())),
            &movie_q,
            &series_q,
        ),
    ];
    for (name, source, ruled_out, allowed) in cases {
        let cancel = Cancel::with_deadline(Duration::from_secs(10));
        match source.find(&cancel, ruled_out) {
            Ok(None) => {}
            other => panic!("{name} ruled-out query: got {other:?}, want Ok(None)"),
        }
        match source.find(&cancel, allowed) {
            Err(err) => assert!(
                !err.message().is_empty(),
                "{name}: the allowed query must surface a wire error"
            ),
            Ok(other) => panic!("{name} allowed query: got {other:?}, want a network error"),
        }
    }
}
