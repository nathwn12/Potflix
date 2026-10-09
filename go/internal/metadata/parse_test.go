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
		{"op theme", "Naruto", "OP 1", "", Query{Kind: KindUnknown, Title: "Naruto"}},
		{"ed lowercase theme", "Naruto", "ed 12", "", Query{Kind: KindUnknown, Title: "Naruto"}},
		{"movie", "The Matrix", "", "", Query{Kind: KindMovie, Title: "The Matrix"}},
		{"year stripped", "Breaking Bad (2008)", "S1 E1", "", Query{Kind: KindSeries, Title: "Breaking Bad", Year: 2008, Season: 1, Episode: 1}},
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
// explicitly observed bonus-variant negative-cache keys. 18 phrase-stem
// extras are KindUnknown (each saves one ~3000 ms wasted iTunes lookup); the
// other 16 stay KindMovie because no phrase stem covers them without risking
// a real title - single common words ("short", "action", "becoming"),
// locations ("michigan, usa"), and bare chapter numbers ("2) ...") are
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
		{"behind the story - bangkok tour with chow.mkv", KindUnknown},
		{"behind the story - the comedy rhythm of todd phillips.mkv", KindUnknown},
		{"inferno - the making of the expendables.mkv", KindUnknown},
		{"executing the plan - the making of escape plan.mkv", KindUnknown},
		{"in search of zombieland.mkv", KindUnknown},
		{"how to survive the z apocalypse.mkv", KindUnknown},
		{"meet the cast.mkv", KindUnknown},
		{"previz - future concepts.mkv", KindUnknown},
		{"visual effects progression scenes.mkv", KindUnknown},
		{"replacing zach - the secret auditions.mkv", KindUnknown},
		{"4) the production design.mkv", KindUnknown},
		{"5) building mcdonald's - time lapse video.mkv", KindUnknown},
		{"nonlinear thinking - the editing process.mkv", KindMovie},
		{"becoming joker.mkv", KindMovie},
		{"how to draw.mkv", KindMovie},
		{"map of destruction.mkv", KindMovie},
		{"pushing the limits.mkv", KindMovie},
		{"extraction mode.mkv", KindMovie},
		{"jumpstart - david's story.mkv", KindMovie},
		{"the fashion of 1969.mkv", KindMovie},
		{"swarmatron.mkv", KindMovie},
		{"kung fu talking.mkv", KindMovie},
		{"michigan, usa.mkv", KindMovie},
		{"new york, usa.mkv", KindMovie},
		{"tokyo, japan.mkv", KindMovie},
		{"the doppelgangers.mkv", KindMovie},
		{"2) gary announcement.mkv", KindMovie},
		{"3) the mcdonald brothers.mkv", KindMovie},
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

// TestParseExtrasNoRealTitleCollision is the collision guard: the real titles
// observed in the same cache must still parse as Movie/Series, never Unknown.
// Escape Plan 2 pins KindSeries: titleclean splits the sequel digit into an
// E2 absolute tag (pre-existing behaviour, unrelated to extraRe) - the point
// is it is fetched, never skipped.
func TestParseExtrasNoRealTitleCollision(t *testing.T) {
	cases := []struct {
		file string
		want Kind
	}{
		{"21.Jump.Street.2012.2160p.mkv", KindMovie},
		{"Fight.Club.1999.mkv", KindMovie},
		{"Shutter.Island.2010.mkv", KindMovie},
		{"The.Revenant.2015.mkv", KindMovie},
		{"Contagion.2011.mkv", KindMovie},
		{"Django.Unchained.2012.mkv", KindMovie},
		{"Drive.2011.1080p.mkv", KindMovie},
		{"Blade.Runner.2049.2017.mkv", KindMovie},
		{"The.Hunger.Games.2012.mkv", KindMovie},
		{"We.re.the.Millers.2013.mkv", KindMovie},
		{"Kick-Ass.mkv", KindMovie},
		{"Ratatouille.mkv", KindMovie},
		{"Pacific.Rim.mkv", KindMovie},
		{"Lolita.mkv", KindMovie},
		{"Joker.2019.1080p.mkv", KindMovie},
		{"Focus.2015.1080p.mkv", KindMovie},
		{"Zombieland.Double.Tap.2019.mkv", KindMovie},
		{"Escape.Plan.2.Hades.2018.mkv", KindSeries},
		{"The.Expendables.3.2014.mkv", KindMovie},
		{"Welcome.to.Derry.S01E01.mkv", KindSeries},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			if q := ParseRaw(tc.file); q.Kind != tc.want {
				t.Fatalf("ParseRaw(%q) Kind = %d, want %d (%+v)", tc.file, q.Kind, tc.want, q)
			}
		})
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
		{"2) michael keaton as ray kroc.mkv", "bare chapter number, no stem"},
		// "action" alone would nuke real action-genre titles.
		{"action - the expendables.mkv", "single common word, barred"},
		// No phrase stem without collision risk ("within"/"without" are common).
		{"within and without with tobey maguire.mkv", "no safe phrase stem"},
		// "short" alone would kill real short-film titles.
		{"short - the tunnel.mkv", "single common word, barred"},
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
