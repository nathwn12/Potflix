package metadata

import (
	"regexp"
	"strconv"
	"strings"

	"potflix/internal/titleclean"
)

// potPlayerSuffix mirrors player.Suffix (" - PotPlayer"). It is duplicated
// here so this package imports only stdlib plus titleclean.
const potPlayerSuffix = " - PotPlayer"

var (
	seasonEpisodeRe = regexp.MustCompile(`(?i)^S\s*(\d{1,4})\s*E\s*(\d{1,3})(?:\s*-\s*\d{1,3})?$`)
	episodeOnlyRe   = regexp.MustCompile(`(?i)^E\s*(\d{1,4})$`)
	yearSuffixRe    = regexp.MustCompile(`^(.*)\s+\((\d{4})\)\s*$`)
	// themeRe spots OP/ED theme tokens (OP 1, ED 01v2, OP1, Show - OP - 001)
	// in a cleaned title or episode tag. The token must stand alone with a
	// number, so "Speed", "Hope", "Top", "Opened", "Operation" never match.
	themeRe = regexp.MustCompile(`(?i)(?:^|[\s._\-;\[\(])(?:NC)?(OP|ED)[\s._\-]*(\d{1,3})(?:v\d+)?(?:$|[\s._\-;\]\)])`)
	// ncThemeRe spots bare creditless markers (NCOP, NCED) with no number.
	// The NC prefix makes them unambiguous, so unlike bare OP/ED they need
	// no digits (e.g. "NCOP 1.1", whose dotted version defeats the tagger).
	ncThemeRe = regexp.MustCompile(`(?i)(?:^|[\s._\-;\[\(])(?:NC)(OP|ED)(?:$|[\s._\-;\]\)])`)
	// episodeRangeRe re-scans a cleaned title for "Episode 53+54" ranges.
	// titleclean splits those at the wrong "+", so the tag is bogus (E2)
	// while the true range stays in the title: take the FIRST number.
	episodeRangeRe = regexp.MustCompile(`(?i)(?:^|[\s._\-;:\[])(?:Episode|Ep)\.?\s*(\d{1,4})\s*\+\s*\d{1,4}`)
	// extraRe lists filename-level extras: KindUnknown, never fetched.
	// Phrase stems only (never a single common word): each alternative is a
	// multi-word bonus-feature phrase observed as a 3000 ms wasted iTunes
	// lookup in the live negative cache (gag/blooper reels, outtakes, tv
	// spots, extended scenes, behind-the-story, making-of, meet-the-cast,
	// visual-effects, in-search-of, how-to-survive, previz, production
	// design, time lapse video, secret auditions). Guarded by
	// TestParseExtrasNoRealTitleCollision: no real title in the same cache
	// may match.
	extraRe = regexp.MustCompile(`(?i)\b(deleted scenes?|extended\s+scenes?|trailers?|gallery|alternate endings?|interviews?|behind the scenes|behind\s+the\s+stor(?:y|ies)|featurettes?|music videos?|samples?|extras?|gag\s+reels?|blooper\s+reels?|outtakes?|tv\s+spots?|making\s+of|meet\s+the\s+cast|visual\s+effects|in\s+search\s+of|how\s+to\s+survive|previz|production\s+design|time[\s\-]*lapse\s+videos?|secret\s+auditions?)\b`)
	// lettersRe and yearOnlyRe detect titles with no usable show/film name:
	// bare numbers ("01") are unresolvable, a lone year ("2012") is a film.
	lettersRe  = regexp.MustCompile(`[A-Za-z]`)
	yearOnlyRe = regexp.MustCompile(`^(\d{4})$`)
	// movieMarkerRe spots a movie marker at the END of a cleaned title: an
	// optional "the", the word "Movie", and an optional trailing number (the
	// number titleclean's ALT_EP split may already have taken as a weak tag).
	// Deliberately narrow: the marker must be the title's last word, so a
	// film whose own title ends in "Movie" and carries nothing after the word
	// is never rewritten.
	movieMarkerRe = regexp.MustCompile(`(?i)^(.*?)[\s._\-]+(?:the[\s._\-]+)?movie(?:[\s._\-]+\d{1,3})?$`)
)

// ParseRaw strips the " - PotPlayer" suffix, runs titleclean.Clean, then Parse.
func ParseRaw(rawTitle string) Query {
	s := strings.TrimSuffix(rawTitle, potPlayerSuffix)
	c := titleclean.Clean(s)
	return Parse(c.Title, c.EpisodeTag, c.EpisodeName)
}

// Parse builds a Query from titleclean fields. EpisodeName is used only to
// compose a movie's subtitle back onto its base title; the title is otherwise
// re-scanned only for the "Episode N+M" ranges titleclean mis-splits.
//
// Classification follows the synthetic library's validated rules:
//   - OP/ED theme tokens are themes, never episodes (KindUnknown, so the
//     resolver never fetches for them); themes outrank every episode form.
//   - An episode signal must be contextual: SxxEyy, E##, Episode N, EP N, a
//     dash-number absolute of 2-3 digits, or a single-digit " - N" clearly
//     dash-separated. Bare sequel/version digits (Harbor Run 2, Trucks 3,
//     3.0+1.0), quality tokens (1080p, 5.1, 10bit, x265) and years never
//     count; 19xx/20xx is a year, never an episode.
//   - Ranges use the first number (S02E12-E13 -> 12, Episode 53+54 -> 53).
//   - Extras, Sample, or no usable title are KindUnknown.
func Parse(title, episodeTag, episodeName string) Query {
	t := strings.TrimSpace(title)
	year := 0
	if m := yearSuffixRe.FindStringSubmatch(t); m != nil {
		if y, err := strconv.Atoi(m[2]); err == nil && y >= 1900 && y <= 2100 {
			year = y
			t = strings.TrimSpace(m[1])
		}
	}
	// OP/ED theme outranks every episode form.
	if isTheme(t, strings.TrimSpace(episodeTag)) {
		return Query{Kind: KindUnknown, Title: t, Year: year}
	}
	q := Query{Kind: KindMovie, Title: t, Year: year}
	tag := strings.TrimSpace(episodeTag)
	strong := false
	if tag != "" {
		switch {
		case seasonEpisodeRe.MatchString(tag):
			m := seasonEpisodeRe.FindStringSubmatch(tag)
			q.Season = atoi(m[1])
			q.Episode = atoi(m[2])
			strong = true
		case episodeOnlyRe.MatchString(tag):
			// A 19xx/20xx "absolute" is a year, never an episode.
			if n := atoi(episodeOnlyRe.FindStringSubmatch(tag)[1]); !isYearNum(n) {
				q.Episode = n
				q.Absolute = true
			}
		default:
			// Unknown tag text (e.g. "Special") carries no signal.
		}
	}
	// "Episode 53+54" ranges: titleclean splits those at the wrong "+",
	// leaving a bogus weak tag (E2) while the true range stays in the title.
	// The FIRST number wins over any weak absolute tag, so this runs before
	// the tag result is accepted.
	if n, head := rangeEpisode(t); n > 0 && !strong {
		q.Season = 0
		q.Episode = n
		q.Absolute = true
		q.Kind = KindSeries
		if head != "" {
			q.Title = head
		}
		return q
	}
	// A movie marker outranks the weak absolute tag titleclean's ALT_EP split
	// left behind. For "[Anime Time] Dragon Ball Super Movie 01 - Broly.mkv"
	// that split yields title "Dragon Ball Super Movie", tag "E1" and name
	// "Broly"; the "01" is the marker's own number, never an episode, and
	// "Broly" is the 2018 film's subtitle. The marker is stripped and the
	// trailing subtitle is composed back onto the base ("Dragon Ball Super" +
	// ": " + "Broly"), which is what the movie sources are queried for.
	// The marker only counts when something follows it - its number or the
	// subtitle - so a title that merely ends in the word (e.g. "The Lego
	// Movie" with nothing after it) is left exactly as parsed. Extras keep
	// their skip, and a strong SxxEyy tag always wins.
	if !strong && !extraRe.MatchString(t) {
		if base, ok := movieMarker(t); ok && (q.Season > 0 || q.Episode > 0 || strings.TrimSpace(episodeName) != "") {
			if sub := strings.TrimSpace(episodeName); sub != "" {
				base = base + ": " + sub
			}
			return Query{Kind: KindMovie, Title: base, Year: year}
		}
	}
	if q.Episode > 0 || q.Season > 0 {
		q.Kind = KindSeries
		return q
	}
	if extraRe.MatchString(t) {
		return Query{Kind: KindUnknown, Title: t, Year: year}
	}
	if t == "" || !lettersRe.MatchString(t) {
		if m := yearOnlyRe.FindStringSubmatch(t); m != nil {
			if y, err := strconv.Atoi(m[1]); err == nil && isYearNum(y) {
				return Query{Kind: KindMovie, Title: t, Year: y}
			}
		}
		return Query{Kind: KindUnknown, Title: t, Year: year}
	}
	return q
}

// movieMarker returns the title base before a trailing movie marker
// ("Show Movie", "Show Movie 01", "Show the Movie 01" -> "Show"). ok=false
// when there is no marker or nothing usable would remain as the base.
func movieMarker(t string) (string, bool) {
	m := movieMarkerRe.FindStringSubmatch(t)
	if m == nil {
		return "", false
	}
	base := strings.TrimSpace(m[1])
	if base == "" || !lettersRe.MatchString(base) {
		return "", false
	}
	return base, true
}

// isTheme reports whether a cleaned title or episode tag is an OP/ED
// theme (numbered tokens like "OP 1"/"ED 01v2", or bare NCOP/NCED).
func isTheme(title, tag string) bool {
	return themeRe.MatchString(title) || themeRe.MatchString(tag) ||
		ncThemeRe.MatchString(title) || ncThemeRe.MatchString(tag)
}

// rangeEpisode re-scans a cleaned title for an "Episode N+M" range and
// returns its FIRST number plus the show head before it ("" when unusable).
// It returns 0 when there is no range or the number is year-like.
func rangeEpisode(t string) (int, string) {
	idx := episodeRangeRe.FindStringSubmatchIndex(t)
	if idx == nil {
		return 0, ""
	}
	n := atoi(t[idx[2]:idx[3]])
	if n <= 0 || isYearNum(n) {
		return 0, ""
	}
	return n, strings.TrimRight(strings.TrimSpace(t[:idx[0]]), " \t._-;:([")
}

// isYearNum reports whether n looks like a release year (hence never an
// episode number).
func isYearNum(n int) bool { return n >= 1900 && n <= 2100 }

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// dateYear parses the year from a date prefix like "2008-01-20" or
// "2010-07-16T07:00:00Z". It returns 0 when no sane year leads.
func dateYear(s string) int {
	s = strings.TrimSpace(s)
	if len(s) < 4 {
		return 0
	}
	y, err := strconv.Atoi(s[:4])
	if err != nil || y < 1900 || y > 2100 {
		return 0
	}
	return y
}
