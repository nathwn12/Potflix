package main

import "syscall"

// instance.go - single-instance lock, exactly as the Java app:
//   new ServerSocket(17843, 10, InetAddress.getByName("127.0.0.1"))
// held in a static field for process life (never closed).
//
// Parity notes:
//   - 127.0.0.1 only, TCP, backlog 10.
//   - Go's raw syscall.Socket does not set SO_REUSEADDR, so a second bind on
//     Windows fails with WSAEADDRINUSE - the exclusive semantics the lock needs.
//   - The listening handle is stored and never closed; it dies with the process.

const (
	lockHost    = "127.0.0.1"
	lockPort    = 17843
	lockBacklog = 10
)

// instanceLock holds the listening handle for process life.
var instanceLock syscall.Handle

// acquireInstanceLockOn binds 127.0.0.1:port with backlog 10 and returns the
// held listening handle. Port 0 asks Winsock for an ephemeral port (test use
// only - production always passes lockPort); the caller discovers it with
// Getsockname and must keep the handle open while it owns the lock. A bind or
// listen failure means another holder owns the port: the temp socket is
// released with closesocket (the Winsock release, not CloseHandle) and the
// error is returned.
func acquireInstanceLockOn(port int) (syscall.Handle, error) {
	// No other package imports "net" here, so start Winsock explicitly.
	var wsa syscall.WSAData
	if err := syscall.WSAStartup(0x0202, &wsa); err != nil {
		return 0, err
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		return 0, err
	}
	sa := &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}
	if err := syscall.Bind(fd, sa); err != nil {
		syscall.Closesocket(fd)
		return 0, err
	}
	if err := syscall.Listen(fd, lockBacklog); err != nil {
		syscall.Closesocket(fd)
		return 0, err
	}
	return fd, nil
}

// acquireInstanceLock binds 127.0.0.1:17843 with backlog 10 and keeps the
// socket open. On failure it returns the bind error (the caller prints the
// "already running" line and exits 0).
func acquireInstanceLock() error {
	fd, err := acquireInstanceLockOn(lockPort)
	if err != nil {
		return err
	}
	instanceLock = fd
	return nil
}
