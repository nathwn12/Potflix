package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"potflix/internal/bundle"
	"potflix/internal/metadata"
)

// settings_sections_test.go - proves the Settings LIBRARY + STARTUP seams
// (go/settings_sections.go, go/config.go additions) without a live window,
// registry writes, dialogs, or network: status-string mappers, dialog
// defaults, the window-free clear/export/import cores on temp dirs, the
// StartWithWindows config round-trip, and the MetadataDir/ScanIndexPath
// conventions.

func TestExportDefaultName(t *testing.T) {
	got := exportDefaultName(time.Date(2026, time.October, 9, 12, 0, 0, 0, time.UTC))
	if got != "potflix-metadata-2026-10-09.json" {
		t.Fatalf("exportDefaultName = %q, want potflix-metadata-2026-10-09.json", got)
	}
}

func TestImportBundleStatus(t *testing.T) {
	st := bundle.ImportStats{Added: 2, Present: 5, SkippedSchema: 1, Invalid: 3, Total: 11}
	if got := importBundleStatus(st); got != "Imported: 2 added, 5 already present, 4 skipped" {
		t.Fatalf("importBundleStatus = %q, want added/present/skipped summary", got)
	}
	zero := importBundleStatus(bundle.ImportStats{})
	if zero != "Imported: 0 added, 0 already present, 0 skipped" {
		t.Fatalf("importBundleStatus(zero) = %q", zero)
	}
}

func TestClearExportStatusStrings(t *testing.T) {
	if got := clearCacheStatus(383, 716); got != "Cleared 383 cached titles - 716 folder records" {
		t.Fatalf("clearCacheStatus = %q", got)
	}
	if got := exportBundleStatus(3, `C:\Users\x\Desktop\potflix-metadata-2026-10-09.json`); !strings.HasPrefix(got, "Exported 3 titles -> ") {
		t.Fatalf("exportBundleStatus = %q", got)
	}
}

func TestStartupToggleLabel(t *testing.T) {
	if got := startupToggleLabel(true); got != "[x] Start with Windows" {
		t.Fatalf("startup on label = %q", got)
	}
	if got := startupToggleLabel(false); got != "[ ] Start with Windows" {
		t.Fatalf("startup off label = %q", got)
	}
}

func TestStartupValueFor(t *testing.T) {
	exe := `C:\Program Files\Potflix\potflix.exe`
	if got := startupValueFor(exe); got != `"`+exe+`"` {
		t.Fatalf("startupValueFor = %q, want the quoted exe path", got)
	}
}

// TestBundleRuleTracksMetadata proves the bundle filename rule follows the
// app's live rule: metadata.CacheFileNameFor (owned by the metadata package)
// yields a 16-hex .json name that bundle.Count/Export/Clear all honour.
func TestBundleRuleTracksMetadata(t *testing.T) {
	key := "movie|tracked|2001"
	name := metadata.CacheFileNameFor(key)
	if len(name) != 16+len(".json") || !strings.HasSuffix(name, ".json") {
		t.Fatalf("CacheFileNameFor(%q) = %q, want 16 hex + .json", key, name)
	}
	dir := t.TempDir()
	payload, err := json.Marshal(map[string]any{"key": key, "schema": metadata.CacheSchema(), "media": json.RawMessage(`{"title":"T"}`)})
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), payload, 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	if c, err := bundle.Count(dir); err != nil || c != 1 {
		t.Fatalf("Count = %d, %v; want 1 (live-rule file honoured)", c, err)
	}
	if removed, err := bundle.Clear(dir); err != nil || removed != 1 {
		t.Fatalf("Clear = (%d, %v); want (1, nil)", removed, err)
	}
}

func TestMetadataDirAndScanIndexPath(t *testing.T) {
	meta := MetadataDir()
	if !strings.HasSuffix(meta, filepath.Join("Potflix", "meta")) {
		t.Fatalf("MetadataDir = %q, want ...\\Potflix\\meta (no hardcoded %%LOCALAPPDATA%%)", meta)
	}
	idx := ScanIndexPath()
	if !strings.HasSuffix(idx, filepath.Join("Potflix", "scanindex.json")) {
		t.Fatalf("ScanIndexPath = %q, want ...\\Potflix\\scanindex.json", idx)
	}
}

// TestConfigStartWithWindowsRoundTrip proves the new flag survives an atomic
// save -> load, and that old stores without the field load tolerant (false).
func TestConfigStartWithWindowsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Potflix", "config.json")
	want := Config{Dirs: []string{`C:\Media`}, StartWithWindows: true}
	if err := saveConfigTo(path, want); err != nil {
		t.Fatalf("saveConfigTo: %v", err)
	}
	got, err := loadConfigFrom(path)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}
	if !got.StartWithWindows {
		t.Fatalf("round-trip StartWithWindows = false, want true")
	}
	if len(got.Dirs) != 1 {
		t.Fatalf("round-trip lost dirs: %+v", got)
	}

	legacy := filepath.Join(t.TempDir(), "legacy.json")
	if err := os.WriteFile(legacy, []byte(`{"dirs":[]}`), 0o644); err != nil {
		t.Fatalf("write legacy fixture: %v", err)
	}
	old, err := loadConfigFrom(legacy)
	if err != nil {
		t.Fatalf("loadConfigFrom legacy: %v", err)
	}
	if old.StartWithWindows {
		t.Fatalf("legacy store StartWithWindows = true, want tolerant false")
	}
}

// seedSixteenHex writes one cache-shaped file under the real 16-hex rule.
func seedSixteenHex(t *testing.T, dir, key string, schema int, media string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(key))
	name := hex.EncodeToString(sum[:])[:16] + ".json"
	data, err := json.Marshal(map[string]any{"key": key, "schema": schema, "media": json.RawMessage(media)})
	if err != nil {
		t.Fatalf("marshal seed %q: %v", key, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatalf("write seed %q: %v", key, err)
	}
	return name
}

func TestClearLibraryCacheCore(t *testing.T) {
	meta := t.TempDir()
	seedSixteenHex(t, meta, "movie|alpha|2001", metadata.CacheSchema(), `{"title":"Alpha"}`)
	seedSixteenHex(t, meta, "movie|beta|2002", metadata.CacheSchema(), `{"title":"Beta"}`)
	if err := os.WriteFile(filepath.Join(meta, "notes.json"), []byte(`{"keep":true}`), 0o644); err != nil {
		t.Fatalf("write notes.json: %v", err)
	}

	idx := filepath.Join(t.TempDir(), "scanindex.json")
	recs := `{"C:\\Media": {"fingerprint": "fp", "schema": 3, "keys": ["k1"], "scanned_at": "2026-01-01T00:00:00Z"}}`
	if err := os.WriteFile(idx, []byte(recs), 0o644); err != nil {
		t.Fatalf("write index fixture: %v", err)
	}

	titles, records, err := clearLibraryCache(meta, idx)
	if err != nil {
		t.Fatalf("clearLibraryCache: %v", err)
	}
	if titles != 2 || records != 1 {
		t.Fatalf("clearLibraryCache = (%d, %d), want (2, 1)", titles, records)
	}
	if _, err := os.Stat(idx); !os.IsNotExist(err) {
		t.Fatalf("scan index file was not deleted")
	}
	if keep, err := os.ReadFile(filepath.Join(meta, "notes.json")); err != nil || string(keep) != `{"keep":true}` {
		t.Fatalf("non-cache notes.json touched: %q, %v", keep, err)
	}
	if c, err := bundle.Count(meta); err != nil || c != 0 {
		t.Fatalf("Count after clear = %d, %v; want 0", c, err)
	}

	// Missing index is not an error (fresh machine, already cleared).
	if _, _, err := clearLibraryCache(meta, idx); err != nil {
		t.Fatalf("clearLibraryCache with missing index: %v", err)
	}
}

func TestExportImportCoresRoundTrip(t *testing.T) {
	src := t.TempDir()
	seedSixteenHex(t, src, "movie|alpha|2001", metadata.CacheSchema(), `{"title":"Alpha"}`)
	seedSixteenHex(t, src, "series|beta|2020|2", metadata.CacheSchema(), `{"title":"Beta"}`)

	dest := filepath.Join(t.TempDir(), exportDefaultName(time.Now()))
	n, err := exportBundleTo(src, dest, metadata.CacheSchema(), appVersion)
	if err != nil {
		t.Fatalf("exportBundleTo: %v", err)
	}
	if n != 2 {
		t.Fatalf("exportBundleTo entries = %d, want 2", n)
	}

	dst := t.TempDir()
	st, err := importBundleFrom(dst, dest, metadata.CacheSchema())
	if err != nil {
		t.Fatalf("importBundleFrom: %v", err)
	}
	if st.Added != 2 || st.Total != 2 || st.Present != 0 {
		t.Fatalf("first import = %+v, want Added=2 Total=2 Present=0", st)
	}
	again, err := importBundleFrom(dst, dest, metadata.CacheSchema())
	if err != nil {
		t.Fatalf("second importBundleFrom: %v", err)
	}
	if again.Added != 0 || again.Present != 2 {
		t.Fatalf("second import = %+v, want Added=0 Present=2 (fill-missing-only)", again)
	}
	if c, err := bundle.Count(dst); err != nil || c != 2 {
		t.Fatalf("Count(dst) = %d, %v; want 2", c, err)
	}

	if _, err := importBundleFrom(dst, filepath.Join(t.TempDir(), "nope.json"), metadata.CacheSchema()); err == nil {
		t.Fatalf("import of a missing file succeeded, want an error")
	}
}

func TestJSONBundleFilterEncoding(t *testing.T) {
	f := jsonBundleFilter()
	if len(f) < 4 || f[len(f)-1] != 0 || f[len(f)-2] != 0 {
		t.Fatalf("filter is not double-NUL terminated (len=%d tail=%v)", len(f), f[max(0, len(f)-2):])
	}
	// Interior NULs must survive (label/glob separators).
	nuls := 0
	for _, u := range f {
		if u == 0 {
			nuls++
		}
	}
	if nuls < 4 {
		t.Fatalf("filter holds %d NULs, want >= 4 (label/glob separators + terminator)", nuls)
	}
}
