//! Ports `go\internal\metadata\jikan_test.go`.

use std::sync::Arc;

use metadata_core::types::{Episode, Kind, Query};
use metadata_http::cancel::Cancel;

use crate::jikan::JikanSource;
use crate::resolver::Source;
use crate::testsupport::{FakeServer, Request, Response, TempDir, http_client, resolver_with};

/// `TestJikan`: `/anime` then the paginated `/anime/<id>/episodes` walk.
#[test]
fn jikan_walks_the_episode_pages() {
    let server = FakeServer::start(|req: &Request| match req.path.as_str() {
        "/anime" => Response::json(
            r#"{"data":[{"mal_id":21,"title":"One Piece","title_english":"One Piece","year":1999}]}"#,
        ),
        "/anime/21/episodes" => {
            if req.query("page").as_deref() == Some("2") {
                Response::json(
                    r#"{"data":[{"episode":2,"title":"Second"}],"pagination":{"last_visible_page":2}}"#,
                )
            } else {
                Response::json(
                    r#"{"data":[{"episode":1,"title":"First"}],"pagination":{"last_visible_page":2}}"#,
                )
            }
        }
        _ => Response::error(404, "not found"),
    });

    let source = JikanSource::new(server.url(), http_client());
    let q = Query {
        kind: Kind::SERIES,
        title: "One Piece".to_string(),
        episode: 2,
        absolute: true,
        ..Query::default()
    };
    let media = source
        .find(&Cancel::background(), &q)
        .expect("Find")
        .expect("expected media, got nil");
    assert_eq!(media.title, "One Piece");
    assert_eq!(media.year, 1999);
    assert_eq!(media.source_id, "21");
    assert_eq!(
        media.episodes,
        vec![
            Episode {
                number: 1,
                title: "First".to_string(),
                ..Episode::default()
            },
            Episode {
                number: 2,
                title: "Second".to_string(),
                ..Episode::default()
            },
        ]
    );

    let dir = TempDir::new();
    let resolver = resolver_with(
        &dir.path(),
        vec![Arc::new(JikanSource::new(server.url(), http_client()))],
    );
    let res = resolver
        .lookup_now(&Cancel::background(), &q)
        .expect("LookupNow");
    assert!(res.found && res.episode == "Second", "resolution = {res:?}");
}

/// `TestJikanUsesEpisodeField`: Number comes from `data[].episode`, never the
/// slice index -- gapped, offset and unordered pages keep their own numbers.
#[test]
fn jikan_uses_the_episode_field_not_the_index() {
    let server = FakeServer::start(|req: &Request| match req.path.as_str() {
        "/anime" => Response::json(
            r#"{"data":[{"mal_id":7,"title":"Show","title_english":"Show","year":2020}]}"#,
        ),
        "/anime/7/episodes" => Response::json(
            r#"{"data":[{"episode":66,"title":"Sixty Six"},{"episode":51,"title":"Fifty One"},{"episode":52,"title":"Fifty Two"}],"pagination":{"last_visible_page":1}}"#,
        ),
        _ => Response::error(404, "not found"),
    });

    let source = JikanSource::new(server.url(), http_client());
    let media = source
        .find(
            &Cancel::background(),
            &Query {
                kind: Kind::SERIES,
                title: "Show".to_string(),
                episode: 66,
                absolute: true,
                ..Query::default()
            },
        )
        .expect("Find")
        .expect("expected media, got nil");
    assert_eq!(
        media.episodes,
        vec![
            Episode {
                number: 51,
                title: "Fifty One".to_string(),
                ..Episode::default()
            },
            Episode {
                number: 52,
                title: "Fifty Two".to_string(),
                ..Episode::default()
            },
            Episode {
                number: 66,
                title: "Sixty Six".to_string(),
                ..Episode::default()
            },
        ]
    );
}

/// `TestJikanEnrichment`: genres/duration/url populate Media plus the
/// View-Anime button path. The duration is the RAW provider string.
#[test]
fn jikan_enrichment_carries_genres_duration_and_url() {
    let server = FakeServer::start(|req: &Request| match req.path.as_str() {
        "/anime" => Response::json(
            r#"{"data":[{"mal_id":21,"title":"One Piece","title_english":"One Piece","year":1999,"duration":"24 min per ep","url":"http://myanimelist.net/anime/21/One_Piece","genres":[{"name":"Action"},{"name":"Adventure"}]}]}"#,
        ),
        "/anime/21/episodes" => Response::json(
            r#"{"data":[{"episode":1,"title":"First"}],"pagination":{"last_visible_page":1}}"#,
        ),
        _ => Response::error(404, "not found"),
    });

    let source = JikanSource::new(server.url(), http_client());
    let q = Query {
        kind: Kind::SERIES,
        title: "One Piece".to_string(),
        episode: 1,
        absolute: true,
        ..Query::default()
    };
    let media = source
        .find(&Cancel::background(), &q)
        .expect("Find")
        .expect("expected media");
    assert_eq!(media.genres, "Action \u{00b7} Adventure");
    assert_eq!(media.length, "24 min per ep");
    assert_eq!(media.show_url, "https://myanimelist.net/anime/21/One_Piece");

    let dir = TempDir::new();
    let resolver = resolver_with(
        &dir.path(),
        vec![Arc::new(JikanSource::new(server.url(), http_client()))],
    );
    let res = resolver
        .lookup_now(&Cancel::background(), &q)
        .expect("LookupNow");
    assert!(res.found, "resolution = {res:?}, want found");
    assert_eq!(
        res.buttons.last().map(|button| button.label.as_str()),
        Some("View Anime"),
        "buttons = {:?}, want trailing View Anime",
        res.buttons
    );
}
