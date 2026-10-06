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
// This is NOT about Discord rate limiting: the tracker emits 0/min while steady
// (change/seek events only), the 30 s heartbeat adds 2/min, and a scrub is
// ~9/min - all far below Discord's update limit. The heartbeat is self-healing
// for residual divergence, nothing more.

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
	// ReasonPaused: the paused card with this content was already sent and is
	// parked; no further card is (re)sent while paused and the content is
	// unchanged, so even the heartbeat stays quiet.
	ReasonPaused = "paused"
)

// Verdict is one gate decision.
type Verdict struct {
	Send bool
	// Clear is retained for the consumer's SET_ACTIVITY-null path but is no
	// longer set: the pause transition now sends a paused card and keeps it
	// (outcome: pause persists), so no decision clears the activity.
	Clear  bool
	Reason string // "" when Send; else one of the Reason* constants
}

func (v Verdict) String() string {
	if v.Send {
		if v.Clear {
			return "sent+clear"
		}
		return "sent"
	}
	return "suppressed:" + v.Reason
}

// intervalClock is a monotonic interval clock. It is fed the wall-clock (epoch)
// `now` on every poll and returns a value that advances only by positive wall
// deltas, holding through a backward step. Interval comparisons (the heartbeat,
// the divergence throttle, the tracker's trailing-edge debounce) use this value,
// so a wall-clock adjustment can never make an interval go negative and stall
// self-healing for the step's duration. Anchors keep the epoch `now`: Discord
// timestamps are epoch ms.
type intervalClock struct {
	started bool
	last    int64 // last wall-clock value fed in
	value   int64 // monotonic interval value, same units (ms)
}

// tick folds one wall-clock observation in and returns the monotonic value. The
// first observation seeds value=now, so on a forward-only clock tick(now)==now.
func (c *intervalClock) tick(now int64) int64 {
	if !c.started {
		c.started = true
		c.last = now
		c.value = now
		return c.value
	}
	if d := now - c.last; d > 0 {
		c.value += d
	}
	c.last = now
	return c.value
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
	clock            intervalClock
}

// ForceNext makes the next Decide send regardless of the card. Positive control
// and test hook: it lets a known event be recorded through the real log.
func (g *Gate) ForceNext() { g.force = true }

// Decide is consulted on every poll. card is the card built from the current
// tracker state; pos/total are the live PotPlayer times; now is epoch ms; event
// reports whether the tracker emitted a change/seek this poll; debounced reports
// whether the tracker is holding a seek for the trailing edge. It returns the
// card with its timestamps anchored (start while running, plus the projected end
// when the duration is known; start only, anchored at the pause moment, while
// paused) and the verdict. A pause sends a paused card once and then stays quiet.
func (g *Gate) Decide(card Activity, pos, total, now int64, event, debounced bool) (Activity, Verdict) {
	// Interval bookkeeping runs on a monotonic clock; the anchors below keep the
	// epoch now.
	clk := g.clock.tick(now)
	out := card
	// Re-anchor only on a ready position read. A not-ready read (pos<=0, see
	// presence.go) must keep the card's existing anchor: recomputing here would
	// reset the elapsed clock to 0:00 and emit a full-duration countdown.
	if out.HasStart && pos > 0 {
		out.Start = now - pos
		out.End = 0
		// end = now + (total - pos); serialized as timestamps.end while playing
		// (README deviation: both start and end go on the wire), so Discord
		// renders the time bar against the real media position.
		if total > pos {
			out.End = now + (total - pos)
		}
	}

	// Pause: send a paused card and KEEP it. The activity type is Watching and
	// the card carries a count-UP timer anchored at the pause moment
	// (timestamps.start = now - pausedElapsedMs, where pausedElapsedMs is 0 at
	// the transition), so the client reads "paused for M:SS". There is NO
	// timestamps.end while paused. Decided here, before the suppression paths,
	// so the pause card is never debounced or throttled.
	if out.SmallImage == "pause" {
		if g.hasSent && g.last.SmallImage == "pause" &&
			g.last.Details == out.Details && g.last.State == out.State {
			// Already showing the parked paused card for this content: stay
			// quiet, so the pause timer is not restarted and Discord is not
			// spammed. Not even the heartbeat re-pushes while paused. A content
			// change (a different title/state, e.g. the next episode) falls
			// through and re-sends, so the parked card cannot go stale - while
			// the unchanged card keeps the no-spam property.
			return out, Verdict{Reason: ReasonPaused}
		}
		out.HasStart = true
		out.Start = now // now - pausedElapsedMs; pausedElapsedMs == 0 here
		out.End = 0
		return g.commit(out, clk), Verdict{Send: true}
	}

	if g.force {
		g.force = false
		return g.commit(out, clk), Verdict{Send: true}
	}
	if !g.hasSent {
		return g.commit(out, clk), Verdict{Send: true}
	}

	contentChanged := out.Details != g.last.Details ||
		out.State != g.last.State ||
		out.SmallImage != g.last.SmallImage ||
		out.HasStart != g.last.HasStart
	anchorMoved := out.HasStart && out.Start != g.lastStart

	if contentChanged {
		return g.commit(out, clk), Verdict{Send: true}
	}
	if event && anchorMoved {
		return g.commit(out, clk), Verdict{Send: true}
	}
	// Divergence: the live position no longer matches what the last-sent anchor
	// predicts (a seek the tracker swallowed, a resume, or clock drift). Re-anchor
	// at once, throttled.
	if out.HasStart && pos > 0 && abs64(pos-(now-g.lastStart)) > DivergenceMS &&
		(clk-g.lastDivergenceAt) >= DivergenceThrottleMS {
		g.lastDivergenceAt = clk
		return g.commit(out, clk), Verdict{Send: true}
	}
	if clk-g.lastSentAt >= HeartbeatMS {
		return g.commit(out, clk), Verdict{Send: true}
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

// commit records a sent card as the new reference point. clk is the monotonic
// interval clock; out.Start is the epoch anchor.
func (g *Gate) commit(out Activity, clk int64) Activity {
	g.hasSent = true
	g.last = out
	g.lastStart = out.Start
	g.lastSentAt = clk
	return out
}
