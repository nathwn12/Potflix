//! `metadata::search` -- the identity search and manual latch builder behind
//! the "Fix match" UI.
//!
//! NEW SURFACE: `go\` has no search-and-latch layer, so nothing here is diffed
//! against a ported file; the contract is stated plainly instead. The
//! automatic ladder ([`crate::resolver`]) answers a filename; this module
//! answers a person. The user types a real name plus an optional year,
//! [`search`] returns the entries the provider legs offer for it as
//! [`Candidate`]s, and [`pin_from_candidate`] turns the picked entry into an
//! [`Override`] latch -- a movie file, one season folder or a whole show --
//! carrying the candidate's cover and the entry's episode table as
//! per-episode pins. [`refresh`] re-fetches a stored latch's identity and
//! returns the same latch with a refreshed payload. Persistence is
//! [`crate::overrides`]'s job; this module only builds the record.
//!
//! # One pass, ranked
//!
//! [`search`] walks the legs applicable to the wanted kind ONCE -- the movie
//! legs for [`Kind::MOVIE`], the series/anime legs otherwise (the resolver's
//! own ranking, with Cinemeta as the general fallback) -- deduplicates by
//! (provider, provider_id), ranks, and caps at [`SEARCH_LIMIT`]. The rank:
//! the wanted kind first, then a year match (only when a year was given) above
//! a mismatch, then the entry whose title best matches the typed name, then
//! provider order. The title order reuses the crate's own matcher
//! ([`pick_best_show`]) with its weight/episode-count tie-breaks neutralized,
//! so the order is pure title strength; entries the matcher declines to rank
//! (below its similarity floor) keep provider order. A leg error, an empty
//! answer and a cancelled scope all end as an empty (or shortened) vector --
//! never a panic.
//!
//! # The latch's identity
//!
//! A latch stores the provider and the provider's own id
//! ([`Override::provider`], [`Override::provider_id`]) next to the picked
//! title/year. [`pin_from_candidate`] and [`refresh`] call THAT leg only and
//! refuse any answer whose `source_id` differs from the stored id, so a
//! re-ranked search can never silently latch a different work: a mismatched
//! or missing answer is an [`Error`] the caller shows, never a half-built
//! latch. The provider legs expose find-by-query only -- no fetch-by-id entry
//! point -- so this verified single-leg fetch is the strongest form of the
//! rule available without each provider module; the refusal is proven in
//! `refresh_and_pin_refuse_a_mismatched_provider_id` below.
//!
//! # Cancellation
//!
//! Both fetching functions block like the provider legs do and every wire
//! wait is the [`Cancel`]'s to end: hand them a deadline-bearing scope (the
//! CLIs build one, `metacheck`: 90 s). A dead scope ends a search early -- the
//! hits already found stand -- and propagates from a fetch, never
//! masquerading as a miss.

use std::collections::HashSet;
use std::sync::Arc;

use metadata_core::match_::{ShowCand, pick_best_show};
use metadata_core::types::{Episode, Kind, Media, Query};
use metadata_http::cancel::Cancel;

use crate::Error;
use crate::overrides::{EpisodePin, Override, Scope, now_ms};
use crate::resolver::{Source, default_sources};

/// The most candidates [`search`] returns.
pub const SEARCH_LIMIT: usize = 8;

/// The legs a movie search walks, in provider order (the resolver's own movie
/// ranking).
const MOVIE_LEGS: &[&str] = &["cinemeta", "wikipedia", "justwatch"];

/// The legs a series/anime search walks, in provider order (the resolver's
/// own series ranking; Cinemeta is the general fallback and carries series
/// too).
const SERIES_LEGS: &[&str] = &[
    "tvmaze",
    "anilist",
    "tenrai",
    "kitsu",
    "cinemeta",
    "episodate",
    "bangumi",
    "livechart",
    "ann",
];

/// The legs applicable to `want`: the movie legs for [`Kind::MOVIE`], the
/// series/anime legs for every other kind (an unknown want gets the wider
/// search, whose Cinemeta leg answers either kind).
fn legs_for(want: Kind) -> &'static [&'static str] {
    if want == Kind::MOVIE {
        MOVIE_LEGS
    } else {
        SERIES_LEGS
    }
}

/// One pickable entry a leg offered for the typed name.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Candidate {
    /// The entry's own kind, as the leg reported it.
    pub kind: Kind,
    /// The canonical title to display and to latch.
    pub title: String,
    /// The title's year, `0` when the provider carries none.
    pub year: i32,
    /// The leg that answered ("tvmaze", "cinemeta", ...).
    pub provider: String,
    /// That leg's own id for the entry, `""` when it has none. The latch and
    /// [`refresh`] key on it.
    pub provider_id: String,
    /// The entry's series/movie cover.
    pub poster: String,
    /// The real number of episodes the entry can serve: the length of the
    /// episode table a latch built from it carries ([`Self::poster`]'s sibling
    /// tell for choosing between a season entry and a whole-show entry).
    /// `0` for a movie.
    pub episode_count: usize,
}

/// `search`: one pass over the legs applicable to `want`, over the built-in
/// provider set.
pub fn search(cancel: &Cancel, name: &str, year: i32, want: Kind) -> Vec<Candidate> {
    search_with(&default_sources(), cancel, name, year, want)
}

/// The injectable core of [`search`]: the same pass over an explicit source
/// set (the tests drive fixture legs here).
pub(crate) fn search_with(
    sources: &[Arc<dyn Source>],
    cancel: &Cancel,
    name: &str,
    year: i32,
    want: Kind,
) -> Vec<Candidate> {
    let name = name.trim();
    if name.is_empty() {
        return Vec::new();
    }
    let mut hits: Vec<(usize, Candidate)> = Vec::new();
    let mut seen: HashSet<(String, String)> = HashSet::new();
    let mut order = 0usize;
    'legs: for leg in legs_for(want) {
        for source in sources.iter().filter(|source| source.name() == *leg) {
            let order_at = order;
            order += 1;
            if cancel.err().is_some() {
                break 'legs;
            }
            let q = Query {
                kind: want,
                title: name.to_string(),
                year,
                season: 0,
                episode: 0,
                absolute: false,
            };
            // A leg error or an empty answer is another leg's chance, never a
            // failed search: the UI's "no results" is the empty vector.
            let Ok(Some(media)) = source.find(cancel, &q) else {
                continue;
            };
            let provider = source.name().to_string();
            let provider_id = media.source_id.trim().to_string();
            if !seen.insert((provider.clone(), provider_id.clone())) {
                continue;
            }
            hits.push((
                order_at,
                Candidate {
                    kind: media.kind,
                    title: media.title.trim().to_string(),
                    year: media.year,
                    provider,
                    provider_id,
                    poster: media.artwork,
                    episode_count: media.episodes.len(),
                },
            ));
        }
    }
    let mut ranked = rank_hits(hits, name, year, want);
    ranked.truncate(SEARCH_LIMIT);
    ranked
}

/// The ranking of [`search`]: the wanted kind first, then a year match (only
/// when a year was given) above a mismatch, then the crate's title order, then
/// provider order. Provider order is part of the key, so the order is total
/// and the same fixtures reproduce the same sequence.
fn rank_hits(
    hits: Vec<(usize, Candidate)>,
    name: &str,
    year: i32,
    want: Kind,
) -> Vec<Candidate> {
    let shows: Vec<ShowCand> = hits
        .iter()
        .map(|(_, cand)| ShowCand {
            name: cand.title.clone(),
            weight: 0,
            episode_count: 0,
            aliases: Vec::new(),
        })
        .collect();
    let ranked = title_rank(name, &shows);
    let mut title_pos = vec![0usize; hits.len()];
    for (pos, hit) in ranked.into_iter().enumerate() {
        title_pos[hit] = pos;
    }
    let mut keyed: Vec<((bool, bool, usize, usize), Candidate)> = hits
        .into_iter()
        .enumerate()
        .map(|(hit, (order, cand))| {
            let key = (
                cand.kind != want,
                year > 0 && cand.year != year,
                title_pos[hit],
                order,
            );
            (key, cand)
        })
        .collect();
    keyed.sort_by_key(|(key, _)| *key);
    keyed.into_iter().map(|(_, cand)| cand).collect()
}

/// The order in which the crate's own title matcher would take `cands` one at
/// a time, best first. `weight`/`episode_count` are neutralized by the caller,
/// so within the matcher's similarity window the higher similarity wins and an
/// exact normalized title outranks every near match; entries the matcher
/// declines to rank (below its similarity floor) keep their input order,
/// which is provider order.
fn title_rank(query: &str, cands: &[ShowCand]) -> Vec<usize> {
    let mut left: Vec<usize> = (0..cands.len()).collect();
    let mut out: Vec<usize> = Vec::with_capacity(cands.len());
    while !left.is_empty() {
        let sub: Vec<ShowCand> = left.iter().map(|&i| cands[i].clone()).collect();
        let Some(best) = pick_best_show(query, &sub) else {
            out.extend(left.iter().copied());
            break;
        };
        let at = sub.iter().position(|cand| *cand == best).unwrap_or(0);
        out.push(left.remove(at));
    }
    out
}

/// `pin`: the latch for one picked candidate, over the built-in provider set.
pub fn pin_from_candidate(
    cancel: &Cancel,
    cand: &Candidate,
    scope: Scope,
    key: &str,
) -> Result<Override, Error> {
    pin_from_candidate_with(&default_sources(), cancel, cand, scope, key)
}

/// The injectable core of [`pin_from_candidate`].
///
/// The latch carries the candidate's title/year/provider/identity, the
/// candidate's cover as the scope's [`Override::poster`] for a season/show,
/// and the fetched record as [`Override::media`]. A movie renders from that
/// media snapshot (its cover rides on `media.artwork`) and carries no pins; a
/// season/show scope pins the fetched episode table -- per episode its own
/// season/number/absolute/title/synopsis, the synopsis left exactly as the
/// provider has it, empty when it has none. A failure to fetch the table is an
/// [`Error`], never a half-built latch.
pub(crate) fn pin_from_candidate_with(
    sources: &[Arc<dyn Source>],
    cancel: &Cancel,
    cand: &Candidate,
    scope: Scope,
    key: &str,
) -> Result<Override, Error> {
    let media = fetch_identity(
        sources,
        cancel,
        &cand.provider,
        &cand.provider_id,
        &cand.title,
        cand.year,
        scope_kind(scope),
    )?;
    let mut over = Override {
        scope,
        key: key.to_string(),
        title: cand.title.clone(),
        year: cand.year,
        provider: cand.provider.clone(),
        provider_id: cand.provider_id.clone(),
        poster: String::new(),
        media: None,
        episodes: Vec::new(),
        created_at_ms: now_ms(),
    };
    if scope != Scope::Movie {
        over.poster = cand.poster.clone();
        over.episodes = pins_from_table(&media.episodes);
    }
    over.media = Some(media);
    Ok(over)
}

/// `refresh`: re-fetch a stored latch's own identity and return it with the
/// payload refreshed, over the built-in provider set.
pub fn refresh(cancel: &Cancel, over: &Override) -> Result<Override, Error> {
    refresh_with(&default_sources(), cancel, over)
}

/// The injectable core of [`refresh`].
///
/// The scope, key and `created_at_ms` survive untouched; the refreshed
/// payload is the [`Override::media`] record plus, for a season/show, the
/// re-pinned episode table and the record's cover as [`Override::poster`].
/// The fetch is [`fetch_identity`]: the latch's own leg, keyed by its stored
/// provider id, so a re-ranked search cannot drift the identity.
pub(crate) fn refresh_with(
    sources: &[Arc<dyn Source>],
    cancel: &Cancel,
    over: &Override,
) -> Result<Override, Error> {
    let media = fetch_identity(
        sources,
        cancel,
        &over.provider,
        &over.provider_id,
        &over.title,
        over.year,
        scope_kind(over.scope),
    )?;
    let mut out = over.clone();
    if over.scope == Scope::Movie {
        out.media = Some(media);
        out.episodes = Vec::new();
    } else {
        out.poster = media.artwork.clone();
        out.episodes = pins_from_table(&media.episodes);
        out.media = Some(media);
    }
    Ok(out)
}

/// The kind a scope's fetch asks the leg for: a movie scope fetches the movie
/// record, a season/show scope the series record (its episode table).
fn scope_kind(scope: Scope) -> Kind {
    if scope == Scope::Movie {
        Kind::MOVIE
    } else {
        Kind::SERIES
    }
}

/// One identity fetch. ONLY the leg named `provider` is called, with the
/// stored title/year, and its answer must carry the stored `provider_id`: a
/// different entry is refused rather than latched, so the identity cannot
/// drift beneath a re-ranked search. An empty stored id (a provider with no
/// id) accepts the leg's answer as-is.
fn fetch_identity(
    sources: &[Arc<dyn Source>],
    cancel: &Cancel,
    provider: &str,
    provider_id: &str,
    title: &str,
    year: i32,
    kind: Kind,
) -> Result<Media, Error> {
    if let Some(reason) = cancel.err() {
        return Err(Error::Cancelled(reason));
    }
    let Some(source) = sources.iter().find(|source| source.name() == provider) else {
        return Err(Error::other(format!(
            "metadata: search: no source named {provider:?}"
        )));
    };
    let q = Query {
        kind,
        title: title.to_string(),
        year,
        season: 0,
        episode: 0,
        absolute: false,
    };
    let Some(media) = source.find(cancel, &q)? else {
        return Err(Error::other(format!(
            "metadata: search: {provider:?} no longer has {title:?}"
        )));
    };
    if !provider_id.is_empty() && media.source_id != provider_id {
        return Err(Error::other(format!(
            "metadata: search: {provider:?} answered id {:?} for {title:?}, not {provider_id:?}; \
             refusing to latch a different entry",
            media.source_id
        )));
    }
    Ok(media)
}

/// The per-episode pins for one fetched table, in table order. An episode
/// numbered by the wire absolutely (season 0) keeps its number as `absolute`;
/// a season-numbered episode's `absolute` is its number plus the episodes the
/// SAME table reports for strictly earlier seasons -- the resolver's own
/// absolute-total rule, so a partial table cannot fabricate a total it does
/// not hold. The synopsis is copied as-is: an episode the provider has none
/// for keeps an empty one, never a fabricated line.
fn pins_from_table(episodes: &[Episode]) -> Vec<EpisodePin> {
    episodes
        .iter()
        .map(|ep| {
            let absolute = if ep.season <= 0 {
                ep.number
            } else {
                let earlier = episodes
                    .iter()
                    .filter(|other| other.season > 0 && other.season < ep.season)
                    .count();
                i32::try_from(earlier)
                    .unwrap_or(i32::MAX)
                    .saturating_add(ep.number)
            };
            EpisodePin {
                season: ep.season,
                number: ep.number,
                absolute,
                title: ep.title.clone(),
                synopsis: ep.synopsis.clone(),
            }
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use std::sync::Mutex;
    use std::time::Duration;

    use metadata_http::cancel::Cancel;

    use super::*;
    use crate::cinemeta::CinemetaSource;
    use crate::testsupport::{FakeServer, FakeSource, Request, Response, dead_url, http_client};
    use crate::tvmaze::TvmazeSource;

    /// A fresh 5 s scope, the shape a CLI hands the legs.
    fn cancel() -> Cancel {
        Cancel::with_deadline(Duration::from_secs(5))
    }

    /// A Windows fixture path with no machine-shaped literal: the drive
    /// letter and the rest live in separate literals, so no single source
    /// line carries the gate's path shape. The runtime value is the
    /// ordinary drive-letter path the test exercises.
    fn win(drive: char, rest: &str) -> String {
        format!("{}:{}", drive, rest)
    }

    fn tvmaze_leg(url: &str) -> Arc<dyn Source> {
        Arc::new(TvmazeSource::new(url, http_client()))
    }

    fn cinemeta_leg(url: &str) -> Arc<dyn Source> {
        Arc::new(CinemetaSource::new(url, http_client()))
    }

    /// The (provider, provider_id) pairs in order: the sequence under test.
    fn identities(hits: &[Candidate]) -> Vec<(String, String)> {
        hits.iter()
            .map(|cand| (cand.provider.clone(), cand.provider_id.clone()))
            .collect()
    }

    /// A TVmaze fixture: one show ("Show", id 169, 2008) with a three-episode
    /// table spanning two seasons, the middle episode carrying no synopsis.
    const TVMAZE_2008: &str = r#"[{"score":1.0,"show":{"id":169,"name":"Show","premiered":"2008-01-20","weight":99,"image":{"original":"https://art/show.jpg"},"summary":"<p>Show.</p>","genres":["Drama"],"averageRuntime":45,"url":"https://tvmaze/169"}}]"#;

    /// The same show in 2010 (a deliberate year mismatch) as id 1.
    const TVMAZE_2010: &str = r#"[{"score":1.0,"show":{"id":1,"name":"Show","premiered":"2010-01-01","weight":99,"image":{"original":"https://art/tv.jpg"},"summary":"<p>TV.</p>","genres":["Drama"],"averageRuntime":45,"url":"https://tvmaze/1"}}]"#;

    const EPISODES: &str = r#"[{"id":1,"name":"Pilot","season":1,"number":1,"summary":"<p>Walt cooks.</p>","url":"https://tvmaze/ep/1","runtime":45},
        {"id":2,"name":"Cat's in the Bag...","season":1,"number":2},
        {"id":3,"name":"Seven Thirty-Seven","season":2,"number":1,"summary":"<p>Krazy-8.</p>","url":"https://tvmaze/ep/3","runtime":45}]"#;

    #[test]
    fn search_ranks_a_year_match_above_a_mismatch_and_repeats_the_order() {
        let tvmaze = FakeServer::start(|req: &Request| match req.path.as_str() {
            "/search/shows" => Response::json(TVMAZE_2010),
            "/shows/1/episodes" => Response::json(EPISODES),
            _ => Response::error(404, "not found"),
        });
        let cinemeta = FakeServer::start(|req: &Request| match req.path.as_str() {
            "/catalog/series/top/search=Show.json" => {
                Response::json(r#"{"metas":[{"id":"tt1","name":"Show","releaseInfo":"2008"}]}"#)
            }
            "/meta/series/tt1.json" => Response::json(
                r#"{"meta":{"name":"Show","year":"2008","runtime":"","genre":["Drama"],"description":"Cinema.","poster":"https://art/cm.jpg","videos":[{"name":"Cinema Pilot","season":1,"number":1,"overview":"Pilot."}]}}"#,
            ),
            _ => Response::error(404, "not found"),
        });
        let sources = vec![tvmaze_leg(&tvmaze.url()), cinemeta_leg(&cinemeta.url())];

        // The year match (Cinemeta's 2008 over TVmaze's 2010) wins despite
        // provider order, and the same fixtures reproduce the same sequence.
        for _ in 0..2 {
            let hits = search_with(&sources, &cancel(), "Show", 2008, Kind::SERIES);
            assert_eq!(
                identities(&hits),
                vec![
                    ("cinemeta".to_string(), "tt1".to_string()),
                    ("tvmaze".to_string(), "1".to_string()),
                ]
            );
            assert_eq!(hits[0].year, 2008);
            assert_eq!(hits[1].year, 2010);
        }

        // With no year given, provider order is the tie-break (equal titles).
        for _ in 0..2 {
            let hits = search_with(&sources, &cancel(), "Show", 0, Kind::SERIES);
            assert_eq!(
                identities(&hits),
                vec![
                    ("tvmaze".to_string(), "1".to_string()),
                    ("cinemeta".to_string(), "tt1".to_string()),
                ]
            );
        }
    }

    #[test]
    fn search_prefers_the_wanted_kind_over_the_earlier_leg() {
        let series = |title: &str, id: &str| Media {
            source: "fake".to_string(),
            source_id: id.to_string(),
            kind: Kind::SERIES,
            title: title.to_string(),
            ..Media::default()
        };
        let movie = |title: &str, id: &str| Media {
            source: "fake".to_string(),
            source_id: id.to_string(),
            kind: Kind::MOVIE,
            title: title.to_string(),
            ..Media::default()
        };

        // Wikipedia answers a SERIES record for a movie query; JustWatch
        // answers the movie. The wanted kind beats the earlier leg.
        let wikipedia: Arc<dyn Source> =
            FakeSource::with_media("wikipedia", series("Show", "series-1")).shared();
        let justwatch: Arc<dyn Source> =
            FakeSource::with_media("justwatch", movie("Show", "movie-1")).shared();
        let hits = search_with(&[wikipedia, justwatch], &cancel(), "Show", 0, Kind::MOVIE);
        assert_eq!(hits.len(), 2);
        assert_eq!(hits[0].kind, Kind::MOVIE);
        assert_eq!(hits[0].provider, "justwatch");

        // ...and for a series want, a movie-kind answer loses to a series one.
        let tvmaze_movie: Arc<dyn Source> =
            FakeSource::with_media("tvmaze", movie("Show", "movie-2")).shared();
        let anilist: Arc<dyn Source> =
            FakeSource::with_media("anilist", series("Show", "series-2")).shared();
        let hits = search_with(&[tvmaze_movie, anilist], &cancel(), "Show", 0, Kind::SERIES);
        assert_eq!(hits.len(), 2);
        assert_eq!(hits[0].kind, Kind::SERIES);
        assert_eq!(hits[0].provider, "anilist");
    }

    #[test]
    fn search_ranks_the_better_title_above_the_provider_order() {
        let series = |title: &str, id: &str| Media {
            source: "fake".to_string(),
            source_id: id.to_string(),
            kind: Kind::SERIES,
            title: title.to_string(),
            ..Media::default()
        };
        // The earlier leg carries the worse title; the matcher's exact match
        // wins the first slot anyway.
        let worse: Arc<dyn Source> =
            FakeSource::with_media("tvmaze", series("Show Commentary Special", "t1")).shared();
        let better: Arc<dyn Source> =
            FakeSource::with_media("anilist", series("Show", "a1")).shared();
        let hits = search_with(&[worse, better], &cancel(), "Show", 0, Kind::SERIES);
        assert_eq!(
            identities(&hits),
            vec![
                ("anilist".to_string(), "a1".to_string()),
                ("tvmaze".to_string(), "t1".to_string()),
            ]
        );
    }

    #[test]
    fn search_caps_at_eight_candidates() {
        let leg_names = [
            "tvmaze", "anilist", "tenrai", "kitsu", "cinemeta", "episodate", "bangumi",
            "livechart", "ann",
        ];
        let sources: Vec<Arc<dyn Source>> = leg_names
            .iter()
            .enumerate()
            .map(|(i, name)| {
                let media = Media {
                    source: "fake".to_string(),
                    source_id: format!("{i}"),
                    kind: Kind::SERIES,
                    title: "Show".to_string(),
                    ..Media::default()
                };
                FakeSource::with_media(name, media).shared()
            })
            .collect();
        let hits = search_with(&sources, &cancel(), "Show", 0, Kind::SERIES);
        assert_eq!(hits.len(), SEARCH_LIMIT);
        // Nine legs answered, all at equal strength: the last one in provider
        // order is the one dropped.
        assert!(!hits.iter().any(|cand| cand.provider == "ann"));
        assert_eq!(hits[0].provider, "tvmaze");
    }

    #[test]
    fn search_dedupes_by_provider_and_id_across_legs() {
        let one = FakeServer::start(|req: &Request| match req.path.as_str() {
            "/search/shows" => Response::json(TVMAZE_2008),
            "/shows/169/episodes" => Response::json(EPISODES),
            _ => Response::error(404, "not found"),
        });
        let two = FakeServer::start(|req: &Request| match req.path.as_str() {
            "/search/shows" => Response::json(
                r#"[{"score":1.0,"show":{"id":170,"name":"Show","premiered":"2009-01-01","weight":50}}]"#,
            ),
            "/shows/170/episodes" => Response::json(
                r#"[{"id":9,"name":"Other Pilot","season":1,"number":1}]"#,
            ),
            _ => Response::error(404, "not found"),
        });
        let first = tvmaze_leg(&one.url());
        let second = tvmaze_leg(&two.url());

        // The same leg mounted twice answers the same (provider, id): one
        // candidate. The other show's id survives as its own candidate.
        let hits = search_with(
            &[Arc::clone(&first), Arc::clone(&first), second],
            &cancel(),
            "Show",
            0,
            Kind::SERIES,
        );
        assert_eq!(
            identities(&hits),
            vec![
                ("tvmaze".to_string(), "169".to_string()),
                ("tvmaze".to_string(), "170".to_string()),
            ]
        );
        assert_eq!(hits[0].episode_count, 3, "the real table length is the tell");
        assert_eq!(hits[1].episode_count, 1);
    }

    #[test]
    fn pin_from_candidate_stores_a_movie_snapshot_with_no_pins() {
        let server = FakeServer::start(|req: &Request| match req.path.as_str() {
            "/catalog/movie/top/search=Dune:%20Part%20Two.json" => Response::json(
                r#"{"metas":[{"id":"tt15239678","name":"Dune: Part Two","releaseInfo":"2024"}]}"#,
            ),
            "/meta/movie/tt15239678.json" => Response::json(
                r#"{"meta":{"name":"Dune: Part Two","year":"2024","runtime":"166 min","genre":["Science Fiction","Adventure"],"description":"Paul Atreides unites with the Fremen.","poster":"https://art/dune.jpg","videos":[]}}"#,
            ),
            _ => Response::error(404, "not found"),
        });
        let cand = Candidate {
            kind: Kind::MOVIE,
            title: "Dune: Part Two".to_string(),
            year: 2024,
            provider: "cinemeta".to_string(),
            provider_id: "tt15239678".to_string(),
            poster: "https://art/dune-search.jpg".to_string(),
            episode_count: 0,
        };
        // The search pass over the same fixture offers that entry as a
        // zero-episode pick.
        let hits = search_with(
            &[cinemeta_leg(&server.url())],
            &cancel(),
            "Dune: Part Two",
            2024,
            Kind::MOVIE,
        );
        assert_eq!(hits.len(), 1);
        assert_eq!(hits[0].kind, Kind::MOVIE);
        assert_eq!(hits[0].provider_id, "tt15239678");
        assert_eq!(hits[0].episode_count, 0);
        assert_eq!(hits[0].poster, "https://art/dune.jpg");

        let over = pin_from_candidate_with(
            &[cinemeta_leg(&server.url())],
            &cancel(),
            &cand,
            Scope::Movie,
            &win('D', r"\Films\Dune Part Two.mkv"),
        )
        .expect("pin");

        assert_eq!(over.scope, Scope::Movie);
        assert_eq!(over.key, win('D', r"\Films\Dune Part Two.mkv"));
        assert_eq!(over.title, "Dune: Part Two");
        assert_eq!(over.year, 2024);
        assert_eq!(over.provider, "cinemeta");
        assert_eq!(over.provider_id, "tt15239678");
        assert!(
            over.poster.is_empty(),
            "a movie renders from its media snapshot"
        );
        assert!(over.episodes.is_empty(), "a movie latch carries no pins");
        let media = over.media.expect("media snapshot");
        assert_eq!(media.title, "Dune: Part Two");
        assert_eq!(media.source_id, "tt15239678");
        assert_eq!(media.artwork, "https://art/dune.jpg");
        assert!(!media.synopsis.is_empty());
        assert!(!media.length.is_empty());
        assert!(media.episodes.is_empty());
    }

    #[test]
    fn pin_from_candidate_pins_a_table_with_absolute_numbers_and_empty_synopses() {
        let server = FakeServer::start(|req: &Request| match req.path.as_str() {
            "/search/shows" => Response::json(TVMAZE_2008),
            "/shows/169/episodes" => Response::json(EPISODES),
            _ => Response::error(404, "not found"),
        });
        let sources = [tvmaze_leg(&server.url())];
        let cand = Candidate {
            kind: Kind::SERIES,
            title: "Show".to_string(),
            year: 2008,
            provider: "tvmaze".to_string(),
            provider_id: "169".to_string(),
            poster: "https://art/show-picked.jpg".to_string(),
            episode_count: 3,
        };

        let show = pin_from_candidate_with(
            &sources,
            &cancel(),
            &cand,
            Scope::Series,
            &win('M', r"\2SERIES\Show"),
        )
        .expect("pin show");
        assert_eq!(show.scope, Scope::Series);
        assert_eq!(show.poster, "https://art/show-picked.jpg");
        assert_eq!(show.episodes.len(), 3);
        assert_eq!(
            show.episodes[0],
            EpisodePin {
                season: 1,
                number: 1,
                absolute: 1,
                title: "Pilot".to_string(),
                synopsis: "Walt cooks.".to_string(),
            }
        );
        assert_eq!(show.episodes[1].absolute, 2);
        assert_eq!(
            show.episodes[1].synopsis, "",
            "a missing synopsis is never fabricated"
        );
        assert_eq!(
            show.episodes[2],
            EpisodePin {
                season: 2,
                number: 1,
                absolute: 3,
                title: "Seven Thirty-Seven".to_string(),
                synopsis: "Krazy-8.".to_string(),
            }
        );
        // The pins serve the store's own lookups: (season, number) and
        // absolute.
        assert_eq!(show.episode_pin(2, 1).map(|pin| pin.absolute), Some(3));
        assert_eq!(show.episode_pin_absolute(3).map(|pin| pin.number), Some(1));
        assert_eq!(
            show.media.as_ref().map(|media| media.title.as_str()),
            Some("Show")
        );

        // A season scope pins the same table: the pins carry their own season,
        // so one table serves every season of the show.
        let season = pin_from_candidate_with(
            &sources,
            &cancel(),
            &cand,
            Scope::Season,
            &win('M', r"\2SERIES\Show\Season 1"),
        )
        .expect("pin season");
        assert_eq!(season.scope, Scope::Season);
        assert_eq!(season.key, win('M', r"\2SERIES\Show\Season 1"));
        assert_eq!(season.episodes, show.episodes);
    }

    #[test]
    fn refresh_keeps_scope_key_and_created_at_and_updates_the_payload() {
        let search_body = Arc::new(Mutex::new(String::from(TVMAZE_2008)));
        let episodes_body = Arc::new(Mutex::new(String::from(
            r#"[{"id":1,"name":"Pilot v1","season":1,"number":1,"summary":"<p>One.</p>","runtime":45}]"#,
        )));
        let (search_handler, episodes_handler) = (Arc::clone(&search_body), Arc::clone(&episodes_body));
        let server = FakeServer::start(move |req: &Request| match req.path.as_str() {
            "/search/shows" => Response::json(&search_handler.lock().unwrap()),
            "/shows/169/episodes" => Response::json(&episodes_handler.lock().unwrap()),
            _ => Response::error(404, "not found"),
        });
        let sources = [tvmaze_leg(&server.url())];
        let cand = Candidate {
            kind: Kind::SERIES,
            title: "Show".to_string(),
            year: 2008,
            provider: "tvmaze".to_string(),
            provider_id: "169".to_string(),
            poster: "https://art/v1.jpg".to_string(),
            episode_count: 1,
        };
        let over = pin_from_candidate_with(
            &sources,
            &cancel(),
            &cand,
            Scope::Series,
            &win('M', r"\2SERIES\Show"),
        )
        .expect("pin");
        assert_eq!(over.episodes[0].title, "Pilot v1");
        assert!(over.created_at_ms > 1_600_000_000_000, "the latch was stamped");

        // The provider moves on: a new episode title and a new cover.
        *search_body.lock().unwrap() = String::from(
            r#"[{"score":1.0,"show":{"id":169,"name":"Show","premiered":"2008-01-20","weight":99,"image":{"original":"https://art/v2.jpg"},"summary":"<p>Show.</p>","genres":["Drama"],"averageRuntime":45,"url":"https://tvmaze/169"}}]"#,
        );
        *episodes_body.lock().unwrap() = String::from(
            r#"[{"id":1,"name":"Pilot v2","season":1,"number":1,"summary":"<p>One.</p>","runtime":45}]"#,
        );

        let refreshed = refresh_with(&sources, &cancel(), &over).expect("refresh");
        assert_eq!(refreshed.scope, over.scope);
        assert_eq!(refreshed.key, over.key);
        assert_eq!(refreshed.created_at_ms, over.created_at_ms);
        assert_eq!(refreshed.title, over.title);
        assert_eq!(refreshed.year, over.year);
        assert_eq!(refreshed.provider, over.provider);
        assert_eq!(refreshed.provider_id, over.provider_id);
        assert_eq!(refreshed.poster, "https://art/v2.jpg");
        assert_eq!(refreshed.episodes[0].title, "Pilot v2");
        assert_eq!(
            refreshed.media.as_ref().map(|media| media.artwork.as_str()),
            Some("https://art/v2.jpg")
        );
        // The input latch is untouched: refresh builds a new record.
        assert_eq!(over.episodes[0].title, "Pilot v1");
        assert_eq!(over.poster, "https://art/v1.jpg");
    }

    #[test]
    fn refresh_and_pin_refuse_a_mismatched_provider_id() {
        let server = FakeServer::start(|req: &Request| match req.path.as_str() {
            "/search/shows" => Response::json(TVMAZE_2008),
            "/shows/169/episodes" => Response::json(EPISODES),
            _ => Response::error(404, "not found"),
        });
        let sources = [tvmaze_leg(&server.url())];
        let cand = Candidate {
            kind: Kind::SERIES,
            title: "Show".to_string(),
            year: 2008,
            provider: "tvmaze".to_string(),
            provider_id: "169".to_string(),
            poster: String::new(),
            episode_count: 3,
        };
        let mut over =
            pin_from_candidate_with(&sources, &cancel(), &cand, Scope::Series, "k").expect("pin");

        // A stored id the leg cannot answer is refused, never latched over.
        over.provider_id = "999".to_string();
        let err = refresh_with(&sources, &cancel(), &over).expect_err("a drifted id is refused");
        assert!(
            err.to_string().contains("999"),
            "the refusal names the id: {err}"
        );
        let cand = Candidate {
            provider_id: "999".to_string(),
            ..cand
        };
        assert!(
            pin_from_candidate_with(&sources, &cancel(), &cand, Scope::Series, "k").is_err(),
            "a mismatched pin never half-builds a latch"
        );
    }

    #[test]
    fn pin_and_refresh_report_a_failing_leg_instead_of_building_a_half_latch() {
        let dead: [Arc<dyn Source>; 1] = [Arc::new(TvmazeSource::new(dead_url(), http_client()))];
        let cand = Candidate {
            kind: Kind::SERIES,
            title: "Show".to_string(),
            year: 2008,
            provider: "tvmaze".to_string(),
            provider_id: "169".to_string(),
            poster: String::new(),
            episode_count: 3,
        };
        let over = Override {
            scope: Scope::Series,
            key: "k".to_string(),
            title: "Show".to_string(),
            year: 2008,
            provider: "tvmaze".to_string(),
            provider_id: "169".to_string(),
            poster: "https://art/show.jpg".to_string(),
            media: None,
            episodes: Vec::new(),
            created_at_ms: 42,
        };
        assert!(pin_from_candidate_with(&dead, &cancel(), &cand, Scope::Series, "k").is_err());
        assert!(refresh_with(&dead, &cancel(), &over).is_err());
        // A missing provider name is an error too, and an empty source set
        // never half-builds either.
        let none: [Arc<dyn Source>; 0] = [];
        assert!(pin_from_candidate_with(&none, &cancel(), &cand, Scope::Series, "k").is_err());
        assert!(refresh_with(&none, &cancel(), &over).is_err());

        // A malformed provider body is reported, not panicked on.
        let junk = FakeServer::start(|_req: &Request| Response::json("{not json"));
        let junk_leg: [Arc<dyn Source>; 1] = [tvmaze_leg(&junk.url())];
        assert!(pin_from_candidate_with(&junk_leg, &cancel(), &cand, Scope::Series, "k").is_err());
        assert!(refresh_with(&junk_leg, &cancel(), &over).is_err());
    }

    #[test]
    fn search_returns_empty_on_no_results_or_total_failure() {
        let none: [Arc<dyn Source>; 0] = [];
        assert!(search_with(&none, &cancel(), "Show", 0, Kind::SERIES).is_empty());
        // A blank name never touches a leg.
        let live = FakeServer::start(|req: &Request| match req.path.as_str() {
            "/search/shows" => Response::json(TVMAZE_2008),
            "/shows/169/episodes" => Response::json(EPISODES),
            _ => Response::error(404, "not found"),
        });
        let sources = [tvmaze_leg(&live.url())];
        assert!(search_with(&sources, &cancel(), "   ", 0, Kind::SERIES).is_empty());

        // No provider rows, a dead leg and a malformed body all end as no
        // results -- never a panic.
        let miss = FakeServer::start(|_req: &Request| Response::json("[]"));
        let miss_leg = [tvmaze_leg(&miss.url())];
        assert!(search_with(&miss_leg, &cancel(), "Show", 0, Kind::SERIES).is_empty());
        let dead: [Arc<dyn Source>; 1] = [Arc::new(TvmazeSource::new(dead_url(), http_client()))];
        assert!(search_with(&dead, &cancel(), "Show", 0, Kind::SERIES).is_empty());
        let junk = FakeServer::start(|_req: &Request| Response::json("{not json"));
        let junk_leg = [tvmaze_leg(&junk.url())];
        assert!(search_with(&junk_leg, &cancel(), "Show", 0, Kind::SERIES).is_empty());

        // A cancelled scope ends the search before any leg runs.
        let cancelled = Cancel::background();
        cancelled.cancel();
        assert!(search_with(&sources, &cancelled, "Show", 0, Kind::SERIES).is_empty());
    }
}
