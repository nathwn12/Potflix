package metadata

import (
	"strings"
	"unicode"
)

// maxSynopsisRunes caps a synopsis at 128 runes (Discord's asset-text cap, so
// the LargeText hover tooltip carries the short synopsis whole; details/state
// must be snippets, not sentences, staying on one line; Netflix timed-text:
// 42 chars/line, max 2 lines, break after punctuation, single U+2026);
// capSynopsis backs off to a word/sentence boundary so the card never shows
// a cut-off word.
const maxSynopsisRunes = 128

// firstNonEmpty returns the first string that is non-empty after trimming
// whitespace, or "" when all are blank.
func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if t := strings.TrimSpace(s); t != "" {
			return t
		}
	}
	return ""
}

// htmlEntities decodes the entities providers emit in summaries.
var htmlEntities = strings.NewReplacer(
	"&nbsp;", " ",
	"&amp;", "&",
	"&quot;", `"`,
	"&#39;", "'",
	"&lt;", "<",
	"&gt;", ">",
)

// stripHTML drops <tags> (each replaced with one space so words on either
// side never join), decodes common entities, collapses all whitespace runs
// (including newlines) to single spaces, and trims. Decoding runs after tag
// stripping so literal "&lt;p&gt;" text survives as "<p>".
func stripHTML(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != '<' {
			b.WriteByte(s[i])
			i++
			continue
		}
		// '<': skip through the next '>' when one exists, else keep it
		// literally (a bare "a < b" comparison, not a tag).
		end := strings.IndexByte(s[i:], '>')
		if end < 0 {
			b.WriteByte(s[i])
			i++
			continue
		}
		b.WriteByte(' ')
		i += end + 1
	}
	return strings.Join(strings.Fields(htmlEntities.Replace(b.String())), " ")
}

// capSynopsis trims s to ~maxSynopsisRunes runes, preferring to end at the
// last sentence boundary (. ! ? followed by space/end) in the back half,
// else at the last word boundary past the halfway mark, else hard at the
// cap. Input at or under the cap is returned trimmed as-is.
func capSynopsis(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= maxSynopsisRunes {
		return s
	}
	cut := r[:maxSynopsisRunes]
	for i := len(cut) - 1; i >= maxSynopsisRunes/2; i-- {
		if c := cut[i]; c == '.' || c == '!' || c == '?' {
			if i+1 == len(cut) || unicode.IsSpace(cut[i+1]) {
				return strings.TrimSpace(string(cut[:i+1]))
			}
		}
	}
	for i := len(cut) - 1; i >= maxSynopsisRunes/2; i-- {
		if unicode.IsSpace(cut[i]) {
			return strings.TrimSpace(string(cut[:i]))
		}
	}
	return strings.TrimSpace(string(cut))
}
