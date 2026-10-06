package player

import (
	"strings"
	"syscall"
	"unsafe"
)

// window.go - PotPlayer window discovery and title/liveness reads, stdlib only
// (lazy user32 procs; no golang.org/x/sys).
//
// Contract section 2/6: EnumWindows finds the first top-level window whose
// title ends with " - PotPlayer" or equals "PotPlayer"; only that one is
// tracked. Per poll the title is re-read with GetWindowTextW and liveness is
// checked with IsWindow.

var (
	user32 = syscall.NewLazyDLL("user32.dll")

	procEnumWindows    = user32.NewProc("EnumWindows")
	procGetWindowTextW = user32.NewProc("GetWindowTextW")
	procIsWindow       = user32.NewProc("IsWindow")
)

// PotPlayer's window-title convention (Java pollLoop :92-93, :97-98).
const (
	AppName = "PotPlayer"
	Suffix  = " - PotPlayer"
)

// maxWindowText is the GetWindowTextW buffer size in UTF-16 units. Java used a
// fixed WINDOW_TEXT_SIZE buffer; media titles are long, so 1024 is generous.
const maxWindowText = 1024

// Matches reports whether a window title identifies a PotPlayer window.
func Matches(title string) bool {
	return strings.HasSuffix(title, Suffix) || title == AppName
}

// enumTarget receives the first matching HWND during an EnumWindows sweep.
// FindFirst is synchronous and the poll loop is single-threaded, so this
// package-level slot is safe; it is deliberately not advertised as concurrent.
var enumTarget uintptr

// enumWindowsCallback stops the enumeration (returns FALSE) on the first match.
var enumWindowsCallback = syscall.NewCallback(func(hwnd, lparam uintptr) uintptr {
	if Matches(Title(hwnd)) {
		enumTarget = hwnd
		return 0
	}
	return 1
})

// FindFirst returns the first PotPlayer window in EnumWindows order and whether
// one was found. Java tracked only windows.get(0); multiple players are ignored.
func FindFirst() (uintptr, bool) {
	enumTarget = 0
	procEnumWindows.Call(enumWindowsCallback, 0)
	return enumTarget, enumTarget != 0
}

// Title reads a window's current title via GetWindowTextW. It returns "" when
// the window has no title or is gone.
func Title(hwnd uintptr) string {
	buf := make([]uint16, maxWindowText)
	n, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:n])
}

// Alive reports whether a window handle is still valid (Java IsWindow, :110).
func Alive(hwnd uintptr) bool {
	r, _, _ := procIsWindow.Call(hwnd)
	return r != 0
}
