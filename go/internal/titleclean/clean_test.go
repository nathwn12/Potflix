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

// TestCleanSection6 pins every row of tools/naming-report.md section 6.
// Each expectation was verified byte-identical against the installed JDK
// (11.0.32) running the frozen Main.clean over the same input; the Go port
// matched all 26 verification rows (these 10 plus TestCleanEpisodePatterns).
func TestCleanSection6(t *testing.T) {
	cases := []struct {
		in                        string
		title, tag, name, display string
	}{
		{
			"Breaking.Bad.S02E08.Better.Call.Saul.1080p.BluRay.x265-CtrlHD.mkv",
			"Breaking Bad", "S2 E8", "Better Call Saul",
			"Breaking Bad \u2014 S2 E8 \"Better Call Saul\"",
		},
		{
			"S01E03-The Rookie Fire Soldier Games [0CCBF9D4].mkv",
			"The Rookie Fire Soldier Games", "S1 E3", "",
			"The Rookie Fire Soldier Games \u2014 S1 E3",
		},
		{
			"The Amazing World of Gumball S02E13E14  The Skull - The Bet.mkv",
			"The Amazing World of Gumball", "S2 E13-14", "The Skull - The Bet",
			"The Amazing World of Gumball \u2014 S2 E13-14 \"The Skull - The Bet\"",
		},
		{
			"[Kosaka] Serial Experiments Lain - 01x09 - Layer09 PROTOCOL.mkv",
			"Serial Experiments Lain", "S1 E9", "Layer09 PROTOCOL",
			"Serial Experiments Lain \u2014 S1 E9 \"Layer09 PROTOCOL\"",
		},
		{
			"Arcane. Season 01. Episode 05. By Wild_Cat.mkv",
			"Arcane", "S1 E5", "By Wild Cat",
			"Arcane \u2014 S1 E5 \"By Wild Cat\"",
		},
		{
			"Monster - Chapter 01 - Herr Dr. Tenma.mkv",
			"Monster", "E1", "Herr Dr Tenma",
			"Monster \u2014 E1 \"Herr Dr Tenma\"",
		},
		{
			"[DB]Saiki Kusuo no Psi-nan_-_NCOP01_(10bit_BD1080p_x265).mkv",
			"Saiki Kusuo no Psi-nan", "OP 1", "",
			"Saiki Kusuo no Psi-nan \u2014 OP 1",
		},
		{
			"The.Boys.S05E04.1080p.WEB.h264-ETHEL.mkv",
			"The Boys", "S5 E4", "",
			"The Boys \u2014 S5 E4",
		},
		{
			"Deleted and Extended Scenes.mkv",
			"Deleted and Extended Scenes", "", "",
			"Deleted and Extended Scenes",
		},
		{
			"[2ndfire] Tengen Toppa Gurren Lagann - 26 - Let's Go, Comrades! [145E4A7F].mkv",
			"Tengen Toppa Gurren Lagann", "E26", "Let's Go, Comrades!",
			"Tengen Toppa Gurren Lagann \u2014 E26 \"Let's Go, Comrades!\"",
		},
	}
	for _, c := range cases {
		got := clean(c.in)
		if got.Title != c.title || got.EpisodeTag != c.tag || got.EpisodeName != c.name || got.Display != c.display {
			t.Errorf("clean(%q) = %+v, want title=%q tag=%q name=%q display=%q",
				c.in, got, c.title, c.tag, c.name, c.display)
		}
	}
}

// TestCleanEpisodePatterns covers the P2-P9 episode rules from
// tools/naming-report.md section 5 in report order: MULTI before SERIES,
// EPWORD before ALT_EP, and the CHAPTER/TRAIL year guards plus the ALT_EP
// connector/year guards. Expectations are JDK-verified ground truth, same
// method as TestCleanSection6.
func TestCleanEpisodePatterns(t *testing.T) {
	cases := []struct {
		in                        string
		title, tag, name, display string
	}{
		// P3 MULTI, second-episode form (MULTI wins over SERIES: E4-5, not E4).
		{
			"Show S1950E04E05 Name.mkv",
			"Show", "S1950 E4-5", "Name",
			"Show \u2014 S1950 E4-5 \"Name\"",
		},
		// P2 SERIES: empty head takes the tag alone; widened 4-digit season.
		{
			"Show S1950E04.mkv",
			"Show", "S1950 E4", "",
			"Show \u2014 S1950 E4",
		},
		{
			"S02E07.mkv",
			"S2 E7", "S2 E7", "",
			"S2 E7",
		},
		// P4 EPWORD: headless, headed, and Ep-abbreviation forms.
		{
			"Episode 104.mkv",
			"E104", "E104", "",
			"E104",
		},
		{
			"BLEACH Kai - Episode 38 - FEAR FOR FIGHT.mkv",
			"BLEACH Kai", "E38", "FEAR FOR FIGHT",
			"BLEACH Kai \u2014 E38 \"FEAR FOR FIGHT\"",
		},
		{
			"Show Ep 12 Finale.mkv",
			"Show", "E12", "Finale",
			"Show \u2014 E12 \"Finale\"",
		},
		// P5 SEASON_EXTRA: numeric tail is an episode; otherwise the
		// "Season NN -" connector is dropped and no tag is produced.
		{
			"Arcane Season 01 05 By Wild Cat.mkv",
			"Arcane", "S1 E5", "By Wild Cat",
			"Arcane \u2014 S1 E5 \"By Wild Cat\"",
		},
		{
			"Season 03 - Stop-Motion Lego Intro.mkv",
			"Stop-Motion Lego Intro", "", "",
			"Stop-Motion Lego Intro",
		},
		// P6 CHAPTER year guard: a dated movie keeps "Chapter 1" as title text.
		{
			"John Wick Chapter 1 2014 1080p BluRay HEVC x265 5.1 BONE.mkv",
			"John Wick Chapter 1 2014 BONE", "", "",
			"John Wick Chapter 1 2014 BONE",
		},
		// P8 TRAIL: zero-prefixed tail is an episode ...
		{
			"Podcast 07.mkv",
			"Podcast", "E7", "",
			"Podcast \u2014 E7",
		},
		// ... but a year tail is not (YEAR suffix still applies).
		{
			"Show 05 2019.mkv",
			"Show 05 (2019)", "", "",
			"Show 05 (2019)",
		},
		// P9 ALT_EP guards: connector-headed head and bare-number tail rejected.
		{
			"The Hunger Games Mockingjay Part 2 Extended.mkv",
			"The Hunger Games Mockingjay Part 2 Extended", "", "",
			"The Hunger Games Mockingjay Part 2 Extended",
		},
		{
			"Apollo 13.mkv",
			"Apollo 13", "", "",
			"Apollo 13",
		},
	}
	for _, c := range cases {
		got := clean(c.in)
		if got.Title != c.title || got.EpisodeTag != c.tag || got.EpisodeName != c.name || got.Display != c.display {
			t.Errorf("clean(%q) = %+v, want title=%q tag=%q name=%q display=%q",
				c.in, got, c.title, c.tag, c.name, c.display)
		}
	}
}

// TestCleanTagVocabP10P11 pins the P10 TAGS vocab additions/broadenings/
// removals and the P11 SCENE_HINT additions from tools/naming-report.md
// section 5 (report section 4 RC2/RC4 rows). The h264-ETHEL row is already
// covered by TestCleanSection6 row 8; bare "Extended" is covered by its row 9.
// Each expectation was verified byte-identical against the installed JDK
// (11.0.32) running the frozen Main.clean over the same input.
func TestCleanTagVocabP10P11(t *testing.T) {
	cases := []struct {
		in                        string
		title, tag, name, display string
	}{
		// P11 opus -> SCENE_HINT drops -RAV1NE; P10 5.1/unrated/av1 tags strip.
		{
			"The.Dictator.2012.UNRATED.1080p.BluRay.AV1.Opus.5.1-RAV1NE.mkv",
			"The Dictator (2012)", "", "",
			"The Dictator (2012)",
		},
		// P10 hdr\d*\+? leaves no bare "+"; nf strips; P11 av1 drops -SomniWare.
		{
			"Arcane.S01E03.HDR10+.AV1.WEB-DL.NF-SomniWare.mkv",
			"Arcane", "S1 E3", "",
			"Arcane \u2014 S1 E3",
		},
		// P10 ddp broaden covers DDP5.1; hdr/atmos/x265 strip.
		{
			"Dune.Part.Two.2024.2160p.WEB-DL.HDR.DDP5.1.Atmos.x265.mkv",
			"Dune Part Two (2024)", "", "",
			"Dune Part Two (2024)",
		},
		// P10 aac broaden covers AAC5.1; -YTS group suffix drops.
		{
			"War.Dogs.2016.AAC5.1.BluRay.x264-YTS.mkv",
			"War Dogs (2016)", "", "",
			"War Dogs (2016)",
		},
		// P10 removed bare final: a real title word survives.
		{
			"The Final Battle.mkv",
			"The Final Battle", "", "",
			"The Final Battle",
		},
		// P10 extended/theatrical strip only in *-cut form.
		{
			"Avatar.2009.Extended.Cut.1080p.BluRay.x264.mkv",
			"Avatar (2009)", "", "",
			"Avatar (2009)",
		},
		{
			"Avatar.2009.Theatrical.Cut.1080p.BluRay.x264.mkv",
			"Avatar (2009)", "", "",
			"Avatar (2009)",
		},
		{
			"Original Japanese Theatrical Trailers.mkv",
			"Original Japanese Theatrical Trailers", "", "",
			"Original Japanese Theatrical Trailers",
		},
		// No bare ma add: "Ma" survives (report section 5 P10 note).
		{
			"Ma.2019.1080p.BluRay.mkv",
			"Ma (2019)", "", "",
			"Ma (2019)",
		},
	}
	for _, c := range cases {
		got := clean(c.in)
		if got.Title != c.title || got.EpisodeTag != c.tag || got.EpisodeName != c.name || got.Display != c.display {
			t.Errorf("clean(%q) = %+v, want title=%q tag=%q name=%q display=%q",
				c.in, got, c.title, c.tag, c.name, c.display)
		}
	}
}

// TestCleanP1P12 pins the naming-report P1 trailing-separator strip and the
// P12 empty-title fallback. The NCED02 row is JDK-verified ground truth
// (java 11.0.32 running the frozen Main.clean over the same input); the
// ED 03 row is the deliberate Go-side deviation -- the frozen cleaner yields
// title/display "ED 3"/"ED 3 - ED 3" there (harness-verified), the Go port
// shows the tag alone per the report. The Saiki NCOP01 (`OP 1`) and the
// S01E03 headless-title rows already live in TestCleanSection6 rows 7 and 2.
// TestCleanTitleMarkFold pins the Go-side deviation P13: the character a
// scene release leaves where Windows forbids ':' - '?' (U+003F, which is
// also what Java's UTF-8 encoder writes for an unpaired surrogate) and its
// fullwidth/small lookalikes (U+FF1F, U+FE56) - folds back to ':' inside an
// extracted title. Live case (2026-10-13): "Spider-Man? Brand New Day
// (2026) (1080p MA WEB-DL x265 EAC3 Silence).mkv" reached Wikipedia as
// "Spider-Man? Brand New Day" and cached under key
// "movie|spider-man? brand new day|0".
//
// The rule: a mark folds ONLY when it sits between title text - preceded by
// a non-space character and not the last character of the extracted title.
// A mark that ends the title is a real question mark and is kept, since no
// scene convention distinguishes "Who Framed Roger Rabbit?" from a stand-in
// with its tail already stripped.
func TestCleanTitleMarkFold(t *testing.T) {
	cases := []struct {
		name                        string
		in                          string
		title, tag, epName, display string
	}{
		{
			// The exact live filename, colon stand-in between title text.
			"live spider-man filename",
			"Spider-Man? Brand New Day (2026) (1080p MA WEB-DL x265 EAC3 Silence).mkv",
			"Spider-Man: Brand New Day", "", "",
			"Spider-Man: Brand New Day",
		},
		{
			// Same stand-in with no following space: the mark is replaced
			// one-for-one, so the spacing comes from the filename.
			"no space after the mark",
			"Spider-Man?Brand New Day (2026).mkv",
			"Spider-Man:Brand New Day", "", "",
			"Spider-Man:Brand New Day",
		},
		{
			// The fullwidth lookalike folds the same way.
			"fullwidth lookalike",
			"Spider-Man\uFF1FBrand New Day (2026).mkv",
			"Spider-Man:Brand New Day", "", "",
			"Spider-Man:Brand New Day",
		},
		{
			// A trailing mark ends the extracted title once the
			// parenthesised tail is gone: kept, never a dangling colon.
			"trailing mark",
			"Spider-Man? (2026) (1080p MA WEB-DL x265 EAC3 Silence).mkv",
			"Spider-Man?", "", "",
			"Spider-Man?",
		},
		{
			// The only mark is a real question mark at the end.
			"real question mark",
			"Who Framed Roger Rabbit? (1988).mkv",
			"Who Framed Roger Rabbit?", "", "",
			"Who Framed Roger Rabbit?",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := clean(c.in)
			if got.Title != c.title || got.EpisodeTag != c.tag || got.EpisodeName != c.epName || got.Display != c.display {
				t.Errorf("clean(%q) = %+v, want title=%q tag=%q name=%q display=%q",
					c.in, got, c.title, c.tag, c.epName, c.display)
			}
		})
	}
}

func TestCleanP1P12(t *testing.T) {
	cases := []struct {
		in                        string
		title, tag, name, display string
	}{
		// P1: trailing "_" left by the parenthetical removal no longer
		// blocks NCBOOK, so the season number is not stolen as E2.
		{
			"[DB]Saiki Kusuo no Psi-nan 2_-_NCED02_(10bit_BD1080p_x265).mkv",
			"Saiki Kusuo no Psi-nan 2", "ED 2", "",
			"Saiki Kusuo no Psi-nan 2 \u2014 ED 2",
		},
		// P12: headless tag with no episode name shows the tag alone.
		{
			"ED 03.mkv",
			"ED 3", "ED 3", "",
			"ED 3",
		},
	}
	for _, c := range cases {
		got := clean(c.in)
		if got.Title != c.title || got.EpisodeTag != c.tag || got.EpisodeName != c.name || got.Display != c.display {
			t.Errorf("clean(%q) = %+v, want title=%q tag=%q name=%q display=%q",
				c.in, got, c.title, c.tag, c.name, c.display)
		}
	}
}
