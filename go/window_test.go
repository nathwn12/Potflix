package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"potflix/internal/library"
	"potflix/internal/metadata"
	"potflix/internal/repair"
)

// window_test.go - proves the owner-drawn hit-test seam (go/window.go):
// point-in-rect and the button/row lookup over the rect table return the
// right ids, and the layout keeps every button inside the client area
// without overlaps. Pure functions only - no window is created.

func TestPtInRect(t *testing.T) {
	r := rect{left: 10, top: 20, right: 30, bottom: 40}
	inside := [][2]int32{{10, 20}, {29, 39}, {15, 25}}
	for _, p := range inside {
		if !ptInRect(r, p[0], p[1]) {
			t.Fatalf("ptInRect(%v, %d, %d) = false, want true", r, p[0], p[1])
		}
	}
	outside := [][2]int32{{9, 20}, {10, 19}, {30, 20}, {10, 40}, {0, 0}, {100, 100}}
	for _, p := range outside {
		if ptInRect(r, p[0], p[1]) {
			t.Fatalf("ptInRect(%v, %d, %d) = true, want false", r, p[0], p[1])
		}
	}
}

func TestButtonAtPoint(t *testing.T) {
	btns := []uiButton{
		{id: btnAdd, label: "Add folder", rc: rect{left: 20, top: 400, right: 224, bottom: 436}},
		{id: btnRemove, label: "Remove", rc: rect{left: 236, top: 400, right: 440, bottom: 436}},
		{id: btnScan, label: "Scan", rc: rect{left: 20, top: 448, right: 224, bottom: 484}},
	}
	if got := buttonAtPoint(btns, 100, 410); got != btnAdd {
		t.Fatalf("Add rect hit = %d, want %d", got, btnAdd)
	}
	if got := buttonAtPoint(btns, 300, 410); got != btnRemove {
		t.Fatalf("Remove rect hit = %d, want %d", got, btnRemove)
	}
	if got := buttonAtPoint(btns, 100, 460); got != btnScan {
		t.Fatalf("Scan rect hit = %d, want %d", got, btnScan)
	}
	if got := buttonAtPoint(btns, 230, 410); got != 0 {
		t.Fatalf("gap hit = %d, want 0 (no button)", got)
	}
	if got := buttonAtPoint(btns, 10, 10); got != 0 {
		t.Fatalf("header hit = %d, want 0 (no button)", got)
	}
	if got := buttonAtPoint(nil, 100, 410); got != 0 {
		t.Fatalf("empty table hit = %d, want 0", got)
	}
}

func TestRowAtPoint(t *testing.T) {
	rows := []rect{
		{left: 30, top: 128, right: 430, bottom: 164},
		{left: 30, top: 172, right: 430, bottom: 208},
	}
	if got := rowAtPoint(rows, 100, 140); got != 0 {
		t.Fatalf("row 0 hit = %d, want 0", got)
	}
	if got := rowAtPoint(rows, 100, 180); got != 1 {
		t.Fatalf("row 1 hit = %d, want 1", got)
	}
	if got := rowAtPoint(rows, 100, 168); got != -1 {
		t.Fatalf("inter-row gap hit = %d, want -1", got)
	}
	if got := rowAtPoint(nil, 100, 140); got != -1 {
		t.Fatalf("empty rows hit = %d, want -1", got)
	}
}

func TestComputeLayoutButtons(t *testing.T) {
	oldDirs := uiDirs
	uiDirs = []string{`C:\Media\Movies`, `D:\Shows`}
	defer func() { uiDirs = oldDirs }()

	l := computeLayout(mainClientW, mainClientH, 96)
	if len(l.btns) != 6 {
		t.Fatalf("buttons = %d, want 6 (Add/Remove/Scan/Refresh/Report/Close)", len(l.btns))
	}
	seen := map[int]bool{}
	for _, b := range l.btns {
		if seen[b.id] {
			t.Fatalf("duplicate button id %d", b.id)
		}
		seen[b.id] = true
		if b.rc.left < 0 || b.rc.top < 0 || b.rc.right > mainClientW || b.rc.bottom > mainClientH {
			t.Fatalf("button %d rect %+v escapes the %dx%d client", b.id, b.rc, mainClientW, mainClientH)
		}
	}
	for _, id := range []int{btnAdd, btnRemove, btnScan, btnRefresh, btnReport, btnClose} {
		if !seen[id] {
			t.Fatalf("button id %d missing from layout", id)
		}
	}
	for i := 0; i < len(l.btns); i++ {
		for j := i + 1; j < len(l.btns); j++ {
			a, b := l.btns[i].rc, l.btns[j].rc
			if a.left < b.right && b.left < a.right && a.top < b.bottom && b.top < a.bottom {
				t.Fatalf("buttons %d and %d overlap: %+v vs %+v", l.btns[i].id, l.btns[j].id, a, b)
			}
		}
	}
	if len(l.rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(l.rows))
	}
	// Every row center must hit-test back to its own index.
	for i, r := range l.rows {
		if got := rowAtPoint(l.rows, (r.left+r.right)/2, (r.top+r.bottom)/2); got != i {
			t.Fatalf("row %d center hits %d", i, got)
		}
	}
	// Every button center must hit-test back to its own id.
	for _, b := range l.btns {
		if got := buttonAtPoint(l.btns, (b.rc.left+b.rc.right)/2, (b.rc.top+b.rc.bottom)/2); got != b.id {
			t.Fatalf("button %d center hits %d", b.id, got)
		}
	}
}

// TestComputeLayoutCardFollowsRows proves the folder card fits its rows:
// one DIR yields a shorter card than four, many DIRs clamp to a capped
// card with an overflow count, every visible row center hit-tests back,
// and the buttons stay anchored (identical rects whatever the row count).
func TestComputeLayoutCardFollowsRows(t *testing.T) {
	oldDirs := uiDirs
	defer func() { uiDirs = oldDirs }()

	cardH := func(n int) (uiLayout, int32) {
		uiDirs = make([]string, n)
		for i := range uiDirs {
			uiDirs[i] = string(rune('A'+i)) + `:\Media`
		}
		l := computeLayout(mainClientW, mainClientH, 96)
		return l, l.card.bottom - l.card.top
	}

	l1, h1 := cardH(1)
	l4, h4 := cardH(4)
	if h1 >= h4 {
		t.Fatalf("1-row card height = %d, want < 4-row height %d (no dead space)", h1, h4)
	}
	if len(l1.rows) != 1 || len(l4.rows) != 4 {
		t.Fatalf("rows = %d/%d, want 1/4", len(l1.rows), len(l4.rows))
	}

	l20, h20 := cardH(20)
	_, h5 := cardH(5)
	// Clamped: 5-row height plus the "+N more" strip, still within budget.
	if h20 != h5+22 {
		t.Fatalf("20-row card height = %d, want 5-row height %d + 22 overflow strip", h20, h5)
	}
	if len(l20.rows) != 5 || l20.overflow != 15 {
		t.Fatalf("20 dirs: rows = %d overflow = %d, want 5/15", len(l20.rows), l20.overflow)
	}
	// Clamped rows still hit-test exactly; the overflow strip is not a row.
	for i, r := range l20.rows {
		if got := rowAtPoint(l20.rows, (r.left+r.right)/2, (r.top+r.bottom)/2); got != i {
			t.Fatalf("clamped row %d center hits %d", i, got)
		}
	}
	// Buttons stay anchored at the bottom whatever the row count.
	for i := range l1.btns {
		if l1.btns[i].rc != l20.btns[i].rc {
			t.Fatalf("button %d moved with row count: %+v vs %+v", l1.btns[i].id, l1.btns[i].rc, l20.btns[i].rc)
		}
	}
	if l1.status != l20.status {
		t.Fatalf("status moved with row count: %+v vs %+v", l1.status, l20.status)
	}
}

// TestMainSettingsAffordance proves the main view gains only the Settings
// link: the 6 existing buttons (Add/Remove/Scan/Refresh/Report/Close) keep
// their table shape and the header link sits inside the client without
// overlapping any existing rect.
func TestMainSettingsAffordance(t *testing.T) {
	oldDirs := uiDirs
	uiDirs = []string{`C:\Media\Movies`, `D:\Shows`}
	defer func() { uiDirs = oldDirs }()

	l := computeLayout(mainClientW, mainClientH, 96)
	if len(l.btns) != 6 {
		t.Fatalf("main buttons = %d, want 6 (Settings lives outside the button table)", len(l.btns))
	}
	s := l.settings
	if s.right > mainClientW || s.bottom > mainClientH || s.left < 0 || s.top < 0 {
		t.Fatalf("settings rect %+v escapes the %dx%d client", s, mainClientW, mainClientH)
	}
	if s.right-s.left <= 0 || s.bottom-s.top <= 0 {
		t.Fatalf("settings rect %+v is empty", s)
	}
	// Dead strip: below the icon (y=52) and above the section label (y=92).
	if s.top < 52 || s.bottom > 92 {
		t.Fatalf("settings rect %+v leaves the header strip (want within y 52..92)", s)
	}
	overlaps := func(a, b rect) bool {
		return a.left < b.right && b.left < a.right && a.top < b.bottom && b.top < a.bottom
	}
	for _, b := range l.btns {
		if overlaps(s, b.rc) {
			t.Fatalf("settings rect %+v overlaps button %d %+v", s, b.id, b.rc)
		}
	}
	for i, r := range l.rows {
		if overlaps(s, r) {
			t.Fatalf("settings rect %+v overlaps row %d %+v", s, i, r)
		}
	}
	if overlaps(s, l.status) || overlaps(s, l.card) {
		t.Fatalf("settings rect %+v overlaps the card or status line", s)
	}
}

// saveViewState snapshots the settings-view globals so view tests restore
// them.
func saveViewState() uiView {
	return curView
}

func restoreViewState(v uiView) {
	curView = v
}

// TestViewSwitch proves the state machine: main -> settings -> back to main.
func TestViewSwitch(t *testing.T) {
	v := saveViewState()
	defer restoreViewState(v)

	curView = viewMain
	openSettings()
	if curView != viewSettings {
		t.Fatalf("view = %d, want settings", curView)
	}
	closeSettings()
	if curView != viewMain {
		t.Fatalf("view = %d, want main after Back", curView)
	}
}

// TestSettingsHitTest proves every settings control resolves: Back, the
// LIBRARY row (Clear metadata/Clear index/Export/Import), the STARTUP row
// (toggle/Open log folder), and the SCAN segments (covered in depth by
// TestSettingsScanHitTest) all hit-test back to their own ids, stay inside
// the client, and never overlap.
func TestSettingsHitTest(t *testing.T) {
	oldScan := scanSpeedState
	scanSpeedState = "balanced"
	defer func() { scanSpeedState = oldScan }()
	l := computeSettingsLayout(mainClientW, mainClientH, 96, false)
	if len(l.btns) != 10 {
		t.Fatalf("settings buttons = %d, want 10 (Back + Clear metadata/Clear index/Export/Import + Startup/OpenLog + 3 SCAN)", len(l.btns))
	}
	for _, id := range []int{btnBack,
		btnClearCache, btnClearIndex, btnExport, btnImport, btnStartup, btnOpenLog} {
		rc, ok := l.settingsBtn(id)
		if !ok {
			t.Fatalf("button id %d missing from settings layout", id)
		}
		if rc.right > mainClientW || rc.bottom > mainClientH || rc.left < 0 || rc.top < 0 {
			t.Fatalf("button %d rect %+v escapes the %dx%d client", id, rc, mainClientW, mainClientH)
		}
		if got := buttonAtPoint(l.btns, (rc.left+rc.right)/2, (rc.top+rc.bottom)/2); got != id {
			t.Fatalf("button %d center hits %d", id, got)
		}
	}
	for i := 0; i < len(l.btns); i++ {
		for j := i + 1; j < len(l.btns); j++ {
			a, b := l.btns[i].rc, l.btns[j].rc
			if a.left < b.right && b.left < a.right && a.top < b.bottom && b.top < a.bottom {
				t.Fatalf("settings buttons %d and %d overlap: %+v vs %+v", l.btns[i].id, l.btns[j].id, a, b)
			}
		}
	}
	// Startup state only flips the toggle checkbox label.
	off := computeSettingsLayout(mainClientW, mainClientH, 96, false)
	on := computeSettingsLayout(mainClientW, mainClientH, 96, true)
	offRc, _ := off.settingsBtn(btnStartup)
	onRc, _ := on.settingsBtn(btnStartup)
	if offRc != onRc {
		t.Fatalf("startup toggle rect moved with state: %+v vs %+v", offRc, onRc)
	}
	for _, b := range off.btns {
		if b.id == btnStartup && b.label != "[ ] Start with Windows" {
			t.Fatalf("startup off label = %q, want unchecked", b.label)
		}
	}
	for _, b := range on.btns {
		if b.id == btnStartup && b.label != "[x] Start with Windows" {
			t.Fatalf("startup on label = %q, want checked", b.label)
		}
	}
	// The LIBRARY/STARTUP rows sit stacked from the top: LIBRARY at 100,
	// STARTUP at 172, SCAN at 244.
	for _, id := range []int{btnClearCache, btnClearIndex, btnExport, btnImport} {
		rc, _ := l.settingsBtn(id)
		if rc.top < 100 || rc.bottom > 140 {
			t.Fatalf("library button %d rect %+v leaves its strip (want y 100..140)", id, rc)
		}
	}
	for _, id := range []int{btnStartup, btnOpenLog} {
		rc, _ := l.settingsBtn(id)
		if rc.top < 172 || rc.bottom > 212 {
			t.Fatalf("startup button %d rect %+v leaves its strip (want y 172..212)", id, rc)
		}
	}
}

// TestLayoutTextInvariants walks computeLayout + computeSettingsLayout at
// 96/120/144/192 DPI and asserts the structural layout contract behind the
// live UI-AUDIT: every painted text rect lies inside the client, has
// positive width and height, and no two text rects intersect (shared edges
// do not count). Row counts 0/2/many cover the empty, normal, and clamped
// card shapes.
func TestLayoutTextInvariants(t *testing.T) {
	oldDirs := uiDirs
	oldScan := scanSpeedState
	defer func() { uiDirs = oldDirs; scanSpeedState = oldScan }()
	scanSpeedState = "balanced"

	inside := func(cw, ch int32, r rect) bool {
		return r.left >= 0 && r.top >= 0 && r.right <= cw && r.bottom <= ch
	}
	for _, dpi := range []int{96, 120, 144, 192} {
		cw := scalePx(int32(dpi), mainClientW)
		ch := scalePx(int32(dpi), mainClientH)
		for _, n := range []int{0, 2, 20} {
			uiDirs = make([]string, n)
			for i := range uiDirs {
				uiDirs[i] = string(rune('A'+i%26)) + `:\Media`
			}
			l := computeLayout(cw, ch, dpi)
			rects := mainTextRects(cw, ch, dpi, l, n)
			for _, r := range rects {
				w, h := r.rc.right-r.rc.left, r.rc.bottom-r.rc.top
				if w <= 0 || h <= 0 {
					t.Fatalf("dpi %d dirs %d: text rect %s %+v has non-positive size", dpi, n, r.id, r.rc)
				}
				if !inside(cw, ch, r.rc) {
					t.Fatalf("dpi %d dirs %d: text rect %s %+v escapes the %dx%d client", dpi, n, r.id, r.rc, cw, ch)
				}
			}
			for i := 0; i < len(rects); i++ {
				for j := i + 1; j < len(rects); j++ {
					a, b := rects[i], rects[j]
					if rectsIntersect(a.rc, b.rc) {
						t.Fatalf("dpi %d dirs %d: text rects %s and %s intersect: %+v vs %+v",
							dpi, n, a.id, b.id, a.rc, b.rc)
					}
				}
			}
		}

		sl := computeSettingsLayout(cw, ch, dpi, false)
		srects := settingsTextRects(cw, ch, dpi, sl)
		for _, r := range srects {
			w, h := r.rc.right-r.rc.left, r.rc.bottom-r.rc.top
			if w <= 0 || h <= 0 {
				t.Fatalf("dpi %d settings: text rect %s %+v has non-positive size", dpi, r.id, r.rc)
			}
			if !inside(cw, ch, r.rc) {
				t.Fatalf("dpi %d settings: text rect %s %+v escapes the %dx%d client", dpi, r.id, r.rc, cw, ch)
			}
		}
		for i := 0; i < len(srects); i++ {
			for j := i + 1; j < len(srects); j++ {
				a, b := srects[i], srects[j]
				if rectsIntersect(a.rc, b.rc) {
					t.Fatalf("dpi %d settings: text rects %s and %s intersect: %+v vs %+v",
						dpi, a.id, b.id, a.rc, b.rc)
				}
			}
		}
	}
}

// TestSettingsEsc proves Esc in the settings view returns to main.
func TestSettingsEsc(t *testing.T) {
	v := saveViewState()
	defer restoreViewState(v)

	curView = viewSettings
	if !onSettingsKeyDown(0, vkEscape) || curView != viewMain {
		t.Fatalf("Esc: view = %d, want main", curView)
	}
	curView = viewMain
	if onSettingsKeyDown(0, vkEscape) {
		t.Fatalf("Esc in main view handled, want ignored")
	}
}

// TestFocusOrderCoversRectTable proves each view's keyboard focus order
// covers exactly its rect table: main = 6 buttons + Settings + Card (8),
// settings = 10 (Back + 4 LIBRARY + 2 STARTUP + 3 SCAN), card = 9 (Back + 4
// Details + 4 State), with no duplicates.
func TestFocusOrderCoversRectTable(t *testing.T) {
	orders := map[uiView][]int{
		viewMain:     mainFocusOrder(),
		viewSettings: settingsFocusOrder(),
		viewCard:     cardFocusOrder(),
	}
	wantLen := map[uiView]int{viewMain: 8, viewSettings: 10, viewCard: 9}
	for v, order := range orders {
		if len(order) != wantLen[v] {
			t.Fatalf("view %d focus order = %d stops, want %d", v, len(order), wantLen[v])
		}
		seen := map[int]bool{}
		for _, id := range order {
			if seen[id] {
				t.Fatalf("view %d focus order duplicates id %d", v, id)
			}
			seen[id] = true
		}
		if got := focusOrderForView(v); len(got) != len(order) {
			t.Fatalf("view %d focusOrderForView = %d stops, want %d", v, len(got), len(order))
		}
	}
	for _, id := range []int{btnAdd, btnRemove, btnScan, btnRefresh, btnReport, btnClose, btnSettings, btnCard} {
		found := false
		for _, got := range mainFocusOrder() {
			if got == id {
				found = true
			}
		}
		if !found {
			t.Fatalf("main focus order misses rect-table id %d", id)
		}
	}
}

// TestMoveFocusWraps proves the focus step: an unknown position starts at
// the first (forward) or last (backward) stop, and stepping wraps both ways.
func TestMoveFocusWraps(t *testing.T) {
	order := []int{btnAdd, btnRemove, btnScan}
	if got := moveFocus(0, 1, order); got != btnAdd {
		t.Fatalf("moveFocus(none,+1) = %d, want %d (first stop)", got, btnAdd)
	}
	if got := moveFocus(0, -1, order); got != btnScan {
		t.Fatalf("moveFocus(none,-1) = %d, want %d (last stop)", got, btnScan)
	}
	if got := moveFocus(btnScan, 1, order); got != btnAdd {
		t.Fatalf("moveFocus(last,+1) = %d, want %d (wrap)", got, btnAdd)
	}
	if got := moveFocus(btnAdd, -1, order); got != btnScan {
		t.Fatalf("moveFocus(first,-1) = %d, want %d (wrap)", got, btnScan)
	}
	if got := moveFocus(btnAdd, 1, order); got != btnRemove {
		t.Fatalf("moveFocus(Add,+1) = %d, want %d", got, btnRemove)
	}
	if got := moveFocus(0, 1, nil); got != 0 {
		t.Fatalf("moveFocus over empty order = %d, want 0", got)
	}
}

// TestFocusTabCycle proves Tab/Shift+Tab walk the main order and the arrows
// step it, while Esc in the main view stays ignored.
func TestFocusTabCycle(t *testing.T) {
	v, f := curView, focusID
	defer func() { curView, focusID = v, f }()

	curView, focusID = viewMain, 0
	if !onSettingsKeyDownEx(0, vkTab, false) || focusID != btnAdd {
		t.Fatalf("Tab: focus = %d, want %d (first stop)", focusID, btnAdd)
	}
	if !onSettingsKeyDownEx(0, vkTab, false) || focusID != btnRemove {
		t.Fatalf("Tab x2: focus = %d, want %d", focusID, btnRemove)
	}
	if !onSettingsKeyDownEx(0, vkTab, true) || focusID != btnAdd {
		t.Fatalf("Shift+Tab: focus = %d, want %d", focusID, btnAdd)
	}
	if !onSettingsKeyDownEx(0, vkDown, false) || focusID != btnRemove {
		t.Fatalf("Down: focus = %d, want %d", focusID, btnRemove)
	}
	if !onSettingsKeyDownEx(0, vkUp, false) || focusID != btnAdd {
		t.Fatalf("Up: focus = %d, want %d", focusID, btnAdd)
	}
	if onSettingsKeyDownEx(0, vkEscape, false) {
		t.Fatalf("Esc in main view handled, want ignored")
	}
}

// TestFocusActivateHonoursDisabled proves a disabled control reports instead
// of acting: Refresh while a scan runs lands the busy reason and the run is
// untouched (still active, nothing started).
func TestFocusActivateHonoursDisabled(t *testing.T) {
	v, f, a, s := curView, focusID, scanActive, statusText
	defer func() { curView, focusID, scanActive, statusText = v, f, a, s }()

	curView, scanActive, focusID = viewMain, true, btnRefresh
	if !onSettingsKeyDownEx(0, vkReturn, false) {
		t.Fatalf("Enter on focused Refresh ignored, want activation")
	}
	if statusText != scanBusyText {
		t.Fatalf("disabled Refresh status = %q, want %q", statusText, scanBusyText)
	}
	if !scanActive {
		t.Fatalf("disabled Refresh disturbed the run, want it still active")
	}
}

// TestRemoveWithoutSelectionFeedback proves Remove with nothing selected
// lands the status hint instead of silently no-op-ing.
func TestRemoveWithoutSelectionFeedback(t *testing.T) {
	d, sr, hr, s := uiDirs, selRow, hoverRow, statusText
	defer func() { uiDirs, selRow, hoverRow, statusText = d, sr, hr, s }()

	uiDirs = []string{`C:\Media\Movies`}
	selRow, hoverRow = -1, -1
	removeDirAt(-1)
	if statusText != "Select a folder first." {
		t.Fatalf("Remove-without-selection status = %q, want the hint", statusText)
	}
	if len(uiDirs) != 1 {
		t.Fatalf("Remove-without-selection dropped a row, want no change")
	}
}

// TestScanBlocksEdits proves the mid-scan disabled set: Add/Remove/Refresh
// report the busy reason, Scan (Cancel) and Close stay free, and idle blocks
// nothing.
func TestScanBlocksEdits(t *testing.T) {
	a := scanActive
	defer func() { scanActive = a }()

	scanActive = true
	for _, id := range []int{btnAdd, btnRemove, btnRefresh} {
		if got := scanBlockedReason(id); got != scanBusyText {
			t.Fatalf("mid-scan id %d reason = %q, want %q", id, got, scanBusyText)
		}
	}
	for _, id := range []int{btnScan, btnClose, btnSettings, btnCard} {
		if got := scanBlockedReason(id); got != "" {
			t.Fatalf("mid-scan id %d blocked (%q), want free", id, got)
		}
	}
	scanActive = false
	for _, id := range []int{btnAdd, btnRemove, btnRefresh, btnScan, btnClose} {
		if got := scanBlockedReason(id); got != "" {
			t.Fatalf("idle id %d blocked (%q), want free", id, got)
		}
	}
}

// TestRemoveDimNeedsTarget proves the Remove dim model: the selected or
// hovered row counts as a target, stale indices beyond the list do not.
func TestRemoveDimNeedsTarget(t *testing.T) {
	d, sr, hr := uiDirs, selRow, hoverRow
	defer func() { uiDirs, selRow, hoverRow = d, sr, hr }()

	uiDirs = []string{`C:\Media\Movies`}
	selRow, hoverRow = -1, -1
	if removeHasTarget() {
		t.Fatalf("removeHasTarget with no selection = true, want false (dim)")
	}
	selRow = 0
	if !removeHasTarget() {
		t.Fatalf("removeHasTarget with row selected = false, want true")
	}
	selRow, hoverRow = -1, 0
	if !removeHasTarget() {
		t.Fatalf("removeHasTarget with row hovered = false, want true")
	}
	uiDirs = nil
	selRow, hoverRow = 0, 0
	if removeHasTarget() {
		t.Fatalf("removeHasTarget with stale indices = true, want false")
	}
}

// TestAuditSingleLineVerdict proves the audit criterion: a fitting extent
// is silent, a deliberately-ellipsized draw that does not fit is
// informational ("trimmed", never a violation), and a draw WITHOUT an
// ellipsis flag that does not fit is still a violation ("overflow") - the
// positive control the gate must keep catching.
func TestAuditSingleLineVerdict(t *testing.T) {
	if got := auditSingleLineVerdict(100, 15, 420, 20, true); got != "" {
		t.Fatalf("fitting ellipsized verdict = %q, want silent", got)
	}
	if got := auditSingleLineVerdict(100, 15, 420, 20, false); got != "" {
		t.Fatalf("fitting plain verdict = %q, want silent", got)
	}
	// Deliberately ellipsized text that does not fit: informational only.
	if got := auditSingleLineVerdict(449, 15, 420, 20, true); got != "trimmed" {
		t.Fatalf("ellipsized overflow verdict = %q, want trimmed", got)
	}
	// Positive control: NO ellipsis flag into a too-small rect still fires.
	if got := auditSingleLineVerdict(449, 15, 420, 20, false); got != "overflow" {
		t.Fatalf("plain overflow verdict = %q, want overflow (gate must fire)", got)
	}
	// Height overflow follows the same split.
	if got := auditSingleLineVerdict(100, 30, 420, 20, true); got != "trimmed" {
		t.Fatalf("ellipsized height verdict = %q, want trimmed", got)
	}
	if got := auditSingleLineVerdict(100, 30, 420, 20, false); got != "overflow" {
		t.Fatalf("plain height verdict = %q, want overflow (gate must fire)", got)
	}
}

// TestScanBarModelWalkResolve proves the strip presentation the paint path
// renders: idle shows nothing, the walk (no key total yet) sweeps
// indeterminate, a partial total is determinate at the honest fraction, and
// a complete (or over-complete, clamped) total is full - never a fake
// percent. paintScanBar renders exactly this model.
func TestScanBarModelWalkResolve(t *testing.T) {
	if show, det, _ := scanBarModel(false, 0, 0); show || det {
		t.Fatalf("idle bar = (%v, %v), want hidden", show, det)
	}
	if show, det, _ := scanBarModel(true, 0, 0); !show || det {
		t.Fatalf("walk bar = (%v, %v), want shown + sweeping", show, det)
	}
	if show, det, frac := scanBarModel(true, 12, 24); !show || !det || frac != 0.5 {
		t.Fatalf("resolve bar = (%v, %v, %v), want shown + determinate 0.5", show, det, frac)
	}
	if show, det, frac := scanBarModel(true, 24, 24); !show || !det || frac != 1 {
		t.Fatalf("done bar = (%v, %v, %v), want shown + determinate 1", show, det, frac)
	}
	if show, det, frac := scanBarModel(true, 30, 24); !show || !det || frac != 1 {
		t.Fatalf("over-done bar = (%v, %v, %v), want clamped to 1", show, det, frac)
	}
}

// TestScanKeyFraction proves the determinate fraction: 0/total is empty,
// mid is proportional, total (and beyond, clamped) is full, and a missing
// total refuses a fraction so the walk phase never shows a fake percent.
func TestScanKeyFraction(t *testing.T) {
	cases := []struct {
		done, total int
		want        float64
		ok          bool
	}{
		{0, 10, 0, true},
		{5, 10, 0.5, true},
		{10, 10, 1, true},
		{15, 10, 1, true},
		{-2, 10, 0, true},
		{5, 0, 0, false},
		{5, -3, 0, false},
	}
	for _, c := range cases {
		got, ok := scanKeyFraction(c.done, c.total)
		if ok != c.ok || (ok && got != c.want) {
			t.Fatalf("scanKeyFraction(%d, %d) = (%v, %v), want (%v, %v)",
				c.done, c.total, got, ok, c.want, c.ok)
		}
	}
}

// TestScanLifecycleThrottle proves coalescing through the same transition
// production calls: progress/tick bursts every 10 ms over one second admit
// at most 10 repaints (the latest snapshot wins each window).
func TestScanLifecycleThrottle(t *testing.T) {
	t0 := time.Now()
	st, fx := scanLifecycleNext(scanLifecycle{}, scanEvStart, t0)
	if !fx.arm || st.lastPaint != t0 {
		t.Fatalf("start = (%+v, %+v), want arm + clock reset to now", st, fx)
	}
	last := st
	admitted := 0
	for i := 1; i <= 100; i++ {
		now := t0.Add(time.Duration(i*10) * time.Millisecond)
		// Mirror refreshScanLiveText: state advances only on admit, so the
		// gate measures real repaint spacing.
		next, fx := scanLifecycleNext(last, scanEvProgress, now)
		if fx.paint {
			admitted++
			last = next
		}
	}
	if admitted > 10 {
		t.Fatalf("throttle admitted %d repaints/s, want <= 10", admitted)
	}
	if admitted == 0 {
		t.Fatalf("throttle admitted nothing, want ~10 (gate must not stall)")
	}
}

// TestScanLifecycleArmAndReset proves a fresh Start arms the repaint tick,
// re-originates the throttle clock through the returned state, and paints
// the opening line: dropping registration or the per-scan clock reset from
// the transition fails here.
func TestScanLifecycleArmAndReset(t *testing.T) {
	now := time.Now()
	old := now.Add(-time.Hour)
	st, fx := scanLifecycleNext(scanLifecycle{lastPaint: old}, scanEvStart, now)
	if !st.active {
		t.Fatalf("start active = false, want true")
	}
	if !fx.arm {
		t.Fatalf("start arm = false, want true (timer registration)")
	}
	if !st.lastPaint.Equal(now) {
		t.Fatalf("start lastPaint = %v, want reset to %v (throttle clock re-origin)", st.lastPaint, now)
	}
	if !fx.paint {
		t.Fatalf("start paint = false, want true (opening line)")
	}
	if fx.kill {
		t.Fatalf("start kill = true, want false")
	}
}

// TestScanLifecycleOneScanAtATime proves Start while a run is active is a
// no-op: no second tick, no throttle reset, no repaint, state untouched.
func TestScanLifecycleOneScanAtATime(t *testing.T) {
	now := time.Now()
	running := scanLifecycle{active: true, lastPaint: now.Add(-time.Second)}
	next, fx := scanLifecycleNext(running, scanEvStart, now)
	if fx != (scanFx{}) {
		t.Fatalf("second start fx = %+v, want zero (no second run)", fx)
	}
	if next != running {
		t.Fatalf("second start state = %+v, want untouched %+v", next, running)
	}
}

// TestScanLifecycleTickGate proves Progress and Tick share one gate: an
// immediate follow-up paints nothing and advances nothing, one interval
// later paints and advances, and an idle tick is silent.
func TestScanLifecycleTickGate(t *testing.T) {
	t0 := time.Now()
	st, _ := scanLifecycleNext(scanLifecycle{}, scanEvStart, t0)
	if next, fx := scanLifecycleNext(st, scanEvTick, t0); fx.paint || next.lastPaint != t0 {
		t.Fatalf("same-instant tick = (%+v, %+v), want gated out", next, fx)
	}
	later := t0.Add(scanPaintInterval)
	next, fx := scanLifecycleNext(st, scanEvProgress, later)
	if !fx.paint || !next.lastPaint.Equal(later) {
		t.Fatalf("interval progress = (%+v, %+v), want paint + clock advance", next, fx)
	}
	if _, fx := scanLifecycleNext(scanLifecycle{}, scanEvTick, later); fx != (scanFx{}) {
		t.Fatalf("idle tick fx = %+v, want zero", fx)
	}
	if !scanPaintGate(t0, t0.Add(-time.Second)) {
		t.Fatalf("backward-clock gate = false, want true (paint rather than stall)")
	}
}

// TestScanLifecycleTerminalPaint proves the terminal path production passes:
// Done for a real run always kills the tick and always lands the terminal
// line (even at the same instant, even with the clock stepped back); an
// idle Done still kills defensively but lands nothing.
func TestScanLifecycleTerminalPaint(t *testing.T) {
	now := time.Now()
	st, _ := scanLifecycleNext(scanLifecycle{}, scanEvStart, now)
	next, fx := scanLifecycleNext(st, scanEvDone, now)
	if !fx.kill {
		t.Fatalf("done kill = false, want true (timer cleanup)")
	}
	if !fx.terminal {
		t.Fatalf("done terminal = false, want true (terminal line always lands)")
	}
	if fx.paint {
		t.Fatalf("done paint = true, want false (live repaints stay gated)")
	}
	if next.active {
		t.Fatalf("done active = true, want false")
	}
	back, fxBack := scanLifecycleNext(st, scanEvDone, now.Add(-time.Hour))
	if !fxBack.kill || !fxBack.terminal || back.active {
		t.Fatalf("backward-clock done = (%+v, %+v), want kill + terminal + idle", back, fxBack)
	}
	if _, fx := scanLifecycleNext(scanLifecycle{}, scanEvDone, now); !fx.kill || fx.terminal {
		t.Fatalf("idle done fx = %+v, want kill-only (no tick survives, nothing to land)", fx)
	}
}

// saveScanRunState snapshots the scan-run globals the start/done-path tests
// mutate, so each restores them.
func saveScanRunState() (active bool, cancel context.CancelFunc, start, lastPaint, resolve time.Time, base int, force bool, status string, progress library.Progress) {
	scanMu.Lock()
	progress, force = scanProgress, scanForce
	scanMu.Unlock()
	return scanActive, scanCancel, scanStart, scanLastPaint, scanResolveStart, scanResolveBase, force, statusText, progress
}

func restoreScanRunState(active bool, cancel context.CancelFunc, start, lastPaint, resolve time.Time, base int, force bool, status string, progress library.Progress) {
	if scanCancel != nil {
		scanCancel()
	}
	scanActive, scanCancel = active, cancel
	scanStart, scanLastPaint = start, lastPaint
	scanResolveStart, scanResolveBase = resolve, base
	scanMu.Lock()
	scanProgress, scanForce = progress, force
	scanMu.Unlock()
	statusText = status
}

// TestScanStartClearsStaleProgress proves the REAL start path clears a stale
// run: a completed 24/24 snapshot plus a resolve origin and baseline go in,
// beginScanRun (the UI-thread half startLibraryScan calls) zeroes all three.
// Deleting the reset from beginScanRun fails here.
func TestScanStartClearsStaleProgress(t *testing.T) {
	a, c, st, lp, rs, rb, f, s, p := saveScanRunState()
	defer restoreScanRunState(a, c, st, lp, rs, rb, f, s, p)
	oldArm := scanArmTimer
	scanArmTimer = func(uintptr) {}
	defer func() { scanArmTimer = oldArm }()

	scanMu.Lock()
	scanProgress = library.Progress{KeysDone: 24, KeysTotal: 24, KeysFetched: 4, KeysCached: 20, FoldersScanned: 6}
	scanMu.Unlock()
	scanResolveStart = time.Now().Add(-time.Minute)
	scanResolveBase = 20
	if _, ok := beginScanRun(0, false, time.Now()); !ok {
		t.Fatalf("beginScanRun while idle = false, want a run")
	}
	scanMu.Lock()
	got := scanProgress
	scanMu.Unlock()
	if got != (library.Progress{}) {
		t.Fatalf("start snapshot = %+v, want zero (stale bar/counters survive)", got)
	}
	if !scanResolveStart.IsZero() {
		t.Fatalf("start resolve clock = %v, want zero", scanResolveStart)
	}
	if scanResolveBase != 0 {
		t.Fatalf("start resolve baseline = %d, want zero", scanResolveBase)
	}
}

// TestScanStartAppliesEffects proves beginScanRun consumes every start effect
// the lifecycle emits: the timer arm lands (the stub observes it), the
// throttle clock re-originates, and the opening line paints - and a second
// start while active begins nothing (no second arm, no repaint, no clock
// touch). Deleting the arm, the reset, or the status write from
// beginScanRun fails here.
func TestScanStartAppliesEffects(t *testing.T) {
	a, c, st, lp, rs, rb, f, s, p := saveScanRunState()
	defer restoreScanRunState(a, c, st, lp, rs, rb, f, s, p)
	arms := 0
	oldArm := scanArmTimer
	scanArmTimer = func(uintptr) { arms++ }
	defer func() { scanArmTimer = oldArm }()

	scanLastPaint = time.Now().Add(-time.Hour) // stale clock
	now := time.Now()
	if _, ok := beginScanRun(0, false, now); !ok {
		t.Fatalf("beginScanRun while idle = false, want a run")
	}
	if !scanActive {
		t.Fatalf("start active = false, want true")
	}
	if arms != 1 {
		t.Fatalf("timer arms = %d, want 1 (SetTimer registration)", arms)
	}
	if !scanLastPaint.Equal(now) {
		t.Fatalf("start lastPaint = %v, want reset to %v", scanLastPaint, now)
	}
	if statusText != "Scan starting..." {
		t.Fatalf("start status = %q, want the opening line", statusText)
	}
	scanMu.Lock()
	force := scanForce
	scanMu.Unlock()
	if force {
		t.Fatalf("scan force flag = true, want false (Scan, not Refresh)")
	}

	// A second start while the run is active begins nothing.
	second := now.Add(time.Second)
	if _, ok := beginScanRun(0, true, second); ok {
		t.Fatalf("second beginScanRun = true, want false (one scan at a time)")
	}
	if arms != 1 {
		t.Fatalf("timer arms after second start = %d, want still 1", arms)
	}
	if !scanLastPaint.Equal(now) {
		t.Fatalf("second start touched the clock (%v), want it untouched", scanLastPaint)
	}
	if statusText != "Scan starting..." {
		t.Fatalf("second start status = %q, want the first opening line kept", statusText)
	}

	// Refresh maps the same path to its own opening line and force flag.
	restoreScanRunState(a, c, st, lp, rs, rb, f, s, p)
	oldArm2 := scanArmTimer
	scanArmTimer = func(uintptr) { arms++ }
	defer func() { scanArmTimer = oldArm2 }()
	if _, ok := beginScanRun(0, true, now); !ok {
		t.Fatalf("refresh beginScanRun while idle = false, want a run")
	}
	if statusText != "Refresh starting..." {
		t.Fatalf("refresh status = %q, want the Refresh opening line", statusText)
	}
	scanMu.Lock()
	force = scanForce
	scanMu.Unlock()
	if !force {
		t.Fatalf("refresh force flag = false, want true")
	}
}

// TestRefreshScanLiveTextAppliesEffects drives the REAL live-repaint path:
// refreshScanLiveText advances the throttle clock, captures the resolve
// origin and baseline on the first totalled snapshot, and writes the
// composed status line. Deleting the clock update, the baseline capture, or
// the status write from refreshScanLiveText fails here. A gated-out tick
// writes nothing, and a cancel-pending run keeps the cancelled summary until
// the done message lands.
func TestRefreshScanLiveTextAppliesEffects(t *testing.T) {
	a, c, st, lp, rs, rb, f, s, p := saveScanRunState()
	defer restoreScanRunState(a, c, st, lp, rs, rb, f, s, p)

	now := time.Now()
	scanActive = true
	_, sc := context.WithCancel(context.Background())
	scanCancel = sc // a live run: no cancel pending (else the summary owns the line)
	scanStart = now.Add(-30 * time.Second)
	scanLastPaint = now.Add(-time.Hour) // stale clock: the gate admits
	scanResolveStart = time.Time{}      // no origin yet: the baseline captures
	scanResolveBase = 0
	snap := library.Progress{KeysDone: 120, KeysTotal: 1000, KeysFetched: 90, KeysCached: 20, KeysFailed: 10, FoldersScanned: 9, FoldersSkipped: 1}
	scanMu.Lock()
	scanProgress = snap
	scanMu.Unlock()
	statusText = "SENTINEL"

	refreshScanLiveText(0, scanEvProgress, now)

	// The throttle clock advances: deleting the clock update fails here.
	if !scanLastPaint.Equal(now) {
		t.Fatalf("live lastPaint = %v, want %v (throttle clock update)", scanLastPaint, now)
	}
	// The resolve origin opens on the first totalled snapshot with the
	// observed completions as baseline: deleting the capture fails here.
	if !scanResolveStart.Equal(now) {
		t.Fatalf("live resolve origin = %v, want %v (baseline capture)", scanResolveStart, now)
	}
	if want := scanObservedDone(snap); scanResolveBase != want {
		t.Fatalf("live resolve baseline = %d, want %d (observed completions)", scanResolveBase, want)
	}
	// The composed line lands: deleting the status write fails here.
	wantText := scanLiveStatusText(snap, now.Sub(scanStart), scanResolveElapsed(now, now), scanObservedDone(snap), windowDpi(0), liveStatusAvailPx(0))
	if statusText != wantText {
		t.Fatalf("live status = %q, want %q (composed line write)", statusText, wantText)
	}
	if !strings.HasPrefix(statusText, "Scan ") {
		t.Fatalf("live status = %q, want the counters line", statusText)
	}

	// A gated-out tick at the same instant advances nothing and writes
	// nothing: the latest snapshot wins the next admitted window.
	statusText = "SENTINEL2"
	scanMu.Lock()
	scanProgress = library.Progress{KeysDone: 121, KeysTotal: 1000, KeysFetched: 91, KeysCached: 20, KeysFailed: 10, FoldersScanned: 9, FoldersSkipped: 1}
	scanMu.Unlock()
	refreshScanLiveText(0, scanEvTick, now)
	if statusText != "SENTINEL2" {
		t.Fatalf("gated tick status = %q, want untouched (coalescing gate)", statusText)
	}
	if !scanLastPaint.Equal(now) {
		t.Fatalf("gated tick lastPaint = %v, want still %v", scanLastPaint, now)
	}

	// Cancel pending: the cancelled summary owns the line until done lands.
	scanCancel = nil
	statusText = "SENTINEL3"
	refreshScanLiveText(0, scanEvProgress, now.Add(10*time.Second))
	if statusText != "SENTINEL3" {
		t.Fatalf("cancel-pending status = %q, want untouched (summary owns the line)", statusText)
	}
}

// TestScanTerminalArgParity drives the REAL done path: onScanDone lands the
// worker's result with the exact arguments production passes - the kill
// effect stops the tick, the terminal line is scanTerminalText(force, stats,
// err) byte-for-byte, and the run deactivates. Corrupting any argument
// onScanDone passes, or dropping the kill, fails here. A stray done while
// idle still kills defensively but leaves the line untouched.
func TestScanTerminalArgParity(t *testing.T) {
	a, c, st, lp, rs, rb, f, s, p := saveScanRunState()
	defer restoreScanRunState(a, c, st, lp, rs, rb, f, s, p)
	kills := 0
	oldKill := scanKillTimer
	scanKillTimer = func(uintptr) { kills++ }
	defer func() { scanKillTimer = oldKill }()

	land := func(force bool, stats library.Stats, err error) {
		scanMu.Lock()
		scanStats, scanErr, scanForce = stats, err, force
		scanMu.Unlock()
		scanActive = true
		_, sc := context.WithCancel(context.Background())
		scanCancel = sc
		onScanDone(0)
	}

	ok := library.Stats{Keys: 24, KeysDone: 24, KeysCached: 5, KeysFetched: 2, KeysNotFound: 17, Files: 10, FoldersScanned: 4, FoldersSkipped: 2}
	land(false, ok, nil)
	if statusText != scanDoneText("Scan", ok) {
		t.Fatalf("scan terminal = %q, want %q", statusText, scanDoneText("Scan", ok))
	}
	land(true, ok, nil)
	if statusText != scanDoneText("Refresh", ok) {
		t.Fatalf("refresh terminal = %q, want %q", statusText, scanDoneText("Refresh", ok))
	}
	if kills != 2 {
		t.Fatalf("timer kills = %d, want 2 (one per landed run)", kills)
	}
	if scanActive {
		t.Fatalf("done active = true, want false")
	}
	if scanCancel != nil {
		t.Fatalf("done left a live cancel func, want it cleared")
	}

	cancelled := library.Stats{Keys: 24, KeysDone: 12, Cancelled: true}
	land(false, cancelled, errors.New("boom"))
	if statusText != scanCancelledText(12, 24) {
		t.Fatalf("cancelled terminal = %q, want the kept-progress line", statusText)
	}
	bare := library.Stats{Keys: 10, KeysDone: 4}
	land(true, bare, context.Canceled)
	if statusText != scanCancelledText(4, 10) {
		t.Fatalf("context-cancelled refresh terminal = %q, want the kept-progress line", statusText)
	}
	land(false, ok, errors.New("disk gone"))
	if !strings.Contains(statusText, "Scan failed:") {
		t.Fatalf("error terminal = %q, want the Scan failure line", statusText)
	}
	land(true, ok, errors.New("disk gone"))
	if !strings.Contains(statusText, "Refresh failed:") {
		t.Fatalf("error refresh terminal = %q, want the Refresh failure line", statusText)
	}

	// Stray done with no run: defensive kill, line untouched.
	kills = 0
	statusText = "keep me"
	scanActive = false
	onScanDone(0)
	if kills != 1 {
		t.Fatalf("stray-done kills = %d, want 1 (no tick survives)", kills)
	}
	if statusText != "keep me" {
		t.Fatalf("stray-done status = %q, want the line untouched", statusText)
	}
}

// TestScanETADefensible proves the ETA shows only on a rate actually
// observed since the resolve origin opened: the walk (no total), nothing
// done, too few observed keys, too little resolve time, and completion all
// suppress it. Cache-precredited keys never feed the rate: 60 s of walking
// followed by 990 instant cache hits must show elapsed alone, never
// "0.6s remaining". And the origin baseline excludes pre-interval work: 100
// completions banked before the origin followed by silence must also show
// elapsed alone, never an 18 s fantasy.
func TestScanETADefensible(t *testing.T) {
	suppressed := []struct {
		name    string
		p       library.Progress
		resolve time.Duration
	}{
		{"walk phase", library.Progress{}, 30 * time.Second},
		{"nothing done", library.Progress{KeysTotal: 1000}, 30 * time.Second},
		{"too few observed", library.Progress{KeysDone: 3, KeysTotal: 1000, KeysFetched: 3}, 30 * time.Second},
		{"cache burst, no observed work", library.Progress{KeysDone: 990, KeysTotal: 1000, KeysCached: 990}, 30 * time.Second},
		{"cache burst, short resolve clock", library.Progress{KeysDone: 995, KeysTotal: 1000, KeysCached: 990, KeysFetched: 5}, time.Second},
		{"too little resolve elapsed", library.Progress{KeysDone: 100, KeysTotal: 1000, KeysFetched: 100}, time.Second},
		{"complete", library.Progress{KeysDone: 1000, KeysTotal: 1000, KeysFetched: 900, KeysCached: 100}, 30 * time.Second},
	}
	for _, c := range suppressed {
		if eta, ok := scanETA(c.p, 0, c.resolve); ok {
			t.Fatalf("scanETA %s = (%v, true), want suppressed", c.name, eta)
		}
	}
	// Pre-origin work never feeds the rate: 100 completions already banked
	// when the origin opens (baseline 100), then 20 s of no further
	// completions, must stay suppressed.
	pre := library.Progress{KeysDone: 100, KeysTotal: 1000, KeysFetched: 100}
	if eta, ok := scanETA(pre, 100, 20*time.Second); ok {
		t.Fatalf("scanETA pre-origin work = (%v, true), want suppressed (elapsed alone)", eta)
	}
	// Steady resolve work projects from the observed delta alone: 100
	// observed in 20 s of resolve with 900 left -> 180 s.
	steady := library.Progress{KeysDone: 100, KeysTotal: 1000, KeysFetched: 100}
	if eta, ok := scanETA(steady, 0, 20*time.Second); !ok || eta != 180*time.Second {
		t.Fatalf("scanETA steady = (%v, %v), want (3m0s, true)", eta, ok)
	}
	// The rate divides only the delta: baseline 100, now 200 observed in
	// 20 s with 800 left -> 160 s, not 20*800/200.
	delta := library.Progress{KeysDone: 200, KeysTotal: 1000, KeysFetched: 200}
	if eta, ok := scanETA(delta, 100, 20*time.Second); !ok || eta != 160*time.Second {
		t.Fatalf("scanETA delta = (%v, %v), want (2m40s, true)", eta, ok)
	}
	// Cached keys do not feed the rate: 600 done but only 100 observed in
	// 20 s with 400 left -> 80 s, not 20*400/600.
	mixed := library.Progress{KeysDone: 600, KeysTotal: 1000, KeysFetched: 100, KeysCached: 500}
	if eta, ok := scanETA(mixed, 0, 20*time.Second); !ok || eta != 80*time.Second {
		t.Fatalf("scanETA mixed = (%v, %v), want (1m20s, true)", eta, ok)
	}
	// Half observed, half left -> the resolve elapsed again.
	half := library.Progress{KeysDone: 500, KeysTotal: 1000, KeysFetched: 500}
	if eta, ok := scanETA(half, 0, 10*time.Second); !ok || eta != 10*time.Second {
		t.Fatalf("scanETA half = (%v, %v), want (10s, true)", eta, ok)
	}
}

// TestFormatScanDuration proves the compact elapsed clock.
func TestFormatScanDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{-time.Second, "0s"},
		{0, "0s"},
		{5 * time.Second, "5s"},
		{65 * time.Second, "1m05s"},
		{3720 * time.Second, "1h02m"},
	}
	for _, c := range cases {
		if got := formatScanDuration(c.d); got != c.want {
			t.Fatalf("formatScanDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// TestScanLiveStatusText proves the live line carries elapsed always, an
// ETA only when the resolve counters defend one, and still fits the status
// cap with the folder counts intact even for large-but-reachable counters.
// The ETA logic is pinned at the parts level (deterministic); whether the
// ETA suffix survives composition is a measured fit, pinned per-DPI by
// TestScanLiveStatusFitsAtAllDPI.
func TestScanLiveStatusText(t *testing.T) {
	p := library.Progress{KeysDone: 12, KeysTotal: 24, KeysFetched: 7, KeysCached: 5, FoldersScanned: 4, FoldersSkipped: 2}
	avail := scanStatusAvailPx(96)
	base, _, earlyETA := scanLiveStatusParts(p, 500*time.Millisecond, 500*time.Millisecond, 0)
	if earlyETA != "" {
		t.Fatalf("early parts eta = %q, want none before a rate exists", earlyETA)
	}
	early := scanLiveStatusText(p, 500*time.Millisecond, 500*time.Millisecond, 0, 96, avail)
	if !strings.Contains(early, " | 0s") {
		t.Fatalf("early live text = %q, want elapsed suffix", early)
	}
	if strings.Contains(early, "eta") {
		t.Fatalf("early live text = %q, want no ETA before a rate exists", early)
	}
	_, _, steadyETA := scanLiveStatusParts(p, 20*time.Second, 20*time.Second, 0)
	if steadyETA == "" {
		t.Fatalf("steady parts eta empty, want an estimate once the rate is defensible")
	}
	steady := scanLiveStatusText(p, 20*time.Second, 20*time.Second, 0, 96, avail)
	if !strings.Contains(steady, base) {
		t.Fatalf("steady live text = %q, want the counters intact (never truncated)", steady)
	}
	if !strings.Contains(steady, "eta") {
		t.Fatalf("steady live text = %q, want ETA once the rate is defensible and fits", steady)
	}
	walk := scanLiveStatusText(library.Progress{}, 30*time.Second, 0, 0, 96, avail)
	if strings.Contains(walk, "eta") {
		t.Fatalf("walk live text = %q, want elapsed alone (no total, no ETA)", walk)
	}
	// The review's regression: 60 s of walking then 990 instant cache hits
	// must show elapsed alone at any resolve clock that cannot defend a
	// rate - never "0.6s remaining".
	burst := library.Progress{KeysDone: 990, KeysTotal: 1000, KeysCached: 990}
	if got := scanLiveStatusText(burst, 65*time.Second, time.Second, 0, 96, avail); strings.Contains(got, "eta") {
		t.Fatalf("cache-burst live text = %q, want elapsed alone", got)
	}
	if got := scanLiveStatusText(burst, 90*time.Second, 30*time.Second, 0, 96, avail); strings.Contains(got, "eta") {
		t.Fatalf("cache-only live text = %q, want elapsed alone (no observed work)", got)
	}
	// Reachable large-scale boundary (buckets sum exactly to done, the live
	// invariant production maintains): the composed line keeps the folder
	// counts and stays within the char cap, abbreviating before dropping.
	big := library.Progress{KeysDone: 45000, KeysTotal: 90000, KeysFetched: 15000, KeysCached: 15000,
		KeysFailed: 15000, FoldersScanned: 450, FoldersSkipped: 32}
	got := scanLiveStatusText(big, time.Hour, time.Hour, 15000, 96, avail)
	if len(got) > maxScanStatusLen {
		t.Fatalf("big live text len=%d > %d: %q", len(got), maxScanStatusLen, got)
	}
	if !strings.Contains(got, "folders") {
		t.Fatalf("big live text lost folder counts: %q", got)
	}
}

// TestComposeScanLiveStatusPriority proves composition walks full -> short
// -> minimal -> floor, dropping the ETA first, then elapsed, then folders,
// then the failed count, and never truncates: with a width-measured fake, a
// fitting full line lands whole, an ETA overflow lands counters+elapsed
// intact, a full-counters overflow lands the abbreviated counters with
// elapsed, a folders overflow lands the minimal pair, and a tiny budget
// lands the floor done/total pair alone - never truncated.
func TestComposeScanLiveStatusPriority(t *testing.T) {
	p := library.Progress{KeysDone: 12000, KeysTotal: 30000, KeysFetched: 8000,
		KeysCached: 3000, KeysFailed: 1000, FoldersScanned: 200, FoldersSkipped: 15}
	elapsed, resolve := 20*time.Second, 20*time.Second
	const base = 0
	measure := func(s string) int32 { return int32(len(s)) }
	cands := scanStatusCandidates(p, elapsed, resolve, base)
	full := cands[0]
	if !strings.Contains(full, "eta") {
		t.Fatalf("first candidate = %q, want the full line with ETA", full)
	}
	if got := composeScanLiveStatus(p, elapsed, resolve, base, int32(len(full)), measure); got != full {
		t.Fatalf("fitting compose = %q, want the full line", got)
	}
	// ETA alone overflows: full counters + elapsed survive byte-identical.
	counters, elapsedSfx, _ := scanLiveStatusParts(p, elapsed, resolve, base)
	if got := composeScanLiveStatus(p, elapsed, resolve, base, int32(len(counters+elapsedSfx)), measure); got != counters+elapsedSfx {
		t.Fatalf("eta-overflow compose = %q, want counters+elapsed", got)
	}
	// Full counters overflow too: the abbreviated counters keep elapsed.
	short := scanProgressShort(p)
	if len(short+elapsedSfx) >= len(counters+elapsedSfx) {
		t.Fatalf("short+elapsed len=%d >= full+elapsed len=%d (abbreviation must shrink)", len(short+elapsedSfx), len(counters+elapsedSfx))
	}
	if got := composeScanLiveStatus(p, elapsed, resolve, base, int32(len(short+elapsedSfx)), measure); got != short+elapsedSfx {
		t.Fatalf("counters-overflow compose = %q, want abbreviated+elapsed", got)
	}
	// Folders overflow too: the minimal pair keeps the failed count, and it
	// is exactly the floor plus that suffix.
	minimal := scanProgressMinimal(p)
	floor := scanProgressFloor(p)
	if minimal != floor+", 1k failed" {
		t.Fatalf("minimal = %q, want floor %q plus the failed suffix", minimal, floor)
	}
	if len(minimal) >= len(short) {
		t.Fatalf("minimal len=%d >= short len=%d (dropping folders must shrink)", len(minimal), len(short))
	}
	if got := composeScanLiveStatus(p, elapsed, resolve, base, int32(len(minimal)), measure); got != minimal {
		t.Fatalf("folders-overflow compose = %q, want the minimal pair", got)
	}
	// Tiny budget: the floor done/total pair alone, never truncated.
	if got := composeScanLiveStatus(p, elapsed, resolve, base, 1, measure); got != floor {
		t.Fatalf("tiny-budget compose = %q, want the floor pair", got)
	}
}

// TestScanLiveStatusFitsAtAllDPI re-measures the composed live line with the
// native GDI technique (GetTextExtentPoint32W in the status font, the same
// call the paint path audits with) at 96/120/144/192 DPI, at the standard
// client AND a shrunken client (the WM_DPICHANGED case: the window keeps its
// physical px, so a 460 px window moved to 144 DPI offers a 400 px status
// rect, not the 630 px standard budget - the composition budget is that
// live rect): every scale from hundreds to a million keys must need no more
// than the live rect offers. Every fixture is a reachable state (key buckets
// sum exactly to done, the invariant production maintains). The review-scale
// case also proves the run exercises the fallback: the full counters line
// alone overflows, so green means the abbreviated tiers did the work - fit
// by construction, not by luck. The pathological case pins the absolute
// floor: even the minimal pair overflows the narrowest rect, so green means
// composition landed the done/total pair alone.
func TestScanLiveStatusFitsAtAllDPI(t *testing.T) {
	cases := []struct {
		name             string
		p                library.Progress
		elapsed, resolve time.Duration
		base             int
	}{
		{"walk", library.Progress{FoldersScanned: 3}, 30 * time.Second, 0, 0},
		{"resolve", library.Progress{KeysDone: 12, KeysTotal: 24, KeysFetched: 7, KeysCached: 5, FoldersScanned: 4, FoldersSkipped: 2}, 20 * time.Second, 20 * time.Second, 0},
		{"mid-failed", library.Progress{KeysDone: 450, KeysTotal: 2000, KeysFetched: 300, KeysCached: 140, KeysFailed: 10, FoldersScanned: 45, FoldersSkipped: 6}, 75 * time.Second, 75 * time.Second, 0},
		{"steady-eta", library.Progress{KeysDone: 100, KeysTotal: 1000, KeysFetched: 100, FoldersScanned: 9, FoldersSkipped: 1}, 20 * time.Second, 20 * time.Second, 0},
		{"thousands", library.Progress{KeysDone: 2450, KeysTotal: 3100, KeysFetched: 1200, KeysCached: 1200, KeysNotFound: 50, FoldersScanned: 145, FoldersSkipped: 32}, 5 * time.Minute, 5 * time.Minute, 0},
		{"tens-of-thousands", library.Progress{KeysDone: 12000, KeysTotal: 30000, KeysFetched: 8000, KeysCached: 3000, KeysFailed: 1000, FoldersScanned: 200, FoldersSkipped: 15}, 10 * time.Minute, 10 * time.Minute, 0},
		{"review-scale", library.Progress{KeysDone: 45000, KeysTotal: 90000, KeysFetched: 15000, KeysCached: 15000, KeysFailed: 15000, FoldersScanned: 450, FoldersSkipped: 32}, time.Hour, time.Hour, 15000},
		{"catching-up", catchingUpSnapshot(), 6 * time.Minute, 5 * time.Minute, 418},
		{"pathological", library.Progress{KeysDone: 999800, KeysTotal: 999900, KeysFetched: 50000, KeysCached: 50000, KeysFailed: 899800, FoldersScanned: 999, FoldersSkipped: 99}, time.Hour, time.Hour, 50000},
	}
	for _, dpi := range []int{96, 120, 144, 192} {
		cw := scalePx(int32(dpi), mainClientW)
		ch := scalePx(int32(dpi), mainClientH)
		l := computeLayout(cw, ch, dpi)
		avail := l.status.right - l.status.left
		if avail != scanStatusAvailPx(dpi) {
			t.Fatalf("dpi %d: layout status width = %d, want scanStatusAvailPx %d", dpi, avail, scanStatusAvailPx(dpi))
		}
		if avail != statusAvailPx(cw, dpi) {
			t.Fatalf("dpi %d: layout status width = %d, want live-derivation statusAvailPx %d", dpi, avail, statusAvailPx(cw, dpi))
		}
		// The shrunken client: a window born at 96 DPI (460 physical px)
		// moved to a higher DPI without a resize. Its live status rect -
		// not the DPI-scaled standard budget - is what composition gets.
		widths := []int32{avail}
		if dpi > 96 {
			if narrow := statusAvailPx(mainClientW, dpi); narrow < avail {
				widths = append(widths, narrow)
			}
		}
		for _, budget := range widths {
			for _, c := range cases {
				text := scanLiveStatusText(c.p, c.elapsed, c.resolve, c.base, dpi, budget)
				need, ok := measureScanStatusWidth(dpi, text)
				if !ok {
					t.Skipf("dpi %d: no GDI DC for measurement", dpi)
				}
				t.Logf("dpi=%d avail=%d case=%s need=%d text=%q", dpi, budget, c.name, need, text)
				if need > budget {
					t.Fatalf("dpi %d avail %d case %s: need=%d > avail=%d: %q", dpi, budget, c.name, need, budget, text)
				}
			}
		}
	}
	// Stress control: at 96 DPI the review-scale full line (counters +
	// elapsed + ETA) must overflow on its own, so the green runs above prove
	// the abbreviated tiers carry the fit rather than the full line fitting
	// by luck.
	big := cases[len(cases)-1]
	counters, elapsedSfx, etaSfx := scanLiveStatusParts(big.p, big.elapsed, big.resolve, big.base)
	if etaSfx == "" {
		t.Fatalf("review-scale parts eta empty, want an estimate (else the control is vacuous)")
	}
	if need, ok := measureScanStatusWidth(96, counters+elapsedSfx+etaSfx); !ok {
		t.Skipf("no GDI DC for measurement")
	} else if need <= scanStatusAvailPx(96) {
		t.Fatalf("review-scale full line need=%d <= avail=%d: control is vacuous (nothing to abbreviate)", need, scanStatusAvailPx(96))
	}
	// Pathological controls: the narrowest budget the table covers is the
	// shrunken 460 px client at 192 DPI (380 px). The minimal pair with its
	// failed suffix must overflow it (so green above proves composition fell
	// through past minimal), while the floor pair alone - and the no-HDC
	// fallback shape (floor with elapsed) - must fit it with margin. The
	// composed pathological line at that budget is the floor byte-for-byte.
	path := cases[len(cases)-1]
	narrow192 := statusAvailPx(mainClientW, 192)
	if need, ok := measureScanStatusWidth(192, scanProgressMinimal(path.p)); !ok {
		t.Skipf("no GDI DC for measurement")
	} else if need <= narrow192 {
		t.Fatalf("pathological minimal need=%d <= narrow=%d: control is vacuous (failed suffix must overflow)", need, narrow192)
	} else {
		t.Logf("narrow192=%d minimal-need=%d (must overflow)", narrow192, need)
	}
	floor := scanProgressFloor(path.p)
	if need, ok := measureScanStatusWidth(192, floor); !ok {
		t.Skipf("no GDI DC for measurement")
	} else if need > narrow192 {
		t.Fatalf("floor need=%d > narrow=%d: the floor does not fit the narrowest budget", need, narrow192)
	} else {
		t.Logf("narrow192=%d floor-need=%d text=%q (must fit)", narrow192, need, floor)
	}
	fallback := floor + " | " + formatScanDuration(path.elapsed)
	if need, ok := measureScanStatusWidth(192, fallback); !ok {
		t.Skipf("no GDI DC for measurement")
	} else if need > narrow192 {
		t.Fatalf("fallback need=%d > narrow=%d: floor+elapsed %q does not fit the narrowest budget", need, narrow192, fallback)
	} else {
		t.Logf("narrow192=%d fallback-need=%d text=%q (must fit)", narrow192, need, fallback)
	}
	if got := scanLiveStatusText(path.p, path.elapsed, path.resolve, path.base, 192, narrow192); got != floor {
		t.Fatalf("pathological narrow compose = %q, want the floor %q", got, floor)
	}
}

// TestScanBarGeometry proves the strip never pushes text out of its rect:
// at the smallest supported size (fixed 460x680 client at 96 DPI) and at
// 120/144/192 DPI, the bar sits fully inside the client with positive size
// and never intersects the status text rect.
func TestScanBarGeometry(t *testing.T) {
	for _, dpi := range []int{96, 120, 144, 192} {
		cw := scalePx(int32(dpi), mainClientW)
		ch := scalePx(int32(dpi), mainClientH)
		l := computeLayout(cw, ch, dpi)
		if dpi == 96 && l.status.right-l.status.left != 420 {
			t.Fatalf("status width = %d, want 420 (smallest size must keep the text rect)", l.status.right-l.status.left)
		}
		bar := scanBarRect(l.status, dpi)
		if w, h := bar.right-bar.left, bar.bottom-bar.top; w <= 0 || h <= 0 {
			t.Fatalf("dpi %d: bar %+v has non-positive size", dpi, bar)
		}
		if bar.left < 0 || bar.top < 0 || bar.right > cw || bar.bottom > ch {
			t.Fatalf("dpi %d: bar %+v escapes the %dx%d client", dpi, bar, cw, ch)
		}
		if bar.left < l.status.right && l.status.left < bar.right &&
			bar.top < l.status.bottom && l.status.top < bar.bottom {
			t.Fatalf("dpi %d: bar %+v intersects the status text rect %+v", dpi, bar, l.status)
		}
	}
}

// TestScanIndeterminatePulse proves the walk-phase sweep cycles inside
// [0,1): starts at 0, mid-loop at 0.5, wraps back at the loop end.
func TestScanIndeterminatePulse(t *testing.T) {
	if got := scanIndeterminatePos(0); got != 0 {
		t.Fatalf("pulse(0) = %v, want 0", got)
	}
	if got := scanIndeterminatePos(600 * time.Millisecond); got != 0.5 {
		t.Fatalf("pulse(600ms) = %v, want 0.5", got)
	}
	if got := scanIndeterminatePos(1200 * time.Millisecond); got != 0 {
		t.Fatalf("pulse(1200ms) = %v, want 0 (wrap)", got)
	}
	for _, d := range []time.Duration{-time.Second, time.Second, 5 * time.Second, time.Hour} {
		if got := scanIndeterminatePos(d); got < 0 || got >= 1 {
			t.Fatalf("pulse(%v) = %v, want in [0,1)", d, got)
		}
	}
}

// catchingUpSnapshot is the state the brief pins: the index phase is complete
// (Done == Total, a real total) while metadata keys are still resolving
// (Resolved < KeysTotal) - still working, never finished.
func catchingUpSnapshot() library.Progress {
	return library.Progress{Done: 23402, Total: 23402, Resolved: 1118, KeysTotal: 2309,
		KeysDone: 1118, KeysCached: 418, KeysFetched: 700, FoldersScanned: 3}
}

// TestScanCatchingUpText proves the catching-up state reads as still working:
// the live line carries the two pairs (indexed Done of Total files, metadata
// Resolved of KeysTotal) and says "catching up", and the bar cannot paint a
// finished fill.
func TestScanCatchingUpText(t *testing.T) {
	p := catchingUpSnapshot()
	if !scanCatchingUp(p) {
		t.Fatalf("scanCatchingUp(%+v) = false, want true (files indexed, keys left)", p)
	}
	seg := scanIndexMetaText(p)
	want := "indexed 23402 of 23402 files | metadata 1118 of 2309 - catching up"
	if seg != want {
		t.Fatalf("scanIndexMetaText = %q, want %q", seg, want)
	}
	if got := scanProgressText(p); !strings.Contains(got, want) {
		t.Fatalf("scanProgressText = %q, want it to lead with %q", got, want)
	}
	// The pair outranks the abbreviated key tiers: with a budget that fits
	// exactly the pair, the pair is what lands (never keys alone).
	measure := func(s string) int32 { return int32(len(s)) }
	if got := composeScanLiveStatus(p, 20*time.Second, 20*time.Second, 0, int32(len(seg)), measure); got != seg {
		t.Fatalf("compose(pair budget) = %q, want the catching-up pair %q", got, seg)
	}
	// Bar: determinate and PARTIAL - the state must not read as finished.
	show, det, frac := scanBarProgress(p, true)
	if !show || !det {
		t.Fatalf("catching-up bar = show %v determinate %v, want a determinate fill", show, det)
	}
	if frac >= 1 {
		t.Fatalf("catching-up bar frac = %v, want < 1 while metadata keys remain", frac)
	}
}

// TestScanFinishedTextNotCatchingUp proves the same snapshot with the keys
// resolved is NOT catching up: the marker goes, the metadata pair reads
// complete, and the bar may fill. The walk phase keeps its old shape too - no
// total yet, so no pair and no determinate bar.
func TestScanFinishedTextNotCatchingUp(t *testing.T) {
	p := catchingUpSnapshot()
	p.Resolved, p.KeysDone = p.KeysTotal, p.KeysTotal
	if scanCatchingUp(p) {
		t.Fatalf("scanCatchingUp(%+v) = true, want false once Resolved == KeysTotal", p)
	}
	seg := scanIndexMetaText(p)
	want := "indexed 23402 of 23402 files | metadata 2309 of 2309"
	if seg != want {
		t.Fatalf("scanIndexMetaText = %q, want %q", seg, want)
	}
	if strings.Contains(seg, "catching up") {
		t.Fatalf("finished text = %q, want no catching-up marker", seg)
	}
	measure := func(s string) int32 { return int32(len(s)) }
	if got := composeScanLiveStatus(p, 20*time.Second, 20*time.Second, 0, int32(len(seg)), measure); got != seg {
		t.Fatalf("compose(finished) = %q, want the complete pair %q", got, seg)
	}
	if _, det, frac := scanBarProgress(p, true); !det || frac != 1 {
		t.Fatalf("finished bar = determinate %v frac %v, want determinate 1", det, frac)
	}
	// Walk phase (no totals yet): no pair, sweep instead of a fake percent.
	walk := library.Progress{Done: 5, Total: 10, FoldersScanned: 1}
	if scanCatchingUp(walk) {
		t.Fatalf("scanCatchingUp(%+v) = true, want false while no key total exists", walk)
	}
	if got := scanIndexMetaText(walk); got != "indexed 5 of 10 files" {
		t.Fatalf("walk pair = %q, want the file segment alone", got)
	}
	if show, det, _ := scanBarProgress(walk, true); !show || det {
		t.Fatalf("walk bar = show %v determinate %v, want an indeterminate sweep", show, det)
	}
	if got := scanIndexMetaText(library.Progress{}); got != "" {
		t.Fatalf("empty pair = %q, want none (the pre-existing line is unchanged)", got)
	}
}

// TestReportDecision proves the Report button's read decision: a missing or
// unreadable report and an empty one all say there is nothing to report, and
// only entries proceed. Drives the REAL handler on a temp path, so no report
// on this machine, no network, and no browser is reachable from the test; the
// nothing-to-report branch returns before the worker goroutine is started
// (hwnd 0 is never posted to).
func TestReportDecision(t *testing.T) {
	prevStatus := statusText
	defer func() { statusText = prevStatus }()

	dir := t.TempDir()
	// Missing file.
	missing := filepath.Join(dir, "missing.json")
	statusText = ""
	onReportAt(0, missing) // must not panic, must not create an issue
	if statusText != reportNothingText {
		t.Fatalf("missing report status = %q, want %q", statusText, reportNothingText)
	}
	if !strings.Contains(strings.ToLower(statusText), "nothing to report") {
		t.Fatalf("missing report status = %q, want it to say nothing to report", statusText)
	}
	// Empty report, written by the repair package itself.
	if _, _, err := repair.WriteReport(dir, repair.Report{}); err != nil {
		t.Fatalf("WriteReport(empty) failed: %v", err)
	}
	statusText = ""
	onReportAt(0, filepath.Join(dir, repair.ReportJSONName))
	if statusText != reportNothingText {
		t.Fatalf("empty report status = %q, want %q", statusText, reportNothingText)
	}
	// Corrupt report: same answer, still no panic.
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write corrupt report: %v", err)
	}
	statusText = ""
	onReportAt(0, bad)
	if statusText != reportNothingText {
		t.Fatalf("corrupt report status = %q, want %q", statusText, reportNothingText)
	}
	// Entries proceed, and nothing else reaches the status line.
	if proceed, status := reportDecision(repair.Report{Entries: []repair.Entry{{Name: "a.mkv"}}}, nil); !proceed || status != "" {
		t.Fatalf("reportDecision(1 entry) = (%v, %q), want (true, \"\")", proceed, status)
	}
	if proceed, status := reportDecision(repair.Report{}, nil); proceed || status != reportNothingText {
		t.Fatalf("reportDecision(empty) = (%v, %q), want (false, %q)", proceed, status, reportNothingText)
	}
	if proceed, status := reportDecision(repair.Report{Entries: []repair.Entry{{}}}, errors.New("unreadable")); proceed || status != reportNothingText {
		t.Fatalf("reportDecision(err) = (%v, %q), want (false, %q)", proceed, status, reportNothingText)
	}
}

// TestReportOffThread proves the Report handler never blocks the UI thread:
// with an injected runner that sleeps, onReportAt returns at once, the status
// line says the report is being filed, and the run's result - the URL the
// injected runner produced - lands through the wmAppReportDone payload (the
// same goroutine + PostMessage pattern the scan uses). hwnd 0 means the post
// goes nowhere in the test, so the done handler is driven directly.
func TestReportOffThread(t *testing.T) {
	prevStatus := statusText
	prevDone := reportDoneText
	prevCreate, prevOpen := reportCreateIssue, reportOpenURL
	defer func() {
		statusText, reportDoneText = prevStatus, prevDone
		reportCreateIssue, reportOpenURL = prevCreate, prevOpen
	}()

	dir := t.TempDir()
	if _, _, err := repair.WriteReport(dir, repair.Report{Entries: []repair.Entry{{Name: "a.mkv"}}}); err != nil {
		t.Fatalf("WriteReport(1 entry) failed: %v", err)
	}
	const wantURL = "https://example.invalid/potflix/issues/1"
	const runnerSleep = 300 * time.Millisecond
	reportCreateIssue = func(context.Context, repair.Report, repair.Options) (string, error) {
		time.Sleep(runnerSleep)
		return wantURL, nil
	}
	// The browser is never launched: this opener refuses the URL, so the run
	// ends on the launch status line, carrying the fake's URL verbatim.
	reportOpenURL = func(string) bool { return false }

	reportDoneText, statusText = "", ""
	start := time.Now()
	onReportAt(0, filepath.Join(dir, repair.ReportJSONName))
	elapsed := time.Since(start)
	if elapsed > runnerSleep/2 {
		t.Fatalf("onReportAt took %v, want an immediate return (the runner sleeps %v)", elapsed, runnerSleep)
	}
	if statusText != reportPendingText {
		t.Fatalf("in-flight status = %q, want %q", statusText, reportPendingText)
	}

	// Wait for the worker's payload, then land it exactly as the wndproc does
	// on wmAppReportDone.
	deadline := time.Now().Add(5 * time.Second)
	for {
		reportMu.Lock()
		text := reportDoneText
		reportMu.Unlock()
		if text != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the report worker never published a status line")
		}
		time.Sleep(5 * time.Millisecond)
	}
	onReportDone()
	if want := reportLaunchText(wantURL); statusText != want {
		t.Fatalf("completion status = %q, want %q", statusText, want)
	}
	if !strings.Contains(statusText, wantURL) {
		t.Fatalf("completion status = %q, want it to carry the runner's URL %q", statusText, wantURL)
	}
}

// TestClearActionsAreDistinct proves the split clear is really two actions:
// distinct status lines, and two cores that touch different files. Clear
// metadata empties the cache and leaves the file index (and its records) on
// disk, so the next scan still skips what it walked; Clear index removes the
// index files and leaves the cached metadata alone, so the full re-walk
// fetches nothing again.
func TestClearActionsAreDistinct(t *testing.T) {
	if clearMetadataStatus == clearIndexStatus(3) {
		t.Fatalf("the two clear status lines collide: %q", clearMetadataStatus)
	}
	base, err := os.MkdirTemp("", "potflix-clearcore-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	// Best-effort delete only: internal/metadata now holds one cache store
	// handle per directory for the process lifetime (cache.go:136-143) and its
	// close hook is package-internal (cache.go:215), so this package cannot
	// unlink the store file on Windows (t.TempDir would fail the test on
	// cleanup instead of on an assertion). The clear itself is asserted above;
	// what remains is one emptied store file.
	defer func() { _ = os.RemoveAll(base) }()
	meta := filepath.Join(base, "meta")
	if err := os.MkdirAll(meta, 0o755); err != nil {
		t.Fatalf("mkdir meta: %v", err)
	}
	name := seedSixteenHex(t, meta, "movie|alpha|2001", metadata.CacheSchema(), `{"title":"Alpha"}`)
	idx := filepath.Join(base, "scanindex.json")
	seedIndex := func() {
		t.Helper()
		ix := library.LoadScanIndex(idx)
		ix.Record(`C:\Media`, "fp-1", metadata.CacheSchema(), []string{"movie|alpha|2001"})
		if err := ix.Save(idx); err != nil {
			t.Fatalf("seed index save: %v", err)
		}
	}
	seedIndex()

	// Clear index: the index files go, the cached title stays.
	records, err := clearScanIndex(idx)
	if err != nil {
		t.Fatalf("clearScanIndex: %v", err)
	}
	if records != 1 {
		t.Fatalf("clearScanIndex records = %d, want 1", records)
	}
	if _, err := os.Stat(library.IndexStorePath(idx)); !os.IsNotExist(err) {
		t.Fatalf("clearScanIndex left the index store %s", library.IndexStorePath(idx))
	}
	if _, err := os.Stat(filepath.Join(meta, name)); err != nil {
		t.Fatalf("clearScanIndex removed the cached title: %v", err)
	}

	// Clear metadata: the cache goes, the index and its records survive - a
	// reopened index still reports the folder, so no re-walk is needed.
	seedIndex()
	if err := clearMetadataCache(meta); err != nil {
		t.Fatalf("clearMetadataCache: %v", err)
	}
	if _, err := os.Stat(filepath.Join(meta, name)); !os.IsNotExist(err) {
		t.Fatalf("clearMetadataCache left the cached title %s", name)
	}
	if err := library.CloseIndex(idx); err != nil {
		t.Fatalf("CloseIndex: %v", err)
	}
	if got := library.LoadScanIndex(idx).Len(); got != 1 {
		t.Fatalf("index records after a metadata clear = %d, want 1 (durable on disk)", got)
	}
}

// TestReportButtonLayout proves the Report control is a first-class member of
// the main action block: same left column and width as Scan (its own row of
// the same grid), Refresh's outline style, present in the layout table, the
// Tab order and the hit test, and free while a scan runs.
func TestReportButtonLayout(t *testing.T) {
	oldDirs := uiDirs
	uiDirs = []string{`C:\Media\Movies`}
	defer func() { uiDirs = oldDirs }()

	l := computeLayout(mainClientW, mainClientH, 96)
	byID := map[int]uiButton{}
	for _, b := range l.btns {
		byID[b.id] = b
	}
	rep, ok := byID[btnReport]
	if !ok {
		t.Fatalf("btnReport missing from the main layout table")
	}
	scan, refresh := byID[btnScan], byID[btnRefresh]
	if rep.label != reportButtonLabel {
		t.Fatalf("Report label = %q, want %q", rep.label, reportButtonLabel)
	}
	if rep.style != refresh.style {
		t.Fatalf("Report style = %d, want %d (same family as Refresh)", rep.style, refresh.style)
	}
	if rep.rc.left != scan.rc.left || rep.rc.right != scan.rc.right {
		t.Fatalf("Report rect %+v, want Scan's column %+v", rep.rc, scan.rc)
	}
	if rep.rc.top <= refresh.rc.bottom {
		t.Fatalf("Report rect %+v, want a row below Refresh %+v", rep.rc, refresh.rc)
	}
	if rep.rc.bottom > l.status.top || rep.rc.right > mainClientW {
		t.Fatalf("Report rect %+v escapes into the status/client", rep.rc)
	}
	if got := buttonAtPoint(l.btns, (rep.rc.left+rep.rc.right)/2, (rep.rc.top+rep.rc.bottom)/2); got != btnReport {
		t.Fatalf("Report center hits %d, want %d", got, btnReport)
	}
	found := false
	for _, id := range mainFocusOrder() {
		if id == btnReport {
			found = true
		}
	}
	if !found {
		t.Fatalf("main focus order misses btnReport")
	}
	a := scanActive
	defer func() { scanActive = a }()
	scanActive = true
	if got := scanBlockedReason(btnReport); got != "" {
		t.Fatalf("mid-scan Report blocked (%q), want free (it only reads a report)", got)
	}
}

// TestOpenURLTexts proves the Report status lines: a filed issue that reached
// the browser, the prefilled-URL fallback when gh failed, and the URL kept on
// the line when the browser hand-off failed.
func TestOpenURLTexts(t *testing.T) {
	if got := reportOpenedText(false); !strings.Contains(got, "opened") {
		t.Fatalf("reportOpenedText(false) = %q, want the opened line", got)
	}
	if got := reportOpenedText(true); !strings.Contains(got, "prefilled") {
		t.Fatalf("reportOpenedText(true) = %q, want the prefilled-URL line", got)
	}
	url := "https://github.com/nathwn12/Potflix/issues/new?title=x"
	if got := reportLaunchText(url); !strings.Contains(got, url) {
		t.Fatalf("reportLaunchText = %q, want the URL kept on the line", got)
	}
	if !strings.HasSuffix(repairReportPath(), repair.ReportJSONName) {
		t.Fatalf("repairReportPath = %q, want it to end with %q", repairReportPath(), repair.ReportJSONName)
	}
	if !strings.Contains(strings.ToLower(repairReportPath()), "potflix") {
		t.Fatalf("repairReportPath = %q, want the Potflix cache tree", repairReportPath())
	}
}
