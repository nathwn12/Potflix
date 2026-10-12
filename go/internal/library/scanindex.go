package library

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// scanindex.go is the folder tier of the library index: the two content
// identities (tier-1 names hash, tier-2 fingerprint) and the folder rollups
// the scan's fast path consults.
//
// The rollups live in the store-backed index (index.go): the JSON file this
// file used to read and rewrite in full is retired. Its record shape and its
// exported API are unchanged, so every caller keeps working - the medium
// changed, not the contract:
//
//   - LoadScanIndex(path) opens the store beside path (library.idx; see
//     IndexStorePath) and, when the store is empty and the legacy JSON
//     exists, imports it and renames it to <name>.bak.
//   - Save/SaveIfDirty persist staged rollups into the store derived from
//     their path argument and fsync (still atomic: the store appends frames
//     behind a CRC and repairs a torn tail on the next open, which the JSON
//     temp-file-plus-rename bought before).
//   - a missing, unreadable or corrupt index is an empty index, never an
//     error to the caller.
//
// One behaviour is new because the file medium changed under it: a rollup
// records whether per-file records exist for it (perFile). A rollup imported
// from the legacy JSON has none - that file never held per-file data - so it
// does not skip; its folder is enumerated normally on the next scan and
// re-stamped with per-file data.

// normalizeScanDir canonicalises a directory path for use as an index key:
// filepath.Clean plus lowercasing on Windows, so the same folder never gets
// two records under different spellings.
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
	fps := make([]fpEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			return "", err
		}
		fe := fpEntry{name: e.Name(), size: info.Size(), mtimeUnixNano: info.ModTime().UnixNano(), isDir: e.IsDir()}
		if e.IsDir() {
			sub, err := os.ReadDir(filepath.Join(dir, e.Name()))
			if err != nil {
				return "", err
			}
			sort.Slice(sub, func(i, j int) bool { return sub[i].Name() < sub[j].Name() })
			for _, s := range sub {
				sinfo, err := s.Info()
				if err != nil {
					return "", err
				}
				fe.sub = append(fe.sub, fpSub{name: s.Name(), size: sinfo.Size()})
			}
		}
		fps = append(fps, fe)
	}
	return hashFingerprint(fi.ModTime().UnixNano(), fps), nil
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
	return hashNames(fi.ModTime().UnixNano(), len(names), names), nil
}

// fpEntry is one directory entry's hashed fields for the tier-2
// fingerprint: name, size and mtime for the entry itself, plus the sorted
// immediate names/sizes of a subdirectory one level deeper.
type fpEntry struct {
	name          string
	size          int64
	mtimeUnixNano int64
	isDir         bool
	sub           []fpSub
}

// fpSub is one grandchild entry's hashed fields (name + size only, matching
// FolderFingerprint's clock-independent second level).
type fpSub struct {
	name string
	size int64
}

// hashNames is the tier-1 hash core: dir mtime (unix nanos), immediate
// entry count, sorted entry names. FolderNamesHash and the scan walk's
// cached listing path share it so both spell the record identically.
func hashNames(modUnixNano int64, count int, names []string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d\n%d\n", modUnixNano, count)
	for _, n := range names {
		fmt.Fprintf(h, "%s\n", n)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// hashFingerprint is the tier-2 hash core over entries sorted by name.
// FolderFingerprint and the scan walk's cached listing path share it so
// both spell the record identically.
func hashFingerprint(modUnixNano int64, entries []fpEntry) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d\n%d\n", modUnixNano, len(entries))
	for _, e := range entries {
		fmt.Fprintf(h, "%s\n%d\n%d\n", e.name, e.size, e.mtimeUnixNano)
		if e.isDir {
			fmt.Fprintf(h, "%d\n", len(e.sub))
			for _, s := range e.sub {
				fmt.Fprintf(h, "%s\n%d\n", s.name, s.size)
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// FolderRecord is one indexed folder: the tier-2 fingerprint seen at scan
// time, the tier-1 names hash, the metadata schema it was scanned under, the
// CACHE keys (metadata.CacheKeyFor, one per unique show/season or
// movie/year) found beneath it, the video file count beneath it (Files,
// recursive, including KindUnknown files, so skipped-folder tallies still add
// up), and when the scan ran. NamesHash/Files are additive: older index files
// without them load with zero values and simply miss tier 1 until
// re-recorded.
//
// perFile is this index's own addition (never serialized to the legacy JSON,
// which is now read-only migration input): true when the store holds per-file
// records for this folder. A rollup without them must not skip, or the first
// scan after a migration would skip exactly the folders whose per-file
// records it is supposed to create.
type FolderRecord struct {
	Fingerprint string    `json:"fingerprint"`
	NamesHash   string    `json:"names_hash"`
	Files       int       `json:"files"`
	Schema      int       `json:"schema"`
	Keys        []string  `json:"keys"`
	ScannedAt   time.Time `json:"scanned_at"`

	perFile bool
}

// ScanIndex is the folder tier of the store-backed library index: one rollup
// record per directory, persisted through internal/store. The zero value is
// usable (an in-memory-only index that persists nothing).
//
// Saves are dirty-gated: Record/RecordFull stage a record only when its
// content actually changes (ScannedAt aside, so re-recording identical
// content is a no-op), and the scan's checkpoint flushes persist only then.
type ScanIndex struct {
	ix *Index
}

// sameScanRecord reports whether two records carry the same content.
// ScannedAt is deliberately excluded: it is write-only (never read by the
// skip guard), so re-recording identical content must not dirty the index.
func sameScanRecord(a, b FolderRecord) bool {
	if a.Fingerprint != b.Fingerprint || a.NamesHash != b.NamesHash ||
		a.Files != b.Files || a.Schema != b.Schema || a.perFile != b.perFile ||
		len(a.Keys) != len(b.Keys) {
		return false
	}
	for i := range a.Keys {
		if a.Keys[i] != b.Keys[i] {
			return false
		}
	}
	return true
}

// maxScanIndexBytes caps how much of a legacy scanindex.json the migration
// reader will read: a larger file is treated as absent/corrupt (an empty
// import, never an error to the caller) so a huge or hostile file cannot
// exhaust memory.
const maxScanIndexBytes = 8 << 20 // 8 MB

// LegacyCounts is the inventory of a legacy scanindex.json: its folder
// records, the video-file total they claim, the distinct metadata keys they
// name (Keys) and the raw number of key REFERENCES (KeyRefs: the per-folder
// key lists summed, so cross-folder duplicates count once per folder).
type LegacyCounts struct {
	Folders int
	Files   int
	Keys    int
	KeyRefs int
}

// readLegacyScanIndex reads the retired JSON index, tolerating exactly what
// the old loader tolerated: a missing, unreadable, oversized or corrupt file
// yields an empty map and an error the caller logs. It never prunes and never
// writes.
func readLegacyScanIndex(path string) (map[string]FolderRecord, LegacyCounts, error) {
	var counts LegacyCounts
	f, err := os.Open(path)
	if err != nil {
		return nil, counts, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxScanIndexBytes+1))
	if err != nil {
		return nil, counts, err
	}
	if len(data) > maxScanIndexBytes {
		return nil, counts, fmt.Errorf("legacy index %q exceeds %d bytes", path, maxScanIndexBytes)
	}
	var raw map[string]FolderRecord
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, counts, err
	}
	out := make(map[string]FolderRecord, len(raw))
	keys := make(map[string]bool)
	for k, v := range raw {
		out[normalizeScanDir(k)] = v
		counts.Files += v.Files
		for _, key := range v.Keys {
			if key != "" {
				keys[key] = true
				counts.KeyRefs++
			}
		}
	}
	counts.Folders = len(out)
	counts.Keys = len(keys)
	return out, counts, nil
}

// LegacyScanIndexCounts reports the folder/file/key totals of the legacy JSON
// index at path without touching the store: the migration parity check.
func LegacyScanIndexCounts(path string) (LegacyCounts, error) {
	_, counts, err := readLegacyScanIndex(path)
	return counts, err
}

// LoadScanIndex opens the store-backed index that replaces the index file at
// path (see IndexStorePath) and, on first use of an empty store, imports a
// legacy scanindex.json at path. A missing, unreadable or corrupt index
// yields an empty index, never an error to the caller.
func LoadScanIndex(path string) *ScanIndex {
	ix, err := sharedIndex(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "library: index %q: %v\n", path, err)
	}
	if ix == nil {
		ix = &Index{folders: map[string]FolderRecord{}, files: map[string]*FileRecord{}}
	}
	return &ScanIndex{ix: ix}
}

// Save persists every staged record into the store derived from path and
// fsyncs it. It never leaves a partial index: the store appends CRC-framed
// records and repairs a torn tail on the next open.
func (ix *ScanIndex) Save(path string) error {
	if ix == nil || ix.ix == nil {
		return errors.New("library: scanindex: nil index")
	}
	return ix.ix.flushTo(path)
}

// SaveIfDirty persists the index only when records actually changed since
// the last save; a clean index is left untouched, so the scan's checkpoint
// cadence (every N folders or M seconds) costs a mutex check instead of a
// write when nothing moved. A save failure is returned to the caller (scan
// flushes log it, never fail).
func (ix *ScanIndex) SaveIfDirty(path string) error {
	if ix == nil || ix.ix == nil {
		return errors.New("library: scanindex: nil index")
	}
	if !ix.ix.isDirty() {
		return nil
	}
	return ix.ix.flushTo(path)
}

// ShouldSkip reports whether dir can be skipped entirely: a record exists,
// it carries per-file records, its fingerprint and schema match, and every
// recorded key still verifies via keyPresent (called directly per key, never
// inferred from a count or a list). A nil keyPresent cannot verify, so it
// reports false. Safe for concurrent use and for calls from a walk callback.
func (ix *ScanIndex) ShouldSkip(dir, fp string, schema int, keyPresent func(key string) bool) bool {
	if ix == nil || ix.ix == nil || keyPresent == nil {
		return false
	}
	rec, ok := ix.ix.folder(dir)
	if !ok {
		return false
	}
	if !rec.perFile {
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
	if rec.Files > 0 && len(rec.Keys) == 0 {
		return false
	}
	if rec.Files > 0 && len(rec.Keys) > rec.Files {
		return false
	}
	for _, k := range rec.Keys {
		if !keyPresent(k) {
			return false
		}
	}
	return true
}

// ShouldSkipNames is the tier-1 skip check: a record exists, it carries
// per-file records, its names hash and schema match, and every recorded key
// still verifies via keyPresent. Records written before NamesHash existed
// (empty) never match here; they fall through to the tier-2 ShouldSkip check.
func (ix *ScanIndex) ShouldSkipNames(dir, namesHash string, schema int, keyPresent func(key string) bool) bool {
	if ix == nil || ix.ix == nil || keyPresent == nil || namesHash == "" {
		return false
	}
	rec, ok := ix.ix.folder(dir)
	if !ok {
		return false
	}
	if !rec.perFile {
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
	if rec.Files > 0 && len(rec.Keys) == 0 {
		return false
	}
	if rec.Files > 0 && len(rec.Keys) > rec.Files {
		return false
	}
	for _, k := range rec.Keys {
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
	if ix == nil || ix.ix == nil {
		return FolderRecord{}, false
	}
	return ix.ix.folder(dir)
}

// RecordFull stores (or replaces) the full two-tier record for dir. The
// index is marked dirty only when the record's content actually changes, so
// re-recording identical content never forces a write. It marks the folder
// as carrying per-file data (the scan only calls it after diffing them).
func (ix *ScanIndex) RecordFull(dir, namesHash, fp string, schema int, keys []string, files int) {
	if ix == nil || ix.ix == nil {
		return
	}
	ix.ix.putFolder(dir, FolderRecord{
		Fingerprint: fp,
		NamesHash:   namesHash,
		Files:       files,
		Schema:      schema,
		Keys:        append([]string(nil), keys...),
		ScannedAt:   time.Now().UTC(),
		perFile:     true,
	})
}

// Record stores (or replaces) the record for dir.
//
// Back-compat: it sets the tier-2 fingerprint, schema, keys and scan time,
// preserving any existing tier-1 NamesHash/Files for the same dir so a
// legacy call never wipes a newer two-tier record's fast-path fields.
func (ix *ScanIndex) Record(dir, fp string, schema int, keys []string) {
	if ix == nil || ix.ix == nil {
		return
	}
	prev, _ := ix.ix.folder(dir)
	ix.ix.putFolder(dir, FolderRecord{
		Fingerprint: fp,
		NamesHash:   prev.NamesHash,
		Files:       prev.Files,
		Schema:      schema,
		Keys:        append([]string(nil), keys...),
		ScannedAt:   time.Now().UTC(),
		perFile:     true,
	})
}

// Len counts indexed folders.
func (ix *ScanIndex) Len() int {
	if ix == nil || ix.ix == nil {
		return 0
	}
	return ix.ix.folderCount()
}

// PruneExisting drops records whose directory no longer exists (one stat
// each), keeping the index small. It stages the removals. Only the scan write
// path calls it (via recordIndex, while scanning live roots); plain loads
// never prune.
func (ix *ScanIndex) PruneExisting() {
	if ix == nil || ix.ix == nil {
		return
	}
	ix.ix.pruneFolders()
}
