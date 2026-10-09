package metadata

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
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

// CacheSchema reports the current on-disk cache schema. The library scan
// index records it per folder so a schema bump invalidates old skip
// records (they rescan instead of skipping).
func CacheSchema() int { return cacheSchema }

// CacheKeyFor returns the on-disk cache identity for one query: per
// (show, season) for series, per (movie, year) for movies. The library
// scan index stores these keys (not the finer dedupe keys) so the skip
// guard can verify each one against the cache.
func CacheKeyFor(q Query) string { return cacheKey(q) }

// CacheFileNameFor returns the cache file base name for a cache key
// (sha256 hex, first 16 chars, + ".json"). Exported so tests can delete
// one key's file to prove the skip guard.
func CacheFileNameFor(key string) string { return cacheFileName(key) }

// CacheHas reports whether the cache entry for key exists under dir (""
// resolves to the default cache dir, mirroring New). It is a stat-only
// existence check: it never reads or parses the file body, so even a
// corrupt entry reports present. If a caller needs the schema, that is the
// index's own Schema field (see library.ScanIndex), not this probe.
func CacheHas(dir, key string) bool {
	if key == "" {
		return false
	}
	d := resolveCacheDir(dir)
	if d == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(d, cacheFileName(key)))
	return err == nil
}

// CacheLive reports whether key has a VALID LIVE answer on disk under dir
// ("" resolves to the default cache dir, mirroring New and CacheHas).
// Unlike the stat-only CacheHas above, CacheLive reads and validates the
// entry body with a bounded read (at most cacheLiveMaxBytes):
//
//   - a positive entry (any age) -> true;
//   - a negative entry -> true only while its TTL has not expired, using
//     the stored CheckedAt; an expired, zero, or far-future CheckedAt
//     (beyond cacheClockSkew, e.g. after a clock jump) -> false;
//   - corrupt / unreadable / wrong-schema / wrong-key / truncated /
//     oversize files -> false.
//
// The sibling library skip guard uses CacheLive so expired misses and
// corrupt files are rescanned instead of skipped.
func CacheLive(dir, key string) bool {
	if key == "" {
		return false
	}
	d := resolveCacheDir(dir)
	if d == "" {
		return false
	}
	p := filepath.Join(d, cacheFileName(key))
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > cacheLiveMaxBytes {
		return false
	}
	data, err := io.ReadAll(io.LimitReader(f, cacheLiveMaxBytes+1))
	if err != nil {
		return false
	}
	if len(data) > cacheLiveMaxBytes {
		return false
	}
	var cf cacheFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return false
	}
	if cf.Key != key || cf.Schema != cacheSchema {
		return false
	}
	if cf.NotFound {
		return negativeFresh(cf.CheckedAt, time.Now())
	}
	return cf.Media.Source != ""
}

// CacheLive probes the resolver's own cache directory for a valid live
// answer to key. It shares the package-level CacheLive semantics; the
// single-key shape exists so callers holding a Resolver need not thread
// its directory through.
func (r *Resolver) CacheLive(key string) bool {
	if r == nil {
		return false
	}
	return CacheLive(r.dir, key)
}

type cacheFile struct {
	Key       string    `json:"key"`
	Schema    int       `json:"schema"`
	Media     Media     `json:"media"`
	NotFound  bool      `json:"notFound,omitempty"`
	CheckedAt time.Time `json:"checkedAt,omitempty"`
}

// negativeTTL bounds how long a persisted not-found entry counts as a
// cached miss. Long enough that rescans stay fast (no network for known
// misses), short enough that a provider improvement or a metadata fix is
// picked up within a month. force/Refresh always re-attempts regardless
// of age.
const negativeTTL = 30 * 24 * time.Hour

// cacheClockSkew tolerates a small future drift in a stored CheckedAt
// (NTP step, dual-boot clock jump). A CheckedAt more than this far in the
// future is treated as expired, never as fresh.
const cacheClockSkew = 5 * time.Minute

// cacheLiveMaxBytes bounds the validity-aware CacheLive probe: it never
// slurps an arbitrarily large file.
const cacheLiveMaxBytes = 64 * 1024

// negativeFresh reports whether a stored CheckedAt still counts as a live
// miss at now. Zero, TTL-expired, and far-future (beyond cacheClockSkew)
// timestamps are all expired. A near-future timestamp within the skew
// window is treated as fresh (clock wobble, not a jump).
func negativeFresh(checkedAt, now time.Time) bool {
	if checkedAt.IsZero() {
		return false
	}
	if checkedAt.After(now.Add(cacheClockSkew)) {
		return false
	}
	if checkedAt.After(now) {
		return true
	}
	return now.Sub(checkedAt) <= negativeTTL
}

// negativeMemExpiry bounds the in-memory suppression so it never outlives
// the stored TTL expiry: min(now+negativeBackoff, checkedAt+negativeTTL).
// Callers must only use it for a fresh (negativeFresh-true) CheckedAt.
func negativeMemExpiry(checkedAt, now time.Time) time.Time {
	ttlExp := checkedAt.Add(negativeTTL)
	backoffExp := now.Add(negativeBackoff)
	if ttlExp.Before(backoffExp) {
		return ttlExp
	}
	return backoffExp
}

// cacheEntry is the three-state result of a cache probe: a positive hit
// carries Media, a negative hit carries NotFound with its CheckedAt, and
// absent/corrupt/expired is reported via ok=false.
type cacheEntry struct {
	Media     *Media
	NotFound  bool
	CheckedAt time.Time
}

// cacheLoadEntry distinguishes positive hits, fresh negative hits, and
// absent/corrupt/expired entries. An expired negative (older than
// negativeTTL) is treated as absent so the caller re-attempts. A far-future
// CheckedAt (beyond cacheClockSkew) is likewise treated as absent.
func cacheLoadEntry(dir, key string) (cacheEntry, bool) {
	if dir == "" {
		return cacheEntry{}, false
	}
	data, err := os.ReadFile(filepath.Join(dir, cacheFileName(key)))
	if err != nil {
		return cacheEntry{}, false
	}
	var cf cacheFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return cacheEntry{}, false
	}
	if cf.Key != key || cf.Schema != cacheSchema {
		return cacheEntry{}, false
	}
	if cf.NotFound {
		if !negativeFresh(cf.CheckedAt, time.Now()) {
			return cacheEntry{}, false
		}
		return cacheEntry{NotFound: true, CheckedAt: cf.CheckedAt}, true
	}
	if cf.Media.Source == "" {
		return cacheEntry{}, false
	}
	m := cf.Media
	return cacheEntry{Media: &m}, true
}

// cacheLoad returns the cached Media for key. A missing, corrupt, or
// mismatched file is a miss, never an error.
func cacheLoad(dir, key string) (*Media, bool) {
	e, ok := cacheLoadEntry(dir, key)
	if !ok || e.NotFound || e.Media == nil {
		return nil, false
	}
	return e.Media, true
}

// cacheStoreNegative persists a known-miss under the SAME key filename so
// the stat-based CacheHas guard keeps working with no change. It holds no
// Media and passes the same schema gate; a fresh negativeTTL window starts
// at write time. Written atomically (temp file + rename), like cacheStore.
// The CheckedAt timestamp is clamped to now (never written in the future)
// so a clock jump cannot pin a far-future miss.
func cacheStoreNegative(dir, key string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	now := time.Now().UTC()
	data, err := json.Marshal(cacheFile{Key: key, Schema: cacheSchema, NotFound: true, CheckedAt: now})
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
