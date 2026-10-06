package presence

import (
	"fmt"
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
	idle := BuildActivity("PotPlayer", &Action{})
	if idle.Type != "PLAYING" || idle.Details != "Browsing" || idle.State != "Idle" ||
		idle.LargeImage != "logo" || idle.SmallImage != "stop" || idle.HasStart {
		t.Fatalf("idle card = %+v", idle)
	}
	if a := BuildActivity(" - PotPlayer", &Action{}); a.Details != "Browsing" {
		t.Fatalf("empty media should be the idle card, got %+v", a)
	}

	// Episode numbers are Integer.parseInt'd, so leading zeros drop: S01 -> S1.
	raw := "Show.S01E02.Pilot.1080p.mkv - PotPlayer"
	run := BuildActivity(raw, &Action{Running: true, HasStart: true, Start: 12345, CT: 1000})
	if run.Details != "Show" || run.State != `S1 E2 "Pilot"` || run.SmallImage != "play" || !run.HasStart || run.Start != 12345 {
		t.Fatalf("running card = %+v", run)
	}
	paused := BuildActivity(raw, &Action{Paused: true, CT: 83000})
	if paused.State != `S1 E2 "Pilot" - Paused` || paused.SmallImage != "pause" || paused.HasStart {
		t.Fatalf("paused card = %+v", paused)
	}
	// No episode context: the state is the marker alone, and there is no
	// user-visible "00:00" artifact from the removed position suffix.
	pausedMovie := BuildActivity("Movie.mkv - PotPlayer", &Action{Paused: true, CT: 83000})
	if pausedMovie.State != "Paused" || pausedMovie.SmallImage != "pause" {
		t.Fatalf("paused movie card = %+v", pausedMovie)
	}
	stopped := BuildActivity(raw, &Action{CT: 0})
	if stopped.SmallImage != "stop" || stopped.State != `S1 E2 "Pilot"` {
		t.Fatalf("stopped card = %+v", stopped)
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
