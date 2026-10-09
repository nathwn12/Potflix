package main

import (
	"os"
	"path/filepath"
	"reflect"
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
