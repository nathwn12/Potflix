package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"potflix/internal/library"
)

// scan_status_test.go - proves the indexed Scan wiring in go/window.go
// without a live window: the progress/final formatters lead with unique-key
// counts (including the all-cached and nothing-to-fetch cases), huge counts
// elide rather than overflow the status line, and the options builder sets
// UseIndex, the real IndexPath, and Force per button.

func TestScanProgressTextFolders(t *testing.T) {
	p := library.Progress{KeysDone: 12, KeysTotal: 24, KeysFetched: 3, KeysCached: 5, FoldersScanned: 4, FoldersSkipped: 2}
	want := "Scanning 12/24 keys | 3 fetched - 5 cached | folders: 4 scanned, 2 skipped"
	if got := scanProgressText(p); got != want {
		t.Fatalf("scanProgressText = %q, want %q", got, want)
	}
}

func TestScanProgressTextAllCached(t *testing.T) {
	p := library.Progress{KeysDone: 24, KeysTotal: 24, KeysFetched: 0, KeysCached: 24, FoldersScanned: 6, FoldersSkipped: 1}
	got := scanProgressText(p)
	want := "Scanning 24/24 keys | 0 fetched - 24 cached | folders: 6 scanned, 1 skipped"
	if got != want {
		t.Fatalf("all-cached scanProgressText = %q, want %q", got, want)
	}
}

func TestScanProgressTextHugeElides(t *testing.T) {
	p := library.Progress{
		KeysDone: 12345678, KeysTotal: 98765432,
		KeysFetched: 123456, KeysCached: 12222222,
		FoldersScanned: 1234567, FoldersSkipped: 7654321,
	}
	got := scanProgressText(p)
	if strings.Contains(got, "12345678") || strings.Contains(got, "98765432") {
		t.Fatalf("huge scanProgressText shows full digits: %q (want compact k/M)", got)
	}
	if !strings.Contains(got, "folders:") || !strings.Contains(got, "scanned,") || !strings.Contains(got, "skipped") {
		t.Fatalf("huge scanProgressText lost folder counts: %q", got)
	}
	if len(got) > maxScanStatusLen {
		t.Fatalf("huge scanProgressText len=%d > %d (must elide, not overflow): %q", len(got), maxScanStatusLen, got)
	}
	// Extreme int64-scale counts stay bounded and keep the suffix.
	huge := library.Progress{
		KeysDone: 1 << 60, KeysTotal: 1 << 60,
		KeysFetched: 1 << 60, KeysCached: 0,
		FoldersScanned: 1 << 40, FoldersSkipped: 1 << 40,
	}
	gotHuge := scanProgressText(huge)
	if strings.Contains(gotHuge, fmt.Sprintf("%d", 1<<60)) {
		t.Fatalf("extreme scanProgressText shows full digits: %q", gotHuge)
	}
	if !strings.Contains(gotHuge, "folders:") {
		t.Fatalf("extreme scanProgressText lost folder counts: %q", gotHuge)
	}
	if len(gotHuge) > maxScanStatusLen {
		t.Fatalf("extreme scanProgressText len=%d > %d: %q", len(gotHuge), maxScanStatusLen, gotHuge)
	}
}

func TestScanDoneTextFolders(t *testing.T) {
	st := library.Stats{Keys: 24, KeysCached: 5, KeysFetched: 2, KeysNotFound: 1, Files: 10, FoldersScanned: 4, FoldersSkipped: 2}
	want := "Scan complete: 24 keys - 5 cached - 2 fetched - 1 not found | 10 files - folders: 4 scanned, 2 skipped"
	if got := scanDoneText("Scan", st); got != want {
		t.Fatalf("scanDoneText = %q, want %q", got, want)
	}
	// Refresh kind prefix is preserved.
	if got := scanDoneText("Refresh", st); !strings.HasPrefix(got, "Refresh complete: ") || !strings.Contains(got, "folders: 4 scanned, 2 skipped") {
		t.Fatalf("refresh scanDoneText = %q, want Refresh prefix + folder counts", got)
	}
}

func TestScanDoneTextAllCached(t *testing.T) {
	st := library.Stats{Keys: 5, KeysCached: 5, KeysFetched: 0, KeysNotFound: 0, Files: 5, FoldersScanned: 3, FoldersSkipped: 0}
	got := scanDoneText("Scan", st)
	want := "Scan complete: 5 keys - 5 cached - 0 fetched - 0 not found | 5 files - folders: 3 scanned, 0 skipped"
	if got != want {
		t.Fatalf("all-cached scanDoneText = %q, want %q", got, want)
	}
}

func TestScanDoneTextNothingToFetch(t *testing.T) {
	st := library.Stats{Keys: 7, KeysCached: 7, KeysFetched: 0, KeysNotFound: 0, Files: 9, FoldersScanned: 4, FoldersSkipped: 3}
	want := "Scan complete: nothing to fetch (7 cached) - 3 folders skipped"
	if got := scanDoneText("Scan", st); got != want {
		t.Fatalf("nothing-to-fetch scanDoneText = %q, want %q", got, want)
	}
	// Kind prefix carries through the nothing-to-do case.
	wantRefresh := "Refresh complete: nothing to fetch (7 cached) - 3 folders skipped"
	if got := scanDoneText("Refresh", st); got != wantRefresh {
		t.Fatalf("refresh nothing-to-fetch scanDoneText = %q, want %q", got, wantRefresh)
	}
}

func TestScanDoneTextHugeElides(t *testing.T) {
	st := library.Stats{
		Keys: 12345678, KeysCached: 12222222, KeysFetched: 123456,
		KeysNotFound: 999999, Files: 2345678,
		FoldersScanned: 1234567, FoldersSkipped: 7654321,
	}
	got := scanDoneText("Scan", st)
	if strings.Contains(got, "12345678") {
		t.Fatalf("huge scanDoneText shows full digits: %q (want compact k/M)", got)
	}
	if !strings.Contains(got, "folders:") || !strings.Contains(got, "scanned,") {
		t.Fatalf("huge scanDoneText lost folder counts: %q", got)
	}
	if len(got) > maxScanStatusLen {
		t.Fatalf("huge scanDoneText len=%d > %d (must elide, not overflow): %q", len(got), maxScanStatusLen, got)
	}
}

func TestScanStatusKeepsFoldersSuffix(t *testing.T) {
	// A pathological >160-char line must still keep the folders: suffix.
	long := strings.Repeat("k", maxScanStatusLen) + " folders: 4 scanned, 2 skipped"
	got := fitScanStatus(long)
	if len(got) > maxScanStatusLen {
		t.Fatalf("fitScanStatus len=%d > %d: %q", len(got), maxScanStatusLen, got)
	}
	if !strings.Contains(got, "folders: 4 scanned, 2 skipped") {
		t.Fatalf("fitScanStatus lost folders suffix: %q", got)
	}
	// The done formatter with an oversized kind keeps the suffix too.
	st := library.Stats{Keys: 24, KeysCached: 5, KeysFetched: 2, KeysNotFound: 1, Files: 10, FoldersScanned: 4, FoldersSkipped: 2}
	gotDone := scanDoneText(strings.Repeat("R", maxScanStatusLen), st)
	if len(gotDone) > maxScanStatusLen {
		t.Fatalf("long-kind scanDoneText len=%d > %d: %q", len(gotDone), maxScanStatusLen, gotDone)
	}
	if !strings.Contains(gotDone, "folders: 4 scanned, 2 skipped") {
		t.Fatalf("long-kind scanDoneText lost folders suffix: %q", gotDone)
	}
}

// TestBuildScanOptions proves the Scan/Refresh buttons (and the post-import
// auto-scan, which reuses startLibraryScan with force=false) build the same
// indexed options: UseIndex on, the real IndexPath/CacheDir, Force per button.
func TestBuildScanOptions(t *testing.T) {
	dirs := []string{`C:\Media\Movies`, `D:\Shows`}
	scan := buildScanOptions(dirs, false, nil)
	if !scan.UseIndex {
		t.Fatalf("Scan options UseIndex = false, want true (folder index in use)")
	}
	if scan.IndexPath != ScanIndexPath() {
		t.Fatalf("Scan IndexPath = %q, want ScanIndexPath() %q", scan.IndexPath, ScanIndexPath())
	}
	if scan.CacheDir != MetadataDir() {
		t.Fatalf("Scan CacheDir = %q, want MetadataDir() %q", scan.CacheDir, MetadataDir())
	}
	if scan.Force {
		t.Fatalf("Scan Force = true, want false (incremental)")
	}
	if len(scan.Dirs) != 2 || scan.Dirs[0] != dirs[0] || scan.Dirs[1] != dirs[1] {
		t.Fatalf("Scan Dirs = %q, want %q", scan.Dirs, dirs)
	}

	refresh := buildScanOptions(dirs, true, nil)
	if !refresh.UseIndex || refresh.IndexPath != ScanIndexPath() || refresh.CacheDir != MetadataDir() {
		t.Fatalf("Refresh options = %+v, want UseIndex + real IndexPath/CacheDir", refresh)
	}
	if !refresh.Force {
		t.Fatalf("Refresh Force = false, want true (walk everything + re-fetch)")
	}
}

// TestScanCancelledText proves the user-cancel summary: mid-scan numbers
// render as "Scan cancelled - done/total keys done, progress kept" with the
// existing compact counts, capped like every other status line.
func TestScanCancelledText(t *testing.T) {
	got := scanCancelledText(12, 24)
	want := "Scan cancelled - 12/24 keys done, progress kept"
	if got != want {
		t.Fatalf("scanCancelledText(12,24) = %q, want %q", got, want)
	}
	// Huge counts elide through formatScanCount, never overflow the line.
	gotHuge := scanCancelledText(12345678, 98765432)
	if strings.Contains(gotHuge, "12345678") || strings.Contains(gotHuge, "98765432") {
		t.Fatalf("huge scanCancelledText shows full digits: %q (want compact k/M)", gotHuge)
	}
	if !strings.HasPrefix(gotHuge, "Scan cancelled - ") || !strings.HasSuffix(gotHuge, "progress kept") {
		t.Fatalf("huge scanCancelledText lost cancel framing: %q", gotHuge)
	}
	if len(gotHuge) > maxScanStatusLen {
		t.Fatalf("huge scanCancelledText len=%d > %d: %q", len(gotHuge), maxScanStatusLen, gotHuge)
	}
}

// TestScanResultCancelledNeverFails proves a Cancelled result lands on the
// cancelled message, never on the error path - even when an error is also
// present, and even when the error alone is a context cancellation.
func TestScanResultCancelledNeverFails(t *testing.T) {
	cancelled := library.Stats{Keys: 24, KeysDone: 12, Cancelled: true}
	got := scanResultText("Scan", cancelled, nil)
	want := scanCancelledText(12, 24)
	if got != want {
		t.Fatalf("cancelled scanResultText = %q, want %q", got, want)
	}
	if strings.Contains(got, "failed") {
		t.Fatalf("cancelled scanResultText reports failure: %q", got)
	}
	// A Cancelled result paired with an error still reports cancelled.
	gotBoth := scanResultText("Scan", cancelled, errors.New("boom"))
	if gotBoth != want {
		t.Fatalf("cancelled+error scanResultText = %q, want %q", gotBoth, want)
	}
	// A bare context-cancel error (no Cancelled flag) is also a cancel.
	bare := library.Stats{Keys: 10, KeysDone: 4}
	gotCtx := scanResultText("Scan", bare, context.Canceled)
	if gotCtx != scanCancelledText(4, 10) {
		t.Fatalf("context-canceled scanResultText = %q, want %q", gotCtx, scanCancelledText(4, 10))
	}
	// The normal paths are unchanged: success completes, real errors fail.
	ok := library.Stats{Keys: 24, KeysCached: 5, KeysFetched: 2, KeysNotFound: 1, Files: 10, FoldersScanned: 4, FoldersSkipped: 2}
	if got := scanResultText("Scan", ok, nil); got != scanDoneText("Scan", ok) {
		t.Fatalf("success scanResultText = %q, want done text %q", got, scanDoneText("Scan", ok))
	}
	if got := scanResultText("Scan", ok, errors.New("disk gone")); !strings.Contains(got, "failed") {
		t.Fatalf("error scanResultText = %q, want failure", got)
	}
}

// TestScanFailedSuffix proves the counters-owner patch: the failed count
// shows ONLY when non-zero (a clean line is unchanged), and a run with
// failures never reports "nothing to fetch".
func TestScanFailedSuffix(t *testing.T) {
	// Clean lines are byte-identical to the pre-patch shape.
	clean := library.Progress{KeysDone: 12, KeysTotal: 24, KeysFetched: 3, KeysCached: 5, FoldersScanned: 4, FoldersSkipped: 2}
	if got := scanProgressText(clean); got != "Scanning 12/24 keys | 3 fetched - 5 cached | folders: 4 scanned, 2 skipped" {
		t.Fatalf("clean progress = %q", got)
	}
	// Non-zero failures append before the folders pipe.
	bad := clean
	bad.KeysFailed = 2
	if got := scanProgressText(bad); got != "Scanning 12/24 keys | 3 fetched - 5 cached - 2 failed | folders: 4 scanned, 2 skipped" {
		t.Fatalf("failed progress = %q", got)
	}
	st := library.Stats{Keys: 7, KeysCached: 7, KeysFetched: 0, KeysNotFound: 0, Files: 9, FoldersScanned: 4, FoldersSkipped: 3}
	if got := scanDoneText("Scan", st); got != "Scan complete: nothing to fetch (7 cached) - 3 folders skipped" {
		t.Fatalf("clean nothing-to-fetch = %q", got)
	}
	stBad := st
	stBad.KeysFailed = 1
	if got := scanDoneText("Scan", stBad); strings.Contains(got, "nothing to fetch") {
		t.Fatalf("failed run reports nothing-to-fetch: %q", got)
	} else if !strings.Contains(got, "1 failed") {
		t.Fatalf("failed done lacks the suffix: %q", got)
	}
}

// TestScanControlState proves the control-state model: idle shows an
// enabled Scan, a running scan shows Cancel (Refresh disabled), and after
// the cancelled run lands the control is back to an enabled Scan.
func TestScanControlState(t *testing.T) {
	// Idle: Scan caption, everything enabled.
	if got := scanButtonLabel(false); got != "Scan" {
		t.Fatalf("idle scan label = %q, want %q", got, "Scan")
	}
	if !scanRefreshEnabled(false) {
		t.Fatalf("idle Refresh enabled = false, want true")
	}
	// Running: the Scan rect shows Cancel, Refresh is disabled.
	if got := scanButtonLabel(true); got != "Cancel" {
		t.Fatalf("running scan label = %q, want %q", got, "Cancel")
	}
	if scanRefreshEnabled(true) {
		t.Fatalf("running Refresh enabled = true, want false (disabled in flight)")
	}
	// Cancelled (run landed, idle again): Scan re-enabled.
	if got := scanButtonLabel(false); got != "Scan" {
		t.Fatalf("cancelled scan label = %q, want %q (re-enabled)", got, "Scan")
	}
	if !scanRefreshEnabled(false) {
		t.Fatalf("cancelled Refresh enabled = false, want true (re-enabled)")
	}
}
