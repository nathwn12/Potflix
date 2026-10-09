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
