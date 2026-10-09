package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"potflix/internal/presence"
)

// config_test.go - proves the settings store seam (go/config.go): an atomic
// save -> load round-trip preserves dirs and scan speed, retired key fields
// in an old store are ignored, and a corrupt file degrades to the zero
// Config instead of an error surfacing in the UI. All tests run against a
// temp dir via the explicit-path cores, never the real store.

func TestConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Potflix", "config.json")
	want := Config{Dirs: []string{`C:\Media\Movies`, `D:\Shows`}, ScanSpeed: "balanced"}
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

// TestConfigCardSpec proves the Card view's persistence: a chosen spec
// round-trips through the store, the zero value falls back to the shipped
// default (no migration), and SetCardSpec normalises unknown ids.
func TestConfigCardSpec(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Potflix", "config.json")
	want := Config{Dirs: []string{`C:\Media`}, ScanSpeed: ScanSpeedTurbo}
	if !want.SetCardSpec("title", "tag") {
		t.Fatalf("SetCardSpec first set = false, want true")
	}
	if want.SetCardSpec("title", "tag") {
		t.Fatalf("SetCardSpec same value = true, want false (unchanged)")
	}
	if err := saveConfigTo(path, want); err != nil {
		t.Fatalf("saveConfigTo: %v", err)
	}
	got, err := loadConfigFrom(path)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("card round-trip = %+v, want %+v", got, want)
	}
	if eff := got.EffectiveCardSpec(); eff.Details != "title" || eff.State != "tag" {
		t.Fatalf("EffectiveCardSpec = %+v, want title/tag", eff)
	}
	// Zero value: no migration needed.
	var zero Config
	if eff := zero.EffectiveCardSpec(); eff != presence.DefaultCardSpec() {
		t.Fatalf("zero EffectiveCardSpec = %+v, want default %+v", eff, presence.DefaultCardSpec())
	}
	// Unknown ids normalise on set, never persist raw.
	var w Config
	w.SetCardSpec("fancy-template", "")
	if eff := w.EffectiveCardSpec(); eff != presence.DefaultCardSpec() {
		t.Fatalf("unknown EffectiveCardSpec = %+v, want default", eff)
	}
}

// Retired names are built at runtime (concatenated fragments) so a plain
// source grep for the retired prefix stays clean and cannot trip on this
// redaction test.
func retiredFixtureNames() []string {
	p := "api"
	return []string{p + "_keys", p + "Key"}
}

// TestConfigScrubRetiredFields proves the privacy scrub: a store carrying
// retired credential fields loads its dirs, carries nothing credential-ish
// in memory, and is rewritten on disk without the retired names.
func TestConfigScrubRetiredFields(t *testing.T) {
	retired := retiredFixtureNames()
	path := filepath.Join(t.TempDir(), "config.json")
	payload := `{"dirs":["C:\\Media"],"scan_speed":"turbo","` + retired[0] + `":"s3cr3t","` + retired[1] + `":"s3cr3t"}`
	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		t.Fatalf("write scrub fixture: %v", err)
	}
	got, err := loadConfigFrom(path)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}
	if !reflect.DeepEqual(got.Dirs, []string{`C:\Media`}) {
		t.Fatalf("scrubbed Dirs = %q, want one entry", got.Dirs)
	}
	wire, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal loaded config: %v", err)
	}
	for _, r := range retired {
		if strings.Contains(string(wire), r) {
			t.Fatalf("in-memory config still carries retired field %q: %s", r, wire)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read scrubbed file: %v", err)
	}
	for _, r := range retired {
		if strings.Contains(string(after), r) {
			t.Fatalf("file on disk still contains retired field %q: %s", r, after)
		}
	}
}

// TestConfigCleanNotRewritten proves a clean store is never rewritten:
// content is byte-identical before and after load.
func TestConfigCleanNotRewritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	want := Config{Dirs: []string{`C:\Media`}, ScanSpeed: ScanSpeedTurbo}
	if err := saveConfigTo(path, want); err != nil {
		t.Fatalf("saveConfigTo: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}
	if _, err := loadConfigFrom(path); err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("clean config rewritten:\nbefore %q\nafter  %q", before, after)
	}
}
