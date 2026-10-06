package player

import "testing"

// player_test.go - pure-logic tests: the window-title predicate and the
// status mapping. Both are contract-exact and need no live PotPlayer.

func TestMatchesTitle(t *testing.T) {
	cases := []struct {
		title string
		want  bool
	}{
		{"Some Movie - PotPlayer", true},
		{"PotPlayer", true},
		{"Show.S01E02.1080p - PotPlayer", true},
		{"- PotPlayer", false}, // lacks the leading space of " - PotPlayer"
		{"PotPlayer - PotPlayer", true},
		{"Some Movie", false},
		{"Some Movie - PotPlayer extra", false},
		{"PotPlayer2", false},
		{"", false},
		{"PotPlayer ", false},
	}
	for _, c := range cases {
		if got := Matches(c.title); got != c.want {
			t.Errorf("Matches(%q) = %v, want %v", c.title, got, c.want)
		}
	}
}

func TestMapPlayStatus(t *testing.T) {
	cases := []struct {
		raw  int
		want PlayStatus
	}{
		{-1, StatusStopped},
		{1, StatusPaused},
		{2, StatusRunning},
		{0, StatusUndefined},
		{3, StatusUndefined},
		{-2, StatusUndefined},
		{20486, StatusUndefined},
	}
	for _, c := range cases {
		if got := MapPlayStatus(c.raw); got != c.want {
			t.Errorf("MapPlayStatus(%d) = %s, want %s", c.raw, got, c.want)
		}
	}
}

func TestPlayStatusString(t *testing.T) {
	cases := []struct {
		st   PlayStatus
		want string
	}{
		{StatusStopped, "Stopped"},
		{StatusPaused, "Paused"},
		{StatusRunning, "Running"},
		{StatusUndefined, "Undefined"},
		{PlayStatus(99), "Undefined"},
	}
	for _, c := range cases {
		if got := c.st.String(); got != c.want {
			t.Errorf("PlayStatus(%d).String() = %q, want %q", c.st, got, c.want)
		}
	}
}
