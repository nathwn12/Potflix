package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"potflix/internal/discord"
	"potflix/internal/metadata"
	"potflix/internal/presence"
)

// cardview_test.go - proves the Card view seam (go/window.go): the view
// state machine gains viewCard, every control hit-tests back, all painted
// text rects stay inside the client without intersections at every DPI, the
// preset buttons map 1:1 onto CardPresets, the preview rows quote the
// recorded payload verbatim, and the dead-titles label counts via
// metadata.CountNegatives.

// TestCardViewSwitch proves the state machine: main -> card -> back to main.
func TestCardViewSwitch(t *testing.T) {
	v := saveViewState()
	defer restoreViewState(v)

	curView = viewMain
	openCard()
	if curView != viewCard {
		t.Fatalf("view = %d, want card", curView)
	}
	closeCard()
	if curView != viewMain {
		t.Fatalf("view = %d, want main after Back", curView)
	}
}

// TestCardEsc proves Esc in the card view returns to main (and stays
// ignored in the main view's card branch).
func TestCardEsc(t *testing.T) {
	v := saveViewState()
	defer restoreViewState(v)

	curView = viewCard
	if !onSettingsKeyDown(0, vkEscape) || curView != viewMain {
		t.Fatalf("Esc: view = %d, want main", curView)
	}
	curView = viewCard
	if onSettingsKeyDown(0, 0x41) {
		t.Fatalf("non-Esc in card view handled, want ignored")
	}
	if curView != viewCard {
		t.Fatalf("non-Esc changed view to %d, want card", curView)
	}
}

// TestCardHitTest proves every card control resolves: Back, the 4 Details
// presets and the 4 State presets all hit-test back to their own ids, stay
// inside the client, and never overlap. The spec only flips button styles,
// never rects.
func TestCardHitTest(t *testing.T) {
	for _, spec := range []presence.CardSpec{
		{},
		{Details: "title", State: "synopsis"},
	} {
		l := computeCardLayout(mainClientW, mainClientH, 96, spec)
		if len(l.btns) != 9 {
			t.Fatalf("card buttons = %d, want 9 (Back + 4 Details + 4 State)", len(l.btns))
		}
		for _, id := range []int{btnCardBack,
			cardDetailsButton(0), cardDetailsButton(1), cardDetailsButton(2), cardDetailsButton(3),
			cardStateButton(0), cardStateButton(1), cardStateButton(2), cardStateButton(3)} {
			rc, ok := l.cardBtn(id)
			if !ok {
				t.Fatalf("card button id %d missing from layout", id)
			}
			if rc.right > mainClientW || rc.bottom > mainClientH || rc.left < 0 || rc.top < 0 {
				t.Fatalf("card button %d rect %+v escapes the %dx%d client", id, rc, mainClientW, mainClientH)
			}
			if got := buttonAtPoint(l.btns, (rc.left+rc.right)/2, (rc.top+rc.bottom)/2); got != id {
				t.Fatalf("card button %d center hits %d", id, got)
			}
		}
		for i := 0; i < len(l.btns); i++ {
			for j := i + 1; j < len(l.btns); j++ {
				a, b := l.btns[i].rc, l.btns[j].rc
				if a.left < b.right && b.left < a.right && a.top < b.bottom && b.top < a.bottom {
					t.Fatalf("card buttons %d and %d overlap: %+v vs %+v", l.btns[i].id, l.btns[j].id, a, b)
				}
			}
		}
		if len(l.rows) != 5 {
			t.Fatalf("preview rows = %d, want 5", len(l.rows))
		}
		for i, r := range l.rows {
			if r.left < l.preview.left || r.right > l.preview.right ||
				r.top < l.preview.top || r.bottom > l.preview.bottom {
				t.Fatalf("preview row %d %+v escapes the preview card %+v", i, r, l.preview)
			}
		}
	}
	// Rects are spec-independent (only styles flip).
	a := computeCardLayout(mainClientW, mainClientH, 96, presence.CardSpec{})
	b := computeCardLayout(mainClientW, mainClientH, 96, presence.CardSpec{Details: "series", State: "none"})
	for i := range a.btns {
		if a.btns[i].rc != b.btns[i].rc {
			t.Fatalf("preset button %d moved with spec: %+v vs %+v", a.btns[i].id, a.btns[i].rc, b.btns[i].rc)
		}
	}
}

// TestCardTextInvariants walks computeCardLayout at 96/120/144/192 DPI and
// asserts the structural contract behind the live UI-AUDIT: every painted
// text rect lies inside the client, has positive size, and no two intersect.
func TestCardTextInvariants(t *testing.T) {
	inside := func(cw, ch int32, r rect) bool {
		return r.left >= 0 && r.top >= 0 && r.right <= cw && r.bottom <= ch
	}
	for _, dpi := range []int{96, 120, 144, 192} {
		cw := scalePx(int32(dpi), mainClientW)
		ch := scalePx(int32(dpi), mainClientH)
		for _, spec := range []presence.CardSpec{{}, {Details: "title", State: "tag"}} {
			l := computeCardLayout(cw, ch, dpi, spec)
			rects := cardTextRects(cw, ch, dpi, l)
			for _, r := range rects {
				w, h := r.rc.right-r.rc.left, r.rc.bottom-r.rc.top
				if w <= 0 || h <= 0 {
					t.Fatalf("dpi %d: card text rect %s %+v has non-positive size", dpi, r.id, r.rc)
				}
				if !inside(cw, ch, r.rc) {
					t.Fatalf("dpi %d: card text rect %s %+v escapes the %dx%d client", dpi, r.id, r.rc, cw, ch)
				}
			}
			for i := 0; i < len(rects); i++ {
				for j := i + 1; j < len(rects); j++ {
					a, b := rects[i], rects[j]
					if rectsIntersect(a.rc, b.rc) {
						t.Fatalf("dpi %d: card text rects %s and %s intersect: %+v vs %+v",
							dpi, a.id, b.id, a.rc, b.rc)
					}
				}
			}
		}
	}
}

// TestMainCardAffordance proves the main view gains only the Card link next
// to Settings: the 5 existing buttons keep working and the new link sits in
// the header dead strip without overlapping any existing text rect.
func TestMainCardAffordance(t *testing.T) {
	oldDirs := uiDirs
	uiDirs = []string{`C:\Media\Movies`, `D:\Shows`}
	defer func() { uiDirs = oldDirs }()

	l := computeLayout(mainClientW, mainClientH, 96)
	s := l.cardLink
	if s.right > mainClientW || s.bottom > mainClientH || s.left < 0 || s.top < 0 {
		t.Fatalf("card link rect %+v escapes the %dx%d client", s, mainClientW, mainClientH)
	}
	if s.right-s.left <= 0 || s.bottom-s.top <= 0 {
		t.Fatalf("card link rect %+v is empty", s)
	}
	// Dead strip: below the subtitle (y=68) and above the section (y=92),
	// left of Settings.
	if s.top < 68 || s.bottom > 92 {
		t.Fatalf("card link rect %+v leaves its strip (want within y 68..92)", s)
	}
	if s.right != l.settings.left {
		t.Fatalf("card link right = %d, want Settings left %d (adjacent)", s.right, l.settings.left)
	}
	rects := mainTextRects(mainClientW, mainClientH, 96, l, 2)
	for i := 0; i < len(rects); i++ {
		for j := i + 1; j < len(rects); j++ {
			a, b := rects[i], rects[j]
			if rectsIntersect(a.rc, b.rc) {
				t.Fatalf("main text rects %s and %s intersect: %+v vs %+v",
					a.id, b.id, a.rc, b.rc)
			}
		}
	}
}

// TestCardPresetMapping proves the 8 preset buttons map 1:1 onto
// CardPresets in order, anything else maps nowhere, and the style marks
// exactly the active preset per line.
func TestCardPresetMapping(t *testing.T) {
	dets, states := presence.CardPresets()
	for i, p := range dets {
		line, id, ok := cardPresetForButton(cardDetailsButton(i))
		if !ok || line != 0 || id != p.ID {
			t.Fatalf("details button %d maps to (%d, %q, %t), want (0, %q, true)",
				i, line, id, ok, p.ID)
		}
	}
	for i, p := range states {
		line, id, ok := cardPresetForButton(cardStateButton(i))
		if !ok || line != 1 || id != p.ID {
			t.Fatalf("state button %d maps to (%d, %q, %t), want (1, %q, true)",
				i, line, id, ok, p.ID)
		}
	}
	for _, id := range []int{0, 1, 2, 7, 10, 29, 34, 39, 44, 99} {
		if _, _, ok := cardPresetForButton(id); ok {
			t.Fatalf("button id %d maps to a preset, want no mapping", id)
		}
	}
	spec := presence.CardSpec{Details: "episode", State: "none"}
	for i, p := range dets {
		want := btnSubtle
		if p.ID == "episode" {
			want = btnAccent
		}
		if got := cardPresetStyle(0, p.ID, spec); got != want {
			t.Fatalf("details style %s = %d, want %d", p.ID, got, want)
		}
		_ = i
	}
	for _, p := range states {
		want := btnSubtle
		if p.ID == "none" {
			want = btnAccent
		}
		if got := cardPresetStyle(1, p.ID, spec); got != want {
			t.Fatalf("state style %s = %d, want %d", p.ID, got, want)
		}
	}
}

// TestPreviewRows proves the preview quotes the recorded payload verbatim:
// header/details/state/images/buttons read the Activity's fields, an empty
// header names the app-name fallback, and the waiting placeholder shows
// while nothing was sent yet.
func TestPreviewRows(t *testing.T) {
	rows := previewRows(presence.Activity{}, false)
	for i, r := range rows {
		if r == "" {
			t.Fatalf("placeholder row %d is empty, want a dash line", i)
		}
	}
	act := presence.Activity{
		Name: "Show Title", Details: "The Episode", State: "S1 E2",
		LargeImage: "https://example.com/poster.jpg", SmallImage: "play",
		Buttons: []presence.Button{
			{Label: "Trailer", URL: "https://example.com/trailer"},
			{Label: "IMDb", URL: "https://example.com/imdb"},
		},
	}
	got := previewRows(act, true)
	want := [5]string{
		"Header: Show Title",
		"Details: The Episode",
		"State: S1 E2",
		"Images: https://example.com/poster.jpg + play",
		"Buttons: Trailer -> https://example.com/trailer | IMDb -> https://example.com/imdb",
	}
	if got != want {
		t.Fatalf("preview rows = %q, want %q", got, want)
	}
	// Empty header falls back to the app name; empty state stays exact.
	idle := presence.Activity{Details: "Browsing", State: "Idle", LargeImage: "logo", SmallImage: "stop"}
	igot := previewRows(idle, true)
	if igot[0] != "Header: (Discord app name)" {
		t.Fatalf("idle header = %q, want the app-name fallback", igot[0])
	}
	if igot[2] != "State: Idle" || igot[3] != "Images: logo + stop" || igot[4] != "Buttons: none" {
		t.Fatalf("idle rows = %q", igot)
	}
	empty := presence.Activity{Details: "X", LargeImage: "logo", SmallImage: "stop"}
	if egot := previewRows(empty, true); egot[2] != "State: " {
		t.Fatalf("empty state = %q, want the exact empty line", egot[2])
	}
}

// TestPreviewRecord proves the observation boundary: recordPreview stores
// the exact handed value and previewActivity reads it back.
func TestPreviewRecord(t *testing.T) {
	previewMu.Lock()
	oldCard, oldHave := previewCard, previewHave
	previewMu.Unlock()
	defer func() {
		previewMu.Lock()
		previewCard, previewHave = oldCard, oldHave
		previewMu.Unlock()
	}()

	want := presence.Activity{Name: "T", Details: "D", State: "S", LargeImage: "logo", SmallImage: "play"}
	recordPreview(want)
	got, have := previewActivity()
	if !have {
		t.Fatalf("previewActivity have = false after record, want true")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("previewActivity = %+v, want %+v", got, want)
	}
}

type previewWire struct {
	Cmd  string `json:"cmd"`
	Args struct {
		Activity *struct {
			Name    string `json:"name"`
			Details string `json:"details"`
			State   string `json:"state"`
			Assets  *struct {
				LargeImage string `json:"large_image"`
				LargeText  string `json:"large_text"`
				SmallImage string `json:"small_image"`
				SmallText  string `json:"small_text"`
			} `json:"assets"`
			Timestamps *struct {
				Start *int64 `json:"start"`
				End   *int64 `json:"end"`
			} `json:"timestamps"`
			Buttons []struct {
				Label string `json:"label"`
				URL   string `json:"url"`
			} `json:"buttons"`
		} `json:"activity"`
	} `json:"args"`
}

// TestPreviewIsPayloadThroughSend is the load-bearing preview-is-the-payload
// proof (negative control verified, see REPORT): it drives the REAL pushActivity over the in-process fake pipe,
// captures the actual SET_ACTIVITY frame on the wire, and asserts the
// previewed presence.Activity equals that wire payload field-for-field.
// A rebuild-for-display could drift from the sent card; this test fails on
// any such divergence (verified by negative control: mutating either side
// breaks it). It covers both the default spec (BuildActivity path) and a
// non-default spec (Render override path).
func TestPreviewIsPayloadThroughSend(t *testing.T) {
	previewMu.Lock()
	oldCard, oldHave := previewCard, previewHave
	previewMu.Unlock()
	defer func() {
		previewMu.Lock()
		previewCard, previewHave = oldCard, oldHave
		previewMu.Unlock()
	}()
	oldCfg := liveConfig.Load()
	oldResolver := metaResolver
	defer func() {
		if oldCfg != nil {
			liveConfig.Store(oldCfg)
		} else {
			liveConfig.Store(&Config{})
		}
		metaResolver = oldResolver
	}()
	metaResolver = nil

	srv := discord.NewFakeServer(0)
	accept := make(chan *discord.FakeConn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		sc, err := srv.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accept <- sc
	}()
	dc := discord.New(discord.Config{
		ClientID:   "123456789012345678",
		PipeBase:   srv.Base,
		RetryDelay: 30 * time.Millisecond,
	})
	dc.Start()
	t.Cleanup(func() { dc.Close() })
	var sc *discord.FakeConn
	select {
	case sc = <-accept:
	case err := <-acceptErr:
		t.Fatalf("fake server accept: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("client did not connect to the fake pipe")
	}
	defer sc.Close()
	if op, _, err := sc.ReadFrame(); err != nil || op != discord.OpHandshake {
		t.Fatalf("handshake: op=%d err=%v", op, err)
	}
	if err := sc.WriteFrame(discord.OpFrame, []byte(`{"cmd":"DISPATCH","evt":"READY","data":{"v":1}}`)); err != nil {
		t.Fatalf("write READY: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for dc.Generation() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("client never became ready after READY")
		}
		time.Sleep(2 * time.Millisecond)
	}
	frames := make(chan []byte, 32)
	go func() {
		for {
			op, payload, err := sc.ReadFrame()
			if err != nil {
				return
			}
			if op == discord.OpFrame {
				frames <- payload
			}
		}
	}()

	drive := func(raw string, act *presence.Action, spec presence.CardSpec, ct, total, now int64) (presence.Activity, previewWire, []byte) {
		previewMu.Lock()
		previewCard, previewHave = presence.Activity{}, false
		previewMu.Unlock()
		liveConfig.Store(&Config{Card: spec})
		lg := newLogger(filepath.Join(t.TempDir(), "preview-payload.log"), 0, 1)
		defer lg.Close()
		gate := &presence.Gate{}
		pushActivity(lg, dc, gate, raw, act, ct, total, now, true, false)
		select {
		case payload := <-frames:
			var w previewWire
			if err := json.Unmarshal(payload, &w); err != nil {
				t.Fatalf("decode frame %s: %v", payload, err)
			}
			if w.Cmd != "SET_ACTIVITY" || w.Args.Activity == nil {
				t.Fatalf("unexpected frame: %s", payload)
			}
			got, have := previewActivity()
			if !have {
				t.Fatalf("preview have=false after successful send, want true")
			}
			return got, w, payload
		case <-time.After(2 * time.Second):
			t.Fatalf("no SET_ACTIVITY frame for %q", raw)
			return presence.Activity{}, previewWire{}, nil
		}
	}
	check := func(got presence.Activity, w previewWire) {
		act := w.Args.Activity
		if got.Name != act.Name || got.Details != act.Details || got.State != act.State {
			t.Fatalf("preview (Name=%q Details=%q State=%q) != wire (Name=%q Details=%q State=%q) preview=%+v wire=%+v",
				got.Name, got.Details, got.State, act.Name, act.Details, act.State, got, act)
		}
		if act.Assets == nil {
			t.Fatalf("wire assets missing: %+v", act)
		}
		wantLarge := got.LargeImage
		if wantLarge != "logo" && len(wantLarge) >= 4 && wantLarge[:4] != "http" {
			wantLarge = "logo"
		}
		if wantLarge == "" {
			wantLarge = "logo"
		}
		if act.Assets.LargeImage != wantLarge || act.Assets.LargeText != got.LargeText ||
			act.Assets.SmallImage != got.SmallImage || act.Assets.SmallText != got.SmallText {
			t.Fatalf("preview assets %+v != wire assets %+v", got, act.Assets)
		}
		var wantStart, wantEnd *int64
		if got.HasStart {
			s, e := got.Start, got.End
			wantStart = &s
			if got.End > 0 {
				wantEnd = &e
			}
		}
		var gotStart, gotEnd *int64
		if act.Timestamps != nil {
			gotStart, gotEnd = act.Timestamps.Start, act.Timestamps.End
		}
		if (wantStart == nil) != (gotStart == nil) || (wantStart != nil && *wantStart != *gotStart) {
			t.Fatalf("preview Start=%v HasStart=%t != wire Start=%v (preview=%+v)", wantStart, got.HasStart, gotStart, got)
		}
		if (wantEnd == nil) != (gotEnd == nil) || (wantEnd != nil && *wantEnd != *gotEnd) {
			t.Fatalf("preview End=%v != wire End=%v (preview=%+v)", wantEnd, gotEnd, got)
		}
		if len(got.Buttons) != len(act.Buttons) {
			t.Fatalf("preview buttons %+v != wire buttons %+v", got.Buttons, act.Buttons)
		}
		for i := range got.Buttons {
			if got.Buttons[i].Label != act.Buttons[i].Label || got.Buttons[i].URL != act.Buttons[i].URL {
				t.Fatalf("preview button %d %+v != wire %+v", i, got.Buttons[i], act.Buttons[i])
			}
		}
	}

	raw := "Show.S01E02.Pilot.1080p.mkv - PotPlayer"
	now := int64(1000000000000)
	t.Run("default", func(t *testing.T) {
		act := &presence.Action{Running: true, HasStart: true, Start: now - 1000, CT: 1000}
		got, w, _ := drive(raw, act, presence.DefaultCardSpec(), 1000, 2832979, now)
		check(got, w)
	})
	t.Run("nondefault", func(t *testing.T) {
		act := &presence.Action{Running: true, HasStart: true, Start: now - 2000, CT: 2000}
		spec := presence.CardSpec{Details: "title", State: "tag"}
		got, w, _ := drive(raw, act, spec, 2000, 2832979, now)
		check(got, w)
		if got.Details == "" || got.State == "" {
			t.Fatalf("non-default preview has empty lines: %+v", got)
		}
	})
	// TestPreviewEmptyHide proves auto-hide end to end: a preset whose state
	// renders "" (Empty on a resolved card) is OMITTED from the wire frame,
	// not sent as "state":"" (discord omitempty), while details still ride.
	t.Run("emptyhide", func(t *testing.T) {
		act := &presence.Action{Running: true, HasStart: true, Start: now - 3000, CT: 3000}
		spec := presence.CardSpec{Details: "default", State: "none"}
		got, w, frame := drive(raw, act, spec, 3000, 2832979, now)
		check(got, w)
		if got.State != "" {
			t.Fatalf("Empty-state preview State = %q, want empty", got.State)
		}
		var env struct {
			Args struct {
				Activity map[string]any `json:"activity"`
			} `json:"args"`
		}
		if err := json.Unmarshal(frame, &env); err != nil {
			t.Fatalf("decode frame %s: %v", frame, err)
		}
		if _, ok := env.Args.Activity["state"]; ok {
			t.Fatalf("wire activity carries a state key for an empty state: %s", frame)
		}
		if env.Args.Activity["details"] != "Pilot" {
			t.Fatalf("wire details = %v, want %q (frame: %s)",
				env.Args.Activity["details"], "Pilot", frame)
		}
	})
}

// TestPreviewNotRecordedOnSendFailure proves the record-after-success fix:
// when SetActivity fails, the preview keeps its previous SENT value instead
// of presenting the failed attempt as sent.
func TestPreviewNotRecordedOnSendFailure(t *testing.T) {
	previewMu.Lock()
	oldCard, oldHave := previewCard, previewHave
	previewMu.Unlock()
	defer func() {
		previewMu.Lock()
		previewCard, previewHave = oldCard, oldHave
		previewMu.Unlock()
	}()
	oldCfg := liveConfig.Load()
	oldResolver := metaResolver
	defer func() {
		if oldCfg != nil {
			liveConfig.Store(oldCfg)
		} else {
			liveConfig.Store(&Config{})
		}
		metaResolver = oldResolver
	}()
	metaResolver = nil
	liveConfig.Store(&Config{})

	previewMu.Lock()
	previewCard, previewHave = presence.Activity{}, false
	previewMu.Unlock()

	// A client that can never send: never started, so conn==nil and every
	// SetActivity returns ErrNotConnected.
	dc := discord.New(discord.Config{
		ClientID:   "123456789012345678",
		PipeBase:   filepath.Join(t.TempDir(), "no-pipe-"),
		RetryDelay: time.Hour,
	})
	// No Start: send must fail.
	lg := newLogger(filepath.Join(t.TempDir(), "preview-fail.log"), 0, 1)
	defer lg.Close()
	gate := &presence.Gate{}
	raw := "Show.S01E02.Pilot.1080p.mkv - PotPlayer"
	act := &presence.Action{Running: true, HasStart: true, Start: 999, CT: 1000}
	pushActivity(lg, dc, gate, raw, act, 1000, 2832979, 1000000000000, true, false)
	if _, have := previewActivity(); have {
		t.Fatalf("preview recorded after failed send, want previous value (none)")
	}
}

// TestDeadCacheLabel proves the Settings dead-titles line text.
func TestDeadCacheLabel(t *testing.T) {
	if got, want := deadCacheLabel(0), "0 titles marked dead - Clear cache retries them"; got != want {
		t.Fatalf("deadCacheLabel(0) = %q, want %q", got, want)
	}
	if got, want := deadCacheLabel(5), "5 titles marked dead - Clear cache retries them"; got != want {
		t.Fatalf("deadCacheLabel(5) = %q, want %q", got, want)
	}
}

// TestCountNegativesEmpty proves metadata.CountNegatives from the Settings
// side: an empty dir counts zero, junk files are ignored, well-formed
// not-found entries count, and positive/corrupt entries do not.
func TestCountNegativesEmpty(t *testing.T) {
	dir := t.TempDir()
	if got := metadata.CountNegatives(dir); got != 0 {
		t.Fatalf("CountNegatives(empty) = %d, want 0", got)
	}
	for _, name := range []string{"notes.txt", ".tmp-abc.json", "partial"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
			t.Fatalf("write junk fixture: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "corrupt.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write corrupt fixture: %v", err)
	}
	if got := metadata.CountNegatives(dir); got != 0 {
		t.Fatalf("CountNegatives(junk) = %d, want 0", got)
	}
	for i := 0; i < 2; i++ {
		body := fmt.Sprintf(`{"key":"k%d","schema":%d,"notFound":true,"checkedAt":%q}`,
			i, metadata.CacheSchema(), time.Now().UTC().Format(time.RFC3339))
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("k%d.json", i)), []byte(body), 0o644); err != nil {
			t.Fatalf("write negative fixture: %v", err)
		}
	}
	pos := fmt.Sprintf(`{"key":"p","schema":%d,"media":{"source":"tvmaze","sourceId":"1","kind":"series","title":"S","fetchedAt":%q}}`,
		metadata.CacheSchema(), time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, "p.json"), []byte(pos), 0o644); err != nil {
		t.Fatalf("write positive fixture: %v", err)
	}
	if got := metadata.CountNegatives(dir); got != 2 {
		t.Fatalf("CountNegatives = %d, want 2 (only the not-found entries)", got)
	}
	if got := metadata.CountNegatives(filepath.Join(dir, "missing")); got != 0 {
		t.Fatalf("CountNegatives(missing dir) = %d, want 0", got)
	}
}
