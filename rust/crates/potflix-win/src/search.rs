//! `potflix-win::search` -- the "Fix match" provisional-override builder.
//!
//! [`provisional_override`] turns the picked [`Candidate`] into an
//! identity-only [`Override`] the dialog saves immediately, so the card can
//! become a series (or a movie) WITHOUT waiting for
//! [`metadata::pin_from_candidate`] to fetch the provider identity and the
//! whole episode table under its 90 s deadline. The full pin (or a later
//! [`metadata::refresh`]) replaces this provisional record with the fetched
//! payload.

use metadata::{Candidate, Override, Scope};

/// The provisional latch for one picked candidate: the candidate's own
/// title/year/provider identity as the [`Override`] identity, and nothing
/// fetched.
///
/// The identity/poster rules mirror
/// `metadata::search::pin_from_candidate_with` exactly, minus the network:
/// the title, year, provider and provider id are the candidate's own; the
/// candidate's cover rides as [`Override::poster`] for every NON-movie scope
/// (a movie renders from its media snapshot, so a movie carries no poster);
/// [`Override::media`] is `None` and [`Override::episodes`] is empty -- there
/// is no episode table until the full pin fetches it.
///
/// `key` is the caller-selected target key (movie file, season folder, show
/// folder) and passes through byte-for-byte: this helper never normalises and
/// never derives -- [`metadata::overrides::Store::put`] normalises on write.
/// `path` is the file the correction is for, threaded through for the
/// [`metadata::overrides::Store::put_correction`] call the UI lane makes next
/// (which derives the conflicting-scope keys from it); no key is invented
/// from it here.
///
/// No network, no fetch, no store write: the caller persists the record
/// (usually [`metadata::overrides::Store::put_correction`]) and the full pin
/// or refresh later replaces it with the fetched media and episode pins.
#[must_use]
pub fn provisional_override(cand: &Candidate, scope: Scope, key: String, path: &str) -> Override {
    let _ = path;
    Override {
        scope,
        key,
        title: cand.title.clone(),
        year: cand.year,
        provider: cand.provider.clone(),
        provider_id: cand.provider_id.clone(),
        poster: if scope == Scope::Movie {
            String::new()
        } else {
            cand.poster.clone()
        },
        media: None,
        episodes: Vec::new(),
        created_at_ms: metadata::overrides::now_ms(),
    }
}

#[cfg(test)]
mod tests {
    use metadata::Candidate;
    use metadata::Scope;
    use metadata_core::types::Kind;

    use super::provisional_override;

    fn series_candidate() -> Candidate {
        Candidate {
            kind: Kind::SERIES,
            title: "Show".to_string(),
            year: 2008,
            provider: "tvmaze".to_string(),
            provider_id: "169".to_string(),
            poster: "https://art/show-picked.jpg".to_string(),
            episode_count: 3,
        }
    }

    fn movie_candidate() -> Candidate {
        Candidate {
            kind: Kind::MOVIE,
            title: "Dune: Part Two".to_string(),
            year: 2024,
            provider: "cinemeta".to_string(),
            provider_id: "tt15239678".to_string(),
            poster: "https://art/dune-search.jpg".to_string(),
            episode_count: 0,
        }
    }

    #[test]
    fn series_candidate_yields_provisional_series_override_without_network() {
        let cand = series_candidate();
        let over = provisional_override(
            &cand,
            Scope::Series,
            "M:\\2SERIES\\Show".to_string(),
            "M:\\2SERIES\\Show\\Season 1\\s01e01.mkv",
        );
        assert_eq!(over.scope, Scope::Series);
        assert_eq!(over.key, "M:\\2SERIES\\Show");
        assert_eq!(over.title, "Show");
        assert_eq!(over.year, 2008);
        assert_eq!(over.provider, "tvmaze");
        assert_eq!(over.provider_id, "169");
        assert_eq!(over.poster, "https://art/show-picked.jpg");
        assert!(over.media.is_none(), "provisional: no fetched record");
        assert!(over.episodes.is_empty(), "provisional: no episode table");
        assert!(over.created_at_ms > 0, "the provisional latch is stamped");
    }

    #[test]
    fn movie_candidate_carries_no_poster_like_a_pinned_movie() {
        let cand = movie_candidate();
        let over = provisional_override(
            &cand,
            Scope::Movie,
            "D:\\Films\\Dune Part Two.mkv".to_string(),
            "D:\\Films\\Dune Part Two.mkv",
        );
        assert_eq!(over.scope, Scope::Movie);
        assert_eq!(over.title, "Dune: Part Two");
        assert_eq!(over.year, 2024);
        assert_eq!(over.provider, "cinemeta");
        assert_eq!(over.provider_id, "tt15239678");
        assert!(
            over.poster.is_empty(),
            "a movie renders from its media snapshot, never the candidate cover"
        );
        assert!(over.media.is_none());
        assert!(over.episodes.is_empty());
        assert!(over.created_at_ms > 0);
    }

    #[test]
    fn key_and_scope_pass_through_exactly_and_path_invents_no_key() {
        let cand = series_candidate();
        // Mixed case plus `/` separators: the helper must NOT normalise --
        // [`metadata::overrides::Store::put`] normalises on write.
        let key = "M:/2SERIES/Show".to_string();
        let first = provisional_override(
            &cand,
            Scope::Season,
            key.clone(),
            "M:\\2SERIES\\Show\\Season 1\\s01e01.mkv",
        );
        assert_eq!(first.scope, Scope::Season);
        assert_eq!(first.key, key, "key passes through byte-for-byte");
        // A season scope is not a movie scope, so the candidate cover rides.
        assert_eq!(first.poster, "https://art/show-picked.jpg");
        // A different path must not change the record: the key is supplied,
        // never derived.
        let second = provisional_override(
            &cand,
            Scope::Season,
            key.clone(),
            "M:\\2SERIES\\Show\\Season 2\\s02e01.mkv",
        );
        assert_eq!(second.key, first.key);
        assert_eq!(second.title, first.title);
        assert_eq!(second.provider, first.provider);
        assert_eq!(second.provider_id, first.provider_id);
        assert_eq!(second.poster, first.poster);
        assert!(second.created_at_ms > 0);
    }

    #[test]
    fn provisional_override_touches_no_store_file() {
        let store_path = metadata::overrides::default_path();
        let before_bytes = std::fs::read(&store_path).ok();
        let before_meta = std::fs::metadata(&store_path)
            .ok()
            .map(|m| (m.len(), m.modified().ok()));
        let cand = series_candidate();
        for _ in 0..3 {
            let over = provisional_override(
                &cand,
                Scope::Series,
                "M:\\2SERIES\\Show".to_string(),
                "M:\\2SERIES\\Show\\Season 1\\s01e01.mkv",
            );
            assert!(over.media.is_none());
        }
        let after_bytes = std::fs::read(&store_path).ok();
        let after_meta = std::fs::metadata(&store_path)
            .ok()
            .map(|m| (m.len(), m.modified().ok()));
        assert_eq!(
            before_bytes, after_bytes,
            "the helper must not create, modify or delete the store file"
        );
        assert_eq!(before_meta, after_meta);
    }
}
