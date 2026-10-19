//! `presence` -- mirrors the Go package `go\internal\presence`.
//!
//! The presence state machine and the activity payload builder.
//!
//! This file is the module body for `go\internal\presence\presence.go`: the
//! transition matrix ([`Tracker`]), the trailing-edge RUN debounce, the card line
//! grammar ([`build_activity`]) and the metadata-resolved card
//! ([`build_activity_resolved`]). [`gate`] is `gate.go` (the send gate) and
//! [`cardspec`] is `cardspec.go` (the Card view's spec model).
//!
//! Ported from me.potflix.rpc.Main (PresenceTracker.poll :265-309,
//! withStateTime :313-317, pushActivity :139-191, fmt :501-508), with one
//! deliberate deviation from the fleet decisions (see [`Tracker`] and the
//! README): repeated RUN pushes within ~1 s are debounced unless the title
//! changed. The debounce is trailing-edge: the suppressed anchor is HELD and
//! emitted when the window expires, so a seek is never dropped forever.
//!
//! Not carried from `presence.go`: the unexported `filterButtons` (:646-652) is
//! never called in the Go original (the builders never populate buttons), so in
//! Rust it would be dead code under `-D warnings`. The pristine-card decision it
//! documents is stated where the call site used to be.

use player::control::PlayStatus;
use player::window::{APP_NAME, SUFFIX};
use titleclean::{clean, trunc};

use crate::clock::IntervalClock;

pub mod cardspec;
pub mod gate;

pub(crate) mod clock;

#[cfg(test)]
mod cardspec_test;
#[cfg(test)]
mod gate_test;
#[cfg(test)]
mod presence_test;

/// SeekJumpMS: a per-poll ct jump above this is a real seek (`presence.go:26`).
pub const SEEK_JUMP_MS: i64 = 1500;

/// SettleMoveMS: ct advance across the window that proves real movement
/// (`presence.go:28`).
pub const SETTLE_MOVE_MS: i64 = 50;

/// MoveWindow is the number of ct samples kept to prove real movement
/// (`presence.go:30`).
pub const MOVE_WINDOW: usize = 3;

/// RunDebounceMS is the fleet-decision-10 deviation: a repeated RUN push within
/// this window, with an unchanged title, is suppressed (`presence.go:35`).
pub const RUN_DEBOUNCE_MS: i64 = 1000;

/// One emission decision (Java PresenceAction). `None` means "emit nothing".
/// `ct` is the sample carried into the payload/log (`presence.go:39-46`).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Action {
    /// The player is playing.
    pub running: bool,
    /// The player is paused.
    pub paused: bool,
    /// Whether `start` is meaningful.
    pub has_start: bool,
    /// Epoch ms, meaningful when `has_start`.
    pub start: i64,
    /// Legacy position suffix; no longer populated (the paused card uses a
    /// "Paused" marker).
    pub suffix: String,
    /// The ct sample carried into the payload/log.
    pub ct: i64,
}

impl Action {
    /// Java `describe()`'s leading token: RUN / PAUSE / STOP (`presence.go:49`).
    pub fn tag(&self) -> &'static str {
        if self.running {
            "RUN"
        } else if self.paused {
            "PAUSE"
        } else {
            "STOP"
        }
    }
}

/// Go's nil-receiver `(*Action).Tag()`: a missing action tags as `"null"`.
pub fn action_tag(a: Option<&Action>) -> &'static str {
    a.map_or("null", Action::tag)
}

/// One PresenceTracker (`presence.go:64-93`). Its zero value is ready to use;
/// the Java ctor state is reproduced by `have_prev = false` /
/// `has_last_status = false`.
#[derive(Debug, Clone)]
pub struct Tracker {
    last_title: String,
    last_status: PlayStatus,
    /// Java's `lastStatus != null`.
    has_last_status: bool,
    /// Last VALID ct, used only as the seek baseline; -1 = none.
    prev_ct: i64,
    /// Position held in the paused text; -1 = none.
    frozen_ct: i64,
    have_prev: bool,

    ct_window: [i64; MOVE_WINDOW],
    ct_window_len: usize,
    ct_window_idx: usize,

    // Fleet decision 10 (deviation, revised): RUN debounce anchor. A repeated
    // RUN push inside the window is HELD (pending_run), not dropped, and emitted
    // on the first poll after the window expires (trailing edge).
    has_run_push: bool,
    last_run_push_at: i64,
    last_run_push_title: String,
    pending_run: bool,
    pending_title: String,

    // The most recent emitted action. `current` exposes it so the send gate can
    // rebuild the card on every poll, including polls where the tracker emits
    // nothing.
    last_action: Option<Action>,

    // The monotonic interval clock for the trailing-edge debounce, so a
    // wall-clock step cannot strand a held seek.
    clock: IntervalClock,
}

impl Default for Tracker {
    fn default() -> Tracker {
        Tracker {
            last_title: String::new(),
            last_status: PlayStatus::STOPPED,
            has_last_status: false,
            prev_ct: 0,
            frozen_ct: 0,
            have_prev: false,
            ct_window: [0; MOVE_WINDOW],
            ct_window_len: 0,
            ct_window_idx: 0,
            has_run_push: false,
            last_run_push_at: 0,
            last_run_push_title: String::new(),
            pending_run: false,
            pending_title: String::new(),
            last_action: None,
            clock: IntervalClock::default(),
        }
    }
}

impl Tracker {
    /// The zero value, spelled out: boot `have_prev = false` /
    /// `has_last_status = false` (the Java ctor state).
    pub fn new() -> Tracker {
        Tracker::default()
    }

    fn sample_ct(&mut self, ct: i64) {
        self.ct_window[self.ct_window_idx] = ct;
        self.ct_window_idx = (self.ct_window_idx + 1) % MOVE_WINDOW;
        if self.ct_window_len < MOVE_WINDOW {
            self.ct_window_len += 1;
        }
    }

    /// The minimum of the sampled window (`presence.go:105-113`); Java returns
    /// `Long.MAX_VALUE` when empty, but `moving` already requires len >= 2.
    fn oldest_ct(&self) -> i64 {
        self.ct_window[..self.ct_window_len]
            .iter()
            .copied()
            .min()
            .unwrap_or(0)
    }

    /// Consumes one sample and returns the action to push, or `None`
    /// (`presence.go:117`). `ps`/`ct` are the raw reads; `now` is epoch ms.
    pub fn poll(&mut self, raw_title: &str, ps: PlayStatus, ct: i64, now: i64) -> Option<Action> {
        // Interval bookkeeping (the trailing-edge debounce) runs on a monotonic
        // clock; anchors below keep the epoch now.
        let clk = self.clock.tick(now);

        // New media: forget everything (Java :267-270).
        if self.have_prev && self.last_title != raw_title {
            self.have_prev = false;
            self.has_last_status = false;
            self.last_status = PlayStatus::STOPPED;
            self.frozen_ct = -1;
            self.prev_ct = -1;
            self.ct_window_len = 0;
            self.ct_window_idx = 0;
            self.pending_run = false;
            self.last_action = None;
        }

        // A not-ready read (ct<=0) must NOT overwrite the seek baseline
        // (:273-275).
        let ct_ok = ct > 0;
        let prev_ct_before = self.prev_ct;
        if ct_ok {
            self.sample_ct(ct);
            self.prev_ct = ct;
        }

        // ct ticks are coarse, so one delta is not proof of movement (:278).
        let moving = ct_ok && self.ct_window_len >= 2 && (ct - self.oldest_ct()) >= SETTLE_MOVE_MS;

        // Undefined carries no information: emit nothing, change no state (:281).
        if ps == PlayStatus::UNDEFINED {
            self.have_prev = true;
            return None;
        }

        // A non-Running report while the position is still moving, having
        // settled into Running, is a transient glitch: the player is really
        // playing (:285-286).
        let mut eff = ps;
        if ps != PlayStatus::RUNNING
            && moving
            && self.has_last_status
            && self.last_status == PlayStatus::RUNNING
        {
            eff = PlayStatus::RUNNING;
        }

        let changed = !self.have_prev || !self.has_last_status || eff != self.last_status;
        let seeked = !changed
            && eff == PlayStatus::RUNNING
            && self.has_last_status
            && self.last_status == PlayStatus::RUNNING
            && self.have_prev
            && ct_ok
            && abs64(ct - prev_ct_before) > SEEK_JUMP_MS;
        let paused_seek = !changed
            && eff == PlayStatus::PAUSED
            && self.has_last_status
            && self.last_status == PlayStatus::PAUSED
            && self.frozen_ct >= 0
            && ct_ok
            && abs64(ct - self.frozen_ct) > SEEK_JUMP_MS;

        let mut action = if eff == PlayStatus::RUNNING {
            if (changed || seeked) && ct_ok {
                Some(Action {
                    running: true,
                    has_start: true,
                    start: now - ct,
                    ct,
                    ..Action::default()
                })
            } else {
                None
            }
        } else if eff == PlayStatus::PAUSED {
            if (changed || paused_seek) && ct_ok {
                // No position suffix any more: the paused card carries a
                // "Paused" marker and counts the pause duration UP from
                // timestamps.start (set by the send gate), so there is no
                // frozen " - 00:00" artifact.
                self.frozen_ct = ct;
                Some(Action {
                    paused: true,
                    ct,
                    ..Action::default()
                })
            } else {
                None
            }
        } else if changed {
            // Stopped.
            Some(Action {
                ct,
                ..Action::default()
            })
        } else {
            None
        };

        // Fleet decision 10 (revised): debounce repeated RUN pushes (seek
        // bursts) within ~1 s unless the title changed, but HOLD the suppressed
        // anchor instead of dropping it. The old drop-then-forget behaviour lost
        // the seek re-anchor forever: the per-poll delta never re-fires, so the
        // card kept the pre-seek anchor. A `changed` RUN push is a real
        // state/title transition and always emits.
        if action.as_ref().is_some_and(|a| a.running) {
            if !changed
                && self.has_run_push
                && (clk - self.last_run_push_at) < RUN_DEBOUNCE_MS
                && raw_title == self.last_run_push_title
            {
                self.pending_run = true;
                self.pending_title = raw_title.to_string();
                action = None;
            } else {
                self.has_run_push = true;
                self.last_run_push_at = clk;
                self.last_run_push_title = raw_title.to_string();
                self.pending_run = false;
            }
        } else if action.is_some() {
            // Any non-RUN emit supersedes a held seek.
            self.pending_run = false;
        }

        // Trailing edge: once the window has expired the held seek lands with a
        // fresh anchor (start = now - ct), so the last seek of a scrub is never
        // lost.
        if action.is_none()
            && self.pending_run
            && ct_ok
            && eff == PlayStatus::RUNNING
            && raw_title == self.pending_title
            && (clk - self.last_run_push_at) >= RUN_DEBOUNCE_MS
        {
            action = Some(Action {
                running: true,
                has_start: true,
                start: now - ct,
                ct,
                ..Action::default()
            });
            self.has_run_push = true;
            self.last_run_push_at = clk;
            self.last_run_push_title = raw_title.to_string();
            self.pending_run = false;
        }

        // On emit only, remember title/status; have_prev is always set
        // (:306-308).
        if let Some(a) = action.as_ref() {
            self.last_title = raw_title.to_string();
            self.last_status = eff;
            self.has_last_status = true;
            self.last_action = Some(a.clone());
        }
        self.have_prev = true;
        action
    }

    /// The most recent emitted action, or `None` before any emit
    /// (`presence.go:234`). The send gate uses it to rebuild the card on every
    /// poll, including polls where the tracker emits nothing.
    pub fn current(&self) -> Option<Action> {
        self.last_action.clone()
    }

    /// Whether a RUN push was suppressed by the trailing-edge debounce and is
    /// waiting for the window to expire (`presence.go:244`).
    pub fn pending_debounce(&self) -> bool {
        self.pending_run
    }
}

/// `abs64` (`presence.go:246`): shared with the send gate's divergence check.
pub(crate) fn abs64(v: i64) -> i64 {
    if v < 0 { -v } else { v }
}

/// Mirrors Main.fmt (`presence.go:254`): MM:SS under an hour, else H:MM:SS.
pub fn fmt_ms(ms: i64) -> String {
    let seconds = ms / 1000;
    let s = seconds % 60;
    let m = (seconds / 60) % 60;
    let h = seconds / 3600;
    if h == 0 {
        format!("{m:02}:{s:02}")
    } else {
        format!("{h}:{m:02}:{s:02}")
    }
}

/// The paused card's state suffix: episode context plus this reads e.g.
/// `S1 E2 - Paused` (or just `Paused` with no context) (`presence.go:267`).
pub(crate) const PAUSED_MARKER: &str = " - Paused";

/// The target budget (UTF-16 units) for synopsis-bearing State lines
/// (`presence.go:275`).
pub(crate) const STATE_SYNOPSIS_BUDGET: usize = 72;

/// Slot budgets (UTF-16 units), named in one place: the header (`name`), the
/// first body line (`details`) and the hover tooltip (`large_text`) each cap
/// at 128; the second body line (`state`) caps at 128 plain and at
/// [`STATE_SYNOPSIS_BUDGET`] when it carries a synopsis.
pub(crate) const NAME_BUDGET: usize = 128;
pub(crate) const DETAILS_BUDGET: usize = 128;
pub(crate) const STATE_BUDGET: usize = 128;
pub(crate) const LARGE_TEXT_BUDGET: usize = 128;

/// Discord caps `large_image` at 300 chars (`code=4000` otherwise). A
/// Kitsu-signed Backblaze URL can exceed it, so an over-budget URL is omitted
/// (fallback to `"logo"`) rather than truncated (a cut signed URL is a broken
/// link). `Resolved` carries no alternative short artwork URL, so `"logo"` is
/// the stable short asset; no new network calls.
pub(crate) const LARGE_IMAGE_BUDGET: usize = 300;

/// The body-line joiner: U+00B7 MIDDLE DOT with single spaces.
pub(crate) const JOINER: &str = " \u{b7} ";

/// The `1<<28` "no cap" budget the sanitizers use before a real cap is applied.
pub(crate) const HUGE: usize = 1 << 28;

/// Enables the SET_ACTIVITY `name` header override: the card header becomes
/// "Watching <title>" instead of the Discord app name (`presence.go:281`).
/// REVERT IN ONE EDIT: set this to `false` to drop the override entirely (the
/// header then falls back to the app name); nothing else needs to change.
const ACTIVITY_NAME_HEADER: bool = true;

/// Java's `\s` class (`[ \t\n\x0B\f\r]`), the class `^\s*-\s*` matches on.
const JAVA_WS: [char; 6] = [' ', '\t', '\n', '\u{0b}', '\u{0c}', '\r'];

/// Java's `^\s*-\s*` (`presence.go:284`) applied by hand: the workspace forbids
/// a third-party regex crate here, and the pattern is a single anchored match,
/// so the hand-rolled form is exact. A string without the dash does not match
/// and is returned unchanged, exactly as `replaceAll` would leave it.
fn strip_leading_dash(s: &str) -> &str {
    let after_ws = s.trim_start_matches(|c| JAVA_WS.contains(&c));
    let Some(after_dash) = after_ws.strip_prefix('-') else {
        return s;
    };
    after_dash.trim_start_matches(|c| JAVA_WS.contains(&c))
}

/// Mirrors Main.withStateTime (`presence.go:293`): context plus a suffix; a
/// leading dash is stripped when there is no context. Used for the paused card's
/// "Paused" marker ([`PAUSED_MARKER`]). Deviation from the Java reference: the
/// Java code truncates context+suffix together, which let the 128-unit cap eat
/// the tail of the marker (a 120-unit context + the 9-unit " - Paused" rendered
/// "... - Paus..."). Here the context is budgeted to `limit - len(suffix)`
/// first, so the suffix always survives whole and the result stays within the
/// limit.
pub fn with_state_time(context: &str, suffix: &str) -> String {
    if suffix.is_empty() {
        return context.to_string();
    }
    if context.is_empty() {
        return strip_leading_dash(suffix).to_string();
    }
    // `len` is BYTES in Go too (`128-len(suffix)`), so the budget is measured the
    // same way here.
    trunc(context, 128usize.saturating_sub(suffix.len())) + suffix
}

/// The Discord payload (contract section 5) (`presence.go:318-344`). `type_` is
/// always "PLAYING"; the fields not listed are deliberately unset.
///
/// Netflix-style mapping (no buttons, no status icons): the header (`name`) is
/// the media title, the first body line (`details`) is the episode name when
/// known, else the episode tag, else the title, and the second body line
/// (`state`) is the episode tag when it adds information, else empty. `large_text`
/// names the app ("PotFlix"); `name` is governed by `ACTIVITY_NAME_HEADER`:
/// empty falls back to the Discord app name header. `paused` marks the parked
/// pause card for the send gate (the pause timer is carried by timestamps, not
/// by an icon).
///
/// `small_image`/`small_text`/`buttons` are retained only so existing callers
/// keep compiling; the builders never populate them and the wire never
/// serializes them, so the card carries no corner icon and no click-through
/// links.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Activity {
    /// The activity type; always "PLAYING" from the builders.
    pub type_: String,
    /// The card header; empty falls back to the Discord app name.
    pub name: String,
    /// The first body line.
    pub details: String,
    /// The second body line.
    pub state: String,
    /// "logo", or the resolved https artwork URL.
    pub large_image: String,
    /// "PotFlix", or the synopsis in the hover slot.
    pub large_text: String,
    /// True for the parked pause card. The gate keys its once-and-park behaviour
    /// off this, not off `small_image`.
    pub paused: bool,
    /// Legacy: the builders never populate it. The gate still honours
    /// `small_image == "pause"` as a pause signal so previously built cards keep
    /// their meaning.
    pub small_image: String,
    /// Legacy: the builders never populate it.
    pub small_text: String,
    /// Whether `start` is meaningful.
    pub has_start: bool,
    /// Epoch ms anchor.
    pub start: i64,
    /// The projected end (epoch ms), set by the send gate while running and the
    /// duration is known. Serialized as `timestamps.end` while playing so
    /// Discord renders the remaining-time bar; zero (and omitted) while paused.
    pub end: i64,
    /// Legacy: the builders never populate it.
    pub buttons: Vec<Button>,
}

/// One presence-local card link (`presence.go:416-419`).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Button {
    /// The button label.
    pub label: String,
    /// The button URL.
    pub url: String,
}

/// Composes the payload for one action from the raw window title
/// (`presence.go:349`, Java pushActivity :139-191). The idle card is used when
/// the title carries no media name.
pub fn build_activity(raw_title: &str, a: Option<&Action>) -> Activity {
    let empty = Action::default();
    let a = a.unwrap_or(&empty);
    let raw = raw_title.strip_suffix(SUFFIX).unwrap_or(raw_title);
    if raw.trim().is_empty() || raw == APP_NAME {
        return Activity {
            type_: "PLAYING".to_string(),
            details: "Browsing".to_string(),
            state: "Idle".to_string(),
            large_image: "logo".to_string(),
            large_text: "PotFlix".to_string(),
            // `name` intentionally unset: the header falls back to the app name.
            // No small image/text: the idle card carries no status icon.
            ..Activity::default()
        };
    }

    let c = clean(raw);
    let title = trunc(&c.title, 128);
    // First body line: the episode name when known, else the episode tag, else
    // the title.
    let mut details = title.clone();
    if !c.episode_name.is_empty() {
        details = trunc(&c.episode_name, 128);
    } else if !c.episode_tag.is_empty() {
        details = trunc(&c.episode_tag, 128);
    }
    // Second body line: the episode tag, but only when it adds information
    // beyond the first line. The comparison uses the FINAL emitted values: both
    // sides are truncated to the 128 cap first, so a tag longer than the cap
    // cannot repeat the details line it already fills.
    let mut state = String::new();
    let tag = trunc(&c.episode_tag, 128);
    if !tag.is_empty() && tag != details {
        state = tag;
    }

    // Paused: the state is the episode tag plus a "Paused" marker, not a frozen
    // position. The pause duration is the count-UP timer anchored by the gate
    // (timestamps.start = the pause moment).
    if a.paused {
        state = with_state_time(&state, PAUSED_MARKER);
    }
    let mut act = Activity {
        type_: "PLAYING".to_string(),
        details,
        state,
        large_image: "logo".to_string(),
        large_text: "PotFlix".to_string(),
        paused: a.paused,
        ..Activity::default()
    };
    if ACTIVITY_NAME_HEADER {
        act.name = title;
    }
    // No small_image/small_text on any branch: the card carries no
    // play/pause/stop corner icon. The pause is signalled by `paused` (for the
    // gate) and the " - Paused" state marker (for the viewer).
    if a.has_start {
        act.has_start = true;
        act.start = a.start;
    }
    act
}

/// Canonical titles from a metadata lookup; a zero field is ignored
/// (`presence.go:422-432`).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Resolved {
    /// Canonical series or movie title.
    pub series: String,
    /// Canonical episode title.
    pub episode_name: String,
    /// Cover/poster URL (https); zero value = absent.
    pub artwork: String,
    /// One-line plain text; zero value = absent.
    pub synopsis: String,
    /// Movie genres, e.g. "Action, Drama"; zero value = absent.
    pub genres: String,
    /// Movie runtime, e.g. "2h 19m"; zero value = absent.
    pub length: String,
    /// Movie release year; <=0 = absent.
    pub year: i32,
    /// True: movie card lines; false: series card lines.
    pub is_movie: bool,
    /// Ignored: the pristine card carries no links.
    pub buttons: Vec<Button>,
}

/// Sanitizes free text for the RPC card (`presence.go:438`): trim, collapse runs
/// of whitespace/newlines/tabs to single spaces, strip C0 control chars, then
/// truncate to `max` via [`titleclean::trunc`]. Internal underscores and other
/// real title content are preserved.
pub(crate) fn rp_text(s: &str, max: usize) -> String {
    let collapsed = s.split_whitespace().collect::<Vec<_>>().join(" ");
    let stripped: String = collapsed
        .chars()
        .filter(|c| !(*c < '\u{20}' || *c == '\u{7f}'))
        .collect();
    trunc(&stripped, max)
}

/// Sanitizes `s` via [`rp_text`], then caps it at `max` UTF-16 units at a word
/// boundary (`presence.go:454`): when the sanitized text fits it is returned
/// as-is, byte for byte; otherwise the prefix fitting in `max-1` units is cut
/// at its last space and a single U+2026 is appended, so a cut ends on a whole
/// word wherever the text has word boundaries. With no space in the prefix it
/// still signals the cut: the prefix fitting in `max-1` units (cut on a char
/// boundary, never a split surrogate) carries a single trailing U+2026 within
/// the `max` budget — a spaceless run (one long token, CJK text) cut on a
/// character boundary. A cut landing on a sentence end — the last kept
/// character is `.`/`!`/`?` and the character after it is whitespace or
/// end-of-text — keeps that sentence with its own terminator and carries no
/// ellipsis.
///
/// DEPARTURE from the frozen reference (`presence.go:454`): the cut base is
/// stripped of trailing punctuation before the ellipsis is appended. Go kept
/// the comma/semicolon the clause cut stopped at, so the wire carried
/// `Majin Buu,…` / `meet Elliot;…` (and a mid-word `…and Ernest` rode the
/// tooltip's hard cut at the call site); here every truncated line ends on a
/// word character plus U+2026, or on nothing extra when nothing was cut. A
/// truncated line is always signalled — either a complete sentence that keeps
/// its own terminator with no ellipsis, or a single U+2026 that ends the line.
/// A line is never silently truncated.
pub(crate) fn trim_word(s: &str, max: usize) -> String {
    if max == 0 {
        return String::new();
    }
    let full = rp_text(s, HUGE);
    if utf16_units(&full) <= max {
        return full;
    }
    if max == 1 {
        return "\u{2026}".to_string();
    }
    let runes: Vec<char> = full.chars().collect();
    let mut used = 0usize;
    let mut end = 0usize;
    for (i, r) in runes.iter().enumerate() {
        let sz = r.len_utf16();
        if used + sz > max - 1 {
            break;
        }
        used += sz;
        end = i + 1;
    }
    let prefix: String = runes[..end].iter().collect();
    if !prefix.is_empty()
        && let Some(idx) = prefix.rfind(' ')
        && idx > 0
    {
        let base = strip_cut_trail(&prefix[..idx]);
        if base.is_empty() {
            return "\u{2026}".to_string();
        }
        // Sentence-end cut (matches `smart_trim_synopsis`): the last kept
        // character is `.`/`!`/`?` and the next character is whitespace or
        // end-of-text, so keep the complete sentence with its own terminator
        // and no ellipsis.
        if let Some(last) = base.chars().next_back()
            && matches!(last, '.' | '!' | '?')
        {
            let kept = base.chars().count();
            if (kept == runes.len() || runes[kept].is_whitespace())
                && utf16_units(base) <= max
            {
                return base.to_string();
            }
        }
        return format!("{base}\u{2026}");
    }
    // No word boundary in the prefix: still signal the cut. The already
    // sanitized `prefix` (char boundary, `max-1` units) carries a single
    // trailing U+2026 within budget; stripping only shrinks, so the budget
    // still holds and no surrogate pair is split.
    let base = strip_cut_trail(&prefix);
    if base.is_empty() {
        return "\u{2026}".to_string();
    }
    // Same sentence-end rule on the spaceless fallback: a character-boundary
    // cut landing on `.`/`!`/`?` followed by whitespace or end-of-text keeps
    // the sentence with no ellipsis.
    if let Some(last) = base.chars().next_back()
        && matches!(last, '.' | '!' | '?')
    {
        let kept = base.chars().count();
        if (kept == runes.len() || runes[kept].is_whitespace()) && utf16_units(base) <= max {
            return base.to_string();
        }
    }
    format!("{base}\u{2026}")
}

/// Trailing marks stripped from a cut BEFORE the U+2026 terminator is appended:
/// the clause separators (`,`, `;`, `:`), the dash family (`-`, `–`, `—`), the
/// middle dot joiner (`·`), and surrounding whitespace. A cut that stopped at
/// `Majin Buu,` or `meet Elliot;` therefore ends `Buu…` / `Elliot…`, never
/// `,…` / `;…`. Sentence-terminal `.`/`!`/`?` are deliberately NOT in this set:
/// the sentence-end path returns a complete sentence with its own terminator
/// and no ellipsis, and must keep it.
fn strip_cut_trail(s: &str) -> &str {
    let mut out = s;
    loop {
        let trimmed = out.trim_end_matches(|c: char| {
            c.is_whitespace() || matches!(c, ',' | ';' | ':' | '-' | '–' | '—' | '·')
        });
        if trimmed.len() == out.len() {
            return out;
        }
        out = trimmed;
    }
}

/// Words that must never dangle before the ellipsis (`presence.go:490-497`):
/// "... and" / "... the" reads as a cut-off thought, so they are stripped first.
const SYNOPSIS_CONNECTORS: [&str; 34] = [
    "and", "or", "but", "the", "a", "an", "of", "to", "in", "on", "with", "for", "from", "by",
    "as", "at", "into", "than", "that", "which", "while", "when", "his", "her", "their", "its",
    "this", "these", "those", "is", "are", "was", "were", "who",
];

/// The runes Go's `strings.Trim` cutset names (`presence.go:514`).
const CONNECTOR_TRIM: &str = ",;:.!?\"'()[]-.\u{b7}";

/// Drops trailing connector words (case-insensitive) from `base` so the ellipsis
/// never follows "and"/"the"/etc. (`presence.go:501`).
fn strip_dangling_connector(base: &str) -> String {
    let mut base = base.to_string();
    loop {
        let t = base.trim().to_string();
        if t.is_empty() {
            return String::new();
        }
        let idx = t.rfind(' ');
        let (last, rest) = match idx {
            Some(i) => (&t[i + 1..], &t[..i]),
            None => (t.as_str(), ""),
        };
        let clean = last
            .trim_matches(|c| CONNECTOR_TRIM.contains(c))
            .to_lowercase();
        if clean.is_empty() {
            return t;
        }
        if !SYNOPSIS_CONNECTORS.contains(&clean.as_str()) {
            return t;
        }
        if idx.is_none() {
            return String::new();
        }
        let next = rest.to_string();
        base = next;
    }
}

/// How many leading runes of `rs` fit in `max_units` UTF-16 units
/// (`presence.go:529`).
fn fit_runes(rs: &[char], max_units: usize) -> usize {
    if max_units == 0 {
        return 0;
    }
    let mut used = 0usize;
    for (i, r) in rs.iter().enumerate() {
        let sz = r.len_utf16();
        if used + sz > max_units {
            return i;
        }
        used += sz;
    }
    rs.len()
}

/// The smallest leading rune count of `rs[..end]` whose UTF-16 length reaches
/// `target_units` (the start of the back half) (`presence.go:549`).
fn rune_count_for_units(rs: &[char], end: usize, target_units: usize) -> usize {
    if target_units == 0 {
        return 0;
    }
    let mut used = 0usize;
    for (i, r) in rs.iter().enumerate().take(end) {
        used += r.len_utf16();
        if used >= target_units {
            return i + 1;
        }
    }
    0
}

/// `String.length()`: UTF-16 code units, the unit the Java-exact
/// [`titleclean::trunc`] limit is measured in.
fn utf16_units(s: &str) -> usize {
    s.encode_utf16().count()
}

/// [`trim_word`] with a seamless cut (Netflix: break after punctuation, single
/// U+2026; Discord: snippet, one line), all measured in UTF-16 units
/// (`presence.go:573`): fits unchanged, byte for byte; else last sentence end
/// (.!?, end-or-space) in the back half with NO ellipsis; else last clause
/// boundary (, ; space-dash variants) in the back half plus U+2026; else last
/// word boundary plus U+2026 (dangling connectors stripped first); else a
/// char-boundary hard cut to `max-1` units plus U+2026 (never a split
/// surrogate); never mid-word, never "...".
///
/// DEPARTURE from the frozen reference (`presence.go:573-644`): every cut that
/// takes the ellipsis also strips trailing punctuation first (see
/// [`trim_word`]), so the wire carries `Buu…`, never `Buu,…`. A truncated line
/// is always signalled — either a complete sentence that keeps its own
/// terminator with no ellipsis, or a single U+2026 that ends the line. A line
/// is never silently truncated.
pub(crate) fn smart_trim_synopsis(s: &str, max: usize) -> String {
    if max == 0 {
        return String::new();
    }
    let full = rp_text(s, HUGE);
    if utf16_units(&full) <= max {
        return full;
    }
    if max == 1 {
        return "\u{2026}".to_string();
    }
    let runes: Vec<char> = full.chars().collect();
    let end_fit = fit_runes(&runes, max);
    let half_fit = rune_count_for_units(&runes, end_fit, max / 2);
    for i in (half_fit..end_fit).rev() {
        let c = runes[i];
        if (c == '.' || c == '!' || c == '?')
            && (i + 1 == runes.len() || runes[i + 1].is_whitespace())
        {
            let cand = runes[..=i].iter().collect::<String>().trim().to_string();
            if utf16_units(&cand) <= max {
                return cand;
            }
        }
    }
    let end_ell = fit_runes(&runes, max - 1);
    let half_ell = rune_count_for_units(&runes, end_ell, (max - 1) / 2);
    let mut upper = end_ell;
    if upper >= runes.len() {
        upper = runes.len() - 1;
    }
    for i in (half_ell..=upper).rev() {
        if i < end_ell
            && (runes[i] == ',' || runes[i] == ';')
            && i + 1 < runes.len()
            && runes[i + 1].is_whitespace()
        {
            let cleaned = strip_dangling_connector(
                runes[..=i].iter().collect::<String>().trim(),
            );
            let base = strip_cut_trail(&cleaned);
            if base.is_empty() {
                return "\u{2026}".to_string();
            }
            return format!("{base}\u{2026}");
        }
        if (runes[i] == '-' || runes[i] == '\u{2013}' || runes[i] == '\u{2014}')
            && i >= 1
            && i + 1 < runes.len()
            && runes[i - 1] == ' '
            && runes[i + 1].is_whitespace()
        {
            let start = i - 1;
            if start >= half_ell && start < end_ell {
                let cleaned = strip_dangling_connector(
                    runes[..start].iter().collect::<String>().trim(),
                );
                let base = strip_cut_trail(&cleaned);
                if base.is_empty() {
                    return "\u{2026}".to_string();
                }
                return format!("{base}\u{2026}");
            }
        }
    }
    for i in (0..end_ell).rev() {
        if runes[i] == ' ' {
            let cleaned =
                strip_dangling_connector(runes[..i].iter().collect::<String>().trim());
            let base = strip_cut_trail(&cleaned);
            if base.is_empty() {
                return "\u{2026}".to_string();
            }
            return format!("{base}\u{2026}");
        }
    }
    // No boundary anywhere: still signal the cut. Hard-cut the sanitized text
    // on a char boundary to `max-1` units with a single trailing U+2026,
    // staying within budget without splitting a surrogate pair.
    let prefix: String = runes[..end_ell].iter().collect();
    let base = strip_cut_trail(&prefix);
    if base.is_empty() {
        return "\u{2026}".to_string();
    }
    format!("{base}\u{2026}")
}

/// The header identity: the canonical series/movie title, else the clean
/// filename title. Empty only for the idle card (the caller returns early).
fn compose_name_inner(raw_title: &str, r: &Resolved) -> String {
    let raw = raw_title.strip_suffix(SUFFIX).unwrap_or(raw_title);
    if raw.trim().is_empty() || raw == APP_NAME {
        return String::new();
    }
    if !r.series.is_empty() {
        return trunc(&r.series, NAME_BUDGET);
    }
    let title = trunc(&clean(raw).title, NAME_BUDGET);
    if title.is_empty() {
        rp_text(raw.trim(), NAME_BUDGET)
    } else {
        title
    }
}

/// The hover slot: the whole synopsis (word-aware, [`LARGE_TEXT_BUDGET`]), else
/// the canonical series title, else "PotFlix".
fn compose_large_text_inner(r: &Resolved) -> String {
    let s = trim_word(&r.synopsis, LARGE_TEXT_BUDGET);
    if !s.is_empty() {
        return s;
    }
    if !r.series.is_empty() {
        return trunc(&r.series, LARGE_TEXT_BUDGET);
    }
    "PotFlix".to_string()
}

/// The artwork slot: the https URL when present and within
/// [`LARGE_IMAGE_BUDGET`], else "logo". Never empty, never truncated, never
/// over budget (Discord rejects `large_image` longer than 300 chars).
fn compose_large_image_inner(r: &Resolved) -> String {
    if (r.artwork.starts_with("http://") || r.artwork.starts_with("https://"))
        && r.artwork.len() <= LARGE_IMAGE_BUDGET
    {
        r.artwork.clone()
    } else {
        "logo".to_string()
    }
}

/// One state-ladder rung: the filename episode tag, or "" when absent. A
/// present rung already carries the paused marker when paused.
fn rung_tag(raw_title: &str, a: Option<&Action>) -> String {
    let raw = raw_title.strip_suffix(SUFFIX).unwrap_or(raw_title);
    let mut tag_raw = String::new();
    if !(raw.trim().is_empty() || raw == APP_NAME) {
        tag_raw = clean(raw).episode_tag;
    }
    let tag = rp_text(&tag_raw, STATE_BUDGET);
    if tag.is_empty() {
        return String::new();
    }
    if a.is_some_and(|x| x.paused) {
        with_state_time(&tag, PAUSED_MARKER)
    } else {
        tag
    }
}

/// One state-ladder rung: genres and length joined with [`JOINER`], or "" when
/// both are absent.
fn rung_genres_length(r: &Resolved, a: Option<&Action>) -> String {
    let g = rp_text(&r.genres, HUGE);
    let l = rp_text(&r.length, HUGE);
    let mut parts: Vec<&str> = Vec::new();
    if !g.is_empty() {
        parts.push(&g);
    }
    if !l.is_empty() {
        parts.push(&l);
    }
    let prefix = parts.join(JOINER);
    if prefix.is_empty() {
        return String::new();
    }
    if a.is_some_and(|x| x.paused) {
        with_state_time(
            &trim_word(&prefix, STATE_BUDGET - PAUSED_MARKER.len()),
            PAUSED_MARKER,
        )
    } else {
        trim_word(&prefix, STATE_BUDGET)
    }
}

/// One state-ladder rung: the synopsis alone at [`STATE_SYNOPSIS_BUDGET`], or
/// "" when absent.
fn rung_synopsis_alone(r: &Resolved, a: Option<&Action>) -> String {
    if rp_text(&r.synopsis, HUGE).is_empty() {
        return String::new();
    }
    if a.is_some_and(|x| x.paused) {
        with_state_time(
            &smart_trim_synopsis(&r.synopsis, STATE_SYNOPSIS_BUDGET - PAUSED_MARKER.len()),
            PAUSED_MARKER,
        )
    } else {
        smart_trim_synopsis(&r.synopsis, STATE_SYNOPSIS_BUDGET)
    }
}

/// One state-ladder rung: the movie year, or "" when `year <= 0`.
fn rung_year(r: &Resolved, a: Option<&Action>) -> String {
    if r.year <= 0 {
        return String::new();
    }
    let y = r.year.to_string();
    if a.is_some_and(|x| x.paused) {
        with_state_time(&y, PAUSED_MARKER)
    } else {
        y
    }
}

/// The tail rung of every ladder: the clean filename title, so a slot is never
/// blank. Falls back to the sanitized raw title when the parser yields nothing.
fn rung_clean_title(raw_title: &str, a: Option<&Action>) -> String {
    let raw = raw_title.strip_suffix(SUFFIX).unwrap_or(raw_title);
    if raw.trim().is_empty() || raw == APP_NAME {
        return String::new();
    }
    let title = trunc(&clean(raw).title, STATE_BUDGET);
    let base = if title.is_empty() {
        rp_text(raw.trim(), STATE_BUDGET)
    } else {
        title
    };
    if base.is_empty() {
        return String::new();
    }
    if a.is_some_and(|x| x.paused) {
        with_state_time(&base, PAUSED_MARKER)
    } else {
        base
    }
}

/// The details fill ladder (never blank outside idle). Series: the canonical
/// episode name, else the filename episode line (name, else tag, else clean
/// title). Movie: `Title (YYYY)` when `year > 0`, else the title, else the
/// filename line. The `title` preset is the header identity; `episode` is the
/// episode name else the ladder; `series` is the canonical series (`(YYYY)`
/// appended for a movie) else the ladder; `default` is today's behavior.
pub(crate) fn compose_details_for(
    raw_title: &str,
    r: &Resolved,
    is_movie: bool,
    details_preset: &str,
) -> String {
    let raw = raw_title.strip_suffix(SUFFIX).unwrap_or(raw_title);
    let c = clean(raw);
    let clean_title = trunc(&c.title, DETAILS_BUDGET);
    let file_ep = trunc(&c.episode_name, DETAILS_BUDGET);
    let file_tag = trunc(&c.episode_tag, DETAILS_BUDGET);
    let mut file_line = clean_title.clone();
    if !file_ep.is_empty() {
        file_line = file_ep;
    } else if !file_tag.is_empty() {
        file_line = file_tag;
    }
    if file_line.is_empty() {
        file_line = rp_text(raw.trim(), DETAILS_BUDGET);
    }
    let canonical_ep = if r.episode_name.is_empty() {
        String::new()
    } else {
        trunc(&r.episode_name, DETAILS_BUDGET)
    };
    let canonical_series = if r.series.is_empty() {
        String::new()
    } else {
        trunc(&r.series, DETAILS_BUDGET)
    };
    let canonical_series_year = if !r.series.is_empty() && is_movie && r.year > 0 {
        trunc(&format!("{} ({})", r.series, r.year), DETAILS_BUDGET)
    } else {
        canonical_series.clone()
    };
    let default_line = if is_movie {
        if !canonical_series.is_empty() {
            canonical_series_year.clone()
        } else {
            file_line.clone()
        }
    } else if !canonical_ep.is_empty() {
        canonical_ep.clone()
    } else {
        file_line.clone()
    };
    match details_preset {
        cardspec::CARD_DETAILS_TITLE => {
            let header = compose_name_inner(raw_title, r);
            if header.is_empty() {
                default_line
            } else {
                header
            }
        }
        cardspec::CARD_DETAILS_EPISODE => {
            if !canonical_ep.is_empty() {
                canonical_ep
            } else {
                default_line
            }
        }
        cardspec::CARD_DETAILS_SERIES => {
            if !canonical_series.is_empty() {
                canonical_series_year
            } else {
                default_line
            }
        }
        _ => default_line,
    }
}

/// The state fill ladder (first non-empty rung wins; only `none` may empty the
/// line). Series: `default` is the tag alone, else the clean title, else the
/// synopsis only as the last resort so the line can never go empty; `tag` is
/// tag, else synopsis, else clean; `synopsis` is tag-plus-synopsis, else tag,
/// else clean. Movie: `default` is genres-plus-length, else the year, else the
/// clean title, else the synopsis only as the last resort; `tag` is the
/// filename tag, else genres-plus-length, else synopsis, else clean;
/// `synopsis` is genres-length-plus-synopsis, else synopsis, else clean. Every
/// rung carries the paused marker when paused; paused with nothing reads
/// "Paused".
pub(crate) fn compose_state_for(
    raw_title: &str,
    a: Option<&Action>,
    r: &Resolved,
    is_movie: bool,
    state_preset: &str,
) -> String {
    if state_preset == cardspec::CARD_STATE_NONE {
        return String::new();
    }
    let paused = a.is_some_and(|x| x.paused);
    // The combined rung follows the explicit kind, not the stored flag. It is
    // only live when a synopsis exists: `synopsis_state` parks an empty
    // synopsis as "Paused", which must fall through the ladder instead.
    let mut shadow = r.clone();
    shadow.is_movie = is_movie;
    let combined = if rp_text(&r.synopsis, HUGE).is_empty() {
        String::new()
    } else {
        synopsis_state(raw_title, a, &shadow)
    };
    let tag = rung_tag(raw_title, a);
    let synopsis = rung_synopsis_alone(r, a);
    let clean = rung_clean_title(raw_title, a);
    if is_movie {
        match state_preset {
            cardspec::CARD_STATE_TAG => {
                if !tag.is_empty() {
                    return tag;
                }
                let gl = rung_genres_length(r, a);
                if !gl.is_empty() {
                    return gl;
                }
                if !synopsis.is_empty() {
                    return synopsis;
                }
                if !clean.is_empty() {
                    return clean;
                }
            }
            cardspec::CARD_STATE_SYNOPSIS => {
                if !combined.is_empty() {
                    return combined;
                }
                if !synopsis.is_empty() {
                    return synopsis;
                }
                if !clean.is_empty() {
                    return clean;
                }
            }
            _ => {
                let gl = rung_genres_length(r, a);
                if !gl.is_empty() {
                    return gl;
                }
                let year = rung_year(r, a);
                if !year.is_empty() {
                    return year;
                }
                if !clean.is_empty() {
                    return clean;
                }
                // Never blank: the synopsis is the very last resort, only
                // reached when even the clean title yields nothing.
                if !synopsis.is_empty() {
                    return synopsis;
                }
            }
        }
    } else {
        match state_preset {
            cardspec::CARD_STATE_TAG => {
                if !tag.is_empty() {
                    return tag;
                }
                if !synopsis.is_empty() {
                    return synopsis;
                }
                if !clean.is_empty() {
                    return clean;
                }
            }
            cardspec::CARD_STATE_SYNOPSIS => {
                if !combined.is_empty() {
                    return combined;
                }
                if !tag.is_empty() {
                    return tag;
                }
                if !clean.is_empty() {
                    return clean;
                }
            }
            _ => {
                if !tag.is_empty() {
                    return tag;
                }
                if !clean.is_empty() {
                    return clean;
                }
                // Never blank: the synopsis is the very last resort, only
                // reached when even the clean title yields nothing.
                if !synopsis.is_empty() {
                    return synopsis;
                }
            }
        }
    }
    if paused {
        return with_state_time("", PAUSED_MARKER);
    }
    String::new()
}

/// The single card composer the app's push path and the Card-view preview both
/// call (`presence.go:662`): one always-filled card with a per-kind fill
/// ladder, so a card never ships an empty body line. `is_movie` drives the
/// kind branches; the idle card keeps its shipped lines under any spec.
pub fn compose_card(
    raw_title: &str,
    a: Option<&Action>,
    r: &Resolved,
    is_movie: bool,
    spec: &CardSpec,
) -> Activity {
    let raw = raw_title.strip_suffix(SUFFIX).unwrap_or(raw_title);
    if raw.trim().is_empty() || raw == APP_NAME {
        return Activity {
            type_: "PLAYING".to_string(),
            details: "Browsing".to_string(),
            state: "Idle".to_string(),
            large_image: "logo".to_string(),
            large_text: "PotFlix".to_string(),
            ..Activity::default()
        };
    }
    let (details, state) = spec.render_kind(raw_title, a, r, is_movie);
    let name = compose_name_inner(raw_title, r);
    let mut act = Activity {
        type_: "PLAYING".to_string(),
        details,
        state,
        large_image: compose_large_image_inner(r),
        large_text: compose_large_text_inner(r),
        paused: a.is_some_and(|x| x.paused),
        ..Activity::default()
    };
    if ACTIVITY_NAME_HEADER {
        act.name = name;
    }
    if let Some(x) = a
        && x.has_start
    {
        act.has_start = true;
        act.start = x.start;
    }
    act
}

/// [`compose_card`] with the stored kind and the shipped default spec
/// (`presence.go:662`).
///
/// Movie (`r.is_movie`): details is the canonical title with " (YYYY)" when
/// `year > 0`, else the filename line; state is genres-plus-length, else the
/// year, else the clean title, else the synopsis only as the last resort, so
/// the line is never blank. Series: details is the episode override when
/// present (else the filename-derived line); state is the episode tag alone,
/// else the clean title, else the synopsis only as the last resort. The hover
/// tooltip carries the whole synopsis; the paused " - Paused" marker path is
/// preserved on every rung.
pub fn build_activity_resolved(raw_title: &str, a: Option<&Action>, r: &Resolved) -> Activity {
    compose_card(raw_title, a, r, r.is_movie, &cardspec::default_card_spec())
}

/// The synopsis-bearing state line: a movie joins genres, length and the
/// synopsis with " \u{b7} ", a series joins the episode tag and the synopsis
/// the same way, both capped word-aware at [`STATE_SYNOPSIS_BUDGET`]. It backs
/// the combined rung of the [`compose_state_for`] ladder under the
/// [`cardspec::CARD_STATE_SYNOPSIS`] preset; the default carries no synopsis.
/// With no synopsis the slot hides (`""`, `"Paused"` when paused), so the
/// preset honors the empty-hides contract (`cardspec_test::card_spec_empty_hide`).
pub(crate) fn synopsis_state(raw_title: &str, a: Option<&Action>, r: &Resolved) -> String {
    let paused = a.is_some_and(|x| x.paused);
    let s_full = rp_text(&r.synopsis, HUGE);
    if s_full.is_empty() {
        if paused {
            return with_state_time("", PAUSED_MARKER);
        }
        return String::new();
    }
    if r.is_movie {
        let g = rp_text(&r.genres, HUGE);
        let l = rp_text(&r.length, HUGE);
        let mut prefix_parts: Vec<&str> = Vec::new();
        if !g.is_empty() {
            prefix_parts.push(&g);
        }
        if !l.is_empty() {
            prefix_parts.push(&l);
        }
        let prefix = prefix_parts.join(" \u{b7} ");
        let join_budget = if paused {
            STATE_SYNOPSIS_BUDGET - PAUSED_MARKER.len()
        } else {
            STATE_SYNOPSIS_BUDGET
        };
        let sep = if prefix.is_empty() { "" } else { " \u{b7} " };
        let prefix_units = utf16_units(&prefix);
        let sep_units = utf16_units(sep);
        if prefix_units + sep_units >= join_budget {
            let full_joined = format!("{prefix}{sep}{s_full}");
            return if paused {
                with_state_time(&trim_word(&full_joined, join_budget), PAUSED_MARKER)
            } else {
                trim_word(&full_joined, join_budget)
            };
        }
        let trimmed = smart_trim_synopsis(&s_full, join_budget - prefix_units - sep_units);
        let joined = format!("{prefix}{sep}{trimmed}");
        if paused {
            with_state_time(&joined, PAUSED_MARKER)
        } else {
            joined
        }
    } else {
        let raw = raw_title.strip_suffix(SUFFIX).unwrap_or(raw_title);
        let mut tag_raw = String::new();
        if !(raw.trim().is_empty() || raw == APP_NAME) {
            tag_raw = clean(raw).episode_tag;
        }
        let tag_t = rp_text(&tag_raw, STATE_BUDGET);
        if tag_t.is_empty() {
            if paused {
                return with_state_time(
                    &smart_trim_synopsis(&s_full, STATE_SYNOPSIS_BUDGET - PAUSED_MARKER.len()),
                    PAUSED_MARKER,
                );
            }
            return smart_trim_synopsis(&s_full, STATE_SYNOPSIS_BUDGET);
        }
        if paused {
            let join_budget = STATE_SYNOPSIS_BUDGET - PAUSED_MARKER.len();
            let sep = " \u{b7} ";
            let prefix_units = utf16_units(&tag_t);
            let sep_units = utf16_units(sep);
            if prefix_units + sep_units >= join_budget {
                return with_state_time(
                    &trim_word(&format!("{tag_t}{sep}{s_full}"), join_budget),
                    PAUSED_MARKER,
                );
            }
            let trimmed = smart_trim_synopsis(&s_full, join_budget - prefix_units - sep_units);
            return with_state_time(&format!("{tag_t}{sep}{trimmed}"), PAUSED_MARKER);
        }
        let join_budget = STATE_SYNOPSIS_BUDGET;
        let prefix_units = utf16_units(&tag_t);
        let sep_units = utf16_units(" \u{b7} ");
        if prefix_units + sep_units >= join_budget {
            trim_word(&format!("{tag_t} \u{b7} {s_full}"), join_budget)
        } else {
            let trimmed = smart_trim_synopsis(&s_full, join_budget - prefix_units - sep_units);
            format!("{tag_t} \u{b7} {trimmed}")
        }
    }
}

// Go callers write `presence.CardSpec`; the module path `cardspec::CardSpec` is
// the canonical one.
pub use cardspec::{CardPreset, CardSpec};
