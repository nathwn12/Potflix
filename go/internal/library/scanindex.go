package library

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// normalizeScanDir canonicalises a directory path for use as a ScanIndex map
// key: filepath.Clean plus lowercasing on Windows, so the same folder never
// gets two records under different spellings.
func normalizeScanDir(dir string) string {
	c := filepath.Clean(dir)
	if runtime.GOOS == "windows" {
		c = strings.ToLower(c)
	}
	return c
}

// FolderFingerprint is a cheap identity for a directory's contents: a hash
// of the dir's own mtime (unix nanos), the child count, and the sorted
// child entries (name + size + mtime) for FILES AND DIRECTORIES alike, plus
// one deterministic level deeper: for each immediate subdirectory, the
// count and sorted names/sizes of ITS immediate entries. That second level
// is clock-independent on purpose: child-directory mtimes observed through
// os.ReadDir/DirEntry.Info can lag a direct os.Stat of the same directory
// on some Windows volumes (measured: entry channel stale, stat channel
// fresh, never converging), so a hash that only moves when that clock
// moves misses real nested changes. Folding grandchildren names/sizes
// keeps the recursive short-circuit correct: adding a file inside a
// subfolder changes the parent's fingerprint on the first read on any
// filesystem.
func FolderFingerprint(dir string) (string, error) {
	fi, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("library: scanindex: not a directory %q", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	h := sha256.New()
	fmt.Fprintf(h, "%d\n%d\n", fi.ModTime().UnixNano(), len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\n%d\n%d\n", e.Name(), info.Size(), info.ModTime().UnixNano())
		if e.IsDir() {
			sub, err := os.ReadDir(filepath.Join(dir, e.Name()))
			if err != nil {
				return "", err
			}
			sort.Slice(sub, func(i, j int) bool { return sub[i].Name() < sub[j].Name() })
			fmt.Fprintf(h, "%d\n", len(sub))
			for _, s := range sub {
				sinfo, err := s.Info()
				if err != nil {
					return "", err
				}
				fmt.Fprintf(h, "%s\n%d\n", s.Name(), sinfo.Size())
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// FolderNamesHash is the cheap tier-1 identity for a directory: a hash of
// the dir's own mtime (unix nanos), the number of immediate entries, and
// the sorted entry NAMES (files and directories alike). It never stats a
// child: names come straight from the directory listing, so a
// rename/add/remove is always caught even when the dir mtime lags under
// heavy I/O. A tier-1 match means "surely unchanged, skip"; a mismatch
// falls through to the tier-2 FolderFingerprint check.
func FolderNamesHash(dir string) (string, error) {
	fi, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("library: scanindex: not a directory %q", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	h := sha256.New()
	fmt.Fprintf(h, "%d\n%d\n", fi.ModTime().UnixNano(), len(names))
	for _, n := range names {
		fmt.Fprintf(h, "%s\n", n)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// FolderRecord is one indexed folder: the tier-2 fingerprint seen at scan
// time, the tier-1 names hash, the metadata schema it was scanned under,
// the CACHE keys (metadata.CacheKeyFor, one per unique show/season or
// movie/year) found beneath it, the video file count beneath it
// (Files, recursive, including KindUnknown files, so skipped-folder
// tallies still add up), and when the scan ran. NamesHash/Files are
// additive: older index files without them load with zero values and
// simply miss tier 1 until re-recorded.
type FolderRecord struct {
	Fingerprint string    `json:"fingerprint"`
	NamesHash   string    `json:"names_hash"`
	Files       int       `json:"files"`
	Schema      int       `json:"schema"`
	Keys        []string  `json:"keys"`
	ScannedAt   time.Time `json:"scanned_at"`
}

// ScanIndex is a folder-level scan cache: one record per directory. The zero
// value is usable; LoadScanIndex is the tolerant file-backed constructor.
type ScanIndex struct {
	mu   sync.Mutex
	recs map[string]FolderRecord
}

// maxScanIndexBytes caps how much LoadScanIndex will read: a larger file
// is treated as absent/corrupt (an empty index, never an error to the
// caller) so a huge or hostile file cannot exhaust memory.
const maxScanIndexBytes = 8 << 20 // 8 MB

// LoadScanIndex reads the index file at path. A missing, unreadable,
// oversized, or corrupt file yields an empty index, never an error to the
// caller.
func LoadScanIndex(path string) *ScanIndex {
	ix := &ScanIndex{recs: make(map[string]FolderRecord)}
	f, err := os.Open(path)
	if err != nil {
		return ix
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxScanIndexBytes+1))
	if err != nil {
		return ix
	}
	if len(data) > maxScanIndexBytes {
		return ix
	}
	var raw map[string]FolderRecord
	if err := json.Unmarshal(data, &raw); err != nil {
		return ix
	}
	for k, v := range raw {
		ix.recs[normalizeScanDir(k)] = v
	}
	return ix
}

// Save writes the index atomically (temp file + rename in the same
// directory, MkdirAll 0o755), mirroring config.go/cache.go. It holds the
// index mutex for the whole write so concurrent Record/Save callers are
// safe, and it never leaves a partial file at path.
func (ix *ScanIndex) Save(path string) error {
	if ix == nil {
		return fmt.Errorf("library: scanindex: nil index")
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(ix.recs)
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

// ShouldSkip reports whether dir can be skipped entirely: a record exists,
// its fingerprint and schema match, and every recorded key still verifies
// via keyPresent (called directly per key, never inferred from a count or a
// list). A nil keyPresent cannot verify, so it reports false. Safe for
// concurrent use and for calls from a walk callback.
func (ix *ScanIndex) ShouldSkip(dir, fp string, schema int, keyPresent func(key string) bool) bool {
	if ix == nil || keyPresent == nil {
		return false
	}
	ix.mu.Lock()
	rec, ok := ix.recs[normalizeScanDir(dir)]
	keys := append([]string(nil), rec.Keys...)
	ix.mu.Unlock()
	if !ok {
		return false
	}
	if rec.Fingerprint != fp {
		return false
	}
	if rec.Schema != schema {
		return false
	}
	// The index is local-trust: a truncated or vacuous record must never
	// hide unresolved files, so fail closed on inconsistent counts. Files
	// is the recursive video file count recorded at scan time and Keys is
	// the deduped cache-key list (one per unique show/season or
	// movie/year): the deduped list can never be longer than the file
	// count, and a record claiming files but naming no keys is truncated.
	// Files == 0 means unknown (records written before Files existed load
	// with zero), so the guards apply only when Files > 0.
	if rec.Files > 0 && len(keys) == 0 {
		return false
	}
	if rec.Files > 0 && len(keys) > rec.Files {
		return false
	}
	for _, k := range keys {
		if !keyPresent(k) {
			return false
		}
	}
	return true
}

// ShouldSkipNames is the tier-1 skip check: a record exists, its names
// hash and schema match, and every recorded key still verifies via
// keyPresent. Records written before NamesHash existed (empty) never
// match here; they fall through to the tier-2 ShouldSkip check.
func (ix *ScanIndex) ShouldSkipNames(dir, namesHash string, schema int, keyPresent func(key string) bool) bool {
	if ix == nil || keyPresent == nil || namesHash == "" {
		return false
	}
	ix.mu.Lock()
	rec, ok := ix.recs[normalizeScanDir(dir)]
	keys := append([]string(nil), rec.Keys...)
	ix.mu.Unlock()
	if !ok {
		return false
	}
	if rec.NamesHash == "" || rec.NamesHash != namesHash {
		return false
	}
	if rec.Schema != schema {
		return false
	}
	// Same local-trust fail-closed guards as ShouldSkip (see above):
	// Files == 0 means unknown, so they apply only when Files > 0.
	if rec.Files > 0 && len(keys) == 0 {
		return false
	}
	if rec.Files > 0 && len(keys) > rec.Files {
		return false
	}
	for _, k := range keys {
		if !keyPresent(k) {
			return false
		}
	}
	return true
}

// Lookup returns a copy of the record for dir, if any. Callers use it to
// read the recorded Files count for skipped-folder tallies and to refresh
// tier-1 values after a tier-2 hit.
func (ix *ScanIndex) Lookup(dir string) (FolderRecord, bool) {
	if ix == nil {
		return FolderRecord{}, false
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	rec, ok := ix.recs[normalizeScanDir(dir)]
	if !ok {
		return FolderRecord{}, false
	}
	rec.Keys = append([]string(nil), rec.Keys...)
	return rec, true
}

// RecordFull stores (or replaces) the full two-tier record for dir.
func (ix *ScanIndex) RecordFull(dir, namesHash, fp string, schema int, keys []string, files int) {
	if ix == nil {
		return
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.recs == nil {
		ix.recs = make(map[string]FolderRecord)
	}
	ix.recs[normalizeScanDir(dir)] = FolderRecord{
		Fingerprint: fp,
		NamesHash:   namesHash,
		Files:       files,
		Schema:      schema,
		Keys:        append([]string(nil), keys...),
		ScannedAt:   time.Now().UTC(),
	}
}

// Record stores (or replaces) the record for dir.
//
// Back-compat: it sets the tier-2 fingerprint, schema, keys and scan time,
// preserving any existing tier-1 NamesHash/Files for the same dir so a
// legacy call never wipes a newer two-tier record's fast-path fields.
func (ix *ScanIndex) Record(dir, fp string, schema int, keys []string) {
	if ix == nil {
		return
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.recs == nil {
		ix.recs = make(map[string]FolderRecord)
	}
	key := normalizeScanDir(dir)
	prev := ix.recs[key]
	ix.recs[key] = FolderRecord{
		Fingerprint: fp,
		NamesHash:   prev.NamesHash,
		Files:       prev.Files,
		Schema:      schema,
		Keys:        append([]string(nil), keys...),
		ScannedAt:   time.Now().UTC(),
	}
}

// Len counts indexed folders.
func (ix *ScanIndex) Len() int {
	if ix == nil {
		return 0
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return len(ix.recs)
}

// PruneExisting drops records whose directory no longer exists (one stat
// each), keeping the index small.
func (ix *ScanIndex) PruneExisting() {
	if ix == nil {
		return
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	for k := range ix.recs {
		if _, err := os.Stat(k); err != nil && os.IsNotExist(err) {
			delete(ix.recs, k)
		}
	}
}
