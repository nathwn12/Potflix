package main

import (
	"strconv"

	"potflix/internal/discord"
	"potflix/internal/presence"
)

// events.go - the exact event strings the port contract lists (section 7).
// These are the later modules' log lines; M1 defines them so M2/M3 reuse one
// place. The presence/decision string is listed verbatim, including the
// duplicated trailing `ct=<ms>` the contract shows.
//
// SECRET RULE (M1): the client id VALUE is never written to the log. The
// contract's literal `connected clientId=<id>` is therefore emitted with the
// id redacted; only the resolution SOURCE is ever logged (see clientid.go).

func (l *logger) eventConnected() {
	l.logf("connected clientId=<redacted>")
}

// eventPushAck is Discord's clean reply to one SET_ACTIVITY, matched by nonce.
func (l *logger) eventPushAck(nonce string) {
	l.logf("push ack nonce=%s", nonce)
}

// eventPushError is Discord's own rejection of one SET_ACTIVITY: the evt is
// logged verbatim, with Discord's data.code and data.message. The client id is
// redacted upstream (internal/discord); nothing else is altered.
func (l *logger) eventPushError(nonce, evt string, code int, message string) {
	l.logf("push error nonce=%s evt=%s code=%d message=%s", nonce, evt, code, message)
}

// eventPushAckTimeout is reported when a pushed command got no reply within the
// ack window: the write path is unproven, and this says so instead of trusting it.
func (l *logger) eventPushAckTimeout(nonce string) {
	l.logf("push ack timeout nonce=%s", nonce)
}

// eventSendError surfaces a transport failure that would otherwise be swallowed;
// on this path no send error is dropped silently.
func (l *logger) eventSendError(err error) {
	l.logf("send error: %s", err)
}

func (l *logger) eventPush(details, state string, ct, total int64) {
	l.logf("push details=[%s] state=[%s] ct=%d total=%d", details, state, ct, total)
}

// eventPushAttempt is the observation boundary for the push: one line per poll
// that has a card, carrying the exact activity that would go on the wire (the
// verbatim payload, including timestamps.start or "none", and timestamps.end
// when the gate projected one) and the gate verdict. A pause parks a paused
// card and sends it once; the gate no longer sets Clear (that branch is
// dormant), so the gate=clear arm below is retained only for the null-activity
// path. Suppressed polls are logged too - a silent log hid the swallowed
// seek re-anchor, which is why that defect was invisible.
func (l *logger) eventPushAttempt(a presence.Activity, v presence.Verdict) {
	// Dormant: Verdict.Clear is no longer set - a pause now parks and sends a
	// paused card. Kept so a clear decision could still be logged as a null
	// activity without changing the callers.
	if v.Clear {
		l.logf("push attempt gate=clear activity=null")
		return
	}
	// The steady-state heartbeat-not-due line dominates the log (~4/s); the
	// paused line is likewise steady. Both keep the verdict but drop the payload
	// suffix: at ~300 B/line the full payload cut retention to ~2-4 h against
	// the 5 MiB x 3 cap. Sent, debounce, unchanged and divergence lines keep the
	// full payload.
	if !v.Send && (v.Reason == presence.ReasonHeartbeatNotDue || v.Reason == presence.ReasonPaused) {
		l.logf("push attempt gate=suppressed reason=%s", v.Reason)
		return
	}
	start := "none"
	if a.HasStart {
		start = strconv.FormatInt(a.Start, 10)
	}
	payload := discord.WireJSON(discord.Activity{
		Name:       a.Name,
		Details:    a.Details,
		State:      a.State,
		LargeImage: a.LargeImage,
		LargeText:  a.LargeText,
		SmallImage: a.SmallImage,
		SmallText:  a.SmallText,
		HasStart:   a.HasStart,
		Start:      a.Start,
		End:        a.End,
	})
	if v.Send {
		l.logf("push attempt gate=sent start=%s payload=%s", start, payload)
	} else {
		l.logf("push attempt gate=suppressed reason=%s start=%s payload=%s", v.Reason, start, payload)
	}
}

func (l *logger) eventDecision(ms int64, st string, ct int64, title, action string, running, paused, hasStart bool, start int64, suf string) {
	l.logf("decision ms=%d st=%s ct=%d title=[%s] action=%s running=%t paused=%t hasStart=%t start=%d suf=%s ct=%d",
		ms, st, ct, title, action, running, paused, hasStart, start, suf, ct)
}

func (l *logger) eventClearedNoPlayer() {
	l.logf("cleared (no PotPlayer)")
}

func (l *logger) eventClearedWindowGone() {
	l.logf("cleared (player window gone)")
}

func (l *logger) eventSessionDropped(err error) {
	l.logf("session dropped: %s", err)
}
