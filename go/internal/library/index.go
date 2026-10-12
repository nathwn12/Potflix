// index.go is the store-backed library index: one crash-safe single-file
// key/value store (internal/store) holding a record per video file plus a
// rollup per folder, replacing scanindex.json.
//
// Why per-file: the JSON index was folder-granular (the owner's real library:
// 928 folders, 23,402 files, 7,378 metadata keys), so every decision was a
// whole-folder decision - one new episode re-walked and re-fetched its whole
// folder. Per-file records let one walk answer "what changed" from (size,
// mtime) alone, and let the metadata work run separately, in the background,
// resuming at launch.
//
// Key space (the store's keys are byte-exact, so paths are normalized here:
// filepath.Clean plus lower-casing on Windows - the same folding the JSON
// index used):
//
//	f|<path>  per-file record (encodeFileRecord)
//	d|<path>  per-folder rollup (encodeFolderRecord)
//	m|index   index-level metadata (schema version, migration stamp)
//
// Values are opaque to the store, so the encoding is ours: a version byte,
// then LEB128 varints and length-prefixed strings. A per-file record for a
// parsed 60-character path measures ~120 bytes against ~150 bytes of JSON
// (TestFileRecordEncodingBudget).
//
// # Handle lifetime and durability
//
// The index keeps its records in memory and holds the store file OPEN ONLY
// WHILE IT WRITES. Mutations stage in dirty sets; a commit opens the store,
// appends the staged frames, fsyncs and closes it again. That keeps the file
// unlocked for every other process (a Windows temp-dir delete, a backup, an
// external reader) and means a hard kill loses at most the staged, uncommitted
// changes - which are re-derived by the next scan or catch-up pass. A torn
// tail (a kill during a commit) is repaired by the store on the next open and
// is never an error here.
package library

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"potflix/internal/metadata"
	"potflix/internal/store"
)

// The store file lives beside the legacy index path, so
// %LOCALAPPDATA%\Potflix\scanindex.json maps to
// %LOCALAPPDATA%\Potflix\library.idx.
const (
	indexStoreName  = "library.idx"
	legacyBakSuffix = ".bak"

	recKeyFile   = "f|"
	recKeyFolder = "d|"
	recKeyMeta   = "m|index"

	// encFileVersion / encFolderVersion version the value payloads, not the
	// store's own frame format (that is locked by internal/store).
	encFileVersion   = 1
	encFolderVersion = 1
)

// Per-file metadata state. Store records use exactly these strings, so an
// index inspection (or a repair report) reads them without a mapping table.
const (
	// StateResolved means the file's metadata key has an answer (a found
	// resolution or a live negative) and nothing is pending.
	StateResolved = "resolved"
	// StatePending means the file still needs a metadata lookup.
	StatePending = "pending"
	// StateNeedsRepair means maxRepairAttempts lookups produced no usable
	// metadata; it is not retried before its NextTry.
	StateNeedsRepair = "needs_repair"
)

// File kinds, stored as one byte each.
const (
	fileKindUnknown = "unknown"
	fileKindSeries  = "series"
	fileKindMovie   = "movie"
)

// Metadata catch-up policy.
const (
	// maxRepairAttempts is how many failed lookups an entry gets before it
	// is parked as needs_repair.
	maxRepairAttempts = 3
	// repairCooldown is how long a needs_repair entry waits before its next
	// try is allowed.
	repairCooldown = 6 * time.Hour
	// catchUpConcurrency bounds concurrent lookups in the background worker.
	catchUpConcurrency = 3
	// catchUpCommitInterval bounds how long an applied verdict sits
	// uncommitted during a pass. Each commit reopens and replays the store
	// (the file is held open only while writing), so the interval trades a
	// little crash-window for a lot of replay: 15 s of verdicts are redoable
	// from the index's own state, and every pass re-derives whatever it
	// loses.
	catchUpCommitInterval = 15 * time.Second
)

// indexStoreOptions configures the store: no per-write fsync (a torn tail is
// repaired on the next open, and this index is a cache), an 8 MiB delta
// budget before a self-compacting rewrite, and a generous value cap (a folder
// rollup lists every distinct key beneath it).
var indexStoreOptions = store.Options{
	SyncOnCommit:     false,
	CompactThreshold: 8 << 20,
	MaxValueSize:     4 << 20,
}

// FileRecord is one indexed video file: identity, the filesystem stamp the
// diff compares, the parsed query, and the metadata state machine for the
// file's cache key.
type FileRecord struct {
	Path      string // full path, original case
	Name      string // basename
	Ext       string // lower-case extension, e.g. ".mkv"
	Size      int64
	Mtime     int64 // unix nanos
	Kind      string
	Title     string
	Season    int
	Episode   int
	Year      int
	Absolute  bool
	MetaKey   string // metadata.CacheKeyFor(Query()); "" for unknown kinds
	State     string
	Attempts  int
	LastError string
	LastTry   int64 // unix nanos, last metadata attempt
	NextTry   int64 // unix nanos, needs_repair retry gate
}

// Query rebuilds the parsed query this record was written from. The stored
// fields are exactly the ones cacheKey() reads (Kind, Title, Year, Season), so
// metadata.CacheKeyFor(rec.Query()) == rec.MetaKey for every record the scan
// wrote (pinned by TestFileRecordQueryRoundTrip).
func (r FileRecord) Query() metadata.Query {
	k := metadata.KindUnknown
	switch r.Kind {
	case fileKindSeries:
		k = metadata.KindSeries
	case fileKindMovie:
		k = metadata.KindMovie
	}
	return metadata.Query{Kind: k, Title: r.Title, Year: r.Year, Season: r.Season, Episode: r.Episode, Absolute: r.Absolute}
}

// indexMeta is the index-level record: the metadata cache schema the records
// were written under, and the migration stamp.
type indexMeta struct {
	Schema          int
	MigratedAt      int64
	MigratedFolders int
}

// Index is the store-backed index. Records live in memory; the store file is
// opened only for a commit (see the package comment on handle lifetime).
//
// An Index with a failed open stays usable in memory: reads and writes work,
// commits report openErr, and the caller logs it - the JSON index treated a
// missing or corrupt file as an empty index, never an error to the scan.
type Index struct {
	mu     sync.Mutex
	path   string // store file path
	legacy string // legacy scanindex.json this index was opened from

	folders map[string]FolderRecord // normalized dir -> rollup
	files   map[string]*FileRecord  // normalized path -> record
	meta    indexMeta

	dirtyFolders map[string]bool // rollups to write (missing = delete)
	dirtyFiles   map[string]bool // file records to write
	goneFiles    map[string]bool // file records to delete
	dirtyMeta    bool

	openErr error
	scratch []byte // encode buffer reused under the lock
}

// IndexStorePath maps a legacy index path (e.g.
// %LOCALAPPDATA%\Potflix\scanindex.json, from ScanIndexPath) to the store
// file that replaces it. An empty path has no store.
func IndexStorePath(indexPath string) string {
	if indexPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(indexPath), indexStoreName)
}

// --- lifecycle -------------------------------------------------------------

// liveIndexes hands out one open Index per store path, so the foreground scan
// and the background catch-up share one handle and one decode of the records
// (the store supports a single writer per file).
var (
	liveIdxMu sync.Mutex
	liveIdx   = map[string]*Index{}
)

// sharedIndex returns the process-wide index for the legacy index path,
// opening it on first use. An open failure comes back as a usable
// in-memory-only index plus the error (see Index).
func sharedIndex(indexPath string) (*Index, error) {
	sp := IndexStorePath(indexPath)
	if sp == "" {
		return nil, fmt.Errorf("library: index: empty index path")
	}
	liveIdxMu.Lock()
	defer liveIdxMu.Unlock()
	if ix, ok := liveIdx[sp]; ok {
		return ix, ix.openErr
	}
	ix := openIndex(indexPath)
	liveIdx[sp] = ix
	return ix, ix.openErr
}

// CloseIndex drops the index for indexPath from the process-wide set, so the
// next use reopens (and re-reads) the file. Tests use it to prove that
// reopening restores state; the app may call it on shutdown.
func CloseIndex(indexPath string) error {
	sp := IndexStorePath(indexPath)
	if sp == "" {
		return fmt.Errorf("library: index: empty index path")
	}
	liveIdxMu.Lock()
	delete(liveIdx, sp)
	liveIdxMu.Unlock()
	return nil
}

// ClearStoreIndex forgets and deletes the store-backed index for indexPath and
// returns the folder-record count it held.
//
// The Settings "Clear cache" path must call this INSTEAD of removing the
// legacy index path: the records live in library.idx beside it (IndexStorePath),
// so removing scanindex.json - which migration already renamed to .bak - leaves
// the real index untouched. The file is not open between commits (see the
// package comment on handle lifetime), so the removal cannot fail on a lock.
func ClearStoreIndex(indexPath string) (int, error) {
	sp := IndexStorePath(indexPath)
	if sp == "" {
		return 0, fmt.Errorf("library: index: empty index path")
	}
	folders := 0
	if ix, _ := sharedIndex(indexPath); ix != nil {
		folders = ix.folderCount()
	}
	if err := CloseIndex(indexPath); err != nil {
		return folders, err
	}
	if err := os.Remove(sp); err != nil && !os.IsNotExist(err) {
		return folders, err
	}
	return folders, nil
}

// openIndex opens (creating if needed) the store beside indexPath, decodes
// every record, and - when the store is empty and a legacy scanindex.json
// exists - imports the legacy folder records and renames the JSON to
// <name>.bak. It never returns nil.
func openIndex(indexPath string) *Index {
	ix := &Index{
		path:         IndexStorePath(indexPath),
		legacy:       indexPath,
		folders:      make(map[string]FolderRecord),
		files:        make(map[string]*FileRecord),
		dirtyFolders: make(map[string]bool),
		dirtyFiles:   make(map[string]bool),
		goneFiles:    make(map[string]bool),
	}
	if err := os.MkdirAll(filepath.Dir(ix.path), 0o755); err != nil {
		ix.openErr = fmt.Errorf("library: index: mkdir %s: %w", filepath.Dir(ix.path), err)
		return ix
	}
	db, err := store.Open(ix.path, indexStoreOptions)
	if err != nil {
		// A file this layer cannot read as a store (bad magic, a truncated
		// header, an alien file) is quarantined instead of fatal: the index
		// is a cache, and the JSON index treated a corrupt file as an empty
		// one. The unreadable file is renamed, never deleted.
		quarantine := ix.path + ".corrupt"
		if rerr := os.Rename(ix.path, quarantine); rerr == nil {
			fmt.Fprintf(os.Stderr, "library: index %q: %v (quarantined as %s)\n", ix.path, err, filepath.Base(quarantine))
			db, err = store.Open(ix.path, indexStoreOptions)
		}
	}
	if err != nil {
		ix.openErr = err
		return ix
	}
	ix.decodeAll(db)
	empty := db.Len() == 0
	if err := db.Close(); err != nil && ix.openErr == nil {
		ix.openErr = err
	}
	if empty {
		ix.importLegacy()
	}
	return ix
}

// decodeAll rebuilds the in-memory maps from the open store. A value that
// fails to decode is dropped and counted to stderr: a record this layer
// cannot read is re-derived by the next scan, never a hard failure.
func (ix *Index) decodeAll(db *store.DB) {
	bad := 0
	_ = db.ForEach(func(k string, v []byte) bool {
		switch {
		case len(k) > 2 && k[:2] == recKeyFile:
			if rec, ok := decodeFileRecord(v); ok {
				ix.files[k[2:]] = &rec
			} else {
				bad++
			}
		case len(k) > 2 && k[:2] == recKeyFolder:
			if rec, ok := decodeFolderRecord(v); ok {
				ix.folders[k[2:]] = rec
			} else {
				bad++
			}
		case k == recKeyMeta:
			if m, ok := decodeIndexMeta(v); ok {
				ix.meta = m
			}
		}
		return true
	})
	if bad > 0 {
		fmt.Fprintf(os.Stderr, "library: index %q: dropped %d undecodable record(s)\n", ix.path, bad)
	}
}

// importLegacy imports the legacy JSON index once, when the store is empty and
// the JSON exists, then renames the JSON to <name>.bak - never deletes it.
// Imported rollups carry no per-file data (the JSON has none), so they are
// marked not-skippable and the next scan enumerates their files normally; the
// import's value is the folder inventory, the file and key totals, and the
// migration record itself.
func (ix *Index) importLegacy() {
	if ix.legacy == "" {
		return
	}
	fi, err := os.Stat(ix.legacy)
	if err != nil || fi.IsDir() {
		return
	}
	raw, counts, err := readLegacyScanIndex(ix.legacy)
	if err != nil {
		// Corrupt or oversized: tolerated exactly as the JSON loader
		// tolerated it, but the file is still parked as .bak so it cannot
		// shadow the store on every later open.
		fmt.Fprintf(os.Stderr, "library: index migrate %q: %v (importing nothing)\n", ix.legacy, err)
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	for dir, rec := range raw {
		rec.perFile = false // the legacy file has no per-file records
		key := normalizeScanDir(dir)
		ix.folders[key] = rec
		ix.dirtyFolders[key] = true
	}
	ix.meta = indexMeta{Schema: metadata.CacheSchema(), MigratedAt: time.Now().UTC().UnixNano(), MigratedFolders: len(raw)}
	ix.dirtyMeta = true
	if err := ix.commitLocked(); err != nil {
		fmt.Fprintf(os.Stderr, "library: index migrate %q: commit: %v\n", ix.legacy, err)
		return
	}
	bak := ix.legacy + legacyBakSuffix
	if err := os.Rename(ix.legacy, bak); err != nil {
		fmt.Fprintf(os.Stderr, "library: index migrate %q: rename to %q: %v\n", ix.legacy, bak, err)
		return
	}
	fmt.Fprintf(os.Stderr,
		"library: index migrated %d folder records (%d files, %d keys) into %q from %q; per-file records are absent in the legacy file, so the next scan enumerates files normally (legacy file renamed %q)\n",
		counts.Folders, counts.Files, counts.Keys, ix.path, ix.legacy, filepath.Base(bak))
}

// commit flushes staged records and fsyncs them.
func (ix *Index) commit() error {
	if ix == nil {
		return nil
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.commitLocked()
}

// isDirty reports whether anything is staged for the next commit.
func (ix *Index) isDirty() bool {
	if ix == nil {
		return false
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.dirtyLocked()
}

func (ix *Index) dirtyLocked() bool {
	return len(ix.dirtyFolders) > 0 || len(ix.dirtyFiles) > 0 || len(ix.goneFiles) > 0 || ix.dirtyMeta
}

// commitLocked opens the store, appends every staged frame, fsyncs, and closes
// the file again. The caller holds mu. With nothing staged it does no I/O; a
// store that cannot be opened reports openErr and keeps the staged state, so
// a later commit can retry it.
func (ix *Index) commitLocked() error {
	if !ix.dirtyLocked() {
		return ix.openErr
	}
	db, err := ix.openStoreLocked()
	if err != nil {
		return err
	}
	if err := ix.writeAllLocked(db, nil); err != nil {
		db.Close()
		return err
	}
	if err := db.Commit(); err != nil {
		db.Close()
		return err
	}
	if err := db.Close(); err != nil {
		return err
	}
	ix.clearDirtyLocked()
	return nil
}

// openStoreLocked opens the index's own store, replaying it (the store needs
// its own view of the file to append correctly). The caller holds mu.
func (ix *Index) openStoreLocked() (*store.DB, error) {
	db, err := store.Open(ix.path, indexStoreOptions)
	if err != nil {
		ix.openErr = err
		return nil, err
	}
	ix.openErr = nil
	return db, nil
}

// writeAllLocked writes the staged records (all of them, when only is nil) to
// db, leaving the commit to the caller. The caller holds mu.
func (ix *Index) writeAllLocked(db *store.DB, only map[string]bool) error {
	putFiles := ix.dirtyFiles
	deleteFiles := ix.goneFiles
	putFolders := ix.dirtyFolders
	if only != nil {
		putFiles, deleteFiles, putFolders = map[string]bool{}, map[string]bool{}, map[string]bool{}
		for k := range ix.files {
			putFiles[k] = true
		}
		for k, rec := range ix.folders {
			_ = rec
			putFolders[k] = true
		}
	}
	for k := range putFiles {
		rec, ok := ix.files[k]
		if !ok {
			continue
		}
		ix.scratch = encodeFileRecord(ix.scratch[:0], *rec)
		if err := db.Put(recKeyFile+k, ix.scratch); err != nil {
			return err
		}
	}
	for k := range deleteFiles {
		if err := db.Delete(recKeyFile + k); err != nil {
			return err
		}
	}
	for k := range putFolders {
		rec, ok := ix.folders[k]
		if !ok {
			if err := db.Delete(recKeyFolder + k); err != nil {
				return err
			}
			continue
		}
		ix.scratch = encodeFolderRecord(ix.scratch[:0], rec)
		if err := db.Put(recKeyFolder+k, ix.scratch); err != nil {
			return err
		}
	}
	if ix.dirtyMeta || only != nil {
		ix.scratch = encodeIndexMeta(ix.scratch[:0], ix.meta)
		if err := db.Put(recKeyMeta, ix.scratch); err != nil {
			return err
		}
	}
	return nil
}

// clearDirtyLocked resets the staged sets after a successful commit.
func (ix *Index) clearDirtyLocked() {
	ix.dirtyFolders = make(map[string]bool)
	ix.dirtyFiles = make(map[string]bool)
	ix.goneFiles = make(map[string]bool)
	ix.dirtyMeta = false
}

// flushTo persists the index into the store derived from legacyPath and
// fsyncs. When that is the index's own store, only staged records are written;
// when it is a different file (the legacy Save(path) contract - the records
// must land in the store LoadScanIndex(path) reads), every record is written
// there with a dedicated handle and this index stays where it is.
func (ix *Index) flushTo(legacyPath string) error {
	sp := IndexStorePath(legacyPath)
	if sp == "" {
		return fmt.Errorf("library: index: empty index path")
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if sp == ix.path {
		return ix.commitLocked()
	}
	if err := os.MkdirAll(filepath.Dir(sp), 0o755); err != nil {
		return err
	}
	db, err := store.Open(sp, indexStoreOptions)
	if err != nil {
		return err
	}
	if err := ix.writeAllLocked(db, map[string]bool{}); err != nil {
		db.Close()
		return err
	}
	if err := db.Commit(); err != nil {
		db.Close()
		return err
	}
	ix.clearDirtyLocked()
	return db.Close()
}

// IndexState is a point-in-time tally over the index's records: the totals a
// scan reports and the metadata counters the progress stream carries.
//
// The file buckets partition Files exactly:
// Files == FilesResolved + FilesNoKey + FilesPending + FilesNeedsRepair.
type IndexState struct {
	Folders int
	Files   int
	Keys    int

	// FilesResolved counts records with a metadata key whose answer is
	// settled; FilesNoKey counts records with nothing to look up at all
	// (unknown kinds: trailers, extras); FilesPending counts records still
	// waiting for a lookup; FilesNeedsRepair counts the parked ones.
	FilesResolved    int
	FilesNoKey       int
	FilesPending     int
	FilesNeedsRepair int

	// KeysResolved counts distinct metadata keys with every file settled.
	KeysResolved int
}

// State reports the tally for the index behind this folder view. It is the
// app-facing form of the scan/catch-up counters.
func (ix *ScanIndex) State() IndexState {
	if ix == nil || ix.ix == nil {
		return IndexState{}
	}
	return ix.ix.state()
}

// state tallies the index. One pass over the file records: a key counts as
// resolved only when every file carrying it is.
func (ix *Index) state() IndexState {
	if ix == nil {
		return IndexState{}
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	s := IndexState{Folders: len(ix.folders), Files: len(ix.files)}
	unresolved := make(map[string]bool)
	for _, r := range ix.files {
		switch r.State {
		case StateResolved:
			s.FilesResolved++
		case StateNeedsRepair:
			s.FilesNeedsRepair++
		default:
			s.FilesPending++
		}
		if r.MetaKey == "" {
			// Nothing to look up: move it out of the resolved bucket so the
			// buckets stay honest (and partition Files).
			if r.State == StateResolved {
				s.FilesResolved--
			} else {
				s.FilesPending--
			}
			s.FilesNoKey++
			continue
		}
		if r.State != StateResolved {
			unresolved[r.MetaKey] = true
		} else if _, seen := unresolved[r.MetaKey]; !seen {
			// First resolved file of this key claims it; a later unresolved
			// file of the same key revokes the claim.
			unresolved[r.MetaKey] = false
		}
	}
	for _, bad := range unresolved {
		s.Keys++
		if !bad {
			s.KeysResolved++
		}
	}
	return s
}

// --- folder rollups -------------------------------------------------------

// folder returns a copy of the rollup for dir.
func (ix *Index) folder(dir string) (FolderRecord, bool) {
	if ix == nil {
		return FolderRecord{}, false
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	rec, ok := ix.folders[normalizeScanDir(dir)]
	if !ok {
		return FolderRecord{}, false
	}
	rec.Keys = append([]string(nil), rec.Keys...)
	return rec, true
}

// putFolder stages rec for dir, marking it dirty only when the content
// actually changed (ScannedAt aside), so re-recording an unchanged folder
// appends nothing to the store.
func (ix *Index) putFolder(dir string, rec FolderRecord) {
	if ix == nil {
		return
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	key := normalizeScanDir(dir)
	if prev, ok := ix.folders[key]; ok && sameScanRecord(prev, rec) {
		return
	}
	ix.folders[key] = rec
	ix.dirtyFolders[key] = true
}

// folderNames returns every recorded folder, normalized.
func (ix *Index) folderNames() []string {
	if ix == nil {
		return nil
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	out := make([]string, 0, len(ix.folders))
	for k := range ix.folders {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// folderCount counts recorded folders.
func (ix *Index) folderCount() int {
	if ix == nil {
		return 0
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return len(ix.folders)
}

// pruneFolders drops rollups whose directory no longer exists and reports how
// many it removed. It is one os.Stat per folder, called only from the scan
// write path, so a plain load never prunes a folder that is merely offline.
func (ix *Index) pruneFolders() int {
	if ix == nil {
		return 0
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	n := 0
	for k := range ix.folders {
		if _, err := os.Stat(k); err != nil && os.IsNotExist(err) {
			delete(ix.folders, k)
			ix.dirtyFolders[k] = true
			n++
		}
	}
	return n
}

// --- file records ---------------------------------------------------------

// file returns a copy of the record for path.
func (ix *Index) file(path string) (FileRecord, bool) {
	if ix == nil {
		return FileRecord{}, false
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	r, ok := ix.files[normalizeScanDir(path)]
	if !ok {
		return FileRecord{}, false
	}
	return *r, true
}

// putFile stores rec and stages it for the next commit.
func (ix *Index) putFile(rec FileRecord) {
	if ix == nil {
		return
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.putFileLocked(rec)
}

// putFileLocked stores rec in memory and stages the write. The caller holds
// mu.
func (ix *Index) putFileLocked(rec FileRecord) {
	key := normalizeScanDir(rec.Path)
	cp := rec
	ix.files[key] = &cp
	ix.dirtyFiles[key] = true
	delete(ix.goneFiles, key)
}

// deleteFile removes the record for path, reporting whether it existed.
func (ix *Index) deleteFile(path string) bool {
	if ix == nil {
		return false
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.deleteFileLocked(path)
}

func (ix *Index) deleteFileLocked(path string) bool {
	key := normalizeScanDir(path)
	if _, ok := ix.files[key]; !ok {
		return false
	}
	delete(ix.files, key)
	delete(ix.dirtyFiles, key)
	ix.goneFiles[key] = true
	return true
}

// filesSnapshot copies every file record, ordered by path so a pass over them
// is deterministic (and a report is stable).
func (ix *Index) filesSnapshot() []FileRecord {
	if ix == nil {
		return nil
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	out := make([]FileRecord, 0, len(ix.files))
	for _, r := range ix.files {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// pruneFiles deletes every record whose parent directory was visited but whose
// path was not enumerated in that visit: the file vanished (or stopped being a
// video file). Directories that were not visited are left alone, so an
// unplugged drive never prunes its subtree. It returns how many records it
// removed.
func (ix *Index) pruneFiles(visitedDirs, enumerated map[string]bool) int {
	if ix == nil {
		return 0
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	n := 0
	for key, rec := range ix.files {
		if enumerated[key] {
			continue
		}
		if !visitedDirs[normalizeScanDir(filepath.Dir(rec.Path))] {
			continue
		}
		delete(ix.files, key)
		delete(ix.dirtyFiles, key)
		ix.goneFiles[key] = true
		n++
	}
	return n
}

// --- encoding -------------------------------------------------------------

func putString(dst []byte, s string) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(s)))
	return append(dst, s...)
}

func getString(b []byte, n int) (string, int, bool) {
	l, m := binary.Uvarint(b[n:])
	if m <= 0 || l > uint64(len(b)-(n+m)) {
		return "", 0, false
	}
	off := n + m
	return string(b[off : off+int(l)]), off + int(l), true
}

func getUvarint(b []byte, n int) (uint64, int, bool) {
	v, m := binary.Uvarint(b[n:])
	if m <= 0 {
		return 0, n, false
	}
	return v, n + m, true
}

func kindCode(kind string) byte {
	switch kind {
	case fileKindMovie:
		return 2
	case fileKindSeries:
		return 1
	default:
		return 0
	}
}

func kindFromCode(c byte) string {
	switch c {
	case 2:
		return fileKindMovie
	case 1:
		return fileKindSeries
	default:
		return fileKindUnknown
	}
}

func stateCode(state string) byte {
	switch state {
	case StateResolved:
		return 1
	case StateNeedsRepair:
		return 2
	default:
		return 0
	}
}

func stateFromCode(c byte) string {
	switch c {
	case 1:
		return StateResolved
	case 2:
		return StateNeedsRepair
	default:
		return StatePending
	}
}

// encodeFileRecord appends the value payload for one file record.
func encodeFileRecord(dst []byte, r FileRecord) []byte {
	dst = append(dst, encFileVersion)
	dst = putString(dst, r.Path)
	dst = putString(dst, r.Name)
	dst = putString(dst, r.Ext)
	dst = binary.AppendUvarint(dst, uint64(r.Size))
	dst = binary.AppendUvarint(dst, uint64(r.Mtime))
	dst = append(dst, kindCode(r.Kind))
	dst = putString(dst, r.Title)
	dst = binary.AppendUvarint(dst, uint64(r.Season))
	dst = binary.AppendUvarint(dst, uint64(r.Episode))
	dst = binary.AppendUvarint(dst, uint64(r.Year))
	dst = putString(dst, r.MetaKey)
	dst = append(dst, stateCode(r.State))
	dst = binary.AppendUvarint(dst, uint64(r.Attempts))
	dst = putString(dst, r.LastError)
	dst = binary.AppendUvarint(dst, uint64(r.LastTry))
	dst = binary.AppendUvarint(dst, uint64(r.NextTry))
	if r.Absolute {
		dst = append(dst, 1)
	} else {
		dst = append(dst, 0)
	}
	return dst
}

// decodeFileRecord decodes one file record; ok is false for a truncated or
// wrong-version payload or a trailing byte (the store's CRC proves the frame,
// not the payload's meaning).
func decodeFileRecord(b []byte) (FileRecord, bool) {
	var r FileRecord
	if len(b) < 1 || b[0] != encFileVersion {
		return r, false
	}
	n := 1
	var ok bool
	if r.Path, n, ok = getString(b, n); !ok {
		return FileRecord{}, false
	}
	if r.Name, n, ok = getString(b, n); !ok {
		return FileRecord{}, false
	}
	if r.Ext, n, ok = getString(b, n); !ok {
		return FileRecord{}, false
	}
	var v uint64
	if v, n, ok = getUvarint(b, n); !ok {
		return FileRecord{}, false
	}
	r.Size = int64(v)
	if v, n, ok = getUvarint(b, n); !ok {
		return FileRecord{}, false
	}
	r.Mtime = int64(v)
	if n >= len(b) {
		return FileRecord{}, false
	}
	r.Kind = kindFromCode(b[n])
	n++
	if r.Title, n, ok = getString(b, n); !ok {
		return FileRecord{}, false
	}
	if v, n, ok = getUvarint(b, n); !ok {
		return FileRecord{}, false
	}
	r.Season = int(v)
	if v, n, ok = getUvarint(b, n); !ok {
		return FileRecord{}, false
	}
	r.Episode = int(v)
	if v, n, ok = getUvarint(b, n); !ok {
		return FileRecord{}, false
	}
	r.Year = int(v)
	if r.MetaKey, n, ok = getString(b, n); !ok {
		return FileRecord{}, false
	}
	if n >= len(b) {
		return FileRecord{}, false
	}
	r.State = stateFromCode(b[n])
	n++
	if v, n, ok = getUvarint(b, n); !ok {
		return FileRecord{}, false
	}
	r.Attempts = int(v)
	if r.LastError, n, ok = getString(b, n); !ok {
		return FileRecord{}, false
	}
	if v, n, ok = getUvarint(b, n); !ok {
		return FileRecord{}, false
	}
	r.LastTry = int64(v)
	if v, n, ok = getUvarint(b, n); !ok {
		return FileRecord{}, false
	}
	r.NextTry = int64(v)
	if n >= len(b) {
		return FileRecord{}, false
	}
	r.Absolute = b[n] == 1
	n++
	if n != len(b) {
		return FileRecord{}, false
	}
	return r, true
}

// encodeFolderRecord appends the value payload for one folder rollup.
func encodeFolderRecord(dst []byte, r FolderRecord) []byte {
	dst = append(dst, encFolderVersion)
	dst = binary.AppendUvarint(dst, uint64(r.Schema))
	dst = binary.AppendUvarint(dst, uint64(r.ScannedAt.UnixNano()))
	dst = binary.AppendUvarint(dst, uint64(r.Files))
	if r.perFile {
		dst = append(dst, 1)
	} else {
		dst = append(dst, 0)
	}
	dst = putString(dst, r.NamesHash)
	dst = putString(dst, r.Fingerprint)
	dst = binary.AppendUvarint(dst, uint64(len(r.Keys)))
	for _, k := range r.Keys {
		dst = putString(dst, k)
	}
	return dst
}

// decodeFolderRecord decodes one folder rollup.
func decodeFolderRecord(b []byte) (FolderRecord, bool) {
	var r FolderRecord
	if len(b) < 1 || b[0] != encFolderVersion {
		return r, false
	}
	n := 1
	var v uint64
	var ok bool
	if v, n, ok = getUvarint(b, n); !ok {
		return FolderRecord{}, false
	}
	r.Schema = int(v)
	if v, n, ok = getUvarint(b, n); !ok {
		return FolderRecord{}, false
	}
	r.ScannedAt = time.Unix(0, int64(v)).UTC()
	if v, n, ok = getUvarint(b, n); !ok {
		return FolderRecord{}, false
	}
	r.Files = int(v)
	if n >= len(b) {
		return FolderRecord{}, false
	}
	r.perFile = b[n] == 1
	n++
	if r.NamesHash, n, ok = getString(b, n); !ok {
		return FolderRecord{}, false
	}
	if r.Fingerprint, n, ok = getString(b, n); !ok {
		return FolderRecord{}, false
	}
	if v, n, ok = getUvarint(b, n); !ok {
		return FolderRecord{}, false
	}
	count := int(v)
	if count < 0 || count > len(b) {
		return FolderRecord{}, false
	}
	if count > 0 {
		r.Keys = make([]string, 0, count)
	}
	for i := 0; i < count; i++ {
		var k string
		if k, n, ok = getString(b, n); !ok {
			return FolderRecord{}, false
		}
		r.Keys = append(r.Keys, k)
	}
	if n != len(b) {
		return FolderRecord{}, false
	}
	return r, true
}

func encodeIndexMeta(dst []byte, m indexMeta) []byte {
	dst = binary.AppendUvarint(dst, uint64(m.Schema))
	dst = binary.AppendUvarint(dst, uint64(m.MigratedAt))
	return binary.AppendUvarint(dst, uint64(m.MigratedFolders))
}

func decodeIndexMeta(b []byte) (indexMeta, bool) {
	var m indexMeta
	var v uint64
	var n int
	var ok bool
	if v, n, ok = getUvarint(b, 0); !ok {
		return m, false
	}
	m.Schema = int(v)
	if v, n, ok = getUvarint(b, n); !ok {
		return m, false
	}
	m.MigratedAt = int64(v)
	if v, n, ok = getUvarint(b, n); !ok {
		return m, false
	}
	m.MigratedFolders = int(v)
	return m, n == len(b)
}
