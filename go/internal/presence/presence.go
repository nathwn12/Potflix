package presence

import (
	"fmt"
	"regexp"
	"strings"

	"potflix/internal/player"
	"potflix/internal/titleclean"
)

// presence.go - the presence state machine and activity payload builder.
//
// Ported from me.potflix.rpc.Main (PresenceTracker.poll :265-309,
// withStateTime :313-317, pushActivity :139-191, fmt :501-508), with one
// deliberate deviation from the fleet decisions (see Tracker and the README):
// repeated RUN pushes within ~1 s are debounced unless the title changed. The
// debounce is trailing-edge: the suppressed anchor is HELD and emitted when the
// window expires, so a seek is never dropped forever.

// Timing constants, exactly as Java (:196-202).
const (
	// SeekJumpMS: a per-poll ct jump above this is a real seek.
	SeekJumpMS int64 = 1500
	// SettleMoveMS: ct advance across the window that proves real movement.
	SettleMoveMS int64 = 50
	// MoveWindow is the number of ct samples kept to prove real movement.
	MoveWindow = 3
)

// RunDebounceMS is the fleet-decision-10 deviation: a repeated RUN push within
// this window, with an unchanged title, is suppressed.
const RunDebounceMS int64 = 1000

// Action is one emission decision (Java PresenceAction). A nil *Action means
// "emit nothing". CT is the sample carried into the payload/log.
type Action struct {
	Running  bool
	Paused   bool
	HasStart bool
	Start    int64  // epoch ms, meaningful when HasStart
	Suffix   string // legacy position suffix; no longer populated (the paused card uses a "Paused" marker)
	CT       int64
}

// Tag is Java describe()'s leading token: RUN / PAUSE / STOP.
func (a *Action) Tag() string {
	switch {
	case a == nil:
		return "null"
	case a.Running:
		return "RUN"
	case a.Paused:
		return "PAUSE"
	default:
		return "STOP"
	}
}

// Tracker is one PresenceTracker. Its zero value is ready to use; the Java ctor
// state is reproduced by havePrev=false / hasLastStatus=false.
type Tracker struct {
	lastTitle     string
	lastStatus    player.PlayStatus
	hasLastStatus bool  // Java's `lastStatus != null`
	prevCt        int64 // last VALID ct, used only as the seek baseline; -1 = none
	frozenCt      int64 // position held in the paused text; -1 = none
	havePrev      bool

	ctWindow    [MoveWindow]int64
	ctWindowLen int
	ctWindowIdx int

	// Fleet decision 10 (deviation, revised): RUN debounce anchor. A repeated
	// RUN push inside the window is HELD (pendingRun), not dropped, and emitted
	// on the first poll after the window expires (trailing edge).
	hasRunPush       bool
	lastRunPushAt    int64
	lastRunPushTitle string
	pendingRun       bool
	pendingTitle     string

	// lastAction is the most recent emitted action. Current() exposes it so the
	// send gate can rebuild the card on every poll, including polls where the
	// tracker emits nothing.
	lastAction *Action

	// clock is the monotonic interval clock for the trailing-edge debounce, so a
	// wall-clock step cannot strand a held seek.
	clock intervalClock
}

func (t *Tracker) sampleCt(ct int64) {
	t.ctWindow[t.ctWindowIdx] = ct
	t.ctWindowIdx = (t.ctWindowIdx + 1) % MoveWindow
	if t.ctWindowLen < MoveWindow {
		t.ctWindowLen++
	}
}

// oldestCt is the minimum of the sampled window; Java returns Long.MAX_VALUE
// when empty, but `moving` already requires len>=2.
func (t *Tracker) oldestCt() int64 {
	var m int64
	for i := 0; i < t.ctWindowLen; i++ {
		if i == 0 || t.ctWindow[i] < m {
			m = t.ctWindow[i]
		}
	}
	return m
}

// Poll consumes one sample and returns the action to push, or nil. ps/ct are
// the raw reads; now is epoch ms.
func (t *Tracker) Poll(rawTitle string, ps player.PlayStatus, ct, now int64) *Action {
	// Interval bookkeeping (the trailing-edge debounce) runs on a monotonic
	// clock; anchors below keep the epoch now.
	clk := t.clock.tick(now)

	// New media: forget everything (Java :267-270).
	if t.havePrev && t.lastTitle != rawTitle {
		t.havePrev = false
		t.hasLastStatus = false
		t.lastStatus = 0
		t.frozenCt = -1
		t.prevCt = -1
		t.ctWindowLen = 0
		t.ctWindowIdx = 0
		t.pendingRun = false
		t.lastAction = nil
	}

	// A not-ready read (ct<=0) must NOT overwrite the seek baseline (:273-275).
	ctOk := ct > 0
	prevCtBefore := t.prevCt
	if ctOk {
		t.sampleCt(ct)
		t.prevCt = ct
	}

	// ct ticks are coarse, so one delta is not proof of movement (:278).
	moving := ctOk && t.ctWindowLen >= 2 && (ct-t.oldestCt()) >= SettleMoveMS

	// Undefined carries no information: emit nothing, change no state (:281).
	if ps == player.StatusUndefined {
		t.havePrev = true
		return nil
	}

	// A non-Running report while the position is still moving, having settled
	// into Running, is a transient glitch: the player is really playing (:285-286).
	eff := ps
	if ps != player.StatusRunning && moving && t.hasLastStatus && t.lastStatus == player.StatusRunning {
		eff = player.StatusRunning
	}

	changed := !t.havePrev || !t.hasLastStatus || eff != t.lastStatus
	seeked := !changed && eff == player.StatusRunning && t.hasLastStatus && t.lastStatus == player.StatusRunning &&
		t.havePrev && ctOk && abs64(ct-prevCtBefore) > SeekJumpMS
	pausedSeek := !changed && eff == player.StatusPaused && t.hasLastStatus && t.lastStatus == player.StatusPaused &&
		t.frozenCt >= 0 && ctOk && abs64(ct-t.frozenCt) > SeekJumpMS

	var action *Action
	switch {
	case eff == player.StatusRunning:
		if (changed || seeked) && ctOk {
			action = &Action{Running: true, HasStart: true, Start: now - ct, CT: ct}
		}
	case eff == player.StatusPaused:
		if (changed || pausedSeek) && ctOk {
			// No position suffix any more: the paused card carries a "Paused"
			// marker and counts the pause duration UP from timestamps.start
			// (set by the send gate), so there is no frozen " - 00:00" artifact.
			action = &Action{Paused: true, CT: ct}
			t.frozenCt = ct
		}
	default: // Stopped
		if changed {
			action = &Action{CT: ct}
		}
	}

	// Fleet decision 10 (revised): debounce repeated RUN pushes (seek bursts)
	// within ~1 s unless the title changed, but HOLD the suppressed anchor
	// instead of dropping it. The old drop-then-forget behaviour lost the seek
	// re-anchor forever: the per-poll delta never re-fires, so the card kept the
	// pre-seek anchor. A `changed` RUN push is a real state/title transition and
	// always emits.
	if action != nil {
		if action.Running {
			if !changed && t.hasRunPush && (clk-t.lastRunPushAt) < RunDebounceMS && rawTitle == t.lastRunPushTitle {
				t.pendingRun = true
				t.pendingTitle = rawTitle
				action = nil
			} else {
				t.hasRunPush = true
				t.lastRunPushAt = clk
				t.lastRunPushTitle = rawTitle
				t.pendingRun = false
			}
		} else {
			// Any non-RUN emit supersedes a held seek.
			t.pendingRun = false
		}
	}

	// Trailing edge: once the window has expired the held seek lands with a fresh
	// anchor (start = now - ct), so the last seek of a scrub is never lost.
	if action == nil && t.pendingRun && ctOk && eff == player.StatusRunning &&
		rawTitle == t.pendingTitle && (clk-t.lastRunPushAt) >= RunDebounceMS {
		action = &Action{Running: true, HasStart: true, Start: now - ct, CT: ct}
		t.hasRunPush = true
		t.lastRunPushAt = clk
		t.lastRunPushTitle = rawTitle
		t.pendingRun = false
	}

	// On emit only, remember title/status; havePrev is always set (:306-308).
	if action != nil {
		t.lastTitle = rawTitle
		t.lastStatus = eff
		t.hasLastStatus = true
		t.lastAction = action
	}
	t.havePrev = true
	return action
}

// Current returns the most recent emitted action, or nil before any emit. The
// send gate uses it to rebuild the card on every poll, including polls where the
// tracker emits nothing.
func (t *Tracker) Current() *Action {
	if t.lastAction == nil {
		return nil
	}
	c := *t.lastAction
	return &c
}

// PendingDebounce reports whether a RUN push was suppressed by the trailing-edge
// debounce and is waiting for the window to expire.
func (t *Tracker) PendingDebounce() bool { return t.pendingRun }

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// Fmt mirrors Main.fmt: MM:SS under an hour, else H:MM:SS (:501-508).
func Fmt(ms int64) string {
	seconds := ms / 1000
	s := seconds % 60
	m := (seconds / 60) % 60
	h := seconds / 3600
	if h == 0 {
		return fmt.Sprintf("%02d:%02d", m, s)
	}
	return fmt.Sprintf("%d:%02d:%02d", h, m, s)
}

// pausedMarker is the paused card's state suffix: episode context plus this
// reads e.g. `S1 E2 - Paused` (or just `Paused` with no context).
const pausedMarker = " - Paused"

// activityNameHeader enables the SET_ACTIVITY `name` header override: the card
// header becomes "Watching <title>" instead of the Discord app name.
// REVERT IN ONE EDIT: set this to false to drop the override entirely (the
// header then falls back to the app name); nothing else needs to change.
const activityNameHeader = true

// leadingDashRe is Java's `^\s*-\s*` with Java's \s class.
var leadingDashRe = regexp.MustCompile(`^[ \t\n\x0B\f\r]*-[ \t\n\x0B\f\r]*`)

// WithStateTime mirrors Main.withStateTime (:313-317): context plus a suffix; a
// leading dash is stripped when there is no context. Used for the paused card's
// "Paused" marker (pausedMarker). Deviation from the Java reference: the Java
// code truncates context+suffix together, which let the 128-unit cap eat the
// tail of the marker (a 120-unit context + the 9-unit " - Paused" rendered
// "... - Paus..."). Here the context is budgeted to limit - len(suffix) first, so
// the suffix always survives whole and the result stays within the limit.
func WithStateTime(context, suffix string) string {
	if suffix == "" {
		return context
	}
	if context == "" {
		return leadingDashRe.ReplaceAllString(suffix, "")
	}
	return titleclean.Trunc(context, 128-len(suffix)) + suffix
}

// Activity is the Discord payload (contract section 5). Type is always
// "PLAYING"; the fields not listed are deliberately unset.
//
// Netflix-style mapping (no buttons): the header (Name) is the media title,
// the first body line (Details) is the episode name when known, else the
// episode tag, else the title, and the second body line (State) is the episode
// tag when it adds information, else empty. LargeText names the app
// ("PotFlix"); SmallText names the playback state. Name is governed by
// activityNameHeader: empty falls back to the Discord app name header.
type Activity struct {
	Type       string
	Name       string
	Details    string
	State      string
	LargeImage string
	LargeText  string
	SmallImage string
	SmallText  string
	HasStart   bool
	Start      int64
	// End is the projected end (epoch ms), set by the send gate while running
	// and the duration is known. Serialized as timestamps.end while playing so
	// Discord renders the remaining-time bar; zero (and omitted) while paused.
	End int64
}

// BuildActivity composes the payload for one action from the raw window title
// (Java pushActivity :139-191). The idle card is used when the title carries no
// media name.
func BuildActivity(rawTitle string, a *Action) Activity {
	if a == nil {
		a = &Action{}
	}
	raw := rawTitle
	if strings.HasSuffix(raw, player.Suffix) {
		raw = raw[:strings.LastIndex(raw, player.Suffix)]
	}
	if strings.TrimSpace(raw) == "" || raw == player.AppName {
		return Activity{
			Type:       "PLAYING",
			Details:    "Browsing",
			State:      "Idle",
			LargeImage: "logo",
			LargeText:  "PotFlix",
			SmallImage: "stop",
			SmallText:  "Idle",
			// Name intentionally unset: the header falls back to the app name.
		}
	}

	c := titleclean.Clean(raw)
	title := titleclean.Trunc(c.Title, 128)
	// First body line: the episode name when known, else the episode tag,
	// else the title.
	details := title
	if c.EpisodeName != "" {
		details = titleclean.Trunc(c.EpisodeName, 128)
	} else if c.EpisodeTag != "" {
		details = titleclean.Trunc(c.EpisodeTag, 128)
	}
	// Second body line: the episode tag, but only when it adds information
	// beyond the first line. The comparison uses the FINAL emitted values:
	// both sides are truncated to the 128 cap first, so a tag longer than
	// the cap cannot repeat the details line it already fills.
	state := ""
	if tag := titleclean.Trunc(c.EpisodeTag, 128); tag != "" && tag != details {
		state = tag
	}

	// Paused: the state is the episode tag plus a "Paused" marker, not a
	// frozen position. The pause duration is the count-UP timer anchored by the
	// gate (timestamps.start = the pause moment).
	if a.Paused {
		state = WithStateTime(state, pausedMarker)
	}
	act := Activity{
		Type:       "PLAYING",
		Details:    details,
		State:      state,
		LargeImage: "logo",
		LargeText:  "PotFlix",
	}
	if activityNameHeader {
		act.Name = title
	}
	switch {
	case a.HasStart:
		act.HasStart = true
		act.Start = a.Start
		act.SmallImage = "play"
		act.SmallText = "Playing"
	case a.Paused:
		act.SmallImage = "pause"
		act.SmallText = "Paused"
	default:
		act.SmallImage = "stop"
		act.SmallText = "Stopped"
	}
	return act
}
