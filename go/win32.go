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

// Settings-window bindings (additive for the PotflixMainWnd seam): paint,
// show, message, cursor, and folder-picker procs; gdi32 + ole32. Only the
// constants this seam uses are declared below.
var (
	procShowWindow     = user32.NewProc("ShowWindow")
	procUpdateWindow   = user32.NewProc("UpdateWindow")
	procGetClientRect  = user32.NewProc("GetClientRect")
	procBeginPaint     = user32.NewProc("BeginPaint")
	procEndPaint       = user32.NewProc("EndPaint")
	procDrawTextW      = user32.NewProc("DrawTextW")
	procSendMessageW   = user32.NewProc("SendMessageW")
	procLoadCursorW    = user32.NewProc("LoadCursorW")
	procEnableWindow   = user32.NewProc("EnableWindow")
	procSetWindowTextW = user32.NewProc("SetWindowTextW")
	procGetDlgItem     = user32.NewProc("GetDlgItem")

	procSHBrowseForFolderW   = shell32.NewProc("SHBrowseForFolderW")
	procSHGetPathFromIDListW = shell32.NewProc("SHGetPathFromIDListW")

	gdi32 = syscall.NewLazyDLL("gdi32.dll")
	ole32 = syscall.NewLazyDLL("ole32.dll")

	procCreateSolidBrush   = gdi32.NewProc("CreateSolidBrush")
	procDeleteObject       = gdi32.NewProc("DeleteObject")
	procSelectObject       = gdi32.NewProc("SelectObject")
	procSetBkMode          = gdi32.NewProc("SetBkMode")
	procSetTextColor       = gdi32.NewProc("SetTextColor")
	procRoundRect          = gdi32.NewProc("RoundRect")
	procCreateFontW        = gdi32.NewProc("CreateFontW")
	procCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	procBitBlt             = gdi32.NewProc("BitBlt")
	procDeleteDC           = gdi32.NewProc("DeleteDC")

	procOleInitialize = ole32.NewProc("OleInitialize")
	procCoTaskMemFree = ole32.NewProc("CoTaskMemFree")
)

// Show / paint / icon messages.
const (
	swHide = 0
	swShow = 5

	wmPaint   = 0x000F
	wmCommand = 0x0111
	wmSetIcon = 0x0080

	iconSmall = 0
	iconBig   = 1
)

// Window / control styles used by the settings window.
const (
	wsCaption     = 0x00C00000
	wsSysMenu     = 0x00080000
	wsMinimizeBox = 0x00020000
	wsChild       = 0x40000000
	wsVisible     = 0x10000000
	wsBorder      = 0x00800000
	wsVScroll     = 0x00200000
	wsTabStop     = 0x00010000

	lbsNotify = 0x0001
	bnClicked = 0
)

// List box messages used by the settings window.
const (
	lbAddString    = 0x0180
	lbDeleteString = 0x0182
	lbResetContent = 0x0184
	lbGetCurSel    = 0x0188
	lbGetText      = 0x0189
)

// Paint constants used by the settings window.
const (
	dtLeft       = 0x0000
	dtSingleLine = 0x0020
	dtNoPrefix   = 0x0800

	transparent = 1

	fwNormal = 400
	fwBold   = 700

	defaultCharset = 1

	idcArrow = 32512

	bifReturnOnlyFSDirs = 0x0001
	bifNewDialogStyle   = 0x0040

	cwUseDefault = uintptr(0x80000000)
)

// rect mirrors RECT.
type rect struct {
	left   int32
	top    int32
	right  int32
	bottom int32
}

// paintStruct mirrors PAINTSTRUCT (68 bytes on amd64).
type paintStruct struct {
	hdc       uintptr
	erase     int32
	left      int32
	top       int32
	right     int32
	bottom    int32
	restore   int32
	incUpdate int32
	reserved  [32]byte
}

// browseInfoW mirrors BROWSEINFOW on amd64.
type browseInfoW struct {
	hwndOwner   uintptr
	pidlRoot    uintptr
	displayName *uint16
	title       *uint16
	flags       uint32
	_           uint32
	callback    uintptr
	lParam      uintptr
	image       int32
	_           int32
}

// Owner-drawn settings window bindings, round 2 (append-only): DPI,
// geometry, mouse tracking, and GDI helpers for the fully owner-drawn
// PotflixMainWnd. Nothing above this block is modified.
var (
	procGetDpiForWindow    = user32.NewProc("GetDpiForWindow")
	procGetDpiForSystem    = user32.NewProc("GetDpiForSystem")
	procGetSystemMetrics   = user32.NewProc("GetSystemMetrics")
	procGetWindowRect      = user32.NewProc("GetWindowRect")
	procMoveWindow         = user32.NewProc("MoveWindow")
	procAdjustWindowRectEx = user32.NewProc("AdjustWindowRectEx")
	procInvalidateRect     = user32.NewProc("InvalidateRect")
	procTrackMouseEvent    = user32.NewProc("TrackMouseEvent")
	procDrawIconEx         = user32.NewProc("DrawIconEx")
	procSetCursor          = user32.NewProc("SetCursor")
	procFillRect           = user32.NewProc("FillRect")

	procCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	procCreatePen              = gdi32.NewProc("CreatePen")
	procGetStockObject         = gdi32.NewProc("GetStockObject")
)

// Owner-drawn window messages.
const (
	wmMouseMove   = 0x0200
	wmLButtonDown = 0x0201
	wmLButtonUp   = 0x0202
	wmSetCursor   = 0x0020
	wmEraseBkgnd  = 0x0014
	wmSize        = 0x0005
	wmDpiChanged  = 0x02E0
	wmMouseLeave  = 0x02A3
)

// DrawTextW alignment/ellipsis flags for the owner-drawn UI.
const (
	dtCenter      = 0x0001
	dtRight       = 0x0002
	dtVCenter     = 0x0004
	dtEndEllipsis = 0x8000
)

// Pen, blit, icon, cursor, and metrics constants for the owner-drawn UI.
const (
	psSolid = 0

	srccopy = 0x00CC0020

	diNormal = 0x0003

	idcHand = 32649

	smCxScreen = 0
	smCyScreen = 1

	tmeLeave = 0x00000002

	fwSemiBold = 600

	nullBrushStock = 5
)

// trackMouseEvent mirrors TRACKMOUSEEVENT.
type trackMouseEvent struct {
	cbSize      uint32
	dwFlags     uint32
	hwndTrack   uintptr
	dwHoverTime uint32
}
