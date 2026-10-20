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
///
/// No network, no fetch, no store write: the caller persists the record
/// (usually [`metadata::overrides::Store::put_correction`]) and the full pin
/// or refresh later replaces it with the fetched media and episode pins.
#[must_use]
pub fn provisional_override(cand: &Candidate, scope: Scope, key: String) -> Override {
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
        let over = provisional_override(&cand, Scope::Series, "M:\\2SERIES\\Show".to_string());
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
    fn key_and_scope_pass_through_exactly() {
        let cand = series_candidate();
        // Mixed case plus `/` separators: the helper must NOT normalise --
        // [`metadata::overrides::Store::put`] normalises on write.
        let key = "M:/2SERIES/Show".to_string();
        let over = provisional_override(&cand, Scope::Season, key.clone());
        assert_eq!(over.scope, Scope::Season);
        assert_eq!(over.key, key, "key passes through byte-for-byte");
        // A season scope is not a movie scope, so the candidate cover rides.
        assert_eq!(over.poster, "https://art/show-picked.jpg");
        assert!(over.media.is_none(), "provisional: no fetched record");
        assert!(over.episodes.is_empty(), "provisional: no episode table");
        assert!(over.created_at_ms > 0);
    }

    #[test]
    fn provisional_override_persists_through_the_caller_store_write() {
        // The helper itself takes no store handle (pure builder); the
        // documented contract is that the CALLER persists the record. This
        // drives that reachable path: build, save through `put_correction`,
        // reload, resolve -- proving the built record is persistable, not
        // merely that a pure function writes nothing.
        crate::test_env::EnvGuard::with_scratch("search-persist", |dir| {
            let store_path = metadata::overrides::default_path();
            assert!(
                store_path.starts_with(dir),
                "the store resolves under scratch: {}",
                store_path.display()
            );
            let path = crate::window::fix_series_path();
            let key = metadata::overrides::series_key_for(&path).expect("series key");
            let cand = series_candidate();
            let over = provisional_override(&cand, Scope::Series, key.clone());
            assert_eq!(over.key, key);
            assert!(over.media.is_none(), "provisional: no fetched record");
            assert!(over.episodes.is_empty(), "provisional: no episode table");
            let mut store = metadata::overrides::load()
                .unwrap_or_else(|_| metadata::overrides::Store::empty());
            store.put_correction(&path, over.clone()).expect("caller save");
            let reloaded = metadata::overrides::load().expect("reload");
            let got = reloaded.resolve_for(&path).expect("persisted latch");
            assert_eq!(got.title, "Show");
            assert_eq!(got.provider_id, "169");
            assert_eq!(got.poster, "https://art/show-picked.jpg");
            assert!(got.media.is_none());
            assert!(got.episodes.is_empty());
        });
    }
}
