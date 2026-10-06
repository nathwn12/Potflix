package discord

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"
	"unsafe"
)

// fakepipe.go - an in-process named-pipe server that speaks the Discord IPC
// framing. It exists so the package tests and `--selftest` can prove the
// transport end to end without a running Discord (and, per the M3 brief,
// without ever touching the real \\.\pipe\discord-ipc-* namespace: every
// instance lives under a process-unique \\.\pipe\potflix-fake-* name).
//
// kernel32 is reached lazily, exactly like the rest of the app.

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procCreateNamedPipeW = kernel32.NewProc("CreateNamedPipeW")
	procConnectNamedPipe = kernel32.NewProc("ConnectNamedPipe")
)

const (
	pipeAccessDuplex       = 0x00000003 // PIPE_ACCESS_DUPLEX
	pipeUnlimitedInstances = 255        // PIPE_UNLIMITED_INSTANCES
	pipeBufferBytes        = 64 * 1024
)

// FakeServer creates one named-pipe instance per Accept under Base+Suffix.
type FakeServer struct {
	Base   string // prefix; the full name is Base + Suffix
	Suffix int
	// BufferBytes overrides the per-direction pipe buffer size (0 =
	// pipeBufferBytes). A tiny value lets a test make the client's write block
	// against a server that has stopped draining.
	BufferBytes int
}

// NewFakeServer returns a server bound to a process-unique pipe namespace.
// suffix selects which discord-ipc-N index it occupies, so a test can force the
// client to fall back past lower suffixes.
func NewFakeServer(suffix int) *FakeServer {
	return &FakeServer{
		Base:   fmt.Sprintf(`\\.\pipe\potflix-fake-%d-%d-`, os.Getpid(), time.Now().UnixNano()),
		Suffix: suffix,
	}
}

// Name is the full pipe path this server listens on.
func (s *FakeServer) Name() string { return s.Base + strconv.Itoa(s.Suffix) }

// Accept creates a fresh pipe instance and blocks until a client opens it. A
// second call after a drop accepts the reconnect on a new instance.
func (s *FakeServer) Accept() (*FakeConn, error) {
	name, _ := syscall.UTF16PtrFromString(s.Name())
	buf := s.BufferBytes
	if buf <= 0 {
		buf = pipeBufferBytes
	}
	h, _, callErr := procCreateNamedPipeW.Call(
		uintptr(unsafe.Pointer(name)),
		pipeAccessDuplex,
		0, // byte-type, blocking
		pipeUnlimitedInstances,
		uintptr(buf), uintptr(buf),
		0, 0,
	)
	if h == 0 || h == uintptr(syscall.InvalidHandle) {
		return nil, fmt.Errorf("CreateNamedPipeW(%s): %v", s.Name(), callErr)
	}
	// Blocks until a client connects. ERROR_PIPE_CONNECTED (a client raced us
	// between create and connect) is also success, so the return value is
	// intentionally not checked.
	procConnectNamedPipe.Call(h, 0)
	f := os.NewFile(h, s.Name())
	if f == nil {
		return nil, fmt.Errorf("os.NewFile(%s) failed", s.Name())
	}
	return &FakeConn{f: f}, nil
}

// FakeConn is one accepted server-side connection.
type FakeConn struct{ f *os.File }

// ReadFrame reads the next client frame.
func (c *FakeConn) ReadFrame() (Opcode, []byte, error) { return readFrame(c.f) }

// WriteFrame writes one server frame.
func (c *FakeConn) WriteFrame(op Opcode, payload []byte) error {
	return writeFrame(c.f, op, payload)
}

// Close drops the connection, which the client must detect and reconnect from.
func (c *FakeConn) Close() error { return c.f.Close() }
