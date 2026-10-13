//! `presence::gate` -- mirrors `go\internal\presence\gate.go`.
//!
//! The send gate: what has to be true before a presence push is allowed out.
//!
//! The app is already event-driven: the tracker emits only when the card changes
//! or a real seek moves the position. The gate sits between the tracker and the
//! wire and decides whether the current card actually goes out. It exists because
//! the tracker's per-poll seek delta can be swallowed (held by the debounce, then
//! never re-fired), which left the card frozen on a stale anchor. The gate adds
//! the two self-healing paths: a divergence re-anchor (live position vs the last
//! anchor Discord was told) and a coarse heartbeat. It also records EVERY
//! decision, so a suppressed poll is visible instead of silent.
//!
//! This is NOT about Discord rate limiting: the tracker emits 0/min while steady
//! (change/seek events only), the 30 s heartbeat adds 2/min, and a scrub is
//! ~9/min - all far below Discord's update limit. The heartbeat is self-healing
//! for residual divergence, nothing more.

use core::fmt;

use crate::clock::IntervalClock;
use crate::{Activity, Button, abs64};

/// HeartbeatMS re-pushes an otherwise-unchanged running card so any residual
/// divergence self-corrects. It is not a rate-limit workaround (`gate.go:23`).
pub const HEARTBEAT_MS: i64 = 30_000;

/// DivergenceMS is how far the live position may differ from what the last-sent
/// anchor predicts before the card is re-anchored at once (`gate.go:26`).
pub const DIVERGENCE_MS: i64 = 2_000;

/// DivergenceThrottleMS bounds divergence re-anchors to at most one per this
/// interval, so a noisy position read cannot turn into a push burst
/// (`gate.go:29`).
pub const DIVERGENCE_THROTTLE_MS: i64 = 5_000;

/// ReasonUnchanged: an event arrived but the payload is identical to the last
/// sent card (a redundant emit) (`gate.go:36`).
pub const REASON_UNCHANGED: &str = "unchanged";

/// ReasonDebounce: a seek was held by the trailing-edge debounce and is waiting
/// for the window to expire (`gate.go:39`).
pub const REASON_DEBOUNCE: &str = "debounce";

/// ReasonHeartbeatNotDue: the card is steady, divergence is within tolerance, and
/// the heartbeat interval has not elapsed (`gate.go:42`).
pub const REASON_HEARTBEAT_NOT_DUE: &str = "heartbeat-not-due";

/// ReasonPaused: the paused card with this content was already sent and is
/// parked; no further card is (re)sent while paused and the content is
/// unchanged, so even the heartbeat stays quiet (`gate.go:46`).
pub const REASON_PAUSED: &str = "paused";

/// One gate decision (`gate.go:50-57`).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Verdict {
    /// Whether the card goes out.
    pub send: bool,
    /// Retained for the consumer's SET_ACTIVITY-null path but no longer set: the
    /// pause transition now sends a paused card and keeps it (outcome: pause
    /// persists), so no decision clears the activity.
    pub clear: bool,
    /// Empty when `send`; else one of the `REASON_*` constants.
    pub reason: String,
}

impl fmt::Display for Verdict {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        if self.send {
            if self.clear {
                f.write_str("sent+clear")
            } else {
                f.write_str("sent")
            }
        } else {
            write!(f, "suppressed:{}", self.reason)
        }
    }
}

/// Whether the card is the parked pause card (`gate.go:98-104`): the builder's
/// `paused` flag, or the legacy `small_image` "pause" marker on cards built
/// before the pristine-card change (kept so previously built cards keep their
/// meaning).
fn is_paused_card(card: &Activity) -> bool {
    card.paused || card.small_image == "pause"
}

/// Whether two card button lists carry the same ordered label+URL pairs
/// (`gate.go:108`). Order matters: the wire serializes buttons in order.
fn buttons_equal(a: &[Button], b: &[Button]) -> bool {
    if a.len() != b.len() {
        return false;
    }
    a.iter()
        .zip(b.iter())
        .all(|(x, y)| x.label == y.label && x.url == y.url)
}

/// Remembers what was last sent so it can decide whether the current card needs
/// to go out (`gate.go:122-130`). The zero value is ready to use.
#[derive(Debug, Clone, Default)]
pub struct Gate {
    has_sent: bool,
    last: Activity,
    last_start: i64,
    last_sent_at: i64,
    last_divergence_at: i64,
    force: bool,
    clock: IntervalClock,
}

impl Gate {
    /// The zero value, spelled out.
    pub fn new() -> Gate {
        Gate::default()
    }

    /// Makes the next [`Gate::decide`] send regardless of the card
    /// (`gate.go:134`). Positive control and test hook: it lets a known event be
    /// recorded through the real log.
    pub fn force_next(&mut self) {
        self.force = true;
    }

    /// Consulted on every poll (`gate.go:143`). `card` is the card built from the
    /// current tracker state; `pos`/`total` are the live PotPlayer times; `now`
    /// is epoch ms; `event` reports whether the tracker emitted a change/seek
    /// this poll; `debounced` reports whether the tracker is holding a seek for
    /// the trailing edge. It returns the card with its timestamps anchored (start
    /// while running, plus the projected end when the duration is known; start
    /// only, anchored at the pause moment, while paused) and the verdict. A pause
    /// sends a paused card once and then stays quiet.
    pub fn decide(
        &mut self,
        card: Activity,
        pos: i64,
        total: i64,
        now: i64,
        event: bool,
        debounced: bool,
    ) -> (Activity, Verdict) {
        // Interval bookkeeping runs on a monotonic clock; the anchors below keep
        // the epoch now.
        let clk = self.clock.tick(now);
        let mut out = card;
        // Re-anchor only on a ready position read. A not-ready read (pos<=0, see
        // presence.go) must keep the card's existing anchor: recomputing here
        // would reset the elapsed clock to 0:00 and emit a full-duration
        // countdown.
        if out.has_start && pos > 0 {
            out.start = now - pos;
            out.end = 0;
            // end = now + (total - pos); serialized as timestamps.end while
            // playing (README deviation: both start and end go on the wire), so
            // Discord renders the time bar against the real media position.
            if total > pos {
                out.end = now + (total - pos);
            }
        }

        // Pause: send a paused card and KEEP it. The activity type is Watching
        // and the card carries a count-UP timer anchored at the pause moment
        // (timestamps.start = now - pausedElapsedMs, where pausedElapsedMs is 0
        // at the transition), so the client reads "paused for M:SS". There is NO
        // timestamps.end while paused. Decided here, before the suppression
        // paths, so the pause card is never debounced or throttled. The pause is
        // detected via is_paused_card (the builder's Paused flag, with the legacy
        // small_image marker honoured); no pause icon rides the wire.
        if is_paused_card(&out) {
            if self.has_sent
                && is_paused_card(&self.last)
                && self.last.paused == out.paused
                && self.last.name == out.name
                && self.last.details == out.details
                && self.last.state == out.state
                && self.last.large_image == out.large_image
                && buttons_equal(&self.last.buttons, &out.buttons)
            {
                // Already showing the parked paused card for this content: stay
                // quiet, so the pause timer is not restarted and Discord is not
                // spammed. Not even the heartbeat re-pushes while paused. A
                // content change (a different title/state, e.g. the next
                // episode) falls through and re-sends, so the parked card cannot
                // go stale - while the unchanged card keeps the no-spam
                // property.
                return (
                    out,
                    Verdict {
                        reason: REASON_PAUSED.to_string(),
                        ..Verdict::default()
                    },
                );
            }
            out.has_start = true;
            out.start = now; // now - pausedElapsedMs; pausedElapsedMs == 0 here
            out.end = 0;
            return (self.commit(out, clk), Verdict::sent());
        }

        if self.force {
            self.force = false;
            return (self.commit(out, clk), Verdict::sent());
        }
        if !self.has_sent {
            return (self.commit(out, clk), Verdict::sent());
        }

        let content_changed = out.name != self.last.name
            || out.details != self.last.details
            || out.state != self.last.state
            || out.paused != self.last.paused
            || out.small_image != self.last.small_image
            || out.large_image != self.last.large_image
            || !buttons_equal(&self.last.buttons, &out.buttons)
            || out.has_start != self.last.has_start;
        let anchor_moved = out.has_start && out.start != self.last_start;

        if content_changed {
            return (self.commit(out, clk), Verdict::sent());
        }
        if event && anchor_moved {
            return (self.commit(out, clk), Verdict::sent());
        }
        // Divergence: the live position no longer matches what the last-sent
        // anchor predicts (a seek the tracker swallowed, a resume, or clock
        // drift). Re-anchor at once, throttled.
        if out.has_start
            && pos > 0
            && abs64(pos - (now - self.last_start)) > DIVERGENCE_MS
            && (clk - self.last_divergence_at) >= DIVERGENCE_THROTTLE_MS
        {
            self.last_divergence_at = clk;
            return (self.commit(out, clk), Verdict::sent());
        }
        if clk - self.last_sent_at >= HEARTBEAT_MS {
            return (self.commit(out, clk), Verdict::sent());
        }

        let reason = if debounced {
            REASON_DEBOUNCE
        } else if event {
            REASON_UNCHANGED
        } else {
            REASON_HEARTBEAT_NOT_DUE
        };
        (
            out,
            Verdict {
                reason: reason.to_string(),
                ..Verdict::default()
            },
        )
    }

    /// Records a sent card as the new reference point (`gate.go:239`). `clk` is
    /// the monotonic interval clock; `out.start` is the epoch anchor.
    fn commit(&mut self, out: Activity, clk: i64) -> Activity {
        self.has_sent = true;
        self.last = out.clone();
        self.last_start = out.start;
        self.last_sent_at = clk;
        out
    }
}

impl Verdict {
    /// A plain send verdict (no clear, no reason).
    fn sent() -> Verdict {
        Verdict {
            send: true,
            ..Verdict::default()
        }
    }
}
