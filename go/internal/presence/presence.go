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
// repeated RUN pushes within ~1 s are debounced unless the title changed.

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
	Suffix   string // "" or " - MM:SS" (paused only)
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

	// Fleet decision 10 (deviation): RUN debounce anchor.
	hasRunPush       bool
	lastRunPushAt    int64
	lastRunPushTitle string
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
	// New media: forget everything (Java :267-270).
	if t.havePrev && t.lastTitle != rawTitle {
		t.havePrev = false
		t.hasLastStatus = false
		t.lastStatus = 0
		t.frozenCt = -1
		t.prevCt = -1
		t.ctWindowLen = 0
		t.ctWindowIdx = 0
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
			action = &Action{Paused: true, Suffix: " - " + Fmt(ct), CT: ct}
			t.frozenCt = ct
		}
	default: // Stopped
		if changed {
			action = &Action{CT: ct}
		}
	}

	// Fleet decision 10: debounce repeated RUN pushes (seek bursts) within ~1 s
	// unless the title changed. A `changed` RUN push is a real state/title
	// transition and always emits.
	if action != nil && action.Running {
		if !changed && t.hasRunPush && (now-t.lastRunPushAt) < RunDebounceMS && rawTitle == t.lastRunPushTitle {
			action = nil
		} else {
			t.hasRunPush = true
			t.lastRunPushAt = now
			t.lastRunPushTitle = rawTitle
		}
	}

	// On emit only, remember title/status; havePrev is always set (:306-308).
	if action != nil {
		t.lastTitle = rawTitle
		t.lastStatus = eff
		t.hasLastStatus = true
	}
	t.havePrev = true
	return action
}

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

// leadingDashRe is Java's `^\s*-\s*` with Java's \s class.
var leadingDashRe = regexp.MustCompile(`^[ \t\n\x0B\f\r]*-[ \t\n\x0B\f\r]*`)

// WithStateTime mirrors Main.withStateTime (:313-317): context plus the paused
// " - MM:SS" suffix; a leading dash is stripped when there is no context.
func WithStateTime(context, suffix string) string {
	if suffix == "" {
		return context
	}
	if context == "" {
		return leadingDashRe.ReplaceAllString(suffix, "")
	}
	return titleclean.Trunc(context+suffix, 128)
}

// Activity is the Discord payload (contract section 5). Type is always
// "PLAYING"; the fields not listed are deliberately unset.
type Activity struct {
	Type       string
	Details    string
	State      string
	LargeImage string
	LargeText  string
	SmallImage string
	SmallText  string
	HasStart   bool
	Start      int64
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
			SmallImage: "stop",
		}
	}

	c := titleclean.Clean(raw)
	details := titleclean.Trunc(c.Title, 120)
	context := ""
	if c.EpisodeTag != "" {
		context = c.EpisodeTag
		if c.EpisodeName != "" {
			context += " \"" + titleclean.Trunc(c.EpisodeName, 60) + "\""
		}
	}
	context = titleclean.Trunc(context, 120)
	if context == details {
		context = ""
	}

	act := Activity{
		Type:       "PLAYING",
		Details:    details,
		State:      WithStateTime(context, a.Suffix),
		LargeImage: "logo",
		SmallText:  "",
	}
	switch {
	case a.HasStart:
		act.HasStart = true
		act.Start = a.Start
		act.SmallImage = "play"
	case a.Paused:
		act.SmallImage = "pause"
	default:
		act.SmallImage = "stop"
	}
	return act
}
