package titleclean

import (
	"regexp"
	"strconv"
)

// Cleaned mirrors me.potflix.rpc.Main.Cleaned.
type Cleaned struct {
	Title       string
	Display     string
	EpisodeTag  string
	EpisodeName string
}

// Java's \s is [ \t\n\x0B\f\r], wider than RE2's (which omits \x0B).
const jws = `[ \t\n\x0B\f\r]`

// tagsAlt is the TAGS alternation with the `\b` assertions removed; those are
// applied in replaceTags using Java's Unicode-aware boundary rule.
const tagsAlt = `2160p|1080p|720p|480p|360p|4k|uhd|hdr\d*\+?|dolby[ .\-]?vision|blu[ .\-]?ray|bdrip|brrip|web[ .\-]?dl|webrip|web[ .\-]?hd|hdtv|dvdrip|dvdscr|camrip|hdts|hdcam|hdrip|h[ .\-]?26[45]|x26[45]|xvid|hevc|av1|vp9|10bit|8bit|aac\d?(?:[ .\-]?\d)?|ac3|eac3|dts(?:[ .\-]?hd)?|ddp?a?[ .\-]?\d(?:[ .\-]?\d)?|truehd|atmos|flac|mp3|opus|dual(?:[ .\-]?audio)?|multi[ .\-]?subs?|multi\d*|msubs|subbed|dubbed|extended[ .\-]?cut|unrated|theatrical[ .\-]?cut|directors?[ .\-]?cut|repack|proper|nf|amzn|dsnp|atvp|hmax|imax|sdr|hlg|vmaf\d*|thai|tamil|hindi|kor|jpn|eng|rarbg|rartv|ettv|yts|yify|mkvcage|psa|evo|lol|ion10|vostfr|vost|5[ .\-]?1|7[ .\-]?1`

var (
	// EXT: trailing media extension, case-insensitive.
	extRe = regexp.MustCompile(portPattern(`\.(mp4|mkv|avi|webm|ts|m2ts|mov|wmv|flv|mpg|mpeg|m4v|ass|srt)$`, true))

	// clean() housekeeping.
	groupPrefixRe = regexp.MustCompile(portPattern(`^`+jws+`*\[[^\]]+\]`+jws+`*`, false))
	crcRe         = regexp.MustCompile(portPattern(jws+`*\[[A-Fa-f0-9]{8}\]`+jws+`*$`, false))
	parenRe       = regexp.MustCompile(portPattern(`\([^)]*\)`, false))
	wsRunRe       = regexp.MustCompile(portPattern(jws+`+`, false))
	trailSepRe    = regexp.MustCompile(portPattern(`[. _\-]+$`, false))
	sceneGroupRe  = regexp.MustCompile(portPattern(`[-_.][A-Za-z0-9]{2,12}$`, false))

	// Episode matcher chain (order matters, exactly as Java).
	multiRe       = regexp.MustCompile(portPattern(`(?i)^(.*?)[. _\-]*[Ss](\d{1,4})[ . _\-]*[Ee](\d{1,3})[ ._\-]*[\-\x{2013}\x{2014}]?[Ee](\d{1,3})(?:[. _\-]+(.*))?$`, true))
	seriesRe      = regexp.MustCompile(portPattern(`(?i)^(.*?)[. _\-]*[Ss](\d{1,4})[ . _\-]*[Ee](\d{1,3})(?:[. _\-]+(.*))?$`, true))
	seasonEpRe    = regexp.MustCompile(portPattern(`(?i)^(.*?)[. _\-]*Season[ . _\-]*(\d{1,2})[. _\-]+(?:Episode|Ep)\.?[ . _\-]*(\d{1,4})(?:[. _\-]+(.*))?$`, true))
	chapterRe     = regexp.MustCompile(portPattern(`(?i)^(.+?)[. _\-]+Chapter[ . _\-]*(\d{1,4})(?:[. _\-]+(.*))?$`, true))
	seasonExtraRe = regexp.MustCompile(portPattern(`(?i)^(.*?)[. _\-]*Season[ . _\-]*(\d{1,2})[. _\-]+(.+)$`, true))
	tnRe          = regexp.MustCompile(portPattern(`^(\d{1,3})(?:[. _\-]+(.*))?$`, false))
	epwordRe      = regexp.MustCompile(portPattern(`(?i)^(.*?)[. _\-]*(?:Episode|Ep)\.?[ . _\-]*(\d{1,4})(?:[. _\-]+(.*))?$`, true))
	xepRe         = regexp.MustCompile(portPattern(`(?i)^(.*?)[. _\-]+(\d{1,2})x(\d{2,3})(?:[. _\-]+(.*))?$`, true))
	trailRe       = regexp.MustCompile(portPattern(`(?i)^(.{3,}?)[. _\-]+(0\d{1,2}|\d{3})$`, true))
	altEpRe       = regexp.MustCompile(portPattern(`(?i)^(.+?)[. _\-]+(?:EP?|#)?[ . _\-]*(\d{1,3})(?:[. _\-]+(.*))?$`, true))
	fansubRe      = regexp.MustCompile(portPattern(`(?i)^(.+?)`+jws+`*[-;]`+jws+`*(?:Ep?\.?`+jws+`*)?(\d{1,4})(?:`+jws+`*[-;]`+jws+`*(.*))?$`, true))
	ncbookRe      = regexp.MustCompile(portPattern(`(?i)^(?:(.*?)[. _\-]+)?(?:NC)?(OP|ED)[ . _\-]*(\d{1,3})(?:`+jws+`*[-;]`+jws+`*(.*))?$`, true))

	// ALT_EP guard patterns.
	altTailRe = regexp.MustCompile(portPattern(`(?i)^.*[. _\-](EP?|#)?[ . _\-]*\d{1,3}([. _\-].*)?$`, true))

	// NCBOOK head season-suffix strip.
	ncHeadSepRe = regexp.MustCompile(portPattern(`(?i)[. _\-]+S\d{1,2}$`, true))

	// epName cleanup.
	epNameTrailRe  = regexp.MustCompile(portPattern(jws+`*-`+jws+`*[A-Z0-9]{2,}$`, false))
	epNameFilterRe = regexp.MustCompile(portPattern(`(?i)^(?:WEB|DS4K|OVA|v\d+|\d+|\+)$`, true))

	// stripTags().
	dotUnderscoreRe = regexp.MustCompile(portPattern(`[._]+`, false))
	bracketRe       = regexp.MustCompile(portPattern(`\[.*?\]`, false))
	parenStripRe    = regexp.MustCompile(portPattern(`\(.*?\)`, false))
	leadingSepRe    = regexp.MustCompile(portPattern(`^[\-_. ]+`, false))
	trailingSepRe   = regexp.MustCompile(portPattern(`[\-_. ]+$`, false))
	yearRe          = regexp.MustCompile(portPattern(`^(.*)[. _\-]+(19\d{2}|20\d{2})$`, false))

	// TAGS core (without \b).
	tagsCoreRe = regexp.MustCompile(portPattern(`(?:`+tagsAlt+`)`, true))
	// TAGS used for full-match tests during boundary backtracking.
	tagsFullRe = regexp.MustCompile(portPattern(`^(?:`+tagsAlt+`)$`, true))

	// SCENE_HINT: presence of codec/source marker.
	sceneHintRe = regexp.MustCompile(portPattern(`(?i)(1080p|2160p|720p|4k|uhd|x26[45]|web[ .\-]?dl|bluray|bdrip|hdtv|dvdrip|hevc|av1|opus)`, true))
)

// replaceTags applies Java's TAGS.replaceAll(" "), where the `\b` assertions
// use Java's Unicode-aware word rule instead of RE2's ASCII one. Java's
// backtracking is emulated: when the greedy end fails the right boundary, the
// longest shorter end that still fully matches the alternation on a boundary
// is used (e.g. "HDR10+" matches "HDR10", leaving "+").
func replaceTags(r string) string {
	runes := []rune(r)
	var b []byte
	last := 0
	for i := 0; i <= len(r); {
		loc := tagsCoreRe.FindStringIndex(r[i:])
		if loc == nil {
			break
		}
		s0, eMax := i+loc[0], i+loc[1]
		if !javaBoundary(runes, len([]rune(r[:s0]))) {
			i = nextRuneIndex(r, s0)
			continue
		}
		matched := false
		for e := eMax; e > s0; e = prevRuneIndex(r, e) {
			if !tagsFullRe.MatchString(r[s0:e]) {
				continue
			}
			if javaBoundary(runes, len([]rune(r[:e]))) {
				b = append(b, r[last:s0]...)
				b = append(b, ' ')
				last = e
				i = e
				matched = true
				break
			}
		}
		if !matched {
			i = nextRuneIndex(r, s0)
		}
	}
	b = append(b, r[last:]...)
	return string(b)
}

// hasYear mirrors Java's `s.matches("(?i).*\b(19|20)\d{2}\b.*")`.
func hasYear(s string) bool {
	if containsLineTerm(s) {
		return false
	}
	r := []rune(s)
	for i := 0; i+4 <= len(r); i++ {
		if !((r[i] == '1' && r[i+1] == '9') || (r[i] == '2' && r[i+1] == '0')) {
			continue
		}
		if !isASCIIDigit(r[i+2]) || !isASCIIDigit(r[i+3]) {
			continue
		}
		if javaBoundary(r, i) && javaBoundary(r, i+4) {
			return true
		}
	}
	return false
}

// altHeadBad mirrors `s.matches("(?i).*\b(Season|Chapter|Part|Episode|Ep|Vol(ume)?)$")`.
func altHeadBad(s string) bool {
	if containsLineTerm(s) {
		return false
	}
	runes := []rune(s)
	lower := asciiLower(s)
	for _, w := range []string{"season", "chapter", "part", "episode", "ep", "volume", "vol"} {
		if len(lower) >= len(w) && lower[len(lower)-len(w):] == w {
			if javaBoundary(runes, len(runes)-len(w)) {
				return true
			}
		}
	}
	return false
}

// yearLead mirrors `s.matches("^(19|20)\d{2}\b.*")`.
func yearLead(s string) bool {
	if containsLineTerm(s) {
		return false
	}
	r := []rune(s)
	if len(r) < 4 {
		return false
	}
	if !((r[0] == '1' && r[1] == '9') || (r[0] == '2' && r[1] == '0')) {
		return false
	}
	if !isASCIIDigit(r[2]) || !isASCIIDigit(r[3]) {
		return false
	}
	return javaBoundary(r, 4)
}

// clean is a faithful port of me.potflix.rpc.Main.clean(String).
func clean(raw string) Cleaned {
	var out Cleaned
	s := javaDollarReplaceAll(extRe, javaTrim(raw), "")
	s = groupPrefixRe.ReplaceAllString(s, "") // [Group] prefix
	s = javaDollarReplaceAll(crcRe, s, "")    // [CRC32] suffix
	yr := hasYear(s)

	ds := parenRe.ReplaceAllString(s, " ")
	ds = wsRunRe.ReplaceAllString(ds, " ")
	ds = javaTrim(ds)
	ds = javaDollarReplaceAll(trailSepRe, ds, "")

	if gs, _, ok := subgroups(ncbookRe, ds); ok {
		var nc Cleaned
		nhead := javaDollarReplaceAll(ncHeadSepRe, gs[1], "")
		op := gs[2]
		if len(op) >= 2 && (op[0] == 'O' || op[0] == 'o') && (op[1] == 'P' || op[1] == 'p') {
			nc.EpisodeTag = "OP " + strconv.Itoa(atoi(gs[3]))
		} else {
			nc.EpisodeTag = "ED " + strconv.Itoa(atoi(gs[3]))
		}
		nc.EpisodeName = stripTags(gs[4])
		ntitle := stripTags(nhead)
		if ntitle == "" {
			if nc.EpisodeName != "" {
				ntitle = nc.EpisodeName
				nc.EpisodeName = ""
			} else {
				// P12 deliberate deviation: a headless tag with no name
				// shows the tag alone; frozen Main yields "ED 3 - ED 3".
				nc.Title = javaTrunc(nc.EpisodeTag, 100)
				nc.Display = nc.Title
				return nc
			}
		}
		nc.Title = javaTrunc(ntitle, 100)
		nc.Display = buildDisplay(nc.Title, nc.EpisodeTag, nc.EpisodeName)
		return nc
	}

	if sceneHintRe.MatchString(s) {
		s = javaDollarReplaceAll(sceneGroupRe, s, "")
	}

	head := ""
	epTag := ""
	rest := ""
	got := false

	if gs, _, ok := subgroups(multiRe, s); ok {
		head = gs[1]
		epTag = "S" + strconv.Itoa(atoi(gs[2])) + " E" + strconv.Itoa(atoi(gs[3])) + "-" + strconv.Itoa(atoi(gs[4]))
		rest = gs[5]
		got = true
	}
	if !got {
		if gs, _, ok := subgroups(seasonEpRe, s); ok {
			head = gs[1]
			epTag = "S" + strconv.Itoa(atoi(gs[2])) + " E" + strconv.Itoa(atoi(gs[3]))
			rest = gs[4]
			got = true
		}
	}
	if !got && !yr {
		if gs, _, ok := subgroups(chapterRe, s); ok {
			head = gs[1]
			epTag = "E" + strconv.Itoa(atoi(gs[2]))
			rest = gs[3]
			got = true
		}
	}
	if !got {
		if gs, _, ok := subgroups(seasonExtraRe, s); ok {
			if tn, _, ok2 := subgroups(tnRe, gs[3]); ok2 {
				head = gs[1]
				epTag = "S" + strconv.Itoa(atoi(gs[2])) + " E" + strconv.Itoa(atoi(tn[1]))
				rest = tn[2]
				got = true
			} else {
				ttl := javaTrim(stripTags(gs[1]) + " " + stripTags(gs[3]))
				if ttl == "" {
					out.Title = javaTrunc(stripTags(s), 100)
				} else {
					out.Title = javaTrunc(ttl, 100)
				}
				out.Display = out.Title
				return out
			}
		}
	}
	if !got {
		if gs, _, ok := subgroups(seriesRe, s); ok {
			head = gs[1]
			epTag = "S" + strconv.Itoa(atoi(gs[2])) + " E" + strconv.Itoa(atoi(gs[3]))
			rest = gs[4]
			got = true
		}
	}
	if !got {
		if gs, _, ok := subgroups(fansubRe, s); ok {
			fnum := atoi(gs[2])
			yearLike := javaLength(gs[2]) == 4 && fnum >= 1900 && fnum <= 2100
			if !yearLike {
				head = gs[1]
				epTag = "E" + strconv.Itoa(fnum)
				rest = gs[3]
				got = true
			}
		}
	}
	if !got {
		if gs, _, ok := subgroups(epwordRe, s); ok {
			head = gs[1]
			epTag = "E" + strconv.Itoa(atoi(gs[2]))
			rest = gs[3]
			got = true
		}
	}
	if !got {
		if gs, _, ok := subgroups(xepRe, s); ok {
			head = gs[1]
			epTag = "S" + strconv.Itoa(atoi(gs[2])) + " E" + strconv.Itoa(atoi(gs[3]))
			rest = gs[4]
			got = true
		}
	}
	if !got && !yr {
		if gs, _, ok := subgroups(trailRe, s); ok {
			head = gs[1]
			epTag = "E" + strconv.Itoa(atoi(gs[2]))
			rest = ""
			got = true
		}
	}
	if !got {
		gs, pr, ok := subgroups(altEpRe, s)
		if ok && javaLength(gs[1]) >= 3 && javaLength(gs[2]) <= 3 && pr[3] && javaLength(gs[3]) < 40 &&
			altTailRe.MatchString(s) && javaLength(stripTags(s)) > javaLength(gs[1]) &&
			!altHeadBad(gs[1]) && !yearLead(gs[3]) {
			head = gs[1]
			epTag = "E" + strconv.Itoa(atoi(gs[2]))
			rest = gs[3]
			got = true
		}
	}

	var core string
	if got {
		core = stripTags(head)
	} else {
		core = stripTags(s)
	}
	epName := ""
	if got {
		epName = stripTags(rest)
	}
	epName = javaDollarReplaceAll(epNameTrailRe, epName, "")
	epName = javaTrim(epName)
	if epNameFilterRe.MatchString(epName) {
		epName = ""
	}
	if got && core == "" {
		out.EpisodeTag = epTag
		if epName != "" {
			out.Title = javaTrunc(epName, 100)
			out.EpisodeName = ""
			out.Display = out.Title + " \u2014 " + epTag
			return out
		}
		out.Title = epTag
		out.EpisodeName = ""
		out.Display = epTag
		return out
	}
	if core == "" {
		core = stripTags(s)
	}
	out.Title = javaTrunc(core, 100)
	out.EpisodeTag = epTag
	out.EpisodeName = epName
	out.Display = buildDisplay(out.Title, out.EpisodeTag, out.EpisodeName)
	return out
}

// stripTags is a faithful port of me.potflix.rpc.Main.stripTags(String).
func stripTags(s string) string {
	r := dotUnderscoreRe.ReplaceAllString(s, " ")
	r = bracketRe.ReplaceAllString(r, " ")
	r = parenStripRe.ReplaceAllString(r, " ")
	r = replaceTags(r)
	r = wsRunRe.ReplaceAllString(r, " ")
	r = javaTrim(r)
	r = leadingSepRe.ReplaceAllString(r, "")
	r = javaDollarReplaceAll(trailingSepRe, r, "")

	year := ""
	if gs, _, ok := subgroups(yearRe, r); ok {
		r = javaTrim(gs[1])
		year = " (" + gs[2] + ")"
	}
	r = javaUpperFirst(r)
	return javaTrim(r + year)
}

// buildDisplay mirrors me.potflix.rpc.Main.buildDisplay.
func buildDisplay(title, tag, name string) string {
	if tag == "" {
		return title
	}
	if title == "" {
		return tag
	}
	if name == "" {
		return title + " \u2014 " + tag
	}
	return title + " \u2014 " + tag + " \"" + javaTrunc(name, 60) + "\""
}

// javaTrim mirrors java.lang.String.trim(): strip chars <= U+0020.
func javaTrim(s string) string {
	start, end := 0, len(s)
	for start < end && s[start] <= 0x20 {
		start++
	}
	for end > start && s[end-1] <= 0x20 {
		end--
	}
	return s[start:end]
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// subgroups returns capture groups plus presence flags (Java null semantics for non-participating groups).
func subgroups(re *regexp.Regexp, s string) ([]string, []bool, bool) {
	idx := re.FindStringSubmatchIndex(s)
	if idx == nil {
		return nil, nil, false
	}
	n := re.NumSubexp()
	gs := make([]string, n+1)
	pr := make([]bool, n+1)
	for i := 0; i <= n; i++ {
		a, b := idx[2*i], idx[2*i+1]
		if a < 0 {
			gs[i], pr[i] = "", false
		} else {
			gs[i], pr[i] = s[a:b], true
		}
	}
	return gs, pr, true
}
