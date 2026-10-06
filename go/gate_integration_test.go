package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"potflix/internal/discord"
	"potflix/internal/player"
	"potflix/internal/presence"
)

// gate_integration_test.go - headless end-to-end proof of the push gate through
// the in-process fake pipe. It drives the REAL tracker, the REAL gate and the
// REAL pushActivity over a scripted position sequence, and asserts the exact
// sequence of SET_ACTIVITY frames that would go to Discord: playing cards carry
// timestamps.start AND the projected end, a pause sends one parked paused card
// (start only, no end) and never clears the activity, and a suppressed poll
// produces no frame at all. No live Discord and no real PotPlayer window are
// touched.

const itTitle = "Breaking Bad (2008) - S04E07 - Problem Dog (1080p BluRay x265 Silence).mkv - PotPlayer"

const (
	itTotal = int64(2832979)
	itN     = int64(1000000000000)
)

type itWire struct {
	Cmd  string `json:"cmd"`
	Args struct {
		Activity *struct {
			Details    string `json:"details"`
			State      string `json:"state"`
			Timestamps *struct {
				Start *int64 `json:"start"`
				End   *int64 `json:"end"`
			} `json:"timestamps"`
		} `json:"activity"`
	} `json:"args"`
}

type itSent struct {
	clear    bool // activity:null
	details  string
	state    string
	hasStart bool
	start    int64
	hasEnd   bool
	end      int64
}

func TestGateFakePipeSequence(t *testing.T) {
	srv := discord.NewFakeServer(0)
	accept := make(chan *discord.FakeConn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		sc, err := srv.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accept <- sc
	}()

	dc := discord.New(discord.Config{
		ClientID:   "123456789012345678",
		PipeBase:   srv.Base,
		RetryDelay: 30 * time.Millisecond,
	})
	dc.Start()
	t.Cleanup(func() { dc.Close() })

	var sc *discord.FakeConn
	select {
	case sc = <-accept:
	case err := <-acceptErr:
		t.Fatalf("fake server accept: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("client did not connect to the fake pipe")
	}
	defer sc.Close()

	if op, _, err := sc.ReadFrame(); err != nil || op != discord.OpHandshake {
		t.Fatalf("handshake: op=%d err=%v", op, err)
	}
	if err := sc.WriteFrame(discord.OpFrame, []byte(`{"cmd":"DISPATCH","evt":"READY","data":{"v":1}}`)); err != nil {
		t.Fatalf("write READY: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for dc.Generation() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("client never became ready after the READY dispatch")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Collect every post-READY frame the client sends.
	frames := make(chan []byte, 32)
	go func() {
		for {
			op, payload, err := sc.ReadFrame()
			if err != nil {
				return
			}
			if op == discord.OpFrame {
				frames <- payload
			}
		}
	}()

	logPath := filepath.Join(t.TempDir(), "gate-integration.log")
	lg := newLogger(logPath, 0, 1)
	t.Cleanup(func() { lg.Close() })

	tr := &presence.Tracker{}
	gate := &presence.Gate{}

	drive := func(st player.PlayStatus, ct, now int64) {
		a := tr.Poll(itTitle, st, ct, now)
		cur := a
		if cur == nil {
			cur = tr.Current()
		}
		if cur != nil {
			pushActivity(lg, dc, gate, itTitle, cur, ct, itTotal, now, a != nil, tr.PendingDebounce())
		}
	}

	R, P := player.StatusRunning, player.StatusPaused

	drive(R, 1000, itN)       // 0 first card -> sent
	drive(R, 1250, itN+250)   // 1 steady -> suppressed heartbeat-not-due
	drive(R, 2800, itN+500)   // 2 small seek inside the window -> held (debounce)
	drive(R, 2800, itN+750)   // 3 still held -> suppressed debounce
	drive(R, 2800, itN+1000)  // 4 trailing edge lands -> sent
	drive(R, 3050, itN+1250)  // 5 steady
	drive(R, 9000, itN+1500)  // 6 big seek inside window -> divergence sent
	drive(R, 9250, itN+1750)  // 7 steady
	drive(R, 15000, itN+2000) // 8 seek after window -> tracker event sent
	drive(R, 15250, itN+2250) // 9 steady
	drive(P, 15250, itN+2500) // 10 flap -> still running
	drive(P, 15250, itN+2750) // 11 real pause -> parked paused card (no clear)
	drive(P, 15250, itN+3000) // 12 paused steady
	drive(R, 15250, itN+3250) // 13 resume -> sent
	// 14+ : 120 smooth 250 ms polls = 30 s with no tracker event; only the
	// heartbeat should send.
	for i := int64(1); i <= 120; i++ {
		drive(R, 15250+250*i, itN+3250+250*i)
	}

	// Positive control: force one push through the real path and confirm the
	// recorder line lands. A silent log proves nothing until a known event is
	// recorded.
	gate.ForceNext()
	pushActivity(lg, dc, gate, itTitle, tr.Current(), 15250+250*120, itTotal, itN+3250+250*120, false, false)

	// The last write is synchronous, but give the reader a beat.
	time.Sleep(200 * time.Millisecond)

	var got []itSent
	var raws []string
drain:
	for {
		select {
		case payload := <-frames:
			raws = append(raws, string(payload))
			var w itWire
			if err := json.Unmarshal(payload, &w); err != nil {
				t.Fatalf("decode frame %s: %v", payload, err)
			}
			if w.Cmd != "SET_ACTIVITY" {
				t.Fatalf("unexpected frame: %s", payload)
			}
			if w.Args.Activity == nil {
				got = append(got, itSent{clear: true})
				continue
			}
			s := itSent{details: w.Args.Activity.Details, state: w.Args.Activity.State}
			if ts := w.Args.Activity.Timestamps; ts != nil {
				if ts.Start != nil {
					s.hasStart = true
					s.start = *ts.Start
				}
				if ts.End != nil {
					s.hasEnd = true
					s.end = *ts.End
				}
			}
			got = append(got, s)
		default:
			break drain
		}
	}

	const (
		start0 = itN - 1000
		start1 = itN - 1800
		start2 = itN - 7500
		start3 = itN - 13000
		start4 = itN - 12000
		// The paused card anchors timestamps.start at the pause moment (now) and
		// carries no end.
		pauseStart = itN + 2750
		// Playing cards carry start + end = now + (total - pos).
		end0 = itN + (itTotal - 1000)
		end1 = itN + 1000 + (itTotal - 2800)
		end2 = itN + 1500 + (itTotal - 9000)
		end3 = itN + 2000 + (itTotal - 15000)
		end5 = itN + 3250 + (itTotal - 15250)
		end6 = itN + 33250 + (itTotal - 45250)
	)
	want := []itSent{
		{details: "Breaking Bad", state: `S4 E7 "Problem Dog"`, hasStart: true, start: start0, hasEnd: true, end: end0}, // first
		{details: "Breaking Bad", state: `S4 E7 "Problem Dog"`, hasStart: true, start: start1, hasEnd: true, end: end1}, // trailing edge
		{details: "Breaking Bad", state: `S4 E7 "Problem Dog"`, hasStart: true, start: start2, hasEnd: true, end: end2}, // divergence
		{details: "Breaking Bad", state: `S4 E7 "Problem Dog"`, hasStart: true, start: start3, hasEnd: true, end: end3}, // seek event
		{details: "Breaking Bad", state: `S4 E7 "Problem Dog" - Paused`, hasStart: true, start: pauseStart},             // pause -> parked card, no end, no clear
		{details: "Breaking Bad", state: `S4 E7 "Problem Dog"`, hasStart: true, start: start4, hasEnd: true, end: end5}, // resume
		// The 30 s heartbeat re-pushes the same card: at 1x the anchor is
		// unchanged, so the payload is byte-identical to the resume - that is
		// exactly the self-healing re-push.
		{details: "Breaking Bad", state: `S4 E7 "Problem Dog"`, hasStart: true, start: start4, hasEnd: true, end: end6}, // heartbeat
		{details: "Breaking Bad", state: `S4 E7 "Problem Dog"`, hasStart: true, start: start4, hasEnd: true, end: end6}, // positive control (forced)
	}

	if len(got) != len(want) {
		t.Fatalf("sent %d frames, want %d\n got: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("frame %d = %+v, want %+v", i, got[i], want[i])
		} else {
			t.Logf("frame %d raw: %s", i, raws[i])
		}
	}

	// R1: playing frames carry BOTH timestamps.start and timestamps.end (the
	// time bar); the paused frame carries start only, never end.
	for i, s := range got {
		if i == 4 {
			if s.hasEnd {
				t.Errorf("paused frame %d carries end=%d, want none: %s", i, s.end, raws[i])
			}
			continue
		}
		if !s.hasStart || !s.hasEnd {
			t.Errorf("playing frame %d hasStart=%t hasEnd=%t, want both: %s", i, s.hasStart, s.hasEnd, raws[i])
		}
	}
	// R2: the pause frame is a parked paused card, and NO frame is a null clear.
	if got[4].clear || got[4].state != `S4 E7 "Problem Dog" - Paused` {
		t.Fatalf("frame 4 = %+v, want the parked paused card (no clear)", got[4])
	}
	for i, s := range got {
		if s.clear {
			t.Errorf("frame %d is an activity:null clear, want a card", i)
		}
	}
	for i, raw := range raws {
		if strings.Contains(raw, "00:15") {
			t.Errorf("frame %d looks like a frozen-position paused card: %s", i, raw)
		}
	}
	// Resume re-anchors to the live position within the poll: now - start == ct.
	const (
		resumeNow = itN + 3250
		resumeCT  = int64(15250)
	)
	if got[5].start != resumeNow-resumeCT {
		t.Errorf("resume start=%d, want %d (now-ct); off by %d ms",
			got[5].start, resumeNow-resumeCT, got[5].start-(resumeNow-resumeCT))
	}
	// Post-seek re-anchor within one poll: the seek-event frame's start matches
	// the live position at that poll.
	const (
		seekNow = itN + 2000
		seekCT  = int64(15000)
	)
	if got[3].start != seekNow-seekCT {
		t.Errorf("post-seek start=%d, want %d (now-ct)", got[3].start, seekNow-seekCT)
	}

	// The observation boundary: every poll logged an attempt, suppressed polls
	// included, with the reason.
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	logText := string(logData)
	const totalPolls = 14 + 120 + 1 // + the forced positive-control push
	if n := strings.Count(logText, "gate=sent"); n != len(want) {
		t.Errorf("log gate=sent lines = %d, want %d (every frame, the parked pause included)", n, len(want))
	}
	if n := strings.Count(logText, "gate=clear"); n != 0 {
		t.Errorf("log gate=clear lines = %d, want 0 (pause parks a card, it never clears)", n)
	}
	if n := strings.Count(logText, "gate=suppressed"); n != totalPolls-len(want) {
		t.Errorf("log gate=suppressed lines = %d, want %d", n, totalPolls-len(want))
	}
	if !strings.Contains(logText, "reason=debounce") {
		t.Errorf("log is missing a suppressed reason=debounce line")
	}
	if !strings.Contains(logText, "reason=heartbeat-not-due") {
		t.Errorf("log is missing a suppressed reason=heartbeat-not-due line")
	}
	if !strings.Contains(logText, "reason=paused") {
		t.Errorf("log is missing a suppressed reason=paused line")
	}
	// F1: the logged payload (the observation boundary) must agree with the
	// frame that actually went on the wire, timestamps.end included. The log
	// line carries the activity object without the cmd/args envelope, so parse
	// it directly and compare each sent line to its frame.
	type logActivity struct {
		Timestamps *struct {
			Start *int64 `json:"start"`
			End   *int64 `json:"end"`
		} `json:"timestamps"`
	}
	type logEnd struct {
		has bool
		val int64
	}
	var loggedEnds []logEnd
	for _, line := range strings.Split(logText, "\n") {
		if !strings.Contains(line, "gate=sent") {
			continue
		}
		at := strings.Index(line, "payload=")
		if at < 0 {
			t.Fatalf("sent line has no payload=: %s", line)
		}
		var la logActivity
		if err := json.Unmarshal([]byte(strings.TrimSpace(line[at+len("payload="):])), &la); err != nil {
			t.Fatalf("decode logged payload %q: %v", line, err)
		}
		var le logEnd
		if la.Timestamps != nil && la.Timestamps.End != nil {
			le.has = true
			le.val = *la.Timestamps.End
		}
		loggedEnds = append(loggedEnds, le)
	}
	if len(loggedEnds) != len(got) {
		t.Fatalf("logged sent payloads = %d, want %d (one per frame)", len(loggedEnds), len(got))
	}
	for i := range got {
		if loggedEnds[i].has != got[i].hasEnd || (got[i].hasEnd && loggedEnds[i].val != got[i].end) {
			t.Errorf("frame %d: logged end=%+v, wire end has=%t val=%d (logged payload must match the wire)",
				i, loggedEnds[i], got[i].hasEnd, got[i].end)
		}
	}
	t.Logf("push-attempt payloads agree with the wire on timestamps.end (%d frames)", len(got))
	t.Logf("observation boundary: %d polls, %d cards sent, 0 clears, %d suppressed (reasons logged)",
		totalPolls, len(want), totalPolls-len(want))
}
