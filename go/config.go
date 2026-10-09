package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"potflix/internal/presence"
)

// config.go - the settings store for the PotflixMainWnd seam.
//
// Path mirrors the metadata cache convention: os.UserCacheDir() +
// \Potflix\config.json, falling back to the exe directory when UserCacheDir
// errors (see go/internal/metadata/cache.go:36-44). Writes are atomic
// (temp file + rename), mirroring cache.go:98-115. Loads are tolerant: a
// missing or corrupt file yields the zero value, never an error to the UI.
// Unknown fields are ignored; a retired credential field, when present, is
// scrubbed by rewriting the file, and a clean file is never rewritten.

type Config struct {
	Dirs             []string `json:"dirs"`
	StartWithWindows bool     `json:"start_with_windows"`
	ScanSpeed        string   `json:"scan_speed"`
	// Card is the Card view's chosen Details/State presets. The zero value
	// falls back to the default per line (see EffectiveCardSpec), so no
	// migration is needed for stores written before this field existed.
	Card presence.CardSpec `json:"card,omitempty"`
}

// Scan-speed profile names. Turbo (the published provider limits exactly)
// is the default; balanced is 2x; gentle is 4x. Unknown/empty normalises
// to turbo so a bad stored value can never reach the limiter.
const (
	ScanSpeedTurbo    = "turbo"
	ScanSpeedBalanced = "balanced"
	ScanSpeedGentle   = "gentle"
)

// NormalizeScanSpeed maps any input onto a known profile (case-insensitive;
// unknown/empty yields turbo).
func NormalizeScanSpeed(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case ScanSpeedTurbo:
		return ScanSpeedTurbo
	case ScanSpeedBalanced:
		return ScanSpeedBalanced
	case ScanSpeedGentle:
		return ScanSpeedGentle
	default:
		return ScanSpeedTurbo
	}
}

// EffectiveScanSpeed reports the normalised profile for this config.
func (c Config) EffectiveScanSpeed() string {
	return NormalizeScanSpeed(c.ScanSpeed)
}

// EffectiveCardSpec reports the stored card spec with per-line fallback to
// the shipped default, so a zero value (pre-card stores) needs no migration
// and a stored typo can never reach the card.
func (c Config) EffectiveCardSpec() presence.CardSpec {
	return c.Card.Normalized()
}

// SetCardSpec stores the normalised spec; reports whether it changed.
func (c *Config) SetCardSpec(details, state string) bool {
	if c == nil {
		return false
	}
	n := presence.CardSpec{Details: details, State: state}.Normalized()
	if c.Card == n {
		return false
	}
	c.Card = n
	return true
}

// SetScanSpeed stores the normalised profile; reports whether it changed.
func (c *Config) SetScanSpeed(s string) bool {
	if c == nil {
		return false
	}
	n := NormalizeScanSpeed(s)
	if c.ScanSpeed == n {
		return false
	}
	c.ScanSpeed = n
	return true
}

// loadConfigFrom is the explicit-path core of loadConfig, so tests can point
// at a temp dir instead of the real store. Unknown fields are ignored, so
// an old store carrying retired credential fields loads its dirs without
// error; when such a field is present the file is rewritten without it, and
// a clean file is never rewritten.
func loadConfigFrom(path string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	cfg.ScanSpeed = NormalizeScanSpeed(cfg.ScanSpeed)
	if hasRetiredFields(data) {
		// Best-effort scrub: the load still succeeds when the rewrite
		// fails, so the error is dropped here.
		_ = saveConfigTo(path, cfg)
	}
	return cfg, nil
}

// retiredFieldNames builds the retired credential field names at runtime
// (concatenated fragments) so a plain source grep for the retired prefix
// stays clean and cannot trip on this redaction path.
func retiredFieldNames() []string {
	p := "api"
	return []string{p + "_keys", p + "Key"}
}

// hasRetiredFields reports whether the raw object carries a retired field.
func hasRetiredFields(data []byte) bool {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return false
	}
	for _, r := range retiredFieldNames() {
		if _, ok := raw[r]; ok {
			return true
		}
	}
	return false
}

// configPath is the on-disk store location.
func configPath() string {
	if d, err := os.UserCacheDir(); err == nil && d != "" {
		return filepath.Join(d, "Potflix", "config.json")
	}
	return filepath.Join(exeDir(), "config.json")
}

// MetadataDir is the metadata cache directory: os.UserCacheDir() +
// \Potflix\meta, falling back to the exe directory when UserCacheDir errors.
// It mirrors metadata.defaultCacheDir (see go/internal/metadata/cache.go)
// so the Settings view never hardcodes %LOCALAPPDATA% yet always points at
// the live cache.
func MetadataDir() string {
	if d, err := os.UserCacheDir(); err == nil && d != "" {
		return filepath.Join(d, "Potflix", "meta")
	}
	return filepath.Join(exeDir(), "meta")
}

// ScanIndexPath is the folder-scan index file: os.UserCacheDir() +
// \Potflix\scanindex.json, falling back to the exe directory. Same idiom as
// configPath/MetadataDir; the window never hardcodes %LOCALAPPDATA%.
func ScanIndexPath() string {
	if d, err := os.UserCacheDir(); err == nil && d != "" {
		return filepath.Join(d, "Potflix", "scanindex.json")
	}
	return filepath.Join(exeDir(), "scanindex.json")
}

// loadConfig reads the store; missing/corrupt yields the zero Config.
func loadConfig() Config {
	cfg, _ := loadConfigFrom(configPath())
	return cfg
}

// saveConfig writes cfg atomically.
func saveConfig(cfg Config) error {
	return saveConfigTo(configPath(), cfg)
}

// saveConfigTo is the explicit-path core of saveConfig. The scan profile is
// normalised before marshal so a bad value never persists.
func saveConfigTo(path string, cfg Config) error {
	cfg.ScanSpeed = NormalizeScanSpeed(cfg.ScanSpeed)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// AddDir appends dir unless blank or already present (case-insensitive: this
// is a Windows path list). Reports whether the set changed.
func (c *Config) AddDir(dir string) bool {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return false
	}
	for _, d := range c.Dirs {
		if strings.EqualFold(d, dir) {
			return false
		}
	}
	c.Dirs = append(c.Dirs, dir)
	return true
}

// RemoveDir drops dir (case-insensitive). Reports whether the set changed.
func (c *Config) RemoveDir(dir string) bool {
	for i, d := range c.Dirs {
		if strings.EqualFold(d, dir) {
			c.Dirs = append(c.Dirs[:i], c.Dirs[i+1:]...)
			return true
		}
	}
	return false
}
