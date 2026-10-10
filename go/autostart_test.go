package main

import "testing"

// autostart_test.go - proves the start-to-tray policy without a live window,
// registry writes, or network: only the autostart path (Run value carries
// --minimized) starts hidden; a manual double-click keeps the visible window.

func TestLaunchedViaAutostart(t *testing.T) {
	if launchedViaAutostart([]string{"potflix.exe"}) {
		t.Fatalf("manual argv detected as autostart")
	}
	if !launchedViaAutostart([]string{"potflix.exe", autostartArg}) {
		t.Fatalf("flagged argv not detected as autostart")
	}
}

func TestShouldShowMainWindow(t *testing.T) {
	if !shouldShowMainWindow([]string{"potflix.exe"}) {
		t.Fatalf("manual launch hides the window, want visible (first-run Add-folder flow)")
	}
	if shouldShowMainWindow([]string{"potflix.exe", autostartArg}) {
		t.Fatalf("autostart launch shows the window, want hidden to tray")
	}
}

func TestStartupCommandFor(t *testing.T) {
	exe := `C:\Program Files\Potflix\potflix.exe`
	want := `"` + exe + `" ` + autostartArg
	if got := startupCommandFor(exe); got != want {
		t.Fatalf("startupCommandFor = %q, want %q", got, want)
	}
}

func TestStartupValueEnabled(t *testing.T) {
	exe := `C:\Program Files\Potflix\potflix.exe`
	if !startupValueEnabled(startupCommandFor(exe), exe) {
		t.Fatalf("flagged Run value reads as disabled, want enabled")
	}
	if !startupValueEnabled(startupValueFor(exe), exe) {
		t.Fatalf("legacy bare-path Run value reads as disabled, want tolerant enabled")
	}
	if startupValueEnabled(`"C:\Other\app.exe"`, exe) {
		t.Fatalf("stale Run value reads as enabled, want off")
	}
	if startupValueEnabled("", exe) {
		t.Fatalf("empty Run value reads as enabled, want off")
	}
}
