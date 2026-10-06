package main

import "syscall"

// Win32 bindings, stdlib only: syscall.NewLazyDLL / NewLazyProc.
// No third-party module is used anywhere in this app.

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")

	procRegisterClassExW         = user32.NewProc("RegisterClassExW")
	procCreateWindowExW          = user32.NewProc("CreateWindowExW")
	procDefWindowProcW           = user32.NewProc("DefWindowProcW")
	procGetMessageW              = user32.NewProc("GetMessageW")
	procTranslateMessage         = user32.NewProc("TranslateMessage")
	procDispatchMessageW         = user32.NewProc("DispatchMessageW")
	procPostQuitMessage          = user32.NewProc("PostQuitMessage")
	procPostMessageW             = user32.NewProc("PostMessageW")
	procDestroyWindow            = user32.NewProc("DestroyWindow")
	procCreatePopupMenu          = user32.NewProc("CreatePopupMenu")
	procDestroyMenu              = user32.NewProc("DestroyMenu")
	procAppendMenuW              = user32.NewProc("AppendMenuW")
	procTrackPopupMenu           = user32.NewProc("TrackPopupMenu")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procGetCursorPos             = user32.NewProc("GetCursorPos")
	procCreateIconFromResourceEx = user32.NewProc("CreateIconFromResourceEx")

	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	procAttachConsole    = kernel32.NewProc("AttachConsole")
	procCreateFileW      = kernel32.NewProc("CreateFileW")
	procGetStdHandle     = kernel32.NewProc("GetStdHandle")

	procShellNotifyIconW = shell32.NewProc("Shell_NotifyIconW")
)

// Window messages.
const (
	wmNull        = 0x0000
	wmDestroy     = 0x0002
	wmClose       = 0x0010
	wmRButtonUp   = 0x0205
	wmContextMenu = 0x007B

	wmApp     = 0x8000
	wmAppTray = wmApp + 1 // Shell_NotifyIcon callback message
	wmAppQuit = wmApp + 2 // self-posted: stop the message loop
)

// NOTIFYICONDATAW flags and Shell_NotifyIconW verbs.
const (
	nimAdd    = 0x00000000
	nimDelete = 0x00000002

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004
)

// Menu / popup flags.
const (
	mfString       = 0x00000000
	tpmRightButton = 0x0002
	tpmBottomAlign = 0x0020
	tpmReturnCmd   = 0x0100
)

// CreateIconFromResourceEx / CreateFileW constants.
const (
	iconResourceVersion = 0x00030000
	genericWrite        = 0x40000000
	fileShareReadWrite  = 0x00000001 | 0x00000002
	openExisting        = 3
	attachParentProcess = ^uintptr(0) // (DWORD)-1

	stdOutputHandle = ^uintptr(10) // (DWORD)-11, STD_OUTPUT_HANDLE
	stdErrorHandle  = ^uintptr(11) // (DWORD)-12, STD_ERROR_HANDLE
)

type wndClassEx struct {
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

type guid struct {
	data1 uint32
	data2 uint16
	data3 uint16
	data4 [8]byte
}

// notifyIconData mirrors NOTIFYICONDATAW (Vista+ layout, 976 bytes on amd64).
type notifyIconData struct {
	cbSize           uint32
	hWnd             uintptr
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            uintptr
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uTimeout         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         guid
	hBalloonIcon     uintptr
}

type point struct {
	x int32
	y int32
}

// msgT mirrors MSG.
type msgT struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}
