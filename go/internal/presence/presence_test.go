package presence

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"

	"potflix/internal/player"
	"potflix/internal/titleclean"
)

// presence_test.go - table-driven tests over the transition matrix, plus the
// debounce deviation and the payload helpers. Expectations are derived from the
// port contract (section 4) and the Java PresenceTracker ported in presence.go.

type sample struct {
	title  string
	status player.PlayStatus
	ct     int64
	now    int64
}

type want struct {
	tag      string // "" means no emit
	start    int64
	hasStart bool
}

func none() want            { return want{} }
func runW(start int64) want { return want{tag: "RUN", start: start, hasStart: true} }
func pauseW() want          { return want{tag: "PAUSE"} }
func stopW() want           { return want{tag: "STOP"} }

// decisionLine reproduces the contract's `decision ...` log line (events.go's
// eventDecision shape). The tracker itself does not log; this is the test's
// rendering of each poll, so the matrix produces a readable decision log.
func decisionLine(s sample, a *Action) string {
	if a == nil {
		return fmt.Sprintf("decision ms=%d st=%s ct=%d title=[%s] action=null", s.now, s.status, s.ct, s.title)
	}
	return fmt.Sprintf("decision ms=%d st=%s ct=%d title=[%s] action=%s running=%t paused=%t hasStart=%t start=%d suf=%s ct=%d",
		s.now, s.status, s.ct, s.title, a.Tag(), a.Running, a.Paused, a.HasStart, a.Start, a.Suffix, a.CT)
}

func runSequence(t *testing.T, name string, samples []sample, wants []want) {
	t.Helper()
	if len(samples) != len(wants) {
		t.Fatalf("%s: %d samples vs %d wants", name, len(samples), len(wants))
	}
	tr := &Tracker{}
	lines := make([]string, 0, len(samples))
	for i, s := range samples {
		a := tr.Poll(s.title, s.status, s.ct, s.now)
		lines = append(lines, decisionLine(s, a))

		gotTag := a.Tag()
		wantTag := wants[i].tag
		if wantTag == "" {
			wantTag = "null"
		}
		if gotTag != wantTag {
			t.Errorf("%s[%d]: tag=%s want=%s", name, i, gotTag, wantTag)
			continue
		}
		if a == nil {
			continue
		}
		if a.HasStart != wants[i].hasStart || a.Start != wants[i].start {
			t.Errorf("%s[%d]: hasStart=%t start=%d want hasStart=%t start=%d",
				name, i, a.HasStart, a.Start, wants[i].hasStart, wants[i].start)
		}
	}
	t.Logf("%s decision log:\n%s", name, strings.Join(lines, "\n"))
}

func TestTransitionMatrix(t *testing.T) {
	const tA = "A - PotPlayer"

	t.Run("running-advance-pause-stop", func(t *testing.T) {
		runSequence(t, "A", []sample{
			{tA, player.StatusRunning, 1000, 100000},
			{tA, player.StatusRunning, 1250, 100250},
			{tA, player.StatusPaused, 1250, 100500},
			{tA, player.StatusPaused, 1250, 100750},
			{tA, player.StatusRunning, 1260, 101000},
			{tA, player.StatusStopped, 0, 101250},
			{tA, player.StatusStopped, 0, 101500},
		}, []want{
			// Index 2 is a Running->Paused report while ct still moves in the
			// window: flap suppression keeps it Running, so Pause lands on 3.
			runW(99000), none(), none(), pauseW(), runW(99740), stopW(), none(),
		})
	})

	t.Run("seek-and-debounce", func(t *testing.T) {
		const tB = "B - PotPlayer"
		runSequence(t, "B", []sample{
			{tB, player.StatusRunning, 1000, 200000},
			{tB, player.StatusRunning, 5000, 200250},
			{tB, player.StatusRunning, 9000, 201000},
			{tB, player.StatusRunning, 9200, 201250},
		}, []want{
			runW(199000), none(), runW(192000), none(),
		})
	})

	t.Run("flap-suppression", func(t *testing.T) {
		const tC = "C - PotPlayer"
		runSequence(t, "C", []sample{
			{tC, player.StatusRunning, 1000, 300000},
			{tC, player.StatusRunning, 1600, 300250},
			{tC, player.StatusPaused, 2200, 300500}, // glitch: still moving -> Running
			{tC, player.StatusPaused, 2200, 300750}, // still moving -> Running
			{tC, player.StatusPaused, 2200, 301000}, // window flushed -> real Pause
			{tC, player.StatusPaused, 2200, 301250},
			{tC, player.StatusPaused, 4000, 301500}, // paused seek
			{tC, player.StatusPaused, 4000, 301750},
		}, []want{
			runW(299000), none(), none(), none(), pauseW(), none(), pauseW(), none(),
		})
	})

	t.Run("title-change-reset", func(t *testing.T) {
		const tD = "D - PotPlayer"
		const tE = "E - PotPlayer"
		runSequence(t, "D", []sample{
			{tD, player.StatusRunning, 1000, 400000},
			{tE, player.StatusRunning, 500, 400250},
			{tE, player.StatusRunning, 750, 400500},
		}, []want{
			runW(399000), runW(399750), none(),
		})
	})

	t.Run("undefined-carries-nothing", func(t *testing.T) {
		const tE = "E - PotPlayer"
		runSequence(t, "E", []sample{
			{tE, player.StatusRunning, 1000, 500000},
			{tE, player.StatusUndefined, 0, 500250},
			{tE, player.StatusUndefined, 0, 500500},
			{tE, player.StatusRunning, 1250, 500750}, // unchanged: lastStatus still Running
			{tE, player.StatusStopped, 0, 501000},    // ct<=0 -> no movement, real transition
			{tE, player.StatusStopped, 0, 501250},
		}, []want{
			runW(499000), none(), none(), none(), stopW(), none(),
		})
	})

	t.Run("undefined-first-then-running", func(t *testing.T) {
		const tF = "F - PotPlayer"
		runSequence(t, "F", []sample{
			{tF, player.StatusUndefined, 0, 600000},
			{tF, player.StatusRunning, 1000, 600250},
			{tF, player.StatusStopped, 0, 600500},
			{tF, player.StatusStopped, 0, 600750},
		}, []want{
			none(), runW(599250), stopW(), none(),
		})
	})

	t.Run("paused-ct-not-ok", func(t *testing.T) {
		const tG = "G - PotPlayer"
		runSequence(t, "G", []sample{
			{tG, player.StatusPaused, 0, 700000},
			{tG, player.StatusPaused, 0, 700250},
			{tG, player.StatusPaused, 1000, 700500},
		}, []want{
			none(), none(), pauseW(),
		})
	})

	t.Run("initial-stopped", func(t *testing.T) {
		const tH = "H - PotPlayer"
		runSequence(t, "H", []sample{
			{tH, player.StatusStopped, 0, 800000},
			{tH, player.StatusStopped, 0, 800250},
		}, []want{
			stopW(), none(),
		})
	})
}

func TestRunDebounce(t *testing.T) {
	// A seek inside the ~1 s window, same title, is HELD (trailing edge) and
	// lands when the window expires - it is no longer dropped forever, which is
	// what froze the card on a stale anchor.
	const x = "X - PotPlayer"
	tr := &Tracker{}
	if a := tr.Poll(x, player.StatusRunning, 1000, 0); a == nil || !a.Running {
		t.Fatalf("first RUN should emit, got %v", a)
	}
	if a := tr.Poll(x, player.StatusRunning, 5000, 200); a != nil {
		t.Fatalf("seek within 1s same title should be held, got %+v", a)
	}
	if !tr.PendingDebounce() {
		t.Fatalf("held seek should report PendingDebounce")
	}
	if a := tr.Poll(x, player.StatusRunning, 5000, 900); a != nil {
		t.Fatalf("seek still inside the window should be held, got %+v", a)
	}
	// The window expires: the held seek lands with a fresh anchor.
	if a := tr.Poll(x, player.StatusRunning, 5200, 1001); a == nil || !a.Running {
		t.Fatalf("held seek should land after the window, got %v", a)
	} else if a.Start != 1001-5200 {
		t.Fatalf("trailing-edge anchor start=%d, want %d", a.Start, 1001-5200)
	}
	if tr.PendingDebounce() {
		t.Fatalf("pending flag should clear after the held seek lands")
	}

	// A title change bypasses the debounce even inside the window.
	if a := tr.Poll("Y - PotPlayer", player.StatusRunning, 500, 1050); a == nil || !a.Running {
		t.Fatalf("title change should bypass debounce, got %v", a)
	}

	// After the window, a seek with the same title emits again.
	const z = "Z - PotPlayer"
	tr2 := &Tracker{}
	tr2.Poll(z, player.StatusRunning, 1000, 0)
	if a := tr2.Poll(z, player.StatusRunning, 3000, 999); a != nil {
		t.Fatalf("seek at +999ms should be held, got %+v", a)
	}
	if a := tr2.Poll(z, player.StatusRunning, 5000, 1200); a == nil || !a.Running {
		t.Fatalf("seek after the window should emit, got %v", a)
	}
}

// F2: the trailing-edge debounce runs on the monotonic clock too, so a backward
// wall-clock step cannot strand a held seek until the wall clock catches up.
func TestRunDebounceBackwardClockStep(t *testing.T) {
	const x = "X - PotPlayer"
	tr := &Tracker{}
	if a := tr.Poll(x, player.StatusRunning, 1000, 100000); a == nil || !a.Running {
		t.Fatalf("first RUN should emit, got %v", a)
	}
	// A seek 200 ms later is held inside the ~1 s window.
	if a := tr.Poll(x, player.StatusRunning, 5000, 100200); a != nil {
		t.Fatalf("seek inside the window should be held, got %+v", a)
	}
	// The wall clock steps back 55 s: the held seek is still inside the window.
	if a := tr.Poll(x, player.StatusRunning, 5000, 45200); a != nil {
		t.Fatalf("seek should still be held right after the step, got %+v", a)
	}
	// ~1 s of monotonic interval later it lands, rather than waiting 55 s.
	if a := tr.Poll(x, player.StatusRunning, 5000, 46300); a == nil || !a.Running {
		t.Fatalf("held seek stranded after a backward step, got %v", a)
	}
}

func TestPausedNotDebounced(t *testing.T) {
	const s = "T - PotPlayer"
	tr := &Tracker{}
	if a := tr.Poll(s, player.StatusPaused, 1000, 0); a == nil || !a.Paused {
		t.Fatalf("initial pause should emit, got %v", a)
	}
	if a := tr.Poll(s, player.StatusPaused, 4000, 100); a == nil || !a.Paused {
		t.Fatalf("paused seek should emit (no debounce), got %v", a)
	}
}

func TestFmt(t *testing.T) {
	cases := []struct {
		ms   int64
		want string
	}{
		{0, "00:00"}, {999, "00:00"}, {1000, "00:01"}, {59000, "00:59"},
		{60000, "01:00"}, {61000, "01:01"}, {3599000, "59:59"},
		{3600000, "1:00:00"}, {3661000, "1:01:01"}, {7322000, "2:02:02"},
	}
	for _, c := range cases {
		if got := Fmt(c.ms); got != c.want {
			t.Errorf("Fmt(%d) = %q, want %q", c.ms, got, c.want)
		}
	}
}

func TestWithStateTime(t *testing.T) {
	cases := []struct {
		context, suffix, want string
	}{
		{"S01 E01", " - 01:23", "S01 E01 - 01:23"},
		{"", " - 01:23", "01:23"},
		{"", "", ""},
		{"ctx", "", "ctx"},
		{"", " - ", ""},
		{"", "  -  01:23", "01:23"},
		{"", "\t-\t01:00", "01:00"},
	}
	for _, c := range cases {
		if got := WithStateTime(c.context, c.suffix); got != c.want {
			t.Errorf("WithStateTime(%q,%q) = %q, want %q", c.context, c.suffix, got, c.want)
		}
	}

	long := strings.Repeat("x", 200)
	got := WithStateTime(long, " - 00:01")
	if got != titleclean.Trunc(long, 128-len(" - 00:01"))+" - 00:01" {
		t.Errorf("long context should be truncated to leave room for the suffix: %q", got)
	}
	if !strings.HasSuffix(got, " - 00:01") {
		t.Errorf("the suffix must survive truncation: %q", got)
	}
	if n := javaUnits(got); n > 128 {
		t.Errorf("state length %d exceeds the 128 limit: %q", n, got)
	}
}

// javaUnits is String.length(): UTF-16 code units, the unit the Java-exact
// titleclean.Trunc limit is measured in.
func javaUnits(s string) int { return len(utf16.Encode([]rune(s))) }

// F3: the paused marker must never be truncated. A 120-unit context plus the
// 9-unit " - Paused" marker is 129 units; the old concatenate-then-truncate
// rendered the state as "... - Paus...". The budgeted context keeps the marker
// whole and the total within 128.
func TestPausedMarkerSurvivesContextCap(t *testing.T) {
	ctx := strings.Repeat("x", 120)
	got := WithStateTime(ctx, pausedMarker)
	if !strings.HasSuffix(got, pausedMarker) {
		t.Fatalf("paused marker was truncated: %q", got)
	}
	if n := javaUnits(got); n > 128 {
		t.Fatalf("state length %d exceeds the 128 limit: %q", n, got)
	}
}

func TestBuildActivity(t *testing.T) {
	// Netflix-style mapping: the header (Name) is the media title, the first
	// body line (Details) is the episode name when known, else the episode
	// tag, else the title, and the second body line (State) is the episode
	// tag when it adds information, else empty.
	const tvRaw = "Show.S01E02.Pilot.1080p.mkv - PotPlayer"
	const tvTagOnlyRaw = "Show.S01E02.1080p.mkv - PotPlayer"
	const movieRaw = "Movie.mkv - PotPlayer"
	cases := []struct {
		name  string
		raw   string
		act   *Action
		want  Activity
		start int64
	}{
		{
			name: "movie running",
			raw:  movieRaw,
			act:  &Action{Running: true, HasStart: true, Start: 12345, CT: 1000},
			want: Activity{Type: "PLAYING", Name: "Movie", Details: "Movie", State: "",
				LargeImage: "logo", LargeText: "PotFlix", SmallImage: "play", SmallText: "Playing",
				HasStart: true},
			start: 12345,
		},
		{
			name: "tv with episode name and tag",
			raw:  tvRaw,
			act:  &Action{Running: true, HasStart: true, Start: 12345, CT: 1000},
			want: Activity{Type: "PLAYING", Name: "Show", Details: "Pilot", State: "S1 E2",
				LargeImage: "logo", LargeText: "PotFlix", SmallImage: "play", SmallText: "Playing",
				HasStart: true},
			start: 12345,
		},
		{
			name: "tv with tag only",
			raw:  tvTagOnlyRaw,
			act:  &Action{Running: true, HasStart: true, Start: 12345, CT: 1000},
			want: Activity{Type: "PLAYING", Name: "Show", Details: "S1 E2", State: "",
				LargeImage: "logo", LargeText: "PotFlix", SmallImage: "play", SmallText: "Playing",
				HasStart: true},
			start: 12345,
		},
		{
			name: "tv paused",
			raw:  tvRaw,
			act:  &Action{Paused: true, CT: 83000},
			want: Activity{Type: "PLAYING", Name: "Show", Details: "Pilot", State: `S1 E2 - Paused`,
				LargeImage: "logo", LargeText: "PotFlix", SmallImage: "pause", SmallText: "Paused"},
		},
		{
			name: "movie paused has marker alone",
			raw:  movieRaw,
			act:  &Action{Paused: true, CT: 83000},
			want: Activity{Type: "PLAYING", Name: "Movie", Details: "Movie", State: "Paused",
				LargeImage: "logo", LargeText: "PotFlix", SmallImage: "pause", SmallText: "Paused"},
		},
		{
			name: "tv stopped",
			raw:  tvRaw,
			act:  &Action{CT: 0},
			want: Activity{Type: "PLAYING", Name: "Show", Details: "Pilot", State: "S1 E2",
				LargeImage: "logo", LargeText: "PotFlix", SmallImage: "stop", SmallText: "Stopped"},
		},
		{
			name: "idle",
			raw:  "PotPlayer",
			act:  &Action{},
			want: Activity{Type: "PLAYING", Details: "Browsing", State: "Idle",
				LargeImage: "logo", LargeText: "PotFlix", SmallImage: "stop", SmallText: "Idle"},
		},
		{
			name: "empty media is idle",
			raw:  " - PotPlayer",
			act:  &Action{},
			want: Activity{Type: "PLAYING", Details: "Browsing", State: "Idle",
				LargeImage: "logo", LargeText: "PotFlix", SmallImage: "stop", SmallText: "Idle"},
		},
		{
			// No episode name, and the over-128-unit title fills the
			// details line on its own: the state line must stay empty
			// rather than repeating the truncated details.
			name: "long title without episode context emits no state",
			raw:  strings.Repeat("A", 200) + ".mkv - PotPlayer",
			act:  &Action{Running: true, HasStart: true, Start: 12345, CT: 1000},
			want: Activity{Type: "PLAYING", Name: strings.Repeat("A", 99) + string([]rune{0x2026}), Details: strings.Repeat("A", 99) + string([]rune{0x2026}), State: "",
				LargeImage: "logo", LargeText: "PotFlix", SmallImage: "play", SmallText: "Playing",
				HasStart: true},
			start: 12345,
		},
	}
	for _, c := range cases {
		got := BuildActivity(c.raw, c.act)
		want := c.want
		want.Start = c.start
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: card = %+v, want %+v", c.name, got, want)
		}
	}
}

func TestActionTag(t *testing.T) {
	if (*Action)(nil).Tag() != "null" {
		t.Error("nil action should tag as null")
	}
	if (&Action{Running: true}).Tag() != "RUN" {
		t.Error("running action should tag RUN")
	}
	if (&Action{Paused: true}).Tag() != "PAUSE" {
		t.Error("paused action should tag PAUSE")
	}
	if (&Action{}).Tag() != "STOP" {
		t.Error("default action should tag STOP")
	}
}

func TestBuildActivityResolved(t *testing.T) {
	const tvRaw = "Show.S01E02.Pilot.1080p.mkv - PotPlayer"
	act := &Action{Running: true, HasStart: true, Start: 12345, CT: 1000}

	t.Run("zero overrides match BuildActivity", func(t *testing.T) {
		if got, want := BuildActivityResolved(tvRaw, act, Resolved{}), BuildActivity(tvRaw, act); !reflect.DeepEqual(got, want) {
			t.Errorf("Resolved{} card = %+v, want %+v", got, want)
		}
	})

	t.Run("series override changes Name and LargeText", func(t *testing.T) {
		base := BuildActivity(tvRaw, act)
		got := BuildActivityResolved(tvRaw, act, Resolved{Series: "Canonical Show"})
		if got.Name != titleclean.Trunc("Canonical Show", 128) {
			t.Errorf("Name = %q, want %q", got.Name, titleclean.Trunc("Canonical Show", 128))
		}
		if got.LargeText != titleclean.Trunc("Canonical Show", 128) {
			t.Errorf("LargeText = %q, want %q", got.LargeText, titleclean.Trunc("Canonical Show", 128))
		}
		base.Name = got.Name
		base.LargeText = got.LargeText
		if !reflect.DeepEqual(got, base) {
			t.Errorf("only Name and LargeText should change: got %+v, want %+v", got, base)
		}
	})

	t.Run("episode name override changes only Details and keeps State tag", func(t *testing.T) {
		base := BuildActivity(tvRaw, act)
		got := BuildActivityResolved(tvRaw, act, Resolved{EpisodeName: "Canonical Episode"})
		if got.Details != titleclean.Trunc("Canonical Episode", 128) {
			t.Errorf("Details = %q, want %q", got.Details, titleclean.Trunc("Canonical Episode", 128))
		}
		if got.State != base.State {
			t.Errorf("State = %q, want tag %q", got.State, base.State)
		}
		if base.State != "S1 E2" {
			t.Errorf("base State = %q, want tag %q", base.State, "S1 E2")
		}
		base.Details = got.Details
		if !reflect.DeepEqual(got, base) {
			t.Errorf("only Details should change: got %+v, want %+v", got, base)
		}
	})

	t.Run("overrides truncate at 128", func(t *testing.T) {
		long := strings.Repeat("y", 200)
		got := BuildActivityResolved(tvRaw, act, Resolved{Series: long, EpisodeName: long})
		if want := titleclean.Trunc(long, 128); got.Name != want || got.Details != want {
			t.Errorf("Name=%q Details=%q, want both %q", got.Name, got.Details, want)
		}
		if n := javaUnits(got.Name); n > 128 {
			t.Errorf("Name length %d exceeds 128: %q", n, got.Name)
		}
		if n := javaUnits(got.Details); n > 128 {
			t.Errorf("Details length %d exceeds 128: %q", n, got.Details)
		}
	})
}

func TestBuildActivityResolvedRich(t *testing.T) {
	const tvRaw = "Show.S01E02.Pilot.1080p.mkv - PotPlayer"
	const tvTagOnlyRaw = "Show.S01E02.1080p.mkv - PotPlayer"
	const movieRaw = "Movie.mkv - PotPlayer"
	runAct := &Action{Running: true, HasStart: true, Start: 12345, CT: 1000}

	t.Run("artwork https URL becomes LargeImage", func(t *testing.T) {
		const art = "https://example.com/poster.jpg"
		got := BuildActivityResolved(tvRaw, runAct, Resolved{Artwork: art})
		if got.LargeImage != art {
			t.Errorf("LargeImage = %q, want %q", got.LargeImage, art)
		}
	})

	t.Run("artwork empty or invalid keeps logo", func(t *testing.T) {
		for _, art := range []string{"", "logo", "ftp://example.com/x.jpg", "example.com/x.jpg", " /https://example.com/x.jpg"} {
			got := BuildActivityResolved(tvRaw, runAct, Resolved{Artwork: art})
			if got.LargeImage != "logo" {
				t.Errorf("Artwork %q: LargeImage = %q, want %q", art, got.LargeImage, "logo")
			}
			if got.LargeImage == "" {
				t.Errorf("Artwork %q: LargeImage must never be empty", art)
			}
		}
	})

	t.Run("series episode tag synopsis semantics", func(t *testing.T) {
		got := BuildActivityResolved(tvRaw, runAct, Resolved{
			Series:      "Canonical Series",
			EpisodeName: "Canonical Episode",
			Synopsis:    "A brave synopsis",
		})
		if want := titleclean.Trunc("Canonical Series", 128); got.Name != want {
			t.Errorf("Name = %q, want %q", got.Name, want)
		}
		if want := titleclean.Trunc("Canonical Episode", 128); got.Details != want {
			t.Errorf("Details = %q, want %q", got.Details, want)
		}
		if want := rpText("S1 E2 \u00b7 A brave synopsis", 128); got.State != want {
			t.Errorf("State = %q, want %q", got.State, want)
		}
		if want := "S1 E2 \u00b7 A brave synopsis"; got.State != want {
			t.Errorf("State = %q, want exact %q", got.State, want)
		}
		if want := titleclean.Trunc("Canonical Series", 128); got.LargeText != want {
			t.Errorf("LargeText = %q, want %q", got.LargeText, want)
		}
	})

	t.Run("synopsis only becomes state", func(t *testing.T) {
		got := BuildActivityResolved(movieRaw, runAct, Resolved{Synopsis: "Just a synopsis"})
		if got.State != "Just a synopsis" {
			t.Errorf("State = %q, want %q", got.State, "Just a synopsis")
		}
	})

	t.Run("tag only becomes state", func(t *testing.T) {
		got := BuildActivityResolved(tvTagOnlyRaw, runAct, Resolved{})
		if got.State != "S1 E2" {
			t.Errorf("State = %q, want %q", got.State, "S1 E2")
		}
	})

	t.Run("long synopsis truncated to 128", func(t *testing.T) {
		long := strings.Repeat("s", 200)
		got := BuildActivityResolved(movieRaw, runAct, Resolved{Synopsis: long})
		if want := rpText(long, 128); got.State != want {
			t.Errorf("State = %q, want %q", got.State, want)
		}
		if n := javaUnits(got.State); n > 128 {
			t.Errorf("State length %d exceeds 128: %q", n, got.State)
		}
	})
}

func TestRpText(t *testing.T) {
	if got, want := rpText("a\n\nb  c", 128), "a b c"; got != want {
		t.Errorf("rpText collapse = %q, want %q", got, want)
	}
	if got, want := rpText("eps1.1_ones-and-zer0es.mpeg", 128), "eps1.1_ones-and-zer0es.mpeg"; got != want {
		t.Errorf("rpText must preserve underscores/dots/dashes: = %q, want %q", got, want)
	}
	if got, want := rpText("a\tb\rc", 128), "a b c"; got != want {
		t.Errorf("rpText tabs/newlines = %q, want %q", got, want)
	}
	if got := rpText("a\x00b\x7Fc", 128); got != "abc" {
		t.Errorf("rpText C0 strip = %q, want %q", got, "abc")
	}
	long := strings.Repeat("z", 200)
	if got := rpText(long, 128); got != titleclean.Trunc(long, 128) {
		t.Errorf("rpText trunc = %q, want %q", got, titleclean.Trunc(long, 128))
	}
}

func TestTrimWord(t *testing.T) {
	if got := trimWord("hello world", 128); got != "hello world" {
		t.Errorf("trimWord fits = %q, want %q", got, "hello world")
	}
	// A long synopsis of whole words must end on "..." at a word boundary,
	// never mid-word: the hard cut would leave a dangling "wo...".
	syn := strings.Repeat("word ", 40) + "tail"
	got := trimWord(syn, 128)
	if !strings.HasSuffix(got, "\u2026") {
		t.Fatalf("trimWord long = %q, want trailing ellipsis", got)
	}
	if strings.HasSuffix(strings.TrimSuffix(got, "\u2026"), "wo") {
		t.Errorf("trimWord ended mid-word: %q", got)
	}
	if !strings.HasSuffix(strings.TrimSuffix(got, "\u2026"), "word") {
		t.Errorf("trimWord should end on a whole word: %q", got)
	}
	if n := javaUnits(got); n > 128 {
		t.Errorf("trimWord length %d exceeds 128: %q", n, got)
	}
	// No spaces: fall back to the hard cut.
	nospace := strings.Repeat("s", 200)
	if got, want := trimWord(nospace, 128), rpText(nospace, 128); got != want {
		t.Errorf("trimWord nospace = %q, want %q", got, want)
	}
	if got := trimWord("", 128); got != "" {
		t.Errorf("trimWord empty = %q, want empty", got)
	}
}

func TestBuildActivityResolvedMovie(t *testing.T) {
	const movieRaw = "Dune.Part.Two.2024.1080p.mkv - PotPlayer"
	runAct := &Action{Running: true, HasStart: true, Start: 12345, CT: 1000}

	t.Run("details carry Title (Year)", func(t *testing.T) {
		got := BuildActivityResolved(movieRaw, runAct, Resolved{
			Series: "Dune: Part Two", Year: 2024, IsMovie: true,
		})
		if want := titleclean.Trunc("Dune: Part Two (2024)", 128); got.Details != want {
			t.Errorf("Details = %q, want %q", got.Details, want)
		}
		if want := titleclean.Trunc("Dune: Part Two", 128); got.Name != want {
			t.Errorf("Name = %q, want %q", got.Name, want)
		}
	})

	t.Run("details without year are the bare title", func(t *testing.T) {
		got := BuildActivityResolved(movieRaw, runAct, Resolved{
			Series: "Dune: Part Two", IsMovie: true,
		})
		if want := titleclean.Trunc("Dune: Part Two", 128); got.Details != want {
			t.Errorf("Details = %q, want %q", got.Details, want)
		}
	})

	t.Run("state joins Genre Length synopsis", func(t *testing.T) {
		got := BuildActivityResolved(movieRaw, runAct, Resolved{
			Series: "Dune: Part Two", Year: 2024, IsMovie: true,
			Genres: "Sci-Fi", Length: "2h 46m", Synopsis: "Paul unites the Fremen",
		})
		if want := "Sci-Fi \u00b7 2h 46m \u00b7 Paul unites the Fremen"; got.State != want {
			t.Errorf("State = %q, want %q", got.State, want)
		}
	})

	t.Run("state skips empties", func(t *testing.T) {
		got := BuildActivityResolved(movieRaw, runAct, Resolved{
			Series: "Dune", IsMovie: true, Length: "2h 19m", Synopsis: "A quest",
		})
		if want := "2h 19m \u00b7 A quest"; got.State != want {
			t.Errorf("State = %q, want %q", got.State, want)
		}
	})

	t.Run("long movie state ends on ellipsis at a word boundary", func(t *testing.T) {
		syn := strings.Repeat("epic ", 40) + "finale"
		got := BuildActivityResolved(movieRaw, runAct, Resolved{
			Series: "Dune", IsMovie: true, Genres: "Adventure", Length: "2h 46m", Synopsis: syn,
		})
		if !strings.HasSuffix(got.State, "\u2026") {
			t.Fatalf("State = %q, want trailing ellipsis", got.State)
		}
		if n := javaUnits(got.State); n > 128 {
			t.Fatalf("State length %d exceeds 128: %q", n, got.State)
		}
		body := strings.TrimSuffix(got.State, "\u2026")
		if tail := body[strings.LastIndex(body, "\u00b7 ")+len("\u00b7 "):]; strings.Contains(tail, " ") {
			// The synopsis tail after the last separator still ends mid-list;
			// the key invariant is no partial word: last char before ellipsis
			// must not be a word prefix fragment. "epic" words are whole.
			if !strings.HasSuffix(body, "epic") && !strings.HasSuffix(body, "finale") {
				t.Errorf("State ends mid-word: %q", got.State)
			}
		}
	})

	t.Run("paused movie keeps the Paused marker", func(t *testing.T) {
		got := BuildActivityResolved(movieRaw, &Action{Paused: true, CT: 1000}, Resolved{
			Series: "Dune", Year: 2024, IsMovie: true,
			Genres: "Sci-Fi", Length: "2h 46m", Synopsis: "Paul unites the Fremen",
		})
		if !strings.HasSuffix(got.State, pausedMarker) {
			t.Fatalf("paused State = %q, want suffix %q", got.State, pausedMarker)
		}
		if n := javaUnits(got.State); n > 128 {
			t.Fatalf("paused State length %d exceeds 128: %q", n, got.State)
		}
	})

	t.Run("empty artwork stays logo", func(t *testing.T) {
		got := BuildActivityResolved(movieRaw, runAct, Resolved{Series: "Dune", IsMovie: true})
		if got.LargeImage != "logo" {
			t.Errorf("LargeImage = %q, want %q", got.LargeImage, "logo")
		}
	})
}

func TestBuildActivityResolvedSeriesState(t *testing.T) {
	const tvRaw = "Show.S01E02.Pilot.1080p.mkv - PotPlayer"
	runAct := &Action{Running: true, HasStart: true, Start: 12345, CT: 1000}
	got := BuildActivityResolved(tvRaw, runAct, Resolved{
		Series: "Canonical Series", EpisodeName: "Canonical Episode",
		Synopsis: "A brave new world unfolds",
	})
	if want := "S1 E2 \u00b7 A brave new world unfolds"; got.State != want {
		t.Errorf("State = %q, want %q", got.State, want)
	}
}

func TestBuildActivityResolvedButtons(t *testing.T) {
	const movieRaw = "Movie.mkv - PotPlayer"
	runAct := &Action{Running: true, HasStart: true, Start: 12345, CT: 1000}

	t.Run("capped at two", func(t *testing.T) {
		got := BuildActivityResolved(movieRaw, runAct, Resolved{
			Buttons: []Button{
				{Label: "Trailer", URL: "https://example.com/trailer"},
				{Label: "IMDb", URL: "https://example.com/imdb"},
				{Label: "Extra", URL: "https://example.com/extra"},
			},
		})
		if len(got.Buttons) != 2 {
			t.Fatalf("Buttons = %+v, want 2 kept", got.Buttons)
		}
		if got.Buttons[0].Label != "Trailer" || got.Buttons[1].Label != "IMDb" {
			t.Errorf("Buttons = %+v, want first two kept", got.Buttons)
		}
	})

	t.Run("http kept ftp and empty dropped", func(t *testing.T) {
		got := BuildActivityResolved(movieRaw, runAct, Resolved{
			Buttons: []Button{
				{Label: "", URL: "https://example.com/x"},
				{Label: "NoURL", URL: "ftp://example.com/x"},
				{Label: "Bare", URL: "example.com/x"},
				{Label: "OK", URL: "http://example.com/ok"},
			},
		})
		if len(got.Buttons) != 1 || got.Buttons[0].Label != "OK" {
			t.Fatalf("Buttons = %+v, want only the http one", got.Buttons)
		}
		if got.Buttons[0].URL != "http://example.com/ok" {
			t.Errorf("URL = %q, want kept verbatim", got.Buttons[0].URL)
		}
	})

	t.Run("label capped at 32", func(t *testing.T) {
		long := strings.Repeat("l", 100)
		got := BuildActivityResolved(movieRaw, runAct, Resolved{
			Buttons: []Button{{Label: long, URL: "https://example.com/x"}},
		})
		if len(got.Buttons) != 1 {
			t.Fatalf("Buttons = %+v, want 1", got.Buttons)
		}
		if n := javaUnits(got.Buttons[0].Label); n > 32 {
			t.Errorf("label length %d exceeds 32: %q", n, got.Buttons[0].Label)
		}
	})

	t.Run("BuildActivity carries no buttons", func(t *testing.T) {
		if got := BuildActivity(movieRaw, runAct); len(got.Buttons) != 0 {
			t.Errorf("BuildActivity.Buttons = %+v, want empty", got.Buttons)
		}
	})
}
