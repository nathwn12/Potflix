//! Ports `go\internal\metadata\anilist_test.go`.

use std::sync::{Arc, Mutex};

use metadata_core::types::{Episode, Kind, Media, Query};
use metadata_http::cancel::Cancel;

use crate::anilist::AnilistSource;
use crate::resolver::Source;
use crate::testsupport::{FakeServer, Request, Response, TempDir, http_client, resolver_with};

/// `TestAniList`: one GraphQL POST, the search variable echoed, and the
/// episode number/title/URL mapping.
#[test]
fn anilist_posts_the_query_and_maps_streaming_episodes() {
    let seen_search: Arc<Mutex<String>> = Arc::new(Mutex::new(String::new()));
    let violations: Arc<Mutex<Vec<String>>> = Arc::new(Mutex::new(Vec::new()));
    let recorded = Arc::clone(&seen_search);
    let bad = Arc::clone(&violations);
    let server = FakeServer::start(move |req: &Request| {
        if req.method != "POST" {
            bad.lock()
                .unwrap()
                .push(format!("method = {}, want POST", req.method));
        }
        let payload: serde_json::Value = serde_json::from_slice(&req.body).expect("decode payload");
        *recorded.lock().unwrap() = payload["variables"]["search"]
            .as_str()
            .unwrap_or_default()
            .to_string();
        Response::json(
            r#"{"data":{"Page":{"media":[{"id":21,"title":{"romaji":"One Piece","english":""},"season":"SPRING","seasonYear":1999,"episodes":2,"streamingEpisodes":[{"title":"Episode 1 - Enter: Naruto Uzumaki!","url":"http://x","site":"Crunchyroll"},{"title":"Episode 2 - Second","url":"http://y","site":"Crunchyroll"}]}]}}}"#,
        )
    });

    let source = AnilistSource::new(server.url(), http_client());
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
        .expect("expected media, got nil");
    assert_eq!(*seen_search.lock().unwrap(), "One Piece");
    // english is empty so the canonical title falls back to romaji.
    assert_eq!(media.title, "One Piece");
    assert_eq!(media.year, 1999);
    assert_eq!(media.source_id, "21");
    assert_eq!(
        media.episodes,
        vec![
            Episode {
                number: 1,
                title: "Enter: Naruto Uzumaki!".to_string(),
                url: "https://x".to_string(),
                ..Episode::default()
            },
            Episode {
                number: 2,
                title: "Second".to_string(),
                url: "https://y".to_string(),
                ..Episode::default()
            },
        ]
    );

    let dir = TempDir::new();
    let resolver = resolver_with(
        &dir.path(),
        vec![Arc::new(AnilistSource::new(server.url(), http_client()))],
    );
    let res = resolver
        .lookup_now(&Cancel::background(), &q)
        .expect("LookupNow");
    assert!(
        res.found && res.episode == "Enter: Naruto Uzumaki!",
        "resolution = {res:?}"
    );
    assert_eq!(violations.lock().unwrap().len(), 0, "{violations:?}");
}

/// `anilistStub`: a source serving one canned `streamingEpisodes` list.
fn anilist_stub(streaming: &str) -> (FakeServer, AnilistSource) {
    let body = format!(
        "{{\"data\":{{\"Page\":{{\"media\":[{{\"id\":1,\"title\":{{\"romaji\":\"Show\",\"english\":\"Show\"}},\"seasonYear\":2020,\"streamingEpisodes\":[{streaming}]}}]}}}}}}"
    );
    let server = FakeServer::start(move |_req: &Request| Response::json(&body));
    let source = AnilistSource::new(server.url(), http_client());
    (server, source)
}

fn anilist_numbers(media: &Media) -> Vec<i32> {
    media
        .episodes
        .iter()
        .map(|episode| episode.number)
        .collect()
}

/// `TestAniListNumberParsing`: Numbers come from the embedded "Episode N" title,
/// not the slice index.
#[test]
fn anilist_numbers_come_from_the_title_regex() {
    struct Case {
        name: &'static str,
        streaming: &'static str,
        want_numbers: Vec<i32>,
        want_titles: Vec<(i32, &'static str)>,
    }
    let cases = vec![
        Case {
            name: "episode prefix at index zero",
            streaming: r#"{"title":"Episode 1 - Pilot","url":"http://x","site":"C"}"#,
            want_numbers: vec![1],
            want_titles: vec![(1, "Pilot")],
        },
        Case {
            name: "offset start maps by parsed number",
            streaming: r#"{"title":"Episode 51 - Theatrical Malice","url":"http://x","site":"C"},{"title":"Episode 52 - A Showdown of Fire and Ice","url":"http://x","site":"C"}"#,
            want_numbers: vec![51, 52],
            want_titles: vec![
                (51, "Theatrical Malice"),
                (52, "A Showdown of Fire and Ice"),
            ],
        },
        Case {
            name: "gap is preserved not closed",
            streaming: r#"{"title":"Episode 1 - A","url":"http://x","site":"C"},{"title":"Episode 3 - C","url":"http://x","site":"C"},{"title":"Episode 4 - D","url":"http://x","site":"C"}"#,
            want_numbers: vec![1, 3, 4],
            want_titles: vec![(1, "A"), (3, "C"), (4, "D")],
        },
        Case {
            name: "descending live shape sorts ascending",
            streaming: r#"{"title":"Episode 66 - The Result of the Battle for Priestella","url":"http://x","site":"C"},{"title":"Episode 65 - A Hideous Dinner Party","url":"http://x","site":"C"},{"title":"Episode 51 - Theatrical Malice","url":"http://x","site":"C"}"#,
            want_numbers: vec![51, 65, 66],
            want_titles: vec![
                (51, "Theatrical Malice"),
                (65, "A Hideous Dinner Party"),
                (66, "The Result of the Battle for Priestella"),
            ],
        },
        Case {
            name: "leading number dash prefix",
            streaming: r#"{"title":"01 - Pilot","url":"http://x","site":"C"},{"title":"02 - Second","url":"http://x","site":"C"}"#,
            want_numbers: vec![1, 2],
            want_titles: vec![(1, "Pilot"), (2, "Second")],
        },
        Case {
            name: "recap dot-five keeps base number",
            streaming: r#"{"title":"Episode 2.5 - Recap","url":"http://x","site":"C"}"#,
            want_numbers: vec![2],
            want_titles: vec![(2, "Recap")],
        },
        Case {
            name: "fallback keeps running index",
            streaming: r#"{"title":"Episode 10 - Tenth","url":"http://x","site":"C"},{"title":"Behind the Scenes","url":"http://x","site":"C"}"#,
            want_numbers: vec![2, 10],
            want_titles: vec![(10, "Tenth"), (2, "Behind the Scenes")],
        },
    ];
    for case in cases {
        let (_server, source) = anilist_stub(case.streaming);
        let media = source
            .find(
                &Cancel::background(),
                &Query {
                    kind: Kind::SERIES,
                    title: "Show".to_string(),
                    absolute: true,
                    ..Query::default()
                },
            )
            .unwrap_or_else(|err| panic!("{}: Find: {err}", case.name))
            .expect("expected media, got nil");
        assert_eq!(
            anilist_numbers(&media),
            case.want_numbers,
            "{}: numbers",
            case.name
        );
        for (number, title) in case.want_titles {
            assert!(
                media
                    .episodes
                    .iter()
                    .any(|episode| episode.number == number && episode.title == title),
                "{}: episodes = {:?}, want number {number} title {title:?}",
                case.name,
                media.episodes
            );
        }
    }
}

/// `TestAniListShowURL`: `idMal` present -> myanimelist.net ShowURL; absent ->
/// the AniList `siteUrl` fallback, https-forced.
#[test]
fn anilist_show_url_prefers_mal() {
    let with_mal = r#"{"data":{"Page":{"media":[{"id":21355,"idMal":12189,"title":{"romaji":"Re Zero","english":"Re:ZERO"},"seasonYear":2016,"genres":["Action","Adventure"],"duration":24,"siteUrl":"https://anilist.co/anime/21355","streamingEpisodes":[{"title":"Episode 1 - Pilot","url":"http://example.com/ep1","site":"Crunchyroll","thumbnail":""}]}]}}}"#;
    let server = FakeServer::start(move |_req: &Request| Response::json(with_mal));
    let source = AnilistSource::new(server.url(), http_client());
    let media = source
        .find(
            &Cancel::background(),
            &Query {
                kind: Kind::SERIES,
                title: "Re Zero".to_string(),
                episode: 1,
                absolute: true,
                ..Query::default()
            },
        )
        .expect("Find")
        .expect("expected media");
    assert_eq!(media.show_url, "https://myanimelist.net/anime/12189");
    assert_eq!(media.genres, "Action \u{00b7} Adventure");
    assert_eq!(media.length, "24 min");
    assert_eq!(
        media.episodes.first().map(|ep| ep.url.as_str()),
        Some("https://example.com/ep1")
    );

    let without_mal = r#"{"data":{"Page":{"media":[{"id":1,"title":{"romaji":"Show","english":"Show"},"seasonYear":2020,"genres":["Drama"],"siteUrl":"http://anilist.co/anime/1","streamingEpisodes":[]}]}}}"#;
    let server2 = FakeServer::start(move |_req: &Request| Response::json(without_mal));
    let source2 = AnilistSource::new(server2.url(), http_client());
    let media2 = source2
        .find(
            &Cancel::background(),
            &Query {
                kind: Kind::SERIES,
                title: "Show".to_string(),
                episode: 1,
                absolute: true,
                ..Query::default()
            },
        )
        .expect("Find")
        .expect("expected media");
    assert_eq!(media2.show_url, "https://anilist.co/anime/1");
}

/// The DBS regression: a `Page` carries the 2015 series (`id21175`) first and
/// the 2026 `Dragon Ball Super: Beerus` release (`id206814`) second. A query
/// with year 2026 must select `id206814`, never the server's top pick.
#[test]
fn anilist_year_2026_selects_beerus_over_the_2015_series() {
    let body = r#"{"data":{"Page":{"media":[
        {"id":21175,"title":{"romaji":"Dragon Ball Super","english":"Dragon Ball Super"},"seasonYear":2015,"streamingEpisodes":[]},
        {"id":206814,"title":{"romaji":"Dragon Ball Super: Beerus","english":"Dragon Ball Super: Beerus"},"seasonYear":2026,"streamingEpisodes":[]}
    ]}}}"#;
    let server = FakeServer::start(move |_req: &Request| Response::json(body));
    let source = AnilistSource::new(server.url(), http_client());
    let media = source
        .find(
            &Cancel::background(),
            &Query {
                kind: Kind::SERIES,
                title: "dragon ball super".to_string(),
                year: 2026,
                absolute: true,
                ..Query::default()
            },
        )
        .expect("Find")
        .expect("expected media");
    assert_eq!(media.source_id, "206814", "title = {:?}", media.title);
}

/// Year is a nice-to-have, never a veto: a year matching no candidate keeps
/// the best title match instead of returning a miss.
#[test]
fn anilist_unmatched_year_keeps_the_best_title_match() {
    let body = r#"{"data":{"Page":{"media":[
        {"id":21175,"title":{"romaji":"Dragon Ball Super","english":"Dragon Ball Super"},"seasonYear":2015,"streamingEpisodes":[]},
        {"id":206814,"title":{"romaji":"Dragon Ball Super: Beerus","english":"Dragon Ball Super: Beerus"},"seasonYear":2026,"streamingEpisodes":[]}
    ]}}}"#;
    let server = FakeServer::start(move |_req: &Request| Response::json(body));
    let source = AnilistSource::new(server.url(), http_client());
    let media = source
        .find(
            &Cancel::background(),
            &Query {
                kind: Kind::SERIES,
                title: "dragon ball super".to_string(),
                year: 1999,
                absolute: true,
                ..Query::default()
            },
        )
        .expect("Find")
        .expect("a year matching nothing must not veto the title winner");
    assert_eq!(media.source_id, "21175", "title = {:?}", media.title);
}

/// A film query surfaces no anime candidates: `inception` returns `Ok(None)`,
/// not a wrong anime.
#[test]
fn anilist_empty_page_is_a_miss_not_a_wrong_anime() {
    let body = r#"{"data":{"Page":{"media":[]}}}"#;
    let server = FakeServer::start(move |_req: &Request| Response::json(body));
    let source = AnilistSource::new(server.url(), http_client());
    let found = source
        .find(
            &Cancel::background(),
            &Query {
                kind: Kind::SERIES,
                title: "inception".to_string(),
                absolute: true,
                ..Query::default()
            },
        )
        .expect("Find");
    assert!(found.is_none(), "film query must miss, got {found:?}");
}

/// AoT regression (measured 2026-10-15): ranking by the english-then-romaji
/// display name alone scores the main series ("Attack on Titan") 0 against a
/// romaji query, so the romaji-named special wins. The romaji alias must
/// count, and the display title stays English.
#[test]
fn anilist_romaji_query_selects_main_series_with_english_title() {
    let body = r#"{"data":{"Page":{"media":[
        {"id":16498,"title":{"romaji":"Shingeki no Kyojin","english":"Attack on Titan"},"seasonYear":2013,"streamingEpisodes":[{"title":"Episode 3 - To You, in 2000 Years","url":"http://x","site":"C"}]},
        {"id":99999,"title":{"romaji":"Shingeki no Kyojin: Picture Drama","english":""},"synonyms":["Attack on Titan Picture Drama"],"seasonYear":2013,"streamingEpisodes":[{"title":"Episode 3 - Day 5/Day 6/Day 7","url":"http://y","site":"C"}]}
    ]}}}"#;
    let server = FakeServer::start(move |_req: &Request| Response::json(body));
    let source = AnilistSource::new(server.url(), http_client());
    let media = source
        .find(
            &Cancel::background(),
            &Query {
                kind: Kind::SERIES,
                title: "Shingeki no Kyojin".to_string(),
                episode: 3,
                absolute: true,
                ..Query::default()
            },
        )
        .expect("Find")
        .expect("the romaji query must answer, got nil");
    assert_eq!(media.source_id, "16498", "a special must not win: {media:?}");
    assert_eq!(media.title, "Attack on Titan", "media = {media:?}");
}

/// An entry with an empty `english` must surface its other titles (here the
/// first synonym, measured live English) rather than the romaji string.
#[test]
fn anilist_empty_english_falls_back_to_synonym_not_romaji() {
    let body = r#"{"data":{"Page":{"media":[
        {"id":108942,"title":{"romaji":"Shingeki no Kyojin: Chimi Kyara Gekijou - Rivai-han","english":""},"synonyms":["Attack on Titan Season 3 Picture Drama","Shingeki no Kyojin Season 3 Specials"],"seasonYear":2018,"streamingEpisodes":[]}
    ]}}}"#;
    let server = FakeServer::start(move |_req: &Request| Response::json(body));
    let source = AnilistSource::new(server.url(), http_client());
    let media = source
        .find(
            &Cancel::background(),
            &Query {
                kind: Kind::SERIES,
                title: "Shingeki no Kyojin: Chimi Kyara Gekijou - Rivai-han".to_string(),
                absolute: true,
                ..Query::default()
            },
        )
        .expect("Find")
        .expect("expected media, got nil");
    assert_eq!(
        media.title, "Attack on Titan Season 3 Picture Drama",
        "media = {media:?}"
    );
}
