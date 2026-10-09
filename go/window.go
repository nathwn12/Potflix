package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"potflix/internal/library"
)

// window.go - tray-resident settings window (PotflixMainWnd), stdlib + win32.
//
// Created hidden at startup on the UI thread; shown by the tray menu's
// "Open Potflix" item and on normal launch (main.go). WM_CLOSE hides it
// again - it is NEVER destroyed, so the tray icon and message loop are
// untouched. The whole client area is OWNER-DRAWN (dark card, folder rows,
// buttons, status line): WM_PAINT paints everything double-buffered, mouse
// messages hit-test an in-memory rect table and call the SAME actions as the
// old native buttons. Scan walks the configured DIRs and caches metadata
// incrementally; Refresh force-refetches every cached entry (both via
// internal/library). Each run executes on a worker goroutine: the wndproc
// only snapshots the config, posts progress messages, and flips the status
// text - it never blocks and never touches PotPlayer or pathquery;
// Add/Remove only touch the config store.

const mainClassName = "PotflixMainWnd"

// Owner-drawn button ids (rect-table keys, not child control ids - there are
// no child controls anymore).
const (
	btnAdd      = 1
	btnRemove   = 2
	btnScan     = 3
	btnRefresh  = 4
	btnClose    = 5
	btnSettings = 6
	btnBack     = 7
	btnSave     = 8
	btnShow     = 9
	btnGetTMDB  = 10
	btnTest     = 11
	btnGetOMDb  = 12
	btnGetTVDB  = 13
	btnGetTrakt = 14
	// Settings view, LIBRARY + STARTUP sections (see settings_sections.go).
	btnClearCache = 15
	btnExport     = 16
	btnImport     = 17
	btnStartup    = 18
	btnOpenLog    = 19
	// Settings view, SCAN section (three-way segmented control).
	btnScanGentle   = 20
	btnScanBalanced = 21
	btnScanTurbo    = 22
)

// Scan progress/completion messages posted by the worker goroutine to the
// settings window; the payloads ride scanMu-guarded vars, never wparam.
const (
	wmAppScanProgress = wmApp + 3
	wmAppScanDone     = wmApp + 4
	wmAppTMDBTestDone = wmApp + 5
)

// mainHwnd is the settings window handle, 0 until createMainWindow runs.
// createMainWindow never runs under --selftest, which stays fully headless.
var mainHwnd uintptr

// uiIcon caches the embedded icon handle for the header drawing; loaded once
// at window creation so WM_PAINT never allocates GDI resources.
var uiIcon uintptr

// uiCentered records whether the window has been centered once on the
// primary monitor; showMainWindow centers only the first show.
var uiCentered bool

// statusText is the bottom status line. UI thread only: set by
// setStatusText (button handler / scan messages), read by the painter.
var statusText = "Ready"

// uiDirs is the painted folder list, a snapshot of the config store
// refreshed by refreshDirList after every Add/Remove and at creation.
var uiDirs []string

// selRow is the selected folder row, -1 when none. Clicking a row selects
// it; Remove drops the selection (falling back to the hovered row).
var selRow = -1

// Hover/pressed state for the owner-drawn controls. UI thread only.
var (
	hoverBtn   int  // button id under the cursor, 0 when none
	hoverRow   int  // folder row under the cursor, -1 when none
	hoverX     bool // cursor is over the hovered row's "x" affordance
	pressedBtn int  // button id held down, 0 when none
	pressedX   int  // row index whose "x" is held down, -1 when none
	lastMX     int32
	lastMY     int32
	haveMouse  bool
)

// View state machine: the same window swaps between the main folders view
// and the settings (API keys) view. The app stays keyless-by-default; the
// main view paints and hit-tests exactly as before plus the Settings link.
type uiView int

const (
	viewMain uiView = iota
	viewSettings
)

// curView is the visible view. UI thread only.
var curView uiView = viewMain

// keyField is the owner-drawn text-input editing model (the only interactive
// text in the app), kept as a pure value so tests drive it without a live
// window: printable input appends (replacing a select-all first),
// backspace deletes, paste inserts, select-all arms a full replace.
type keyField struct {
	text   string
	selAll bool
}

// typeRune appends r, replacing the whole buffer first when a select-all is
// armed.
func (f *keyField) typeRune(r rune) {
	if f.selAll {
		f.text, f.selAll = string(r), false
		return
	}
	f.text += string(r)
}

// backspace deletes the last rune, or clears a select-all.
func (f *keyField) backspace() {
	if f.selAll {
		f.text, f.selAll = "", false
		return
	}
	r := []rune(f.text)
	if len(r) == 0 {
		return
	}
	f.text = string(r[:len(r)-1])
}

// paste inserts s at the end (replacing a select-all first). Verbatim: the
// caller already pulled plain clipboard text.
func (f *keyField) paste(s string) {
	if s == "" {
		f.selAll = false
		return
	}
	if f.selAll {
		f.text, f.selAll = s, false
		return
	}
	f.text += s
}

// selectAll arms a full replace: the next printable character or paste
// replaces the buffer. Empty buffer is a no-op (nothing to select).
func (f *keyField) selectAll() {
	f.selAll = len(f.text) > 0
}

// Settings view state. UI thread only, except where noted.
var (
	tmdbField     keyField // TMDB key being edited (real value, never logged)
	tmdbShow      bool     // reveal the real value instead of dots
	fieldFocused  bool     // the key field has logical input focus
	windowFocused = true   // the window has keyboard focus (caret gate)
)

// Caret policy: STEADY (non-blinking). The caret draws at the end of the
// field text whenever the settings view is visible, the field is logically
// focused, and the window has keyboard focus (WM_SETFOCUS/WM_KILLFOCUS
// gate it so the caret stops when unfocused). No timer is involved.

// Scan run state. scanActive is touched only on the UI thread (button
// handler sets it, the done message clears it); scanCancel holds the
// running scan's context cancel (nil when idle or after a cancel request);
// the progress/stats payloads are written by the worker goroutine and read
// in the wndproc under scanMu.
var (
	scanMu       sync.Mutex
	scanActive   bool
	scanCancel   context.CancelFunc
	scanForce    bool
	scanProgress library.Progress
	scanStats    library.Stats
	scanErr      error
)

// Dark palette (logical colors; see the design spec in the brief).
var (
	colBG         = rgb(0x14, 0x14, 0x14)
	colCard       = rgb(0x1C, 0x1C, 0x1C)
	colRow        = rgb(0x24, 0x24, 0x24)
	colRowHover   = rgb(0x2E, 0x2E, 0x2E)
	colBorder     = rgb(0x33, 0x33, 0x33)
	colText       = rgb(0xE8, 0xE8, 0xE8)
	colMuted      = rgb(0x9A, 0x9A, 0x9A)
	colAccent     = rgb(0xE5, 0x09, 0x14)
	colAccentHov  = rgb(0xF6, 0x12, 0x1D)
	colAccentDown = rgb(0xB0, 0x07, 0x10)
	colDanger     = rgb(0xC0, 0x66, 0x66)
)

// Button paint styles.
const (
	btnSubtle  = 0 // #242424 fill on #141414, hover #2E2E2E
	btnAccent  = 1 // accent fill, white text (primary)
	btnOutline = 2 // #333 border, transparent fill
	btnText    = 3 // text-only, muted, hover #E8E8E8
	btnDanger  = 4 // text-only, muted red, hover bright red (destructive)
)

// Settings view: key-page URLs (opened in the default browser) and the
// provider id stored in the config. Only TMDB is active; the rest are
// planned links with no input field.
const (
	tmdbProvider = "tmdb"
	urlTMDB      = "https://www.themoviedb.org/settings/api"
	urlOMDb      = "https://www.omdbapi.com/apikey.aspx"
	urlTVDB      = "https://thetvdb.com/api-information"
	urlTrakt     = "https://trakt.tv/oauth/applications"
)

// Settings-view win32 extras, declared here (not win32.go) to keep this
// change inside window.go: ShellExecute for the get-a-key links, clipboard
// + focus + keyboard-message bindings for the owner-drawn text input, and
// text measurement for the caret.
var (
	procShellExecuteW      = shell32.NewProc("ShellExecuteW")
	procOpenClipboard      = user32.NewProc("OpenClipboard")
	procCloseClipboard     = user32.NewProc("CloseClipboard")
	procGetClipboardData   = user32.NewProc("GetClipboardData")
	procGetKeyState        = user32.NewProc("GetKeyState")
	procSetFocus           = user32.NewProc("SetFocus")
	procGetTextExtentPoint = gdi32.NewProc("GetTextExtentPoint32W")
	procGlobalLock         = kernel32.NewProc("GlobalLock")
	procGlobalUnlock       = kernel32.NewProc("GlobalUnlock")
	procGlobalSize         = kernel32.NewProc("GlobalSize")
	procRtlMoveMemory      = kernel32.NewProc("RtlMoveMemory")
)

// Settings-view message, key, clipboard, and text constants.
const (
	wmChar      = 0x0102
	wmKeyDown   = 0x0100
	wmSetFocus  = 0x0007
	wmKillFocus = 0x0008

	vkBack    = 0x08
	vkEscape  = 0x1B
	vkControl = 0x11

	cfUnicodeText = 13
	swShowNormal  = 1

	dtWordBreak = 0x0010
)

// uiButton is one actionable rect in the paint-time rect table.
type uiButton struct {
	id    int
	label string
	style int
	rc    rect
}

// uiLayout is the full hit-test table for one paint/mouse pass, computed
// purely from client size + DPI + uiDirs (no syscalls), so tests can drive
// it directly.
type uiLayout struct {
	card     rect
	rows     []rect
	xbtns    []rect
	btns     []uiButton
	status   rect
	overflow int
	settings rect // "Settings" text affordance, header row right-aligned
}

// settingsLayout is the hit-test table for the settings view, computed
// purely from client size + DPI (+ whether the key is revealed, which only
// flips the Show/Hide label), so tests can drive it without a window.
type settingsLayout struct {
	field  rect // the owner-drawn TMDB text input (focus/caret target)
	status rect
	btns   []uiButton // Back/Show/GetTMDB/Test/Save + 3 planned links
}

// settingsBtn returns the rect of the settings button id, or false.
func (l settingsLayout) settingsBtn(id int) (rect, bool) {
	for _, b := range l.btns {
		if b.id == id {
			return b.rc, true
		}
	}
	return rect{}, false
}

// createMainWindow registers PotflixMainWnd and creates the fixed-size
// window hidden; there are no child controls - everything is owner-drawn.
// Must be called on the UI thread.
func createMainWindow() error {
	// The folder picker needs COM on this thread; failure is non-fatal (Add
	// then just reports and does nothing).
	procOleInitialize.Call(0)

	hInst, _, _ := procGetModuleHandleW.Call(0)
	className, _ := syscall.UTF16PtrFromString(mainClassName)

	cursor, _, _ := procLoadCursorW.Call(0, idcArrow)
	icon := loadEmbeddedIcon()
	uiIcon = icon
	bg, _, _ := procCreateSolidBrush.Call(colBG)

	wc := wndClassEx{
		cbSize:        uint32(unsafe.Sizeof(wndClassEx{})),
		lpfnWndProc:   syscall.NewCallback(mainWndProc),
		hInstance:     hInst,
		hIcon:         icon,
		hCursor:       cursor,
		hbrBackground: bg,
		lpszClassName: className,
		hIconSm:       icon,
	}
	if atom, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		return callErr
	}

	// Fixed 460x580 logical client; grow to a window rect for the frame.
	dpi := systemDpi()
	cw := scalePx(int32(dpi), mainClientW)
	ch := scalePx(int32(dpi), mainClientH)
	wrc := rect{left: 0, top: 0, right: cw, bottom: ch}
	procAdjustWindowRectEx.Call(uintptr(unsafe.Pointer(&wrc)),
		wsCaption|wsSysMenu|wsMinimizeBox, 0, 0)
	winW := wrc.right - wrc.left
	winH := wrc.bottom - wrc.top

	title, _ := syscall.UTF16PtrFromString("Potflix")
	hwnd, _, callErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		wsCaption|wsSysMenu|wsMinimizeBox,
		cwUseDefault, cwUseDefault, uintptr(winW), uintptr(winH),
		0, 0, hInst, 0,
	)
	if hwnd == 0 {
		return callErr
	}
	mainHwnd = hwnd

	refreshDirList()

	if icon != 0 {
		procSendMessageW.Call(hwnd, wmSetIcon, iconSmall, icon)
		procSendMessageW.Call(hwnd, wmSetIcon, iconBig, icon)
	}
	return nil
}

// showMainWindow shows (or re-shows) the settings window, centering it on
// the primary monitor at first show. A no-op when never created.
func showMainWindow() {
	if mainHwnd == 0 {
		return
	}
	if !uiCentered {
		uiCentered = true
		centerWindow(mainHwnd)
	}
	procShowWindow.Call(mainHwnd, swShow)
	procUpdateWindow.Call(mainHwnd)
	procSetForegroundWindow.Call(mainHwnd)
}

// centerWindow moves hwnd to the primary-monitor center, keeping its size.
func centerWindow(hwnd uintptr) {
	var rc rect
	if r, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rc))); r == 0 {
		return
	}
	winW := rc.right - rc.left
	winH := rc.bottom - rc.top
	sw, _, _ := procGetSystemMetrics.Call(smCxScreen)
	sh, _, _ := procGetSystemMetrics.Call(smCyScreen)
	if sw == 0 || sh == 0 {
		return
	}
	x := (int32(sw) - winW) / 2
	y := (int32(sh) - winH) / 2
	if y < 0 {
		y = 0
	}
	procMoveWindow.Call(hwnd, uintptr(x), uintptr(y), uintptr(winW), uintptr(winH), 1)
}

func mainWndProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	switch msg {
	case wmClose:
		// NEVER DestroyWindow: closing parks the window back to the tray.
		procShowWindow.Call(hwnd, swHide)
		return 0
	case wmEraseBkgnd:
		return 1 // fully painted in WM_PAINT; skip the flickering fill
	case wmPaint:
		paintMainWindow(hwnd)
		return 0
	case wmSize, wmDpiChanged:
		invalidateMain()
		return 0
	case wmMouseMove:
		x, y := mouseXY(lparam)
		lastMX, lastMY, haveMouse = x, y, true
		trackMouseLeave(hwnd)
		if updateHover(hwnd, x, y) {
			invalidateMain()
		}
		return 0
	case wmMouseLeave:
		if hoverBtn != 0 || hoverRow != -1 || hoverX {
			hoverBtn, hoverRow, hoverX = 0, -1, false
			invalidateMain()
		}
		return 0
	case wmLButtonDown:
		x, y := mouseXY(lparam)
		pressAt(hwnd, x, y)
		return 0
	case wmLButtonUp:
		x, y := mouseXY(lparam)
		releaseAt(hwnd, x, y)
		return 0
	case wmSetFocus:
		windowFocused = true
		invalidateMain()
		return 0
	case wmKillFocus:
		windowFocused = false
		invalidateMain()
		return 0
	case wmChar:
		if onSettingsChar(rune(wparam & 0xFFFF)) {
			invalidateMain()
		}
		return 0
	case wmKeyDown:
		if onSettingsKeyDown(hwnd, wparam) {
			invalidateMain()
		}
		return 0
	case wmSetCursor:
		if haveMouse && hitTestCode(hwnd, lastMX, lastMY) != 0 {
			hand, _, _ := procLoadCursorW.Call(0, idcHand)
			if hand != 0 {
				procSetCursor.Call(hand)
				return 1
			}
		}
		r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
		return r
	case wmAppScanProgress:
		if scanActive && scanCancel == nil {
			// Cancel requested: keep the cancelled summary; ignore late
			// progress posts until the worker's done message lands.
			return 0
		}
		scanMu.Lock()
		p := scanProgress
		scanMu.Unlock()
		setStatusText(scanProgressText(p))
		return 0
	case wmAppScanDone:
		onScanDone(hwnd)
		return 0
	case wmAppTMDBTestDone:
		onTMDBTestDone()
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return r
}

// ---------------------------------------------------------------------------
// Hit-testing: pure functions over the rect table (unit-tested).
// ---------------------------------------------------------------------------

// ptInRect reports whether (x,y) lands inside rc (left/top inclusive,
// right/bottom exclusive, the Win32 convention).
func ptInRect(rc rect, x, y int32) bool {
	return x >= rc.left && x < rc.right && y >= rc.top && y < rc.bottom
}

// buttonAtPoint returns the id of the button under (x,y), or 0.
func buttonAtPoint(btns []uiButton, x, y int32) int {
	for _, b := range btns {
		if ptInRect(b.rc, x, y) {
			return b.id
		}
	}
	return 0
}

// rowAtPoint returns the folder row index under (x,y), or -1.
func rowAtPoint(rows []rect, x, y int32) int {
	for i, r := range rows {
		if ptInRect(r, x, y) {
			return i
		}
	}
	return -1
}

// Logical layout metrics (96-DPI px; scaled by the window DPI at use).
const (
	mainClientW = int32(460)
	mainClientH = int32(680)
	mainMargin  = int32(20)
)

// computeLayout builds the hit-test table for the given client size and
// DPI. All metrics scale from the 460x580 logical design.
func computeLayout(cw, ch int32, dpi int) uiLayout {
	sc := func(v int32) int32 { return scalePx(int32(dpi), v) }
	margin := sc(mainMargin)
	cardLeft := margin
	cardW := cw - margin*2
	if cardW < sc(100) {
		cardW = sc(100)
	}

	var l uiLayout

	// Folder card: height follows the row count (padding + one pill per
	// DIR + gaps), clamped to maxRows pills; anything beyond shows as
	// "+N more". Clamp choice: extra DIRs stay configured but unreachable
	// by click (no scroll) - the buttons/status below stay anchored at
	// their fixed positions and the window size never changes.
	const (
		cardTopL   = int32(118)
		maxCardHL  = int32(262) // row budget cap: the region the fixed card used
		padL       = int32(10)
		rowHL      = int32(36)
		gapL       = int32(8)
		overflowHL = int32(22) // "+N more" strip, only when clamped
	)
	cardTop := sc(cardTopL)
	maxCardH := sc(maxCardHL)
	pad, rowH, gap := sc(padL), sc(rowHL), sc(gapL)
	avail := maxCardH - pad*2
	maxRows := int((avail + gap) / (rowH + gap))
	if maxRows < 1 {
		maxRows = 1
	}
	vis := len(uiDirs)
	if vis > maxRows {
		l.overflow = vis - maxRows
		vis = maxRows
	}
	cardH := pad*2 + rowH // empty state: room for the placeholder line
	if vis > 0 {
		cardH = pad*2 + int32(vis)*rowH + int32(vis-1)*gap
		if l.overflow > 0 {
			cardH += sc(overflowHL)
		}
	}
	if cardH > maxCardH {
		cardH = maxCardH
	}
	l.card = rect{left: cardLeft, top: cardTop, right: cardLeft + cardW, bottom: cardTop + cardH}
	rowX := cardLeft + pad
	rowW := cardW - pad*2
	for i := 0; i < vis; i++ {
		y := cardTop + pad + int32(i)*(rowH+gap)
		l.rows = append(l.rows, rect{left: rowX, top: y, right: rowX + rowW, bottom: y + rowH})
		xw := sc(30)
		l.xbtns = append(l.xbtns, rect{left: rowX + rowW - xw, top: y, right: rowX + rowW, bottom: y + rowH})
	}

	// Action buttons: two right-aligned pairs + a text-only Close.
	const (
		btnHL   = int32(36)
		btnGapL = int32(12)
	)
	btnTop := cardTop + maxCardH + sc(16) // anchored: never follows the card
	btnH, btnGap := sc(btnHL), sc(btnGapL)
	btnW := (cardW - btnGap) / 2
	x0, x1 := cardLeft, cardLeft+btnW+btnGap
	y0, y1 := btnTop, btnTop+btnH+btnGap
	l.btns = []uiButton{
		{id: btnAdd, label: "Add folder", style: btnSubtle, rc: rect{left: x0, top: y0, right: x0 + btnW, bottom: y0 + btnH}},
		{id: btnRemove, label: "Remove", style: btnSubtle, rc: rect{left: x1, top: y0, right: x1 + btnW, bottom: y0 + btnH}},
		{id: btnScan, label: "Scan", style: btnAccent, rc: rect{left: x0, top: y1, right: x0 + btnW, bottom: y1 + btnH}},
		{id: btnRefresh, label: "Refresh", style: btnOutline, rc: rect{left: x1, top: y1, right: x1 + btnW, bottom: y1 + btnH}},
	}
	closeW, closeH := sc(120), sc(28)
	l.btns = append(l.btns, uiButton{id: btnClose, label: "Close to tray", style: btnText,
		rc: rect{left: cardLeft + cardW - closeW, top: y1 + btnH + sc(8), right: cardLeft + cardW, bottom: y1 + btnH + sc(8) + closeH}})

	// "Settings" text affordance: header row, right-aligned under the icon,
	// muted text style. Placed in the dead strip between the icon bottom
	// (y=52) and the section label (y=92); every rect above is untouched.
	setW, setH := sc(120), sc(28)
	l.settings = rect{left: cw - margin - setW, top: sc(56), right: cw - margin, bottom: sc(56) + setH}

	// Status line pinned near the bottom.
	stH := sc(20)
	l.status = rect{left: margin, top: ch - sc(14) - stH, right: cw - margin, bottom: ch - sc(14)}
	return l
}

// computeSettingsLayout builds the settings-view hit-test table for the
// given client size, DPI, reveal state (show only flips the Show/Hide
// label), and startup-toggle state (startup only flips its checkbox label).
// Same window, same palette/fonts/DPI as the main view.
func computeSettingsLayout(cw, ch int32, dpi int, show, startup bool) settingsLayout {
	sc := func(v int32) int32 { return scalePx(int32(dpi), v) }
	margin := sc(mainMargin)

	var l settingsLayout

	backW, backH := sc(80), sc(28)
	back := rect{left: margin, top: sc(16), right: margin + backW, bottom: sc(16) + backH}

	showLabel := "Show"
	if show {
		showLabel = "Hide"
	}
	showW := sc(70)
	fieldTop, fieldH := sc(204), sc(36)
	fieldRight := cw - margin - showW - sc(12)
	field := rect{left: margin, top: fieldTop, right: fieldRight, bottom: fieldTop + fieldH}
	showRc := rect{left: fieldRight + sc(12), top: fieldTop, right: cw - margin, bottom: fieldTop + fieldH}

	getTop := fieldTop + fieldH + sc(4)
	getKey := rect{left: margin, top: getTop, right: margin + sc(120), bottom: getTop + sc(24)}
	testW, testH := sc(110), sc(32)
	test := rect{left: cw - margin - testW, top: getTop, right: cw - margin, bottom: getTop + testH}

	linkTop := sc(312)
	linkH := sc(24)
	linkW := sc(100)
	omdb := rect{left: margin, top: linkTop, right: margin + linkW, bottom: linkTop + linkH}
	tvdb := rect{left: margin + linkW + sc(10), top: linkTop, right: margin + linkW*2 + sc(10), bottom: linkTop + linkH}
	trakt := rect{left: margin + linkW*2 + sc(20), top: linkTop, right: margin + linkW*3 + sc(20), bottom: linkTop + linkH}

	// LIBRARY section: label + one row of three (Clear cache / Export... /
	// Import...). STARTUP section: label + toggle + Open-log-folder row.
	// Both sit in the free strip between the planned row (ends 336) and the
	// attribution footer (starts at saveRc.top-48); existing rects above and
	// below are untouched.
	libTop := sc(362)
	libH := sc(32)
	libW := (cw - margin*2 - sc(12)*2) / 3
	clearRc := rect{left: margin, top: libTop, right: margin + libW, bottom: libTop + libH}
	exportRc := rect{left: margin + libW + sc(12), top: libTop, right: margin + libW*2 + sc(12), bottom: libTop + libH}
	importRc := rect{left: margin + libW*2 + sc(24), top: libTop, right: cw - margin, bottom: libTop + libH}

	startTop := sc(420)
	startH := sc(32)
	toggleW := sc(230)
	toggleRc := rect{left: margin, top: startTop, right: margin + toggleW, bottom: startTop + startH}
	logRc := rect{left: margin + toggleW + sc(12), top: startTop, right: cw - margin, bottom: startTop + startH}

	// SCAN section: label + three-way segmented control (Gentle / Balanced
	// / Turbo) + muted hint. It sits in the gap the taller window opens
	// between the STARTUP row (ends 452) and the Save-anchored attribution
	// (starts at save.top-48, i.e. 554 at 96 DPI with the 680 client);
	// existing rects above and below are untouched.
	scanTop := sc(482)
	scanH := sc(32)
	scanW := (cw - margin*2 - sc(12)*2) / 3
	curScan := NormalizeScanSpeed(scanSpeedState)
	gentleRc := rect{left: margin, top: scanTop, right: margin + scanW, bottom: scanTop + scanH}
	balancedRc := rect{left: margin + scanW + sc(12), top: scanTop, right: margin + scanW*2 + sc(12), bottom: scanTop + scanH}
	turboRc := rect{left: margin + scanW*2 + sc(24), top: scanTop, right: cw - margin, bottom: scanTop + scanH}

	saveW, saveH := sc(130), sc(36)
	stH := sc(20)
	status := rect{left: margin, top: ch - sc(14) - stH, right: cw - margin, bottom: ch - sc(14)}
	save := rect{left: cw - margin - saveW, top: status.top - sc(8) - saveH, right: cw - margin, bottom: status.top - sc(8)}

	l.field = field
	l.status = status
	l.btns = []uiButton{
		{id: btnBack, label: "Back", style: btnText, rc: back},
		{id: btnShow, label: showLabel, style: btnOutline, rc: showRc},
		{id: btnGetTMDB, label: "Get a key", style: btnText, rc: getKey},
		{id: btnTest, label: "Test", style: btnSubtle, rc: test},
		{id: btnGetOMDb, label: "OMDb key", style: btnText, rc: omdb},
		{id: btnGetTVDB, label: "TVDB key", style: btnText, rc: tvdb},
		{id: btnGetTrakt, label: "Trakt key", style: btnText, rc: trakt},
		{id: btnSave, label: "Save", style: btnAccent, rc: save},
		{id: btnClearCache, label: "Clear cache", style: btnDanger, rc: clearRc},
		{id: btnExport, label: "Export...", style: btnSubtle, rc: exportRc},
		{id: btnImport, label: "Import...", style: btnSubtle, rc: importRc},
		{id: btnStartup, label: startupToggleLabel(startup), style: btnSubtle, rc: toggleRc},
		{id: btnOpenLog, label: "Open log folder", style: btnOutline, rc: logRc},
		{id: btnScanGentle, label: "Gentle", style: scanSegmentStyle(ScanSpeedGentle, curScan), rc: gentleRc},
		{id: btnScanBalanced, label: "Balanced", style: scanSegmentStyle(ScanSpeedBalanced, curScan), rc: balancedRc},
		{id: btnScanTurbo, label: "Turbo", style: scanSegmentStyle(ScanSpeedTurbo, curScan), rc: turboRc},
	}
	return l
}

// layoutForHwnd snapshots the current client rect + DPI and builds the
// hit-test table; ok=false when the window has no size yet.
func layoutForHwnd(hwnd uintptr) (uiLayout, bool) {
	var rc rect
	procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	cw := rc.right - rc.left
	ch := rc.bottom - rc.top
	if cw <= 0 || ch <= 0 {
		return uiLayout{}, false
	}
	return computeLayout(cw, ch, windowDpi(hwnd)), true
}

// layoutForSettings snapshots the current client rect + DPI and builds the
// settings hit-test table; ok=false when the window has no size yet.
func layoutForSettings(hwnd uintptr) (settingsLayout, bool) {
	var rc rect
	procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	cw := rc.right - rc.left
	ch := rc.bottom - rc.top
	if cw <= 0 || ch <= 0 {
		return settingsLayout{}, false
	}
	return computeSettingsLayout(cw, ch, windowDpi(hwnd), tmdbShow, startupState), true
}

// updateHover refreshes the hover state from (x,y); reports whether a
// repaint is needed.
func updateHover(hwnd uintptr, x, y int32) bool {
	if curView == viewSettings {
		l, ok := layoutForSettings(hwnd)
		if !ok {
			return false
		}
		nb := buttonAtPoint(l.btns, x, y)
		if nb == hoverBtn && hoverRow == -1 && !hoverX {
			return false
		}
		hoverBtn, hoverRow, hoverX = nb, -1, false
		return true
	}
	l, ok := layoutForHwnd(hwnd)
	if !ok {
		return false
	}
	nb := buttonAtPoint(l.btns, x, y)
	if ptInRect(l.settings, x, y) {
		nb = btnSettings
	}
	nr := rowAtPoint(l.rows, x, y)
	nx := false
	if nr >= 0 && nr < len(l.xbtns) && ptInRect(l.xbtns[nr], x, y) {
		nx = true
	}
	if nb == hoverBtn && nr == hoverRow && nx == hoverX {
		return false
	}
	hoverBtn, hoverRow, hoverX = nb, nr, nx
	return true
}

// pressAt records the pressed control for the down-stroke visual.
func pressAt(hwnd uintptr, x, y int32) {
	if curView == viewSettings {
		l, ok := layoutForSettings(hwnd)
		if !ok {
			return
		}
		pressedBtn, pressedX = 0, -1
		if id := buttonAtPoint(l.btns, x, y); id != 0 {
			pressedBtn = id
		} else if ptInRect(l.field, x, y) {
			focusSettingsField(hwnd)
		} else {
			fieldFocused = false
		}
		invalidateMain()
		return
	}
	l, ok := layoutForHwnd(hwnd)
	if !ok {
		return
	}
	pressedBtn, pressedX = 0, -1
	if id := buttonAtPoint(l.btns, x, y); id != 0 {
		pressedBtn = id
	} else if ptInRect(l.settings, x, y) {
		pressedBtn = btnSettings
	} else if r := rowAtPoint(l.rows, x, y); r >= 0 && r < len(l.xbtns) {
		if ptInRect(l.xbtns[r], x, y) {
			pressedX = r
		}
	}
	invalidateMain()
}

// releaseAt dispatches the click when the up-stroke lands on the pressed
// control (or selects a folder row).
func releaseAt(hwnd uintptr, x, y int32) {
	if curView == viewSettings {
		l, ok := layoutForSettings(hwnd)
		if !ok {
			pressedBtn, pressedX = 0, -1
			return
		}
		pb := pressedBtn
		pressedBtn, pressedX = 0, -1
		if pb != 0 {
			if buttonAtPoint(l.btns, x, y) == pb {
				doSettingsAction(hwnd, pb)
			} else {
				invalidateMain()
			}
			return
		}
		if ptInRect(l.field, x, y) {
			focusSettingsField(hwnd)
			return
		}
		fieldFocused = false
		invalidateMain()
		return
	}
	l, ok := layoutForHwnd(hwnd)
	if !ok {
		pressedBtn, pressedX = 0, -1
		return
	}
	pb, px := pressedBtn, pressedX
	pressedBtn, pressedX = 0, -1
	if pb != 0 {
		if pb == btnSettings {
			if ptInRect(l.settings, x, y) {
				enterSettings()
			} else {
				invalidateMain()
			}
			return
		}
		if buttonAtPoint(l.btns, x, y) == pb {
			doButtonAction(hwnd, pb)
		} else {
			invalidateMain()
		}
		return
	}
	if px >= 0 {
		if r := rowAtPoint(l.rows, x, y); r == px && r < len(l.xbtns) && ptInRect(l.xbtns[r], x, y) {
			removeDirAt(r)
		} else {
			invalidateMain()
		}
		return
	}
	if r := rowAtPoint(l.rows, x, y); r >= 0 {
		if selRow != r {
			selRow = r
			invalidateMain()
		}
		return
	}
	if id := buttonAtPoint(l.btns, x, y); id != 0 {
		doButtonAction(hwnd, id)
	}
}

// hitTestCode returns nonzero when (x,y) sits over an actionable rect, so
// WM_SETCURSOR can show the hand cursor there.
func hitTestCode(hwnd uintptr, x, y int32) uintptr {
	if curView == viewSettings {
		l, ok := layoutForSettings(hwnd)
		if !ok {
			return 0
		}
		if buttonAtPoint(l.btns, x, y) != 0 {
			return 1
		}
		if ptInRect(l.field, x, y) {
			return 1
		}
		return 0
	}
	l, ok := layoutForHwnd(hwnd)
	if !ok {
		return 0
	}
	if buttonAtPoint(l.btns, x, y) != 0 {
		return 1
	}
	if ptInRect(l.settings, x, y) {
		return 1
	}
	if r := rowAtPoint(l.rows, x, y); r >= 0 {
		return 1
	}
	return 0
}

// doButtonAction routes an owner-drawn button click into the SAME actions
// the native buttons used to call.
func doButtonAction(hwnd uintptr, id int) {
	switch id {
	case btnAdd:
		onAddDir(hwnd)
	case btnRemove:
		onRemoveSelected()
	case btnScan:
		if scanActive {
			cancelLibraryScan()
		} else {
			startLibraryScan(hwnd, false)
		}
	case btnRefresh:
		startLibraryScan(hwnd, true)
	case btnClose:
		procShowWindow.Call(hwnd, swHide)
	}
}

// ---------------------------------------------------------------------------
// Settings view: view switching, text input, and actions.
// ---------------------------------------------------------------------------

// openSettings is the pure view-switch core (no syscalls, no disk): it arms
// the settings view with initial as the TMDB field value, masked. Tests
// drive this directly; enterSettings is the disk-backed wrapper.
func openSettings(initial string) {
	curView = viewSettings
	tmdbField = keyField{text: initial}
	tmdbShow = false
	fieldFocused = false
	hoverBtn, pressedBtn = 0, 0
	hoverRow, hoverX, pressedX = -1, false, -1
	invalidateMain()
}

// closeSettings is the pure return-to-main core: the field loses focus and
// the main view paints again.
func closeSettings() {
	curView = viewMain
	fieldFocused = false
	hoverBtn, pressedBtn = 0, 0
	invalidateMain()
}

// enterSettings loads the stored TMDB key (value never logged), refreshes
// the ACTUAL startup state from the registry (never the stored flag alone),
// refreshes the SCAN speed selection from the config store, and swaps to
// the settings view.
func enterSettings() {
	startupState = queryStartupEnabled()
	scanSpeedState = loadConfig().EffectiveScanSpeed()
	openSettings(loadConfig().GetAPIKey(tmdbProvider))
}

// enterMain returns to the folders view.
func enterMain() {
	closeSettings()
}

// focusSettingsField gives the key field logical input focus and ensures the
// window holds keyboard focus so WM_CHAR arrives. A fresh click clears a
// select-all arm.
func focusSettingsField(hwnd uintptr) {
	fieldFocused = true
	tmdbField.selAll = false
	if hwnd != 0 {
		procSetFocus.Call(hwnd)
	}
	invalidateMain()
}

// caretVisible reports whether the steady caret draws: settings view,
// logically focused field, window holding keyboard focus.
func caretVisible() bool {
	return curView == viewSettings && fieldFocused && windowFocused
}

// onSettingsChar handles a WM_CHAR code unit for the key field: printable
// characters append (replacing a select-all first), VK_BACK deletes.
// Reports whether the keystroke landed in the field. Control characters
// (Ctrl+A/V arrive here as 0x01/0x16) are ignored - WM_KEYDOWN owns them -
// so a paste or select-all never double-applies.
func onSettingsChar(r rune) bool {
	if curView != viewSettings || !fieldFocused || !windowFocused {
		return false
	}
	switch {
	case r == vkBack:
		tmdbField.backspace()
		return true
	case r >= 32 && r != 127:
		tmdbField.typeRune(r)
		return true
	}
	return false
}

// onSettingsKeyDown handles WM_KEYDOWN for the settings view: Esc returns to
// the main view, Ctrl+A selects all, Ctrl+V pastes clipboard text.
// Reports whether a repaint is needed.
func onSettingsKeyDown(hwnd uintptr, vk uintptr) bool {
	if curView != viewSettings {
		return false
	}
	if vk == vkEscape {
		enterMain()
		return true
	}
	if curView == viewSettings && fieldFocused && windowFocused {
		ctrl, _, _ := procGetKeyState.Call(vkControl)
		if ctrl&0x8000 != 0 {
			switch vk {
			case 'A':
				tmdbField.selectAll()
				return true
			case 'V':
				if s := getClipboardText(); s != "" {
					tmdbField.paste(s)
				} else {
					tmdbField.selAll = false
				}
				return true
			}
		}
	}
	_ = hwnd
	return false
}

// doSettingsAction routes a settings-view button click.
func doSettingsAction(hwnd uintptr, id int) {
	switch id {
	case btnBack:
		enterMain()
	case btnSave:
		onSaveSettings()
	case btnShow:
		tmdbShow = !tmdbShow
		invalidateMain()
	case btnGetTMDB:
		openKeyPage(urlTMDB)
	case btnGetOMDb:
		openKeyPage(urlOMDb)
	case btnGetTVDB:
		openKeyPage(urlTVDB)
	case btnGetTrakt:
		openKeyPage(urlTrakt)
	case btnTest:
		startTMDBTest(hwnd, tmdbField.text)
	case btnScanGentle, btnScanBalanced, btnScanTurbo:
		if profile := scanProfileForButton(id); profile != "" {
			onSelectScanSpeed(profile)
		}
	case btnClearCache:
		onClearCache(hwnd)
	case btnExport:
		onExportBundle(hwnd)
	case btnImport:
		onImportBundle(hwnd)
	case btnStartup:
		onToggleStartup()
	case btnOpenLog:
		onOpenLogFolder()
	}
}

// onSaveSettings persists the field through SetAPIKey (empty clears the
// entry), re-masks the field, and reports Saved / Key cleared. Only
// presence is logged - the value never reaches the log.
func onSaveSettings() {
	value := tmdbField.text
	cfg := loadConfig()
	cfg.SetAPIKey(tmdbProvider, value)
	if err := saveConfig(cfg); err != nil {
		appLogf("settings: save failed: %s", err)
		setStatusText("Save failed.")
		return
	}
	liveConfig.Store(&cfg) // the key takes effect without a restart
	tmdbShow = false       // re-mask after save
	if value == "" {
		setStatusText("Key cleared")
		appLogf("settings: tmdb key cleared")
	} else {
		setStatusText("Saved")
		appLogf("settings: tmdb key saved (present=true)")
	}
}

// openKeyPage opens url in the default browser via ShellExecute.
func openKeyPage(url string) {
	op, _ := syscall.UTF16PtrFromString("open")
	u, _ := syscall.UTF16PtrFromString(url)
	procShellExecuteW.Call(0, uintptr(unsafe.Pointer(op)), uintptr(unsafe.Pointer(u)), 0, 0, swShowNormal)
	appLogf("settings: opened key page")
}

// getClipboardText pulls CF_UNICODETEXT from the clipboard (up to 512
// runes), or "" when unavailable. Plain helper so Ctrl+V never touches the
// debug log with key material.
func getClipboardText() string {
	if r, _, _ := procOpenClipboard.Call(0); r == 0 {
		return ""
	}
	defer procCloseClipboard.Call()
	h, _, _ := procGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return ""
	}
	p, _, _ := procGlobalLock.Call(h)
	if p == 0 {
		return ""
	}
	defer procGlobalUnlock.Call(h)
	// Bound the read by the real block size: never scan past the HGLOBAL.
	n, _, _ := procGlobalSize.Call(h)
	count := int(n) / 2
	if count > 512 {
		count = 512
	}
	if count <= 0 {
		return ""
	}
	// Copy out with RtlMoveMemory, staying in uintptr-land: the destination
	// is a real Go pointer (Pointer->uintptr is vet-clean) and the locked
	// source address is already a uintptr, so no uintptr->Pointer
	// conversion trips unsafeptr. The lock holds the block stable.
	buf := make([]uint16, count)
	procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&buf[0])), p, uintptr(count*2))
	u16 := make([]uint16, 0, 64)
	for _, v := range buf {
		if v == 0 {
			break
		}
		u16 = append(u16, v)
	}
	return syscall.UTF16ToString(u16)
}

// trackMouseLeave arms the TME_LEAVE notification so hover clears when the
// cursor exits the window.
func trackMouseLeave(hwnd uintptr) {
	tm := trackMouseEvent{
		cbSize:      uint32(unsafe.Sizeof(trackMouseEvent{})),
		dwFlags:     tmeLeave,
		hwndTrack:   hwnd,
		dwHoverTime: 0,
	}
	procTrackMouseEvent.Call(uintptr(unsafe.Pointer(&tm)))
}

func invalidateMain() {
	if mainHwnd != 0 {
		procInvalidateRect.Call(mainHwnd, 0, 0)
	}
}

// ---------------------------------------------------------------------------
// Config wiring: identical behavior to the native list box, new presentation.
// ---------------------------------------------------------------------------

// onAddDir picks a folder via the native browser, then routes it through the
// config helpers (load -> AddDir -> save) and repaints the row list.
func onAddDir(owner uintptr) {
	path := browseForFolder(owner)
	if path == "" {
		return // cancelled or unavailable
	}
	cfg := loadConfig()
	if !cfg.AddDir(path) {
		appLogf("config: dir already watched (count=%d)", len(cfg.Dirs))
		return
	}
	if err := saveConfig(cfg); err != nil {
		appLogf("config: save failed: %s", err)
		return
	}
	refreshDirList()
	appLogf("config: dir added (count=%d)", len(cfg.Dirs))
}

// onRemoveSelected drops the selected row (falling back to the hovered one)
// through the config helpers.
func onRemoveSelected() {
	idx := selRow
	if idx < 0 {
		idx = hoverRow
	}
	removeDirAt(idx)
}

// removeDirAt drops uiDirs[idx] through the config helpers; out-of-range is
// a no-op.
func removeDirAt(idx int) {
	if idx < 0 || idx >= len(uiDirs) {
		return
	}
	cfg := loadConfig()
	if !cfg.RemoveDir(uiDirs[idx]) {
		return
	}
	if err := saveConfig(cfg); err != nil {
		appLogf("config: save failed: %s", err)
		return
	}
	refreshDirList()
	appLogf("config: dir removed (count=%d)", len(cfg.Dirs))
}

// maxScanStatusLen caps the scan status strings. The painter already
// ellipsizes the status line (dtEndEllipsis), so the window never overflows;
// the cap keeps the string itself sane even for huge counts (folder suffix
// is always preserved by fitScanStatus).
const maxScanStatusLen = 160

// formatScanCount renders n compactly: exact below 100k, k/M/B/T/P/E above,
// so a huge count elides (e.g. 12345678 -> "12.3M") rather than widening the
// status line unboundedly. Rounding that would hit 1000 promotes to the next
// unit (999999 -> "1M", never "1000k").
func formatScanCount(n int) string {
	if n < 100000 && n > -100000 {
		return fmt.Sprintf("%d", n)
	}
	f := float64(n)
	divs := []float64{1e3, 1e6, 1e9, 1e12, 1e15, 1e18}
	sufs := []string{"k", "M", "B", "T", "P", "E"}
	idx := 0
	for idx < len(divs)-1 && f/divs[idx] >= 999.95 {
		idx++
	}
	s := fmt.Sprintf("%.1f%s", f/divs[idx], sufs[idx])
	s = strings.Replace(s, ".0"+sufs[idx], sufs[idx], 1)
	return s
}

// fitScanStatus caps s to maxScanStatusLen, preserving the "folders:" suffix
// so the folder counts are never truncated away. Typical compact-count
// strings never reach the cap; it only bites for pathological inputs.
func fitScanStatus(s string) string {
	if len(s) <= maxScanStatusLen {
		return s
	}
	if i := strings.Index(s, "folders:"); i >= 0 {
		tail := s[i:]
		keep := maxScanStatusLen - len(tail) - len("...")
		if keep > 20 {
			return s[:keep] + "..." + tail
		}
	}
	return s[:maxScanStatusLen-len("...")] + "..."
}

// scanProgressText maps a library.Progress to the live status line. Pure so
// tests drive it without a live window.
func scanProgressText(p library.Progress) string {
	return fitScanStatus(fmt.Sprintf("Scanning %s/%s keys | %s fetched - %s cached | folders: %s scanned, %s skipped",
		formatScanCount(p.KeysDone), formatScanCount(p.KeysTotal),
		formatScanCount(p.KeysFetched), formatScanCount(p.KeysCached),
		formatScanCount(p.FoldersScanned), formatScanCount(p.FoldersSkipped)))
}

// scanDoneText maps kind ("Scan"/"Refresh") + stats to the final summary.
// Pure so tests drive it without a live window.
func scanDoneText(kind string, s library.Stats) string {
	if s.KeysFetched == 0 && s.KeysNotFound == 0 && s.FoldersSkipped > 0 {
		return fitScanStatus(fmt.Sprintf("%s complete: nothing to fetch (%s cached) - %s folders skipped",
			kind, formatScanCount(s.KeysCached), formatScanCount(s.FoldersSkipped)))
	}
	return fitScanStatus(fmt.Sprintf("%s complete: %s keys - %s cached - %s fetched - %s not found | %s files - folders: %s scanned, %s skipped",
		kind, formatScanCount(s.Keys), formatScanCount(s.KeysCached), formatScanCount(s.KeysFetched),
		formatScanCount(s.KeysNotFound), formatScanCount(s.Files),
		formatScanCount(s.FoldersScanned), formatScanCount(s.FoldersSkipped)))
}

// scanCancelledText renders the user-cancel summary: done/total unique-key
// progress kept (compact counts), capped like every other status line.
// Pure so tests drive it without a live window.
func scanCancelledText(done, total int) string {
	return fitScanStatus(fmt.Sprintf("Scan cancelled - %s/%s keys done, progress kept",
		formatScanCount(done), formatScanCount(total)))
}

// scanButtonLabel maps running state to the Scan control caption: while a
// scan is running the same rect becomes Cancel. Pure so tests drive the
// control-state model without a live window.
func scanButtonLabel(active bool) string {
	if active {
		return "Cancel"
	}
	return "Scan"
}

// scanRefreshEnabled reports whether Refresh is clickable: it stays
// disabled while a run is in flight (the Scan rect stays enabled as
// Cancel). Pure so tests drive the control-state model.
func scanRefreshEnabled(active bool) bool {
	return !active
}

// scanResultText maps a finished run to its status line. A Cancelled result
// is never a failure: it lands on the cancelled message (stats numbers),
// even if a context error also arrived. Pure so tests drive it.
func scanResultText(kind string, s library.Stats, err error) string {
	if s.Cancelled {
		return scanCancelledText(s.KeysDone, s.Keys)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return scanCancelledText(s.KeysDone, s.Keys)
		}
		return kind + " failed: " + err.Error()
	}
	return scanDoneText(kind, s)
}

// buildScanOptions is the indexed scan configuration for the Scan/Refresh
// buttons (and the post-import auto-scan, which calls startLibraryScan with
// force=false): the configured DIRs, the live metadata cache dir, the scan
// index file, UseIndex on, and Force per button (Scan=false, Refresh=true).
// Pure so tests drive it without a live window.
func buildScanOptions(dirs []string, force bool, progress func(library.Progress)) library.Options {
	return library.Options{
		Dirs:      dirs,
		Force:     force,
		CacheDir:  MetadataDir(),
		IndexPath: ScanIndexPath(),
		UseIndex:  true,
		Progress:  progress,
	}
}

// startLibraryScan launches a Scan (force=false) or Refresh (force=true) on
// a worker goroutine. It runs on the UI thread: snapshots the DIRs from the
// loaded config and returns at once - the wndproc never blocks and never
// touches PotPlayer or pathquery here.
//
// Index semantics (library.ScanWithOptions): force=false consults the folder
// index with the two-tier skip (unchanged subtrees are skipped via
// FoldersSkipped/FilesSkipped); force=true ignores the index for skipping -
// it walks everything and re-fetches every cached entry - but still rewrites
// every visited index record.
func startLibraryScan(hwnd uintptr, force bool) {
	if scanActive {
		return
	}
	dirs := loadConfig().Dirs
	kind, lname := "Scan", "scan"
	if force {
		kind, lname = "Refresh", "refresh"
	}
	if len(dirs) == 0 {
		setStatusText("No folders configured - Add folder first.")
		appLogf("%s: no dirs configured", lname)
		return
	}
	scanActive = true
	ctx, cancel := context.WithCancel(context.Background())
	scanCancel = cancel
	scanMu.Lock()
	scanForce = force
	scanMu.Unlock()
	enableScanButtons(hwnd, false)
	setStatusText(kind + " starting...")
	appLogf("%s: started (dirs=%d force=%t)", lname, len(dirs), force)
	go func() {
		stats, err := library.ScanWithOptions(ctx, buildScanOptions(dirs, force, func(p library.Progress) {
			scanMu.Lock()
			scanProgress = p
			scanMu.Unlock()
			// Async post only: the worker never touches a control directly.
			procPostMessageW.Call(hwnd, wmAppScanProgress, 0, 0)
		}))
		scanMu.Lock()
		scanStats = stats
		scanErr = err
		scanMu.Unlock()
		procPostMessageW.Call(hwnd, wmAppScanDone, 0, 0)
	}()
}

// cancelLibraryScan requests cancellation of the running scan. UI thread
// only: it cancels the scan context and shows the cancelled summary from
// the last progress snapshot (progress kept). The worker stays in flight
// until it posts wmAppScanDone, which lands on the same cancelled message
// with the final partial stats; Refresh stays disabled until then.
func cancelLibraryScan() {
	if !scanActive || scanCancel == nil {
		return
	}
	scanCancel()
	scanCancel = nil
	scanMu.Lock()
	p := scanProgress
	scanMu.Unlock()
	setStatusText(scanCancelledText(p.KeysDone, p.KeysTotal))
	appLogf("scan: cancel requested (%d/%d keys done)", p.KeysDone, p.KeysTotal)
}

// onScanDone lands the worker's result on the UI thread: re-enable the
// buttons and show the final summary in the status line. A Cancelled
// result is never a failure: it lands on the cancelled message.
func onScanDone(hwnd uintptr) {
	scanMu.Lock()
	stats, err, force := scanStats, scanErr, scanForce
	scanMu.Unlock()
	scanActive = false
	if scanCancel != nil {
		scanCancel()
		scanCancel = nil
	}
	enableScanButtons(hwnd, true)
	kind, lname := "Scan", "scan"
	if force {
		kind, lname = "Refresh", "refresh"
	}
	if stats.Cancelled || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		setStatusText(scanCancelledText(stats.KeysDone, stats.Keys))
		appLogf("%s: cancelled (%d/%d keys done, progress kept)", lname, stats.KeysDone, stats.Keys)
		return
	}
	if err != nil {
		setStatusText(kind + " failed: " + err.Error())
		appLogf("%s: failed: %s", lname, err)
		return
	}
	setStatusText(scanDoneText(kind, stats))
	appLogf("%s: complete files=%d skipped=%d unique=%d resolved=%d cached=%d fetched=%d not-found=%d errors=%d denied=%d foldersScanned=%d foldersSkipped=%d filesSkipped=%d",
		lname, stats.Files, stats.Skipped, stats.Unique, stats.Resolved, stats.Cached, stats.Fetched, stats.NotFound, stats.Errors, stats.Denied, stats.FoldersScanned, stats.FoldersSkipped, stats.FilesSkipped)
}

// enableScanButtons repaints so Scan/Refresh flip between active and dimmed;
// always called on the UI thread via the button handler or the done message.
func enableScanButtons(_ uintptr, _ bool) {
	invalidateMain()
}

// setStatusText rewrites the status line and repaints; always called on the
// UI thread.
func setStatusText(s string) {
	statusText = s
	invalidateMain()
}

// refreshDirList rebuilds the painted row list from the config store.
func refreshDirList() {
	uiDirs = loadConfig().Dirs
	if selRow >= len(uiDirs) {
		selRow = len(uiDirs) - 1
	}
	if len(uiDirs) == 0 {
		selRow = -1
	}
	if hoverRow >= len(uiDirs) {
		hoverRow, hoverX = -1, false
	}
	invalidateMain()
}

// browseForFolder opens the native folder picker and returns the chosen
// path, or "" on cancel/failure. It never touches the config itself.
func browseForFolder(owner uintptr) string {
	var display [260]uint16
	title, _ := syscall.UTF16PtrFromString("Choose a media folder")
	bi := browseInfoW{
		hwndOwner:   owner,
		displayName: &display[0],
		title:       title,
		flags:       bifReturnOnlyFSDirs | bifNewDialogStyle,
	}
	pidl, _, _ := procSHBrowseForFolderW.Call(uintptr(unsafe.Pointer(&bi)))
	if pidl == 0 {
		return ""
	}
	defer procCoTaskMemFree.Call(pidl)
	var path [260]uint16
	r, _, _ := procSHGetPathFromIDListW.Call(pidl, uintptr(unsafe.Pointer(&path[0])))
	if r == 0 {
		return ""
	}
	return syscall.UTF16ToString(path[:])
}

// ---------------------------------------------------------------------------
// Painting: double-buffered owner-drawn dark UI.
// ---------------------------------------------------------------------------

func paintMainWindow(hwnd uintptr) {
	var ps paintStruct
	hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	if hdc == 0 {
		return
	}
	var rc rect
	procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	w := rc.right - rc.left
	h := rc.bottom - rc.top
	dpi := windowDpi(hwnd)

	mem, _, _ := procCreateCompatibleDC.Call(hdc)
	bmp, _, _ := procCreateCompatibleBitmap.Call(hdc, uintptr(w), uintptr(h))
	if mem != 0 && bmp != 0 {
		oldBmp, _, _ := procSelectObject.Call(mem, bmp)
		paintMainCard(mem, rc, dpi)
		procBitBlt.Call(hdc, 0, 0, uintptr(w), uintptr(h), mem, 0, 0, srccopy)
		procSelectObject.Call(mem, oldBmp)
		procDeleteObject.Call(bmp)
		procDeleteDC.Call(mem)
	} else {
		// Paranoid fallback: paint direct rather than leave garbage.
		if mem != 0 {
			procDeleteDC.Call(mem)
		}
		if bmp != 0 {
			procDeleteObject.Call(bmp)
		}
		paintMainCard(hdc, rc, dpi)
	}
	procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
}

// paintMainCard draws the full dark UI: header wordmark + subtitle + icon,
// the MEDIA FOLDERS section, the folder card with one pill per DIR, the
// action button pairs, and the status line.
func paintMainCard(hdc uintptr, rc rect, dpi int) {
	if curView == viewSettings {
		paintSettingsCard(hdc, rc, dpi)
		return
	}
	cw := rc.right - rc.left
	ch := rc.bottom - rc.top
	sc := func(v int32) int32 { return scalePx(int32(dpi), v) }

	// Base fill.
	bgBrush, _, _ := procCreateSolidBrush.Call(colBG)
	var fill rect
	fill = rect{left: 0, top: 0, right: cw, bottom: ch}
	procFillRect.Call(hdc, uintptr(unsafe.Pointer(&fill)), bgBrush)
	procDeleteObject.Call(bgBrush)

	procSetBkMode.Call(hdc, transparent)

	titleFont := createUIFont(-sc(22), fwSemiBold)
	sectionFont := createUIFont(-sc(12), fwNormal)
	bodyFont := createUIFont(-sc(14), fwNormal)
	buttonFont := createUIFont(-sc(14), fwBold)
	defer procDeleteObject.Call(titleFont)
	defer procDeleteObject.Call(sectionFont)
	defer procDeleteObject.Call(bodyFont)
	defer procDeleteObject.Call(buttonFont)

	// 1. Header: red wordmark + muted subtitle + 32px icon at the right.
	drawUIText(hdc, "POTFLIX",
		rect{left: sc(20), top: sc(16), right: cw - sc(72), bottom: sc(48)},
		dtLeft|dtSingleLine|dtNoPrefix|dtVCenter, titleFont, colAccent)
	drawUIText(hdc, "Discord Rich Presence for PotPlayer",
		rect{left: sc(20), top: sc(48), right: cw - sc(72), bottom: sc(68)},
		dtLeft|dtSingleLine|dtNoPrefix|dtVCenter, sectionFont, colMuted)
	if uiIcon != 0 {
		sz := sc(32)
		procDrawIconEx.Call(hdc, uintptr(cw-sc(20)-sz), uintptr(sc(20)),
			uiIcon, uintptr(sz), uintptr(sz), 0, 0, diNormal)
	}

	// 2. Section label + count.
	drawUIText(hdc, "MEDIA FOLDERS",
		rect{left: sc(20), top: sc(92), right: cw - sc(60), bottom: sc(110)},
		dtLeft|dtSingleLine|dtNoPrefix|dtVCenter, sectionFont, colMuted)
	drawUIText(hdc, fmt.Sprintf("%d", len(uiDirs)),
		rect{left: cw - sc(60), top: sc(92), right: cw - sc(20), bottom: sc(110)},
		dtRight|dtSingleLine|dtNoPrefix|dtVCenter, sectionFont, colMuted)

	l := computeLayout(cw, ch, dpi)

	// 3. Folder card + one pill per DIR.
	fillRoundRect(hdc, l.card, colCard, sc(12))
	if len(l.rows) == 0 {
		drawUIText(hdc, "No folders yet - add one below",
			rect{left: l.card.left, top: l.card.top, right: l.card.right, bottom: l.card.bottom},
			dtCenter|dtSingleLine|dtNoPrefix|dtVCenter, bodyFont, colMuted)
	} else {
		for i, r := range l.rows {
			fill := colRow
			if i == hoverRow {
				fill = colRowHover
			}
			if i == selRow {
				fill = colRowHover
			}
			fillRoundRect(hdc, r, fill, sc(8))
			if i == selRow {
				strokeRoundRect(hdc, r, colBorder, sc(8))
			}
			textRight := r.right - sc(12)
			if i == hoverRow && i < len(l.xbtns) {
				drawUIText(hdc, "x", l.xbtns[i],
					dtCenter|dtSingleLine|dtNoPrefix|dtVCenter, bodyFont, colMuted)
				textRight = l.xbtns[i].left - sc(4)
			}
			drawUIText(hdc, uiDirs[i],
				rect{left: r.left + sc(12), top: r.top, right: textRight, bottom: r.bottom},
				dtLeft|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, bodyFont, colText)
		}
		if l.overflow > 0 {
			drawUIText(hdc, fmt.Sprintf("+%d more", l.overflow),
				rect{left: l.card.left, top: l.card.bottom - sc(22), right: l.card.right - sc(12), bottom: l.card.bottom - sc(6)},
				dtRight|dtSingleLine|dtNoPrefix|dtVCenter, sectionFont, colMuted)
		}
	}

	// 4. Action buttons. While a scan runs the Scan rect becomes Cancel
	// (same rect, same owner-drawn fill); Refresh stays disabled.
	for _, b := range l.btns {
		if b.id == btnScan && scanActive {
			b.label = scanButtonLabel(true)
		}
		paintButton(hdc, b, dpi, buttonFont, sectionFont)
	}

	// 4b. Settings affordance: muted text in the header row, right-aligned
	// under the icon. Brightens on hover/press like any btnText control.
	{
		tc := colMuted
		if hoverBtn == btnSettings || pressedBtn == btnSettings {
			tc = colText
		}
		drawUIText(hdc, "Settings", l.settings,
			dtRight|dtSingleLine|dtNoPrefix|dtVCenter, sectionFont, tc)
	}

	// 5. Status line.
	drawUIText(hdc, statusText, l.status,
		dtLeft|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, sectionFont, colMuted)
}

// paintSettingsCard draws the settings view in the same window: Back +
// title, the API KEYS section with the active TMDB row (owner-drawn masked
// input, Show/Hide, Get-a-key link, Test), the planned-sources row, the
// TMDB attribution footer, Save, and the shared status line. Same palette,
// fonts, and DPI scaling as the main view.
func paintSettingsCard(hdc uintptr, rc rect, dpi int) {
	cw := rc.right - rc.left
	ch := rc.bottom - rc.top
	sc := func(v int32) int32 { return scalePx(int32(dpi), v) }

	bgBrush, _, _ := procCreateSolidBrush.Call(colBG)
	var fill rect
	fill = rect{left: 0, top: 0, right: cw, bottom: ch}
	procFillRect.Call(hdc, uintptr(unsafe.Pointer(&fill)), bgBrush)
	procDeleteObject.Call(bgBrush)

	procSetBkMode.Call(hdc, transparent)

	titleFont := createUIFont(-sc(22), fwSemiBold)
	sectionFont := createUIFont(-sc(12), fwNormal)
	bodyFont := createUIFont(-sc(14), fwNormal)
	buttonFont := createUIFont(-sc(14), fwBold)
	defer procDeleteObject.Call(titleFont)
	defer procDeleteObject.Call(sectionFont)
	defer procDeleteObject.Call(bodyFont)
	defer procDeleteObject.Call(buttonFont)

	l := computeSettingsLayout(cw, ch, dpi, tmdbShow, startupState)

	// Title.
	drawUIText(hdc, "Settings",
		rect{left: sc(20), top: sc(48), right: cw - sc(20), bottom: sc(80)},
		dtLeft|dtSingleLine|dtNoPrefix|dtVCenter, titleFont, colText)

	// Section label.
	drawUIText(hdc, "API KEYS",
		rect{left: sc(20), top: sc(100), right: cw - sc(20), bottom: sc(118)},
		dtLeft|dtSingleLine|dtNoPrefix|dtVCenter, sectionFont, colMuted)

	// Explainer, muted and wrapped.
	drawUITextWrapped(hdc, "Optional. The app works without any key - TVmaze, AniList, Jikan and iTunes stay the default. Adding a key enables that source and is tried first.",
		rect{left: sc(20), top: sc(120), right: cw - sc(20), bottom: sc(172)}, sectionFont, colMuted)

	// Active row label.
	drawUIText(hdc, "TMDB",
		rect{left: sc(20), top: sc(180), right: cw - sc(20), bottom: sc(200)},
		dtLeft|dtSingleLine|dtNoPrefix|dtVCenter, bodyFont, colText)

	// Key field: row fill, focus ring, masked-or-revealed text, selection
	// highlight, steady caret. The real value lives only in tmdbField and
	// is never logged.
	fillRoundRect(hdc, l.field, colRow, sc(8))
	ring := colBorder
	if fieldFocused {
		ring = colText
	}
	strokeRoundRect(hdc, l.field, ring, sc(8))
	shown := tmdbField.text
	if !tmdbShow {
		shown = MaskKey(shown)
	}
	textRc := rect{left: l.field.left + sc(12), top: l.field.top, right: l.field.right - sc(12), bottom: l.field.bottom}
	if shown == "" {
		drawUIText(hdc, "Not set", textRc,
			dtLeft|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, bodyFont, colMuted)
	} else {
		if tmdbField.selAll && fieldFocused {
			sz := measureUIText(hdc, bodyFont, shown)
			hiW := sz.cx
			maxW := textRc.right - textRc.left
			if hiW > maxW {
				hiW = maxW
			}
			fillUIRect(hdc, rect{left: textRc.left, top: l.field.top + sc(6), right: textRc.left + hiW + sc(4), bottom: l.field.bottom - sc(6)}, colAccent)
		}
		drawUIText(hdc, shown, textRc,
			dtLeft|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, bodyFont, colText)
	}
	if caretVisible() && tmdbField.selAll == false {
		sz := measureUIText(hdc, bodyFont, shown)
		cx := textRc.left + sz.cx + sc(2)
		if shown == "" {
			cx = textRc.left
		}
		if cx > textRc.right-sc(2) {
			cx = textRc.right - sc(2)
		}
		fillUIRect(hdc, rect{left: cx, top: l.field.top + sc(8), right: cx + sc(2), bottom: l.field.bottom - sc(8)}, colText)
	}

	// Planned row: muted, no input field.
	drawUIText(hdc, "OMDb . TheTVDB . Trakt - planned",
		rect{left: sc(20), top: sc(288), right: cw - sc(20), bottom: sc(308)},
		dtLeft|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, sectionFont, colMuted)

	// LIBRARY + STARTUP + SCAN section labels (buttons come from the rect table).
	drawUIText(hdc, "LIBRARY",
		rect{left: sc(20), top: sc(340), right: cw - sc(20), bottom: sc(358)},
		dtLeft|dtSingleLine|dtNoPrefix|dtVCenter, sectionFont, colMuted)
	drawUIText(hdc, "STARTUP",
		rect{left: sc(20), top: sc(398), right: cw - sc(20), bottom: sc(416)},
		dtLeft|dtSingleLine|dtNoPrefix|dtVCenter, sectionFont, colMuted)
	drawUIText(hdc, "SCAN",
		rect{left: sc(20), top: sc(460), right: cw - sc(20), bottom: sc(478)},
		dtLeft|dtSingleLine|dtNoPrefix|dtVCenter, sectionFont, colMuted)
	drawUITextWrapped(hdc, scanHintText,
		rect{left: sc(20), top: sc(518), right: cw - sc(20), bottom: sc(552)}, sectionFont, colMuted)

	// Buttons: Back/Show/Get-a-key/Test/planned links/Save + LIBRARY row +
	// STARTUP row + SCAN segments.
	for _, b := range l.btns {
		paintButton(hdc, b, dpi, buttonFont, sectionFont)
	}

	// Attribution footer, muted small, wrapped to two lines.
	if saveRc, ok := l.settingsBtn(btnSave); ok {
		drawUITextWrapped(hdc, "This product uses the TMDB API but is not endorsed or certified by TMDB.",
			rect{left: sc(20), top: saveRc.top - sc(48), right: cw - sc(20), bottom: saveRc.top - sc(6)}, sectionFont, colMuted)
	}

	// Status line (shared with the main view).
	drawUIText(hdc, statusText, l.status,
		dtLeft|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, sectionFont, colMuted)
}

// drawUITextWrapped paints multi-line muted text with word wrap.
func drawUITextWrapped(hdc uintptr, s string, rc rect, font uintptr, color uintptr) {
	procSetTextColor.Call(hdc, color)
	oldFont, _, _ := procSelectObject.Call(hdc, font)
	p, _ := syscall.UTF16PtrFromString(s)
	box := rc
	procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(p)), uintptr(^uint32(0)), uintptr(unsafe.Pointer(&box)), uintptr(dtLeft|dtWordBreak|dtNoPrefix))
	procSelectObject.Call(hdc, oldFont)
}

// textExtent mirrors SIZE for GetTextExtentPoint32W.
type textExtent struct {
	cx int32
	cy int32
}

// measureUIText measures s in font on hdc (rune count == UTF-16 length for
// the ASCII keys and - masking this UI ever measures).
func measureUIText(hdc, font uintptr, s string) textExtent {
	var sz textExtent
	if s == "" {
		return sz
	}
	oldFont, _, _ := procSelectObject.Call(hdc, font)
	p, _ := syscall.UTF16PtrFromString(s)
	procGetTextExtentPoint.Call(hdc, uintptr(unsafe.Pointer(p)), uintptr(len([]rune(s))), uintptr(unsafe.Pointer(&sz)))
	procSelectObject.Call(hdc, oldFont)
	return sz
}

// fillUIRect paints a solid rect (caret, selection highlight).
func fillUIRect(hdc uintptr, rc rect, color uintptr) {
	br, _, _ := procCreateSolidBrush.Call(color)
	box := rc
	procFillRect.Call(hdc, uintptr(unsafe.Pointer(&box)), br)
	procDeleteObject.Call(br)
}

// paintButton draws one rect-table button per its style, honoring hover,
// pressed, and the scan-disabled state (Refresh dims while a run is in
// flight; the Scan rect stays enabled as Cancel).
func paintButton(hdc uintptr, b uiButton, dpi int, buttonFont, smallFont uintptr) {
	sc := func(v int32) int32 { return scalePx(int32(dpi), v) }
	hovered := hoverBtn == b.id
	pressed := pressedBtn == b.id
	disabled := scanActive && b.id == btnRefresh

	switch b.style {
	case btnAccent:
		fill := colAccent
		switch {
		case disabled:
			fill = colRow
		case pressed:
			fill = colAccentDown
		case hovered:
			fill = colAccentHov
		}
		fillRoundRect(hdc, b.rc, fill, sc(8))
		tc := colText
		if disabled {
			tc = colMuted
		}
		drawUIText(hdc, b.label, b.rc, dtCenter|dtSingleLine|dtNoPrefix|dtVCenter, buttonFont, tc)
	case btnOutline:
		fill := colBG
		if hovered && !disabled {
			fill = colCard
		}
		if disabled {
			fill = colBG
		}
		fillRoundRect(hdc, b.rc, fill, sc(8))
		strokeRoundRect(hdc, b.rc, colBorder, sc(8))
		tc := colText
		if disabled {
			tc = colMuted
		}
		drawUIText(hdc, b.label, b.rc, dtCenter|dtSingleLine|dtNoPrefix|dtVCenter, buttonFont, tc)
	case btnText:
		tc := colMuted
		if hovered || pressed {
			tc = colText
		}
		drawUIText(hdc, b.label, b.rc, dtCenter|dtSingleLine|dtNoPrefix|dtVCenter, smallFont, tc)
	case btnDanger:
		// Destructive text button: muted red at rest, bright red on
		// hover/press. Same text-only geometry as btnText.
		tc := colDanger
		switch {
		case pressed:
			tc = colAccentDown
		case hovered:
			tc = colAccentHov
		}
		drawUIText(hdc, b.label, b.rc, dtCenter|dtSingleLine|dtNoPrefix|dtVCenter, smallFont, tc)
	default: // btnSubtle
		fill := colRow
		switch {
		case pressed:
			fill = colCard
		case hovered:
			fill = colRowHover
		}
		fillRoundRect(hdc, b.rc, fill, sc(8))
		drawUIText(hdc, b.label, b.rc, dtCenter|dtSingleLine|dtNoPrefix|dtVCenter, buttonFont, colText)
	}
}

// fillRoundRect paints a solid rounded rect: brush AND pen share the fill
// color so no dark outline survives the RoundRect edge.
func fillRoundRect(hdc uintptr, rc rect, color uintptr, radius int32) {
	br, _, _ := procCreateSolidBrush.Call(color)
	pen, _, _ := procCreatePen.Call(psSolid, 1, color)
	oldBr, _, _ := procSelectObject.Call(hdc, br)
	oldPen, _, _ := procSelectObject.Call(hdc, pen)
	procRoundRect.Call(hdc, uintptr(rc.left), uintptr(rc.top), uintptr(rc.right), uintptr(rc.bottom), uintptr(radius), uintptr(radius))
	procSelectObject.Call(hdc, oldBr)
	procSelectObject.Call(hdc, oldPen)
	procDeleteObject.Call(pen)
	procDeleteObject.Call(br)
}

// strokeRoundRect outlines an already-filled rounded rect with a 1px border.
func strokeRoundRect(hdc uintptr, rc rect, color uintptr, radius int32) {
	pen, _, _ := procCreatePen.Call(psSolid, 1, color)
	oldPen, _, _ := procSelectObject.Call(hdc, pen)
	oldMode, _, _ := procSetBkMode.Call(hdc, transparent)
	_ = oldMode
	// NULL_BRUSH fill keeps the interior: stock object 5 (NULL_BRUSH).
	nullBrush, _, _ := procGetStockObject.Call(nullBrushStock)
	oldBr, _, _ := procSelectObject.Call(hdc, nullBrush)
	procRoundRect.Call(hdc, uintptr(rc.left), uintptr(rc.top), uintptr(rc.right), uintptr(rc.bottom), uintptr(radius), uintptr(radius))
	procSelectObject.Call(hdc, oldBr)
	procSelectObject.Call(hdc, oldPen)
	procDeleteObject.Call(pen)
}

// drawUIText paints one line of text in the given font/color/alignment.
func drawUIText(hdc uintptr, s string, rc rect, format uintptr, font uintptr, color uintptr) {
	procSetTextColor.Call(hdc, color)
	oldFont, _, _ := procSelectObject.Call(hdc, font)
	p, _ := syscall.UTF16PtrFromString(s)
	box := rc
	procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(p)), uintptr(^uint32(0)), uintptr(unsafe.Pointer(&box)), format)
	procSelectObject.Call(hdc, oldFont)
}

// mouseXY splits an LPARAM mouse position into signed client coordinates.
func mouseXY(lparam uintptr) (int32, int32) {
	return int32(int16(lparam & 0xFFFF)), int32(int16((lparam >> 16) & 0xFFFF))
}

// windowDpi returns the window DPI, falling back to 96 when the API is
// absent (pre-1607) or fails.
func windowDpi(hwnd uintptr) int {
	if hwnd == 0 {
		return 96
	}
	if d, _, _ := procGetDpiForWindow.Call(hwnd); d != 0 {
		return int(d)
	}
	return 96
}

// systemDpi is the pre-window DPI guess used to size the frame at creation.
func systemDpi() int {
	if d, _, _ := procGetDpiForSystem.Call(); d != 0 {
		return int(d)
	}
	return 96
}

// scalePx scales a 96-DPI logical px value to the given DPI.
func scalePx(dpi, v int32) int32 {
	if dpi <= 0 {
		dpi = 96
	}
	return (v*dpi + 48) / 96
}

// createUIFont builds a Segoe UI font at the given pixel height (negative =
// character height) and weight. The caller DeleteObjects it when done.
func createUIFont(height int32, weight uintptr) uintptr {
	face, _ := syscall.UTF16PtrFromString("Segoe UI")
	h, _, _ := procCreateFontW.Call(
		uintptr(uint32(height)), 0, 0, 0, weight,
		0, 0, 0, defaultCharset, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(face)),
	)
	return h
}

// rgb packs bytes into a COLORREF (0x00BBGGRR).
func rgb(r, g, b byte) uintptr {
	return uintptr(r) | uintptr(g)<<8 | uintptr(b)<<16
}

func appLogf(format string, args ...any) {
	if appLog != nil {
		appLog.logf(format, args...)
	}
}
