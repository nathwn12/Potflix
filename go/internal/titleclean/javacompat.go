package titleclean

import (
	"regexp"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// anyCh is Java's default "." : any code point except a line terminator.
// Java line terminators are \n, \r, U+0085, U+2028, U+2029.
const anyCh = `[^\n\r\x{0085}\x{2028}\x{2029}]`

// portPattern turns a Java-flavoured pattern into an RE2 pattern with Java's
// semantics: `^`/`$` become `\A`/`\z`, wildcard `.` excludes Java line
// terminators, and (when folded) ASCII letters are expanded to explicit
// two-case classes because Java CASE_INSENSITIVE is ASCII-only while RE2's
// (?i) folds Unicode.
func portPattern(p string, folded bool) string {
	if folded {
		p = strings.TrimPrefix(p, "(?i)")
	}
	var b strings.Builder
	inClass := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '\\' && i+1 < len(p) {
			b.WriteByte(c)
			b.WriteByte(p[i+1])
			i++
			continue
		}
		if inClass {
			b.WriteByte(c)
			if c == ']' {
				inClass = false
			}
			continue
		}
		switch {
		case c == '[':
			inClass = true
			b.WriteByte(c)
		case c == '.':
			b.WriteString(anyCh)
		case c == '$':
			b.WriteString(`\z`)
		case c == '^':
			b.WriteString(`\A`)
		case folded && c >= 'a' && c <= 'z':
			b.WriteByte('[')
			b.WriteByte(c)
			b.WriteByte(c - 32)
			b.WriteByte(']')
		case folded && c >= 'A' && c <= 'Z':
			b.WriteByte('[')
			b.WriteByte(c + 32)
			b.WriteByte(c)
			b.WriteByte(']')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// javaEndPositions returns the UTF-8 byte offsets where Java's `$` can match
// in a non-multiline pattern: absolute end, or before a single final line
// terminator, or before a final atomic CRLF. Terminators are multi-byte in
// UTF-8 (U+0085 = 2 bytes, U+2028/U+2029 = 3 bytes) so the offset is the byte
// index where the terminator begins.
func javaEndPositions(s string) []int {
	n := len(s)
	ps := []int{n}
	if n >= 2 && s[n-2] == '\r' && s[n-1] == '\n' {
		ps = append(ps, n-2)
		return ps
	}
	if n >= 1 {
		switch s[n-1] {
		case '\n', '\r':
			ps = append(ps, n-1)
		case 0x85: // U+0085 NEL, encoded C2 85
			if n >= 2 && s[n-2] == 0xC2 {
				ps = append(ps, n-2)
			}
		case 0xA8, 0xA9: // U+2028 / U+2029, encoded E2 80 A8/A9
			if n >= 3 && s[n-3] == 0xE2 && s[n-2] == 0x80 {
				ps = append(ps, n-3)
			}
		}
	}
	return ps
}

// javaDollarReplaceAll emulates Java Matcher.replaceAll for a pattern that is
// anchored at `$` (compiled here with `\z`): the final line terminator is not
// consumed, but `$` may still match just before it.
func javaDollarReplaceAll(re *regexp.Regexp, s, repl string) string {
	bestStart, bestP := -1, -1
	for _, p := range javaEndPositions(s) {
		loc := re.FindStringIndex(s[:p])
		if loc == nil || loc[1] != p {
			continue
		}
		if bestStart == -1 || loc[0] < bestStart || (loc[0] == bestStart && p > bestP) {
			bestStart, bestP = loc[0], p
		}
	}
	if bestStart == -1 {
		return s
	}
	return s[:bestStart] + repl + s[bestP:]
}

func isASCIIDigit(r rune) bool { return r >= '0' && r <= '9' }

// --- JDK-11 exact membership (tables in jdk11tables.go) ---

func inRanges2(rs [][2]rune, c rune) bool {
	lo, hi := 0, len(rs)-1
	for lo <= hi {
		m := (lo + hi) / 2
		if c < rs[m][0] {
			hi = m - 1
		} else if c > rs[m][1] {
			lo = m + 1
		} else {
			return true
		}
	}
	return false
}

func inRanges3(rs [][3]rune, c rune) (rune, bool) {
	lo, hi := 0, len(rs)-1
	for lo <= hi {
		m := (lo + hi) / 2
		if c < rs[m][0] {
			hi = m - 1
		} else if c > rs[m][1] {
			lo = m + 1
		} else {
			return c + rs[m][2], true
		}
	}
	return c, false
}

// jdkIsLetterOrDigit is JDK 11 Character.isLetterOrDigit.
func jdkIsLetterOrDigit(c rune) bool { return inRanges2(jdkWordRanges, c) }

// jdkIsMn is JDK 11 Character.getType(c) == NON_SPACING_MARK.
func jdkIsMn(c rune) bool { return inRanges2(jdkMnRanges, c) }

// jdkUpperSimple is JDK 11 Character.toUpperCase for BMP code points.
func jdkUpperSimple(c rune) rune {
	if u, ok := inRanges3(jdkUpperRanges, c); ok {
		return u
	}
	return c
}

// isJavaWord mirrors Pattern.Bound.isWord: ch == '_' || isLetterOrDigit(ch).
func isJavaWord(r rune) bool { return r == '_' || jdkIsLetterOrDigit(r) }

// javaCodePointAt mirrors Character.codePointAt(CharSequence, int): it decodes a
// surrogate pair only when the unit at i is a high surrogate and i+1 is low,
// otherwise it returns the single unit (a lone surrogate mid-pair stays lone).
func javaCodePointAt(u []uint16, i int) rune {
	c := u[i]
	if c >= 0xD800 && c <= 0xDBFF && i+1 < len(u) {
		if d := u[i+1]; d >= 0xDC00 && d <= 0xDFFF {
			return utf16.DecodeRune(rune(c), rune(d))
		}
	}
	return rune(c)
}

// javaCodePointBefore mirrors Character.codePointBefore(CharSequence, int).
func javaCodePointBefore(u []uint16, i int) rune {
	c := u[i-1]
	if c >= 0xDC00 && c <= 0xDFFF && i-2 >= 0 {
		if d := u[i-2]; d >= 0xD800 && d <= 0xDBFF {
			return utf16.DecodeRune(rune(d), rune(c))
		}
	}
	return rune(c)
}

// hasBaseU mirrors Pattern.hasBaseCharacter: it walks UTF-16 units downward
// (not code points), so an astral base separated by marks is NOT found - Java
// lands on the low surrogate and stops. That quirk must be reproduced.
func hasBaseU(u []uint16, x int) bool {
	for ; x >= 0; x-- {
		c := javaCodePointAt(u, x)
		if jdkIsLetterOrDigit(c) {
			return true
		}
		if jdkIsMn(c) {
			continue
		}
		return false
	}
	return false
}

// javaBoundaryUnit reports whether Java's \b holds at UTF-16 offset off.
func javaBoundaryUnit(u []uint16, off int) bool {
	left := false
	if off > 0 {
		c := javaCodePointBefore(u, off)
		left = isJavaWord(c) || (jdkIsMn(c) && hasBaseU(u, off-1))
	}
	right := false
	if off < len(u) {
		c := javaCodePointAt(u, off)
		right = isJavaWord(c) || (jdkIsMn(c) && hasBaseU(u, off))
	}
	return left != right
}

// javaBoundary reports whether Java's \b holds at rune position pos.
func javaBoundary(r []rune, pos int) bool {
	u := utf16.Encode(r)
	off := 0
	for i := 0; i < pos; i++ {
		if r[i] >= 0x10000 {
			off += 2
		} else {
			off++
		}
	}
	return javaBoundaryUnit(u, off)
}

func containsLineTerm(s string) bool {
	for _, c := range s {
		if c == '\n' || c == '\r' || c == 0x85 || c == 0x2028 || c == 0x2029 {
			return true
		}
	}
	return false
}

func nextRuneIndex(s string, i int) int {
	if i >= len(s) {
		return i + 1
	}
	_, sz := utf8.DecodeRuneInString(s[i:])
	return i + sz
}

func prevRuneIndex(s string, i int) int {
	if i <= 0 {
		return -1
	}
	_, sz := utf8.DecodeLastRuneInString(s[:i])
	return i - sz
}

func asciiLower(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		b.WriteByte(c)
	}
	return b.String()
}

// javaUpperFirst mirrors java's `s.substring(0,1).toUpperCase() + s.substring(1)`:
// only the first UTF-16 unit is mapped, so a leading supplementary code point
// (surrogate pair) is left untouched, while full BMP expansions apply.
func javaUpperFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	c := r[0]
	if c >= 0x10000 {
		return s
	}
	if exp, ok := jdkFullUpper[c]; ok {
		return exp + string(r[1:])
	}
	if u := jdkUpperSimple(c); u != c {
		r[0] = u
		return string(r)
	}
	return s
}

// unitsToString reconstructs a string from UTF-16 units, rendering an unpaired
// surrogate as '?' exactly as Java's UTF-8 encoder does on output.
func unitsToString(u []uint16) string {
	var b strings.Builder
	for i := 0; i < len(u); i++ {
		c := u[i]
		switch {
		case c >= 0xD800 && c <= 0xDBFF:
			if i+1 < len(u) && u[i+1] >= 0xDC00 && u[i+1] <= 0xDFFF {
				b.WriteRune(utf16.DecodeRune(rune(c), rune(u[i+1])))
				i++
			} else {
				b.WriteByte('?')
			}
		case c >= 0xDC00 && c <= 0xDFFF:
			b.WriteByte('?')
		default:
			b.WriteRune(rune(c))
		}
	}
	return b.String()
}

// javaTrunc mirrors Main.trunc using Java's UTF-16 length and substring, with a
// split surrogate pair becoming '?' in the final UTF-8 output.
func javaTrunc(s string, n int) string {
	if s == "" {
		return ""
	}
	u := utf16.Encode([]rune(s))
	if len(u) <= n {
		return s
	}
	return javaTrim(unitsToString(u[:n-1])) + "\u2026"
}

// javaLength mirrors String.length(): UTF-16 code units.
func javaLength(s string) int {
	return len(utf16.Encode([]rune(s)))
}
