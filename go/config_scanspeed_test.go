package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestConfigScanSpeedRoundTrip proves ScanSpeed survives an atomic save ->
// load round-trip for every named profile.
func TestConfigScanSpeedRoundTrip(t *testing.T) {
	for _, profile := range []string{"gentle", "balanced", "turbo"} {
		path := filepath.Join(t.TempDir(), "Potflix", "config.json")
		want := Config{Dirs: []string{`C:\Media`}, ScanSpeed: profile}
		if err := saveConfigTo(path, want); err != nil {
			t.Fatalf("saveConfigTo(%q): %v", profile, err)
		}
		got, err := loadConfigFrom(path)
		if err != nil {
			t.Fatalf("loadConfigFrom(%q): %v", profile, err)
		}
		if got.ScanSpeed != profile {
			t.Fatalf("round-trip ScanSpeed = %q, want %q", got.ScanSpeed, profile)
		}
		if got.EffectiveScanSpeed() != profile {
			t.Fatalf("EffectiveScanSpeed = %q, want %q", got.EffectiveScanSpeed(), profile)
		}
	}
}

// TestConfigScanSpeedNormalises proves a bad or empty stored value
// normalises to turbo on load, and the normaliser maps case-insensitively.
func TestConfigScanSpeedNormalises(t *testing.T) {
	for _, bad := range []string{"", "   ", "ludicrous", "TURBO-fast"} {
		path := filepath.Join(t.TempDir(), "config.json")
		raw := `{"dirs":[],"scan_speed":` + quoteJSON(bad) + `}`
		if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		got, err := loadConfigFrom(path)
		if err != nil {
			t.Fatalf("loadConfigFrom(%q): %v", bad, err)
		}
		if got.ScanSpeed != "turbo" {
			t.Fatalf("stored %q loads as %q, want turbo", bad, got.ScanSpeed)
		}
	}
	if got := NormalizeScanSpeed("Turbo"); got != "turbo" {
		t.Fatalf("NormalizeScanSpeed(Turbo) = %q, want turbo", got)
	}
	if got := NormalizeScanSpeed("GENTLE"); got != "gentle" {
		t.Fatalf("NormalizeScanSpeed(GENTLE) = %q, want gentle", got)
	}
	// Old stores without the field load tolerant (turbo).
	path := filepath.Join(t.TempDir(), "legacy.json")
	if err := os.WriteFile(path, []byte(`{"dirs":[]}`), 0o644); err != nil {
		t.Fatalf("write legacy fixture: %v", err)
	}
	old, err := loadConfigFrom(path)
	if err != nil {
		t.Fatalf("loadConfigFrom legacy: %v", err)
	}
	if old.EffectiveScanSpeed() != "turbo" {
		t.Fatalf("legacy EffectiveScanSpeed = %q, want turbo", old.EffectiveScanSpeed())
	}
	// A zero Config (missing/corrupt store) is turbo too.
	if (Config{}).EffectiveScanSpeed() != "turbo" {
		t.Fatalf("zero Config EffectiveScanSpeed = %q, want turbo", (Config{}).EffectiveScanSpeed())
	}

	// SetScanSpeed normalises and reports change.
	var c Config
	if !c.SetScanSpeed("TURBO") || c.ScanSpeed != "turbo" {
		t.Fatalf("SetScanSpeed(TURBO) = %q, want turbo with change", c.ScanSpeed)
	}
	if c.SetScanSpeed("turbo") {
		t.Fatalf("SetScanSpeed same value reported change, want false")
	}
	// Bogus normalises to the turbo default; move off turbo first so the
	// change is observable.
	if !c.SetScanSpeed("gentle") || c.ScanSpeed != "gentle" {
		t.Fatalf("SetScanSpeed(gentle) = %q, want gentle with change", c.ScanSpeed)
	}
	if !c.SetScanSpeed("bogus") || c.ScanSpeed != "turbo" {
		t.Fatalf("SetScanSpeed(bogus) = %q, want turbo", c.ScanSpeed)
	}
}

func quoteJSON(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\\' {
			out = append(out, '\\')
		}
		out = append(out, s[i])
	}
	return string(append(out, '"'))
}
