package metadata

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cacheKeyTitle lowercases and collapses whitespace for stable cache keys.
// (Named apart from match.go's normalizeTitle, which additionally folds
// punctuation/diacritics for cross-title similarity.)
func cacheKeyTitle(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// cacheKey is per (show, season) for series, per (movie, year) for movies.
func cacheKey(q Query) string {
	nt := cacheKeyTitle(q.Title)
	if q.Kind == KindMovie {
		return fmt.Sprintf("movie|%s|%d", nt, q.Year)
	}
	return fmt.Sprintf("series|%s|%d|%d", nt, q.Year, q.Season)
}

func cacheFileName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:16] + ".json"
}

// defaultCacheDir is os.UserCacheDir() + \Potflix\meta, falling back to
// <dir of os.Executable()>\meta when UserCacheDir errors.
func defaultCacheDir() string {
	if d, err := os.UserCacheDir(); err == nil && d != "" {
		return filepath.Join(d, "Potflix", "meta")
	}
	if exe, err := os.Executable(); err == nil && exe != "" {
		return filepath.Join(filepath.Dir(exe), "meta")
	}
	return filepath.Join(".", "meta")
}

func resolveCacheDir(dir string) string {
	if strings.TrimSpace(dir) != "" {
		return dir
	}
	return defaultCacheDir()
}

// cacheSchema versions the on-disk envelope. cacheLoad treats any entry
// whose Schema differs as a MISS so art/synopsis are refetched (and stale
// pre-art entries are naturally replaced on the next fetch).
const cacheSchema = 3

type cacheFile struct {
	Key    string `json:"key"`
	Schema int    `json:"schema"`
	Media  Media  `json:"media"`
}

// cacheLoad returns the cached Media for key. A missing, corrupt, or
// mismatched file is a miss, never an error.
func cacheLoad(dir, key string) (*Media, bool) {
	if dir == "" {
		return nil, false
	}
	data, err := os.ReadFile(filepath.Join(dir, cacheFileName(key)))
	if err != nil {
		return nil, false
	}
	var cf cacheFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return nil, false
	}
	if cf.Key != key || cf.Schema != cacheSchema || cf.Media.Source == "" {
		return nil, false
	}
	m := cf.Media
	return &m, true
}

// cacheStore writes one JSON file per key atomically (temp file + rename).
// Entries are additive; nothing here ever deletes.
func cacheStore(dir, key string, m *Media) error {
	if m == nil {
		return fmt.Errorf("metadata: cacheStore nil media")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(cacheFile{Key: key, Schema: cacheSchema, Media: *m})
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
	if err := os.Rename(tmpName, filepath.Join(dir, cacheFileName(key))); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
