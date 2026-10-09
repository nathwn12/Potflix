package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"potflix/internal/bundle"
	"potflix/internal/library"
	"potflix/internal/metadata"
)

// settings_sections.go - the Settings view's LIBRARY + SCAN + STARTUP sections.
//
// Same owner-drawn window, palette, fonts, DPI scaling, hover/double-buffering
// as window.go; this file only adds behaviour behind new rect-table ids:
//   - LIBRARY: Clear cache (danger-styled) / Export... / Import... backed by
//     internal/bundle over MetadataDir(), plus the ScanIndexPath() index file.
//     Import is followed by an incremental library.Scan via the SAME
//     goroutine + PostMessage path as the Scan button (startLibraryScan).
//   - STARTUP: Start-with-Windows toggle via HKCU Run (advapi32, stdlib-only
//     LazyDLL) showing the ACTUAL registry state, plus Open-log-folder via
//     ShellExecuteW.
//
// All win32/dialog/registry work lives here so window.go keeps only the
// layout rects, paint labels, and action routing. Status strings and dialog
// defaults are pure helpers so tests drive them without a live window.

// appVersion stamps bundle headers. It tracks the installer version
// (tools\potflix.iss MyAppVersion); the app has no other version const.
const appVersion = "1.0.0"

// ---------------------------------------------------------------------------
// Status strings: pure mappers from results to the status line.
// ---------------------------------------------------------------------------

// clearCacheStatus maps a Clear outcome to the status line.
func clearCacheStatus(titles, records int) string {
	return fmt.Sprintf("Cleared %d cached titles - %d folder records", titles, records)
}

// exportBundleStatus maps an Export outcome to the status line. The dest
// path is unbounded, so the string is capped like every other status line
// (the painter also ellipsizes, but the cap keeps the string itself sane).
func exportBundleStatus(entries int, dest string) string {
	return fitScanStatus(fmt.Sprintf("Exported %d titles -> %s", entries, dest))
}

// importBundleStatus maps an Import outcome to the status line: "skipped"
// covers both schema-mismatched and invalid entries.
func importBundleStatus(st bundle.ImportStats) string {
	return fmt.Sprintf("Imported: %d added, %d already present, %d skipped",
		st.Added, st.Present, st.SkippedSchema+st.Invalid)
}

// exportDefaultName is the Save-As default: potflix-metadata-YYYY-MM-DD.json.
func exportDefaultName(t time.Time) string {
	return "potflix-metadata-" + t.Format("2006-01-02") + ".json"
}

// startupToggleLabel renders the toggle state as an ASCII checkbox (no font
// dependency): "[x] Start with Windows" when on, "[ ] ..." when off.
func startupToggleLabel(on bool) string {
	if on {
		return "[x] Start with Windows"
	}
	return "[ ] Start with Windows"
}

// ---------------------------------------------------------------------------
// SCAN speed section: three-way segmented control (Gentle / Balanced /
// Turbo) over the metadata per-host rate limiter. Pure label/selection
// model plus the live apply path (save + SetRateProfile, no restart).
// ---------------------------------------------------------------------------

// scanSpeedState is the current SCAN selection shown in the Settings view.
// Refreshed from the config store each time the view opens (enterSettings)
// and on every segment click. UI thread only, like startupState.
var scanSpeedState = ScanSpeedTurbo

// scanHintText is the muted hint drawn under the SCAN segments.
const scanHintText = "Turbo = each provider's published limit (default). Balanced = half speed, Gentle = quarter speed - pick one if you want to be gentler on the services."

// scanSegmentLabel renders a profile id as its segment caption.
func scanSegmentLabel(profile string) string {
	switch NormalizeScanSpeed(profile) {
	case ScanSpeedTurbo:
		return "Turbo"
	case ScanSpeedGentle:
		return "Gentle"
	default:
		return "Balanced"
	}
}

// scanProfileForButton maps a SCAN segment button id to its profile id,
// or "" for any other id.
func scanProfileForButton(id int) string {
	switch id {
	case btnScanGentle:
		return ScanSpeedGentle
	case btnScanBalanced:
		return ScanSpeedBalanced
	case btnScanTurbo:
		return ScanSpeedTurbo
	default:
		return ""
	}
}

// scanButtonForProfile maps a profile id to its segment button id.
func scanButtonForProfile(profile string) int {
	switch NormalizeScanSpeed(profile) {
	case ScanSpeedTurbo:
		return btnScanTurbo
	case ScanSpeedGentle:
		return btnScanGentle
	default:
		return btnScanBalanced
	}
}

// scanSegmentStyle renders the segmented-control selection state in the
// existing button-style language: the selected segment is accent-filled,
// the rest are subtle pills (hover/press handled by paintButton).
func scanSegmentStyle(seg, current string) int {
	if NormalizeScanSpeed(seg) == NormalizeScanSpeed(current) {
		return btnAccent
	}
	return btnSubtle
}

// deadCacheCount is the last-measured CountNegatives value shown in the
// Settings view. Refreshed from disk each time the view opens
// (refreshDeadCacheCount, called from enterSettings) and never during
// paint, so WM_PAINT stays I/O-free. UI thread only, like startupState.
var deadCacheCount int

// deadCacheLabel renders the count as one muted line under the SCAN hint:
// "N titles marked dead - Clear cache retries them".
func deadCacheLabel(n int) string {
	return fmt.Sprintf("%d titles marked dead - Clear cache retries them", n)
}

// refreshDeadCacheCount re-reads the not-found entry count from the live
// metadata cache dir.
func refreshDeadCacheCount() {
	deadCacheCount = metadata.CountNegatives(MetadataDir())
}

// onSelectScanSpeed saves profile AND applies it live via SetRateProfile
// (no restart). The value is normalised first so a bad value can never
// reach the limiter. Only presence is logged - there is no secret here.
func onSelectScanSpeed(profile string) {
	profile = NormalizeScanSpeed(profile)
	scanSpeedState = profile
	cfg := loadConfig()
	cfg.ScanSpeed = profile
	if err := saveConfig(cfg); err != nil {
		appLogf("settings: scan speed save failed: %s", err)
		setStatusText("Save failed.")
		return
	}
	liveConfig.Store(&cfg)
	metadata.SetRateProfile(profile)
	setStatusText("Scan speed: " + scanSegmentLabel(profile))
	appLogf("settings: scan speed %s", profile)
}

// ---------------------------------------------------------------------------
// Window-free cores: take explicit dirs so tests run on temp dirs.
// ---------------------------------------------------------------------------

// clearLibraryCache deletes the bundle cache files under metaDir plus the
// scan index file at indexPath. It reports removed cache files and the folder
// record count seen before deletion. A missing index is not an error.
func clearLibraryCache(metaDir, indexPath string) (titles, records int, err error) {
	titles, err = bundle.Clear(metaDir)
	if err != nil {
		return titles, 0, err
	}
	records = library.LoadScanIndex(indexPath).Len()
	if rerr := os.Remove(indexPath); rerr != nil && !os.IsNotExist(rerr) {
		return titles, records, rerr
	}
	return titles, records, nil
}

// exportBundleTo writes one bundle of metaDir to dest.
func exportBundleTo(metaDir, dest string, schema int, ver string) (int, error) {
	f, err := os.Create(dest)
	if err != nil {
		return 0, err
	}
	st, ever := bundle.Export(metaDir, f, schema, ver)
	if cerr := f.Close(); ever == nil {
		ever = cerr
	}
	if ever != nil {
		return 0, ever
	}
	return st.Entries, nil
}

// importBundleFrom merges the bundle at src into metaDir (fill-missing-only).
func importBundleFrom(metaDir, src string, schema int) (bundle.ImportStats, error) {
	f, err := os.Open(src)
	if err != nil {
		return bundle.ImportStats{}, err
	}
	defer f.Close()
	return bundle.Import(metaDir, f, schema)
}

// ---------------------------------------------------------------------------
// Native dialogs: MessageBoxW confirm, comdlg32 Save-As / Open.
// ---------------------------------------------------------------------------

const (
	mbYesNo       = 0x00000004
	mbIconWarning = 0x00000030
	idYes         = 6
)

var procMessageBoxW = user32.NewProc("MessageBoxW")

var comdlg32 = syscall.NewLazyDLL("comdlg32.dll")

var (
	procGetSaveFileNameW = comdlg32.NewProc("GetSaveFileNameW")
	procGetOpenFileNameW = comdlg32.NewProc("GetOpenFileNameW")
)

// comdlg32 open/save flags.
const (
	ofnOverwritePrompt = 0x00000002
	ofnNoChangeDir     = 0x00000008
	ofnFileMustExist   = 0x00001000
	ofnPathMustExist   = 0x00000800
	ofnExplorer        = 0x00080000
)

// openFileName mirrors OPENFILENAMEW on amd64 (field order == ABI order).
type openFileName struct {
	lStructSize       uint32
	hwndOwner         uintptr
	hInstance         uintptr
	lpstrFilter       *uint16
	lpstrCustomFilter *uint16
	nMaxCustFilter    uint32
	nFilterIndex      uint32
	lpstrFile         *uint16
	nMaxFile          uint32
	lpstrFileTitle    *uint16
	nMaxFileTitle     uint32
	lpstrInitialDir   *uint16
	lpstrTitle        *uint16
	flags             uint32
	nFileOffset       uint16
	nFileExtension    uint16
	lpstrDefExt       *uint16
	lCustData         uintptr
	lpfnHook          uintptr
	lpTemplateName    *uint16
	pvReserved        uintptr
	dwReserved        uint32
	flagsEx           uint32
}

// utf16Filter encodes a comdlg32 filter string ("label\x00glob\x00...\x00"
// with a double-NUL terminator); syscall.StringToUTF16 cannot be used
// because it panics on interior NULs. The filter text is ASCII-only, so a
// direct rune->uint16 encoding is exact.
func utf16Filter(s string) []uint16 {
	u := make([]uint16, 0, len(s)+2)
	for _, r := range s {
		u = append(u, uint16(r))
	}
	return append(u, 0)
}

// jsonBundleFilter is the shared Save-As/Open filter.
func jsonBundleFilter() []uint16 {
	return utf16Filter("Potflix metadata (*.json)\x00*.json\x00All files (*.*)\x00*.*\x00")
}

// desktopDir is the Save-As default directory: ~/Desktop, or "" when the
// home dir is unknown (comdlg32 then falls back to its own default).
func desktopDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Join(h, "Desktop")
	}
	return ""
}

// confirmClearCache shows the native warning dialog first: Yes deletes,
// anything else aborts. Counts are pre-delete (bundle.Count + index Len).
func confirmClearCache(owner uintptr, titles, records int) bool {
	text := fmt.Sprintf("Delete %d cached titles and the folder scan index (%d folder records)? They will be fetched again from the internet. Your folders are kept.",
		titles, records)
	t, _ := syscall.UTF16PtrFromString(text)
	c, _ := syscall.UTF16PtrFromString("Potflix - Clear cache")
	r, _, _ := procMessageBoxW.Call(owner,
		uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)),
		uintptr(mbYesNo|mbIconWarning))
	return r == idYes
}

// saveBundleDialog opens the native Save-As dialog (default dir Desktop,
// default name potflix-metadata-YYYY-MM-DD.json); "" means cancelled.
func saveBundleDialog(owner uintptr) string {
	filter := jsonBundleFilter()
	file := make([]uint16, 260)
	copy(file, syscall.StringToUTF16(exportDefaultName(time.Now())))
	initDir := syscall.StringToUTF16(desktopDir())
	title, _ := syscall.UTF16PtrFromString("Export Potflix metadata")
	defExt, _ := syscall.UTF16PtrFromString("json")
	ofn := openFileName{
		lpstrFilter:     &filter[0],
		lpstrFile:       &file[0],
		nMaxFile:        uint32(len(file)),
		lpstrInitialDir: &initDir[0],
		lpstrTitle:      title,
		flags:           ofnExplorer | ofnNoChangeDir | ofnPathMustExist | ofnOverwritePrompt,
		lpstrDefExt:     defExt,
	}
	ofn.lStructSize = uint32(unsafe.Sizeof(ofn))
	r, _, _ := procGetSaveFileNameW.Call(uintptr(unsafe.Pointer(&ofn)))
	if r == 0 {
		return ""
	}
	return syscall.UTF16ToString(file)
}

// openBundleDialog opens the native Open dialog filtered to *.json;
// "" means cancelled.
func openBundleDialog(owner uintptr) string {
	filter := jsonBundleFilter()
	file := make([]uint16, 260)
	initDir := syscall.StringToUTF16(desktopDir())
	title, _ := syscall.UTF16PtrFromString("Import Potflix metadata")
	ofn := openFileName{
		lpstrFilter:     &filter[0],
		lpstrFile:       &file[0],
		nMaxFile:        uint32(len(file)),
		lpstrInitialDir: &initDir[0],
		lpstrTitle:      title,
		flags:           ofnExplorer | ofnNoChangeDir | ofnFileMustExist | ofnPathMustExist,
	}
	ofn.lStructSize = uint32(unsafe.Sizeof(ofn))
	r, _, _ := procGetOpenFileNameW.Call(uintptr(unsafe.Pointer(&ofn)))
	if r == 0 {
		return ""
	}
	return syscall.UTF16ToString(file)
}

// ---------------------------------------------------------------------------
// STARTUP: HKCU\...\Run value "Potflix" = "<exe path>", via advapi32
// (stdlib-only LazyDLL; no reg.exe fallback needed).
// ---------------------------------------------------------------------------

const (
	hkeyCurrentUser = 0x80000001
	keyQueryValue   = 0x0001
	keySetValue     = 0x0002
	regSZ           = 1

	startupRunSubkey = `Software\Microsoft\Windows\CurrentVersion\Run`
	startupRunValue  = "Potflix"
)

var advapi32 = syscall.NewLazyDLL("advapi32.dll")

var (
	procRegOpenKeyExW    = advapi32.NewProc("RegOpenKeyExW")
	procRegQueryValueExW = advapi32.NewProc("RegQueryValueExW")
	procRegSetValueExW   = advapi32.NewProc("RegSetValueExW")
	procRegDeleteValueW  = advapi32.NewProc("RegDeleteValueW")
	procRegCloseKey      = advapi32.NewProc("RegCloseKey")
)

// startupState is the ACTUAL registry state, refreshed from HKCU each time
// the Settings view opens (enterSettings) and after every toggle. UI thread
// only.
var startupState bool

// startupValueFor renders the Run value for exe: the quoted path.
func startupValueFor(exe string) string {
	return `"` + exe + `"`
}

// startupExePath is the current executable path for the Run value.
func startupExePath() string {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return ""
	}
	return exe
}

// openRunKey opens the HKCU Run subkey with access and returns the handle;
// the caller must RegCloseKey it.
func openRunKey(access uint32) (uintptr, bool) {
	sub, _ := syscall.UTF16PtrFromString(startupRunSubkey)
	var hk uintptr
	r, _, _ := procRegOpenKeyExW.Call(hkeyCurrentUser,
		uintptr(unsafe.Pointer(sub)), 0, uintptr(access),
		uintptr(unsafe.Pointer(&hk)))
	if r != 0 || hk == 0 {
		return 0, false
	}
	return hk, true
}

// readStartupValue returns the current Potflix Run value, if present.
func readStartupValue() (string, bool) {
	hk, ok := openRunKey(keyQueryValue)
	if !ok {
		return "", false
	}
	defer procRegCloseKey.Call(hk)
	name, _ := syscall.UTF16PtrFromString(startupRunValue)
	var typ, size uint32
	r, _, _ := procRegQueryValueExW.Call(hk, uintptr(unsafe.Pointer(name)),
		0, uintptr(unsafe.Pointer(&typ)), 0, uintptr(unsafe.Pointer(&size)))
	if r != 0 || typ != regSZ || size == 0 || size > 4096 {
		return "", false
	}
	buf := make([]uint16, size/2+1)
	r, _, _ = procRegQueryValueExW.Call(hk, uintptr(unsafe.Pointer(name)),
		0, uintptr(unsafe.Pointer(&typ)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if r != 0 {
		return "", false
	}
	return syscall.UTF16ToString(buf), true
}

// queryStartupEnabled reports the ACTUAL current state: the Run value exists
// and points at this executable. A stale value pointing elsewhere counts as
// off (toggling on overwrites it).
func queryStartupEnabled() bool {
	exe := startupExePath()
	if exe == "" {
		return false
	}
	got, ok := readStartupValue()
	return ok && got == startupValueFor(exe)
}

// setStartupEnabled writes (on) or deletes (off) the Run value. Deleting a
// missing value counts as success (already off).
func setStartupEnabled(on bool) error {
	hk, ok := openRunKey(keySetValue)
	if !ok {
		return fmt.Errorf("startup: cannot open Run key")
	}
	defer procRegCloseKey.Call(hk)
	name, _ := syscall.UTF16PtrFromString(startupRunValue)
	if !on {
		r, _, _ := procRegDeleteValueW.Call(hk, uintptr(unsafe.Pointer(name)))
		if r != 0 && r != 2 { // 2 == ERROR_FILE_NOT_FOUND: already off
			return fmt.Errorf("startup: cannot delete Run value (%d)", r)
		}
		return nil
	}
	exe := startupExePath()
	if exe == "" {
		return fmt.Errorf("startup: cannot resolve exe path")
	}
	data := append(syscall.StringToUTF16(startupValueFor(exe)), 0)
	r, _, _ := procRegSetValueExW.Call(hk, uintptr(unsafe.Pointer(name)),
		0, regSZ, uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)*2))
	if r != 0 {
		return fmt.Errorf("startup: cannot write Run value (%d)", r)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Handlers: invoked from doSettingsAction (UI thread).
// ---------------------------------------------------------------------------

// onClearCache confirms natively, then clears bundle.Clear(metaDir) plus the
// scan index file, and reports counts in the status line.
func onClearCache(hwnd uintptr) {
	meta, idx := MetadataDir(), ScanIndexPath()
	titles, _ := bundle.Count(meta)
	records := library.LoadScanIndex(idx).Len()
	if !confirmClearCache(hwnd, titles, records) {
		return
	}
	got, recs, err := clearLibraryCache(meta, idx)
	if err != nil {
		appLogf("library: clear failed: %s", err)
		setStatusText(fitScanStatus("Clear failed: " + err.Error()))
		return
	}
	appLogf("library: cleared (titles=%d records=%d)", got, recs)
	setStatusText(clearCacheStatus(got, recs))
}

// onExportBundle prompts for a Save-As path, writes the bundle, and reports.
// The schema passed is metadata.CacheSchema(), the value the app already
// knows - never a local duplicate.
func onExportBundle(hwnd uintptr) {
	dest := saveBundleDialog(hwnd)
	if dest == "" {
		return // cancelled
	}
	n, err := exportBundleTo(MetadataDir(), dest, metadata.CacheSchema(), appVersion)
	if err != nil {
		appLogf("library: export failed: %s", err)
		setStatusText(fitScanStatus("Export failed: " + err.Error()))
		return
	}
	appLogf("library: exported (entries=%d)", n)
	setStatusText(exportBundleStatus(n, dest))
}

// onImportBundle prompts for a bundle file, merges it fill-missing-only,
// reports added/present/skipped, then starts an incremental scan on the SAME
// goroutine + PostMessage progress path as the Scan button so the missing
// keys are fetched online.
func onImportBundle(hwnd uintptr) {
	src := openBundleDialog(hwnd)
	if src == "" {
		return // cancelled
	}
	st, err := importBundleFrom(MetadataDir(), src, metadata.CacheSchema())
	if err != nil {
		appLogf("library: import failed: %s", err)
		setStatusText(fitScanStatus("Import failed: " + err.Error()))
		return
	}
	appLogf("library: import (added=%d present=%d skipped-schema=%d invalid=%d total=%d)",
		st.Added, st.Present, st.SkippedSchema, st.Invalid, st.Total)
	setStatusText(importBundleStatus(st))
	startLibraryScan(hwnd, false)
}

// onToggleStartup flips the Run value, re-reads the ACTUAL state, persists
// the config flag to match, and reports on/off.
func onToggleStartup() {
	if err := setStartupEnabled(!startupState); err != nil {
		appLogf("startup: toggle failed: %s", err)
		setStatusText("Start with Windows failed.")
		return
	}
	startupState = queryStartupEnabled()
	cfg := loadConfig()
	cfg.StartWithWindows = startupState
	if err := saveConfig(cfg); err != nil {
		appLogf("startup: save failed: %s", err)
	}
	if startupState {
		setStatusText("Start with Windows: on")
	} else {
		setStatusText("Start with Windows: off")
	}
	appLogf("startup: enabled=%t", startupState)
}

// onOpenLogFolder opens the directory holding the exe (where
// potflix-debug.log lives) in Explorer.
func onOpenLogFolder() {
	dir, _ := syscall.UTF16PtrFromString(exeDir())
	op, _ := syscall.UTF16PtrFromString("open")
	procShellExecuteW.Call(0, uintptr(unsafe.Pointer(op)),
		uintptr(unsafe.Pointer(dir)), 0, 0, swShowNormal)
	appLogf("startup: opened log folder")
}
