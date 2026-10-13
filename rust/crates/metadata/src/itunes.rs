//! `metadata::itunes` -- mirrors `go\internal\metadata\itunes.go`.
//!
//! iTunes provider: the first movie source. Its search deliberately omits
//! `media=movie` -- that filter returns `resultCount 0`, and removing it is what
//! makes the movie chain work -- and it takes the first `results[]` entry whose
//! `kind` is exactly `"feature-movie"`.
//!
//! `Find` is movie-only and self-gates BEFORE the wire: a series query returns
//! `(nil, nil)` without a fetch, so the 3 s per-host limit is never paid for a
//! series miss that only invokes this source for its chain cost.
//!
//! `itunesArt` scales the 100x100 store thumbnail to 600x600 with a literal
//! first-occurrence substring replace, not a URL parse.

use std::time::Duration;

use metadata_core::parse::date_year;
use metadata_core::types::{Kind, Media, Query, millis_length, normalize_url};
use metadata_http::cancel::Cancel;
use metadata_http::http::{HttpClient, decode_capped};

use crate::Error;
use crate::artwork::{cap_synopsis, first_non_empty, strip_html};
use crate::serdeutil::null_default;
use crate::urlutil::query_escape;

/// The provider's default host.
pub const ITUNES_DEFAULT_BASE: &str = "https://itunes.apple.com";

/// `itunesSource`.
pub struct ItunesSource {
    base: String,
    client: HttpClient,
}

impl ItunesSource {
    /// A source pointed at `base` (`""` means [`ITUNES_DEFAULT_BASE`]).
    pub fn new(base: impl Into<String>, client: HttpClient) -> ItunesSource {
        ItunesSource {
            base: base.into(),
            client,
        }
    }

    fn base_url(&self) -> &str {
        if self.base.is_empty() {
            ITUNES_DEFAULT_BASE
        } else {
            &self.base
        }
    }
}

impl Default for ItunesSource {
    fn default() -> ItunesSource {
        ItunesSource::new("", HttpClient::new(Duration::ZERO))
    }
}

#[derive(Debug, Clone, Default, serde::Deserialize)]
struct ItunesResult {
    #[serde(default, deserialize_with = "null_default")]
    kind: String,
    #[serde(rename = "trackName", default, deserialize_with = "null_default")]
    track_name: String,
    #[serde(rename = "releaseDate", default, deserialize_with = "null_default")]
    release_date: String,
    #[serde(rename = "artworkUrl100", default, deserialize_with = "null_default")]
    artwork_url100: String,
    #[serde(
        rename = "shortDescription",
        default,
        deserialize_with = "null_default"
    )]
    short_description: String,
    #[serde(rename = "longDescription", default, deserialize_with = "null_default")]
    long_description: String,
    #[serde(
        rename = "primaryGenreName",
        default,
        deserialize_with = "null_default"
    )]
    primary_genre_name: String,
    #[serde(rename = "trackTimeMillis", default, deserialize_with = "null_default")]
    track_time_millis: i64,
    #[serde(rename = "trackViewUrl", default, deserialize_with = "null_default")]
    track_view_url: String,
    #[serde(rename = "previewUrl", default, deserialize_with = "null_default")]
    preview_url: String,
}

#[derive(Debug, Clone, Default, serde::Deserialize)]
struct ItunesSearchResp {
    #[serde(default, deserialize_with = "null_default")]
    results: Vec<ItunesResult>,
}

/// `itunesArt`: the 100x100 store thumbnail scaled to 600x600, first occurrence
/// only.
pub(crate) fn itunes_art(raw: &str) -> String {
    let raw = raw.trim();
    if raw.is_empty() {
        return String::new();
    }
    raw.replacen("100x100", "600x600", 1)
}

impl crate::resolver::Source for ItunesSource {
    fn name(&self) -> &str {
        "itunes"
    }

    /// `Find` is movie-only: it deliberately omits `media=movie` and takes the
    /// first `results[]` entry with `kind == "feature-movie"`.
    fn find(&self, cancel: &Cancel, q: &Query) -> Result<Option<Media>, Error> {
        if q.kind == Kind::SERIES || q.title.is_empty() {
            return Ok(None);
        }
        let search_url = format!(
            "{}/search?term={}&limit=5",
            self.base_url(),
            query_escape(&q.title)
        );
        let response = self.client.get(cancel, &search_url)?;
        let search: ItunesSearchResp = decode_capped(response)?;
        for result in &search.results {
            if result.kind != "feature-movie" {
                continue;
            }
            let title = result.track_name.trim();
            if title.is_empty() {
                continue;
            }
            return Ok(Some(Media {
                source: "itunes".to_string(),
                source_id: title.to_string(),
                kind: Kind::MOVIE,
                title: title.to_string(),
                year: date_year(&result.release_date),
                artwork: itunes_art(&result.artwork_url100),
                synopsis: cap_synopsis(&strip_html(&first_non_empty(&[
                    &result.long_description,
                    &result.short_description,
                ]))),
                genres: result.primary_genre_name.trim().to_string(),
                length: millis_length(result.track_time_millis),
                show_url: String::new(),
                episode_url: String::new(),
                movie_url: normalize_url(&result.track_view_url),
                trailer_url: normalize_url(&result.preview_url),
                episodes: Vec::new(),
                fetched_at: crate::timefmt::now_rfc3339(),
            }));
        }
        Ok(None)
    }
}
