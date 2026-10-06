package main

import "testing"

// tray_test.go - proves the tray callback message extraction (go/tray.go).
// A real right-click must reach showTrayMenu: without NIM_SETVERSION the shell
// sends the legacy shape wParam=icon id, lParam=mouse message, and the old code
// wrongly read wParam. The fallback accepts the message in wParam too, and a
// callback carrying neither must open no menu.

func TestTrayMouseMessageLegacyShape(t *testing.T) {
	// Exactly what a real right-click on the icon delivers without
	// NIM_SETVERSION: wParam = trayUID (1), lParam = WM_RBUTTONUP.
	if got := trayMouseMessage(1, wmRButtonUp); got != wmRButtonUp {
		t.Fatalf("legacy right-click: trayMouseMessage(1, WM_RBUTTONUP) = 0x%04x, want 0x%04x", got, wmRButtonUp)
	}
}

func TestTrayMouseMessageV4Shape(t *testing.T) {
	// Defensive shape: the menu message carried in LOWORD(wParam), with lParam
	// holding coordinates rather than a message (LOWORD 0x0064 is not a menu
	// message, so only the wParam fallback can match).
	coords := uintptr(0x00C80064)
	if got := trayMouseMessage(wmContextMenu, coords); got != wmContextMenu {
		t.Fatalf("wParam shape: trayMouseMessage(WM_CONTEXTMENU, 0x%x) = 0x%04x, want 0x%04x", coords, got, wmContextMenu)
	}
}

func TestTrayMouseMessageNonMouseOpensNoMenu(t *testing.T) {
	for _, tc := range []struct {
		name           string
		wparam, lparam uintptr
	}{
		{"hover", 1, 0x0200},           // WM_MOUSEMOVE
		{"zero", 0, 0},                 // WM_NULL
		{"coords-only", 0, 0x00C80064}, // coordinate payload, no message
	} {
		if got := trayMouseMessage(tc.wparam, tc.lparam); opensTrayMenu(got) {
			t.Fatalf("%s: trayMouseMessage(0x%x, 0x%x) = 0x%04x opens a menu, want none", tc.name, tc.wparam, tc.lparam, got)
		}
	}
}
