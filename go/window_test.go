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
func saveViewState() (uiView, keyField, bool, bool, bool) {
	return curView, tmdbField, tmdbShow, fieldFocused, windowFocused
}

func restoreViewState(v uiView, f keyField, show, focused, winFocused bool) {
	curView, tmdbField, tmdbShow, fieldFocused, windowFocused = v, f, show, focused, winFocused
}

// TestViewSwitch proves the state machine: main -> settings (field loaded,
// masked) -> back to main.
func TestViewSwitch(t *testing.T) {
	v, f, show, focused, winFocused := saveViewState()
	defer restoreViewState(v, f, show, focused, winFocused)

	curView = viewMain
	openSettings("secret-key")
	if curView != viewSettings {
		t.Fatalf("view = %d, want settings", curView)
	}
	if tmdbField.text != "secret-key" {
		t.Fatalf("field = %q, want the loaded value", tmdbField.text)
	}
	if tmdbShow {
		t.Fatalf("reveal = true on entry, want masked by default")
	}
	if fieldFocused {
		t.Fatalf("field focused on entry, want unfocused until clicked")
	}
	if got := MaskKey(tmdbField.text); got == "secret-key" {
		t.Fatalf("masked field shows the raw value")
	}
	closeSettings()
	if curView != viewMain {
		t.Fatalf("view = %d, want main after Back", curView)
	}
	if fieldFocused {
		t.Fatalf("field still focused after Back, want unfocused")
	}
}

// TestSettingsHitTest proves every settings control resolves: Back, Save,
// Show/Hide, the TMDB get-a-key link, Test, the three planned links, the
// LIBRARY row (Clear cache/Export/Import), the STARTUP row (toggle/Open
// log folder), and the SCAN segments (covered in depth by
// TestSettingsScanHitTest) all hit-test back to their own ids, stay inside
// the client, and never overlap.
func TestSettingsHitTest(t *testing.T) {
	oldScan := scanSpeedState
	scanSpeedState = "balanced"
	defer func() { scanSpeedState = oldScan }()
	l := computeSettingsLayout(mainClientW, mainClientH, 96, false, false)
	if len(l.btns) != 16 {
		t.Fatalf("settings buttons = %d, want 16 (Back/Show/GetTMDB/Test/3 planned/Save + Clear/Export/Import/Startup/OpenLog + 3 SCAN)", len(l.btns))
	}
	for _, id := range []int{btnBack, btnSave, btnShow, btnGetTMDB, btnTest, btnGetOMDb, btnGetTVDB, btnGetTrakt,
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
	// The field rect is the focus target, not a button.
	if got := buttonAtPoint(l.btns, (l.field.left+l.field.right)/2, (l.field.top+l.field.bottom)/2); got != 0 {
		t.Fatalf("field center hits button %d, want 0 (field is not a button)", got)
	}
	if l.field.right > mainClientW || l.field.left < 0 {
		t.Fatalf("field rect %+v escapes the client width", l.field)
	}
	// Reveal state only flips the Show/Hide label.
	shown := computeSettingsLayout(mainClientW, mainClientH, 96, true, false)
	rc, _ := shown.settingsBtn(btnShow)
	var label string
	for _, b := range shown.btns {
		if b.id == btnShow {
			label = b.label
		}
	}
	_ = rc
	if label != "Hide" {
		t.Fatalf("revealed Show label = %q, want Hide", label)
	}
	for _, b := range l.btns {
		if b.id == btnShow && b.label != "Show" {
			t.Fatalf("masked Show label = %q, want Show", b.label)
		}
	}
	// Startup state only flips the toggle checkbox label.
	off := computeSettingsLayout(mainClientW, mainClientH, 96, false, false)
	on := computeSettingsLayout(mainClientW, mainClientH, 96, false, true)
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
	// The LIBRARY/STARTUP rows sit in the free strip: below the planned row
	// (ends y=336) and above the attribution footer (starts above Save at
	// y=502, i.e. well clear of the Save-anchored attribution at 454).
	for _, id := range []int{btnClearCache, btnExport, btnImport} {
		rc, _ := l.settingsBtn(id)
		if rc.top < 336 || rc.bottom > 454 {
			t.Fatalf("library button %d rect %+v leaves the free strip (want y 336..454)", id, rc)
		}
	}
	for _, id := range []int{btnStartup, btnOpenLog} {
		rc, _ := l.settingsBtn(id)
		if rc.top < 336 || rc.bottom > 454 {
			t.Fatalf("startup button %d rect %+v leaves the free strip (want y 336..454)", id, rc)
		}
	}
}

// TestKeyFieldEditing proves the owner-drawn text-input model as a pure
// function: printable append, backspace, paste, and select-all-replace.
func TestKeyFieldEditing(t *testing.T) {
	var f keyField
	f.typeRune('a')
	f.typeRune('b')
	if f.text != "ab" {
		t.Fatalf("append = %q, want ab", f.text)
	}
	f.backspace()
	if f.text != "a" {
		t.Fatalf("backspace = %q, want a", f.text)
	}
	f.paste("cd-123")
	if f.text != "acd-123" {
		t.Fatalf("paste = %q, want acd-123", f.text)
	}
	// Select-all + printable replaces the whole buffer.
	f.selectAll()
	if !f.selAll {
		t.Fatalf("selectAll armed = false, want true")
	}
	f.typeRune('Z')
	if f.text != "Z" || f.selAll {
		t.Fatalf("select-all-replace = %q sel=%t, want Z/false", f.text, f.selAll)
	}
	// Select-all + paste replaces too.
	f.paste("xy")
	f.selectAll()
	f.paste("NEW")
	if f.text != "NEW" || f.selAll {
		t.Fatalf("select-all-paste = %q sel=%t, want NEW/false", f.text, f.selAll)
	}
	// Select-all + backspace clears.
	f.selectAll()
	f.backspace()
	if f.text != "" || f.selAll {
		t.Fatalf("select-all-backspace = %q sel=%t, want empty/false", f.text, f.selAll)
	}
	// Edge cases: no-ops, never panic.
	var e keyField
	e.backspace()
	e.selectAll() // empty: nothing to select
	if e.selAll {
		t.Fatalf("selectAll on empty armed, want false")
	}
	e.paste("")
	if e.text != "" {
		t.Fatalf("empty paste = %q, want empty", e.text)
	}
}

// TestSettingsCharGate proves the WM_CHAR gate: keystrokes land only when
// the settings view is visible with a focused field, backspace deletes, and
// control echoes (Ctrl+A/V) never apply twice.
func TestSettingsCharGate(t *testing.T) {
	v, f, show, focused, winFocused := saveViewState()
	defer restoreViewState(v, f, show, focused, winFocused)

	curView, tmdbField, tmdbShow, fieldFocused, windowFocused =
		viewSettings, keyField{}, false, true, true
	if !onSettingsChar('k') || tmdbField.text != "k" {
		t.Fatalf("char gate: printable did not append (text=%q)", tmdbField.text)
	}
	if onSettingsChar(0x16) {
		t.Fatalf("char gate: Ctrl+V echo applied, want ignored (WM_KEYDOWN owns paste)")
	}
	if onSettingsChar(0x01) {
		t.Fatalf("char gate: Ctrl+A echo applied, want ignored")
	}
	if !onSettingsChar(rune(vkBack)) || tmdbField.text != "" {
		t.Fatalf("char gate: backspace did not delete (text=%q)", tmdbField.text)
	}
	// Unfocused field: keystrokes go nowhere.
	fieldFocused = false
	if onSettingsChar('x') || tmdbField.text != "" {
		t.Fatalf("char gate: unfocused field accepted input (text=%q)", tmdbField.text)
	}
	// Main view: keystrokes go nowhere.
	curView, fieldFocused = viewMain, true
	if onSettingsChar('x') {
		t.Fatalf("char gate: main view accepted settings input")
	}
	// Esc returns to the main view.
	curView = viewSettings
	if !onSettingsKeyDown(0, vkEscape) || curView != viewMain {
		t.Fatalf("Esc: view = %d, want main", curView)
	}
}
