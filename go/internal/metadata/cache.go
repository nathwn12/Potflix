package metadata

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"potflix/internal/store"
)

// The cache lives in ONE store file per cache directory - "<dir>.idx", i.e.
// %LOCALAPPDATA%\Potflix\meta.idx for the default dir - instead of one JSON
// file per key (1,118 files / 3,010,068 bytes on the owner's machine). The
// stored bytes keep the cacheFile record schema, so every reader, the schema
// gate and the negative TTL are unchanged; only the container moved.
//
// The old per-key directory is read exactly once per directory, by
// migrateLegacyCache, which imports it into the store and leaves it in place
// as the recovery copy; ClearCache is the only code that removes it.
//
// One store handle per directory is opened lazily and held for the process
// lifetime (see cacheHandle), so a read costs a map lookup rather than a
// replay of the file; ClearCache is the only way to invalidate an entry, since
// there is no per-key file left to delete.

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

// cacheFileName is the LEGACY one-file-per-key name: sha256(key)[:16] hex +
// ".json". The store keys entries by the raw cache key instead, but this name
// stays the stable identity of a key for the migration and for the exported
// CacheFileNameFor (Settings bundle export/import).
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

// cacheSchema versions the on-disk envelope. cacheLoadEntry treats any record
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

// CacheFileNameFor returns the LEGACY cache file base name for a cache key
// (sha256 hex, first 16 chars, + ".json"). The live cache stores keys in
// <dir>.idx instead, so this name is now the migration/export identity
// rather than a file to delete; it stays exported because the Settings
// bundle and its tests address entries by it.
func CacheFileNameFor(key string) string { return cacheFileName(key) }

// --- store plumbing -------------------------------------------------------

// cacheIndexSuffix names the one-store-file cache: the cache directory plus
// ".idx", so the default dir %LOCALAPPDATA%\Potflix\meta is backed by
// %LOCALAPPDATA%\Potflix\meta.idx.
const cacheIndexSuffix = ".idx"

// cacheMaxValueSize bounds one stored record. The largest entry measured on
// the owner's machine is 231,757 bytes, so 1 MiB stores every real entry with
// headroom while still refusing a pathological value. An over-size record
// fails its Put (the resolver logs it) and is simply re-fetched next time.
const cacheMaxValueSize = 1 << 20

// cacheIndexPath is the store file backing the cache directory dir.
func cacheIndexPath(dir string) string { return dir + cacheIndexSuffix }

// cacheHandle is ONE store handle for a cache directory: opened lazily on the
// first operation and held for the process lifetime, so the store file is
// replayed once instead of once per operation.
//
// The per-operation open/use/close it replaces replayed the whole file and
// fsynced on every call: measured at 1.80 ms per operation on the owner's
// machine (1,118 records / 3,056,161 bytes), which is nothing beside the
// network fetch one miss triggers - but a scan probes 2,309 distinct keys, so
// the walk paid ~4 s for a cache it only read. With the handle held, a read is
// a map lookup: the same walk totals single-digit milliseconds.
//
// A held handle is also what keeps the file's single-writer invariant: opening
// a second handle before another handle's append would replay a stale tail and
// overwrite it, so exactly one handle per directory exists in a process and
// every operation on it runs under mu. mu covers a whole operation, not one
// store call, because a caller like ClearCache spans several.
type cacheHandle struct {
	mu sync.Mutex // held for the duration of one operation
	db *store.DB
}

// cacheHandles is the process-wide registry: one held handle per cache
// directory. It is never closed in production - a handle lives as long as the
// process - and closeCacheHandles exists only for tests that must remove a
// temporary directory.
var cacheHandles = struct {
	mu sync.Mutex
	m  map[string]*cacheHandle
}{m: make(map[string]*cacheHandle)}

// cacheHandleFor returns dir's held handle, opening the store on first use
// (importing the legacy directory while the store is still empty). Opening
// runs under the registry lock: a second concurrent open of the same file
// would hand out two handles to one file, and the loser's append would replay
// a stale tail.
func cacheHandleFor(dir string) (*cacheHandle, error) {
	cacheHandles.mu.Lock()
	defer cacheHandles.mu.Unlock()
	if h, ok := cacheHandles.m[dir]; ok {
		return h, nil
	}
	if err := os.MkdirAll(filepath.Dir(cacheIndexPath(dir)), 0o755); err != nil {
		return nil, err
	}
	db, err := store.Open(cacheIndexPath(dir), store.Options{MaxValueSize: cacheMaxValueSize})
	if err != nil {
		return nil, err
	}
	h := &cacheHandle{db: db}
	cacheHandles.m[dir] = h
	return h, nil
}

// cacheHandleHeld reports whether dir's store is already open in this process.
func cacheHandleHeld(dir string) bool {
	cacheHandles.mu.Lock()
	defer cacheHandles.mu.Unlock()
	_, ok := cacheHandles.m[dir]
	return ok
}

// withCache runs fn against dir's held store: it creates the cache directory
// if it is missing and opens the store on first use, then runs fn under the
// directory's lock. It is the READ shape; a mutation uses withCacheWrite.
func withCache(dir string, fn func(*store.DB) error) error {
	h, err := cacheHandleFor(dir)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// The legacy directory stays watched while the store is empty, exactly as
	// it was when every operation re-opened the store: a caller that seeds
	// legacy files after the store was first touched (the Settings bundle, a
	// test) still sees them imported. On a populated store this is a Len()
	// check, not a directory read.
	if migrateLegacy(h.db, dir) > 0 {
		if err := h.db.Commit(); err != nil {
			return err
		}
	}
	return fn(h.db)
}

// withCacheWrite is withCache for a mutation. It commits (flush + fsync) before
// returning, so a stored record is durable exactly as it was when every call
// ended in a close that flushed and fsynced. Every mutation path in this
// package goes through here, so no append sits unflushed between calls.
func withCacheWrite(dir string, fn func(*store.DB) error) error {
	return withCache(dir, func(db *store.DB) error {
		err := fn(db)
		if cerr := db.Commit(); err == nil {
			err = cerr
		}
		return err
	})
}

// closeCacheHandles closes and forgets every held handle, so a temporary cache
// directory can be removed. It is the test-side half of the held handle and is
// never called by production code, where a handle is meant to live for the
// process: tests register it with t.Cleanup after the t.TempDir call that owns
// the directory (a cleanup registered later runs earlier, so the close lands
// before the temp dir is removed - the store file <dir>.idx sits inside the
// per-test temp root and an open file there is at best fragile to remove). A
// handle reopens lazily, so the directory stays usable afterwards.
func closeCacheHandles() {
	cacheHandles.mu.Lock()
	handles := make([]*cacheHandle, 0, len(cacheHandles.m))
	for dir, h := range cacheHandles.m {
		handles = append(handles, h)
		delete(cacheHandles.m, dir)
	}
	cacheHandles.mu.Unlock()
	for _, h := range handles {
		h.mu.Lock()
		_ = h.db.Close()
		h.mu.Unlock()
	}
}

// CacheForEach calls fn once per entry in the live cache under dir ("" resolves
// to the default cache dir, mirroring CacheHas, CacheLive and ClearCache), in
// unspecified order. fn returns false to stop early. value is a READ-ONLY
// snapshot of the stored record - it aliases the store's in-memory copy, so a
// caller that keeps it must copy it and must never write into it; it is the
// record's stored bytes, byte for byte, exactly as a Put received them.
//
// The live cache is one store file per directory, so this is the only way to
// read it as a whole: the Settings bundle export, its count and the library
// index's pre-count are built on it. A directory with no cache on disk calls fn
// zero times and is not an error, and a read never creates a cache that was not
// there (a legacy per-key directory that migration has not imported yet counts
// as one, and is imported by this call exactly as any other first touch does).
// fn runs with the directory's store locked (see cacheHandle), so it must not
// call back into the cache for the same directory.
func CacheForEach(dir string, fn func(key string, value []byte) bool) error {
	if fn == nil {
		return nil
	}
	d := resolveCacheDir(dir)
	if d == "" || !cacheExists(d) {
		return nil
	}
	return withCache(d, func(db *store.DB) error {
		return db.ForEach(fn)
	})
}

// CachePutRaw stores one already-encoded record under key, creating the cache if
// this is its first write, and commits (flush + fsync) before returning, so the
// record is durable and visible to CacheHas, CacheLive and ProbeCache the
// moment this returns. It is the bulk-write seam for a caller that holds a
// record's bytes (the Settings bundle import); the bytes are stored exactly as
// given - no key, schema or size-validity check beyond the store's own
// cacheMaxValueSize cap, and no re-encoding - so the CALLER owns that contract.
// An over-size value is refused by the store and stores nothing. Unlike the
// resolver's own writes it does not read, replace or invalidate anything else,
// and it never touches the legacy per-key directory (see migrateLegacy).
func CachePutRaw(dir, key string, value []byte) error {
	if dir == "" {
		return fmt.Errorf("metadata: cache put raw without a cache directory")
	}
	if key == "" {
		return fmt.Errorf("metadata: cache put raw with an empty key")
	}
	return withCacheWrite(dir, func(db *store.DB) error {
		if err := db.Put(key, value); err != nil {
			return fmt.Errorf("metadata: cache put raw %q: %w", key, err)
		}
		return nil
	})
}

// migrateLegacy imports the legacy per-key files while the store is empty, and
// reports how many records it added. Only an empty store imports: a populated
// cache is never re-imported, and an emptied one cannot resurrect its old
// entries because ClearCache removes the legacy directory too.
func migrateLegacy(db *store.DB, dir string) int {
	if db.Len() > 0 {
		return 0
	}
	return migrateLegacyCache(db, legacyCacheFiles(dir))
}

// cacheExists reports whether dir has a cache on disk: an open handle (whose
// store file is on disk and loaded), or the store index, or legacy per-key
// .json files that migration has not imported yet. Read paths use it so a probe
// never creates a cache that was never there. The held handle short-circuits
// the two disk probes, which is what makes a scan's per-key probes free once
// the first one has opened the store.
func cacheExists(dir string) bool {
	if cacheHandleHeld(dir) {
		return true
	}
	if fi, err := os.Stat(cacheIndexPath(dir)); err == nil && !fi.IsDir() {
		return true
	}
	return len(legacyCacheFiles(dir)) > 0
}

// cacheRecord returns the stored record for key. A cache that is not on disk
// is a miss and is never created by a read. The returned bytes are an immutable
// snapshot: the store copies every value on Put and never writes into a stored
// one, so decoding them is safe whatever a later write does to the entry.
func cacheRecord(dir, key string) ([]byte, bool) {
	if dir == "" || key == "" || !cacheExists(dir) {
		return nil, false
	}
	var data []byte
	err := withCache(dir, func(db *store.DB) error {
		if v, ok := db.Get(key); ok {
			data = v
		}
		return nil
	})
	if err != nil || data == nil {
		return nil, false
	}
	return data, true
}

// legacyCacheFiles lists the legacy per-key cache files in dir, ignoring the
// ".tmp-" files the old atomic writer could leave behind. A missing dir lists
// none, never an error.
func legacyCacheFiles(dir string) []string {
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil
	}
	out := matches[:0]
	for _, p := range matches {
		if strings.HasPrefix(filepath.Base(p), ".tmp-") {
			continue
		}
		out = append(out, p)
	}
	return out
}

// migrateLegacyCache imports every legacy per-key record into the store and
// returns how many records it added. It is additive and never destructive: the
// files stay exactly where they are, as the recovery copy, so a partial or
// failed import costs nothing (best-effort - an unreadable file is skipped,
// never fatal). Only a body that decodes as a cache record with a non-empty
// key is imported, so junk, truncation and foreign .json files import nothing;
// a wrong-schema record IS imported and stays a MISS for the reader's schema
// gate, exactly as it was on disk. The record's own key is authoritative and
// its stored bytes are the file's bytes, byte for byte.
func migrateLegacyCache(db *store.DB, files []string) int {
	imported := 0
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var cf cacheFile
		if err := json.Unmarshal(data, &cf); err != nil || cf.Key == "" {
			continue
		}
		if err := db.Put(cf.Key, data); err != nil {
			continue
		}
		imported++
	}
	return imported
}

// ClearCache drops every cached entry under dir ("" resolves to the default
// cache dir, mirroring New) and removes the legacy one-file-per-key directory
// if it is still there. It is THE way to invalidate a cache entry: the cache is
// one store file keyed by the raw key, so there is no per-key file left to
// delete, and a Resolver re-validates a probe-warmed entry against the store
// (see probedLive), which this empties. It is idempotent: an empty or absent
// cache reports no error and a second call is a no-op. The store file itself is
// reused - its entries are deleted and the file compacted back to an empty
// snapshot - so the next lookup writes a fresh record for the same key. The
// legacy directory must go: it is the recovery copy migration imports from, so
// leaving it would resurrect exactly what the owner cleared. An entry a
// Resolver loaded into memory through a lookup is not re-validated and keeps
// serving for the rest of that process; an entry warmed by ProbeCache is
// re-validated against the emptied store and stops being served.
func ClearCache(dir string) error {
	d := resolveCacheDir(dir)
	if d == "" {
		return nil
	}
	if _, err := ClearCacheEntries(d); err != nil {
		return err
	}
	return os.RemoveAll(d)
}

// ClearCacheEntries drops every entry in the live cache under dir ("" resolves
// to the default cache dir, mirroring ClearCache) and reports how many entries
// it cleared. It is ClearCache's entry half: the store file survives as an
// empty snapshot, the legacy per-key directory is left exactly where it is, and
// every other file in the directory is untouched - ClearCache is this plus the
// directory removal, and bundle.Clear (which must not delete files that are not
// cache entries) is this plus the legacy cache files it has always removed.
//
// It enumerates and deletes, so its count is the number of TITLES the cache
// held, not the number of files on disk. It is idempotent - an empty or absent
// cache reports 0 and no error - and it never creates a cache that was not
// there. An entry a Resolver loaded into memory through a lookup is not
// re-validated and keeps serving for the rest of that process; an entry warmed
// by ProbeCache is re-validated against the emptied store and stops being
// served.
func ClearCacheEntries(dir string) (int, error) {
	d := resolveCacheDir(dir)
	if d == "" || !cacheExists(d) {
		return 0, nil
	}
	cleared := 0
	err := withCacheWrite(d, func(db *store.DB) error {
		keys := make([]string, 0, db.Len())
		if err := db.ForEach(func(k string, _ []byte) bool {
			keys = append(keys, k)
			return true
		}); err != nil {
			return err
		}
		for _, k := range keys {
			if err := db.Delete(k); err != nil {
				return err
			}
		}
		cleared = len(keys)
		// Tombstones alone would leave the file's bytes on disk: rewrite it
		// as an empty snapshot.
		return db.Compact()
	})
	if err != nil {
		return 0, err
	}
	return cleared, nil
}

// CacheHas reports whether key has an entry in the cache under dir ("" resolves
// to the default cache dir, mirroring New). Presence in the store is the
// existence check: the body is never read or parsed, so a stored entry reports
// present whatever its schema. A legacy file that could not be imported
// (corrupt, truncated, foreign, or a temp file) reports absent - it never was
// an entry. If a caller needs the schema, that is the index's own Schema field
// (see library.ScanIndex), not this probe.
func CacheHas(dir, key string) bool {
	if key == "" {
		return false
	}
	d := resolveCacheDir(dir)
	if d == "" {
		return false
	}
	_, ok := cacheRecord(d, key)
	return ok
}

// CacheLive reports whether key has a VALID LIVE answer in the cache under dir
// ("" resolves to the default cache dir, mirroring New and CacheHas). Unlike
// the presence-only CacheHas above, CacheLive decodes and validates the stored
// record:
//
//   - a positive entry (any age) -> true;
//   - a negative entry -> true only while its TTL has not expired, using
//     the stored CheckedAt; an expired, zero, or far-future CheckedAt
//     (beyond cacheClockSkew, e.g. after a clock jump) -> false;
//   - corrupt / wrong-schema / wrong-key / truncated / unreadable records
//     -> false.
//
// Size is NOT a validity gate: the store caps a record at cacheMaxValueSize
// when it is written and returns a stored record whole, so the largest real
// entry (231,757 bytes) is live exactly like a small one. The sibling library
// skip guard uses CacheLive so expired misses and corrupt records are
// rescanned instead of skipped.
func CacheLive(dir, key string) bool {
	_, ok := cacheLoadEntry(dir, key)
	return ok
}

// CacheLive probes the resolver's own cache directory for a valid live answer
// to key. It shares the package-level CacheLive semantics; the single-key
// shape exists so callers holding a Resolver need not thread its directory
// through.
func (r *Resolver) CacheLive(key string) bool {
	if r == nil {
		return false
	}
	return CacheLive(r.dir, key)
}

// ProbeCache reports whether key has a live cache answer without ever
// fetching, warming the in-memory fast path (positives into mem, fresh
// negatives into neg) so a later lookup for the same key is served without
// another store read. The verdict matches CacheLive exactly (same loader, same
// validity gates); only the side effect differs. A scan probes each distinct
// key once and shares the result across its guard, pre-count and fetch phases.
// Only live entries warm memory: a missing/corrupt/expired record warms
// nothing, so the later lookup re-reads it from the store and the store stays
// the source of truth. Warmed entries are marked probed so an entry removed
// after the probe is never served from memory (see probedLive).
func (r *Resolver) ProbeCache(key string) bool {
	if r == nil || key == "" {
		return false
	}
	e, ok := cacheLoadEntry(r.dir, key)
	if !ok {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e.NotFound {
		r.neg[key] = negativeMemExpiry(e.CheckedAt, time.Now())
		r.probed[key] = true
		return true
	}
	r.mem[key] = e.Media
	r.probed[key] = true
	return true
}

// CountNegatives reports how many not-found (dead) entries the cache holds
// under dir ("" resolves to the default cache dir, mirroring New). Only
// well-formed current-schema negative records count; positive entries,
// corrupt records, and a missing cache count zero, never an error. The
// Settings view shows this so the owner knows Clear cache will retry those
// titles.
func CountNegatives(dir string) int {
	d := resolveCacheDir(dir)
	if d == "" || !cacheExists(d) {
		return 0
	}
	n := 0
	// ForEach runs with the store locked: this callback only decodes a copy.
	err := withCache(d, func(db *store.DB) error {
		return db.ForEach(func(_ string, v []byte) bool {
			var cf cacheFile
			if err := json.Unmarshal(v, &cf); err != nil {
				return true
			}
			if cf.Schema != cacheSchema || !cf.NotFound {
				return true
			}
			n++
			return true
		})
	})
	if err != nil {
		return 0
	}
	return n
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
// absent/corrupt/expired entries in dir's store ("" is always a miss). An
// expired negative (older than negativeTTL) is treated as absent so the caller
// re-attempts. A far-future CheckedAt (beyond cacheClockSkew) is likewise
// treated as absent. The record is decoded into fresh values, so a returned
// Media never aliases stored memory.
func cacheLoadEntry(dir, key string) (cacheEntry, bool) {
	data, ok := cacheRecord(dir, key)
	if !ok {
		return cacheEntry{}, false
	}
	return decodeCacheEntry(data, key)
}

// decodeCacheEntry applies the cache's validity gates to one stored record:
// the schema and key must match, a negative must still be fresh, and a
// positive must carry a Source. CacheLive and ProbeCache share it so their
// verdicts agree case by case.
func decodeCacheEntry(data []byte, key string) (cacheEntry, bool) {
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
// mismatched entry is a miss, never an error.
func cacheLoad(dir, key string) (*Media, bool) {
	e, ok := cacheLoadEntry(dir, key)
	if !ok || e.NotFound || e.Media == nil {
		return nil, false
	}
	return e.Media, true
}

// cacheStoreNegative persists a known-miss under the SAME key so the presence
// probe (CacheHas) keeps answering true for it with no change. It holds no
// Media and passes the same schema/TTL gates; a fresh negativeTTL window
// starts at write time. The CheckedAt timestamp is clamped to now (never
// written in the future) so a clock jump cannot pin a far-future miss.
func cacheStoreNegative(dir, key string) error {
	if key == "" {
		return fmt.Errorf("metadata: cacheStoreNegative empty key")
	}
	now := time.Now().UTC()
	data, err := json.Marshal(cacheFile{Key: key, Schema: cacheSchema, NotFound: true, CheckedAt: now})
	if err != nil {
		return err
	}
	return cachePut(dir, key, data)
}

// cacheStore writes one record for key into the store. Entries are additive;
// nothing here ever deletes.
func cacheStore(dir, key string, m *Media) error {
	if m == nil {
		return fmt.Errorf("metadata: cacheStore nil media")
	}
	if key == "" {
		return fmt.Errorf("metadata: cacheStore empty key")
	}
	data, err := json.Marshal(cacheFile{Key: key, Schema: cacheSchema, Media: *m})
	if err != nil {
		return err
	}
	return cachePut(dir, key, data)
}

// cachePut stores one encoded record, creating the cache if this is its first
// write. The write is committed (flush + fsync) before this returns, so a
// stored record is durable exactly like the atomic temp+rename it replaces.
func cachePut(dir, key string, data []byte) error {
	if dir == "" {
		return fmt.Errorf("metadata: cache put without a cache directory")
	}
	return withCacheWrite(dir, func(db *store.DB) error {
		if err := db.Put(key, data); err != nil {
			return fmt.Errorf("metadata: cache put %q: %w", key, err)
		}
		return nil
	})
}
