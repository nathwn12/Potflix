package discord

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// pipe_test.go - transport tests against the in-process fake pipe: handshake
// shape, suffix fallback, PING -> PONG, and reconnect after the server drops.
// No test connects to the real \\.\pipe\discord-ipc-* namespace.

// connectPair starts a FakeServer on suffix and a Client pointed at it, waits
// for the connection, consumes and returns the client's handshake frame.
func connectPair(t *testing.T, suffix int, cfg Config) (*Client, *FakeConn, []byte) {
	t.Helper()
	srv := NewFakeServer(suffix)
	accept := make(chan *FakeConn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		sc, err := srv.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accept <- sc
	}()

	if cfg.ClientID == "" {
		cfg.ClientID = "123456789012345678"
	}
	if cfg.PipeBase == "" {
		cfg.PipeBase = srv.Base
	}
	if cfg.RetryDelay == 0 {
		cfg.RetryDelay = 30 * time.Millisecond
	}
	c := New(cfg)
	c.Start()
	t.Cleanup(func() { c.Close() })

	var sc *FakeConn
	select {
	case sc = <-accept:
	case err := <-acceptErr:
		t.Fatalf("fake server accept: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("client did not connect to the fake pipe")
	}

	op, hs, err := sc.ReadFrame()
	if err != nil {
		t.Fatalf("read handshake: %v", err)
	}
	if op != OpHandshake {
		t.Fatalf("first frame opcode = %d, want %d", op, OpHandshake)
	}
	// Discord answers the handshake with a READY dispatch; the client refuses to
	// send commands until it has seen READY.
	if err := sc.WriteFrame(OpFrame, []byte(`{"cmd":"DISPATCH","evt":"READY","data":{"v":1}}`)); err != nil {
		t.Fatalf("write READY: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for c.Generation() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("client never became ready after the READY dispatch")
		}
		time.Sleep(2 * time.Millisecond)
	}
	return c, sc, hs
}

func TestHandshakePayload(t *testing.T) {
	const id = "987654321098765432"
	_, sc, hs := connectPair(t, 3, Config{ClientID: id}) // server on suffix 3: 0..2 must fail first
	defer sc.Close()

	const want = `{"v":1,"client_id":"987654321098765432"}`
	if string(hs) != want {
		t.Fatalf("handshake = %s, want %s", hs, want)
	}
}

func TestPingPong(t *testing.T) {
	_, sc, _ := connectPair(t, 0, Config{})
	defer sc.Close()

	if err := sc.WriteFrame(OpPing, []byte("ping-payload")); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	op, payload, err := sc.ReadFrame()
	if err != nil {
		t.Fatalf("read pong: %v", err)
	}
	if op != OpPong {
		t.Fatalf("reply opcode = %d, want %d", op, OpPong)
	}
	if string(payload) != "ping-payload" {
		t.Fatalf("pong payload = %q, want %q", payload, "ping-payload")
	}
}

func TestReconnectAfterDrop(t *testing.T) {
	srv := NewFakeServer(0)
	handshakes := make(chan string, 2)
	go func() {
		for i := 0; i < 2; i++ {
			sc, err := srv.Accept()
			if err != nil {
				return
			}
			_, p, err := sc.ReadFrame()
			if err != nil {
				sc.Close()
				return
			}
			handshakes <- string(p)
			sc.Close() // drop so the client must reconnect
		}
	}()

	drops := make(chan error, 2)
	c := New(Config{
		ClientID:   "424242424242424242",
		PipeBase:   srv.Base,
		RetryDelay: 40 * time.Millisecond,
		OnDrop: func(e error) {
			select {
			case drops <- e:
			default:
			}
		},
	})
	c.Start()
	t.Cleanup(func() { c.Close() })

	const want = `{"v":1,"client_id":"424242424242424242"}`
	for i := 0; i < 2; i++ {
		select {
		case hs := <-handshakes:
			if hs != want {
				t.Fatalf("handshake %d = %s, want %s", i, hs, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for handshake %d (reconnect failed)", i)
		}
	}
	select {
	case <-drops:
	case <-time.After(5 * time.Second):
		t.Fatal("OnDrop was not called after the server closed the pipe")
	}
}

// nonceOf decodes the nonce from a client frame payload.
func nonceOf(t *testing.T, payload []byte) string {
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

// waitReady blocks until the client's generation reaches want.
func waitReady(t *testing.T, c *Client, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for c.Generation() < want {
		if time.Now().After(deadline) {
			t.Fatalf("client never became ready (gen=%d, want >=%d)", c.Generation(), want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestStalledPeerWriteIsBounded proves the reviewer's MEDIUM finding is closed:
// a peer that accepts the connection, completes READY, then stops draining must
// not stall the client. The send returns once the write deadline expires, the
// client drops the truncated connection and reconnects, and a later push is
// served on the fresh connection.
func TestStalledPeerWriteIsBounded(t *testing.T) {
	srv := NewFakeServer(0)
	srv.BufferBytes = 256 // small enough that a normal SET_ACTIVITY blocks

	accept := make(chan *FakeConn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		sc, err := srv.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accept <- sc
	}()

	replies := make(chan Reply, 4)
	c := New(Config{
		ClientID:   "123456789012345678",
		PipeBase:   srv.Base,
		RetryDelay: 30 * time.Millisecond,
		OnReply:    func(r Reply) { replies <- r },
	})
	c.writeTimeout = 200 * time.Millisecond // test-only; the production default is ackTimeout
	c.Start()
	t.Cleanup(func() { c.Close() })

	var sc *FakeConn
	select {
	case sc = <-accept:
	case err := <-acceptErr:
		t.Fatalf("fake server accept: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("client did not connect to the fake pipe")
	}
	defer sc.Close()

	if op, _, err := sc.ReadFrame(); err != nil || op != OpHandshake {
		t.Fatalf("handshake: op=%d err=%v", op, err)
	}
	if err := sc.WriteFrame(OpFrame, []byte(`{"cmd":"DISPATCH","evt":"READY","data":{"v":1}}`)); err != nil {
		t.Fatalf("write READY: %v", err)
	}
	waitReady(t, c, 1)

	// The server now stops reading. A payload larger than the pipe buffer blocks
	// in writeFrame unless the deadline bounds it.
	sent := make(chan error, 1)
	start := time.Now()
	go func() {
		sent <- c.SetActivity(Activity{Details: strings.Repeat("x", 4096), State: "S1 E1"})
	}()
	select {
	case err := <-sent:
		if err == nil {
			t.Fatal("SetActivity to a stalled peer returned nil, want a write error")
		}
		if el := time.Since(start); el > 3*time.Second {
			t.Fatalf("SetActivity stalled %s; the write is not bounded", el)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SetActivity never returned; the write is not bounded")
	}

	// The client must reconnect on a fresh instance; the new server drains and a
	// later push must be served.
	accept2 := make(chan *FakeConn, 1)
	go func() {
		sc2, err := srv.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accept2 <- sc2
	}()
	var sc2 *FakeConn
	select {
	case sc2 = <-accept2:
	case err := <-acceptErr:
		t.Fatalf("fake server accept (reconnect): %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("client did not reconnect after the bounded write")
	}
	defer sc2.Close()

	if op, _, err := sc2.ReadFrame(); err != nil || op != OpHandshake {
		t.Fatalf("reconnect handshake: op=%d err=%v", op, err)
	}
	if err := sc2.WriteFrame(OpFrame, []byte(`{"cmd":"DISPATCH","evt":"READY","data":{"v":1}}`)); err != nil {
		t.Fatalf("write READY (reconnect): %v", err)
	}
	waitReady(t, c, 2)

	if err := c.SetActivity(Activity{Details: "Show", State: "S1 E2"}); err != nil {
		t.Fatalf("later push after reconnect: %v", err)
	}
	op, frame, err := sc2.ReadFrame()
	if err != nil || op != OpFrame {
		t.Fatalf("read later push: op=%d err=%v", op, err)
	}
	want := nonceOf(t, frame)
	if err := sc2.WriteFrame(OpFrame, []byte(`{"cmd":"SET_ACTIVITY","nonce":"`+want+`"}`)); err != nil {
		t.Fatalf("write later ack: %v", err)
	}
	select {
	case r := <-replies:
		if r.Nonce != want {
			t.Fatalf("later push ack nonce=%s, want %s", r.Nonce, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("later push was not served after the bounded write")
	}
}

// TestOverlappingSendsBothAccounted proves the reviewer's LOW finding is closed:
// two sends that overlap (clear-on-exit racing the poll loop) each keep their own
// verdict; the earlier one is not silently overwritten by the later.
func TestOverlappingSendsBothAccounted(t *testing.T) {
	replies := make(chan string, 2)
	c, sc, _ := connectPair(t, 0, Config{
		OnReply: func(r Reply) { replies <- r.Nonce },
	})
	defer sc.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			if err := c.SetActivity(Activity{Details: "Show", State: "S1 E2"}); err != nil {
				t.Errorf("SetActivity: %v", err)
			}
		}()
	}

	// Read both frames: both sends are now in flight (both beginPending calls
	// happened before either reply can be written).
	nonces := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		op, frame, err := sc.ReadFrame()
		if err != nil || op != OpFrame {
			t.Fatalf("read frame %d: op=%d err=%v", i, op, err)
		}
		nonces = append(nonces, nonceOf(t, frame))
	}
	wg.Wait()

	for _, n := range nonces {
		if err := sc.WriteFrame(OpFrame, []byte(`{"cmd":"SET_ACTIVITY","nonce":"`+n+`"}`)); err != nil {
			t.Fatalf("reply nonce %s: %v", n, err)
		}
	}

	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case n := <-replies:
			got[n] = true
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d of 2 verdicts accounted for; got %v want %v", i, got, nonces)
		}
	}
	for _, n := range nonces {
		if !got[n] {
			t.Fatalf("verdict for nonce %s was dropped; accounted: %v", n, got)
		}
	}
}
