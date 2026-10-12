// Tests for the store-backed index (index.go) and the scan's enumerate+diff
// phase (scan.go): migration parity against the owner's real scanindex.json,
// the unchanged-file fast path, prune, the encoding budget, and survival of a
// torn tail.
package library

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"potflix/internal/metadata"
)

// realIndexSource is the owner's live legacy index, if this machine has one.
func realIndexSource(t *testing.T) string {
	t.Helper()
	d, err := os.UserCacheDir()
	if err != nil || d == "" {
		return ""
	}
	return filepath.Join(d, "Potflix", "scanindex.json")
}

// copyRealIndex copies the live scanindex.json into a fresh temp dir and
// returns the copy's path. The live file is never opened for writing, never
// renamed, never deleted: migration parity must not cost the owner his index.
func copyRealIndex(t *testing.T) string {
	t.Helper()
	src := realIndexSource(t)
	if src == "" {
		t.Skip("no user cache dir on this machine")
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Skipf("no live legacy index to copy: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "scanindex.json")
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("copy legacy index: %v", err)
	}
	return dst
}

// TestMigrationParityRealIndex proves the migration reads the owner's real
// index exactly: 928 folder records claiming 23,402 files, imported into the
// store, the JSON renamed to .bak, and the folder count intact after a
// reopen.
//
// Key parity note: the retired file stores the per-folder DEDUPED metadata
// cache keys (metadata.CacheKeyFor), so the surviving totals are 2,310
// distinct keys over 7,218 per-folder references. The brief's "7,378 keys" is
// the per-file EPISODE-key count a full walk reports (Stats.Keys, deduped by
// DedupeKey over 23,402 files) and is not stored in scanindex.json - see the
// return's unverified note.
func TestMigrationParityRealIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("reads the owner's real index")
	}
	copyPath := copyRealIndex(t)
	live := realIndexSource(t)

	counts, err := LegacyScanIndexCounts(copyPath)
	if err != nil {
		t.Fatalf("LegacyScanIndexCounts: %v", err)
	}
	t.Logf("legacy index: %d folders, %d files, %d distinct keys, %d key references",
		counts.Folders, counts.Files, counts.Keys, counts.KeyRefs)
	if counts.Folders != 928 || counts.Files != 23402 {
		t.Errorf("legacy parity = %d folders / %d files, want 928 / 23402", counts.Folders, counts.Files)
	}
	if counts.Keys != 2310 || counts.KeyRefs != 7218 {
		t.Errorf("legacy keys = %d distinct / %d references, want 2310 / 7218", counts.Keys, counts.KeyRefs)
	}

	// Migration: first open of an empty store beside the copy imports the
	// JSON and renames it aside.
	start := time.Now()
	ix := LoadScanIndex(copyPath)
	load := time.Since(start)
	t.Logf("store load (migrated, %d folder records): %v", ix.Len(), load)
	if got := ix.Len(); got != counts.Folders {
		t.Errorf("indexed folders = %d, want %d", got, counts.Folders)
	}
	if _, err := os.Stat(copyPath + legacyBakSuffix); err != nil {
		t.Errorf("legacy file was not renamed to %s: %v", legacyBakSuffix, err)
	}
	if _, err := os.Stat(copyPath); !os.IsNotExist(err) {
		t.Errorf("legacy file still present at %q after migration (err=%v)", copyPath, err)
	}
	// The migrated rollups carry no per-file data, so they must not skip:
	// the next scan enumerates their files and creates the records.
	for _, dir := range ix.ix.folderNames() {
		rec, _ := ix.Lookup(dir)
		if rec.perFile {
			t.Fatalf("migrated rollup %q claims per-file data (the legacy file has none)", dir)
		}
	}
	// Reopen from disk: the count must survive a full store reload.
	if err := CloseIndex(copyPath); err != nil {
		t.Fatalf("close index: %v", err)
	}
	reopened := LoadScanIndex(copyPath)
	if got := reopened.Len(); got != counts.Folders {
		t.Errorf("reopened folders = %d, want %d", got, counts.Folders)
	}
	if live == "" {
		return
	}
	// The live file must be exactly as we found it.
	if _, err := os.Stat(live); err != nil {
		t.Errorf("live legacy index was disturbed: %v", err)
	}
}

// TestMigrationImportsZeroAndRenames proves a corrupt legacy file is parked as
// .bak with zero imported records (never deleted, never an error).
func TestMigrationImportsZeroAndRenames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scanindex.json")
	if err := os.WriteFile(path, []byte("{{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	ix := LoadScanIndex(path)
	if ix.Len() != 0 {
		t.Fatalf("corrupt legacy import: %d folders, want 0", ix.Len())
	}
	if _, err := os.Stat(path + legacyBakSuffix); err != nil {
		t.Errorf("corrupt legacy file was not parked as .bak: %v", err)
	}
}

// TestUnchangedFileFastPath proves the second scan over the same tree
// re-parses NOTHING: the folders are forced to re-walk (their mtimes move, so
// no folder record matches), every file is enumerated and diffed, and the
// parser sees zero calls.
func TestUnchangedFileFastPath(t *testing.T) {
	dirs, parent := seedIndexCorpus(t)
	cache := metaCacheDir(t)
	index := filepath.Join(t.TempDir(), "scanindex.json")
	ctx := context.Background()

	restore := withFakeResolver("V1", nil)
	defer restore()

	if _, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, false)); err != nil {
		t.Fatalf("scan 1: %v", err)
	}

	// Count parses from here on.
	origParse := parseQuery
	defer func() { parseQuery = origParse }()
	parses := 0
	parseQuery = func(rawTitle, path string) metadata.Query {
		parses++
		return origParse(rawTitle, path)
	}

	// Move every corpus folder's mtime so no folder record can match: the
	// walk visits each folder and diffs each file.
	later := time.Now().Add(3 * time.Second)
	for _, d := range append([]string{parent}, dirs...) {
		if err := os.Chtimes(d, later, later); err != nil {
			t.Fatalf("touch %q: %v", d, err)
		}
	}
	st2, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, false))
	if err != nil {
		t.Fatalf("scan 2: %v", err)
	}
	if parses != 0 {
		t.Errorf("scan 2 re-parsed %d files, want 0 (unchanged files keep their record)", parses)
	}
	if st2.Parsed != 0 {
		t.Errorf("scan 2 Parsed = %d, want 0", st2.Parsed)
	}
	if st2.FoldersSkipped != 0 {
		t.Fatalf("scan 2 skipped %d folders, want 0 (mtimes moved)", st2.FoldersSkipped)
	}
	if st2.Files == 0 || st2.Unchanged != st2.Files {
		t.Errorf("scan 2 = %+v, want every walked file counted Unchanged", st2)
	}
}

// TestPruneDeletedFileDisappearsFromStore proves a deleted file's record is
// deleted from the store (and survives a reopen as absent).
func TestPruneDeletedFileDisappearsFromStore(t *testing.T) {
	dirs, _ := seedIndexCorpus(t)
	cache := metaCacheDir(t)
	index := filepath.Join(t.TempDir(), "scanindex.json")
	ctx := context.Background()

	restore := withFakeResolver("V1", nil)
	defer restore()
	if _, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, false)); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	gone := filepath.Join(dirs[0], "ShowA.S01E02.mkv")
	if _, ok := LoadScanIndex(index).ix.file(gone); !ok {
		t.Fatalf("no record for %q after scan 1", gone)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	st2, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, false))
	if err != nil {
		t.Fatalf("scan 2: %v", err)
	}
	if st2.Pruned != 1 {
		t.Errorf("scan 2 Pruned = %d, want 1", st2.Pruned)
	}
	if _, ok := LoadScanIndex(index).ix.file(gone); ok {
		t.Errorf("deleted file %q still has a record in memory", gone)
	}
	if err := CloseIndex(index); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, ok := LoadScanIndex(index).ix.file(gone); ok {
		t.Errorf("deleted file %q came back after a store reopen", gone)
	}
}

// TestTornTailKeepsIndexReadable proves this layer survives a hard kill
// mid-write: the store truncates the torn tail on the next open and every
// earlier record - folders and files - is still there.
func TestTornTailKeepsIndexReadable(t *testing.T) {
	dirs, _ := seedIndexCorpus(t)
	cache := metaCacheDir(t)
	index := filepath.Join(t.TempDir(), "scanindex.json")
	ctx := context.Background()

	restore := withFakeResolver("V1", nil)
	defer restore()
	if _, err := ScanWithOptions(ctx, indexScanOpts(dirs, cache, index, false)); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	before := LoadScanIndex(index).State()
	if before.Files == 0 || before.Keys == 0 {
		t.Fatalf("nothing indexed to survive: %+v", before)
	}
	sp := IndexStorePath(index)
	if err := CloseIndex(index); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A kill mid-frame: a put frame header claiming a payload that never
	// landed, plus a partial payload.
	f, err := os.OpenFile(sp, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{1, 60, 0, 0, 0, 0xde, 0xad, 0xbe, 0xef, 'h', 'a', 'l'}); err != nil {
		t.Fatal(err)
	}
	f.Close()
	torn, err := os.Stat(sp)
	if err != nil {
		t.Fatal(err)
	}

	after := LoadScanIndex(index).State()
	if after.Files != before.Files || after.Keys != before.Keys {
		t.Errorf("torn tail lost records: before %+v, after %+v", before, after)
	}
	if rec, ok := LoadScanIndex(index).Lookup(dirs[0]); !ok || rec.Files == 0 {
		t.Errorf("folder rollups lost after a torn tail: %+v (ok=%t)", rec, ok)
	}
	// The store truncated the invalid tail back to the last good frame.
	trimmed, err := os.Stat(sp)
	if err != nil {
		t.Fatal(err)
	}
	if trimmed.Size() >= torn.Size() {
		t.Errorf("torn tail was not trimmed: size %d, want < %d", trimmed.Size(), torn.Size())
	}
}

// TestFileRecordQueryRoundTrip proves the stored query fields reproduce the
// record's own metadata key: the catch-up rebuilds its queries from them, so
// a mismatch would look up the wrong key.
func TestFileRecordQueryRoundTrip(t *testing.T) {
	files := []string{
		"ShowA.S01E01.mkv", "ShowA.S01E02.mkv", "Movie.Title.1999.mkv",
		"[GRP] Anime Show 12 [1080p].mkv", "ShowCtrailer.mkv",
	}
	for _, name := range files {
		p := filepath.Join(`D:\lib`, name)
		q := metadata.ResolveQuery(name, p)
		rec := FileRecord{
			Path: p, Name: name, Kind: recordKind(q.Kind), Title: q.Title,
			Season: q.Season, Episode: q.Episode, Year: q.Year, Absolute: q.Absolute,
		}
		if q.Kind != metadata.KindUnknown {
			rec.MetaKey = metadata.CacheKeyFor(q)
			if got := metadata.CacheKeyFor(rec.Query()); got != rec.MetaKey {
				t.Errorf("%s: rebuilt key %q, want %q (query %+v -> %+v)", name, got, rec.MetaKey, q, rec.Query())
			}
		}
	}
}

// TestFileRecordEncodingBudget pins the encoding: compact enough that 23,402
// records fit comfortably in one file, and exactly round-tripped. The measured
// number is logged next to the JSON equivalent, because "compact" is a claim
// that needs a number behind it.
func TestFileRecordEncodingBudget(t *testing.T) {
	rec := FileRecord{
		Path: `D:\Media\Series\RealShapeShow123\Season 02\RealShapeShow123.S02E07.1080p.WEB-DL.x264-GRP.mkv`,
		Name: "RealShapeShow123.S02E07.1080p.WEB-DL.x264-GRP.mkv",
		Ext:  ".mkv", Size: 1 << 30, Mtime: 1750000000000000000,
		Kind: fileKindSeries, Title: "RealShapeShow123", Season: 2, Episode: 7,
		MetaKey: "series|realshapeshow123|0|2", State: StateResolved,
	}
	enc := encodeFileRecord(nil, rec)
	js, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("file record: %d bytes binary vs %d bytes JSON (path len %d)", len(enc), len(js), len(rec.Path))
	if len(enc) >= len(js) {
		t.Errorf("binary record = %d bytes, JSON = %d; want the binary encoding smaller", len(enc), len(js))
	}
	// 23,402 records at this size stay a few megabytes in one file.
	if got, max := len(enc)*23402, 6<<20; got > max {
		t.Errorf("23,402 records would occupy %d bytes, want <= %d", got, max)
	}
	got, ok := decodeFileRecord(enc)
	if !ok {
		t.Fatalf("decode failed")
	}
	if got != rec {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, rec)
	}
	// A truncated or trailing-byte payload is refused, never misread.
	if _, ok := decodeFileRecord(enc[:len(enc)-1]); ok {
		t.Error("truncated payload decoded")
	}
	if _, ok := decodeFileRecord(append(append([]byte(nil), enc...), 0)); ok {
		t.Error("trailing byte accepted")
	}
}

// TestFolderRecordEncodingRoundTrip pins the rollup round trip including the
// per-file flag the migration depends on.
func TestFolderRecordEncodingRoundTrip(t *testing.T) {
	rec := FolderRecord{
		Fingerprint: "fp", NamesHash: "nh", Files: 3, Schema: 3,
		Keys: []string{"series|a|0|1", "movie|b|1999"}, ScannedAt: time.Unix(1700000000, 0).UTC(),
		perFile: true,
	}
	enc := encodeFolderRecord(nil, rec)
	got, ok := decodeFolderRecord(enc)
	if !ok {
		t.Fatalf("decode failed")
	}
	if !sameScanRecord(rec, got) || got.perFile != rec.perFile || !got.ScannedAt.Equal(rec.ScannedAt) {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, rec)
	}
}

// TestIndexStoreLoadTime reports the load cost of a full-size store: 23,402
// file records (the owner's file count) over 2,310 keys (the owner's key
// count), written once and then reopened. The numbers are logged, not
// asserted (they are measurements, not gates).
func TestIndexStoreLoadTime(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 23,402-record store")
	}
	const (
		files = 23402
		keys  = 2310
	)
	tmp := t.TempDir()
	index := filepath.Join(tmp, "scanindex.json")
	ix, err := sharedIndex(index)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	build := time.Now()
	for i := 0; i < files; i++ {
		k := i % keys
		title := fmt.Sprintf("RealShapeShow%04d", k)
		base := fmt.Sprintf("%s.S%02dE%02d.1080p.WEB-DL.x264-GRP%05d.mkv", title, k%5+1, i%24+1, i)
		dir := filepath.Join(`D:\Media\Series`, fmt.Sprintf("Show%04d", k), fmt.Sprintf("Season %02d", k%5+1))
		ix.putFile(FileRecord{
			Path: filepath.Join(dir, base), Name: base,
			Ext: ".mkv", Size: int64(i*1024 + 1), Mtime: int64(i) * 1000000,
			Kind: fileKindSeries, Title: title, Season: k%5 + 1, Episode: i%24 + 1,
			MetaKey: fmt.Sprintf("series|realshapeshow%04d|0|%d", k, k%5+1), State: StatePending,
		})
	}
	if err := ix.commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	built := time.Since(build)
	if err := CloseIndex(index); err != nil {
		t.Fatalf("close: %v", err)
	}
	if fi, err := os.Stat(IndexStorePath(index)); err == nil {
		t.Logf("store file: %d bytes for %d records", fi.Size(), files)
	}
	start := time.Now()
	reopened, err := sharedIndex(index)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	load := time.Since(start)
	state := reopened.state()
	t.Logf("23,402-record store: build+write %v, reopen+decode %v (%d files, %d keys)",
		built, load, state.Files, state.Keys)
	if state.Files != files {
		t.Errorf("reopened Files = %d, want %d", state.Files, files)
	}
	if state.Keys != keys {
		t.Errorf("reopened Keys = %d, want %d", state.Keys, keys)
	}
}
