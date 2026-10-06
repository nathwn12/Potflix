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

func TestGatePauseNoTimestamps(t *testing.T) {
	g := &Gate{}
	out, v := g.Decide(Activity{
		Type: "PLAYING", Details: "Show", State: "S1 E1 - 00:01", SmallImage: "pause",
	}, 1000, 60000, 100000, true, false)
	if !v.Send {
		t.Fatalf("pause should send, got %s", v)
	}
	if out.HasStart || out.End != 0 {
		t.Fatalf("pause must carry no timestamps: hasStart=%t end=%d", out.HasStart, out.End)
	}
}

func TestGateResumeReanchors(t *testing.T) {
	g := &Gate{}
	g.Decide(Activity{
		Type: "PLAYING", Details: "Show", State: "S1 E1 - 00:01", SmallImage: "pause",
	}, 1000, 60000, 100000, true, false)
	out, v := g.Decide(runningCard("Show", "S1 E1"), 1000, 60000, 101000, true, false)
	if !v.Send {
		t.Fatalf("resume should send, got %s", v)
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
