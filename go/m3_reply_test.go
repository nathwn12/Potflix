package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"potflix/internal/discord"
)

// m3_reply_test.go - proves the reply path end to end over the in-process fake
// pipe: a successful handshake logs `connected`, a clean SET_ACTIVITY reply logs
// `push ack nonce=<n>`, and Discord's own ERROR frame is logged verbatim (with
// the client id redacted). No real \\.\pipe\discord-ipc-* pipe is touched.

func waitLogContains(t *testing.T, path, want string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		b, _ := os.ReadFile(path)
		if strings.Contains(string(b), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("log %s missing %q; contents:\n%s", path, want, string(b))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func frameNonce(t *testing.T, payload []byte) string {
	t.Helper()
	var m struct {
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("decode frame %s: %v", payload, err)
	}
	if m.Nonce == "" {
		t.Fatalf("frame carries no nonce: %s", payload)
	}
	return m.Nonce
}

func TestPushReplyLoggedVerbatim(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "potflix-m3-reply.log")
	log := newLogger(logPath, 1<<20, 2)
	t.Cleanup(log.Close)

	srv := discord.NewFakeServer(0)
	const clientID = "123456789012345678"

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

	c := discord.New(discord.Config{
		ClientID:   clientID,
		PipeBase:   srv.Base,
		RetryDelay: 30 * time.Millisecond,
		OnConnect:  func() { log.eventConnected() },
		OnReply: func(r discord.Reply) {
			if r.Error {
				log.eventPushError(r.Nonce, r.Evt, r.Code, r.Message)
			} else {
				log.eventPushAck(r.Nonce)
			}
		},
	})
	c.Start()
	t.Cleanup(func() { c.Close() })

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

	act := discord.Activity{Details: "Show", State: `S1 E2 "Pilot"`, SmallImage: "play", HasStart: true, Start: 12345}

	// Before Discord's READY dispatch a command is refused, not written: Discord
	// discards commands sent earlier.
	if err := c.SetActivity(act); !errors.Is(err, discord.ErrNotReady) {
		t.Fatalf("pre-READY SetActivity = %v, want ErrNotReady", err)
	}

	// Discord's READY dispatch completes the handshake: OnConnect fires the
	// contract's `connected` line.
	if err := sc.WriteFrame(discord.OpFrame, []byte(`{"cmd":"DISPATCH","evt":"READY","data":{"v":1}}`)); err != nil {
		t.Fatalf("write READY: %v", err)
	}
	waitLogContains(t, logPath, "connected clientId=<redacted>", 3*time.Second)

	// (1) A clean reply to SET_ACTIVITY logs `push ack nonce=<n>`.
	if err := c.SetActivity(act); err != nil {
		t.Fatalf("SetActivity: %v", err)
	}
	op, frame, err := sc.ReadFrame()
	if err != nil || op != discord.OpFrame {
		t.Fatalf("read SET_ACTIVITY: op=%d err=%v", op, err)
	}
	nonce1 := frameNonce(t, frame)
	ack := `{"cmd":"SET_ACTIVITY","evt":null,"nonce":"` + nonce1 + `","data":null}`
	if err := sc.WriteFrame(discord.OpFrame, []byte(ack)); err != nil {
		t.Fatalf("write ack: %v", err)
	}
	waitLogContains(t, logPath, "push ack nonce="+nonce1, 3*time.Second)

	// (2) Discord's ERROR frame is logged verbatim, client id redacted.
	if err := c.SetActivity(act); err != nil {
		t.Fatalf("SetActivity 2: %v", err)
	}
	op, frame, err = sc.ReadFrame()
	if err != nil || op != discord.OpFrame {
		t.Fatalf("read SET_ACTIVITY 2: op=%d err=%v", op, err)
	}
	nonce2 := frameNonce(t, frame)
	const raw = "Invalid activity for client " + clientID + ": bad assets"
	errFrame := `{"cmd":"SET_ACTIVITY","evt":"ERROR","nonce":"` + nonce2 + `","data":{"code":4003,"message":"` + raw + `"}}`
	if err := sc.WriteFrame(discord.OpFrame, []byte(errFrame)); err != nil {
		t.Fatalf("write error frame: %v", err)
	}
	want := "push error nonce=" + nonce2 + ` evt=ERROR code=4003 message=Invalid activity for client <redacted>: bad assets`
	waitLogContains(t, logPath, want, 3*time.Second)
}
