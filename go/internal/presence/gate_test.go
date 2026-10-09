package presence

import "testing"

// gate_test.go - the send gate: unchanged steady state is suppressed with a
// reason; a changed card, a divergence re-anchor, a seek event, and the
// heartbeat all send; pause carries no timestamps; resume re-anchors.

func runningCard(details, state string) Activity {
	return Activity{Type: "PLAYING", Details: details, State: state, SmallImage: "play", HasStart: true}
}

func TestGateFirstSendThenSteadySuppressed(t *testing.T) {
	g := &Gate{}
	out, v := g.Decide(runningCard("Show", "S1 E1"), 1000, 60000, 100000, false, false)
	if !v.Send {
		t.Fatalf("first card should send, got %s", v)
	}
	if out.Start != 99000 || out.End != 159000 {
		t.Fatalf("anchors start=%d end=%d, want 99000/159000", out.Start, out.End)
	}
	// Steady playback: pos and now advance together, so the anchor holds.
	_, v = g.Decide(runningCard("Show", "S1 E1"), 2000, 60000, 101000, false, false)
	if v.Send {
		t.Fatalf("steady unchanged card should be suppressed, got %s", v)
	}
	if v.Reason != ReasonHeartbeatNotDue {
		t.Fatalf("steady suppression reason = %q, want %q", v.Reason, ReasonHeartbeatNotDue)
	}
}

func TestGateChangedDetailsSent(t *testing.T) {
	g := &Gate{}
	g.Decide(runningCard("Show", "S1 E1"), 1000, 60000, 100000, false, false)
	if _, v := g.Decide(runningCard("Other", "S1 E1"), 2000, 60000, 101000, false, false); !v.Send {
		t.Fatalf("changed details should send, got %s", v)
	}
}

func TestGateChangedStateSent(t *testing.T) {
	g := &Gate{}
	g.Decide(runningCard("Show", "S1 E1"), 1000, 60000, 100000, false, false)
	if _, v := g.Decide(runningCard("Show", "S1 E1 - 00:01"), 2000, 60000, 101000, false, false); !v.Send {
		t.Fatalf("changed state should send, got %s", v)
	}
}

func TestGateChangedLargeImageSent(t *testing.T) {
	g := &Gate{}
	base := runningCard("Show", "S1 E1")
	base.LargeImage = "logo"
	g.Decide(base, 1000, 60000, 100000, false, false)
	// Late metadata resolves only the cover: Name/Details/State identical.
	next := runningCard("Show", "S1 E1")
	next.LargeImage = "https://example.example/cover.jpg"
	if _, v := g.Decide(next, 2000, 60000, 101000, false, false); !v.Send {
		t.Fatalf("changed LargeImage should send, got %s", v)
	}
}

func TestGateChangedButtonsSent(t *testing.T) {
	g := &Gate{}
	base := runningCard("Show", "S1 E1")
	base.LargeImage = "logo"
	g.Decide(base, 1000, 60000, 100000, false, false)
	// Late metadata resolves only the buttons: Name/Details/State identical.
	next := runningCard("Show", "S1 E1")
	next.LargeImage = "logo"
	next.Buttons = []Button{{Label: "Info", URL: "https://example.example/info"}}
	if _, v := g.Decide(next, 2000, 60000, 101000, false, false); !v.Send {
		t.Fatalf("changed Buttons should send, got %s", v)
	}
}

func TestGateIdenticalArtAndButtonsSuppressed(t *testing.T) {
	g := &Gate{}
	card := func() Activity {
		c := runningCard("Show", "S1 E1")
		c.LargeImage = "https://example.example/cover.jpg"
		c.Buttons = []Button{{Label: "Info", URL: "https://example.example/info"}}
		return c
	}
	g.Decide(card(), 1000, 60000, 100000, false, false)
	// Truly identical repeat (same art + same ordered buttons) stays quiet.
	_, v := g.Decide(card(), 2000, 60000, 101000, false, false)
	if v.Send {
		t.Fatalf("identical art+buttons repeat should be suppressed, got %s", v)
	}
	if v.Reason != ReasonHeartbeatNotDue {
		t.Fatalf("identical suppression reason = %q, want %q", v.Reason, ReasonHeartbeatNotDue)
	}
}

func TestGateHeartbeatDueSent(t *testing.T) {
	g := &Gate{}
	g.Decide(runningCard("Show", "S1 E1"), 1000, 60000, 100000, false, false)
	// pos and now advance 30 s together: the anchor holds, the heartbeat fires.
	if _, v := g.Decide(runningCard("Show", "S1 E1"), 31000, 60000, 130000, false, false); !v.Send {
		t.Fatalf("heartbeat due should send, got %s", v)
	}
}

func TestGateDivergenceSent(t *testing.T) {
	// Forward: the position jumps +5 s while wall time advances 1 s.
	g := &Gate{}
	g.Decide(runningCard("Show", "S1 E1"), 1000, 60000, 100000, false, false)
	if _, v := g.Decide(runningCard("Show", "S1 E1"), 6000, 60000, 101000, false, false); !v.Send {
		t.Fatalf("forward divergence should send, got %s", v)
	}

	// Backward: the position jumps -5 s.
	g2 := &Gate{}
	g2.Decide(runningCard("Show", "S1 E1"), 6000, 60000, 100000, false, false)
	if _, v := g2.Decide(runningCard("Show", "S1 E1"), 1000, 60000, 101000, false, false); !v.Send {
		t.Fatalf("backward divergence should send, got %s", v)
	}
}

func TestGateDivergenceThrottled(t *testing.T) {
	g := &Gate{}
	g.Decide(runningCard("Show", "S1 E1"), 1000, 60000, 100000, false, false)
	if _, v := g.Decide(runningCard("Show", "S1 E1"), 6000, 60000, 101000, false, false); !v.Send {
		t.Fatalf("first divergence should send, got %s", v)
	}
	// A second divergence 1 s later is inside the throttle window.
	if _, v := g.Decide(runningCard("Show", "S1 E1"), 12000, 60000, 102000, false, false); v.Send {
		t.Fatalf("divergence inside the throttle should be suppressed, got %s", v)
	}
}

func TestGateEventSeekReanchors(t *testing.T) {
	g := &Gate{}
	g.Decide(runningCard("Show", "S1 E1"), 1000, 60000, 100000, false, false)
	out, v := g.Decide(runningCard("Show", "S1 E1"), 30000, 60000, 101000, true, false)
	if !v.Send {
		t.Fatalf("seek event should send, got %s", v)
	}
	if out.Start != 71000 {
		t.Fatalf("seek anchor start=%d, want 71000", out.Start)
	}
}

func TestGateUnchangedEventSuppressed(t *testing.T) {
	g := &Gate{}
	g.Decide(runningCard("Show", "S1 E1"), 1000, 60000, 100000, false, false)
	// An event with the same content and anchor is redundant.
	_, v := g.Decide(runningCard("Show", "S1 E1"), 2000, 60000, 101000, true, false)
	if v.Send || v.Reason != ReasonUnchanged {
		t.Fatalf("redundant event = %s, want suppressed:%s", v, ReasonUnchanged)
	}
}

func TestGateDebounceReason(t *testing.T) {
	g := &Gate{}
	g.Decide(runningCard("Show", "S1 E1"), 1000, 60000, 100000, false, false)
	_, v := g.Decide(runningCard("Show", "S1 E1"), 2000, 60000, 101000, false, true)
	if v.Send || v.Reason != ReasonDebounce {
		t.Fatalf("held debounce = %s, want suppressed:%s", v, ReasonDebounce)
	}
}

func TestGatePauseSendsParkedCard(t *testing.T) {
	// Pause sends a parked paused card and does NOT clear the activity. The card
	// keeps the cleaned details/state, small_image "pause", and a count-UP start
	// anchored at the pause moment (now), with no end.
	g := &Gate{}
	out, v := g.Decide(Activity{
		Type: "PLAYING", Details: "Show", State: `S1 E1 - Paused`, SmallImage: "pause",
	}, 1000, 60000, 100000, true, false)
	if !v.Send {
		t.Fatalf("pause must send a parked card, got %s", v)
	}
	if v.Clear {
		t.Fatalf("pause must NOT clear the activity")
	}
	if out.SmallImage != "pause" || !out.HasStart || out.Start != 100000 || out.End != 0 {
		t.Fatalf("paused card = %+v, want pause start=100000 end=0", out)
	}
}

func TestGatePauseNotDebounced(t *testing.T) {
	// Regression: a pause arriving while the tracker is holding a seek (the
	// debounced flag set) must still send the parked card. The pause rides on
	// the transition, not on the debounce-suppressed path.
	g := &Gate{}
	g.Decide(runningCard("Show", "S1 E1"), 1000, 60000, 100000, false, false)
	_, v := g.Decide(Activity{
		Type: "PLAYING", Details: "Show", State: `S1 E1 - Paused`, SmallImage: "pause",
	}, 1000, 60000, 100250, true, true)
	if !v.Send || v.Clear {
		t.Fatalf("pause was suppressed/debounced or cleared: %s (clear=%t)", v, v.Clear)
	}
}

func TestGatePausedSteadyStaysQuiet(t *testing.T) {
	// Once the parked card is sent, no further card goes out while paused - not
	// even the heartbeat, which would restart the pause timer. The activity is
	// never cleared.
	g := &Gate{}
	if _, v := g.Decide(Activity{
		Type: "PLAYING", Details: "Show", State: `S1 E1 - Paused`, SmallImage: "pause",
	}, 1000, 60000, 100000, true, false); !v.Send || v.Clear {
		t.Fatalf("first pause should send a parked card (no clear), got %s (clear=%t)", v, v.Clear)
	}
	for _, now := range []int64{100250, 120000, 140000, 200000} {
		_, v := g.Decide(Activity{
			Type: "PLAYING", Details: "Show", State: `S1 E1 - Paused`, SmallImage: "pause",
		}, 1000, 60000, now, false, false)
		if v.Send {
			t.Fatalf("paused poll at now=%d sent (clear=%t, reason=%s); want quiet", now, v.Clear, v.Reason)
		}
		if v.Reason != ReasonPaused {
			t.Fatalf("paused suppression reason = %q, want %q", v.Reason, ReasonPaused)
		}
	}
}

func TestGatePausedContentChangeResends(t *testing.T) {
	// The parked paused card is suppressed only while its content is unchanged.
	// A content change while paused (e.g. a different title/state) must re-send,
	// or the parked card stays stale forever. This pins BOTH halves: the
	// unchanged card sends exactly once and stays quiet, the changed card sends.
	g := &Gate{}
	paused := func(details, state string) Activity {
		return Activity{Type: "PLAYING", Details: details, State: state, SmallImage: "pause"}
	}
	if _, v := g.Decide(paused("ShowA", `S1 E1 - Paused`), 1000, 60000, 100000, true, false); !v.Send {
		t.Fatalf("first paused card should send, got %s", v)
	}
	// Unchanged: still quiet (the no-spam property).
	if _, v := g.Decide(paused("ShowA", `S1 E1 - Paused`), 1000, 60000, 100250, false, false); v.Send || v.Reason != ReasonPaused {
		t.Fatalf("unchanged paused card = %s, want suppressed:%s", v, ReasonPaused)
	}
	// Changed content: must re-send, anchored at the new pause moment.
	out, v := g.Decide(paused("ShowB", `S1 E2 - Paused`), 1000, 60000, 100500, false, false)
	if !v.Send {
		t.Fatalf("changed paused card should re-send, got %s", v)
	}
	if out.Details != "ShowB" || out.State != `S1 E2 - Paused` || !out.HasStart || out.Start != 100500 || out.End != 0 {
		t.Fatalf("changed paused card = %+v, want ShowB/S1 E2 - Paused start=100500 end=0", out)
	}
	// The re-sent card becomes the new parked reference: unchanged again stays
	// quiet.
	if _, v := g.Decide(paused("ShowB", `S1 E2 - Paused`), 1000, 60000, 100750, false, false); v.Send || v.Reason != ReasonPaused {
		t.Fatalf("re-parked unchanged card = %s, want suppressed:%s", v, ReasonPaused)
	}
}

func TestGateResumeReanchors(t *testing.T) {
	g := &Gate{}
	g.Decide(Activity{
		Type: "PLAYING", Details: "Show", State: `S1 E1 - Paused`, SmallImage: "pause",
	}, 1000, 60000, 100000, true, false)
	out, v := g.Decide(runningCard("Show", "S1 E1"), 1000, 60000, 101000, true, false)
	if !v.Send || v.Clear {
		t.Fatalf("resume should send a card (not a clear), got %s (clear=%t)", v, v.Clear)
	}
	if !out.HasStart || out.Start != 100000 || out.End != 160000 {
		t.Fatalf("resume anchors start=%d end=%d, want 100000/160000", out.Start, out.End)
	}
}

func TestGateEndOmittedWhenTotalUnknown(t *testing.T) {
	g := &Gate{}
	out, v := g.Decide(runningCard("Show", "S1 E1"), 1000, 0, 100000, false, false)
	if !v.Send {
		t.Fatalf("first card should send, got %s", v)
	}
	if out.End != 0 {
		t.Fatalf("end=%d, want omitted (0) when total is unknown", out.End)
	}
}

func TestGateForceNext(t *testing.T) {
	g := &Gate{}
	g.Decide(runningCard("Show", "S1 E1"), 1000, 60000, 100000, false, false)
	g.ForceNext()
	if _, v := g.Decide(runningCard("Show", "S1 E1"), 2000, 60000, 101000, false, false); !v.Send {
		t.Fatalf("forced Decide should send, got %s", v)
	}
}

// F1: a running card can be handed back by the tracker while the player read is
// not ready (ct<=0, presence.go). The heartbeat is due, but the not-ready read
// must keep the card's existing anchor instead of resetting the elapsed clock
// to 0:00 and emitting a full-duration countdown.
func TestGateNotReadyHeartbeatKeepsAnchor(t *testing.T) {
	g := &Gate{}
	out, v := g.Decide(runningCard("Show", "S1 E1"), 1000, 60000, 100000, false, false)
	if !v.Send || out.Start != 99000 {
		t.Fatalf("first send start=%d verdict=%s, want 99000/sent", out.Start, v)
	}
	// The card carries the last anchor; pos=0 marks the not-ready read.
	card := runningCard("Show", "S1 E1")
	card.Start = 99000
	out2, v2 := g.Decide(card, 0, 60000, 130000, false, false)
	if !v2.Send {
		t.Fatalf("heartbeat due should send, got %s", v2)
	}
	if out2.Start != 99000 || out2.End != 0 {
		t.Fatalf("not-ready heartbeat reset the anchor: start=%d end=%d, want start=99000 end=0", out2.Start, out2.End)
	}
}

// F2: intervals run on a monotonic clock. A backward wall-clock step must not
// make the heartbeat delta negative and stall self-healing for the step.
func TestGateBackwardClockStepDoesNotStallHeartbeat(t *testing.T) {
	// An idle card carries no start, so the divergence path cannot fire: the
	// heartbeat is the only self-healing route - the case the reviewer saw stall
	// with a -55 s step. (A paused card cannot be used here: the parked pause
	// stays quiet after its one send, by design.)
	idle := Activity{Type: "PLAYING", Details: "Browsing", State: "Idle", SmallImage: "stop"}
	g := &Gate{}
	if _, v := g.Decide(idle, 0, 0, 100000, false, false); !v.Send {
		t.Fatalf("first idle card should send, got %s", v)
	}
	if _, v := g.Decide(idle, 0, 0, 120000, false, false); v.Send {
		t.Fatalf("20 s in, heartbeat should not be due, got %s", v)
	}
	// The wall clock steps back 55 s; only 20 s of interval has elapsed.
	if _, v := g.Decide(idle, 0, 0, 65000, false, false); v.Send {
		t.Fatalf("55 s backward step fired the heartbeat early: %s", v)
	}
	// 10 s of real interval later the monotonic clock reaches 30 s: the
	// heartbeat self-heals instead of stalling for the 55 s step.
	if _, v := g.Decide(idle, 0, 0, 75000, false, false); !v.Send {
		t.Fatalf("heartbeat stalled after a backward step, got %s", v)
	}
}
