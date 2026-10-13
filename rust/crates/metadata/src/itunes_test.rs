//! Ports `go\internal\metadata\itunes_test.go`.

use std::sync::{Arc, Mutex};

use metadata_core::types::{Kind, Query};
use metadata_http::cancel::Cancel;

use crate::itunes::ItunesSource;
use crate::resolver::Source;
use crate::testsupport::{FakeServer, Request, Response, TempDir, http_client, resolver_with};

/// `TestITunes`: the search path is `/search`, `media=movie` is NEVER sent, and
/// the first `feature-movie` row answers.
#[test]
fn itunes_finds_the_first_feature_movie() {
    let violations: Arc<Mutex<Vec<String>>> = Arc::new(Mutex::new(Vec::new()));
    let recorded = Arc::clone(&violations);
    let server = FakeServer::start(move |req: &Request| {
        if !req.path.starts_with("/search") {
            recorded
                .lock()
                .unwrap()
                .push(format!("path = {}, want /search", req.path));
        }
        if req.query("media").as_deref() == Some("movie") {
            recorded
                .lock()
                .unwrap()
                .push("must NOT send media=movie (it returns 0 rows)".to_string());
        }
        Response::json(
            r#"{"resultCount":2,"results":[{"kind":"song","trackName":"Noise","releaseDate":"2020-01-01T08:00:00Z"},{"kind":"feature-movie","trackName":"Inception","releaseDate":"2010-07-16T07:00:00Z"}]}"#,
        )
    });

    let source = ItunesSource::new(server.url(), http_client());
    let q = Query {
        kind: Kind::MOVIE,
        title: "Inception".to_string(),
        year: 2010,
        ..Query::default()
    };
    let media = source
        .find(&Cancel::background(), &q)
        .expect("Find")
        .expect("expected media, got nil");
    assert_eq!(media.title, "Inception");
    assert_eq!(media.year, 2010);
    assert_eq!(media.kind, Kind::MOVIE);
    assert!(
        media.episodes.is_empty(),
        "movie must have no episodes: {:?}",
        media.episodes
    );

    let dir = TempDir::new();
    let resolver = resolver_with(
        &dir.path(),
        vec![Arc::new(ItunesSource::new(server.url(), http_client()))],
    );
    let res = resolver
        .lookup_now(&Cancel::background(), &q)
        .expect("LookupNow");
    assert!(
        res.found && res.series == "Inception" && res.episode.is_empty(),
        "resolution = {res:?}"
    );
    assert_eq!(violations.lock().unwrap().len(), 0, "{violations:?}");
}

/// `TestITunesEnrichment`: `trackTimeMillis` -> "2h 19m" plus genre and
/// movie/trailer URLs, with View-Movie/View-Trailer buttons.
#[test]
fn itunes_enrichment_carries_length_urls_and_buttons() {
    let server = FakeServer::start(|_req: &Request| {
        Response::json(
            r#"{"resultCount":1,"results":[{"kind":"feature-movie","trackName":"Blade Runner 2049","releaseDate":"2017-10-06T07:00:00Z","primaryGenreName":"Sci-Fi","trackTimeMillis":8340000,"trackViewUrl":"http://itunes.apple.com/movie/id123","previewUrl":"http://example.com/trailer.mov"}]}"#,
        )
    });
    let source = ItunesSource::new(server.url(), http_client());
    let q = Query {
        kind: Kind::MOVIE,
        title: "Blade Runner 2049".to_string(),
        year: 2017,
        ..Query::default()
    };
    let media = source
        .find(&Cancel::background(), &q)
        .expect("Find")
        .expect("expected media");
    assert_eq!(media.genres, "Sci-Fi");
    assert_eq!(media.length, "2h 19m");
    assert_eq!(media.movie_url, "https://itunes.apple.com/movie/id123");
    assert_eq!(media.trailer_url, "https://example.com/trailer.mov");

    let dir = TempDir::new();
    let resolver = resolver_with(
        &dir.path(),
        vec![Arc::new(ItunesSource::new(server.url(), http_client()))],
    );
    let res = resolver
        .lookup_now(&Cancel::background(), &q)
        .expect("LookupNow");
    assert!(
        res.found && res.is_movie && res.year == 2017 && res.length == "2h 19m",
        "resolution = {res:?}"
    );
    assert_eq!(res.buttons.len(), 2, "buttons = {:?}", res.buttons);
    assert_eq!(res.buttons[0].label, "View Movie");
    assert_eq!(res.buttons[1].label, "View Trailer");
}
