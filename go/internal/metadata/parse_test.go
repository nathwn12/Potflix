package metadata

import (
	"testing"

	"potflix/internal/titleclean"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name            string
		title, tag, epi string
		want            Query
	}{
		{"season episode", "Show", "S1 E2", "", Query{Kind: KindSeries, Title: "Show", Season: 1, Episode: 2}},
		{"no space lowercase", "Show", "s01e02", "", Query{Kind: KindSeries, Title: "Show", Season: 1, Episode: 2}},
		{"absolute", "Show", "E12", "", Query{Kind: KindSeries, Title: "Show", Episode: 12, Absolute: true}},
		{"op theme", "Zentaro", "OP 1", "", Query{Kind: KindUnknown, Title: "Zentaro"}},
		{"ed lowercase theme", "Zentaro", "ed 12", "", Query{Kind: KindUnknown, Title: "Zentaro"}},
		{"movie", "The Matrix", "", "", Query{Kind: KindMovie, Title: "The Matrix"}},
		{"year stripped", "Harbor Lights (2008)", "S1 E1", "", Query{Kind: KindSeries, Title: "Harbor Lights", Year: 2008, Season: 1, Episode: 1}},
		{"movie year", "Inception (2010)", "", "", Query{Kind: KindMovie, Title: "Inception", Year: 2010}},
		{"no signal", "Show", "Special", "", Query{Kind: KindMovie, Title: "Show"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Parse(tc.title, tc.tag, tc.epi); got != tc.want {
				t.Fatalf("Parse(%q,%q,%q) = %+v, want %+v", tc.title, tc.tag, tc.epi, got, tc.want)
			}
		})
	}
}

func TestParseTitlecleanSeries(t *testing.T) {
	c := titleclean.Clean("Show.S01E02.Pilot.1080p.mkv")
	if c.EpisodeTag != "S1 E2" {
		t.Fatalf("EpisodeTag = %q, want %q", c.EpisodeTag, "S1 E2")
	}
	if c.EpisodeName != "Pilot" {
		t.Fatalf("EpisodeName = %q, want %q", c.EpisodeName, "Pilot")
	}
	q := Parse(c.Title, c.EpisodeTag, c.EpisodeName)
	want := Query{Kind: KindSeries, Title: "Show", Season: 1, Episode: 2}
	if q != want {
		t.Fatalf("Parse(cleaned) = %+v, want %+v", q, want)
	}
}

func TestParseTitlecleanMovie(t *testing.T) {
	c := titleclean.Clean("Inception.2010.1080p.BluRay.x264.mkv")
	if c.EpisodeTag != "" {
		t.Fatalf("EpisodeTag = %q, want empty (movie, no tag)", c.EpisodeTag)
	}
	q := Parse(c.Title, c.EpisodeTag, c.EpisodeName)
	if q.Kind != KindMovie {
		t.Fatalf("Kind = %v, want KindMovie", q.Kind)
	}
	if q.Title != "Inception" || q.Year != 2010 {
		t.Fatalf("got Title=%q Year=%d, want Title=%q Year=%d", q.Title, q.Year, "Inception", 2010)
	}
}

func TestParseRaw(t *testing.T) {
	q1 := ParseRaw("Show.S01E02.Pilot.1080p.mkv - PotPlayer")
	want := Query{Kind: KindSeries, Title: "Show", Season: 1, Episode: 2}
	if q1 != want {
		t.Fatalf("ParseRaw suffixed = %+v, want %+v", q1, want)
	}
	if q2 := ParseRaw("Show.S01E02.Pilot.1080p.mkv"); q2 != want {
		t.Fatalf("ParseRaw bare = %+v, want %+v", q2, want)
	}
}

// TestParseBonusVariantCoverage is the executable parse table for the 34
// synthetic bonus-variant negative-cache keys. 18 phrase-stem
// extras are KindUnknown (each saves one ~3000 ms wasted iTunes lookup); the
// other 16 stay KindMovie because no phrase stem covers them without risking
// a real title - single common words ("short", "action", "becoming"),
// locations ("cantonia, usa"), and bare chapter numbers ("2) ...") are
// barred, and lost coverage is worse than a wasted lookup.
func TestParseBonusVariantCoverage(t *testing.T) {
	cases := []struct {
		file string
		want Kind
	}{
		{"blooper reel.mkv", KindUnknown},
		{"gag reel.mkv", KindUnknown},
		{"outtakes.mkv", KindUnknown},
		{"tv spots.mkv", KindUnknown},
		{"extended scenes.mkv", KindUnknown},
		{"alternate & extended scenes.mkv", KindUnknown},
		{"behind the story - harbor tour with rex.mkv", KindUnknown},
		{"behind the story - the quiet cadence of harlan marsh.mkv", KindUnknown},
		{"cinder - the making of the drifters.mkv", KindUnknown},
		{"charting the course - the making of harbor run.mkv", KindUnknown},
		{"in search of driftland.mkv", KindUnknown},
		{"how to survive the q tempest.mkv", KindUnknown},
		{"meet the cast.mkv", KindUnknown},
		{"previz - distant concepts.mkv", KindUnknown},
		{"visual effects assembly reels.mkv", KindUnknown},
		{"replacing milo - the secret auditions.mkv", KindUnknown},
		{"4) the production design.mkv", KindUnknown},
		{"5) building harborline - time lapse video.mkv", KindUnknown},
		{"lateral thinking - the cutting process.mkv", KindMovie},
		{"becoming harlow.mkv", KindMovie},
		{"how to sketch.mkv", KindMovie},
		{"chart of ruin.mkv", KindMovie},
		{"testing the bounds.mkv", KindMovie},
		{"retrieval mode.mkv", KindMovie},
		{"ignition - harlan's story.mkv", KindMovie},
		{"the music of 1974.mkv", KindMovie},
		{"humbucker.mkv", KindMovie},
		{"tai chi walking.mkv", KindMovie},
		{"cantonia, usa.mkv", KindMovie},
		{"porto verro, usa.mkv", KindMovie},
		{"kestria, japan.mkv", KindMovie},
		{"the mimickers.mkv", KindMovie},
		{"2) milo announcement.mkv", KindMovie},
		{"3) the harbor brothers.mkv", KindMovie},
	}
	unknown := 0
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			if q := ParseRaw(tc.file); q.Kind != tc.want {
				t.Fatalf("ParseRaw(%q) Kind = %d, want %d (%+v)", tc.file, q.Kind, tc.want, q)
			}
		})
		if ParseRaw(tc.file).Kind == KindUnknown {
			unknown++
		}
	}
	if unknown != 18 {
		t.Fatalf("Unknown count = %d, want 18 (table drift: a stem or titleclean changed)", unknown)
	}
}

// TestParseExtrasNoRealTitleCollision is the collision guard: the synthetic
// representative titles must still parse as Movie/Series, never Unknown.
// Harbor Run 2 pins KindSeries: titleclean splits the sequel digit into an
// E2 absolute tag (pre-existing behaviour, unrelated to extraRe) - the point
// is it is fetched, never skipped.
func TestParseExtrasNoRealTitleCollision(t *testing.T) {
	cases := []struct {
		file string
		want Kind
	}{
		{"22.Warp.Avenue.2013.2160p.mkv", KindMovie},
		{"Iron.Circle.1999.mkv", KindMovie},
		{"Harbor.Island.2011.mkv", KindMovie},
		{"The.Wayfarer.2015.mkv", KindMovie},
		{"Driftfall.2011.mkv", KindMovie},
		{"Harlan.Unbound.2012.mkv", KindMovie},
		{"Drift.2011.1080p.mkv", KindMovie},
		{"Stone.Rider.2051.2017.mkv", KindMovie},
		{"The.Harbor.Trials.2012.mkv", KindMovie},
		{"We.re.the.Harlows.2013.mkv", KindMovie},
		{"Snap-Back.mkv", KindMovie},
		{"Quimbly.mkv", KindMovie},
		{"Atlantic.Edge.mkv", KindMovie},
		{"Marisol.mkv", KindMovie},
		{"Mimic.2019.1080p.mkv", KindMovie},
		{"Pivot.2015.1080p.mkv", KindMovie},
		{"Driftland.Double.Hit.2019.mkv", KindMovie},
		{"Harbor.Run.2.Styx.2018.mkv", KindSeries},
		{"The.Drifters.3.2014.mkv", KindMovie},
		{"Welcome.to.Harbor.S01E01.mkv", KindSeries},
		// Time Lapse (2014) is the collision that forced the time-lapse stem
		// to require a trailing "video": the bare film title must stay
		// fetched (KindMovie), while the "time lapse video" extras stay
		// skipped (KindUnknown, covered in the bonus table).
		{"Time.Lapse.2014.mkv", KindMovie},
		{"Time.Lapse.2014.1080p.mkv", KindMovie},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			if q := ParseRaw(tc.file); q.Kind != tc.want {
				t.Fatalf("ParseRaw(%q) Kind = %d, want %d (%+v)", tc.file, q.Kind, tc.want, q)
			}
		})
	}
}

// TestParseTimeLapseStemConstrained pins the time-lapse fix: the bare film
// "Time Lapse" (2014) is fetched, while the observed "time lapse video"
// extras phrasing is still skipped. The stem requires a trailing "video",
// so no plausible release title without that word can collide.
func TestParseTimeLapseStemConstrained(t *testing.T) {
	for _, file := range []string{"Time.Lapse.2014.mkv", "Time.Lapse.2014.1080p.mkv"} {
		if q := ParseRaw(file); q.Kind != KindMovie {
			t.Fatalf("ParseRaw(%q) Kind = %d, want KindMovie (%+v)", file, q.Kind, q)
		}
	}
	if q := ParseRaw("5) building harborline - time lapse video.mkv"); q.Kind != KindUnknown {
		t.Fatalf("ParseRaw(time lapse video extras) Kind = %d, want KindUnknown (%+v)", q.Kind, q)
	}
	if q := ParseRaw("Time.Lapse.Video.mkv"); q.Kind != KindUnknown {
		t.Fatalf("ParseRaw(%q) Kind = %d, want KindUnknown (%+v)", "Time.Lapse.Video.mkv", q.Kind, q)
	}
}

// TestParseAmbiguousExtras states the four ambiguous classifications and why:
// each stays fetched because the only stem that would catch it is a single
// common word or a bare chapter number, both barred (lost coverage beats a
// wasted lookup).
func TestParseAmbiguousExtras(t *testing.T) {
	cases := []struct {
		file string
		why  string
	}{
		// Chapter-styled, but no numeric-prefix stem was adopted: it is
		// indistinguishable from real chapter-titled content.
		{"2) harlan marsh as tom vane.mkv", "bare chapter number, no stem"},
		// "action" alone would nuke real action-genre titles.
		{"action - the drifters.mkv", "single common word, barred"},
		// No phrase stem without collision risk ("within"/"without" are common).
		{"within and without with harlan marsh.mkv", "no safe phrase stem"},
		// "short" alone would kill real short-film titles.
		{"short - the causeway.mkv", "single common word, barred"},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			q := ParseRaw(tc.file)
			if q.Kind == KindUnknown {
				t.Fatalf("ParseRaw(%q) = KindUnknown (%s), want fetched (%+v)", tc.file, tc.why, q)
			}
		})
	}
}
