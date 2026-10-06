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
//	         "activity":{"type":0,"details":...,"state":...,
//	                     "timestamps":{"start":...},
//	                     "assets":{"large_image":"logo","large_text":"",
//	                               "small_image":...,"small_text":""}}},
//	 "nonce":"<unique>"}
//
// A clear is the same command with "activity":null. Only timestamps.start goes
// on the wire while running: the Discord client renders a COUNTDOWN from
// timestamps.end (README deviation 5, corrected), and the owner requires a
// count-UP elapsed timer matching PotPlayer's left-hand number, so end is
// deliberately omitted even when the send gate knows the projected end. The
// Java reference left it absent too, which is what the port contract's section 5
// specifies. Deliberately NOT set: party, secrets, instance, buttons,
// application_id/name.
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
	Start      int64 // epoch ms, set only while running
	End        int64 // epoch ms, projected end from the gate; deliberately not serialized (deviation 5)
}

// activityPayload is the wire activity object. Every empty string field is
// omitted (`omitempty`) so Discord never sees `"large_text":""`.
type activityPayload struct {
	Type       int          `json:"type"`
	Details    string       `json:"details,omitempty"`
	State      string       `json:"state,omitempty"`
	Timestamps *timestamps  `json:"timestamps,omitempty"`
	Assets     activityAsst `json:"assets"`
}

// timestamps carries the elapsed anchor. Only Start is ever put on the wire: a
// playing card carries start and never end (README deviation 5, corrected),
// because the client renders a COUNTDOWN from end while the owner requires a
// count-up elapsed timer. A paused card is never sent at all - the presence is
// cleared (see gate.go) - so this object is absent while paused.
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

// newActivity maps one card to the wire payload. type is always 0 (PLAYING);
// large_image is always "logo" with an empty large_text/small_text.
func newActivity(a Activity) *activityPayload {
	p := &activityPayload{
		Type:    0,
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
		// Playing: start ONLY. end is deliberately not sent (README deviation 5,
		// corrected): the client renders a countdown from it, and the owner
		// requires a count-up elapsed timer. This is the single place the
		// timestamp pair is decided, so re-enabling is one line here.
		p.Timestamps = &timestamps{Start: a.Start}
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
