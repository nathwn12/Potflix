package main

import (
	"strings"
	"testing"
)

// TestSettingsScanHitTest proves the SCAN segmented control resolves: the
// three segments hit-test back to their own ids, stay inside the client,
// and never overlap any existing control.
func TestSettingsScanHitTest(t *testing.T) {
	oldScan := scanSpeedState
	scanSpeedState = "balanced"
	defer func() { scanSpeedState = oldScan }()

	l := computeSettingsLayout(mainClientW, mainClientH, 96, false)
	if len(l.btns) != 9 {
		t.Fatalf("settings buttons = %d, want 9 (Back + 3 LIBRARY + 2 STARTUP + 3 SCAN)", len(l.btns))
	}
	for _, id := range []int{btnScanGentle, btnScanBalanced, btnScanTurbo} {
		rc, ok := l.settingsBtn(id)
		if !ok {
			t.Fatalf("SCAN segment id %d missing from settings layout", id)
		}
		if rc.right > mainClientW || rc.bottom > mainClientH || rc.left < 0 || rc.top < 0 {
			t.Fatalf("SCAN segment %d rect %+v escapes the %dx%d client", id, rc, mainClientW, mainClientH)
		}
		if got := buttonAtPoint(l.btns, (rc.left+rc.right)/2, (rc.top+rc.bottom)/2); got != id {
			t.Fatalf("SCAN segment %d center hits %d", id, got)
		}
	}
	// The three segments sit side by side in one row.
	g, _ := l.settingsBtn(btnScanGentle)
	b, _ := l.settingsBtn(btnScanBalanced)
	u, _ := l.settingsBtn(btnScanTurbo)
	if !(g.top == b.top && b.top == u.top && g.bottom == b.bottom && b.bottom == u.bottom) {
		t.Fatalf("SCAN segments not in one row: %+v %+v %+v", g, b, u)
	}
	if !(g.right <= b.left && b.right <= u.left) {
		t.Fatalf("SCAN segments not ordered gentle/balanced/turbo: %+v %+v %+v", g, b, u)
	}
	// No overlaps across the whole table (existing + SCAN).
	for i := 0; i < len(l.btns); i++ {
		for j := i + 1; j < len(l.btns); j++ {
			a, c := l.btns[i].rc, l.btns[j].rc
			if a.left < c.right && c.left < a.right && a.top < c.bottom && c.top < a.bottom {
				t.Fatalf("settings buttons %d and %d overlap: %+v vs %+v", l.btns[i].id, l.btns[j].id, a, c)
			}
		}
	}
	// SCAN lives below STARTUP (ends 204) and above the status line.
	for _, id := range []int{btnScanGentle, btnScanBalanced, btnScanTurbo} {
		rc, _ := l.settingsBtn(id)
		if rc.top < 204 || rc.bottom > 340 {
			t.Fatalf("SCAN segment %d rect %+v leaves the SCAN strip (want y 204..340)", id, rc)
		}
	}
}

// TestScanSegmentModel proves the pure segment label/selection model:
// labels, profile<->id round-trip, and accent-on-selected styling.
func TestScanSegmentModel(t *testing.T) {
	if got := scanSegmentLabel("gentle"); got != "Gentle" {
		t.Fatalf("label gentle = %q, want Gentle", got)
	}
	if got := scanSegmentLabel("balanced"); got != "Balanced" {
		t.Fatalf("label balanced = %q, want Balanced", got)
	}
	if got := scanSegmentLabel("turbo"); got != "Turbo" {
		t.Fatalf("label turbo = %q, want Turbo", got)
	}
	if got := scanSegmentLabel("bogus"); got != "Turbo" {
		t.Fatalf("label bogus = %q, want Turbo (normalised to the turbo default)", got)
	}
	for _, tc := range []struct {
		profile string
		id      int
	}{
		{"gentle", btnScanGentle},
		{"balanced", btnScanBalanced},
		{"turbo", btnScanTurbo},
	} {
		if got := scanButtonForProfile(tc.profile); got != tc.id {
			t.Fatalf("scanButtonForProfile(%q) = %d, want %d", tc.profile, got, tc.id)
		}
		if got := scanProfileForButton(tc.id); got != tc.profile {
			t.Fatalf("scanProfileForButton(%d) = %q, want %q", tc.id, got, tc.profile)
		}
	}
	if got := scanProfileForButton(btnBack); got != "" {
		t.Fatalf("scanProfileForButton(Back) = %q, want empty", got)
	}
	// Selection: only the current profile is accent-filled.
	for _, cur := range []string{"gentle", "balanced", "turbo"} {
		for _, seg := range []string{"gentle", "balanced", "turbo"} {
			want := btnSubtle
			if seg == cur {
				want = btnAccent
			}
			if got := scanSegmentStyle(seg, cur); got != want {
				t.Fatalf("scanSegmentStyle(%q, current=%q) = %d, want %d", seg, cur, got, want)
			}
		}
	}
	// No explicit choice (empty/bogus current) selects Turbo: the segment
	// model normalises onto the turbo default.
	for _, cur := range []string{"", "   ", "bogus"} {
		if got := scanButtonForProfile(cur); got != btnScanTurbo {
			t.Fatalf("scanButtonForProfile(%q) = %d, want Turbo (%d)", cur, got, btnScanTurbo)
		}
		if got := scanSegmentStyle("turbo", cur); got != btnAccent {
			t.Fatalf("scanSegmentStyle(turbo, current=%q) = %d, want accent-selected", cur, got)
		}
		if got := scanSegmentStyle("balanced", cur); got != btnSubtle {
			t.Fatalf("scanSegmentStyle(balanced, current=%q) = %d, want subtle", cur, got)
		}
	}
	// The hint states the default honestly.
	if !strings.Contains(scanHintText, "Turbo") || !strings.Contains(scanHintText, "(default)") {
		t.Fatalf("scanHintText = %q, want it to name Turbo as the default", scanHintText)
	}
}
