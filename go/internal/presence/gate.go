package presence

// gate.go - the send gate.
//
// The app is already event-driven: the tracker emits only when the card changes
// or a real seek moves the position. The gate sits between the tracker and the
// wire and decides whether the current card actually goes out. It exists because
// the tracker's per-poll seek delta can be swallowed (held by the debounce, then
// never re-fired), which left the card frozen on a stale anchor. The gate adds
// the two self-healing paths: a divergence re-anchor (live position vs the last
// anchor Discord was told) and a coarse heartbeat. It also records EVERY
// decision, so a suppressed poll is visible instead of silent.
//
// This is NOT about Discord rate limiting: measured pushes are 0/min while
// steady and ~9/min during scrubs, far below Discord's update limit. The
// heartbeat is self-healing for residual divergence, nothing more.

// Timing constants for the send gate.
const (
	// HeartbeatMS re-pushes an otherwise-unchanged running card so any residual
	// divergence self-corrects. It is not a rate-limit workaround.
	HeartbeatMS int64 = 30_000
	// DivergenceMS is how far the live position may differ from what the
	// last-sent anchor predicts before the card is re-anchored at once.
	DivergenceMS int64 = 2_000
	// DivergenceThrottleMS bounds divergence re-anchors to at most one per this
	// interval, so a noisy position read cannot turn into a push burst.
	DivergenceThrottleMS int64 = 5_000
)

// Suppression reasons, logged verbatim.
const (
	// ReasonUnchanged: an event arrived but the payload is identical to the last
	// sent card (a redundant emit).
	ReasonUnchanged = "unchanged"
	// ReasonDebounce: a seek was held by the trailing-edge debounce and is
	// waiting for the window to expire.
	ReasonDebounce = "debounce"
	// ReasonHeartbeatNotDue: the card is steady, divergence is within tolerance,
	// and the heartbeat interval has not elapsed.
	ReasonHeartbeatNotDue = "heartbeat-not-due"
)

// Verdict is one gate decision.
type Verdict struct {
	Send   bool
	Reason string // "" when Send; else one of the Reason* constants
}

func (v Verdict) String() string {
	if v.Send {
		return "sent"
	}
	return "suppressed:" + v.Reason
}

// Gate remembers what was last sent so it can decide whether the current card
// needs to go out. The zero value is ready to use.
type Gate struct {
	hasSent          bool
	last             Activity
	lastStart        int64
	lastSentAt       int64
	lastDivergenceAt int64
	force            bool
}

// ForceNext makes the next Decide send regardless of the card. Positive control
// and test hook: it lets a known event be recorded through the real log.
func (g *Gate) ForceNext() { g.force = true }

// Decide is consulted on every poll. card is the card built from the current
// tracker state; pos/total are the live PotPlayer times; now is epoch ms; event
// reports whether the tracker emitted a change/seek this poll; debounced reports
// whether the tracker is holding a seek for the trailing edge. It returns the
// card with its timestamps anchored (start, and end when the duration is known)
// and the verdict.
func (g *Gate) Decide(card Activity, pos, total, now int64, event, debounced bool) (Activity, Verdict) {
	out := card
	if out.HasStart {
		out.Start = now - pos
		out.End = 0
		// end = now + (total - pos); omitted when the duration is unknown.
		if total > pos {
			out.End = now + (total - pos)
		}
	}
	if g.force {
		g.force = false
		return g.commit(out, now), Verdict{Send: true}
	}
	if !g.hasSent {
		return g.commit(out, now), Verdict{Send: true}
	}

	contentChanged := out.Details != g.last.Details ||
		out.State != g.last.State ||
		out.SmallImage != g.last.SmallImage ||
		out.HasStart != g.last.HasStart
	anchorMoved := out.HasStart && out.Start != g.lastStart

	if contentChanged {
		return g.commit(out, now), Verdict{Send: true}
	}
	if event && anchorMoved {
		return g.commit(out, now), Verdict{Send: true}
	}
	// Divergence: the live position no longer matches what the last-sent anchor
	// predicts (a seek the tracker swallowed, a resume, or clock drift). Re-anchor
	// at once, throttled.
	if out.HasStart && pos > 0 && abs64(pos-(now-g.lastStart)) > DivergenceMS &&
		(now-g.lastDivergenceAt) >= DivergenceThrottleMS {
		g.lastDivergenceAt = now
		return g.commit(out, now), Verdict{Send: true}
	}
	if now-g.lastSentAt >= HeartbeatMS {
		return g.commit(out, now), Verdict{Send: true}
	}

	switch {
	case debounced:
		return out, Verdict{Reason: ReasonDebounce}
	case event:
		return out, Verdict{Reason: ReasonUnchanged}
	default:
		return out, Verdict{Reason: ReasonHeartbeatNotDue}
	}
}

// commit records a sent card as the new reference point.
func (g *Gate) commit(out Activity, now int64) Activity {
	g.hasSent = true
	g.last = out
	g.lastStart = out.Start
	g.lastSentAt = now
	return out
}
