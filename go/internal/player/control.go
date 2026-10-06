package player

// control.go - PotPlayer control-channel reads.
//
// The Java helper sent raw commands as SendMessage(hwnd, type, cmd, val); read
// queries use type 1024 (WM_USER) with the message id in WPARAM and read the
// LRESULT (MESSAGE-CODES.txt). Only the three queries the app uses are bound.

// requestType is the query channel: WM_USER (0x0400).
const requestType = 1024

// Query message ids from JNAMessageConst (MESSAGE-CODES.txt).
const (
	msgGetTotalTime   = 20482
	msgGetCurrentTime = 20484
	msgGetPlayStatus  = 20486
)

// PlayStatus is the decoded player state. The zero value is Stopped, matching
// Java's enum order (Stopped, Paused, Running, Undefined).
type PlayStatus int

const (
	StatusStopped PlayStatus = iota
	StatusPaused
	StatusRunning
	StatusUndefined
)

// String matches PlayStatus.toString() in the Java app (used in the decision log).
func (s PlayStatus) String() string {
	switch s {
	case StatusStopped:
		return "Stopped"
	case StatusPaused:
		return "Paused"
	case StatusRunning:
		return "Running"
	default:
		return "Undefined"
	}
}

// MapPlayStatus maps the raw GET_PLAY_STATUS int: -1 Stopped, 1 Paused,
// 2 Running, anything else (including 0) Undefined.
func MapPlayStatus(raw int) PlayStatus {
	switch raw {
	case -1:
		return StatusStopped
	case 1:
		return StatusPaused
	case 2:
		return StatusRunning
	default:
		return StatusUndefined
	}
}

var procSendMessageW = user32.NewProc("SendMessageW")

// send issues one query and returns the raw LRESULT.
func send(hwnd uintptr, msgID int) uintptr {
	r, _, _ := procSendMessageW.Call(hwnd, requestType, uintptr(msgID), 0)
	return r
}

// GetPlayStatus queries the play state. The LRESULT low 32 bits are read as a
// signed int, so a wndproc returning -1 arrives as -1 rather than 0xFFFFFFFF.
func GetPlayStatus(hwnd uintptr) PlayStatus {
	return MapPlayStatus(int(int32(uint32(send(hwnd, msgGetPlayStatus)))))
}

// GetCurrentTime queries the current playback position in milliseconds.
func GetCurrentTime(hwnd uintptr) int64 { return int64(send(hwnd, msgGetCurrentTime)) }

// GetTotalTime queries the media duration in milliseconds.
func GetTotalTime(hwnd uintptr) int64 { return int64(send(hwnd, msgGetTotalTime)) }
