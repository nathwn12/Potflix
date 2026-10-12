//! `presence::clock` -- the monotonic interval clock shared by the tracker and
//! the send gate.
//!
//! This module has no Go file of its own: it is `gate.go:76-96`'s unexported
//! `intervalClock`, which `presence.go` reaches because both files live in one
//! Go package. The Rust module split gives the shared type its own body.
//!
//! The clock is fed the wall-clock (epoch) `now` on every poll and returns a
//! value that advances only by positive wall deltas, holding through a backward
//! step. Interval comparisons (the heartbeat, the divergence throttle, the
//! tracker's trailing-edge debounce) use this value, so a wall-clock adjustment
//! can never make an interval go negative and stall self-healing for the step's
//! duration. Anchors keep the epoch `now`: Discord timestamps are epoch ms.

/// A monotonic interval clock (`gate.go:76-96`). The zero value is ready to use.
#[derive(Debug, Clone, Default)]
pub(crate) struct IntervalClock {
    /// Whether the first observation has been folded in.
    started: bool,
    /// The last wall-clock value fed in.
    last: i64,
    /// The monotonic interval value, same units (ms).
    value: i64,
}

impl IntervalClock {
    /// Folds one wall-clock observation in and returns the monotonic value. The
    /// first observation seeds `value = now`, so on a forward-only clock
    /// `tick(now) == now`.
    pub(crate) fn tick(&mut self, now: i64) -> i64 {
        if !self.started {
            self.started = true;
            self.last = now;
            self.value = now;
            return self.value;
        }
        let delta = now - self.last;
        if delta > 0 {
            self.value += delta;
        }
        self.last = now;
        self.value
    }
}
