//! `metadata::cinemeta` -- Stremio's default catalog addon as a keyless movie and
//! series source (`com.linvo.cinemeta`, base [`CINEMETA_DEFAULT_BASE`]).
//!
//! Wire shape: `/catalog/<movie|series>/top/search=<title>.json` resolves the
//! title to an IMDb id, then `/meta/<movie|series>/<id>.json` carries the
//! details (year, `runtime` like `"148 min"`, `genre[]`, `description`,
//! `poster`) plus the series episode list (`videos[]`) in the same call.
//!
//! Two behaviours that are contract, not detail:
//!
//! * the catalog pick is title-validated with [`pick_best_show`]: a
//!   low-confidence best returns a definitive MISS rather than a wrong movie
//!   (the iTunes leg it replaces returned the first `feature-movie` row with
//!   no title check at all);
//! * year NEVER rejects: a known query year only PREFERS, at equal title
//!   strength, the candidate carrying it (any challenger fully covering the
//!   query title, not just a duplicate exact one) and otherwise rides along
//!   as a display field. A missing year, or a year that disagrees with the
//!   filename, never skips a provider holding the essentials.
//!
//! An `Absolute` series query renumbers the episode table positionally in
//! `(season, number)` air order, exactly as TVmaze does, so the resolver's
//! number-exact match holds.

use std::time::Duration;

use metadata_core::match_::{ShowCand, pick_best_show};
use metadata_core::types::{
    Episode, Kind, Media, Query, join_genres, normalize_url, runtime_length,
};
use metadata_http::cancel::Cancel;
use metadata_http::http::{HttpClient, decode_capped, drain_and_close};
use ureq::http::StatusCode;

use crate::Error;
use crate::artwork::{cap_synopsis, strip_html};
use crate::serdeutil::null_default;
use crate::urlutil::path_escape;

/// The provider's default host (Stremio Cinemeta v3).
pub const CINEMETA_DEFAULT_BASE: &str = "https://v3-cinemeta.strem.io";

/// `cinemetaSource`.
pub struct CinemetaSource {
    base: String,
    client: HttpClient,
}

impl CinemetaSource {
    /// A source pointed at `base` (`""` means [`CINEMETA_DEFAULT_BASE`]).
    pub fn new(base: impl Into<String>, client: HttpClient) -> CinemetaSource {
        CinemetaSource {
            base: base.into(),
            client,
        }
    }

    fn base_url(&self) -> &str {
        if self.base.is_empty() {
            CINEMETA_DEFAULT_BASE
        } else {
            &self.base
        }
    }
}

impl Default for CinemetaSource {
    fn default() -> CinemetaSource {
        CinemetaSource::new("", HttpClient::new(Duration::ZERO))
    }
}

#[derive(Debug, Clone, Default, serde::Deserialize)]
struct CinemetaMeta {
    #[serde(default, deserialize_with = "null_default")]
    id: String,
    #[serde(default, deserialize_with = "null_default")]
    name: String,
    #[serde(rename = "releaseInfo", default, deserialize_with = "null_default")]
    release_info: String,
}

#[derive(Debug, Clone, Default, serde::Deserialize)]
struct CinemetaCatalogResp {
    #[serde(default, deserialize_with = "null_default")]
    metas: Vec<CinemetaMeta>,
}

#[derive(Debug, Clone, Default, serde::Deserialize)]
struct CinemetaVideo {
    #[serde(default, deserialize_with = "null_default")]
    name: String,
    #[serde(default)]
    season: Option<i32>,
    #[serde(default)]
    number: Option<i32>,
    #[serde(default, deserialize_with = "null_default")]
    overview: String,
    #[serde(default, deserialize_with = "null_default")]
    thumbnail: String,
}

#[derive(Debug, Clone, Default, serde::Deserialize)]
struct CinemetaDetails {
    #[serde(default, deserialize_with = "null_default")]
    name: String,
    #[serde(default, deserialize_with = "null_default")]
    year: String,
    #[serde(default, deserialize_with = "null_default")]
    runtime: String,
    #[serde(default, deserialize_with = "null_default")]
    genre: Vec<String>,
    #[serde(default, deserialize_with = "null_default")]
    description: String,
    #[serde(default, deserialize_with = "null_default")]
    poster: String,
    #[serde(default, deserialize_with = "null_default")]
    videos: Vec<CinemetaVideo>,
}

#[derive(Debug, Clone, Default, serde::Deserialize)]
struct CinemetaMetaResp {
    #[serde(default)]
    meta: Option<CinemetaDetails>,
}

/// `releaseYear`: the leading year of a catalog `releaseInfo` (`"2010"` or
/// `"2008-2013"`) or a meta `year` (`"2010"`); 0 when unknown.
fn release_year(s: &str) -> i32 {
    let digits: String = s.trim().chars().take_while(|c| c.is_ascii_digit()).collect();
    if digits.len() >= 4 {
        digits[..4].parse().unwrap_or(0)
    } else {
        0
    }
}

/// `runtimeMinutes` parses Cinemeta's `"148 min"` (with an `"Xh Ym"` fallback);
/// 0 when unparseable.
fn runtime_minutes(s: &str) -> i32 {
    let s = s.trim();
    if s.is_empty() {
        return 0;
    }
    if let Some(mins) = s.strip_suffix("min").and_then(|n| n.trim().parse().ok()) {
        return mins;
    }
    let mut total = 0;
    let mut current = String::new();
    let mut seen = false;
    for c in s.chars() {
        if c.is_ascii_digit() {
            current.push(c);
        } else if (c == 'h' || c == 'm') && !current.is_empty() {
            let n: i32 = current.parse().unwrap_or(0);
            current.clear();
            seen = true;
            if c == 'h' {
                total += n * 60;
            } else {
                total += n;
            }
        } else if !c.is_whitespace() {
            current.clear();
        }
    }
    if seen { total } else { 0 }
}

/// The IMDb page for `id`, for the card's View button.
fn imdb_url(id: &str) -> String {
    if id.trim().is_empty() {
        return String::new();
    }
    normalize_url(&format!("https://www.imdb.com/title/{}/", id.trim()))
}

fn build_episode(video: &CinemetaVideo, season: i32, number: i32) -> Episode {
    Episode {
        season,
        number,
        title: video.name.trim().to_string(),
        artwork: normalize_url(&video.thumbnail),
        synopsis: cap_synopsis(&strip_html(&video.overview)),
        url: String::new(),
        length: String::new(),
    }
}

impl CinemetaSource {
    /// One validated catalog pick for `catalog` (`"movie"` or `"series"`):
    /// the title-ranked best, with a known query year preferring any
    /// challenger that both carries it and fully covers the query title.
    /// `Ok(None)` is a definitive miss. Year never gates beyond that soft
    /// preference.
    fn pick(
        &self,
        cancel: &Cancel,
        q: &Query,
        catalog: &str,
    ) -> Result<Option<CinemetaMeta>, Error> {
        let search_url = format!(
            "{}/catalog/{catalog}/top/search={}.json",
            self.base_url(),
            path_escape(&q.title)
        );
        let response = self.client.get(cancel, &search_url)?;
        if response.status() == StatusCode::NOT_FOUND {
            drain_and_close(response);
            return Ok(None);
        }
        let catalog_resp: CinemetaCatalogResp = decode_capped(response)?;
        let mut cands: Vec<ShowCand> = Vec::with_capacity(catalog_resp.metas.len());
        let mut entries: Vec<CinemetaMeta> = Vec::with_capacity(catalog_resp.metas.len());
        for meta in catalog_resp.metas {
            if meta.id.trim().is_empty() || meta.name.trim().is_empty() {
                continue;
            }
            cands.push(ShowCand {
                name: meta.name.clone(),
                weight: 0,
                episode_count: 0,
            });
            entries.push(meta);
        }
        if cands.is_empty() {
            return Ok(None);
        }
        let Some(winner) = pick_best_show(&q.title, &cands) else {
            // Low-confidence best: a wrong title is worse than no title.
            return Ok(None);
        };
        let mut winner_idx = None;
        for (i, cand) in cands.iter().enumerate() {
            if *cand == winner {
                winner_idx = Some(i);
                break;
            }
        }
        let Some(winner_idx) = winner_idx else {
            return Ok(None);
        };
        // A known query year prefers any challenger that both carries it and
        // fully covers the query title (the live catalog ranks the 2015
        // "Dragon Ball Super" above the 2026 "Dragon Ball Super: Beerus" on
        // title alone). Soft only: no year match keeps the title winner.
        let years: Vec<i32> = entries
            .iter()
            .map(|entry| release_year(&entry.release_info))
            .collect();
        let take = crate::yearpref::prefer_year_match(&q.title, q.year, &cands, &years, winner_idx);
        Ok(Some(entries[take].clone()))
    }

    /// The `/meta` details for a validated catalog pick.
    fn details(
        &self,
        cancel: &Cancel,
        kind: &str,
        picked: &CinemetaMeta,
    ) -> Result<Option<CinemetaDetails>, Error> {
        let meta_url = format!(
            "{}/meta/{kind}/{}.json",
            self.base_url(),
            path_escape(picked.id.trim())
        );
        let response = self.client.get(cancel, &meta_url)?;
        if response.status() == StatusCode::NOT_FOUND {
            drain_and_close(response);
            return Ok(None);
        }
        let parsed: CinemetaMetaResp = decode_capped(response)?;
        Ok(parsed.meta)
    }

    fn find_movie(&self, cancel: &Cancel, q: &Query) -> Result<Option<Media>, Error> {
        let Some(picked) = self.pick(cancel, q, "movie")? else {
            return Ok(None);
        };
        let Some(details) = self.details(cancel, "movie", &picked)? else {
            return Ok(None);
        };
        let title = details.name.trim();
        if title.is_empty() {
            return Ok(None);
        }
        let mut year = release_year(&details.year);
        if year == 0 {
            year = release_year(&picked.release_info);
        }
        Ok(Some(Media {
            source: "cinemeta".to_string(),
            source_id: picked.id.trim().to_string(),
            kind: Kind::MOVIE,
            title: title.to_string(),
            year,
            artwork: normalize_url(&details.poster),
            synopsis: cap_synopsis(&strip_html(&details.description)),
            genres: join_genres(&details.genre),
            length: runtime_length(runtime_minutes(&details.runtime)),
            show_url: String::new(),
            episode_url: String::new(),
            movie_url: imdb_url(picked.id.trim()),
            trailer_url: String::new(),
            episodes: Vec::new(),
            fetched_at: crate::timefmt::now_rfc3339(),
        }))
    }

    fn find_series(&self, cancel: &Cancel, q: &Query) -> Result<Option<Media>, Error> {
        let Some(picked) = self.pick(cancel, q, "series")? else {
            return Ok(None);
        };
        let Some(details) = self.details(cancel, "series", &picked)? else {
            return Ok(None);
        };
        let title = details.name.trim();
        if title.is_empty() {
            return Ok(None);
        }
        let mut year = release_year(&details.year);
        if year == 0 {
            year = release_year(&picked.release_info);
        }
        let mut episodes: Vec<Episode> = Vec::with_capacity(details.videos.len());
        for video in &details.videos {
            let Some(number) = video.number else {
                continue;
            };
            episodes.push(build_episode(video, video.season.unwrap_or(0), number));
        }
        episodes.sort_by_key(|episode| (episode.season, episode.number));
        if q.absolute {
            // Absolute N maps to the Nth entry in (season, number) air order,
            // exactly as the TVmaze source renumbers, so number-exact matching
            // holds for absolute anime queries.
            for (i, episode) in episodes.iter_mut().enumerate() {
                episode.number = i as i32 + 1;
            }
        }
        Ok(Some(Media {
            source: "cinemeta".to_string(),
            source_id: picked.id.trim().to_string(),
            kind: Kind::SERIES,
            title: title.to_string(),
            year,
            artwork: normalize_url(&details.poster),
            synopsis: cap_synopsis(&strip_html(&details.description)),
            genres: join_genres(&details.genre),
            length: String::new(),
            show_url: imdb_url(picked.id.trim()),
            episode_url: String::new(),
            movie_url: String::new(),
            trailer_url: String::new(),
            episodes,
            fetched_at: crate::timefmt::now_rfc3339(),
        }))
    }
}

impl crate::resolver::Source for CinemetaSource {
    fn name(&self) -> &str {
        "cinemeta"
    }

    /// `Find`: movies via `/catalog/movie`, series via `/catalog/series`. An
    /// `UNKNOWN` query tries series first, then movie (a series answer can
    /// only match when the query carries an episode number).
    fn find(&self, cancel: &Cancel, q: &Query) -> Result<Option<Media>, Error> {
        if q.title.is_empty() {
            return Ok(None);
        }
        if q.kind == Kind::MOVIE {
            return self.find_movie(cancel, q);
        }
        if q.kind == Kind::SERIES {
            return self.find_series(cancel, q);
        }
        if let Some(media) = self.find_series(cancel, q)? {
            return Ok(Some(media));
        }
        self.find_movie(cancel, q)
    }
}
