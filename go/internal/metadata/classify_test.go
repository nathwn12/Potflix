package metadata

import "testing"

// TestParseRealWorldPatterns proves ParseRaw classification on verbatim
// filenames from the library inventory (7,803 files, buckets A/B/C/D/E/F/G/X).
// It asserts the (Kind, Title, Season, Episode, Absolute) produced; Year is
// titleclean's job and is not asserted here.
//
// Filename-level notes: "Better Call Saul 01" / "Podcast 01" sit under
// Featurettes in situ, so folder context treats them as extras, but the bare
// filename is a C absolute (space + 2 digits). "Season 01 - Animatic Extra 1"
// is bucketed C in the inventory yet reads as an extra, so it is Unknown.
func TestParseRealWorldPatterns(t *testing.T) {
	cases := []struct {
		bucket          string
		file            string
		kind            Kind
		title           string
		season, episode int
		absolute        bool
	}{
		// A: SxxEyy (already handled, locked here against regressions).
		{"A", "Black Bird (2022) - S01E01 - Pilot (1080p ATVP WEB-DL x265 t3nzin).mkv", KindSeries, "Black Bird", 1, 1, false},
		{"A", "Arcane.S01E02.1080p.NF.WEB-DL.DDP5.1.Atmos.HDR10+.AV1-SomniWare.mkv", KindSeries, "Arcane", 1, 2, false},
		{"A", "Invincible (2021) - S01E01 - It's About Time (1080p WEB-DL x265 SAMPA).mkv", KindSeries, "Invincible", 1, 1, false},
		{"A", "Breaking Bad (2008) - S02E09 - 4 Days Out (1080p BluRay x265 Silence).mkv", KindSeries, "Breaking Bad", 2, 9, false},
		{"A", "[Trix] Shingeki no Kyojin S01E20 (20) (BD 1080p AV1) [0B99ED2A].mkv", KindSeries, "Shingeki no Kyojin", 1, 20, false},
		{"A", "Chainsaw Man - S00E01 - Chainsaw Man - The Movie - Reze Arc.mkv", KindSeries, "Chainsaw Man", 0, 1, false},
		{"A", "The Legend of Korra - S04E10 - Operation Beifong.mkv", KindSeries, "The Legend of Korra", 4, 10, false},
		// B: Episode/Season-word forms.
		{"B", "BLEACH Kai - Episode 49 - The Lost Agent.mkv", KindSeries, "BLEACH Kai", 0, 49, true},
		{"B", "Arcane. Season 01. Episode 01. By Wild_Cat.mkv", KindSeries, "Arcane", 1, 1, false},
		{"B", "Gurren Lagann Parallel Works - Episode 1 (CBB).mkv", KindSeries, "Gurren Lagann Parallel Works", 0, 1, true},
		// C: anime absolute numbers (dash, space, underscore, chapter).
		{"C", "[Anime Time] Re Zero - 01.mkv", KindSeries, "Re Zero", 0, 1, true},
		{"C", "[Cleo]Angel_Beats!_-_01_(Dual Audio_10bit_BD1080p_x265).mkv", KindSeries, "Angel Beats!", 0, 1, true},
		{"C", "Better Call Saul 01.mkv", KindSeries, "Better Call Saul", 0, 1, true},
		{"C", "Podcast 01.mkv", KindSeries, "Podcast", 0, 1, true},
		{"C", "[DB]Danshi Koukousei no Nichijou_-_01_(10bit_BD1080p_x265).mkv", KindSeries, "Danshi Koukousei no Nichijou", 0, 1, true},
		{"C", "[AnimeRG] Bleach - 224 - 3 vs. 1 Battle! Rangiku's Crisis [480p] [x265] [pseudo].mkv", KindSeries, "Bleach", 0, 224, true},
		{"C", "Monster - Chapter 01 - Herr Dr. Tenma.mkv", KindSeries, "Monster", 0, 1, true},
		// D: OP/ED themes are never episodes.
		{"D", "ED 01v2.mkv", KindUnknown, "ED 01v2", 0, 0, false},
		{"D", "[Anime Time] Naruto - OP - 001 -  RO?C?K?S.mkv", KindUnknown, "Naruto", 0, 0, false},
		{"D", "ED 01.mkv", KindUnknown, "ED 1", 0, 0, false},
		{"D", "[Anime Time] Naruto - OP - 002  - Haruka Kanata.mkv", KindUnknown, "Naruto", 0, 0, false},
		{"D", "OP 01.mkv", KindUnknown, "OP 1", 0, 0, false},
		// E: movies; sequel digits, quality tokens and [NNN] tags are not episodes.
		{"E", "21.Jump.Street.2012.2160p.4K.BluRay.x265.10bit.AAC5.1-[YTS.MX].mkv", KindMovie, "21 Jump Street", 0, 0, false},
		{"E", "Kingsman The Golden Circle 2017 1080p BluRay AV1 Opus 5.1 [981].mkv", KindMovie, "Kingsman The Golden Circle", 0, 0, false},
		{"E", "Cars.3.2017.1080p_HEVCCLUB.mkv", KindMovie, "Cars 3", 0, 0, false},
		{"E", "Cars - 2. 2011. 1080p. HEVC. 10 bit.mkv", KindMovie, "Cars - 2 2011 10 bit", 0, 0, false},
		{"E", "[JySzE] Dragon Ball Super - Broly [2018].mkv", KindMovie, "Dragon Ball Super - Broly", 0, 0, false},
		{"E", "John Wick Chapter 1 2014 1080p BluRay HEVC x265 5.1 BONE.mkv", KindMovie, "John Wick Chapter 1 2014 BONE", 0, 0, false},
		{"E", "A Haunted House 2 2014 1080p WEB-DL HEVC x265 5.1 BONE.mkv", KindMovie, "A Haunted House 2 2014 BONE", 0, 0, false},
		// F: extras and unresolvable-alone names are never fetched.
		{"F", "Deleted Scenes.mkv", KindUnknown, "Deleted Scenes", 0, 0, false},
		{"F", "Trailer.mkv", KindUnknown, "Trailer", 0, 0, false},
		{"F", "Trailer 1.mkv", KindUnknown, "Trailer 1", 0, 0, false},
		{"F", "Alternate ending.mkv", KindUnknown, "Alternate ending", 0, 0, false},
		{"F", "Gallery.mkv", KindUnknown, "Gallery", 0, 0, false},
		{"F", "Trailers.mkv", KindUnknown, "Trailers", 0, 0, false},
		{"F", "Interview - Adrian Lyne.mkv", KindUnknown, "Interview - Adrian Lyne", 0, 0, false},
		{"F", "Interviews - Cast & Crew.mkv", KindUnknown, "Interviews - Cast & Crew", 0, 0, false},
		{"F", "Better Call Saul Music Video.mkv", KindUnknown, "Better Call Saul Music Video", 0, 0, false},
		{"F", "Season 01 - Animatic Extra 1.mkv", KindUnknown, "Animatic Extra 1", 0, 0, false},
		{"F", "Sample.mkv", KindUnknown, "Sample", 0, 0, false},
		// G: ranges use the FIRST number.
		{"G", "Avatar - The Last Airbender - S02E12-E13 - The Serpent's Pass & The Drill.mkv", KindSeries, "Avatar - The Last Airbender", 2, 12, false},
		{"G", "BLEACH Kai - Episode 53+54 - The Deathberry Returns 2 + Goodbye to Our Xcution.mkv", KindSeries, "BLEACH Kai", 0, 53, true},
		{"G", "Adventure Time (2008) - S07E14-E15 - The More You Moe & The Moe You Know (1080p BluRay x265 ImE).mkv", KindSeries, "Adventure Time", 7, 14, false},
		{"G", "BLEACH Kai - Episode 3+4 - memories in the rain + QUINCY ARCHER HATES YOU.mkv", KindSeries, "BLEACH Kai", 0, 3, true},
		{"G", "Regular Show (2010) - S03E01-E02 - Stick Hockey & Bet To Be Blonde (1080p AMZN WEB-DL x265 ImE).mkv", KindSeries, "Regular Show", 3, 1, false},
		// X: yearless/ambiguous titles default to Movie (folder or lookup refines).
		{"X", "Designing the End of the World.mkv", KindMovie, "Designing the End of the World", 0, 0, false},
		{"X", "[Valenciano] Evangelion- 3.0+1.0 Thrice Upon a Time [2160p][AV1 10 bit][Opus 5.1][Multi-Audio][Multi-Sub].mkv", KindMovie, "Evangelion- 3 0+1 0 Thrice Upon a Time", 0, 0, false},
		{"X", "[Sav1or] Natsu e no Tunnel, Sayonara no Deguchi (The Tunnel to Summer, the Exit of Goodbyes) [BD][804p][AV1][OPUS][Dual-Audio].mkv", KindMovie, "Natsu e no Tunnel, Sayonara no Deguchi", 0, 0, false},
		{"X", "[Lulu] Demon Slayer - Infinity Train [BD 1080p x264][FLAC][E-AC3][Dual-Audio].mkv", KindMovie, "Demon Slayer - Infinity Train", 0, 0, false},
		{"X", "[NH] Weathering With You V2 (BD 1080p x265 10-bit FLAC) [Dual-Audio] [830080EB].mkv", KindMovie, "Weathering With You V2", 0, 0, false},
		{"X", "NCED 1 (001-199).mkv", KindUnknown, "ED 1", 0, 0, false},
		{"X", "NCOP 1.1 (001-021).mkv", KindUnknown, "NCOP", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.bucket+"/"+tc.file, func(t *testing.T) {
			q := ParseRaw(tc.file)
			if q.Kind != tc.kind || q.Title != tc.title || q.Season != tc.season ||
				q.Episode != tc.episode || q.Absolute != tc.absolute {
				t.Fatalf("ParseRaw(%q) = (Kind=%d Title=%q S=%d E=%d Abs=%v), want (Kind=%d Title=%q S=%d E=%d Abs=%v)",
					tc.file, q.Kind, q.Title, q.Season, q.Episode, q.Absolute,
					tc.kind, tc.title, tc.season, tc.episode, tc.absolute)
			}
		})
	}
}

// TestParseThemeDirect locks the OP/ED -> KindUnknown change at the Parse seam.
func TestParseThemeDirect(t *testing.T) {
	for _, tc := range []struct {
		title, tag string
	}{
		{"Naruto", "OP 1"},
		{"Naruto", "OP1"},
		{"Show", "ED 2"},
		{"Show", "ED 12"},
		{"ED 01v2", ""},
		{"Show - OP - 001 - Song", ""},
	} {
		if q := Parse(tc.title, tc.tag, ""); q.Kind != KindUnknown {
			t.Fatalf("Parse(%q,%q) Kind = %d, want KindUnknown (%+v)", tc.title, tc.tag, q.Kind, q)
		}
	}
}

// TestParseYearNeverEpisode locks that 19xx/20xx is a year, never an episode.
func TestParseYearNeverEpisode(t *testing.T) {
	if q := Parse("Show", "E2015", ""); q.Kind != KindMovie || q.Episode != 0 {
		t.Fatalf("Parse(Show,E2015) = %+v, want KindMovie with no episode", q)
	}
	if q := Parse("Show", "E101", ""); q.Kind != KindSeries || q.Episode != 101 || !q.Absolute {
		t.Fatalf("Parse(Show,E101) = %+v, want KindSeries absolute 101", q)
	}
}

// TestParseAmbiguousAndBare locks the ambiguous/bare-title floor: a space +
// single digit is not an episode, bare numbers are unresolvable, a lone year
// is a film.
func TestParseAmbiguousAndBare(t *testing.T) {
	if q := Parse("Show 1", "", ""); q.Kind != KindMovie || q.Episode != 0 {
		t.Fatalf("Parse(Show 1) = %+v, want KindMovie with no episode", q)
	}
	if q := Parse("", "", ""); q.Kind != KindUnknown {
		t.Fatalf("Parse(empty) Kind = %d, want KindUnknown", q.Kind)
	}
	if q := Parse("01", "", ""); q.Kind != KindUnknown {
		t.Fatalf("Parse(01) Kind = %d, want KindUnknown", q.Kind)
	}
	if q := Parse("2012", "", ""); q.Kind != KindMovie || q.Year != 2012 {
		t.Fatalf("Parse(2012) = %+v, want KindMovie Year=2012", q)
	}
}
