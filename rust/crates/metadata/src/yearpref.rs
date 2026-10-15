//! `metadata::yearpref` -- the bounded year preference shared by the search
//! picks (Cinemeta, Tenrai, Kitsu, TVmaze).
//!
//! Rule: at equal title strength, a candidate carrying the query's year
//! outranks one that does not. "Equal strength" means the challenger fully
//! covers the query title (every query token present in the candidate name),
//! so a rip-year typo can never drag in an unrelated entity, and a candidate
//! with no year is neither preferred nor penalised (only `year == query_year`
//! counts as a match).
//!
//! The preference is SOFT: it only swaps one complete answer for another and
//! never returns a miss. A year mismatch never rejects a provider holding the
//! essentials (cover, title, episode, synopsis); year is a nice-to-have.
//!
//! The real ranking stays in [`pick_best_show`]; this module only decides
//! whether a year-matching challenger may take the already-picked winner's
//! place.

use metadata_core::match_::{ShowCand, pick_best_show};

/// Lowercase alphanumeric tokens (punctuation folds to spaces, like the
/// matcher's own normalization, minus accent folding -- so this check is
/// conservative: it may decline a swap, never force a wrong one).
fn tokens(s: &str) -> Vec<String> {
    s.to_lowercase()
        .chars()
        .map(|c| if c.is_alphanumeric() { c } else { ' ' })
        .collect::<String>()
        .split_whitespace()
        .map(str::to_string)
        .collect()
}

/// Full query-token coverage: every query token appears in the candidate.
fn covers_title(query: &str, candidate: &str) -> bool {
    let q = tokens(query);
    if q.is_empty() {
        return false;
    }
    let c = tokens(candidate);
    q.iter().all(|t| c.contains(t))
}

/// The index to take: `winner`, unless `query_year > 0`, the winner's own
/// year disagrees, and some other candidate both carries the query year and
/// fully covers the query title -- then the best such challenger (ranked by
/// [`pick_best_show`] over the year-matching subset, so weight/popularity
/// still breaks ties among them).
pub(crate) fn prefer_year_match(
    query: &str,
    query_year: i32,
    cands: &[ShowCand],
    years: &[i32],
    winner: usize,
) -> usize {
    if query_year <= 0 || winner >= cands.len() || cands.len() != years.len() {
        return winner;
    }
    if years[winner] == query_year {
        return winner;
    }
    let mut sub: Vec<ShowCand> = Vec::new();
    let mut back: Vec<usize> = Vec::new();
    for (i, cand) in cands.iter().enumerate() {
        if years[i] == query_year && covers_title(query, &cand.name) {
            sub.push(cand.clone());
            back.push(i);
        }
    }
    if sub.is_empty() {
        return winner;
    }
    let Some(alt) = pick_best_show(query, &sub) else {
        return winner;
    };
    sub.iter().position(|c| *c == alt).map(|at| back[at]).unwrap_or(winner)
}
