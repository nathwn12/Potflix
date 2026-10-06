package discord

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// pipe.go - the local Discord IPC transport (contract section 5, fleet decision
// 9): a named-pipe client for \\.\pipe\discord-ipc-0..9, stdlib only.
//
// Frame = int32 LE opcode + int32 LE length + JSON payload. Opcodes: 0 handshake,
// 1 frame, 2 close, 3 ping, 4 pong. The client sends the handshake, a read-pump
// goroutine answers PING with PONG and surfaces READY/dispatch frames, and any
// transport error triggers a reconnect after Config.RetryDelay (5000 ms), with
// the contract's `session dropped: <err>` line emitted through OnDrop.
//
// Discord answers every command with the SAME nonce. send records each
// outstanding nonce in a nonce-keyed set and the read pump matches an incoming
// frame against it; a matched frame is surfaced through Config.OnReply (an
// evt:"ERROR" rejection carries Discord's own data.code and data.message).
// Commands are held back until Discord's READY dispatch (ErrNotReady before it),
// because Discord discards commands sent earlier; READY also fires
// Config.OnConnect and bumps the generation. Every write is bounded by a
// deadline, because wmu is held across it: a peer that accepted the connection
// but stopped draining would otherwise stall the poll loop forever. Nothing on
// this path is swallowed: the caller logs every reply, every send error, a
// `push ack timeout` when a command gets no reply, and a write expiry.

// Opcode is the frame's leading 32-bit little-endian opcode.
type Opcode int32

// Discord IPC opcodes.
const (
	OpHandshake Opcode = 0
	OpFrame     Opcode = 1
	OpClose     Opcode = 2
	OpPing      Opcode = 3
	OpPong      Opcode = 4
)

// DefaultPipeBase is the Discord IPC namespace; suffix 0..9 is appended.
const DefaultPipeBase = `\\.\pipe\discord-ipc-`

// pipeSuffixes is the number of Discord IPC sockets probed per attempt.
const pipeSuffixes = 10

// maxFrameBytes bounds a single payload read, so a corrupt length cannot make
// the client allocate unbounded memory.
const maxFrameBytes = 1 << 20

// ackTimeout is how long a pushed command waits for Discord's reply before the
// client reports the write path as unproven (`push ack timeout`): a write that
// left the process but was starved/dead gets said out loud, not swallowed.
const ackTimeout = 2 * time.Second

// ErrNotConnected is returned when a frame is pushed before the pipe is up (or
// after it dropped). The caller keeps polling; the supervisor reconnects.
var ErrNotConnected = errors.New("discord: not connected")

// ErrNotReady is returned when a command is pushed after the handshake but
// before Discord's READY dispatch. Discord discards commands sent before READY,
// so nothing is written until the session is ready.
var ErrNotReady = errors.New("discord: not ready")

// handshake is the one-time client hello: {"v":1,"client_id":"<id>"}.
type handshake struct {
	V        int    `json:"v"`
	ClientID string `json:"client_id"`
}

// Config configures one Client. Only ClientID is required; the rest fall back to
// the contract defaults (RetryDelay 5000 ms) or the fixed Discord namespace.
type Config struct {
	ClientID     string
	PipeBase     string        // default DefaultPipeBase
	RetryDelay   time.Duration // default 5s; the contract's reconnect delay
	OnConnect    func()        // after a successful handshake (generation bumped)
	OnFrame      func(payload []byte)
	OnReply      func(Reply)        // a frame whose nonce matched an outstanding command
	OnAckTimeout func(nonce string) // no reply to an outstanding command in ackTimeout
	OnDrop       func(err error)
}

// Reply is one server frame matched to an outstanding command by nonce. Error is
// true for an evt:"ERROR" rejection; Code/Message then carry Discord's own
// data.code / data.message (the client id is redacted before Message is exposed).
type Reply struct {
	Nonce   string
	Evt     string
	Code    int
	Message string
	Error   bool
}

// Client is one reconnect-aware IPC client. Start launches the supervisor; all
// writes are serialized so a PONG from the read pump cannot interleave with a
// SET_ACTIVITY frame.
type Client struct {
	cfg  Config
	done chan struct{}

	wmu   sync.Mutex // serializes frame writes
	mu    sync.Mutex // guards conn/ready/closed
	conn  *os.File
	ready bool // true once Discord's READY dispatch has arrived
	shut  bool

	pmu     sync.Mutex             // guards pending
	pending map[string]*time.Timer // in-flight commands, keyed by nonce

	gen atomic.Int64 // increments on each successful handshake

	// writeTimeout bounds every frame write (default ackTimeout). wmu is held
	// across a write, so without this a non-draining peer stalls the poll loop.
	writeTimeout time.Duration
}

// New returns a client; it does not connect until Start.
func New(cfg Config) *Client {
	if cfg.PipeBase == "" {
		cfg.PipeBase = DefaultPipeBase
	}
	if cfg.RetryDelay <= 0 {
		cfg.RetryDelay = 5 * time.Second
	}
	return &Client{cfg: cfg, done: make(chan struct{}), writeTimeout: ackTimeout}
}

// Start launches the connect/pump/reconnect supervisor in the background.
func (c *Client) Start() { go c.loop() }

// Close stops the supervisor and closes the pipe. It is safe to call twice.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.shut {
		c.mu.Unlock()
		return nil
	}
	c.shut = true
	c.ready = false
	close(c.done)
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()
	c.clearPending()
	if conn != nil {
		return conn.Close()
	}
	return nil
}

// loop connects, pumps until the transport drops, then retries after RetryDelay.
func (c *Client) loop() {
	for {
		err := c.connectAndPump()
		if c.closed() {
			return
		}
		if err != nil && c.cfg.OnDrop != nil {
			c.cfg.OnDrop(err)
		}
		select {
		case <-time.After(c.cfg.RetryDelay):
		case <-c.done:
			return
		}
	}
}

// connectAndPump dials one suffix, sends the handshake, and reads frames until
// an error. The connection is published only after the handshake is out.
func (c *Client) connectAndPump() error {
	conn, err := c.dial()
	if err != nil {
		return err
	}

	hs, err := json.Marshal(handshake{V: 1, ClientID: c.cfg.ClientID})
	if err != nil {
		conn.Close()
		return err
	}
	c.wmu.Lock()
	err = c.writeFrameBounded(conn, OpHandshake, hs)
	c.wmu.Unlock()
	if err != nil {
		conn.Close()
		return err
	}

	c.mu.Lock()
	c.conn = conn
	c.ready = false
	c.mu.Unlock()
	// The connection is published but NOT usable yet: Discord discards commands
	// sent before its READY dispatch, so send() returns ErrNotReady until READY
	// arrives (see dispatchFrame).
	defer func() {
		c.mu.Lock()
		c.conn = nil
		c.ready = false
		c.mu.Unlock()
		c.clearPending() // the connection is gone: no reply can arrive
		conn.Close()
	}()

	return c.pump(conn)
}

// dial probes suffix 0..9 and returns the first pipe that opens. A failure on
// every suffix is reported so the supervisor retries.
func (c *Client) dial() (*os.File, error) {
	var lastErr error
	for i := 0; i < pipeSuffixes; i++ {
		name := c.cfg.PipeBase + strconv.Itoa(i)
		f, err := openPipe(name)
		if err == nil {
			return f, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("no discord-ipc pipe 0..%d: %w", pipeSuffixes-1, lastErr)
}

// openPipe opens one named pipe and wraps the handle in an *os.File.
//
// The handle must be FILE_FLAG_OVERLAPPED. A plain os.OpenFile handle is
// synchronous, and on a synchronous pipe a pending ReadFile blocks a subsequent
// WriteFile on the same handle - which deadlocks this client's read pump against
// its own writes. The overlapped handle is registered with the Go poller by
// os.NewFile, so concurrent Read and Write are safe; everything downstream still
// uses os.File/io.
func openPipe(name string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(
		p,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_OVERLAPPED,
		0,
	)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(h), name)
	if f == nil {
		syscall.CloseHandle(h)
		return nil, fmt.Errorf("os.NewFile(%s) failed", name)
	}
	return f, nil
}

// pump reads frames until the peer closes or the transport errors.
func (c *Client) pump(conn *os.File) error {
	for {
		op, payload, err := readFrame(conn)
		if err != nil {
			return err
		}
		switch op {
		case OpPing:
			c.wmu.Lock()
			werr := c.writeFrameBounded(conn, OpPong, payload)
			c.wmu.Unlock()
			if werr != nil {
				return werr
			}
		case OpFrame:
			c.dispatchFrame(payload)
		case OpClose:
			return errors.New("closed by peer")
		case OpPong:
			// Unsolicited PONG: nothing to do.
		case OpHandshake:
			// The client sends the handshake; a server-side one is unexpected.
		default:
			// Unknown opcode: ignore, keep the stream in sync.
		}
	}
}

func (c *Client) closed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.shut
}

// Generation increments on every successful handshake. The poll loop watches it
// to detect a reconnect and re-emit the current card (contract section 4:
// "first poll after connect").
func (c *Client) Generation() int64 { return c.gen.Load() }

// SetActivity sends one SET_ACTIVITY command (activity.go).
func (c *Client) SetActivity(a Activity) error {
	return c.send(command{
		Cmd:   "SET_ACTIVITY",
		Args:  setActivityArgs{PID: pid(), Activity: newActivity(a)},
		Nonce: newNonce(),
	})
}

// ClearActivity sends SET_ACTIVITY with a null activity.
func (c *Client) ClearActivity() error {
	return c.send(command{
		Cmd:   "SET_ACTIVITY",
		Args:  setActivityArgs{PID: pid(), Activity: nil},
		Nonce: newNonce(),
	})
}

// send marshals and writes one frame. The connection pointer is snapshotted
// under mu (connectAndPump and Close replace it there), then the write itself is
// serialized by wmu; a reply cannot race the pointer read.
func (c *Client) send(cmd command) error {
	payload, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	c.mu.Lock()
	conn := c.conn
	ready := c.ready
	c.mu.Unlock()
	if conn == nil {
		return ErrNotConnected
	}
	if !ready {
		return ErrNotReady
	}
	// Record the outstanding nonce before the bytes leave, so a reply that
	// arrives immediately still matches, and arm the ack timeout.
	c.beginPending(cmd.Nonce)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	// wmu is held across the write, so bound it: a peer that accepted the
	// connection but stopped draining must not stall this send (and every later
	// one) forever. On expiry, drop the connection so the read pump unblocks and
	// the supervisor reconnects.
	err = c.writeFrameBounded(conn, OpFrame, payload)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		c.abort(conn)
		return fmt.Errorf("discord: write timeout after %s: %w", c.writeTimeout, err)
	}
	return err
}

// writeFrameBounded writes one frame with a deadline, so a blocked write cannot
// hold wmu indefinitely. openPipe's handle is overlapped and registered with the
// Go poller, so the deadline cancels a stuck write; on a non-pollable handle
// SetWriteDeadline would error and the write would proceed unbounded.
func (c *Client) writeFrameBounded(conn *os.File, op Opcode, payload []byte) error {
	_ = conn.SetWriteDeadline(time.Now().Add(c.writeTimeout))
	err := writeFrame(conn, op, payload)
	_ = conn.SetWriteDeadline(time.Time{})
	return err
}

// abort drops the current connection so the blocked read pump unblocks and the
// supervisor reconnects. Used when a write expired mid-frame: the byte stream is
// now truncated, so the connection is not reusable.
func (c *Client) abort(conn *os.File) {
	c.mu.Lock()
	if c.conn == conn {
		c.conn = nil
		c.ready = false
	}
	c.mu.Unlock()
	_ = conn.Close()
}

// beginPending records one outstanding command nonce and arms its ack timeout:
// if the nonce is still outstanding after ackTimeout, the write path is reported
// as unproven rather than silently trusted. Commands are keyed by nonce, not
// held in one slot, so two overlapping sends (clear-on-exit racing the poll
// loop) each keep their own verdict instead of the later one overwriting the
// earlier.
func (c *Client) beginPending(nonce string) {
	c.pmu.Lock()
	defer c.pmu.Unlock()
	if c.pending == nil {
		c.pending = make(map[string]*time.Timer)
	}
	if c.cfg.OnAckTimeout == nil {
		c.pending[nonce] = nil
		return
	}
	c.pending[nonce] = time.AfterFunc(ackTimeout, func() { c.ackExpired(nonce) })
}

// ackExpired reports a command still unanswered after ackTimeout, once.
func (c *Client) ackExpired(nonce string) {
	c.pmu.Lock()
	_, ok := c.pending[nonce]
	if ok {
		delete(c.pending, nonce)
	}
	c.pmu.Unlock()
	if ok && !c.closed() {
		c.cfg.OnAckTimeout(nonce)
	}
}

// takePending clears and reports whether nonce is an outstanding command, and
// disarms its ack timeout.
func (c *Client) takePending(nonce string) bool {
	c.pmu.Lock()
	defer c.pmu.Unlock()
	t, ok := c.pending[nonce]
	if !ok {
		return false
	}
	if t != nil {
		t.Stop()
	}
	delete(c.pending, nonce)
	return true
}

// clearPending drops every outstanding command and its timer (on drop/close).
func (c *Client) clearPending() {
	c.pmu.Lock()
	for n, t := range c.pending {
		if t != nil {
			t.Stop()
		}
		delete(c.pending, n)
	}
	c.pmu.Unlock()
}

// dispatchFrame handles one OpFrame payload: it completes the handshake on
// Discord's READY dispatch (bumping the generation and firing OnConnect), then
// surfaces a matched command reply (OnReply) and finally the raw frame
// (OnFrame). A frame with no matching nonce that is not READY is not a reply.
func (c *Client) dispatchFrame(payload []byte) {
	var m struct {
		Evt   *string         `json:"evt"`
		Nonce string          `json:"nonce"`
		Data  json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(payload, &m)

	// READY completes the handshake. Only now may commands be sent; the
	// generation bump tells the poll loop to re-emit the current card.
	if m.Evt != nil && *m.Evt == "READY" {
		c.mu.Lock()
		already := c.ready
		c.ready = true
		c.mu.Unlock()
		if !already {
			c.gen.Add(1)
			if c.cfg.OnConnect != nil {
				c.cfg.OnConnect()
			}
		}
	}

	if c.cfg.OnReply != nil && m.Nonce != "" && c.takePending(m.Nonce) {
		r := Reply{Nonce: m.Nonce}
		if m.Evt != nil {
			r.Evt = *m.Evt
			r.Error = *m.Evt == "ERROR"
		}
		if r.Error && len(m.Data) > 0 {
			var d struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}
			if json.Unmarshal(m.Data, &d) == nil {
				r.Code = d.Code
				r.Message = redactClientID(d.Message, c.cfg.ClientID)
			}
		}
		c.cfg.OnReply(r)
	}
	if c.cfg.OnFrame != nil {
		c.cfg.OnFrame(payload)
	}
}

// redactClientID removes the resolved client id from a message before it can be
// logged; the id VALUE is never written (M1 secret rule).
func redactClientID(s, clientID string) string {
	if s == "" || clientID == "" {
		return s
	}
	return strings.ReplaceAll(s, clientID, "<redacted>")
}

// writeFrame writes the 8-byte header and payload as one logical frame.
func writeFrame(w io.Writer, op Opcode, payload []byte) error {
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[0:4], uint32(op))
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// readFrame reads one header and its payload.
func readFrame(r io.Reader) (Opcode, []byte, error) {
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	op := Opcode(int32(binary.LittleEndian.Uint32(hdr[0:4])))
	n := int32(binary.LittleEndian.Uint32(hdr[4:8]))
	if n < 0 || n > maxFrameBytes {
		return 0, nil, fmt.Errorf("bad frame length %d", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return op, payload, nil
}
