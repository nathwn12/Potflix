package repair

import (
	"os"
	"os/user"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SanitizeError is the published-text filter: it flattens whitespace, collapses
// every absolute local path to its basename, redacts the machine's identity
// terms (account name, home directory) and any id-shaped digit run, and keeps
// everything else unchanged.
//
// It is the single entry point for any string that may reach a GitHub issue, so
// a provider error such as
//
//	open D:\1movies\Some Folder\Some File.mkv: Access is denied
//
// publishes as
//
//	open Some File.mkv: Access is denied
func SanitizeError(s string) string {
	if s == "" {
		return ""
	}
	s = oneLine(s)
	s = collapsePaths(s)
	s = redactTerms(s, identityTermSource())
	return redactIDShapedDigits(s)
}

// identityTermSource returns the strings that must never be published. It is a
// test seam: tests pin the terms so an assertion never depends on the account
// name of the machine running them (a username that happens to collide with a
// word in a fixture would otherwise change the expected output).
var identityTermSource = identityTerms

// identityTerms is the machine identity: the Windows account name, the Unix
// user, and the home directory.
func identityTerms() []string {
	var out []string
	add := func(v string) {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	add(os.Getenv("USERNAME"))
	add(os.Getenv("USER"))
	add(os.Getenv("USERPROFILE"))
	add(os.Getenv("HOME"))
	if u, err := user.Current(); err == nil {
		add(u.Username)
		add(u.HomeDir)
		// A domain-qualified account (DOMAIN\user) is stripped to the account.
		if i := strings.LastIndexAny(u.Username, `\/`); i >= 0 {
			add(u.Username[i+1:])
		}
	}
	// Longest first: the home directory before the bare account name, so
	// "C:\Users\tester" is replaced as a whole.
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// minRedactTerm is the shortest identity term worth redacting; below it a term
// is too likely to appear inside unrelated words.
const minRedactTerm = 3

// redactTerms replaces each term, case-insensitively, with <redacted>.
func redactTerms(s string, terms []string) string {
	for _, term := range terms {
		if utf8.RuneCountInString(term) < minRedactTerm {
			continue
		}
		re, err := regexp.Compile(`(?i)` + regexp.QuoteMeta(term))
		if err != nil {
			continue
		}
		s = re.ReplaceAllString(s, "<redacted>")
	}
	return s
}

// idShaped matches a Discord snowflake: 17-20 digits standing alone. The client
// id must never be published; this is the belt to the structural braces (the
// report simply has no field that carries it). A bare unix-nanosecond
// timestamp is the same shape and is redacted with it - harmless inside error
// text, and cheaper than a leak.
var idShaped = regexp.MustCompile(`\b\d{17,20}\b`)

func redactIDShapedDigits(s string) string {
	return idShaped.ReplaceAllString(s, "<id>")
}

// collapsePaths rewrites every absolute local path inside s to its basename.
// Recognised starts: a drive-letter path (C:\ or C:/), a UNC or extended-length
// path (\\server\share, \\?\C:\), and a POSIX absolute path. A start is only
// honoured at a word boundary, which keeps URLs such as https://host/a/b
// untouched.
func collapsePaths(s string) string {
	if s == "" || (!strings.ContainsAny(s, `\/`) && !strings.Contains(s, ":")) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		span, ok := pathSpan(s, i)
		if !ok {
			b.WriteByte(s[i])
			i++
			continue
		}
		b.WriteString(basenameOf(span))
		i += len(span)
	}
	return b.String()
}

// pathSpan returns the path starting at i, if one starts there.
func pathSpan(s string, i int) (string, bool) {
	if !pathStart(s, i) {
		return "", false
	}
	end := consumePath(s, i)
	if end <= i {
		return "", false
	}
	return s[i:end], true
}

// pathStart reports whether a path begins at byte i.
func pathStart(s string, i int) bool {
	if i >= len(s) {
		return false
	}
	if i > 0 && isPathByte(s[i-1]) {
		return false // mid-word: not the start of a path
	}
	switch {
	case s[i] == '\\' && i+1 < len(s) && s[i+1] == '\\':
		return true // \\server\share or \\?\C:\
	case s[i] == '/' && i+1 < len(s) && !isSep(s[i+1]) && isPathByte(s[i+1]):
		return true // /home/user/x, but never the // of a URL
	case s[i] == '\\' && i+1 < len(s) && !isSep(s[i+1]) && isPathByte(s[i+1]):
		return true // \Users\tester\x.mkv - rooted without a drive
	case i+2 < len(s) && isDriveLetter(s[i]) && s[i+1] == ':' && isSep(s[i+2]):
		return true // C:\ or C:/
	}
	return false
}

// consumePath returns the end byte of the path starting at i. The first word is
// always taken; a following space only continues the path when the next word
// still looks like a path part (it carries a separator or a dot). That captures
// "D:\My Movies\Some File.mkv" and still stops at "while reading" in
// "open D:\a\b.mkv while reading".
func consumePath(s string, i int) int {
	j := i
	switch {
	case s[j] == '\\' && j+1 < len(s) && s[j+1] == '\\':
		j += 2
		if j+1 < len(s) && s[j] == '?' && isSep(s[j+1]) {
			j += 2 // \\?\
		}
		if j+2 < len(s) && isDriveLetter(s[j]) && s[j+1] == ':' && isSep(s[j+2]) {
			j += 3 // \\?\C:\ - the drive would otherwise be left behind
		}
	case isSep(s[j]):
		j++ // a POSIX root, or the single leading \ of \Users\...
	case isDriveLetter(s[j]) && j+2 < len(s) && s[j+1] == ':' && isSep(s[j+2]):
		j += 3
	}
	for first := true; ; {
		wordStart := j
		j = consumeWord(s, j)
		if j == wordStart {
			// "C:\" with nothing behind it, or a stray separator.
			if first && j > i {
				return j
			}
			break
		}
		first = false
		k := j
		for k < len(s) && s[k] == ' ' {
			k++
		}
		if k == j || k >= len(s) || !looksLikePathWord(s, k) {
			break
		}
		j = k
	}
	return j
}

// consumeWord returns the end of the run of path bytes starting at i.
func consumeWord(s string, i int) int {
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			return i // invalid encoding: never swallow it
		}
		if !isPathRune(r) {
			return i
		}
		i += size
	}
	return i
}

// looksLikePathWord reports whether the word at k reads as a further path part
// rather than the prose that follows one.
func looksLikePathWord(s string, k int) bool {
	w := s[k:consumeWord(s, k)]
	if w == "" {
		return false
	}
	if strings.ContainsAny(w, `\/`) {
		return true
	}
	return strings.Contains(w, ".") && !strings.HasPrefix(w, ".")
}

// basenameOf reduces p to its last element, dropping separators, any drive or
// UNC head and trailing punctuation, so "D:\a\b.mkv" is "b.mkv" and
// "(see D:\a\b.mkv)" does not publish "b.mkv)".
func basenameOf(p string) string {
	p = strings.TrimRight(p, `\/`)
	p = strings.TrimPrefix(p, `\\?\`)
	p = strings.TrimPrefix(p, `\\`)
	if len(p) >= 2 && isDriveLetter(p[0]) && p[1] == ':' {
		p = strings.TrimLeft(p[2:], `\/`)
	}
	if i := strings.LastIndexAny(p, `\/`); i >= 0 {
		p = p[i+1:]
	}
	p = strings.TrimRight(p, `.,;:)]}'"!?`)
	if p == "" {
		return "<path>" // a bare root, e.g. "C:\" or "\\server"
	}
	return p
}

// isSep reports whether b is a path separator.
func isSep(b byte) bool { return b == '\\' || b == '/' }

func isDriveLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// isPathByte reports whether b may sit inside a path word. Separators are
// included, so "1movies\Some" is one word.
func isPathByte(b byte) bool {
	if b >= utf8.RuneSelf {
		return true
	}
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	}
	return strings.IndexByte(`._-~+()[]\/`, b) >= 0
}

// isPathRune is isPathByte for a decoded rune; non-ASCII letters and digits are
// path bytes so non-Latin file names survive.
func isPathRune(r rune) bool {
	if r < utf8.RuneSelf {
		return isPathByte(byte(r))
	}
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// truncateBytes shortens s to at most n bytes, never splitting a rune, and
// marks the cut with "...".
func truncateBytes(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	suffix := ""
	cut := n
	if n > 3 {
		suffix = "..."
		cut = n - 3
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}
