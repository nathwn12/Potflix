//! `metadata::tvmaze` -- mirrors `go\internal\metadata\tvmaze.go`.
//!
//! TVmaze provider (the TV branch of the resolver chain, and the FIRST source
//! for every series query including absolute anime numbering).
//!
//! Wire shape: `/search/shows?q=<lowercased title>`, then either
//! `/shows/<id>/seasons` + `/seasons/<seasonId>/episodes` for a
//! `Season>0 && !Absolute` query, or `/shows/<id>/episodes` for the whole show
//! in air order (the `Season==0` and `Absolute` branch).
//!
//! Two behaviours that are contract, not detail:
//!
//! * the search query is LOWERCASED before escaping, and the score order
//!   `/search/shows` returns is re-ranked by [`pick_best_show`] -- the live
//!   ranking puts the "Re:Zero kara Hajimeru Break Time" spinoff above the main
//!   show, and a low-confidence best returns a definitive MISS rather than a
//!   wrong title;
//! * an `Absolute` query RENUMBERS the episodes positionally (`i + 1` in
//!   `(season, number)` air order) so number-exact matching holds; without it an
//!   absolute anime query never matches. `season` is kept for display.

use std::time::Duration;

use metadata_core::match_::{ShowCand, pick_best_show};
use metadata_core::parse::date_year;
use metadata_core::types::{
    Episode, Kind, Media, Query, join_genres, normalize_url, runtime_length,
};
use metadata_http::cancel::Cancel;
use metadata_http::http::{HttpClient, decode_capped, drain_and_close};
use ureq::http::StatusCode;

use crate::Error;
use crate::artwork::{cap_synopsis, first_non_empty, strip_html};
use crate::serdeutil::null_default;
use crate::urlutil::query_escape;

/// The provider's default host.
pub const TVMAZE_DEFAULT_BASE: &str = "https://api.tvmaze.com";

/// `tvmazeSource`.
pub struct TvmazeSource {
    base: String,
    client: HttpClient,
}

impl TvmazeSource {
    /// A source pointed at `base` (`""` means [`TVMAZE_DEFAULT_BASE`]) fetching
    /// through `client`; cloning the client shares its connection pool.
    pub fn new(base: impl Into<String>, client: HttpClient) -> TvmazeSource {
        TvmazeSource {
            base: base.into(),
            client,
        }
    }

    fn base_url(&self) -> &str {
        if self.base.is_empty() {
            TVMAZE_DEFAULT_BASE
        } else {
            &self.base
        }
    }

    fn http_client(&self) -> &HttpClient {
        &self.client
    }
}

impl Default for TvmazeSource {
    fn default() -> TvmazeSource {
        TvmazeSource::new("", HttpClient::new(Duration::ZERO))
    }
}

#[derive(Debug, Clone, Default, serde::Deserialize)]
struct TvmazeImage {
    #[serde(default, deserialize_with = "null_default")]
    medium: String,
    #[serde(default, deserialize_with = "null_default")]
    original: String,
}

/// `tvmazeArt` prefers the full-size original, falling back to medium.
fn tvmaze_art(image: Option<&TvmazeImage>) -> String {
    let Some(image) = image else {
        return String::new();
    };
    first_non_empty(&[&image.original, &image.medium])
}

#[derive(Debug, Clone, Default, serde::Deserialize)]
struct TvmazeShow {
    #[serde(default, deserialize_with = "null_default")]
    id: i32,
    #[serde(default, deserialize_with = "null_default")]
    name: String,
    #[serde(default, deserialize_with = "null_default")]
    premiered: String,
    #[serde(default, deserialize_with = "null_default")]
    weight: i32,
    #[serde(default)]
    image: Option<TvmazeImage>,
    #[serde(default, deserialize_with = "null_default")]
    summary: String,
    #[serde(default, deserialize_with = "null_default")]
    genres: Vec<String>,
    #[serde(default)]
    runtime: Option<i32>,
    #[serde(rename = "averageRuntime", default)]
    average_runtime: Option<i32>,
    #[serde(default, deserialize_with = "null_default")]
    url: String,
}

/// One entry of `GET /search/shows?q=...`. Go's struct also carries `score`,
/// which nothing reads; the field is omitted here (serde ignores it either way).
#[derive(Debug, Clone, Default, serde::Deserialize)]
struct TvmazeSearchResult {
    #[serde(default, deserialize_with = "null_default")]
    show: TvmazeShow,
}

#[derive(Debug, Clone, Default, serde::Deserialize)]
struct TvmazeSeason {
    #[serde(default, deserialize_with = "null_default")]
    id: i32,
    #[serde(default, deserialize_with = "null_default")]
    number: i32,
}

#[derive(Debug, Clone, Default, serde::Deserialize)]
struct TvmazeEpisode {
    #[serde(default, deserialize_with = "null_default")]
    name: String,
    #[serde(default, deserialize_with = "null_default")]
    season: i32,
    #[serde(default)]
    number: Option<i32>,
    #[serde(default)]
    image: Option<TvmazeImage>,
    #[serde(default, deserialize_with = "null_default")]
    summary: String,
    #[serde(default, deserialize_with = "null_default")]
    url: String,
    #[serde(default)]
    runtime: Option<i32>,
}

/// `tvmazeShowLength` prefers `averageRuntime`, else `runtime`.
fn tvmaze_show_length(show: &TvmazeShow) -> String {
    if let Some(average) = show.average_runtime
        && average > 0
    {
        return runtime_length(average);
    }
    if let Some(runtime) = show.runtime
        && runtime > 0
    {
        return runtime_length(runtime);
    }
    String::new()
}

fn tvmaze_ep_length(runtime: Option<i32>) -> String {
    runtime.map(runtime_length).unwrap_or_default()
}

/// `tvmazeMatchedURL`: the matched episode's page URL for `q`, `""` when the
/// table lacks it. Stored on `Media.episode_url` so the resolver can build the
/// View-Episode button even from cache.
fn tvmaze_matched_url(out: &[Episode], q: &Query) -> String {
    crate::resolver::match_episode(out, q)
        .map(|ep| ep.url)
        .unwrap_or_default()
}

fn build_episode(e: &TvmazeEpisode, number: i32) -> Episode {
    Episode {
        season: e.season,
        number,
        title: e.name.clone(),
        artwork: tvmaze_art(e.image.as_ref()),
        synopsis: cap_synopsis(&strip_html(&e.summary)),
        url: normalize_url(&e.url),
        length: tvmaze_ep_length(e.runtime),
    }
}

impl crate::resolver::Source for TvmazeSource {
    fn name(&self) -> &str {
        "tvmaze"
    }

    /// `Find` resolves one season (`Season>0`, not absolute) via
    /// `/shows/<id>/seasons` + `/seasons/<seasonId>/episodes`, or the whole show
    /// in air order for absolute/`Season==0` queries via `/shows/<id>/episodes`.
    fn find(&self, cancel: &Cancel, q: &Query) -> Result<Option<Media>, Error> {
        if q.kind == Kind::MOVIE || q.title.is_empty() {
            return Ok(None);
        }
        let base = self.base_url();
        let client = self.http_client();

        // Ranked search: `/search/shows` returns score order, which misranks
        // (singlesearch picked the "Re:Zero ... Break Time" spinoff over the
        // main show), so pick_best_show re-ranks by title similarity with
        // TVmaze-weight tie-breaks. Two guards keep a fuzzy score from ever
        // becoming a card: only candidates that clear the matcher's own floor
        // on their own rank at all (a shows-only search returns ten fuzzy
        // rows and zero exact matches for films -- `inception` must miss, not
        // become `Deception`), and a low-confidence best returns not-found so
        // the resolver falls through to AniList/Jikan.
        let search_url = format!(
            "{base}/search/shows?q={}",
            query_escape(&q.title.to_lowercase())
        );
        let response = client.get(cancel, &search_url)?;
        if response.status() == StatusCode::NOT_FOUND {
            drain_and_close(response);
            return Ok(None);
        }
        let results: Vec<TvmazeSearchResult> = decode_capped(response)?;
        if results.is_empty() {
            return Ok(None);
        }
        let mut cands: Vec<ShowCand> = Vec::with_capacity(results.len());
        let mut shows: Vec<TvmazeShow> = Vec::with_capacity(results.len());
        for result in results {
            if result.show.id == 0 || result.show.name.is_empty() {
                continue;
            }
            cands.push(ShowCand {
                name: result.show.name.clone(),
                weight: result.show.weight,
                episode_count: 0,
                aliases: Vec::new(),
            });
            shows.push(result.show);
        }
        if cands.is_empty() {
            return Ok(None);
        }
        // Title identity: rank only candidates that match the query on their
        // own. No year can rescue a title miss, so this runs before any year
        // preference; an empty confident set is a definitive miss.
        let mut conf_cands: Vec<ShowCand> = Vec::with_capacity(cands.len());
        let mut conf_idx: Vec<usize> = Vec::with_capacity(cands.len());
        for (i, cand) in cands.iter().enumerate() {
            if pick_best_show(&q.title, std::slice::from_ref(cand)).is_some() {
                conf_cands.push(cand.clone());
                conf_idx.push(i);
            }
        }
        if conf_cands.is_empty() {
            return Ok(None);
        }
        let Some(winner) = pick_best_show(&q.title, &conf_cands) else {
            return Ok(None);
        };
        let mut winner_at = None;
        for (at, cand) in conf_cands.iter().enumerate() {
            if *cand == winner {
                winner_at = Some(conf_idx[at]);
                break;
            }
        }
        let Some(winner_at) = winner_at else {
            return Ok(None);
        };
        // Soft year preference only: a known query year prefers a challenger
        // carrying it at equal title strength, never a miss.
        let years: Vec<i32> = shows
            .iter()
            .map(|show| date_year(&show.premiered))
            .collect();
        let take =
            crate::yearpref::prefer_year_match(&q.title, q.year, &cands, &years, winner_at);
        let show = shows[take].clone();

        let year = date_year(&show.premiered);
        let genres = join_genres(&show.genres);
        let length = tvmaze_show_length(&show);
        let show_url = normalize_url(&show.url);

        let (episodes, episode_url) = if q.season > 0 && !q.absolute {
            let seasons_url = format!("{base}/shows/{}/seasons", show.id);
            let response = client.get(cancel, &seasons_url)?;
            let seasons: Vec<TvmazeSeason> = decode_capped(response)?;
            let mut season_id = -1;
            for season in &seasons {
                if i64::from(season.number) == q.season {
                    season_id = season.id;
                    break;
                }
            }
            if season_id < 0 {
                return Ok(None);
            }
            let episodes_url = format!("{base}/seasons/{season_id}/episodes");
            let response = client.get(cancel, &episodes_url)?;
            let raw: Vec<TvmazeEpisode> = decode_capped(response)?;
            let mut out: Vec<Episode> = Vec::with_capacity(raw.len());
            for e in &raw {
                if let Some(number) = e.number {
                    out.push(build_episode(e, number));
                }
            }
            out.sort_by_key(|episode| (episode.season, episode.number));
            let url = tvmaze_matched_url(&out, q);
            (out, url)
        } else {
            let episodes_url = format!("{base}/shows/{}/episodes", show.id);
            let response = client.get(cancel, &episodes_url)?;
            let mut raw: Vec<TvmazeEpisode> = decode_capped(response)?;
            // The nil-safe sort runs BEFORE the nil-numbered entries are
            // dropped, exactly as the Go original orders those two steps.
            raw.sort_by(|a, b| {
                (a.season, a.number.unwrap_or(0)).cmp(&(b.season, b.number.unwrap_or(0)))
            });
            let mut out: Vec<Episode> = Vec::with_capacity(raw.len());
            for e in &raw {
                if let Some(number) = e.number {
                    out.push(build_episode(e, number));
                }
            }
            if q.absolute {
                // Absolute N maps to the Nth episode in (season, number) air
                // order: renumber so number-exact matching holds. An empty or
                // short table simply lacks the Number, so the resolver falls
                // through to the next source.
                for (i, episode) in out.iter_mut().enumerate() {
                    episode.number = i as i32 + 1;
                }
            }
            let url = tvmaze_matched_url(&out, q);
            (out, url)
        };

        Ok(Some(Media {
            source: "tvmaze".to_string(),
            source_id: show.id.to_string(),
            kind: Kind::SERIES,
            title: show.name.clone(),
            year,
            artwork: tvmaze_art(show.image.as_ref()),
            synopsis: cap_synopsis(&strip_html(&show.summary)),
            genres,
            length,
            show_url,
            episode_url,
            movie_url: String::new(),
            trailer_url: String::new(),
            episodes,
            fetched_at: crate::timefmt::now_rfc3339(),
        }))
    }
}
