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
/// as-is; otherwise the prefix fitting in `max-1` units is cut at its last space
/// and a single U+2026 is appended, so a synopsis never ends mid-word. With no
/// space in the prefix it falls back to `rp_text`'s hard cut.
///
/// DEPARTURE from the frozen reference (`presence.go:454`): the cut base is
/// stripped of trailing punctuation before the ellipsis is appended. Go kept
/// the comma/semicolon the clause cut stopped at, so the wire carried
/// `Majin Buu,…` / `meet Elliot;…` (and a mid-word `…and Ernest` rode the
/// tooltip's hard cut at the call site); here every truncated line ends on a
/// word character plus U+2026, or on nothing extra when nothing was cut.
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
        return format!("{base}\u{2026}");
    }
    rp_text(s, max)
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
/// (`presence.go:573`): fits unchanged; else last sentence end (.!?, end-or-space)
/// in the back half with NO ellipsis; else last clause boundary (, ; space-dash
/// variants) in the back half plus U+2026; else last word boundary plus U+2026
/// (dangling connectors stripped first); never mid-word, never "...".
///
/// DEPARTURE from the frozen reference (`presence.go:573-644`): every cut that
/// takes the ellipsis also strips trailing punctuation first (see
/// [`trim_word`]), so the wire carries `Buu…`, never `Buu,…`.
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
    rp_text(s, max)
}

/// [`build_activity`] with canonical title overrides applied
/// (`presence.go:662`).
///
/// Movie (`r.is_movie`): details is the canonical title with " (YYYY)" when
/// `year > 0`; state joins genres, length and the synopsis with " \u{b7} ",
/// skipping empties, capped word-aware at 128. Series: details is the episode
/// override when present (else [`build_activity`]'s filename-derived line); state
/// joins the episode tag and the synopsis the same way, with the synopsis
/// word-capped. The paused " - Paused" marker path is preserved for both.
pub fn build_activity_resolved(raw_title: &str, a: Option<&Action>, r: &Resolved) -> Activity {
    let mut act = build_activity(raw_title, a);
    let paused = a.is_some_and(|x| x.paused);
    if !r.series.is_empty() {
        act.name = trunc(&r.series, 128);
        act.large_text = trunc(&r.series, 128);
    }
    // Hover slot: assets.large_text is the poster tooltip. `name` already carries
    // the series title as the card header, so repeating it here wastes the slot;
    // carry the complete short synopsis instead (capped at Discord's 128-unit
    // asset-text cap, so it arrives whole). The cap is word-aware with a U+2026
    // terminator when cut (never the hard mid-word cut `rp_text` does: the live
    // card showed `...Zak Penn and Ernest`). No synopsis: keep today's fallback
    // (series title when known, else "PotFlix").
    let s = trim_word(&r.synopsis, 128);
    if !s.is_empty() {
        act.large_text = s;
    }
    if r.is_movie {
        if !r.series.is_empty() {
            if r.year > 0 {
                act.details = trunc(&format!("{} ({})", r.series, r.year), 128);
            } else {
                act.details = trunc(&r.series, 128);
            }
        }
        let g = rp_text(&r.genres, HUGE);
        let l = rp_text(&r.length, HUGE);
        let s_full = rp_text(&r.synopsis, HUGE);
        let mut prefix_parts: Vec<&str> = Vec::new();
        if !g.is_empty() {
            prefix_parts.push(&g);
        }
        if !l.is_empty() {
            prefix_parts.push(&l);
        }
        let prefix = prefix_parts.join(" \u{b7} ");
        if prefix.is_empty() && s_full.is_empty() {
            // Empty join: keep build_activity's state (including any Paused
            // marker).
        } else if s_full.is_empty() {
            // No synopsis: keep the 128 ceiling (non-synopsis states untouched).
            act.state = if paused {
                with_state_time(
                    &trim_word(&prefix, 128 - PAUSED_MARKER.len()),
                    PAUSED_MARKER,
                )
            } else {
                trim_word(&prefix, 128)
            };
        } else {
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
                act.state = if paused {
                    with_state_time(&trim_word(&full_joined, join_budget), PAUSED_MARKER)
                } else {
                    trim_word(&full_joined, join_budget)
                };
            } else {
                let trimmed = smart_trim_synopsis(&s_full, join_budget - prefix_units - sep_units);
                let joined = format!("{prefix}{sep}{trimmed}");
                act.state = if paused {
                    with_state_time(&joined, PAUSED_MARKER)
                } else {
                    joined
                };
            }
        }
        // Empty join: keep build_activity's state (including any Paused marker).
    } else {
        if !r.episode_name.is_empty() {
            act.details = trunc(&r.episode_name, 128);
        }
        // Episode tag for the state line, derived from the title the same way
        // build_activity derives it.
        let raw = raw_title.strip_suffix(SUFFIX).unwrap_or(raw_title);
        let mut tag_raw = String::new();
        if !(raw.trim().is_empty() || raw == APP_NAME) {
            tag_raw = clean(raw).episode_tag;
        }
        let tag_t = rp_text(&tag_raw, 128);
        let syn_full = rp_text(&r.synopsis, HUGE);
        if tag_t.is_empty() && syn_full.is_empty() {
            // Neither: keep build_activity's state (including any Paused marker).
        } else if paused {
            if syn_full.is_empty() {
                // Tag-only paused: no synopsis, keep the 128 ceiling.
                act.state =
                    with_state_time(&trim_word(&tag_t, 128 - PAUSED_MARKER.len()), PAUSED_MARKER);
            } else {
                let join_budget = STATE_SYNOPSIS_BUDGET - PAUSED_MARKER.len();
                let sep = if tag_t.is_empty() { "" } else { " \u{b7} " };
                let prefix_units = utf16_units(&tag_t);
                let sep_units = utf16_units(sep);
                if prefix_units + sep_units >= join_budget {
                    act.state = with_state_time(
                        &trim_word(&format!("{tag_t}{sep}{syn_full}"), join_budget),
                        PAUSED_MARKER,
                    );
                } else {
                    let trimmed =
                        smart_trim_synopsis(&syn_full, join_budget - prefix_units - sep_units);
                    act.state = with_state_time(&format!("{tag_t}{sep}{trimmed}"), PAUSED_MARKER);
                }
            }
        } else if !tag_t.is_empty() && !syn_full.is_empty() {
            let join_budget = STATE_SYNOPSIS_BUDGET;
            let prefix_units = utf16_units(&tag_t);
            let sep_units = utf16_units(" \u{b7} ");
            if prefix_units + sep_units >= join_budget {
                act.state = trim_word(&format!("{tag_t} \u{b7} {syn_full}"), join_budget);
            } else {
                let trimmed =
                    smart_trim_synopsis(&syn_full, join_budget - prefix_units - sep_units);
                act.state = format!("{tag_t} \u{b7} {trimmed}");
            }
        } else if !tag_t.is_empty() {
            act.state = tag_t;
        } else {
            act.state = smart_trim_synopsis(&syn_full, STATE_SYNOPSIS_BUDGET);
        }
    }
    if r.artwork.starts_with("http://") || r.artwork.starts_with("https://") {
        act.large_image = r.artwork.clone();
    }
    // Otherwise large_image stays as build_activity produced it ("logo"): an
    // empty large image is never emitted.
    // No buttons: the pristine card carries no click-through links, so
    // Resolved::buttons is always ignored.
    act
}

// Go callers write `presence.CardSpec`; the module path `cardspec::CardSpec` is
// the canonical one.
pub use cardspec::{CardPreset, CardSpec};
