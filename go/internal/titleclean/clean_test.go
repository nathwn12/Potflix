package titleclean

import (
	"regexp"
	"testing"
)

// Expectations in this file were produced by running the installed JDK
// (java 11.0.32, Locale en_US) against the same inputs; none are corpus data.

func bits(r []rune) string {
	out := make([]byte, 0, len(r)+1)
	for i := 0; i <= len(r); i++ {
		if javaBoundary(r, i) {
			out = append(out, '1')
		} else {
			out = append(out, '0')
		}
	}
	return string(out)
}

func TestJavaBoundary(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a", "11"},
		{"a\u0301", "101"},
		{"\u0301", "00"},
		{"\u00E9", "11"},
		{"\u7248", "11"},
		{"a\u0301b", "1001"},
		{"\u0301a", "011"},
		{"a\u0301\u0301b", "10001"},
		{"1\u0301", "101"},
		{"A_1", "1001"},
		{"\u017F", "11"},
		{"\u212A", "11"},
		{"\u0131", "11"},
		{"a\u212A", "101"},
		{"\u00DF", "11"},
		{"_\u0301", "110"},
	}
	for _, c := range cases {
		if got := bits([]rune(c.in)); got != c.want {
			t.Errorf("javaBoundary(%q) bits = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestJavaTrunc(t *testing.T) {
	astral := "\U0001D54F" // U+1D54F, one code point, two UTF-16 units
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"AAAA" + astral + "B", 3, "AA\u2026"},
		{"AAAA" + astral + "B", 4, "AAA\u2026"},
		{"AAAA" + astral + "B", 5, "AAAA\u2026"},
		{"AAAA" + astral + "B", 6, "AAAA?\u2026"}, // Java cut a surrogate pair; UTF-8 emits '?'
		{"short", 10, "short"},
		{"", 5, ""},
	}
	for _, c := range cases {
		if got := javaTrunc(c.in, c.n); got != c.want {
			t.Errorf("javaTrunc(%q,%d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestJavaUpperFirst(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\u00DFeta", "SSeta"},
		{"\uFB01nal", "FInal"},
		{"\u01F0ack", "J\u030Cack"},
		{"\U0001D569yle", "\U0001D569yle"}, // leading surrogate pair is left as-is by Java
		{"hello", "Hello"},
		{"", ""},
	}
	for _, c := range cases {
		if got := javaUpperFirst(c.in); got != c.want {
			t.Errorf("javaUpperFirst(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAsciiFold(t *testing.T) {
	season := regexp.MustCompile(portPattern(`(?i)season`, true))
	for _, s := range []string{"season", "SEASON", "Season", "sEaSoN"} {
		if !season.MatchString(s) {
			t.Errorf("ascii-folded season should match %q", s)
		}
	}
	if season.MatchString("\u017Feason") {
		t.Error("Java CASE_INSENSITIVE is ASCII-only; ſeason must not match season")
	}
	k := regexp.MustCompile(portPattern(`(?i)4k`, true))
	if !k.MatchString("4K") {
		t.Error("ascii-folded 4k should match ASCII K")
	}
	if k.MatchString("4\u212A") {
		t.Error("Kelvin sign K must not fold to k under Java CASE_INSENSITIVE")
	}
	i := regexp.MustCompile(portPattern(`(?i)i`, true))
	if i.MatchString("\u0131") {
		t.Error("dotless i must not fold to i under Java CASE_INSENSITIVE")
	}
}

func TestJavaEndPositions(t *testing.T) {
	cases := []struct {
		in   string
		want []int
	}{
		{"abc", []int{3}},
		{"a\n", []int{2, 1}},
		{"a\r", []int{2, 1}},
		{"a\r\n", []int{3, 1}},
		{"a\u2028", []int{4, 1}},
		{"a\u0085", []int{3, 1}},
		{"a\u2029", []int{4, 1}},
	}
	for _, c := range cases {
		got := javaEndPositions(c.in)
		if len(got) != len(c.want) {
			t.Errorf("javaEndPositions(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("javaEndPositions(%q) = %v, want %v", c.in, got, c.want)
				break
			}
		}
	}
}

func TestJavaDollarReplace(t *testing.T) {
	sep := regexp.MustCompile(portPattern(`[. _\-]+$`, false))
	if got := javaDollarReplaceAll(sep, "Name \u2028", ""); got != "Name\u2028" {
		t.Errorf("trailing separator before U+2028: got %q", got)
	}
	crc := regexp.MustCompile(portPattern(jws+`*\[[A-Fa-f0-9]{8}\]`+jws+`*$`, false))
	if got := javaDollarReplaceAll(crc, "Name [DEADBEEF]\u2028", ""); got != "Name\u2028" {
		t.Errorf("CRC before U+2028: got %q", got)
	}
	if got := javaDollarReplaceAll(crc, "Name [DEADBEEF]\u0085", ""); got != "Name\u0085" {
		t.Errorf("CRC before U+0085: got %q", got)
	}
}

func TestDotExcludesLineTerminators(t *testing.T) {
	re := regexp.MustCompile(portPattern(`^a.b$`, false))
	if !re.MatchString("a\u00E9b") {
		t.Error("dot must match non-terminator code points such as é")
	}
	for _, s := range []string{"a\nb", "a\rb", "a\u0085b", "a\u2028b", "a\u2029b"} {
		if re.MatchString(s) {
			t.Errorf("Java dot must not match line terminator in %q", s)
		}
	}
}

func TestJavaBoundaryAstralMarks(t *testing.T) {
	// Java's hasBaseCharacter steps UTF-16 units, so an astral base behind a
	// mark run is not found; these bits are JDK truth (code-point positions).
	cases := []struct{ in, want string }{
		{"\U0001D400\u0301\u0301", "1100"},
		{"a\U0001D400\u0301\u0301", "10100"},
		{"\U0001D400\u0301", "110"},
		{"\U0001D400x", "101"},
		{"\u0301\U0001D400", "011"},
		{"x265Chapter\u0301\U0001D400\u0301\u0301", "1000000000000100"},
	}
	for _, c := range cases {
		if got := bits([]rune(c.in)); got != c.want {
			t.Errorf("javaBoundary(%q) bits = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestJdk11Tables(t *testing.T) {
	// Drift rows: Go 1.27 carries newer Unicode than JDK 11.
	// U+1C90 Georgian Mtavruli is a letter in newer Unicode, not in JDK 11.
	if isJavaWord(0x1C90) {
		t.Error("JDK 11 does not treat U+1C90 as a word character")
	}
	if jdkIsLetterOrDigit(0x1E100) {
		t.Error("JDK 11 does not treat U+1E100 as a letter")
	}
	if !isJavaWord(0x10D0) {
		t.Error("U+10D0 is a letter in JDK 11")
	}
	if jdkUpperSimple(0x10D0) != 0x10D0 {
		t.Errorf("JDK 11 leaves U+10D0 unchanged, got %04X", jdkUpperSimple(0x10D0))
	}
	if jdkUpperSimple(0x019B) != 0x019B {
		t.Errorf("JDK 11 leaves U+019B unchanged, got %04X", jdkUpperSimple(0x019B))
	}
	if jdkIsMn(0x07FD) {
		t.Error("JDK 11 does not classify U+07FD as a non-spacing mark")
	}
	// Drift rows through the whole clean() pipeline.
	if c := clean("Movie\u1C901080p.mkv"); c.Title != "Movie\u1C90" || c.EpisodeTag != "" {
		t.Errorf("Movie<U+1C90>1080p.mkv = %+v, want title Movie\\u1C90", c)
	}
	if c := clean("\u10D0bc.mkv"); c.Title != "\u10D0bc" {
		t.Errorf("<U+10D0>bc.mkv = %+v, want title \\u10D0bc", c)
	}
	// A sample of areas that already matched.
	if !isJavaWord('a') || !isJavaWord('_') || isJavaWord('\u0301') {
		t.Error("word membership sample drifted")
	}
	if !jdkIsLetterOrDigit(0x00E9) || !jdkIsLetterOrDigit(0x7248) {
		t.Error("letter sample drifted")
	}
	if !jdkIsMn(0x0301) {
		t.Error("U+0301 should be a non-spacing mark")
	}
	if jdkUpperSimple('a') != 'A' || jdkUpperSimple(0x00B5) != 0x039C || jdkUpperSimple(0x03C2) != 0x03A3 {
		t.Error("simple uppercase sample drifted")
	}
	if jdkFullUpper[0x00DF] != "SS" || jdkFullUpper[0xFB01] != "FI" || jdkFullUpper[0x01F0] != "J\u030C" {
		t.Error("full uppercase expansive sample drifted")
	}
}

func TestBoundaryGuards(t *testing.T) {
	if !hasYear("Show 2019") {
		t.Error("hasYear should accept a boundary-delimited year")
	}
	if !hasYear("Show.2019.1080p") {
		t.Error("hasYear should accept a separator-delimited year")
	}
	if hasYear("2019\u5E74") {
		t.Error("2019年: CJK letter is a word char, so Java sees no boundary after the year")
	}
	if hasYear("2019\u2028") {
		t.Error("a line terminator blocks the surrounding .* in hasYear")
	}
	if !altHeadBad("Show Season") {
		t.Error("altHeadBad should reject a trailing Season word")
	}
	if altHeadBad("Show \u017Feason") {
		t.Error("altHeadBad fold is ASCII-only; ſeason is not Season")
	}
	if altHeadBad("Show Season\u2028") {
		t.Error("altHeadBad must fail when a line terminator is present")
	}
	if !yearLead("2019 x") {
		t.Error("yearLead should accept trailing content after a boundary")
	}
	if yearLead("2019\u5E74") {
		t.Error("yearLead boundary after 2019 fails before a CJK letter")
	}
}
