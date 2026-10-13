//! Ports `go\internal\metadata\tvmaze_test.go`.

use std::sync::Arc;

use metadata_core::types::{Episode, Kind, Query};
use metadata_http::cancel::Cancel;

use crate::resolver::Source;
use crate::testsupport::{FakeServer, Request, Response, TempDir, http_client, resolver_with};
use crate::tvmaze::TvmazeSource;

/// `TestTVMazeSeason`: the lowercased `q`, the season lookup, and the resolver
/// carrying the matched episode title.
#[test]
fn tvmaze_resolves_one_season() {
    let server = FakeServer::start(|req: &Request| match req.path.as_str() {
        "/search/shows" => {
            if req.query("q").as_deref() != Some("breaking bad") {
                Response::json("[]")
            } else {
                Response::json(
                    r#"[{"score":1.0,"show":{"id":169,"name":"Breaking Bad","premiered":"2008-01-20","weight":99}}]"#,
                )
            }
        }
        "/shows/169/seasons" => Response::json(r#"[{"id":753,"number":1},{"id":754,"number":2}]"#),
        "/seasons/753/episodes" => Response::json(
            r#"[{"id":12192,"name":"Pilot","season":1,"number":1},{"id":12193,"name":"Cat's in the Bag...","season":1,"number":2}]"#,
        ),
        _ => Response::error(404, "not found"),
    });

    let source = TvmazeSource::new(server.url(), http_client());
    let q = Query {
        kind: Kind::SERIES,
        title: "Breaking Bad".to_string(),
        season: 1,
        episode: 1,
        ..Query::default()
    };
    let media = source
        .find(&Cancel::background(), &q)
        .expect("Find")
        .expect("expected media, got nil");
    assert_eq!(media.title, "Breaking Bad");
    assert_eq!(media.year, 2008);
    assert_eq!(media.source_id, "169");
    assert_eq!(
        media.episodes,
        vec![
            Episode {
                season: 1,
                number: 1,
                title: "Pilot".to_string(),
                ..Episode::default()
            },
            Episode {
                season: 1,
                number: 2,
                title: "Cat's in the Bag...".to_string(),
                ..Episode::default()
            },
        ]
    );

    // A movie query is gated before the wire.
    source
        .find(
            &Cancel::background(),
            &Query {
                kind: Kind::MOVIE,
                title: "Inception".to_string(),
                ..Query::default()
            },
        )
        .expect("movie Find");

    let dir = TempDir::new();
    let resolver = resolver_with(
        &dir.path(),
        vec![Arc::new(TvmazeSource::new(server.url(), http_client()))],
    );
    let res = resolver
        .lookup_now(&Cancel::background(), &q)
        .expect("LookupNow");
    assert!(
        res.found && res.series == "Breaking Bad" && res.episode == "Pilot",
        "resolution = {res:?}"
    );
}

/// `TestTVMazeAbsoluteAndNotFound`: absolute numbering renumbers in air order,
/// and an empty search is a definitive miss.
#[test]
fn tvmaze_absolute_renumbers_and_missing_is_a_miss() {
    let server = FakeServer::start(|req: &Request| match req.path.as_str() {
        "/search/shows" => {
            if req.raw_query.contains("Missing") || req.raw_query.contains("missing") {
                Response::json("[]")
            } else {
                Response::json(
                    r#"[{"score":1.0,"show":{"id":169,"name":"Show","premiered":"2008-01-20","weight":99}}]"#,
                )
            }
        }
        "/shows/169/episodes" => Response::json(
            r#"[{"id":1,"name":"S1E1","season":1,"number":1},{"id":2,"name":"S1E2","season":1,"number":2},{"id":3,"name":"S2E1","season":2,"number":1}]"#,
        ),
        _ => Response::error(404, "not found"),
    });

    let source = TvmazeSource::new(server.url(), http_client());
    let q = Query {
        kind: Kind::SERIES,
        title: "Show".to_string(),
        episode: 3,
        absolute: true,
        ..Query::default()
    };
    let media = source
        .find(&Cancel::background(), &q)
        .expect("Find")
        .expect("absolute media");
    assert_eq!(media.episodes.len(), 3);
    // Air order is (1,1) (1,2) (2,1); absolute 3 is the third entry.
    assert_eq!(
        media
            .episodes
            .iter()
            .map(|ep| ep.number)
            .collect::<Vec<_>>(),
        vec![1, 2, 3]
    );

    let dir = TempDir::new();
    let resolver = resolver_with(
        &dir.path(),
        vec![Arc::new(TvmazeSource::new(server.url(), http_client()))],
    );
    let res = resolver
        .lookup_now(&Cancel::background(), &q)
        .expect("LookupNow");
    assert!(
        res.found && res.episode == "S2E1",
        "absolute resolution = {res:?}, want S2E1"
    );

    let miss = source
        .find(
            &Cancel::background(),
            &Query {
                kind: Kind::SERIES,
                title: "Missing Show".to_string(),
                ..Query::default()
            },
        )
        .expect("not-found Find");
    assert!(miss.is_none(), "not-found = {miss:?}, want None");
}

/// `TestTVMazeSearchRankingReZero`: the live misranking -- the "Break Time"
/// spinoff (weight 80) listed above the main show (weight 98) -- still resolves
/// to the main show.
#[test]
fn tvmaze_search_ranking_picks_the_main_show() {
    let server = FakeServer::start(|req: &Request| match req.path.as_str() {
        "/search/shows" => Response::json(
            r#"[{"score":0.6979369,"show":{"id":82431,"name":"Re:Zero kara Hajimeru Break Time","premiered":"2016-04-08","weight":80}},{"score":0.669693,"show":{"id":14459,"name":"Re:ZERO -Starting Life in Another World-","premiered":"2016-04-04","weight":98}}]"#,
        ),
        "/shows/14459/seasons" => Response::json(r#"[{"id":14459,"number":1}]"#),
        "/seasons/14459/episodes" => Response::json(
            r#"[{"id":1,"name":"The End of the Beginning and the Beginning of the End - 1","season":1,"number":1}]"#,
        ),
        _ => Response::error(404, "not found"),
    });

    let source = TvmazeSource::new(server.url(), http_client());
    let media = source
        .find(
            &Cancel::background(),
            &Query {
                kind: Kind::SERIES,
                title: "Re Zero".to_string(),
                season: 1,
                episode: 1,
                ..Query::default()
            },
        )
        .expect("Find")
        .expect("expected media, got nil (low-confidence fallthrough misfired)");
    assert_eq!(media.source_id, "14459");
    assert_eq!(media.title, "Re:ZERO -Starting Life in Another World-");
}

/// `TestTVMazeLowConfidenceFallsThrough`: a best similarity below ~0.5 is a
/// not-found so the resolver tries the next source.
#[test]
fn tvmaze_low_confidence_falls_through() {
    let server = FakeServer::start(|_req: &Request| {
        Response::json(
            r#"[{"score":0.9,"show":{"id":14459,"name":"Re:ZERO -Starting Life in Another World-","premiered":"2016-04-04","weight":98}}]"#,
        )
    });
    let source = TvmazeSource::new(server.url(), http_client());
    let media = source
        .find(
            &Cancel::background(),
            &Query {
                kind: Kind::SERIES,
                title: "Breaking Bad".to_string(),
                season: 1,
                episode: 1,
                ..Query::default()
            },
        )
        .expect("low-confidence Find");
    assert!(media.is_none(), "low-confidence = {media:?}, want None");
}

/// `TestTVMazeEnrichment`: genres/length/urls, the matched episode URL on the
/// Media, and the resolver's View-Episode/View-Show buttons.
#[test]
fn tvmaze_enrichment_carries_genres_length_and_urls() {
    let server = FakeServer::start(|req: &Request| match req.path.as_str() {
        "/search/shows" => Response::json(
            r#"[{"score":1.0,"show":{"id":169,"name":"Breaking Bad","premiered":"2008-01-20","weight":99,"genres":["Drama","Crime"],"averageRuntime":45,"runtime":60,"url":"http://www.tvmaze.com/shows/169/breaking-bad"}}]"#,
        ),
        "/shows/169/seasons" => Response::json(r#"[{"id":753,"number":1}]"#),
        "/seasons/753/episodes" => Response::json(
            r#"[{"id":12192,"name":"Pilot","season":1,"number":1,"url":"http://www.tvmaze.com/episodes/12192/breaking-bad-1x01-pilot","runtime":45}]"#,
        ),
        _ => Response::error(404, "not found"),
    });

    let source = TvmazeSource::new(server.url(), http_client());
    let q = Query {
        kind: Kind::SERIES,
        title: "Breaking Bad".to_string(),
        season: 1,
        episode: 1,
        ..Query::default()
    };
    let media = source
        .find(&Cancel::background(), &q)
        .expect("Find")
        .expect("expected media, got nil");
    assert_eq!(media.genres, "Drama \u{00b7} Crime");
    assert_eq!(media.length, "45 min");
    assert_eq!(
        media.show_url,
        "https://www.tvmaze.com/shows/169/breaking-bad"
    );
    assert_eq!(
        media.episodes.first().map(|ep| ep.url.clone()),
        Some("https://www.tvmaze.com/episodes/12192/breaking-bad-1x01-pilot".to_string())
    );
    assert_eq!(media.episodes[0].length, "45 min");
    assert_eq!(
        media.episode_url,
        "https://www.tvmaze.com/episodes/12192/breaking-bad-1x01-pilot"
    );

    let dir = TempDir::new();
    let resolver = resolver_with(
        &dir.path(),
        vec![Arc::new(TvmazeSource::new(server.url(), http_client()))],
    );
    let res = resolver
        .lookup_now(&Cancel::background(), &q)
        .expect("LookupNow");
    assert!(
        res.found
            && res.genres == "Drama \u{00b7} Crime"
            && res.length == "45 min"
            && res.year == 2008
            && !res.is_movie,
        "resolution = {res:?}, want genres/length/year"
    );
    assert_eq!(res.buttons.len(), 2, "buttons = {:?}", res.buttons);
    assert_eq!(res.buttons[0].label, "View Episode");
    assert_eq!(res.buttons[1].label, "View Show");
    for button in &res.buttons {
        assert!(
            button.url.starts_with("https://"),
            "button not https: {button:?}"
        );
    }
}
