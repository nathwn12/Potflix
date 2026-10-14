//! Cinemeta provider tests: the catalog-then-meta round trip against a fake
//! server (including the title/year validation that the iTunes leg lacked),
//! plus one live round trip against the real endpoint.

use std::sync::{Arc, Mutex};
use std::time::Duration;

use metadata_core::types::{Kind, Query};
use metadata_http::cancel::Cancel;

use crate::cinemeta::CinemetaSource;
use crate::resolver::Source;
use crate::testsupport::{FakeServer, Request, Response, TempDir, http_client, resolver_with};

const MOVIE_CATALOG: &str = r#"{"metas":[{"id":"tt1375666","name":"Inception","releaseInfo":"2010","poster":"https://images.metahub.space/poster/small/tt1375666/img"}]}"#;
const MOVIE_META: &str = r#"{"meta":{"name":"Inception","year":"2010","runtime":"148 min","genre":["Adventure","Sci-Fi","Thriller"],"description":"A skilled thief who steals secrets from deep within the subconscious is offered one last job.","poster":"https://images.metahub.space/poster/small/tt1375666/img"}}"#;
const SERIES_CATALOG: &str = r#"{"metas":[{"id":"tt0903747","name":"Breaking Bad","releaseInfo":"2008-2013","poster":"https://images.metahub.space/poster/small/tt0903747/img"}]}"#;
const SERIES_META: &str = r#"{"meta":{"name":"Breaking Bad","year":"2008","genre":["Drama","Crime"],"description":"A chemistry teacher turns to cooking crystal meth.","poster":"https://images.metahub.space/poster/small/tt0903747/img","videos":[{"name":"Pilot","season":1,"number":1,"overview":"Walter White learns he has cancer.","thumbnail":"https://episodes.metahub.space/tt0903747/s1e1/img"}]}}"#;

/// A fake Cinemeta addon serving the canned catalog/meta pairs, recording
/// every request path so a test can prove the meta leg was never touched.
fn fake_addon(paths: Arc<Mutex<Vec<String>>>) -> FakeServer {
    FakeServer::start(move |req: &Request| {
        paths.lock().unwrap().push(req.path.clone());
        if req.path.starts_with("/catalog/movie/") {
            return Response::json(MOVIE_CATALOG);
        }
        if req.path.starts_with("/catalog/series/") {
            return Response::json(SERIES_CATALOG);
        }
        match req.path.as_str() {
            "/meta/movie/tt1375666.json" => Response::json(MOVIE_META),
            "/meta/series/tt0903747.json" => Response::json(SERIES_META),
            _ => Response::error(404, "not found"),
        }
    })
}

fn movie_query(title: &str, year: i32) -> Query {
    Query {
        kind: Kind::MOVIE,
        title: title.to_string(),
        year,
        ..Query::default()
    }
}

/// The catalog-then-meta round trip maps name/year/runtime/genres/synopsis/
/// poster, and the resolver carries the movie answer.
#[test]
fn cinemeta_finds_movie_by_validated_title() {
    let paths: Arc<Mutex<Vec<String>>> = Arc::new(Mutex::new(Vec::new()));
    let server = fake_addon(Arc::clone(&paths));
    let source = CinemetaSource::new(server.url(), http_client());
    let q = movie_query("Inception", 2010);
    let media = source
        .find(&Cancel::background(), &q)
        .expect("Find")
        .expect("expected media, got nil");
    assert_eq!(media.title, "Inception");
    assert_eq!(media.year, 2010);
    assert_eq!(media.kind, Kind::MOVIE);
    assert_eq!(media.source, "cinemeta");
    assert_eq!(media.source_id, "tt1375666");
    assert_eq!(media.length, "148 min");
    assert_eq!(media.genres, "Adventure \u{00b7} Sci-Fi \u{00b7} Thriller");
    assert!(
        !media.synopsis.is_empty(),
        "synopsis must survive the cap/strip helpers"
    );
    assert!(
        media.artwork.starts_with("https://"),
        "artwork = {:?}",
        media.artwork
    );
    assert_eq!(
        media.movie_url, "https://www.imdb.com/title/tt1375666/",
        "movie_url = {:?}",
        media.movie_url
    );
    assert!(
        media.episodes.is_empty(),
        "movie must have no episodes: {:?}",
        media.episodes
    );

    let dir = TempDir::new();
    let resolver = resolver_with(
        &dir.path(),
        vec![Arc::new(CinemetaSource::new(server.url(), http_client()))],
    );
    let res = resolver
        .lookup_now(&Cancel::background(), &q)
        .expect("LookupNow");
    assert!(
        res.found && res.is_movie && res.series == "Inception" && res.year == 2010,
        "resolution = {res:?}"
    );
    assert_eq!(res.length, "148 min");
    assert!(
        paths
            .lock()
            .unwrap()
            .iter()
            .any(|p| p.starts_with("/meta/movie/")),
        "the meta leg must have been fetched: {paths:?}"
    );
}

/// The iTunes failure, inverted: a catalog whose best title does not match
/// the query is a definitive miss, and the meta leg is never fetched.
#[test]
fn cinemeta_rejects_wrong_title_without_meta_fetch() {
    let paths: Arc<Mutex<Vec<String>>> = Arc::new(Mutex::new(Vec::new()));
    let recorded = Arc::clone(&paths);
    let server = FakeServer::start(move |req: &Request| {
        recorded.lock().unwrap().push(req.path.clone());
        if req.path.starts_with("/catalog/") {
            return Response::json(
                r#"{"metas":[{"id":"tt9999999","name":"Ring of Fire","releaseInfo":"2012"}]}"#,
            );
        }
        Response::error(404, "not found")
    });
    let source = CinemetaSource::new(server.url(), http_client());
    let found = source
        .find(&Cancel::background(), &movie_query("Inception", 0))
        .expect("Find");
    assert!(found.is_none(), "a wrong title must miss, got {found:?}");
    assert!(
        !paths.lock().unwrap().iter().any(|p| p.starts_with("/meta/")),
        "no meta fetch for a rejected title: {paths:?}"
    );
}

/// A known query year that disagrees with the pick's release year is a
/// definitive miss with no meta fetch.
#[test]
fn cinemeta_rejects_year_mismatch_without_meta_fetch() {
    let paths: Arc<Mutex<Vec<String>>> = Arc::new(Mutex::new(Vec::new()));
    let server = fake_addon(Arc::clone(&paths));
    let source = CinemetaSource::new(server.url(), http_client());
    let found = source
        .find(&Cancel::background(), &movie_query("Inception", 2000))
        .expect("Find");
    assert!(
        found.is_none(),
        "a year mismatch must miss, got {found:?}"
    );
    assert!(
        !paths.lock().unwrap().iter().any(|p| p.starts_with("/meta/")),
        "no meta fetch for a year mismatch: {paths:?}"
    );
}

/// Series round trip: the episode table arrives in the meta call and the
/// resolver carries the number-exact episode title.
#[test]
fn cinemeta_resolves_series_episode() {
    let paths: Arc<Mutex<Vec<String>>> = Arc::new(Mutex::new(Vec::new()));
    let server = fake_addon(Arc::clone(&paths));
    let source = CinemetaSource::new(server.url(), http_client());
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
    assert_eq!(media.genres, "Drama \u{00b7} Crime");
    assert_eq!(media.episodes.len(), 1);
    assert_eq!(media.episodes[0].title, "Pilot");

    // A movie query is served by the movie leg, never the series table.
    let movie = source
        .find(&Cancel::background(), &movie_query("Inception", 2010))
        .expect("movie Find")
        .expect("expected movie media");
    assert_eq!(movie.kind, Kind::MOVIE);

    let dir = TempDir::new();
    let resolver = resolver_with(
        &dir.path(),
        vec![Arc::new(CinemetaSource::new(server.url(), http_client()))],
    );
    let res = resolver
        .lookup_now(&Cancel::background(), &q)
        .expect("LookupNow");
    assert!(
        res.found && res.series == "Breaking Bad" && res.episode == "Pilot",
        "resolution = {res:?}"
    );
}

/// Duplicate exact titles: the live catalog carries two string-exact
/// "Inception" rows, and the year-less one sorts first. A known query year
/// must still land on the right id.
#[test]
fn cinemeta_year_disambiguates_duplicate_exact_titles() {
    let server = FakeServer::start(|req: &Request| {
        if req.path.starts_with("/catalog/") {
            return Response::json(
                r#"{"metas":[{"id":"tt5581256","name":"Inception"},{"id":"tt1375666","name":"Inception","releaseInfo":"2010"}]}"#,
            );
        }
        if req.path.as_str() == "/meta/movie/tt1375666.json" {
            return Response::json(MOVIE_META);
        }
        Response::error(404, "not found")
    });
    let source = CinemetaSource::new(server.url(), http_client());
    let media = source
        .find(&Cancel::background(), &movie_query("Inception", 2010))
        .expect("Find")
        .expect("the year-matching exact must answer");
    assert_eq!(media.source_id, "tt1375666");
    assert_eq!(media.year, 2010);
}

/// The meta year veto: a catalog pick whose details disagree with a known
/// query year is a miss, never a card.
#[test]
fn cinemeta_meta_year_veto_is_a_miss() {
    let server = FakeServer::start(|req: &Request| {
        if req.path.starts_with("/catalog/") {
            return Response::json(
                r#"{"metas":[{"id":"tt1375666","name":"Inception","releaseInfo":"2010"}]}"#,
            );
        }
        Response::json(
            r#"{"meta":{"name":"Inception","year":"2000","runtime":"148 min","genre":["Sci-Fi"],"description":"Wrong decade.","poster":"https://example.com/p.jpg"}}"#,
        )
    });
    let source = CinemetaSource::new(server.url(), http_client());
    let found = source
        .find(&Cancel::background(), &movie_query("Inception", 2010))
        .expect("Find");
    assert!(found.is_none(), "a meta year veto must miss, got {found:?}");
}

/// LIVE: the real Cinemeta endpoint answers `Inception` -- the title the old
/// iTunes leg mismatched -- with year, runtime, genres, synopsis and poster.
/// Real network, real input; a mock proves nothing here.
#[test]
fn cinemeta_live_inception_round_trip() {
    let source = CinemetaSource::new("", http_client());
    let cancel = Cancel::with_deadline(Duration::from_secs(25));
    let media = source
        .find(&cancel, &movie_query("Inception", 2010))
        .expect("live Find")
        .expect("the live catalog must answer Inception");
    assert_eq!(media.title, "Inception", "media = {media:?}");
    assert_eq!(media.year, 2010, "media = {media:?}");
    assert_eq!(media.length, "148 min", "media = {media:?}");
    assert!(
        media.genres.contains("Sci-Fi"),
        "genres = {:?}",
        media.genres
    );
    assert!(
        !media.synopsis.is_empty(),
        "the live synopsis must be non-empty"
    );
    assert!(
        media.artwork.starts_with("https://"),
        "artwork = {:?}",
        media.artwork
    );
}
