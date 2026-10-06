package titleclean

import "testing"

// export_test.go - checks the package's public surface (Clean, Trunc).
// Expectations are JDK-derived, same as clean_test.go; no corpus data.

func TestCleanEntryPoint(t *testing.T) {
	// Java parses the numbers with Integer.parseInt, so S01 -> S1.
	c := Clean("Show.S01E02.Pilot.1080p.mkv")
	if c.Title != "Show" || c.EpisodeTag != "S1 E2" || c.EpisodeName != "Pilot" {
		t.Fatalf("Clean = %+v", c)
	}
	if c.Display != "Show \u2014 S1 E2 \"Pilot\"" {
		t.Fatalf("Display = %q", c.Display)
	}
}

func TestTruncExported(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"abcdef", 4, "abc\u2026"},
		{"", 5, ""},
		{"AAAA\U0001D54FB", 6, "AAAA?\u2026"}, // split surrogate pair -> '?'
	}
	for _, c := range cases {
		if got := Trunc(c.in, c.n); got != c.want {
			t.Errorf("Trunc(%q,%d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}
