//! `metadata::jikan` -- mirrors `go\internal\metadata\jikan.go`.
//!
//! Jikan provider (anime branch, second half of the anime pair): `/anime?q=`
//! then `/anime/<id>/episodes` walked in page order.
//!
//! Three behaviours that are contract:
//!
//! * the episode NUMBER is `data[].episode`, NEVER the slice index or the page
//!   index -- pages may be unordered and gap-filled;
//! * only the TOP search result is used, with no re-ranking;
//! * `Length` is the raw `duration` string ("24 min per ep"), not reformatted.
//!
//! The pagination loop is bounded by [`MAX_JIKAN_PAGES`] and checks the scope's
//! cancellation on every iteration.

use std::time::Duration;

use metadata_core::parse::date_year;
use metadata_core::types::{Episode, Kind, Media, Query, join_genres, normalize_url};
use metadata_http::cancel::Cancel;
use metadata_http::http::{HttpClient, decode_capped};

use crate::Error;
use crate::artwork::{cap_synopsis, first_non_empty, strip_html};
use crate::serdeutil::null_default;
use crate::urlutil::query_escape;

/// The provider's default host.
pub const JIKAN_DEFAULT_BASE: &str = "https://api.jikan.moe/v4";

/// `maxJikanPages` bounds the episode pagination loop.
pub const MAX_JIKAN_PAGES: i32 = 50;

/// `jikanSource`.
pub struct JikanSource {
    base: String,
    client: HttpClient,
}

impl JikanSource {
    /// A source pointed at `base` (`""` means [`JIKAN_DEFAULT_BASE`]).
    pub fn new(base: impl Into<String>, client: HttpClient) -> JikanSource {
        JikanSource {
            base: base.into(),
            client,
        }
    }

    fn base_url(&self) -> &str {
        if self.base.is_empty() {
            JIKAN_DEFAULT_BASE
        } else {
            &self.base
        }
    }
}

impl Default for JikanSource {
    fn default() -> JikanSource {
        JikanSource::new("", HttpClient::new(Duration::ZERO))
    }
}

#[derive(Debug, Clone, Default, serde::Deserialize)]
struct JikanGenre {
    #[serde(default, deserialize_with = "null_default")]
    name: String,
}

#[derive(Debug, Clone, Default, serde::Deserialize)]
struct JikanJpg {
    #[serde(rename = "image_url", default, deserialize_with = "null_default")]
    image_url: String,
    #[serde(rename = "large_image_url", default, deserialize_with = "null_default")]
    large_image_url: String,
}

#[derive(Debug, Clone, Default, serde::Deserialize)]
struct JikanImages {
    #[serde(default, deserialize_with = "null_default")]
    jpg: JikanJpg,
}

#[derive(Debug, Clone, Default, serde::Deserialize)]
struct JikanAired {
    #[serde(default)]
    from: Option<String>,
}

#[derive(Debug, Clone, Default, serde::Deserialize)]
struct JikanAnime {
    #[serde(rename = "mal_id", default, deserialize_with = "null_default")]
    mal_id: i32,
    #[serde(default, deserialize_with = "null_default")]
    title: String,
    #[serde(rename = "title_english", default)]
    title_english: Option<String>,
    #[serde(default, deserialize_with = "null_default")]
    synopsis: String,
    #[serde(default, deserialize_with = "null_default")]
    duration: String,
    #[serde(default, deserialize_with = "null_default")]
    url: String,
    #[serde(default, deserialize_with = "null_default")]
    genres: Vec<JikanGenre>,
    #[serde(default)]
    images: Option<JikanImages>,
    #[serde(default)]
    year: Option<i32>,
    #[serde(default)]
    aired: Option<JikanAired>,
}

#[derive(Debug, Clone, Default, serde::Deserialize)]
struct JikanSearchResp {
    #[serde(default, deserialize_with = "null_default")]
    data: Vec<JikanAnime>,
}

#[derive(Debug, Clone, Default, serde::Deserialize)]
struct JikanEpisodeEntry {
    #[serde(default)]
    episode: Option<i32>,
    #[serde(default, deserialize_with = "null_default")]
    title: String,
}

#[derive(Debug, Clone, Default, serde::Deserialize)]
struct JikanPagination {
    #[serde(rename = "last_visible_page", default)]
    last_visible_page: Option<i32>,
}

#[derive(Debug, Clone, Default, serde::Deserialize)]
struct JikanEpsResp {
    #[serde(default, deserialize_with = "null_default")]
    data: Vec<JikanEpisodeEntry>,
    #[serde(default, deserialize_with = "null_default")]
    pagination: JikanPagination,
}

impl crate::resolver::Source for JikanSource {
    fn name(&self) -> &str {
        "jikan"
    }

    /// `Find`: `/anime?q=<title>&limit=5`, then `/anime/<malId>/episodes?page=N`
    /// up to `pagination.last_visible_page` and [`MAX_JIKAN_PAGES`].
    fn find(&self, cancel: &Cancel, q: &Query) -> Result<Option<Media>, Error> {
        if q.kind == Kind::MOVIE || q.title.is_empty() {
            return Ok(None);
        }
        let base = self.base_url();
        let client = &self.client;

        let search_url = format!("{base}/anime?q={}&limit=5", query_escape(&q.title));
        let response = client.get(cancel, &search_url)?;
        let search: JikanSearchResp = decode_capped(response)?;
        let Some(top) = search.data.first() else {
            return Ok(None);
        };
        if top.mal_id == 0 {
            return Ok(None);
        }
        let mut title = top.title.trim().to_string();
        if let Some(english) = top.title_english.as_deref()
            && !english.trim().is_empty()
        {
            title = english.trim().to_string();
        }
        let year = match top.year {
            Some(year) => year,
            None => top
                .aired
                .as_ref()
                .and_then(|aired| aired.from.as_deref())
                .map(date_year)
                .unwrap_or(0),
        };
        let genre_names: Vec<String> = top
            .genres
            .iter()
            .map(|genre| genre.name.trim().to_string())
            .filter(|name| !name.is_empty())
            .collect();

        let mut episodes: Vec<Episode> = Vec::with_capacity(64);
        let mut last_page = 1;
        let mut page = 1;
        while page <= last_page && page <= MAX_JIKAN_PAGES {
            if let Some(reason) = cancel.err() {
                return Err(Error::Cancelled(reason));
            }
            let page_url = format!("{base}/anime/{}/episodes?page={page}", top.mal_id);
            let response = client.get(cancel, &page_url)?;
            let parsed: JikanEpsResp = decode_capped(response)?;
            for entry in &parsed.data {
                let Some(number) = entry.episode else {
                    continue;
                };
                // Number is the authoritative data[].episode field, never the
                // slice index: pages may be unordered or gap-filled.
                episodes.push(Episode {
                    season: 0,
                    number,
                    title: entry.title.trim().to_string(),
                    ..Episode::default()
                });
            }
            if let Some(visible) = parsed.pagination.last_visible_page
                && visible > last_page
            {
                last_page = visible;
            }
            page += 1;
        }
        episodes.sort_by_key(|episode| episode.number);

        let artwork = match top.images.as_ref() {
            Some(images) => first_non_empty(&[&images.jpg.large_image_url, &images.jpg.image_url]),
            None => String::new(),
        };
        Ok(Some(Media {
            source: "jikan".to_string(),
            source_id: top.mal_id.to_string(),
            kind: Kind::SERIES,
            title,
            year,
            artwork,
            synopsis: cap_synopsis(&strip_html(&top.synopsis)),
            genres: join_genres(&genre_names),
            length: top.duration.trim().to_string(),
            show_url: normalize_url(&top.url),
            episode_url: String::new(),
            movie_url: String::new(),
            trailer_url: String::new(),
            episodes,
            fetched_at: crate::timefmt::now_rfc3339(),
        }))
    }
}
