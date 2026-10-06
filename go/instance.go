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

// acquireInstanceLock binds 127.0.0.1:17843 with backlog 10 and keeps the
// socket open. On failure it returns the bind error (the caller prints the
// "already running" line and exits 0).
func acquireInstanceLock() error {
	// No other package imports "net" here, so start Winsock explicitly.
	var wsa syscall.WSAData
	if err := syscall.WSAStartup(0x0202, &wsa); err != nil {
		return err
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		return err
	}
	sa := &syscall.SockaddrInet4{Port: lockPort, Addr: [4]byte{127, 0, 0, 1}}
	if err := syscall.Bind(fd, sa); err != nil {
		syscall.CloseHandle(fd)
		return err
	}
	if err := syscall.Listen(fd, lockBacklog); err != nil {
		syscall.CloseHandle(fd)
		return err
	}
	instanceLock = fd
	return nil
}
