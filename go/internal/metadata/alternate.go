package metadata

import "strings"

// alternateQuery derives the single separator-normalized fallback for a
// series title ("Show - Arc" -> "Show: Arc"): every " - " rendered as ": ",
// result trimmed. Kind, Year, Season, Episode and Absolute carry over
// unchanged so episode matching still answers the original request.
//
// Exactly one alternate, never recursive, and only for KindSeries:
// KindUnknown and KindMovie never alternate. ok=false when there is no
// " - ", when the result trims empty, or for a non-series kind.
//
// The previous head-strip fallback ("Show - Arc" -> "Show") is deliberately
// NOT stacked here: it fabricated a confident wrong match to a different
// show for "[Erai-raws] Dragon Ball Super - Beerus - 01 [1080p ...].mkv"
// (resolved the 2015 "Dragon Ball Super" S1E1 with its poster instead of
// the 2026 "Dragon Ball Super: Beerus"), and a wrong card is worse than the
// bare tag it replaced.
func alternateQuery(q Query) (alt Query, ok bool) {
	if q.Kind != KindSeries {
		return Query{}, false
	}
	if !strings.Contains(q.Title, " - ") {
		return Query{}, false
	}
	normalized := strings.TrimSpace(strings.ReplaceAll(q.Title, " - ", ": "))
	if normalized == "" {
		return Query{}, false
	}
	alt = q
	alt.Title = normalized
	return alt, true
}
