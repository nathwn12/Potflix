package metadata

import "strings"

// alternateQuery derives the single head-segment fallback for an
// arc-suffixed series title ("Show - Arc" -> "Show"): the head before the
// FIRST " - ", trimmed. Kind, Year, Season, Episode and Absolute carry over
// unchanged so episode matching still answers the original request.
//
// Exactly one alternate, never recursive (the head is never re-split), and
// only for KindSeries: KindUnknown and KindMovie never alternate. ok=false
// when there is no " - ", when the head trims empty, or for a non-series
// kind.
func alternateQuery(q Query) (alt Query, ok bool) {
	if q.Kind != KindSeries {
		return Query{}, false
	}
	idx := strings.Index(q.Title, " - ")
	if idx < 0 {
		return Query{}, false
	}
	head := strings.TrimSpace(q.Title[:idx])
	if head == "" {
		return Query{}, false
	}
	alt = q
	alt.Title = head
	return alt, true
}
