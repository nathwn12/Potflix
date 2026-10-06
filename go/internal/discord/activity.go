package discord

import (
	"encoding/json"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

// activity.go - the exact Discord surface the contract sets (section 5).
//
// SET_ACTIVITY carries:
//
//	{"cmd":"SET_ACTIVITY",
//	 "args":{"pid":<os.Getpid()>,
//	         "activity":{"type":3,"details":...,"state":...,
//	                     "timestamps":{"start":...,"end":...},
//	                     "assets":{"large_image":"logo","large_text":"",
//	                               "small_image":...,"small_text":""}}},
//	 "nonce":"<unique>"}
//
// A clear is the same command with "activity":null. The activity type is
// Watching (3), not the Java reference's Playing (0): Discord documents the
// activity time bar as requiring Listening (2) or Watching (3), and "Watching
// <title>" fits a video player. While playing the payload carries BOTH
// timestamps.start and (when the duration is known) timestamps.end, so the
// client renders the time bar against the real media position. While paused it
// carries timestamps.start only, anchored at the pause moment, so the client
// counts the pause UP ("paused for M:SS"). Deliberately NOT set: party, secrets,
// instance, buttons, application_id/name.
//
// Empty-string fields are OMITTED, not sent as "". Discord rejects an activity
// whose assets carry a present-but-empty large_text/small_text (code 4000:
// "large_text is not allowed to be empty"). The Java app looked like it set
// empty strings, but the GameSDK's wire serialization dropped empty fields, so
// parity is "empty text omitted on the wire". Hence `omitempty` here.

// Activity is the caller-facing subset: the poll loop fills it from
// presence.BuildActivity and the fixed brand fields are added here.
type Activity struct {
	Details    string
	State      string
	SmallImage string // "play" running, "pause" paused, "stop" idle/stopped
	HasStart   bool
	Start      int64 // epoch ms, set while running and paused
	End        int64 // epoch ms, projected end from the gate; serialized while playing (deviation)
}

// activityTypeWatching is Discord's ActivityType "Watching" (3). The Java
// reference hardcoded 0 (Playing). Discord documents the activity time bar as
// requiring Listening (2) or Watching (3); Watching fits a video player and
// renders "Watching <title>". Deliberate, user-visible deviation (see README).
const activityTypeWatching = 3

// activityPayload is the wire activity object. Every empty string field is
// omitted (`omitempty`) so Discord never sees `"large_text":""`.
type activityPayload struct {
	Type       int          `json:"type"`
	Details    string       `json:"details,omitempty"`
	State      string       `json:"state,omitempty"`
	Timestamps *timestamps  `json:"timestamps,omitempty"`
	Assets     activityAsst `json:"assets"`
}

// timestamps carries the elapsed anchor and, while playing, the projected end.
// A playing card carries start + end (the time bar); a paused card carries start
// only, anchored at the pause moment, so the client counts the pause UP.
type timestamps struct {
	Start int64 `json:"start,omitempty"`
	End   int64 `json:"end,omitempty"`
}

// activityAsst mirrors the contract's asset field names exactly; empty text is
// omitted on the wire (see the file header).
type activityAsst struct {
	LargeImage string `json:"large_image,omitempty"`
	LargeText  string `json:"large_text,omitempty"`
	SmallImage string `json:"small_image,omitempty"`
	SmallText  string `json:"small_text,omitempty"`
}

// setActivityArgs is the args object; a nil Activity marshals to null.
type setActivityArgs struct {
	PID      int              `json:"pid"`
	Activity *activityPayload `json:"activity"`
}

// command is the outer IPC command envelope.
type command struct {
	Cmd   string          `json:"cmd"`
	Args  setActivityArgs `json:"args"`
	Nonce string          `json:"nonce"`
}

// newActivity maps one card to the wire payload. type is Watching (3);
// large_image is always "logo" with an empty large_text/small_text.
func newActivity(a Activity) *activityPayload {
	p := &activityPayload{
		Type:    activityTypeWatching,
		Details: a.Details,
		State:   a.State,
		Assets: activityAsst{
			LargeImage: "logo",
			LargeText:  "",
			SmallImage: a.SmallImage,
			SmallText:  "",
		},
	}
	if a.HasStart {
		// Playing: start + end (when the duration is known) so the client renders
		// the time bar. Paused: start only (a.End == 0) so the client counts the
		// pause UP from the pause moment. This is the single place the timestamp
		// pair is decided.
		ts := &timestamps{Start: a.Start}
		if a.End > 0 {
			ts.End = a.End
		}
		p.Timestamps = ts
	}
	return p
}

// WireJSON renders the exact activity object SetActivity puts on the wire, for
// the push-attempt log line (the observation boundary).
func WireJSON(a Activity) string {
	b, err := json.Marshal(newActivity(a))
	if err != nil {
		return "{}"
	}
	return string(b)
}

// pid is os.Getpid, isolated so tests can reason about the one dynamic field.
func pid() int { return os.Getpid() }

// nonceSeq makes the nonce unique within the process; the timestamp keeps it
// unique across restarts.
var nonceSeq atomic.Int64

// newNonce returns a unique nonce string.
func newNonce() string {
	return strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + strconv.FormatInt(nonceSeq.Add(1), 10)
}
