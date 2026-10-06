package main

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// debuglog.go - the port's single debug log.
//
// Contract: appends HH:mm:ss + " " + msg, one line per event, beside the
// executable (Java used dist\potflix-debug.log = the jar's parent).
//
// Deliberate deviation (fleet decision 8): size-based rotation instead of the
// Java app's unbounded growth (observed 1,272,183 B / 13,536 lines). The cap
// and file count are parameters so the selftest can force a tiny cap and prove
// the real rotation path.

const (
	defaultLogCapBytes int64 = 5 * 1024 * 1024
	defaultLogMaxFiles       = 3 // potflix-debug.log + .1 + .2
)

type logger struct {
	mu       sync.Mutex
	path     string
	capBytes int64
	maxFiles int
	f        *os.File
	size     int64
}

func newLogger(path string, capBytes int64, maxFiles int) *logger {
	l := &logger{path: path, capBytes: capBytes, maxFiles: maxFiles}
	l.openLocked()
	return l
}

func (l *logger) openLocked() {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		l.f = nil
		return
	}
	l.f = f
	if st, err := f.Stat(); err == nil {
		l.size = st.Size()
	}
}

// Close releases the file handle. Rotation hooks are not needed elsewhere.
func (l *logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
}

func (l *logger) logf(format string, args ...any) {
	if l == nil {
		return
	}
	msg := fmt.Sprintf(format, args...)
	line := time.Now().Format("15:04:05") + " " + msg + "\n"

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return
	}
	if l.capBytes > 0 && l.size+int64(len(line)) > l.capBytes {
		l.rotateLocked()
	}
	n, _ := l.f.WriteString(line)
	l.size += int64(n)
}

// rotateLocked shifts .N-1 -> .N, drops the oldest, and starts a fresh file.
func (l *logger) rotateLocked() {
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
	if l.maxFiles < 2 {
		l.openLocked() // no rotation slots: just reopen/append
		return
	}
	os.Remove(l.rotatedPath(l.maxFiles - 1))
	for i := l.maxFiles - 2; i >= 1; i-- {
		os.Rename(l.rotatedPath(i), l.rotatedPath(i+1))
	}
	os.Rename(l.path, l.rotatedPath(1))
	l.openLocked()
}

func (l *logger) rotatedPath(i int) string {
	return fmt.Sprintf("%s.%d", l.path, i)
}
