//! `potflix-win::search` -- the "Fix match" provisional-override builder.
//!
//! [`provisional_override`] turns the picked [`Candidate`] into an
//! identity-only [`Override`] the dialog saves immediately, so the card can
//! become a series (or a movie) WITHOUT waiting for
//! [`metadata::pin_from_candidate`] to fetch the provider identity and the
//! whole episode table under its 90 s deadline. The full pin (or a later
//! [`metadata::refresh`]) replaces this provisional record with the fetched
//! payload.

use metadata::{Candidate, Kind, Override, Scope};

/// The most Unicode scalar values the one query field accepts. Past this the
/// field refuses the character (typed or pasted) and says so, so a 300-char
/// paste can never produce a title the user did not see.
pub const MAX_QUERY_CHARS: usize = 128;

/// The explicit search scope. `All` is the default and the main flow's scope:
/// the search is never constrained by a hidden kind flag left over from a
/// previous title. `Movies`/`Series` are a deliberately chosen Advanced filter.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum KindFilter {
    #[default]
    All,
    Movies,
    Series,
}

impl KindFilter {
    /// The provider legs this scope asks for, in leg precedence order.
    #[must_use]
    pub fn kinds(self) -> &'static [Kind] {
        match self {
            KindFilter::All => &[Kind::SERIES, Kind::MOVIE],
            KindFilter::Movies => &[Kind::MOVIE],
            KindFilter::Series => &[Kind::SERIES],
        }
    }

    /// The segment label, so the control and its tests read one string.
    #[must_use]
    pub fn label(self) -> &'static str {
        match self {
            KindFilter::All => "All types",
            KindFilter::Movies => "Movies",
            KindFilter::Series => "Series",
        }
    }

    /// Whether this scope includes `kind`.
    #[must_use]
    pub fn includes(self, kind: Kind) -> bool {
        self == KindFilter::All || self.kinds().contains(&kind)
    }
}

/// Why the one query cannot be searched. Every variant carries its own copy, so
/// an empty, malformed or year-only submit explains itself in the field's own
/// status line instead of running a provider search that can only come back
/// empty.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum QueryIssue {
    Empty,
    YearOnly,
    TooLong,
    NoTitleCharacters,
}

impl QueryIssue {
    #[must_use]
    pub fn message(self) -> &'static str {
        match self {
            QueryIssue::Empty => "Type a title to search.",
            QueryIssue::YearOnly => "Add a title - a year on its own has nothing to search for.",
            QueryIssue::TooLong => "That is longer than 128 characters - shorten the title.",
            QueryIssue::NoTitleCharacters => "Add at least one letter or digit.",
        }
    }
}

/// The query's own validation, shared by the button, the Enter key and the
/// paste path so all three refuse the same input for the same stated reason.
#[must_use]
pub fn query_issue(query: &str) -> Option<QueryIssue> {
    if query.chars().count() > MAX_QUERY_CHARS {
        return Some(QueryIssue::TooLong);
    }
    let trimmed = query.trim();
    if trimmed.is_empty() {
        return Some(QueryIssue::Empty);
    }
    let (title, year) = crate::window::parse_title_year(trimmed);
    if title.trim().is_empty() && year > 0 {
        return Some(QueryIssue::YearOnly);
    }
    if !title.chars().any(char::is_alphanumeric) {
        return Some(QueryIssue::NoTitleCharacters);
    }
    None
}

/// The Card workspace's one search model. It owns the single title/year query,
/// the explicit search scope, and the result set. The Fix match dialog seeds
/// its own widgets from this model when it opens and writes the edited query
/// back when it closes, so there is one canonical query on the main surface
/// rather than two independent search states.
#[derive(Clone, Debug, Default)]
pub struct MediaPicker {
    /// The one input: a title, or a title with a trailing year.
    pub query: String,
    /// The explicit scope. `All` by default, so nothing the user chose for an
    /// earlier title can narrow the next one.
    pub filter: KindFilter,
    pub candidates: Vec<Candidate>,
    pub selected: usize,
    pub busy: bool,
    pub searched: bool,
    /// Whether the result overlay is showing.
    pub open: bool,
    pub status: String,
    /// Monotonic worker generation: a stale result never overwrites a newer
    /// search.
    pub generation: u64,
    /// The row the user explicitly picked, keyed by provider + provider id.
    /// A pick belongs to the result set it was made in, so every query edit,
    /// scope change and new search clears it: the next list's auto-highlight is
    /// not a choice and must not arm Apply.
    pub picked: Option<String>,
}

impl MediaPicker {
    /// The one-field parse shared with the dialog.
    #[must_use]
    pub fn parsed(&self) -> (String, i32) {
        crate::window::parse_title_year(&self.query)
    }

    /// How many characters the field holds (scalar values, not bytes).
    #[must_use]
    pub fn query_len(&self) -> usize {
        self.query.chars().count()
    }

    /// The counter beside the field: `37/128`.
    #[must_use]
    pub fn counter_text(&self) -> String {
        format!("{}/{}", self.query_len(), MAX_QUERY_CHARS)
    }

    /// Whether one more character fits.
    #[must_use]
    pub fn accepts_more(&self) -> bool {
        self.query_len() < MAX_QUERY_CHARS
    }

    /// Whether Search is enabled: a query that passes [`query_issue`], and no
    /// running job.
    #[must_use]
    pub fn search_allowed(&self) -> bool {
        !self.busy && query_issue(&self.query).is_none()
    }

    /// The selected row, if the overlay has one.
    #[must_use]
    pub fn selected_candidate(&self) -> Option<&Candidate> {
        self.candidates.get(self.selected)
    }

    /// Record an explicit pick of one row (`provider + provider id`).
    pub fn pick_row(&mut self, key: String) {
        self.picked = Some(key);
    }

    /// Drop everything that belonged to the previous result set and abandon any
    /// running job.
    ///
    /// Bumping the generation is what makes the abandonment safe: a worker that
    /// finishes after this posts a generation that no longer matches, so
    /// [`MediaPicker::finish_search`] refuses its rows instead of filling a list
    /// for a query or scope the user has already left. Clearing `busy` is the
    /// other half -- the cancelled job must not keep Search disabled.
    fn invalidate_results(&mut self) {
        self.generation += 1;
        self.busy = false;
        self.candidates.clear();
        self.selected = 0;
        self.open = false;
        self.searched = false;
        self.status.clear();
        self.picked = None;
    }

    /// Edit the single query field. Clearing the query clears the stale result
    /// set, so the overlay can never show results for text that is no longer
    /// in the field.
    pub fn set_query(&mut self, query: String) {
        if self.query == query {
            return;
        }
        self.query = query;
        self.invalidate_results();
    }

    /// Apply the explicit scope filter and drop results that belonged to
    /// another scope. An in-flight search for the old scope is abandoned here:
    /// its completion no longer matches the generation, so it can never publish
    /// rows into the new scope's list.
    pub fn set_filter(&mut self, filter: KindFilter) {
        if self.filter == filter {
            return;
        }
        self.filter = filter;
        self.invalidate_results();
    }

    /// Mark a search running and return the worker input. A query that fails
    /// [`query_issue`] (or a running job) returns `None` and starts nothing.
    pub fn begin_search(&mut self) -> Option<(u64, String, i32, KindFilter)> {
        if !self.search_allowed() {
            return None;
        }
        let (title, year) = self.parsed();
        self.invalidate_results();
        self.busy = true;
        self.status = "Searching\u{2026}".to_string();
        Some((self.generation, title, year, self.filter))
    }

    /// Apply one finished search. `generation` rejects a stale worker post.
    pub fn finish_search(&mut self, generation: u64, rows: Vec<Candidate>) -> bool {
        if generation != self.generation {
            return false;
        }
        self.busy = false;
        self.searched = true;
        self.candidates = rows;
        self.selected = 0;
        self.open = !self.candidates.is_empty();
        let (title, year) = self.parsed();
        let asked = if year > 0 {
            format!("{title} ({year})")
        } else {
            title.clone()
        };
        self.status = if self.candidates.is_empty() {
            // The empty state names the query it failed on, so "no matches" is
            // never a silent blank panel.
            format!("No matches for \"{asked}\". Try a shorter title or add the year.")
        } else {
            format!(
                "{} match{} for \"{asked}\"",
                self.candidates.len(),
                if self.candidates.len() == 1 { "" } else { "es" }
            )
        };
        true
    }

    /// Apply a failed search: the providers could not be reached, which is a
    /// different state from "no matches". The query is preserved so a retry
    /// needs no retyping.
    pub fn fail_search(&mut self, generation: u64) -> bool {
        if generation != self.generation {
            return false;
        }
        self.busy = false;
        self.searched = true;
        self.candidates.clear();
        self.selected = 0;
        self.open = false;
        self.status = crate::window::FIX_MATCH_ERROR.to_string();
        true
    }

    /// Close the result overlay without touching the typed query.
    pub fn close_results(&mut self) {
        self.open = false;
    }
}

/// Merge the per-kind provider passes of an `All types` search into one list.
///
/// The providers are kind-scoped, so a mixed search is two passes; this is the
/// one place they are joined, and it is pure so the ordering is pinnable:
///   1. an exact (case-insensitive) title match first - the user typed it;
///   2. then newest year first (`0`/unknown last);
///   3. then title, then provider - a total order, so the row indices the
///      selection points at never shuffle between identical answers.
///   4. Exact duplicates (same provider + provider id) are dropped, so a leg
///      that answers twice cannot produce two identical rows.
#[must_use]
pub fn merge_candidates(query: &str, passes: Vec<Vec<Candidate>>) -> Vec<Candidate> {
    let want = query.trim().to_lowercase();
    let mut out: Vec<Candidate> = Vec::new();
    let mut seen: std::collections::HashSet<(String, String)> = std::collections::HashSet::new();
    for pass in passes {
        for cand in pass {
            let id = (cand.provider.clone(), cand.provider_id.clone());
            if seen.insert(id) {
                out.push(cand);
            }
        }
    }
    out.sort_by(|a, b| {
        let exact = |c: &Candidate| usize::from(c.title.trim().to_lowercase() != want);
        exact(a)
            .cmp(&exact(b))
            .then(b.year.cmp(&a.year))
            .then(a.title.cmp(&b.title))
            .then(a.provider.cmp(&b.provider))
    });
    out
}

/// The kind a matched record imposes. A confirmed match is painted with the
/// candidate's own kind: the manual Movie/TV choice only shapes the *search*
/// request, never the record the user ends up with.
#[must_use]
pub fn matched_kind_is_movie(cand: &Candidate) -> bool {
    cand.kind == Kind::MOVIE
}

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
    fn media_picker_keeps_one_query_and_rejects_a_stale_generation() {
        use super::MediaPicker;

        let mut picker = MediaPicker::default();
        picker.set_query("Dune 2021".to_string());
        assert_eq!(picker.parsed(), ("Dune".to_string(), 2021));
        assert_eq!(
            picker.filter,
            super::KindFilter::All,
            "the main search defaults to All types, never to a stale kind"
        );
        let (generation, title, year, want) = picker.begin_search().expect("search starts");
        assert_eq!((title.as_str(), year, want), ("Dune", 2021, super::KindFilter::All));
        assert!(picker.busy);
        assert!(
            !picker.finish_search(generation + 1, Vec::new()),
            "stale result rejected"
        );
        assert!(picker.busy, "a stale result must not clear the running state");
        let cand = Candidate {
            kind: Kind::MOVIE,
            title: "Dune".to_string(),
            year: 2021,
            provider: "cinemeta".to_string(),
            provider_id: "tt1".to_string(),
            poster: "https://example.com/dune.jpg".to_string(),
            episode_count: 0,
        };
        assert!(picker.finish_search(generation, vec![cand]));
        assert!(picker.open && !picker.busy);
        assert_eq!(picker.status, "1 match for \"Dune (2021)\"");
        picker.set_filter(super::KindFilter::Movies);
        assert_eq!(picker.filter, super::KindFilter::Movies);
        assert!(picker.candidates.is_empty() && !picker.open);
        picker.set_query("Dune: Part Two 2024".to_string());
        assert_eq!(picker.parsed(), ("Dune: Part Two".to_string(), 2024));
    }

    /// A scope change made while a search is in flight must cancel it: the
    /// worker's late completion carries the abandoned generation, so it cannot
    /// publish rows into the new scope's list, and Search is usable again
    /// immediately instead of waiting on a result nobody asked for.
    #[test]
    fn scope_change_during_an_in_flight_search_cannot_publish_stale_results() {
        use super::{KindFilter, MediaPicker};

        let mut picker = MediaPicker::default();
        picker.set_query("Dune".to_string());
        let (generation, ..) = picker.begin_search().expect("search starts");
        assert!(picker.busy, "the job is running");
        picker.set_filter(KindFilter::Movies);
        assert!(!picker.busy, "the abandoned job no longer blocks Search");
        assert!(
            !picker.finish_search(generation, vec![movie_candidate()]),
            "the old scope's completion must be refused"
        );
        assert!(
            picker.candidates.is_empty() && !picker.open && !picker.searched,
            "no stale row can populate the list"
        );
        // A fresh search in the new scope is the only thing that publishes.
        let (next, _, _, want) = picker.begin_search().expect("search starts again");
        assert_eq!(want, KindFilter::Movies, "the new scope is what is asked");
        assert!(next > generation, "the generation moved past the abandoned job");
        assert!(picker.finish_search(next, vec![movie_candidate()]));
        assert_eq!(picker.candidates.len(), 1);
        // The same rule covers a query edit, which is also a new result set.
        let (stale, ..) = picker.begin_search().expect("search starts");
        picker.set_query("Dune: Part Two".to_string());
        assert!(
            !picker.finish_search(stale, vec![movie_candidate()]),
            "a completion for the old query is refused after the text changed"
        );
    }

    /// An explicit pick belongs to one result set. A query edit, a scope change
    /// and a new search each clear it, so the next list's auto-highlight (even
    /// on the identical record) cannot arm Apply without a fresh pick.
    #[test]
    fn an_explicit_pick_is_cleared_by_a_query_edit_scope_change_or_new_search() {
        use super::{KindFilter, MediaPicker};

        let key = "cinemeta:tt15239678".to_string();
        let mut picker = MediaPicker::default();
        picker.set_query("Dune".to_string());
        let (generation, ..) = picker.begin_search().expect("search starts");
        assert!(picker.finish_search(generation, vec![movie_candidate()]));

        picker.pick_row(key.clone());
        assert_eq!(picker.picked.as_deref(), Some(key.as_str()));

        // Scope change.
        picker.set_filter(KindFilter::Movies);
        assert_eq!(picker.picked, None, "a scope change drops the pick");

        // Query edit, with the same pick re-armed first.
        picker.pick_row(key.clone());
        picker.set_query("Dune 2021".to_string());
        assert_eq!(picker.picked, None, "a query edit drops the pick");

        // A new search, with the same pick re-armed first: the same record can
        // come back, and it is still not a choice.
        let (generation, ..) = picker.begin_search().expect("search starts");
        assert!(picker.finish_search(generation, Vec::new()));
        picker.pick_row(key.clone());
        let (generation, ..) = picker.begin_search().expect("search starts");
        assert_eq!(picker.picked, None, "a new search drops the pick");
        assert!(picker.finish_search(generation, vec![movie_candidate()]));
        assert_eq!(picker.picked, None, "the fresh list carries no pick");
    }

    #[test]
    fn query_issue_names_empty_year_only_long_and_malformed_queries() {        use super::{KindFilter, MediaPicker, QueryIssue, query_issue};

        assert_eq!(query_issue(""), Some(QueryIssue::Empty));
        assert_eq!(query_issue("   "), Some(QueryIssue::Empty));
        assert_eq!(query_issue("2024"), Some(QueryIssue::YearOnly));
        assert_eq!(query_issue(" 1999 "), Some(QueryIssue::YearOnly));
        assert_eq!(query_issue("%%%"), Some(QueryIssue::NoTitleCharacters));
        assert_eq!(query_issue("Dune"), None);
        assert_eq!(query_issue("Dune 2021"), None);
        assert_eq!(query_issue("1899"), None, "1899 is a title, not a year");
        let long: String = "a".repeat(super::MAX_QUERY_CHARS);
        assert_eq!(query_issue(&long), None, "exactly the cap is accepted");
        let over: String = "a".repeat(super::MAX_QUERY_CHARS + 1);
        assert_eq!(query_issue(&over), Some(QueryIssue::TooLong));
        // Every issue explains itself and the search is refused for it.
        for (query, issue) in [
            ("", QueryIssue::Empty),
            ("2024", QueryIssue::YearOnly),
            ("%%%", QueryIssue::NoTitleCharacters),
        ] {
            let mut p = MediaPicker::default();
            p.set_query(query.to_string());
            assert!(!p.search_allowed(), "{query:?} must not search");
            assert!(!issue.message().is_empty());
            assert_eq!(p.filter, KindFilter::All);
        }
    }

    #[test]
    fn mixed_search_merges_both_kinds_and_keeps_kind_truthful() {
        use super::{KindFilter, merge_candidates};

        assert_eq!(KindFilter::All.kinds(), &[Kind::SERIES, Kind::MOVIE]);
        assert_eq!(KindFilter::Movies.kinds(), &[Kind::MOVIE]);
        assert_eq!(KindFilter::Series.kinds(), &[Kind::SERIES]);
        assert!(KindFilter::All.includes(Kind::MOVIE) && KindFilter::All.includes(Kind::SERIES));
        assert!(!KindFilter::Movies.includes(Kind::SERIES));

        let series = vec![Candidate {
            kind: Kind::SERIES,
            title: "Dune: Prophecy".to_string(),
            year: 2024,
            provider: "tvmaze".to_string(),
            provider_id: "tvmaze-1".to_string(),
            poster: String::new(),
            episode_count: 6,
        }];
        let movies = vec![
            Candidate {
                kind: Kind::MOVIE,
                title: "Dune".to_string(),
                year: 2021,
                provider: "cinemeta".to_string(),
                provider_id: "tt1160419".to_string(),
                poster: String::new(),
                episode_count: 0,
            },
            Candidate {
                kind: Kind::MOVIE,
                title: "Dune".to_string(),
                year: 1984,
                provider: "cinemeta".to_string(),
                provider_id: "tt0087182".to_string(),
                poster: String::new(),
                episode_count: 0,
            },
        ];
        let merged = merge_candidates("Dune", vec![series, movies]);
        assert_eq!(merged.len(), 3, "every leg's rows survive the merge");
        assert_eq!(merged[0].year, 2021, "exact title, newest year first");
        assert_eq!(merged[1].year, 1984);
        assert_eq!(merged[2].title, "Dune: Prophecy");
        assert!(
            merged.iter().any(|c| c.kind == Kind::SERIES)
                && merged.iter().any(|c| c.kind == Kind::MOVIE),
            "an All-types search returns both kinds in one list"
        );

        // A duplicate identity from two legs is one row, not two.
        let again = merge_candidates(
            "Dune",
            vec![vec![merged[0].clone()], vec![merged[0].clone()]],
        );
        assert_eq!(again.len(), 1, "provider+provider_id de-duplicates");
    }

    #[test]
    fn failed_search_is_its_own_state_and_keeps_the_query() {
        use super::MediaPicker;

        let mut p = MediaPicker::default();
        p.set_query("Dune".to_string());
        let (generation, ..) = p.begin_search().expect("search starts");
        assert!(p.fail_search(generation));
        assert!(!p.busy && !p.open && p.candidates.is_empty());
        assert_eq!(p.status, crate::window::FIX_MATCH_ERROR);
        assert_eq!(p.query, "Dune", "a failure never eats what was typed");
        assert!(!p.fail_search(generation + 1), "a stale failure is ignored");
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
