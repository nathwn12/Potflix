package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"potflix/internal/library"
	"potflix/internal/presence"
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
	// Main view "Card" link (header, left of Settings) and the Card view's
	// Back + preset buttons (see cardview layout below).
	btnCard          = 8
	btnCardBack      = 9
	btnCardDetailsAt = 30 // +0..3: the 4 Details presets in CardPresets order
	btnCardStateAt   = 40 // +0..3: the 4 State presets in CardPresets order
	// Settings view, LIBRARY + SCAN + STARTUP sections (see settings_sections.go).
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
// and the settings view (library, scan, startup). The app is keyless; the
// main view paints and hit-tests exactly as before plus the Settings link.
type uiView int

const (
	viewMain uiView = iota
	viewSettings
	viewCard
)

// curView is the visible view. UI thread only.
var curView uiView = viewMain

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

// Settings-view win32 extras, declared here (not win32.go) to keep this
// change inside window.go: ShellExecute for the log-folder link.
var (
	procShellExecuteW = shell32.NewProc("ShellExecuteW")
)

// Settings-view message, key, and text constants.
const (
	wmChar      = 0x0102
	wmKeyDown   = 0x0100
	wmSetFocus  = 0x0007
	wmKillFocus = 0x0008

	vkEscape = 0x1B

	swShowNormal = 1

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
	cardLink rect // "Card" text affordance, left of Settings (see computeLayout)
}

// settingsLayout is the hit-test table for the settings view, computed
// purely from client size + DPI (+ the startup-toggle state, which only
// flips its checkbox label), so tests can drive it without a window.
type settingsLayout struct {
	status rect
	btns   []uiButton // Back + LIBRARY row + STARTUP row + SCAN segments
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
		invalidateMain()
		return 0
	case wmKillFocus:
		invalidateMain()
		return 0
	case wmChar:
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
// DPI. All metrics scale from the 460x680 logical design.
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

	// "Card" text affordance: left of Settings, in the dead strip below the
	// subtitle (y=68) and above the section label (y=92). Its edges only
	// touch the subtitle, Settings, and section rects, so no painted text
	// rect ever intersects it.
	cardLnkW, cardLnkH := sc(100), sc(24)
	l.cardLink = rect{left: l.settings.left - cardLnkW, top: sc(68), right: l.settings.left, bottom: sc(68) + cardLnkH}

	// Status line pinned near the bottom.
	stH := sc(20)
	l.status = rect{left: margin, top: ch - sc(14) - stH, right: cw - margin, bottom: ch - sc(14)}
	return l
}

// computeSettingsLayout builds the settings-view hit-test table for the
// given client size, DPI, and startup-toggle state (startup only flips its
// checkbox label). Same window, same palette/fonts/DPI as the main view.
// Sections stack from the top with no gaps: LIBRARY, STARTUP, SCAN.
func computeSettingsLayout(cw, ch int32, dpi int, startup bool) settingsLayout {
	sc := func(v int32) int32 { return scalePx(int32(dpi), v) }
	margin := sc(mainMargin)

	var l settingsLayout

	backW, backH := sc(80), sc(28)
	back := rect{left: margin, top: sc(16), right: margin + backW, bottom: sc(16) + backH}

	// LIBRARY section: label + one row of three (Clear cache / Export... /
	// Import...).
	libTop := sc(100)
	libH := sc(32)
	libW := (cw - margin*2 - sc(12)*2) / 3
	clearRc := rect{left: margin, top: libTop, right: margin + libW, bottom: libTop + libH}
	exportRc := rect{left: margin + libW + sc(12), top: libTop, right: margin + libW*2 + sc(12), bottom: libTop + libH}
	importRc := rect{left: margin + libW*2 + sc(24), top: libTop, right: cw - margin, bottom: libTop + libH}

	startTop := sc(172)
	startH := sc(32)
	toggleW := sc(230)
	toggleRc := rect{left: margin, top: startTop, right: margin + toggleW, bottom: startTop + startH}
	logRc := rect{left: margin + toggleW + sc(12), top: startTop, right: cw - margin, bottom: startTop + startH}

	// SCAN section: label + three-way segmented control (Gentle / Balanced
	// / Turbo) + muted hint.
	scanTop := sc(244)
	scanH := sc(32)
	scanW := (cw - margin*2 - sc(12)*2) / 3
	curScan := NormalizeScanSpeed(scanSpeedState)
	gentleRc := rect{left: margin, top: scanTop, right: margin + scanW, bottom: scanTop + scanH}
	balancedRc := rect{left: margin + scanW + sc(12), top: scanTop, right: margin + scanW*2 + sc(12), bottom: scanTop + scanH}
	turboRc := rect{left: margin + scanW*2 + sc(24), top: scanTop, right: cw - margin, bottom: scanTop + scanH}

	stH := sc(20)
	status := rect{left: margin, top: ch - sc(14) - stH, right: cw - margin, bottom: ch - sc(14)}

	l.status = status
	l.btns = []uiButton{
		{id: btnBack, label: "Back", style: btnText, rc: back},
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

// namedTextRect is one painted text rect with its stable audit id, for the
// layout-invariant test (no HDC needed - geometry only).
type namedTextRect struct {
	id string
	rc rect
}

// mainTextRects returns every text rect the main paint draws for the given
// client/DPI/dir set: header, section, rows, buttons, Settings link, status.
// Pure (no syscalls) so tests drive it at any DPI.
func mainTextRects(cw, ch int32, dpi int, l uiLayout, dirCount int) []namedTextRect {
	sc := func(v int32) int32 { return scalePx(int32(dpi), v) }
	out := []namedTextRect{
		{id: "main.title", rc: rect{left: sc(20), top: sc(16), right: cw - sc(72), bottom: sc(48)}},
		{id: "main.subtitle", rc: rect{left: sc(20), top: sc(48), right: cw - sc(20) - sc(120) - sc(8), bottom: sc(68)}},
		{id: "main.section", rc: rect{left: sc(20), top: sc(92), right: cw - sc(60), bottom: sc(110)}},
		{id: "main.count", rc: rect{left: cw - sc(60), top: sc(92), right: cw - sc(20), bottom: sc(110)}},
	}
	if len(l.rows) == 0 {
		out = append(out, namedTextRect{id: "main.empty",
			rc: rect{left: l.card.left, top: l.card.top, right: l.card.right, bottom: l.card.bottom}})
	} else {
		for i, r := range l.rows {
			textRight := r.right - sc(12)
			// Hover-x inset is paint-state dependent; the invariant uses
			// the full inset rect (a superset of the hovered text rect).
			out = append(out, namedTextRect{id: fmt.Sprintf("main.row-%d", i),
				rc: rect{left: r.left + sc(12), top: r.top, right: textRight, bottom: r.bottom}})
		}
		if l.overflow > 0 {
			out = append(out, namedTextRect{id: "main.overflow",
				rc: rect{left: l.card.left, top: l.card.bottom - sc(22), right: l.card.right - sc(12), bottom: l.card.bottom - sc(6)}})
		}
	}
	for _, b := range l.btns {
		out = append(out, namedTextRect{id: fmt.Sprintf("main.btn-%d", b.id), rc: b.rc})
	}
	out = append(out, namedTextRect{id: "main.settings", rc: l.settings})
	out = append(out, namedTextRect{id: "main.card", rc: l.cardLink})
	out = append(out, namedTextRect{id: "main.status", rc: l.status})
	_ = dirCount
	return out
}

// settingsTextRects returns every text rect the settings paint draws for the
// given client/DPI: title, section labels, hint box, buttons, status. Pure
// (no syscalls) so tests drive it at any DPI.
func settingsTextRects(cw, ch int32, dpi int, l settingsLayout) []namedTextRect {
	sc := func(v int32) int32 { return scalePx(int32(dpi), v) }
	out := []namedTextRect{
		{id: "settings.title", rc: rect{left: sc(20), top: sc(48), right: cw - sc(20), bottom: sc(80)}},
		{id: "settings.lib", rc: rect{left: sc(20), top: sc(80), right: cw - sc(20), bottom: sc(98)}},
		{id: "settings.startup", rc: rect{left: sc(20), top: sc(150), right: cw - sc(20), bottom: sc(168)}},
		{id: "settings.scan", rc: rect{left: sc(20), top: sc(222), right: cw - sc(20), bottom: sc(240)}},
		{id: "settings.hint", rc: rect{left: sc(20), top: sc(280), right: cw - sc(20), bottom: sc(328)}},
		{id: "settings.dead", rc: rect{left: sc(20), top: sc(332), right: cw - sc(20), bottom: sc(350)}},
	}
	for _, b := range l.btns {
		out = append(out, namedTextRect{id: fmt.Sprintf("settings.btn-%d", b.id), rc: b.rc})
	}
	out = append(out, namedTextRect{id: "settings.status", rc: l.status})
	_, _ = cw, ch
	return out
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
	return computeSettingsLayout(cw, ch, windowDpi(hwnd), startupState), true
}

// updateHover refreshes the hover state from (x,y); reports whether a
// repaint is needed.
func updateHover(hwnd uintptr, x, y int32) bool {
	if curView == viewCard {
		l, ok := layoutForCard(hwnd)
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
	if ptInRect(l.cardLink, x, y) {
		nb = btnCard
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
	if curView == viewCard {
		l, ok := layoutForCard(hwnd)
		if !ok {
			return
		}
		pressedBtn, pressedX = 0, -1
		if id := buttonAtPoint(l.btns, x, y); id != 0 {
			pressedBtn = id
		}
		invalidateMain()
		return
	}
	if curView == viewSettings {
		l, ok := layoutForSettings(hwnd)
		if !ok {
			return
		}
		pressedBtn, pressedX = 0, -1
		if id := buttonAtPoint(l.btns, x, y); id != 0 {
			pressedBtn = id
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
	} else if ptInRect(l.cardLink, x, y) {
		pressedBtn = btnCard
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
	if curView == viewCard {
		l, ok := layoutForCard(hwnd)
		if !ok {
			pressedBtn, pressedX = 0, -1
			return
		}
		pb := pressedBtn
		pressedBtn, pressedX = 0, -1
		if pb != 0 {
			if buttonAtPoint(l.btns, x, y) == pb {
				doCardAction(hwnd, pb)
			} else {
				invalidateMain()
			}
			return
		}
		invalidateMain()
		return
	}
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
		if pb == btnCard {
			if ptInRect(l.cardLink, x, y) {
				enterCard()
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
	if curView == viewCard {
		l, ok := layoutForCard(hwnd)
		if !ok {
			return 0
		}
		if buttonAtPoint(l.btns, x, y) != 0 {
			return 1
		}
		return 0
	}
	if curView == viewSettings {
		l, ok := layoutForSettings(hwnd)
		if !ok {
			return 0
		}
		if buttonAtPoint(l.btns, x, y) != 0 {
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
	if ptInRect(l.cardLink, x, y) {
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
// Settings view: view switching and actions.
// ---------------------------------------------------------------------------

// openSettings is the pure view-switch core (no syscalls, no disk).
// Tests drive this directly; enterSettings is the disk-backed wrapper.
func openSettings() {
	curView = viewSettings
	hoverBtn, pressedBtn = 0, 0
	hoverRow, hoverX, pressedX = -1, false, -1
	invalidateMain()
}

// closeSettings is the pure return-to-main core.
func closeSettings() {
	curView = viewMain
	hoverBtn, pressedBtn = 0, 0
	invalidateMain()
}

// enterSettings refreshes the ACTUAL startup state from the registry
// (never the stored flag alone), refreshes the SCAN speed selection from
// the config store, and swaps to the settings view.
func enterSettings() {
	startupState = queryStartupEnabled()
	scanSpeedState = loadConfig().EffectiveScanSpeed()
	refreshDeadCacheCount()
	openSettings()
}

// enterMain returns to the folders view.
func enterMain() {
	closeSettings()
}

// onSettingsKeyDown handles WM_KEYDOWN for the settings and card views:
// Esc returns to the main view. Reports whether a repaint is needed.
func onSettingsKeyDown(hwnd uintptr, vk uintptr) bool {
	if curView == viewCard {
		if vk == vkEscape {
			closeCard()
			return true
		}
		_ = hwnd
		return false
	}
	if curView != viewSettings {
		return false
	}
	if vk == vkEscape {
		enterMain()
		return true
	}
	_ = hwnd
	return false
}

// doSettingsAction routes a settings-view button click.
func doSettingsAction(hwnd uintptr, id int) {
	switch id {
	case btnBack:
		enterMain()
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

// ---------------------------------------------------------------------------
// Card view: the exact-payload preview plus the Details/State preset rows.
// Same window, palette, fonts, DPI scaling, hover/double-buffering as the
// other views. The preview reads the recorded sent Activity verbatim
// (previewActivity in main.go) - it never rebuilds the payload. There is no
// bitmap rendering here by design: the app hands Discord a URL and never
// decodes a pixel, so the preview shows the payload's values.
// ---------------------------------------------------------------------------

// cardSpecState is the preset selection shown in the Card view. Refreshed
// from the config store each time the view opens (enterCard) and on every
// preset click. UI thread only, like scanSpeedState.
var cardSpecState presence.CardSpec

// cardLayout is the hit-test table for the Card view: the preview card with
// its five value rows, Back, and the 8 preset buttons. Computed purely from
// client size + DPI + spec (no syscalls), so tests drive it directly.
type cardLayout struct {
	preview rect
	rows    []rect // five preview value rows inside preview
	status  rect
	btns    []uiButton // Back + 4 Details + 4 State presets
}

// cardLayoutBtn returns the rect of the card button id, or false.
func (l cardLayout) cardBtn(id int) (rect, bool) {
	for _, b := range l.btns {
		if b.id == id {
			return b.rc, true
		}
	}
	return rect{}, false
}

// cardDetailsButton maps a Details preset index (CardPresets order) to its
// button id.
func cardDetailsButton(i int) int { return btnCardDetailsAt + i }

// cardStateButton maps a State preset index (CardPresets order) to its
// button id.
func cardStateButton(i int) int { return btnCardStateAt + i }

// cardPresetForButton maps a Card-view preset button id to its line (0 =
// Details, 1 = State) and preset id; ok=false for any other id.
func cardPresetForButton(id int) (line int, preset string, ok bool) {
	dets, states := presence.CardPresets()
	if i := id - btnCardDetailsAt; i >= 0 && i < len(dets) {
		return 0, dets[i].ID, true
	}
	if i := id - btnCardStateAt; i >= 0 && i < len(states) {
		return 1, states[i].ID, true
	}
	return 0, "", false
}

// cardPresetStyle renders the preset selection in the existing button-style
// language: the active preset is accent-filled, the rest are subtle pills.
func cardPresetStyle(line int, id string, spec presence.CardSpec) int {
	spec = spec.Normalized()
	if line == 0 && id == spec.Details {
		return btnAccent
	}
	if line == 1 && id == spec.State {
		return btnAccent
	}
	return btnSubtle
}

// previewRows renders the recorded payload as five single-line preview
// strings: header, Details, State, images, buttons. Values are the recorded
// Activity's verbatim fields. Pure so tests drive it without a live pipe.
func previewRows(act presence.Activity, have bool) [5]string {
	if !have {
		return [5]string{
			"Header: -",
			"Details: -",
			"State: -",
			"Images: -",
			"Buttons: -",
		}
	}
	header := act.Name
	if header == "" {
		// presence.go: an empty Name falls back to the Discord app name.
		header = "(Discord app name)"
	}
	images := act.LargeImage + " + " + act.SmallImage
	buttons := "none"
	if len(act.Buttons) > 0 {
		parts := make([]string, 0, len(act.Buttons))
		for _, b := range act.Buttons {
			parts = append(parts, b.Label+" -> "+b.URL)
		}
		buttons = strings.Join(parts, " | ")
	}
	return [5]string{
		"Header: " + header,
		"Details: " + act.Details,
		"State: " + act.State,
		"Images: " + images,
		"Buttons: " + buttons,
	}
}

// computeCardLayout builds the Card-view hit-test table for the given
// client size, DPI, and spec (the spec only flips preset button styles).
// Sections stack with gaps: Back/title, preview card, LINE 1 row, LINE 2
// row, hint, status.
func computeCardLayout(cw, ch int32, dpi int, spec presence.CardSpec) cardLayout {
	sc := func(v int32) int32 { return scalePx(int32(dpi), v) }
	margin := sc(mainMargin)

	var l cardLayout

	backW, backH := sc(80), sc(28)
	back := rect{left: margin, top: sc(16), right: margin + backW, bottom: sc(16) + backH}

	// Preview region: rows stack from top+pad and must end by bottom-pad.
	// Short content draws the five fixed single-line rows (l.rows); long
	// values wrap in boxes measured with the real HDC at paint time (see
	// paintPreviewRows), so the region is tall enough for real payloads.
	prevTop, prevBottom := sc(100), sc(400)
	prevPad := sc(10)
	rowH, rowPitch := sc(20), sc(22)
	l.preview = rect{left: margin, top: prevTop, right: cw - margin, bottom: prevBottom}
	for i := 0; i < 5; i++ {
		top := prevTop + prevPad + int32(i)*rowPitch
		l.rows = append(l.rows, rect{left: margin + prevPad, top: top,
			right: cw - margin - prevPad, bottom: top + rowH})
	}

	// Preset rows: four across with 12px gaps, like the LIBRARY row.
	rowW := (cw - margin*2 - sc(12)*3) / 4
	seg := func(top int32, line int, presets []presence.CardPreset) []uiButton {
		out := make([]uiButton, 0, len(presets))
		for i, p := range presets {
			left := margin + int32(i)*(rowW+sc(12))
			right := left + rowW
			if i == len(presets)-1 {
				right = cw - margin
			}
			var id int
			if line == 0 {
				id = cardDetailsButton(i)
			} else {
				id = cardStateButton(i)
			}
			out = append(out, uiButton{id: id, label: p.Label,
				style: cardPresetStyle(line, p.ID, spec),
				rc:    rect{left: left, top: top, right: right, bottom: top + sc(32)}})
		}
		return out
	}
	dets, states := presence.CardPresets()
	l.btns = append(l.btns, uiButton{id: btnCardBack, label: "Back", style: btnText, rc: back})
	l.btns = append(l.btns, seg(sc(434), 0, dets)...)
	l.btns = append(l.btns, seg(sc(500), 1, states)...)

	stH := sc(20)
	l.status = rect{left: margin, top: ch - sc(14) - stH, right: cw - margin, bottom: ch - sc(14)}
	return l
}

// cardTextRects returns every text rect the card paint draws: title, preview
// label, the five value rows, the two line labels, the preset buttons, the
// hint, and status. Pure (no syscalls) so tests drive it at any DPI.
func cardTextRects(cw, ch int32, dpi int, l cardLayout) []namedTextRect {
	sc := func(v int32) int32 { return scalePx(int32(dpi), v) }
	out := []namedTextRect{
		{id: "card.title", rc: rect{left: sc(20), top: sc(48), right: cw - sc(20), bottom: sc(80)}},
		{id: "card.preview", rc: rect{left: sc(20), top: sc(80), right: cw - sc(20), bottom: sc(98)}},
		{id: "card.details", rc: rect{left: sc(20), top: sc(412), right: cw - sc(20), bottom: sc(430)}},
		{id: "card.state", rc: rect{left: sc(20), top: sc(478), right: cw - sc(20), bottom: sc(496)}},
		{id: "card.hint", rc: rect{left: sc(20), top: sc(544), right: cw - sc(20), bottom: sc(562)}},
	}
	for i, r := range l.rows {
		out = append(out, namedTextRect{id: fmt.Sprintf("card.row-%d", i), rc: r})
	}
	for _, b := range l.btns {
		out = append(out, namedTextRect{id: fmt.Sprintf("card.btn-%d", b.id), rc: b.rc})
	}
	out = append(out, namedTextRect{id: "card.status", rc: l.status})
	_, _ = cw, ch
	return out
}

// layoutForCard snapshots the current client rect + DPI and builds the
// Card-view hit-test table; ok=false when the window has no size yet.
func layoutForCard(hwnd uintptr) (cardLayout, bool) {
	var rc rect
	procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	cw := rc.right - rc.left
	ch := rc.bottom - rc.top
	if cw <= 0 || ch <= 0 {
		return cardLayout{}, false
	}
	return computeCardLayout(cw, ch, windowDpi(hwnd), cardSpecState), true
}

// openCard is the pure view-switch core (no syscalls, no disk).
// Tests drive this directly; enterCard is the disk-backed wrapper.
func openCard() {
	curView = viewCard
	hoverBtn, pressedBtn = 0, 0
	hoverRow, hoverX, pressedX = -1, false, -1
	invalidateMain()
}

// closeCard is the pure return-to-main core for the Card view.
func closeCard() {
	curView = viewMain
	hoverBtn, pressedBtn = 0, 0
	invalidateMain()
}

// enterCard refreshes the preset selection from the config store and swaps
// to the Card view.
func enterCard() {
	cardSpecState = loadConfig().EffectiveCardSpec()
	openCard()
}

// onSelectCardPreset saves the chosen Details (line 0) or State (line 1)
// preset and stores it live: the next pushActivity send carries the new
// lines (the default spec is byte-identical to today's card, so a default
// store changes nothing on the wire).
func onSelectCardPreset(line int, preset string) {
	cur := loadConfig().EffectiveCardSpec()
	details, state := cur.Details, cur.State
	if line == 0 {
		details = preset
	} else {
		state = preset
	}
	cfg := loadConfig()
	if !cfg.SetCardSpec(details, state) {
		cardSpecState = cfg.EffectiveCardSpec()
		invalidateMain()
		return
	}
	if err := saveConfig(cfg); err != nil {
		appLogf("card: save failed: %s", err)
		setStatusText("Save failed.")
		return
	}
	liveConfig.Store(&cfg)
	cardSpecState = cfg.EffectiveCardSpec()
	dets, states := presence.CardPresets()
	name := preset
	if line == 0 {
		for _, p := range dets {
			if p.ID == cardSpecState.Details {
				name = p.Label
			}
		}
		setStatusText("Line 1: " + name)
	} else {
		for _, p := range states {
			if p.ID == cardSpecState.State {
				name = p.Label
			}
		}
		setStatusText("Line 2: " + name)
	}
	appLogf("card: line %d -> %s", line+1, preset)
}

// doCardAction routes a Card-view button click: Back or a preset segment.
func doCardAction(hwnd uintptr, id int) {
	switch {
	case id == btnCardBack:
		closeCard()
	default:
		if line, preset, ok := cardPresetForButton(id); ok {
			onSelectCardPreset(line, preset)
		}
	}
	_ = hwnd
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
	// The failed count shows ONLY when non-zero, so a clean line is unchanged.
	failed := ""
	if p.KeysFailed > 0 {
		failed = fmt.Sprintf(" - %s failed", formatScanCount(p.KeysFailed))
	}
	return fitScanStatus(fmt.Sprintf("Scanning %s/%s keys | %s fetched - %s cached%s | folders: %s scanned, %s skipped",
		formatScanCount(p.KeysDone), formatScanCount(p.KeysTotal),
		formatScanCount(p.KeysFetched), formatScanCount(p.KeysCached), failed,
		formatScanCount(p.FoldersScanned), formatScanCount(p.FoldersSkipped)))
}

// scanDoneText maps kind ("Scan"/"Refresh") + stats to the final summary.
// Pure so tests drive it without a live window.
func scanDoneText(kind string, s library.Stats) string {
	if s.KeysFetched == 0 && s.KeysNotFound == 0 && s.KeysFailed == 0 && s.FoldersSkipped > 0 {
		return fitScanStatus(fmt.Sprintf("%s complete: nothing to fetch (%s cached) - %s folders skipped",
			kind, formatScanCount(s.KeysCached), formatScanCount(s.FoldersSkipped)))
	}
	// The failed count shows ONLY when non-zero, so a clean line is unchanged.
	failed := ""
	if s.KeysFailed > 0 {
		failed = fmt.Sprintf(" - %s failed", formatScanCount(s.KeysFailed))
	}
	return fitScanStatus(fmt.Sprintf("%s complete: %s keys - %s cached - %s fetched - %s not found%s | %s files - folders: %s scanned, %s skipped",
		kind, formatScanCount(s.Keys), formatScanCount(s.KeysCached), formatScanCount(s.KeysFetched),
		formatScanCount(s.KeysNotFound), failed, formatScanCount(s.Files),
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
		return fitScanStatus(kind + " failed: " + err.Error())
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
		setStatusText(fitScanStatus(kind + " failed: " + err.Error()))
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
	beginUIAudit()
	defer endUIAudit()
	if curView == viewCard {
		paintCardView(hdc, rc, dpi)
		return
	}
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
	// The subtitle reserves the Settings link's width (link is sc(120)
	// wide at the right margin + an sc(8) gap) so the two rects can never
	// intersect; the wordmark above the link has no vertical overlap.
	subRight := cw - sc(20) - sc(120) - sc(8)
	drawUIText(hdc, "main.title", "POTFLIX",
		rect{left: sc(20), top: sc(16), right: cw - sc(72), bottom: sc(48)},
		dtLeft|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, titleFont, colAccent)
	drawUIText(hdc, "main.subtitle", "Discord Rich Presence for PotPlayer",
		rect{left: sc(20), top: sc(48), right: subRight, bottom: sc(68)},
		dtLeft|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, sectionFont, colMuted)
	if uiIcon != 0 {
		sz := sc(32)
		procDrawIconEx.Call(hdc, uintptr(cw-sc(20)-sz), uintptr(sc(20)),
			uiIcon, uintptr(sz), uintptr(sz), 0, 0, diNormal)
	}

	// 2. Section label + count.
	drawUIText(hdc, "main.section", "MEDIA FOLDERS",
		rect{left: sc(20), top: sc(92), right: cw - sc(60), bottom: sc(110)},
		dtLeft|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, sectionFont, colMuted)
	drawUIText(hdc, "main.count", fmt.Sprintf("%d", len(uiDirs)),
		rect{left: cw - sc(60), top: sc(92), right: cw - sc(20), bottom: sc(110)},
		dtRight|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, sectionFont, colMuted)

	l := computeLayout(cw, ch, dpi)

	// 3. Folder card + one pill per DIR.
	fillRoundRect(hdc, l.card, colCard, sc(12))
	if len(l.rows) == 0 {
		drawUIText(hdc, "main.empty", "No folders yet - add one below",
			rect{left: l.card.left, top: l.card.top, right: l.card.right, bottom: l.card.bottom},
			dtCenter|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, bodyFont, colMuted)
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
				drawUIText(hdc, fmt.Sprintf("main.rowx-%d", i), "x", l.xbtns[i],
					dtCenter|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, bodyFont, colMuted)
				textRight = l.xbtns[i].left - sc(4)
			}
			drawUIText(hdc, fmt.Sprintf("main.row-%d", i), uiDirs[i],
				rect{left: r.left + sc(12), top: r.top, right: textRight, bottom: r.bottom},
				dtLeft|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, bodyFont, colText)
		}
		if l.overflow > 0 {
			drawUIText(hdc, "main.overflow", fmt.Sprintf("+%d more", l.overflow),
				rect{left: l.card.left, top: l.card.bottom - sc(22), right: l.card.right - sc(12), bottom: l.card.bottom - sc(6)},
				dtRight|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, sectionFont, colMuted)
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
		drawUIText(hdc, "main.settings", "Settings", l.settings,
			dtRight|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, sectionFont, tc)
	}

	// 4c. Card affordance: the same text-button language, one slot left of
	// Settings in the header dead strip (see computeLayout).
	{
		tc := colMuted
		if hoverBtn == btnCard || pressedBtn == btnCard {
			tc = colText
		}
		drawUIText(hdc, "main.card", "Card", l.cardLink,
			dtRight|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, sectionFont, tc)
	}

	// 5. Status line.
	drawUIText(hdc, "main.status", statusText, l.status,
		dtLeft|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, sectionFont, colMuted)
}

// paintSettingsCard draws the settings view in the same window: Back +
// title, the LIBRARY section, the STARTUP section, the SCAN section, and
// the shared status line. Same palette, fonts, and DPI scaling as the main
// view.
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
	buttonFont := createUIFont(-sc(14), fwBold)
	defer procDeleteObject.Call(titleFont)
	defer procDeleteObject.Call(sectionFont)
	defer procDeleteObject.Call(buttonFont)

	l := computeSettingsLayout(cw, ch, dpi, startupState)

	// Title.
	drawUIText(hdc, "settings.title", "Settings",
		rect{left: sc(20), top: sc(48), right: cw - sc(20), bottom: sc(80)},
		dtLeft|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, titleFont, colText)

	// LIBRARY + STARTUP + SCAN section labels (buttons come from the rect table).
	drawUIText(hdc, "settings.lib", "LIBRARY",
		rect{left: sc(20), top: sc(80), right: cw - sc(20), bottom: sc(98)},
		dtLeft|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, sectionFont, colMuted)
	drawUIText(hdc, "settings.startup", "STARTUP",
		rect{left: sc(20), top: sc(150), right: cw - sc(20), bottom: sc(168)},
		dtLeft|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, sectionFont, colMuted)
	drawUIText(hdc, "settings.scan", "SCAN",
		rect{left: sc(20), top: sc(222), right: cw - sc(20), bottom: sc(240)},
		dtLeft|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, sectionFont, colMuted)
	// SCAN hint: the old 280..314 box (34 px) clipped the third wrapped
	// line (~45 px for 3 lines at section size). The 280..328 box (48 px)
	// fits 3 lines with room; anything longer still ellipsizes via
	// drawUITextWrapped instead of clipping. The status line sits far
	// below (646+), so the taller box overlaps nothing.
	drawUITextWrapped(hdc, "settings.hint", scanHintText,
		rect{left: sc(20), top: sc(280), right: cw - sc(20), bottom: sc(328)}, sectionFont, colMuted)

	// Buttons: Back + LIBRARY row + STARTUP row + SCAN segments.
	for _, b := range l.btns {
		paintButton(hdc, b, dpi, buttonFont, sectionFont)
	}

	// Dead-titles line: how many cache entries are marked not-found (a Clear
	// cache retries them). One ellipsized line in the empty strip below the
	// SCAN hint, far above the status line.
	drawUIText(hdc, "settings.dead", deadCacheLabel(deadCacheCount),
		rect{left: sc(20), top: sc(332), right: cw - sc(20), bottom: sc(350)},
		dtLeft|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, sectionFont, colMuted)

	// Status line (shared with the main view).
	drawUIText(hdc, "settings.status", statusText, l.status,
		dtLeft|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, sectionFont, colMuted)
}

// paintCardView draws the Card view in the same window: Back + title, the
// exact-payload preview card, the LINE 1 / LINE 2 preset rows, a hint, and
// the shared status line. Same palette, fonts, and DPI scaling as the other
// views. The preview rows show the recorded sent Activity verbatim
// (previewActivity in main.go) - no bitmap rendering by design (NO bitmap
// rendering: the app hands Discord a URL and never decodes a pixel).
func paintCardView(hdc uintptr, rc rect, dpi int) {
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
	buttonFont := createUIFont(-sc(14), fwBold)
	defer procDeleteObject.Call(titleFont)
	defer procDeleteObject.Call(sectionFont)
	defer procDeleteObject.Call(buttonFont)

	l := computeCardLayout(cw, ch, dpi, cardSpecState)
	act, have := previewActivity()
	rows := previewRows(act, have)

	// Title + section labels.
	drawUIText(hdc, "card.title", "Card",
		rect{left: sc(20), top: sc(48), right: cw - sc(20), bottom: sc(80)},
		dtLeft|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, titleFont, colText)
	drawUIText(hdc, "card.preview", "PREVIEW - EXACT DISCORD PAYLOAD",
		rect{left: sc(20), top: sc(80), right: cw - sc(20), bottom: sc(98)},
		dtLeft|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, sectionFont, colMuted)

	// Preview card: the five value rows. Short content draws the fixed
	// single-line geometry (l.rows, matching cardTextRects); long values
	// wrap in boxes measured with the real HDC (see paintPreviewRows), so
	// the exact sent strings never overflow or overlap.
	paintPreviewRows(hdc, dpi, l.preview, rows, sectionFont)

	// Preset rows.
	drawUIText(hdc, "card.details", "LINE 1 - DETAILS",
		rect{left: sc(20), top: sc(412), right: cw - sc(20), bottom: sc(430)},
		dtLeft|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, sectionFont, colMuted)
	drawUIText(hdc, "card.state", "LINE 2 - STATE",
		rect{left: sc(20), top: sc(478), right: cw - sc(20), bottom: sc(496)},
		dtLeft|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, sectionFont, colMuted)
	drawUIText(hdc, "card.hint", "Preview shows the exact values handed to Discord.",
		rect{left: sc(20), top: sc(544), right: cw - sc(20), bottom: sc(562)},
		dtLeft|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, sectionFont, colMuted)

	// Buttons: Back + 4 Details + 4 State presets.
	for _, b := range l.btns {
		paintButton(hdc, b, dpi, buttonFont, sectionFont)
	}

	// Status line (shared with the other views).
	drawUIText(hdc, "card.status", statusText, l.status,
		dtLeft|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, sectionFont, colMuted)
}

// singleLineFits reports whether s fits in w px on hdc with the currently
// selected font. Callers select the font first (as drawUIText does).
func singleLineFits(hdc uintptr, s string, w int32) bool {
	if s == "" {
		return true
	}
	u, err := syscall.UTF16FromString(s)
	if err != nil || len(u) == 0 {
		return true
	}
	n := len(u) - 1
	if n <= 0 {
		return true
	}
	var sz textSize
	r, _, _ := procGetTextExtentPoint32W.Call(hdc,
		uintptr(unsafe.Pointer(&u[0])), uintptr(n),
		uintptr(unsafe.Pointer(&sz)))
	if r == 0 {
		return true
	}
	return sz.cx <= w
}

// wrappedNeed returns the DT_CALCRECT wrapped height of s at width w on hdc
// with the currently selected font (the caller selects it first).
func wrappedNeed(hdc uintptr, s string, w int32) int32 {
	p, _ := syscall.UTF16PtrFromString(s)
	box := rect{left: 0, top: 0, right: w, bottom: 0}
	procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(p)), uintptr(^uint32(0)),
		uintptr(unsafe.Pointer(&box)),
		uintptr(dtLeft|dtWordBreak|dtNoPrefix|dtEndEllipsis|dtCalcRect))
	return box.bottom - box.top
}

// breakRow splits s into lines that each fit w px on hdc with the
// currently selected font, preferring spaces and hard-breaking long tokens
// (URLs) anywhere, and joins them with a newline for DT_WORDBREAK drawing.
// Display-only: the recorded payload value is unchanged. A single rune
// wider than w still overflows, and the audit then correctly flags it.
func breakRow(hdc uintptr, s string, w int32) string {
	if singleLineFits(hdc, s, w) {
		return s
	}
	runes := []rune(s)
	var lines []string
	start := 0
	for start < len(runes) {
		// Binary-search the longest fitting prefix from start.
		lo, hi := start+1, len(runes)
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if singleLineFits(hdc, string(runes[start:mid]), w) {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		end := lo
		if end <= start {
			end = start + 1 // one rune, even if it overflows
		}
		// Prefer the last space within the fitting prefix.
		cut := end
		for i := end - 1; i > start; i-- {
			if runes[i] == ' ' {
				cut = i
				break
			}
		}
		lines = append(lines, string(runes[start:cut]))
		start = cut
		if start < len(runes) && runes[start] == ' ' {
			start++ // skip the break space
		}
	}
	return strings.Join(lines, "\n")
}

// paintPreviewRows draws the five preview value rows stacked from the top of
// the preview region, then the card background around the used extent (the
// background is not a text span, so its size never reaches the audit).
// Each row is single-line with dtEndEllipsis when its real extent fits, and
// a wrapped box sized to its CALCRECT height otherwise - so the live audit
// sees no overflow (single-line rows fit by check, wrapped rows fit by
// measurement) and no overlaps (rows stack with gaps inside the region).
// Rows that would pass the region cap are clamped with an ellipsis and the
// audit then correctly flags that pathological case.
func paintPreviewRows(hdc uintptr, dpi int, region rect, rows [5]string, font uintptr) {
	sc := func(v int32) int32 { return scalePx(int32(dpi), v) }
	pad, rowH, gap := sc(10), sc(20), sc(2)
	w := region.right - region.left - pad*2
	x0 := region.left + pad
	y := region.top + pad
	maxBottom := region.bottom - pad

	procSetTextColor.Call(hdc, colText)
	oldFont, _, _ := procSelectObject.Call(hdc, font)
	defer procSelectObject.Call(hdc, oldFont)

	type rowBox struct {
		draw string
		h    int32
		wrap bool
	}
	boxes := make([]rowBox, 0, len(rows))
	for _, s := range rows {
		draw, h := s, rowH
		wrap := false
		if !singleLineFits(hdc, s, w) {
			draw = breakRow(hdc, s, w)
			h = wrappedNeed(hdc, draw, w)
			if h < rowH {
				h = rowH
			}
			wrap = true
		}
		if y+h > maxBottom {
			// Pathological content (e.g. a huge URL): clamp with an
			// ellipsis; the audit flags this case by design.
			h = maxBottom - y
			if h <= 0 {
				break
			}
			wrap = true
		}
		boxes = append(boxes, rowBox{draw: draw, h: h, wrap: wrap})
		y += h + gap
	}
	usedBottom := y - gap
	if usedBottom < region.top+pad+rowH {
		usedBottom = region.top + pad + rowH
	}
	fillRoundRect(hdc, rect{left: region.left, top: region.top, right: region.right, bottom: usedBottom + pad}, colCard, sc(12))

	y = region.top + pad
	for i, b := range boxes {
		rc := rect{left: x0, top: y, right: x0 + w, bottom: y + b.h}
		if b.wrap {
			drawUITextWrapped(hdc, fmt.Sprintf("card.row-%d", i), b.draw, rc, font, colText)
		} else {
			drawUIText(hdc, fmt.Sprintf("card.row-%d", i), b.draw, rc,
				dtLeft|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, font, colText)
		}
		y += b.h + gap
	}
}

// drawUITextWrapped paints multi-line muted text with word wrap, ellipsizing
// any tail that still exceeds rc (dtEndEllipsis) so a long hint trims
// instead of clipping silently.
func drawUITextWrapped(hdc uintptr, id, s string, rc rect, font uintptr, color uintptr) {
	procSetTextColor.Call(hdc, color)
	oldFont, _, _ := procSelectObject.Call(hdc, font)
	if uiAuditOn {
		auditWrapped(hdc, id, s, rc)
	}
	p, _ := syscall.UTF16PtrFromString(s)
	box := rc
	procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(p)), uintptr(^uint32(0)), uintptr(unsafe.Pointer(&box)), uintptr(dtLeft|dtWordBreak|dtNoPrefix|dtEndEllipsis))
	procSelectObject.Call(hdc, oldFont)
}

// paintButton draws one rect-table button per its style, honoring hover,
// pressed, and the scan-disabled state (Refresh dims while a run is in
// flight; the Scan rect stays enabled as Cancel). Labels ellipsize
// (dtEndEllipsis) so a long caption trims instead of clipping silently.
func paintButton(hdc uintptr, b uiButton, dpi int, buttonFont, smallFont uintptr) {
	sc := func(v int32) int32 { return scalePx(int32(dpi), v) }
	hovered := hoverBtn == b.id
	pressed := pressedBtn == b.id
	disabled := scanActive && b.id == btnRefresh
	id := fmt.Sprintf("btn-%d", b.id)

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
		drawUIText(hdc, id, b.label, b.rc, dtCenter|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, buttonFont, tc)
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
		drawUIText(hdc, id, b.label, b.rc, dtCenter|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, buttonFont, tc)
	case btnText:
		tc := colMuted
		if hovered || pressed {
			tc = colText
		}
		drawUIText(hdc, id, b.label, b.rc, dtCenter|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, smallFont, tc)
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
		drawUIText(hdc, id, b.label, b.rc, dtCenter|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, smallFont, tc)
	default: // btnSubtle
		fill := colRow
		switch {
		case pressed:
			fill = colCard
		case hovered:
			fill = colRowHover
		}
		fillRoundRect(hdc, b.rc, fill, sc(8))
		drawUIText(hdc, id, b.label, b.rc, dtCenter|dtSingleLine|dtNoPrefix|dtVCenter|dtEndEllipsis, buttonFont, colText)
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
// Callers pass a stable audit id (e.g. "main.status"); when the live audit
// is armed the real-HDC extent is measured and recorded under that id.
func drawUIText(hdc uintptr, id, s string, rc rect, format uintptr, font uintptr, color uintptr) {
	procSetTextColor.Call(hdc, color)
	oldFont, _, _ := procSelectObject.Call(hdc, font)
	if uiAuditOn {
		auditSingleLine(hdc, id, s, rc)
	}
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

// ---------------------------------------------------------------------------
// UI audit (POTFLIX_UI_AUDIT=1): gated live measurement of every painted
// string with the REAL HDC and font. Zero cost when the env var is unset
// (one bool check per draw call, no slice, no syscalls).
//
// During paint, each drawUIText/drawUITextWrapped call records its id, rect,
// and text; single-line extents come from GetTextExtentPoint32W, wrapped
// heights from DrawTextW+DT_CALCRECT. Overflows log immediately as
// "UI-AUDIT <id> overflow need=WxH rect=WxH text=..."; at the end of the
// paint pass every recorded pair is checked for intersection and logs
// "UI-AUDIT <idA> overlaps <idB> rect=...". Declared here (not win32.go) to
// keep this change inside window.go.
// ---------------------------------------------------------------------------

var procGetTextExtentPoint32W = gdi32.NewProc("GetTextExtentPoint32W")

const dtCalcRect = 0x0400

// textSize mirrors SIZE (cx, cy).
type textSize struct {
	cx int32
	cy int32
}

// uiAuditSpan is one painted text rect in the current paint pass.
type uiAuditSpan struct {
	id   string
	rc   rect
	text string
}

var uiAuditSpans []uiAuditSpan
var uiAuditOn bool

// uiAuditEnabled reports whether the live audit is armed. Checked once per
// paint pass (paintMainCard entry); draw calls only consult uiAuditOn.
func uiAuditEnabled() bool {
	return os.Getenv("POTFLIX_UI_AUDIT") == "1"
}

// beginUIAudit resets the pass slice when armed; endUIAudit checks the
// recorded pairs for intersections. Both are no-ops when disarmed.
func beginUIAudit() {
	uiAuditOn = uiAuditEnabled()
	if uiAuditOn && uiAuditSpans == nil {
		uiAuditSpans = make([]uiAuditSpan, 0, 32)
	}
	if uiAuditOn {
		uiAuditSpans = uiAuditSpans[:0]
	}
}

func endUIAudit() {
	if !uiAuditOn {
		return
	}
	for i := 0; i < len(uiAuditSpans); i++ {
		for j := i + 1; j < len(uiAuditSpans); j++ {
			a, b := uiAuditSpans[i], uiAuditSpans[j]
			if rectsIntersect(a.rc, b.rc) {
				appLogf("UI-AUDIT %s overlaps %s rect=%d,%d-%d,%d vs %d,%d-%d,%d",
					a.id, b.id,
					a.rc.left, a.rc.top, a.rc.right, a.rc.bottom,
					b.rc.left, b.rc.top, b.rc.right, b.rc.bottom)
			}
		}
	}
}

// rectsIntersect reports whether a and b overlap with nonzero area (the
// Win32 rect convention: right/bottom exclusive, so shared edges do not
// count).
func rectsIntersect(a, b rect) bool {
	return a.left < b.right && b.left < a.right && a.top < b.bottom && b.top < a.bottom
}

// auditSingleLine measures s with the REAL HDC and currently-selected font
// and logs when the extent exceeds rc. Caller has already selected font.
func auditSingleLine(hdc uintptr, id, s string, rc rect) {
	if s == "" {
		return
	}
	u, err := syscall.UTF16FromString(s)
	if err != nil || len(u) == 0 {
		return
	}
	// UTF16FromString appends the NUL; GetTextExtent takes a char count.
	n := len(u) - 1
	if n <= 0 {
		return
	}
	var sz textSize
	r, _, _ := procGetTextExtentPoint32W.Call(hdc,
		uintptr(unsafe.Pointer(&u[0])), uintptr(n),
		uintptr(unsafe.Pointer(&sz)))
	if r == 0 {
		return
	}
	w, h := rc.right-rc.left, rc.bottom-rc.top
	if sz.cx > w || sz.cy > h {
		appLogf("UI-AUDIT %s overflow need=%dx%d rect=%dx%d text=%q", id, sz.cx, sz.cy, w, h, s)
	}
	uiAuditSpans = append(uiAuditSpans, uiAuditSpan{id: id, rc: rc, text: s})
}

// auditWrapped measures the CALCRECT wrapped height of s with the REAL HDC
// and currently-selected font and logs when it exceeds rc. Caller has
// already selected font.
func auditWrapped(hdc uintptr, id, s string, rc rect) {
	if s == "" {
		uiAuditSpans = append(uiAuditSpans, uiAuditSpan{id: id, rc: rc, text: s})
		return
	}
	p, _ := syscall.UTF16PtrFromString(s)
	box := rc
	procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(p)), uintptr(^uint32(0)),
		uintptr(unsafe.Pointer(&box)),
		uintptr(dtLeft|dtWordBreak|dtNoPrefix|dtCalcRect))
	w, h := rc.right-rc.left, rc.bottom-rc.top
	needW, needH := box.right-box.left, box.bottom-box.top
	if needH > h || needW > w {
		appLogf("UI-AUDIT %s overflow need=%dx%d rect=%dx%d text=%q", id, needW, needH, w, h, s)
	}
	uiAuditSpans = append(uiAuditSpans, uiAuditSpan{id: id, rc: rc, text: s})
}
