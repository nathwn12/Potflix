package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// config.go - the settings store for the PotflixMainWnd seam.
//
// Path mirrors the metadata cache convention: os.UserCacheDir() +
// \Potflix\config.json, falling back to the exe directory when UserCacheDir
// errors (see go/internal/metadata/cache.go:36-44). Writes are atomic
// (temp file + rename), mirroring cache.go:98-115. Loads are tolerant: a
// missing or corrupt file yields the zero value, never an error to the UI.
//
// API keys are optional: the app is keyless-by-default and every metadata
// source works without one. Stored keys live in APIKeys by provider name
// (today only "tmdb"); a legacy top-level "apiKey" value is migrated into
// api_keys["tmdb"] on load and never lost.
//
// Secrecy: a key VALUE is NEVER logged anywhere, never appears in an error
// string, and Config.String redacts to presence only.

type Config struct {
	Dirs             []string          `json:"dirs"`
	APIKeys          map[string]string `json:"api_keys"`
	StartWithWindows bool              `json:"start_with_windows"`
	ScanSpeed        string            `json:"scan_speed"`
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

// rawConfig is the load-time shape: the live fields plus the legacy
// top-level "apiKey", so an old store migrates instead of losing its key.
type rawConfig struct {
	Dirs             []string          `json:"dirs"`
	APIKeys          map[string]string `json:"api_keys"`
	APIKey           string            `json:"apiKey"`
	StartWithWindows bool              `json:"start_with_windows"`
	ScanSpeed        string            `json:"scan_speed"`
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

// loadConfigFrom is the explicit-path core of loadConfig, so tests can point
// at a temp dir instead of the real store. A legacy "apiKey" migrates into
// api_keys["tmdb"] when tmdb has no value yet; an explicitly stored tmdb
// entry always wins and the legacy value is otherwise dropped (never lost
// while tmdb is empty).
func loadConfigFrom(path string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var raw rawConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return Config{}, err
	}
	cfg.Dirs = raw.Dirs
	cfg.APIKeys = raw.APIKeys
	cfg.StartWithWindows = raw.StartWithWindows
	cfg.ScanSpeed = NormalizeScanSpeed(raw.ScanSpeed)
	if raw.APIKey != "" && cfg.APIKeys["tmdb"] == "" {
		if cfg.APIKeys == nil {
			cfg.APIKeys = map[string]string{}
		}
		cfg.APIKeys["tmdb"] = raw.APIKey
	}
	return cfg, nil
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

// GetAPIKey returns the stored key for provider ("" when unset). The caller
// must never log the result.
func (c Config) GetAPIKey(provider string) string {
	return c.APIKeys[provider]
}

// SetAPIKey stores value for provider; an empty value deletes the entry.
// It never logs, and reports whether the set changed.
func (c *Config) SetAPIKey(provider, value string) bool {
	if c == nil {
		return false
	}
	if value == "" {
		if _, ok := c.APIKeys[provider]; !ok {
			return false
		}
		delete(c.APIKeys, provider)
		return true
	}
	if c.APIKeys == nil {
		c.APIKeys = map[string]string{}
	}
	if c.APIKeys[provider] == value {
		return false
	}
	c.APIKeys[provider] = value
	return true
}

// MaskKey renders s safe for the UI: "" stays "", short values become all
// dots, and len>=8 (runes) becomes 8 dots plus the last 4 runes. The full
// raw value is never returned for a non-empty input.
func MaskKey(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) < 8 {
		return strings.Repeat("-", len(r))
	}
	return strings.Repeat("-", 8) + string(r[len(r)-4:])
}

// String redacts: provider names and presence only, never any key value
// (not even MaskKey's last-4 tail).
func (c Config) String() string {
	names := make([]string, 0, len(c.APIKeys))
	for name, v := range c.APIKeys {
		if v == "" {
			continue
		}
		names = append(names, name+":set")
	}
	return fmt.Sprintf("Config{dirs=%d keys=[%s]}", len(c.Dirs), strings.Join(names, " "))
}

// GoString redacts exactly like String (provider names and presence only,
// never a value) so %#v formatting can never leak a stored key.
func (c Config) GoString() string { return c.String() }

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
