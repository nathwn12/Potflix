//! `metadata::alternate` -- mirrors `go\internal\metadata\alternate.go`.
//!
//! The single separator-normalized fallback title for a series ("Show - Arc" ->
//! "Show: Arc"): every " - " rendered as ": ", result trimmed. Kind, Year,
//! Season, Episode and Absolute carry over unchanged so episode matching still
//! answers the original request.
//!
//! Exactly one alternate, never recursive, and only for KindSeries: KindUnknown
//! and KindMovie never alternate. `None` when there is no " - ", when the result
//! trims empty, or for a non-series kind.
//!
//! The previous head-strip fallback ("Show - Arc" -> "Show") is deliberately NOT
//! stacked here: it fabricated a confident wrong match to a different show for
//! "[Erai-raws] Dragon Ball Super - Beerus - 01 [1080p ...].mkv" (resolved the
//! 2015 "Dragon Ball Super" S1E1 with its poster instead of the 2026 "Dragon
//! Ball Super: Beerus"), and a wrong card is worse than the bare tag it
//! replaced.

use crate::types::{Kind, Query};

/// `alternateQuery` derives the fallback query. `None` mirrors Go's
/// `(Query{}, false)`.
pub fn alternate_query(q: &Query) -> Option<Query> {
    if q.kind != Kind::SERIES {
        return None;
    }
    if !q.title.contains(" - ") {
        return None;
    }
    let normalized = q.title.replace(" - ", ": ").trim().to_string();
    if normalized.is_empty() {
        return None;
    }
    let mut alt = q.clone();
    alt.title = normalized;
    Some(alt)
}
