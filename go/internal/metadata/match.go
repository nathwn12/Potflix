package metadata

import (
	"strings"
	"unicode"
)

// ShowCand is one candidate show for title matching. Weight carries the
// TVmaze show weight (higher ranks more popular/authoritative); EpisodeCount
// breaks remaining ties toward the fuller catalog entry.
type ShowCand struct {
	Name         string
	Weight       int
	EpisodeCount int
}

// Tuning for pickBestShow: candidates need at least this similarity to
// answer at all, and candidates within matchTieMargin of the top similarity
// form a tie group broken by Weight then EpisodeCount.
const (
	matchMinSimilarity = 0.5
	matchTieMargin     = 0.1
)

// foldedAccent maps a lowercased accented/special Latin rune to ASCII.
// ok=false means the rune is not a special case.
func foldedAccent(r rune) (rep string, ok bool) {
	switch r {
	case 'à', 'á', 'â', 'ã', 'ä', 'å', 'ā', 'ă', 'ą', 'ǎ', 'ȁ', 'ȃ':
		return "a", true
	case 'æ':
		return "ae", true
	case 'ç', 'ć', 'ĉ', 'ċ', 'č':
		return "c", true
	case 'ď':
		return "d", true
	case 'ð':
		return "d", true
	case 'è', 'é', 'ê', 'ë', 'ē', 'ĕ', 'ė', 'ę', 'ě', 'ȅ', 'ȇ':
		return "e", true
	case 'ĝ', 'ğ', 'ġ', 'ģ':
		return "g", true
	case 'ĥ':
		return "h", true
	case 'ì', 'í', 'î', 'ï', 'ĩ', 'ī', 'ĭ', 'į', 'ǐ', 'ȉ', 'ȋ', 'ı':
		return "i", true
	case 'ĵ':
		return "j", true
	case 'ķ':
		return "k", true
	case 'ĺ', 'ļ', 'ľ', 'ŀ', 'ł':
		return "l", true
	case 'ñ', 'ń', 'ņ', 'ň', 'ŉ':
		return "n", true
	case 'ò', 'ó', 'ô', 'õ', 'ö', 'ø', 'ō', 'ŏ', 'ő', 'ǒ', 'ȍ', 'ȏ':
		return "o", true
	case 'œ':
		return "oe", true
	case 'ŕ', 'ŗ', 'ř':
		return "r", true
	case 'ś', 'ŝ', 'ş', 'š', 'ș', 'ß':
		if r == 'ß' {
			return "ss", true
		}
		return "s", true
	case 'ţ', 'ť', 'ŧ', 'ț':
		return "t", true
	case 'þ':
		return "th", true
	case 'ù', 'ú', 'û', 'ü', 'ũ', 'ū', 'ŭ', 'ů', 'ű', 'ų', 'ǔ', 'ȕ', 'ȗ':
		return "u", true
	case 'ŵ':
		return "w", true
	case 'ý', 'ÿ', 'ŷ':
		return "y", true
	case 'ź', 'ż', 'ž':
		return "z", true
	}
	return "", false
}

// normalizeTitle lowercases, folds punctuation to spaces and diacritics to
// ASCII, and collapses whitespace, so "Re:ZERO", "Mr. Robot" and "Café"
// compare as "re zero", "mr robot" and "cafe".
func normalizeTitle(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		if rep, ok := foldedAccent(r); ok {
			b.WriteString(rep)
			continue
		}
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
			continue
		}
		b.WriteByte(' ')
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// titleSimilarity scores query against candidate in [0,1]. Exact normalized
// equality is 1; otherwise 0.7*coverage (fraction of query tokens found in
// the candidate) + 0.3*Jaccard, where the Jaccard term supplies the length
// penalty for much-longer candidates. No query token in the candidate is 0.
func titleSimilarity(query, candidate string) float64 {
	nq := normalizeTitle(query)
	nc := normalizeTitle(candidate)
	if nq == nc {
		return 1
	}
	if nq == "" || nc == "" {
		return 0
	}
	qs := strings.Fields(nq)
	cs := strings.Fields(nc)
	qset := make(map[string]struct{}, len(qs))
	for _, t := range qs {
		qset[t] = struct{}{}
	}
	cset := make(map[string]struct{}, len(cs))
	for _, t := range cs {
		cset[t] = struct{}{}
	}
	inter := 0
	for t := range qset {
		if _, ok := cset[t]; ok {
			inter++
		}
	}
	if inter == 0 {
		return 0
	}
	union := len(qset) + len(cset) - inter
	sim := 0.7*float64(inter)/float64(len(qset)) + 0.3*float64(inter)/float64(union)
	if sim < 0 {
		return 0
	}
	if sim > 1 {
		return 1
	}
	return sim
}

// pickBestShow picks the candidate best matching query. The top-similarity
// candidate wins outright unless others fall within matchTieMargin of it, in
// which case the tie group breaks by higher Weight then higher EpisodeCount
// (TVmaze's own ranking put the "Re:Zero kara Hajimeru Break Time" spinoff
// above the main show, so near-ties must defer to weight). Exact normalized
// matches always outrank near matches. ok=false when empty or when the best
// similarity is below matchMinSimilarity, so the resolver falls through.
func pickBestShow(query string, cands []ShowCand) (best ShowCand, ok bool) {
	if len(cands) == 0 {
		return ShowCand{}, false
	}
	sims := make([]float64, len(cands))
	top := -1.0
	for i, c := range cands {
		sims[i] = titleSimilarity(query, c.Name)
		if sims[i] > top {
			top = sims[i]
		}
	}
	if top < matchMinSimilarity {
		return ShowCand{}, false
	}
	hasExact := false
	for _, s := range sims {
		if s == 1 {
			hasExact = true
			break
		}
	}
	idx := -1
	for i, c := range cands {
		if hasExact {
			if sims[i] != 1 {
				continue
			}
		} else if sims[i] < top-matchTieMargin {
			continue
		}
		if idx < 0 {
			idx = i
			continue
		}
		b := cands[idx]
		switch {
		case c.Weight != b.Weight:
			if c.Weight > b.Weight {
				idx = i
			}
		case c.EpisodeCount != b.EpisodeCount:
			if c.EpisodeCount > b.EpisodeCount {
				idx = i
			}
		case sims[i] > sims[idx]:
			idx = i
		}
	}
	if idx < 0 {
		return ShowCand{}, false
	}
	return cands[idx], true
}
