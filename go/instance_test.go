package main

import (
	"sync"
	"syscall"
	"testing"
)

// instance_test.go - offline proof for the single-instance lock.
//
// Never binds the live lockPort (17843): every test stages an ephemeral
// loopback port (port 0) as a fake lock, so a live Potflix owner is neither
// touched nor disturbed. The spam burst below models rapid double-click
// opens: all contenders fail while the lock is held, and exactly one wins an
// open race - the losers' path is the caller printing alreadyRunningMsg on
// stderr and exiting 0 before any log exists (main.go takes the lock before
// creating the logger; selftest step (5) proves the exe-level message live).

// acquireFakeLock stages a held fake lock on an ephemeral loopback port. It
// redraws if Winsock ever deals the live lock port number.
func acquireFakeLock(t *testing.T) syscall.Handle {
	t.Helper()
	for tries := 0; tries < 10; tries++ {
		fd, err := acquireInstanceLockOn(0)
		if err != nil {
			t.Fatalf("fake lock acquire: %v", err)
		}
		if p := lockPortOf(t, fd); p == lockPort {
			syscall.Closesocket(fd)
			continue
		}
		return fd
	}
	t.Fatalf("ephemeral bind kept landing on live port %d", lockPort)
	return 0
}

func lockPortOf(t *testing.T, fd syscall.Handle) int {
	t.Helper()
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatalf("Getsockname: %v", err)
	}
	inet, ok := sa.(*syscall.SockaddrInet4)
	if !ok {
		t.Fatalf("Getsockname = %T, want *SockaddrInet4", sa)
	}
	if inet.Port == 0 {
		t.Fatalf("ephemeral bind returned port 0")
	}
	return inet.Port
}

// A second bind while the lock is held must fail, and the holder must still
// own the port afterwards (no handle stolen or dropped on the failure path).
func TestSecondInstanceFailsWhileHeld(t *testing.T) {
	holder := acquireFakeLock(t)
	defer syscall.Closesocket(holder)
	port := lockPortOf(t, holder)

	if _, err := acquireInstanceLockOn(port); err == nil {
		t.Fatalf("second bind to held 127.0.0.1:%d succeeded, want failure", port)
	}
	if _, err := acquireInstanceLockOn(port); err == nil {
		t.Fatalf("third bind to held 127.0.0.1:%d succeeded, holder lost the lock", port)
	}
}

// Rapid spam-open: a concurrent burst against a held lock all fails clean
// (each loser releases only its own temp socket via closesocket); then, with
// the port freed, the same burst races and exactly one contender wins - the
// OS serializes the bind, so there is no app-level window for two owners.
func TestSpamOpenSecondInstances(t *testing.T) {
	const burst = 32

	holder := acquireFakeLock(t)
	port := lockPortOf(t, holder)

	// Phase A: burst vs the held lock - every contender must fail.
	var wg sync.WaitGroup
	var mu sync.Mutex
	fails := 0
	start := make(chan struct{})
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			fd, err := acquireInstanceLockOn(port)
			if err != nil {
				mu.Lock()
				fails++
				mu.Unlock()
				return
			}
			syscall.Closesocket(fd)
			t.Errorf("spam contender bound held 127.0.0.1:%d", port)
		}()
	}
	close(start)
	wg.Wait()
	if fails != burst {
		t.Fatalf("spam burst vs held lock: %d/%d contenders failed, want all", fails, burst)
	}

	// The holder survived the burst: one more bind still fails.
	if _, err := acquireInstanceLockOn(port); err == nil {
		t.Fatalf("holder lost 127.0.0.1:%d under the spam burst", port)
	}
	syscall.Closesocket(holder)

	// Phase B: burst racing for a free port - exactly one winner. A round
	// with zero winners means a third party stole the staged port between
	// release and burst, so restage with a fresh port and retry.
	for round := 0; round < 3; round++ {
		stage := acquireFakeLock(t)
		racePort := lockPortOf(t, stage)
		syscall.Closesocket(stage)

		type outcome struct {
			fd  syscall.Handle
			err error
		}
		results := make([]outcome, burst)
		start = make(chan struct{})
		wg = sync.WaitGroup{}
		for i := range results {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				fd, err := acquireInstanceLockOn(racePort)
				results[i] = outcome{fd, err}
			}(i)
		}
		close(start)
		wg.Wait()

		wins := 0
		for _, r := range results {
			if r.err == nil {
				wins++
			}
		}
		for _, r := range results {
			if r.err == nil {
				syscall.Closesocket(r.fd)
			}
		}
		if wins == 1 {
			return
		}
		if wins > 1 {
			t.Fatalf("spam race for free 127.0.0.1:%d produced %d winners, want exactly 1", racePort, wins)
		}
	}
	t.Fatalf("spam race never produced exactly one winner in 3 rounds")
}

// The second-instance line is a contract: exact bytes on stderr, exit 0, and
// no log side effects (the lock is taken before the logger exists).
func TestAlreadyRunningMessageExact(t *testing.T) {
	const want = "Potflix is already running."
	if alreadyRunningMsg != want {
		t.Fatalf("alreadyRunningMsg = %q, want %q", alreadyRunningMsg, want)
	}
}
