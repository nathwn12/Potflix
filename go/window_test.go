package main

import "testing"

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
	if len(l.btns) != 5 {
		t.Fatalf("buttons = %d, want 5 (Add/Remove/Scan/Refresh/Close)", len(l.btns))
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
	for _, id := range []int{btnAdd, btnRemove, btnScan, btnRefresh, btnClose} {
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
// link: the 5 existing buttons keep their table shape and the header link
// sits inside the client without overlapping any existing rect.
func TestMainSettingsAffordance(t *testing.T) {
	oldDirs := uiDirs
	uiDirs = []string{`C:\Media\Movies`, `D:\Shows`}
	defer func() { uiDirs = oldDirs }()

	l := computeLayout(mainClientW, mainClientH, 96)
	if len(l.btns) != 5 {
		t.Fatalf("main buttons = %d, want 5 (Settings lives outside the button table)", len(l.btns))
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
// LIBRARY row (Clear cache/Export/Import), the STARTUP row (toggle/Open
// log folder), and the SCAN segments (covered in depth by
// TestSettingsScanHitTest) all hit-test back to their own ids, stay inside
// the client, and never overlap.
func TestSettingsHitTest(t *testing.T) {
	oldScan := scanSpeedState
	scanSpeedState = "balanced"
	defer func() { scanSpeedState = oldScan }()
	l := computeSettingsLayout(mainClientW, mainClientH, 96, false)
	if len(l.btns) != 9 {
		t.Fatalf("settings buttons = %d, want 9 (Back + Clear/Export/Import + Startup/OpenLog + 3 SCAN)", len(l.btns))
	}
	for _, id := range []int{btnBack,
		btnClearCache, btnExport, btnImport, btnStartup, btnOpenLog} {
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
	for _, id := range []int{btnClearCache, btnExport, btnImport} {
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
