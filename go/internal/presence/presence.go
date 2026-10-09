package presence

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf16"

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

// stateSynopsisBudget is the target budget (UTF-16 units) for synopsis-bearing
// State lines: Discord best-practices says details/state must be snippets, not
// sentences, staying on one line (the 128-unit hard cap stays as the ceiling;
// 72 < 128 so every budgeted State respects it); Netflix timed-text (42
// chars/line, max 2 lines, break after punctuation, single U+2026) motivates
// ~72 units so the card never fills to the API max.
const stateSynopsisBudget = 72

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
	// Buttons are the card's click-through links (max 2), populated only by
	// BuildActivityResolved; BuildActivity leaves them nil.
	Buttons []Button
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

// Button is one presence-local card link.
type Button struct {
	Label string
	URL   string
}

// Resolved carries canonical titles from a metadata lookup; a zero field is ignored.
type Resolved struct {
	Series      string // canonical series or movie title
	EpisodeName string // canonical episode title
	Artwork     string // cover/poster URL (https); zero value = absent
	Synopsis    string // one-line plain text; zero value = absent
	Genres      string // movie genres, e.g. "Action, Drama"; zero value = absent
	Length      string // movie runtime, e.g. "2h 19m"; zero value = absent
	Year        int    // movie release year; <=0 = absent
	IsMovie     bool   // true: movie card lines; false: series card lines
	Buttons     []Button
}

// rpText sanitizes free text for the RPC card: trim, collapse runs of
// whitespace/newlines/tabs to single spaces, strip C0 control chars, then
// truncate to max via titleclean.Trunc. Internal underscores and other real
// title content are preserved.
func rpText(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7F {
			return -1
		}
		return r
	}, s)
	return titleclean.Trunc(s, max)
}

// trimWord sanitizes s via rpText, then caps it at max UTF-16 units at a word
// boundary: when the sanitized text fits it is returned as-is; otherwise the
// prefix fitting in max-1 units is cut at its last space and a single U+2026
// is appended, so a synopsis never ends mid-word. With no space in the
// prefix it falls back to rpText's hard cut.
func trimWord(s string, max int) string {
	if max <= 0 {
		return ""
	}
	const huge = 1 << 28
	full := rpText(s, huge)
	if len(utf16.Encode([]rune(full))) <= max {
		return full
	}
	if max == 1 {
		return "\u2026"
	}
	runes := []rune(full)
	used := 0
	end := 0
	for i, r := range runes {
		sz := 1
		if r >= 0x10000 {
			sz = 2
		}
		if used+sz > max-1 {
			break
		}
		used += sz
		end = i + 1
	}
	if prefix := string(runes[:end]); prefix != "" {
		if idx := strings.LastIndex(prefix, " "); idx > 0 {
			return prefix[:idx] + "\u2026"
		}
	}
	return rpText(s, max)
}

// synopsisConnectors are words that must never dangle before the ellipsis:
// "... and" / "... the" reads as a cut-off thought, so they are stripped first.
var synopsisConnectors = map[string]struct{}{
	"and": {}, "or": {}, "but": {}, "the": {}, "a": {}, "an": {},
	"of": {}, "to": {}, "in": {}, "on": {}, "with": {}, "for": {},
	"from": {}, "by": {}, "as": {}, "at": {}, "into": {}, "than": {},
	"that": {}, "which": {}, "while": {}, "when": {}, "his": {}, "her": {},
	"their": {}, "its": {}, "this": {}, "these": {}, "those": {},
	"is": {}, "are": {}, "was": {}, "were": {}, "who": {},
}

// stripDanglingConnector drops trailing connector words (case-insensitive)
// from base so the ellipsis never follows "and"/"the"/etc.
func stripDanglingConnector(base string) string {
	for {
		t := strings.TrimSpace(base)
		if t == "" {
			return ""
		}
		idx := strings.LastIndex(t, " ")
		var last, rest string
		if idx < 0 {
			last, rest = t, ""
		} else {
			last, rest = t[idx+1:], t[:idx]
		}
		clean := strings.ToLower(strings.Trim(last, ",;:.!?\"'()[]---.\u00b7"))
		if clean == "" {
			return t
		}
		if _, ok := synopsisConnectors[clean]; !ok {
			return t
		}
		if idx < 0 {
			return ""
		}
		base = rest
	}
}

// fitRunes returns how many leading runes of rs fit in maxUnits UTF-16 units.
func fitRunes(rs []rune, maxUnits int) int {
	if maxUnits <= 0 {
		return 0
	}
	used := 0
	for i, r := range rs {
		sz := 1
		if r >= 0x10000 {
			sz = 2
		}
		if used+sz > maxUnits {
			return i
		}
		used += sz
	}
	return len(rs)
}

// runeCountForUnits returns the smallest leading rune count of rs[:end] whose
// UTF-16 length reaches targetUnits (the start of the back half).
func runeCountForUnits(rs []rune, end, targetUnits int) int {
	if targetUnits <= 0 {
		return 0
	}
	used := 0
	for i := 0; i < end; i++ {
		sz := 1
		if rs[i] >= 0x10000 {
			sz = 2
		}
		used += sz
		if used >= targetUnits {
			return i + 1
		}
	}
	return 0
}

// smartTrimSynopsis is trimWord with a seamless cut (Netflix: break after
// punctuation, single U+2026; Discord: snippet, one line), all measured in
// UTF-16 units: fits unchanged; else last sentence end (.!?, end-or-space) in
// the back half with NO ellipsis; else last clause boundary (, ; space-dash
// variants) in the back half plus U+2026; else last word boundary plus U+2026
// (dangling connectors stripped first); never mid-word, never "...".
func smartTrimSynopsis(s string, max int) string {
	if max <= 0 {
		return ""
	}
	const huge = 1 << 28
	full := rpText(s, huge)
	if len(utf16.Encode([]rune(full))) <= max {
		return full
	}
	if max == 1 {
		return "\u2026"
	}
	runes := []rune(full)
	endFit := fitRunes(runes, max)
	halfFit := runeCountForUnits(runes, endFit, max/2)
	for i := endFit - 1; i >= halfFit; i-- {
		if c := runes[i]; c == '.' || c == '!' || c == '?' {
			if i+1 == len(runes) || unicode.IsSpace(runes[i+1]) {
				cand := strings.TrimSpace(string(runes[:i+1]))
				if len(utf16.Encode([]rune(cand))) <= max {
					return cand
				}
			}
		}
	}
	endEll := fitRunes(runes, max-1)
	halfEll := runeCountForUnits(runes, endEll, (max-1)/2)
	upper := endEll
	if upper >= len(runes) {
		upper = len(runes) - 1
	}
	for i := upper; i >= halfEll; i-- {
		if i < 0 || i >= len(runes) {
			continue
		}
		if i < endEll && (runes[i] == ',' || runes[i] == ';') {
			if i+1 < len(runes) && unicode.IsSpace(runes[i+1]) {
				base := stripDanglingConnector(strings.TrimSpace(string(runes[:i+1])))
				if base == "" {
					return "\u2026"
				}
				return base + "\u2026"
			}
		}
		if runes[i] == '-' || runes[i] == '\u2013' || runes[i] == '\u2014' {
			if i-1 >= 0 && i+1 < len(runes) && runes[i-1] == ' ' && unicode.IsSpace(runes[i+1]) {
				start := i - 1
				if start >= halfEll && start < endEll {
					base := stripDanglingConnector(strings.TrimSpace(string(runes[:start])))
					if base == "" {
						return "\u2026"
					}
					return base + "\u2026"
				}
			}
		}
	}
	for i := endEll - 1; i >= 0; i-- {
		if runes[i] == ' ' {
			base := stripDanglingConnector(strings.TrimSpace(string(runes[:i])))
			if base == "" {
				return "\u2026"
			}
			res := base + "\u2026"
			if len(utf16.Encode([]rune(res))) <= max {
				return res
			}
			return res
		}
	}
	return rpText(s, max)
}

// filterButtons keeps at most two buttons with a non-empty label and an
// http(s) URL; labels are sanitized and capped at 32 units.
func filterButtons(in []Button) []Button {
	var out []Button
	for _, b := range in {
		if len(out) >= 2 {
			break
		}
		lbl := rpText(b.Label, 32)
		if lbl == "" {
			continue
		}
		if !strings.HasPrefix(b.URL, "http://") && !strings.HasPrefix(b.URL, "https://") {
			continue
		}
		out = append(out, Button{Label: lbl, URL: b.URL})
	}
	return out
}

// BuildActivityResolved is BuildActivity with canonical title overrides applied.
//
// Movie (r.IsMovie): Details is the canonical title with " (YYYY)" when
// Year>0; State joins Genres, Length and the synopsis with " \u00b7 ",
// skipping empties, capped word-aware at 128. Series: Details is the episode
// override when present (else BuildActivity's filename-derived line); State
// joins the episode tag and the synopsis the same way, with the synopsis
// word-capped. The paused " - Paused" marker path is preserved for both.
func BuildActivityResolved(rawTitle string, a *Action, r Resolved) Activity {
	act := BuildActivity(rawTitle, a)
	if r.Series != "" {
		act.Name = titleclean.Trunc(r.Series, 128)
		act.LargeText = titleclean.Trunc(r.Series, 128)
	}
	// Hover slot: assets.large_text is the poster tooltip. Name already
	// carries the series title as the card header, so repeating it here
	// wastes the slot; carry the complete short synopsis instead (capped at
	// Discord's 128-unit asset-text cap, so it arrives whole). No synopsis:
	// keep today's fallback (series title when known, else "PotFlix").
	if s := rpText(r.Synopsis, 128); s != "" {
		act.LargeText = s
	}
	if r.IsMovie {
		if r.Series != "" {
			if r.Year > 0 {
				act.Details = titleclean.Trunc(fmt.Sprintf("%s (%d)", r.Series, r.Year), 128)
			} else {
				act.Details = titleclean.Trunc(r.Series, 128)
			}
		}
		g := rpText(r.Genres, 1<<28)
		l := rpText(r.Length, 1<<28)
		sFull := rpText(r.Synopsis, 1<<28)
		var prefixParts []string
		if g != "" {
			prefixParts = append(prefixParts, g)
		}
		if l != "" {
			prefixParts = append(prefixParts, l)
		}
		prefix := strings.Join(prefixParts, " \u00b7 ")
		switch {
		case prefix == "" && sFull == "":
			// Empty join: keep BuildActivity's state (including any Paused marker).
		case sFull == "":
			// No synopsis: keep the 128 ceiling (non-synopsis states untouched).
			if a != nil && a.Paused {
				act.State = WithStateTime(trimWord(prefix, 128-len(pausedMarker)), pausedMarker)
			} else {
				act.State = trimWord(prefix, 128)
			}
		default:
			joinBudget := stateSynopsisBudget
			if a != nil && a.Paused {
				joinBudget = stateSynopsisBudget - len(pausedMarker)
			}
			sep := " \u00b7 "
			if prefix == "" {
				sep = ""
			}
			prefixUnits := len(utf16.Encode([]rune(prefix)))
			sepUnits := len(utf16.Encode([]rune(sep)))
			if prefixUnits+sepUnits >= joinBudget {
				fullJoined := prefix + sep + sFull
				if a != nil && a.Paused {
					act.State = WithStateTime(trimWord(fullJoined, joinBudget), pausedMarker)
				} else {
					act.State = trimWord(fullJoined, joinBudget)
				}
			} else {
				trimmed := smartTrimSynopsis(sFull, joinBudget-prefixUnits-sepUnits)
				joined := prefix + sep + trimmed
				if a != nil && a.Paused {
					act.State = WithStateTime(joined, pausedMarker)
				} else {
					act.State = joined
				}
			}
		}
		// Empty join: keep BuildActivity's state (including any Paused marker).
	} else {
		if r.EpisodeName != "" {
			act.Details = titleclean.Trunc(r.EpisodeName, 128)
		}
		// Episode tag for the state line, derived from the title the same way
		// BuildActivity derives it.
		raw := rawTitle
		if strings.HasSuffix(raw, player.Suffix) {
			raw = raw[:strings.LastIndex(raw, player.Suffix)]
		}
		var tagRaw string
		if idle := strings.TrimSpace(raw) == "" || raw == player.AppName; !idle {
			tagRaw = titleclean.Clean(raw).EpisodeTag
		}
		tagT := rpText(tagRaw, 128)
		synFull := rpText(r.Synopsis, 1<<28)
		switch {
		case tagT == "" && synFull == "":
			// Neither: keep BuildActivity's state (including any Paused marker).
		case a != nil && a.Paused:
			if synFull == "" {
				// Tag-only paused: no synopsis, keep the 128 ceiling.
				act.State = WithStateTime(trimWord(tagT, 128-len(pausedMarker)), pausedMarker)
				break
			}
			joinBudget := stateSynopsisBudget - len(pausedMarker)
			sep := " \u00b7 "
			if tagT == "" {
				sep = ""
			}
			prefixUnits := len(utf16.Encode([]rune(tagT)))
			sepUnits := len(utf16.Encode([]rune(sep)))
			if prefixUnits+sepUnits >= joinBudget {
				act.State = WithStateTime(trimWord(tagT+sep+synFull, joinBudget), pausedMarker)
			} else {
				trimmed := smartTrimSynopsis(synFull, joinBudget-prefixUnits-sepUnits)
				act.State = WithStateTime(tagT+sep+trimmed, pausedMarker)
			}
		default:
			switch {
			case tagT != "" && synFull != "":
				joinBudget := stateSynopsisBudget
				prefixUnits := len(utf16.Encode([]rune(tagT)))
				sepUnits := len(utf16.Encode([]rune(" \u00b7 ")))
				if prefixUnits+sepUnits >= joinBudget {
					act.State = trimWord(tagT+" \u00b7 "+synFull, joinBudget)
				} else {
					trimmed := smartTrimSynopsis(synFull, joinBudget-prefixUnits-sepUnits)
					act.State = tagT + " \u00b7 " + trimmed
				}
			case tagT != "":
				act.State = tagT
			default:
				act.State = smartTrimSynopsis(synFull, stateSynopsisBudget)
			}
		}
	}
	if strings.HasPrefix(r.Artwork, "http://") || strings.HasPrefix(r.Artwork, "https://") {
		act.LargeImage = r.Artwork
	}
	// Otherwise LargeImage stays as BuildActivity produced it ("logo"): an
	// empty large image is never emitted.
	if btns := filterButtons(r.Buttons); len(btns) > 0 {
		act.Buttons = btns
	}
	return act
}
