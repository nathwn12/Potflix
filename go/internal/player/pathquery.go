package player

import (
	"errors"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// pathquery.go - full-path query for the file PotPlayer is playing, stdlib
// only (lazy user32 procs; no golang.org/x/sys).
//
// Protocol (verified live): PostMessageW(potHwnd, WM_USER (1024),
// POT_GET_PLAYFILE_NAME (24608 / 0x6020), receiverHwnd). PotPlayer answers
// SendMessageW(receiverHwnd, WM_COPYDATA (0x004A), wParam, &COPYDATASTRUCT)
// with dwData == 0x6020 and lpData pointing at UTF-8 path bytes.
//
// PostMessage is used for the request (never SendMessage cross-thread); the
// calling thread pumps its own queue with PeekMessageW until the reply's
// WndProc signals or ~800ms elapse. Empty/timeout yields ("", error) so the
// caller treats it as "no path".

var (
	procPathRegisterClassExW = user32.NewProc("RegisterClassExW")
	procPathCreateWindowExW  = user32.NewProc("CreateWindowExW")
	procPathDefWindowProcW   = user32.NewProc("DefWindowProcW")
	procPathDestroyWindow    = user32.NewProc("DestroyWindow")
	procPathPostMessageW     = user32.NewProc("PostMessageW")
	procPathPeekMessageW     = user32.NewProc("PeekMessageW")
	procPathTranslateMessage = user32.NewProc("TranslateMessage")
	procPathDispatchMessageW = user32.NewProc("DispatchMessageW")

	playerKernel32           = syscall.NewLazyDLL("kernel32.dll")
	procPathGetModuleHandleW = playerKernel32.NewProc("GetModuleHandleW")
	procPathRtlMoveMemory    = playerKernel32.NewProc("RtlMoveMemory")
)

const (
	wmCopyData = 0x004A

	pmRemove = 1

	pathQueryClassName = "PotflixPathQuery"

	// hwndMessage is (HWND)-3: CreateWindowExW parent for a message-only
	// window, which never becomes visible and only receives messages.
	hwndMessage = ^uintptr(2)

	pathQueryTimeout = 800 * time.Millisecond
)

// copyData mirrors COPYDATASTRUCT (dwData ULONG_PTR, cbData DWORD, lpData
// PVOID; 24 bytes on amd64 with the mid padding).
type copyData struct {
	dwData uintptr
	cbData uint32
	_      uint32
	lpData uintptr
}

// wndClassExPath mirrors WNDCLASSEXW, matching the layout used in win32.go.
type wndClassExPath struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

// pathMsg mirrors MSG, sized for PeekMessageW/Translate/Dispatch.
type pathMsg struct {
	hwnd    uintptr
	message uint32
	_       uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	ptX     int32
	ptY     int32
	private uint32
	_       uint32
}

// pathQueryResult carries a decoded reply from the WndProc to CurrentFile.
type pathQueryResult struct {
	path string
	ok   bool
}

// pathQueryMu serialises CurrentFile so the single package-level reply slot
// below is safe to use from the poll goroutine.
var pathQueryMu sync.Mutex

// pathQueryCh is the in-flight reply slot, set for the duration of one
// CurrentFile call while pathQueryMu is held.
var pathQueryCh chan pathQueryResult

// pathQueryWndProc answers WM_COPYDATA with dwData == msgGetPlayFileName by
// copying cbData bytes out of lpData and signalling the in-flight call. The
// COPYDATASTRUCT and payload are read with RtlMoveMemory rather than a direct
// uintptr->Pointer dereference, which go vet flags in a NewCallback handler.
var pathQueryWndProc = syscall.NewCallback(func(hwnd, msg, wParam, lParam uintptr) uintptr {
	if uint32(msg) == wmCopyData && lParam != 0 {
		var cds copyData
		procPathRtlMoveMemory.Call(uintptr(unsafe.Pointer(&cds)), lParam, uintptr(unsafe.Sizeof(cds)))
		if cds.cbData > 0 && cds.lpData != 0 {
			data := make([]byte, cds.cbData)
			procPathRtlMoveMemory.Call(uintptr(unsafe.Pointer(&data[0])), cds.lpData, uintptr(cds.cbData))
			if s, ok := DecodeCopyDataString(cds.dwData, data); ok {
				if ch := pathQueryCh; ch != nil {
					select {
					case ch <- pathQueryResult{path: s, ok: true}:
					default:
					}
				}
				return 1
			}
		}
	}
	r, _, _ := procPathDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r
})

var (
	pathQueryClassOnce  sync.Once
	pathQueryClassNameP *uint16
	pathQueryClassErr   error
)

// ensurePathQueryClass registers the receiver window class exactly once.
func ensurePathQueryClass() error {
	pathQueryClassOnce.Do(func() {
		p, err := syscall.UTF16PtrFromString(pathQueryClassName)
		if err != nil {
			pathQueryClassErr = err
			return
		}
		pathQueryClassNameP = p
		hInst, _, _ := procPathGetModuleHandleW.Call(0)
		wc := wndClassExPath{
			lpfnWndProc:   pathQueryWndProc,
			hInstance:     hInst,
			lpszClassName: p,
		}
		wc.cbSize = uint32(unsafe.Sizeof(wc))
		r, _, _ := procPathRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
		if r == 0 {
			pathQueryClassErr = errors.New("player: RegisterClassExW for path query failed")
		}
	})
	return pathQueryClassErr
}

// DecodeCopyDataString decodes one WM_COPYDATA payload: it reports ok iff
// dwData is msgGetPlayFileName (0x6020) and data is non-empty, trimming a
// single trailing NUL; the remaining bytes are the UTF-8 path.
func DecodeCopyDataString(dwData uintptr, data []byte) (string, bool) {
	if dwData != uintptr(msgGetPlayFileName) {
		return "", false
	}
	if len(data) == 0 {
		return "", false
	}
	if data[len(data)-1] == 0 {
		data = data[:len(data)-1]
	}
	return string(data), true
}

// CurrentFile returns the full path of the file PotPlayer (hwnd) is playing.
// An empty reply, a failed post, or no reply within ~800ms yields ("", error)
// and the caller treats it as "no path". It is safe for the poll goroutine.
func CurrentFile(hwnd uintptr) (string, error) {
	if hwnd == 0 {
		return "", errors.New("player: no PotPlayer window")
	}
	pathQueryMu.Lock()
	defer pathQueryMu.Unlock()

	if err := ensurePathQueryClass(); err != nil {
		return "", err
	}
	ch := make(chan pathQueryResult, 1)
	pathQueryCh = ch
	defer func() { pathQueryCh = nil }()

	hInst, _, _ := procPathGetModuleHandleW.Call(0)
	r, _, _ := procPathCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(pathQueryClassNameP)),
		0,
		0, 0, 0, 0, 0,
		hwndMessage,
		0,
		hInst,
		0,
	)
	if r == 0 {
		return "", errors.New("player: CreateWindowExW for path query failed")
	}
	receiver := r
	defer procPathDestroyWindow.Call(receiver)

	ok, _, _ := procPathPostMessageW.Call(hwnd, requestType, uintptr(msgGetPlayFileName), receiver)
	if ok == 0 {
		return "", errors.New("player: PostMessageW for play file name failed")
	}

	deadline := time.Now().Add(pathQueryTimeout)
	var msg pathMsg
	for {
		select {
		case res := <-ch:
			if !res.ok || res.path == "" {
				return "", errors.New("player: empty play file name")
			}
			return res.path, nil
		default:
		}
		if time.Now().After(deadline) {
			return "", errors.New("player: play file name query timed out")
		}
		r, _, _ := procPathPeekMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0, pmRemove)
		if r != 0 {
			procPathTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
			procPathDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
		} else {
			time.Sleep(5 * time.Millisecond)
		}
	}
}
