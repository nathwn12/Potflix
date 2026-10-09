package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// config_test.go - proves the settings store seam (go/config.go): an atomic
// save -> load round-trip preserves dirs + api_keys, a legacy top-level
// "apiKey" migrates into api_keys["tmdb"] without loss, Set/Get/Clear round
// trips, MaskKey never leaks the raw value, and a corrupt file degrades to
// the zero Config instead of an error surfacing in the UI. All tests run
// against a temp dir via the explicit-path cores, never the real store.

func TestConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Potflix", "config.json")
	want := Config{Dirs: []string{`C:\Media\Movies`, `D:\Shows`}, APIKeys: map[string]string{"tmdb": "secret-not-logged"}, ScanSpeed: "balanced"}
	if err := saveConfigTo(path, want); err != nil {
		t.Fatalf("saveConfigTo: %v", err)
	}
	got, err := loadConfigFrom(path)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip = %+v, want %+v", got, want)
	}
}

func TestConfigCorruptYieldsDefaults(t *testing.T) {
	dir := t.TempDir()
	missing, err := loadConfigFrom(filepath.Join(dir, "nope.json"))
	if err == nil {
		t.Fatalf("missing file: expected an error, got none")
	}
	if !reflect.DeepEqual(missing, Config{}) {
		t.Fatalf("missing file = %+v, want zero Config", missing)
	}

	bad := filepath.Join(dir, "config.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write corrupt fixture: %v", err)
	}
	got, err := loadConfigFrom(bad)
	if err == nil {
		t.Fatalf("corrupt file: expected an error, got none")
	}
	if !reflect.DeepEqual(got, Config{}) {
		t.Fatalf("corrupt file = %+v, want zero Config", got)
	}
}

func TestConfigAddRemoveDir(t *testing.T) {
	var c Config
	if !c.AddDir(`C:\Media`) {
		t.Fatalf("AddDir first insert = false, want true")
	}
	if c.AddDir(`c:\media`) {
		t.Fatalf("AddDir case-insensitive duplicate = true, want false")
	}
	if c.AddDir("   ") {
		t.Fatalf("AddDir blank = true, want false")
	}
	if len(c.Dirs) != 1 {
		t.Fatalf("Dirs = %q, want one entry", c.Dirs)
	}
	if c.RemoveDir(`D:\Elsewhere`) {
		t.Fatalf("RemoveDir absent = true, want false")
	}
	if !c.RemoveDir(`C:\MEDIA`) {
		t.Fatalf("RemoveDir case-insensitive hit = false, want true")
	}
	if len(c.Dirs) != 0 {
		t.Fatalf("Dirs = %q, want empty", c.Dirs)
	}
}

// TestConfigLegacyAPIKeyMigration proves a pre-map store keeps its key: a
// non-empty top-level "apiKey" lands in api_keys["tmdb"] on load.
func TestConfigLegacyAPIKeyMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	legacy := `{"dirs":["C:\\Media"],"apiKey":"legacy-secret-value"}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatalf("write legacy fixture: %v", err)
	}
	got, err := loadConfigFrom(path)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}
	if got.GetAPIKey("tmdb") != "legacy-secret-value" {
		t.Fatalf("migrated tmdb key = %q, want the legacy value preserved", got.GetAPIKey("tmdb"))
	}
	if len(got.Dirs) != 1 || got.Dirs[0] != `C:\Media` {
		t.Fatalf("Dirs = %q, want the legacy dirs preserved", got.Dirs)
	}
	// A re-save + reload keeps the migrated key (never lost).
	if err := saveConfigTo(path, got); err != nil {
		t.Fatalf("saveConfigTo: %v", err)
	}
	again, err := loadConfigFrom(path)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}
	if again.GetAPIKey("tmdb") != "legacy-secret-value" {
		t.Fatalf("reloaded tmdb key = %q, want it preserved", again.GetAPIKey("tmdb"))
	}
}

// TestConfigLegacyMigrationKeepsExplicitTMDB proves an explicitly stored
// api_keys["tmdb"] wins over a stale legacy "apiKey".
func TestConfigLegacyMigrationKeepsExplicitTMDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	both := `{"dirs":[],"api_keys":{"tmdb":"new-key"},"apiKey":"stale-key"}`
	if err := os.WriteFile(path, []byte(both), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	got, err := loadConfigFrom(path)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}
	if got.GetAPIKey("tmdb") != "new-key" {
		t.Fatalf("tmdb key = %q, want the explicit api_keys entry to win", got.GetAPIKey("tmdb"))
	}
}

// TestConfigSetGetClearKey proves the Set/Get round-trip and that an empty
// value deletes the entry.
func TestConfigSetGetClearKey(t *testing.T) {
	var c Config
	if got := c.GetAPIKey("tmdb"); got != "" {
		t.Fatalf("GetAPIKey empty = %q, want \"\"", got)
	}
	if !c.SetAPIKey("tmdb", "k1") {
		t.Fatalf("SetAPIKey new = false, want true")
	}
	if got := c.GetAPIKey("tmdb"); got != "k1" {
		t.Fatalf("GetAPIKey = %q, want k1", got)
	}
	if c.SetAPIKey("tmdb", "k1") {
		t.Fatalf("SetAPIKey same value = true, want false (no change)")
	}
	if !c.SetAPIKey("tmdb", "") {
		t.Fatalf("SetAPIKey empty = false, want true (clears)")
	}
	if got := c.GetAPIKey("tmdb"); got != "" {
		t.Fatalf("GetAPIKey after clear = %q, want \"\"", got)
	}
	if _, ok := c.APIKeys["tmdb"]; ok {
		t.Fatalf("entry still present after clear, want it deleted")
	}
	if c.SetAPIKey("tmdb", "") {
		t.Fatalf("SetAPIKey empty on absent = true, want false")
	}

	// Round-trip through disk.
	path := filepath.Join(t.TempDir(), "config.json")
	c.SetAPIKey("tmdb", "disk-key")
	if err := saveConfigTo(path, c); err != nil {
		t.Fatalf("saveConfigTo: %v", err)
	}
	got, err := loadConfigFrom(path)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}
	if got.GetAPIKey("tmdb") != "disk-key" {
		t.Fatalf("reloaded tmdb key = %q, want disk-key", got.GetAPIKey("tmdb"))
	}
}

// TestConfigMaskKey proves the masked form never carries the raw value:
// empty stays empty, short values are all dots, long values keep only the
// last 4 behind 8 dots.
func TestConfigMaskKey(t *testing.T) {
	if got := MaskKey(""); got != "" {
		t.Fatalf("MaskKey(\"\") = %q, want \"\"", got)
	}
	short := "abc"
	if got := MaskKey(short); got == short || strings.Contains(got, short) {
		t.Fatalf("MaskKey(%q) = %q, want all dots (no raw value)", short, got)
	} else if got != "---" {
		t.Fatalf("MaskKey(%q) = %q, want 3 dots", short, got)
	}
	long := "abcdefgh12345678"
	got := MaskKey(long)
	if got == long || strings.Contains(got, long) {
		t.Fatalf("MaskKey(%q) = %q, want masked (no raw value)", long, got)
	}
	if !strings.HasSuffix(got, "5678") || !strings.HasPrefix(got, "--------") {
		t.Fatalf("MaskKey(%q) = %q, want 8 dots + last 4", long, got)
	}
	exact8 := "12345678"
	if got := MaskKey(exact8); got != "--------5678" {
		t.Fatalf("MaskKey(%q) = %q, want 8 dots + last 4", exact8, got)
	}
}

// TestConfigStringRedacts proves %v/%s of a Config never carries a key
// value (presence only).
func TestConfigStringRedacts(t *testing.T) {
	c := Config{Dirs: []string{`C:\Media`}, APIKeys: map[string]string{"tmdb": "super-secret-value"}}
	s := c.String()
	if strings.Contains(s, "super-secret-value") {
		t.Fatalf("Config.String() leaks the key value: %q", s)
	}
	if !strings.Contains(s, "tmdb") {
		t.Fatalf("Config.String() = %q, want the provider name present", s)
	}
}

// TestConfigGoStringRedacts proves %#v formatting redacts exactly like
// String: provider names and presence only, never a key value.
func TestConfigGoStringRedacts(t *testing.T) {
	c := Config{Dirs: []string{`C:\Media`}, APIKeys: map[string]string{"tmdb": "super-secret-value"}}
	s := fmt.Sprintf("%#v", c)
	if strings.Contains(s, "super-secret-value") {
		t.Fatalf("Config.GoString() leaks the key value: %q", s)
	}
	if !strings.Contains(s, "tmdb") {
		t.Fatalf("Config.GoString() = %q, want the provider name present", s)
	}
	if got := c.GoString(); got != c.String() {
		t.Fatalf("GoString() = %q, String() = %q; want the same redacted shape", got, c.String())
	}
}
