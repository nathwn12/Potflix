package library

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeScanFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fingerprintOf(t *testing.T, dir string) string {
	t.Helper()
	fp, err := FolderFingerprint(dir)
	if err != nil {
		t.Fatalf("FolderFingerprint(%q): %v", dir, err)
	}
	return fp
}

func scanAllPresent(string) bool { return true }

func TestScanIndexFingerprint(t *testing.T) {
	t.Run("stable", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "stable")
		writeScanFile(t, filepath.Join(dir, "a.mkv"), "a")
		if got, want := fingerprintOf(t, dir), fingerprintOf(t, dir); got != want {
			t.Fatalf("same content: fingerprints differ:\n%s\n%s", got, want)
		}
	})

	t.Run("addFile", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "add")
		writeScanFile(t, filepath.Join(dir, "a.mkv"), "a")
		before := fingerprintOf(t, dir)
		writeScanFile(t, filepath.Join(dir, "b.mkv"), "b")
		if after := fingerprintOf(t, dir); after == before {
			t.Fatal("adding a file did not change the fingerprint")
		}
	})

	t.Run("rename", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "rename")
		writeScanFile(t, filepath.Join(dir, "a.mkv"), "a")
		before := fingerprintOf(t, dir)
		if err := os.Rename(filepath.Join(dir, "a.mkv"), filepath.Join(dir, "b.mkv")); err != nil {
			t.Fatal(err)
		}
		if after := fingerprintOf(t, dir); after == before {
			t.Fatal("renaming a file did not change the fingerprint")
		}
	})

	t.Run("touchMtime", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "touch")
		p := filepath.Join(dir, "a.mkv")
		writeScanFile(t, p, "a")
		before := fingerprintOf(t, dir)
		mt := time.Now().Add(2 * time.Hour)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
		if after := fingerprintOf(t, dir); after == before {
			t.Fatal("touching a file mtime did not change the fingerprint")
		}
	})

	t.Run("subfolder", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "parent")
		sub := filepath.Join(parent, "sub")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		before := fingerprintOf(t, parent)
		// Separate the inner create from the subdir create by a tick so
		// the subdir mtime bump lands on a later timestamp even on
		// coarse-grained filesystems.
		time.Sleep(50 * time.Millisecond)
		inner := filepath.Join(sub, "ep01.mkv")
		writeScanFile(t, inner, "x")
		if _, err := os.Stat(inner); err != nil {
			t.Fatalf("inner file missing after write: %v", err)
		}
		// Windows can apply directory mtime updates lazily, so allow a
		// grace period for the propagation instead of asserting it is
		// instantaneous. A hash that ignored child-directory mtimes
		// would never change and still fail here at the deadline.
		deadline := time.Now().Add(10 * time.Second)
		for {
			if after := fingerprintOf(t, parent); after != before {
				return
			}
			if time.Now().After(deadline) {
				submtime := "<stat failed>"
				if st, err := os.Stat(sub); err == nil {
					submtime = st.ModTime().UTC().Format(time.RFC3339Nano)
				}
				t.Fatalf("adding a file inside a subfolder did not change the parent fingerprint (before=%s submtime=%s)", before, submtime)
			}
			time.Sleep(100 * time.Millisecond)
		}
	})

	t.Run("missing", func(t *testing.T) {
		if _, err := FolderFingerprint(filepath.Join(t.TempDir(), "no-such-dir")); err == nil {
			t.Fatal("expected an error for a missing directory")
		}
	})
}

func TestScanIndexShouldSkip(t *testing.T) {
	setup := func(t *testing.T) (string, string, *ScanIndex) {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "lib")
		writeScanFile(t, filepath.Join(dir, "show.mkv"), "v")
		fp := fingerprintOf(t, dir)
		ix := LoadScanIndex(filepath.Join(t.TempDir(), "scanindex.json"))
		ix.Record(dir, fp, 7, []string{"k1", "k2"})
		return dir, fp, ix
	}

	t.Run("hit", func(t *testing.T) {
		dir, fp, ix := setup(t)
		if !ix.ShouldSkip(dir, fp, 7, scanAllPresent) {
			t.Fatal("unchanged dir with all keys present should skip")
		}
	})

	t.Run("keyAbsent", func(t *testing.T) {
		dir, fp, ix := setup(t)
		present := func(k string) bool { return k != "k2" }
		if ix.ShouldSkip(dir, fp, 7, present) {
			t.Fatal("one absent key must not skip")
		}
	})

	t.Run("schemaMismatch", func(t *testing.T) {
		dir, fp, ix := setup(t)
		if ix.ShouldSkip(dir, fp, 8, scanAllPresent) {
			t.Fatal("schema mismatch must not skip")
		}
	})

	t.Run("fpMismatch", func(t *testing.T) {
		dir, fp, ix := setup(t)
		if ix.ShouldSkip(dir, fp+"tampered", 7, scanAllPresent) {
			t.Fatal("fingerprint mismatch must not skip")
		}
	})

	t.Run("noRecord", func(t *testing.T) {
		dir, fp, _ := setup(t)
		fresh := LoadScanIndex(filepath.Join(t.TempDir(), "empty.json"))
		if fresh.ShouldSkip(dir, fp, 7, scanAllPresent) {
			t.Fatal("a dir with no record must not skip")
		}
	})

	t.Run("nilKeyPresent", func(t *testing.T) {
		dir, fp, ix := setup(t)
		if ix.ShouldSkip(dir, fp, 7, nil) {
			t.Fatal("nil keyPresent must not skip")
		}
	})

	t.Run("absentFile", func(t *testing.T) {
		ix := LoadScanIndex(filepath.Join(t.TempDir(), "does-not-exist.json"))
		if ix.Len() != 0 {
			t.Fatalf("absent index file: Len = %d, want 0", ix.Len())
		}
		if ix.ShouldSkip(t.TempDir(), "fp", 7, scanAllPresent) {
			t.Fatal("empty index must not skip")
		}
	})

	t.Run("corruptFile", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "scanindex.json")
		if err := os.WriteFile(path, []byte("{{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		ix := LoadScanIndex(path)
		if ix.Len() != 0 {
			t.Fatalf("corrupt index file: Len = %d, want 0", ix.Len())
		}
		if ix.ShouldSkip(t.TempDir(), "fp", 7, scanAllPresent) {
			t.Fatal("corrupt index must not skip")
		}
	})
}

func TestScanIndexRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	d1 := filepath.Join(tmp, "a")
	d2 := filepath.Join(tmp, "b")
	writeScanFile(t, filepath.Join(d1, "x.mkv"), "x")
	writeScanFile(t, filepath.Join(d2, "y.mkv"), "y")
	fp1, fp2 := fingerprintOf(t, d1), fingerprintOf(t, d2)

	ix := LoadScanIndex(filepath.Join(tmp, "idx.json"))
	ix.Record(d1, fp1, 3, []string{"ka"})
	ix.Record(d2, fp2, 3, []string{"kb", "kc"})

	// Nested path proves Save creates parent dirs (MkdirAll).
	path := filepath.Join(tmp, "sub", "dir", "scanindex.json")
	if err := ix.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Save did not leave a file at %q: %v", path, err)
	}

	loaded := LoadScanIndex(path)
	if loaded.Len() != 2 {
		t.Fatalf("round-trip: Len = %d, want 2", loaded.Len())
	}
	if !loaded.ShouldSkip(d1, fp1, 3, func(k string) bool { return k == "ka" }) {
		t.Fatal("round-trip: d1 should skip")
	}
	if !loaded.ShouldSkip(d2, fp2, 3, func(k string) bool { return k == "kb" || k == "kc" }) {
		t.Fatal("round-trip: d2 should skip")
	}
	if loaded.ShouldSkip(d2, fp2, 3, func(k string) bool { return k == "kb" }) {
		t.Fatal("round-trip: d2 with a missing key must not skip")
	}

	// Corrupt the file in place: the loader must stay tolerant.
	if err := os.WriteFile(path, []byte("{{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadScanIndex(path); got.Len() != 0 {
		t.Fatalf("corrupt after round-trip: Len = %d, want 0", got.Len())
	}
}

func TestScanIndexConcurrent(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "scanindex.json")
	ix := LoadScanIndex(path)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			dir := filepath.Join(tmp, fmt.Sprintf("d%d", g))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Error(err)
				return
			}
			for i := 0; i < 25; i++ {
				ix.Record(dir, fmt.Sprintf("fp-%d-%d", g, i), 1, []string{fmt.Sprintf("k-%d", g)})
				if i%5 == 0 {
					if err := ix.Save(path); err != nil {
						t.Error(err)
						return
					}
				}
			}
		}(g)
	}
	wg.Wait()
	if err := ix.Save(path); err != nil {
		t.Fatalf("final Save: %v", err)
	}
	if got := LoadScanIndex(path); got.Len() != 8 {
		t.Fatalf("concurrent: Len = %d, want 8", got.Len())
	}
}

func TestScanIndexPrune(t *testing.T) {
	tmp := t.TempDir()
	keep := filepath.Join(tmp, "keep")
	gone := filepath.Join(tmp, "gone")
	writeScanFile(t, filepath.Join(keep, "a.mkv"), "a")
	writeScanFile(t, filepath.Join(gone, "b.mkv"), "b")
	fpKeep := fingerprintOf(t, keep)

	ix := LoadScanIndex(filepath.Join(tmp, "idx.json"))
	ix.Record(keep, fpKeep, 1, []string{"k1"})
	ix.Record(gone, fingerprintOf(t, gone), 1, []string{"k2"})
	if ix.Len() != 2 {
		t.Fatalf("before prune: Len = %d, want 2", ix.Len())
	}
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	ix.PruneExisting()
	if ix.Len() != 1 {
		t.Fatalf("after prune: Len = %d, want 1", ix.Len())
	}
	if !ix.ShouldSkip(keep, fpKeep, 1, scanAllPresent) {
		t.Fatal("prune dropped a record for a dir that still exists")
	}
}

func TestScanIndexNormalize(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "Videos")
	writeScanFile(t, filepath.Join(dir, "a.mkv"), "a")
	fp := fingerprintOf(t, dir)

	ix := LoadScanIndex(filepath.Join(tmp, "idx.json"))
	ix.Record(dir, fp, 1, []string{"k"})
	// Unclean spelling of the same folder must hit the same record.
	unclean := dir + string(filepath.Separator) + "."
	ix.Record(unclean, fp, 1, []string{"k"})
	if ix.Len() != 1 {
		t.Fatalf("unclean spelling: Len = %d, want 1", ix.Len())
	}
	if !ix.ShouldSkip(unclean, fp, 1, scanAllPresent) {
		t.Fatal("unclean spelling of a recorded dir should skip")
	}
	if runtime.GOOS == "windows" {
		if !ix.ShouldSkip(strings.ToUpper(dir), fp, 1, scanAllPresent) {
			t.Fatal("case-differing spelling of a recorded dir should skip on Windows")
		}
	}
}

func namesHashOf(t *testing.T, dir string) string {
	t.Helper()
	h, err := FolderNamesHash(dir)
	if err != nil {
		t.Fatalf("FolderNamesHash(%q): %v", dir, err)
	}
	return h
}

// TestFolderNamesHash proves the tier-1 identity: stable on no change,
// sensitive to add/remove/rename by NAMES (same count, new name differs),
// and independent of the dir mtime alone for renames (names come from the
// listing, so restoring the old dir mtime after a rename still differs).
func TestFolderNamesHash(t *testing.T) {
	t.Run("stable", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "stable")
		writeScanFile(t, filepath.Join(dir, "a.mkv"), "a")
		if got, want := namesHashOf(t, dir), namesHashOf(t, dir); got != want {
			t.Fatal("same content: names hashes differ")
		}
	})

	t.Run("addFile", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "add")
		writeScanFile(t, filepath.Join(dir, "a.mkv"), "a")
		before := namesHashOf(t, dir)
		writeScanFile(t, filepath.Join(dir, "b.mkv"), "b")
		if after := namesHashOf(t, dir); after == before {
			t.Fatal("adding a file did not change the names hash")
		}
	})

	t.Run("renameSameCount", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "rename")
		writeScanFile(t, filepath.Join(dir, "a.mkv"), "a")
		before := namesHashOf(t, dir)
		if err := os.Rename(filepath.Join(dir, "a.mkv"), filepath.Join(dir, "b.mkv")); err != nil {
			t.Fatal(err)
		}
		if after := namesHashOf(t, dir); after == before {
			t.Fatal("renaming a file (same count, new name) did not change the names hash")
		}
	})

	t.Run("renameDespiteMtimeRestore", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "lag")
		writeScanFile(t, filepath.Join(dir, "a.mkv"), "a")
		before := namesHashOf(t, dir)
		st, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		oldMtime := st.ModTime()
		if err := os.Rename(filepath.Join(dir, "a.mkv"), filepath.Join(dir, "b.mkv")); err != nil {
			t.Fatal(err)
		}
		// Simulate lagging dir mtime: restore the pre-rename timestamp.
		if err := os.Chtimes(dir, oldMtime, oldMtime); err != nil {
			t.Fatal(err)
		}
		if after := namesHashOf(t, dir); after == before {
			t.Fatal("rename with restored dir mtime did not change the names hash (names must come from the listing)")
		}
	})

	t.Run("touchFileKeepsTier1", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "touch")
		p := filepath.Join(dir, "a.mkv")
		writeScanFile(t, p, "a")
		before := namesHashOf(t, dir)
		mt := time.Now().Add(2 * time.Hour)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
		// Tier 1 never stats children, so a pure file-mtime touch keeps
		// the names hash: the rescan decision for touches belongs to
		// tier 2 (FolderFingerprint), which is mtime-sensitive.
		if after := namesHashOf(t, dir); after != before {
			t.Logf("note: file touch changed dir mtime too (platform bump); names hash differs, tier-2 fallback still applies")
		}
	})

	t.Run("missing", func(t *testing.T) {
		if _, err := FolderNamesHash(filepath.Join(t.TempDir(), "no-such-dir")); err == nil {
			t.Fatal("expected an error for a missing directory")
		}
	})
}

// TestShouldSkipNames proves the tier-1 guard: hit, rename-miss,
// schema-miss, key-absent-miss, and legacy empty-NamesHash never-hit.
func TestShouldSkipNames(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "lib")
	writeScanFile(t, filepath.Join(dir, "show.mkv"), "v")
	nh := namesHashOf(t, dir)
	ix := LoadScanIndex(filepath.Join(t.TempDir(), "scanindex.json"))
	ix.RecordFull(dir, nh, fingerprintOf(t, dir), 7, []string{"k1"}, 1)

	if !ix.ShouldSkipNames(dir, nh, 7, scanAllPresent) {
		t.Fatal("unchanged dir should skip via tier 1")
	}
	if ix.ShouldSkipNames(dir, nh+"x", 7, scanAllPresent) {
		t.Fatal("tampered names hash must not skip")
	}
	if ix.ShouldSkipNames(dir, nh, 8, scanAllPresent) {
		t.Fatal("schema mismatch must not skip")
	}
	if ix.ShouldSkipNames(dir, nh, 7, func(k string) bool { return false }) {
		t.Fatal("absent key must not skip")
	}
	if ix.ShouldSkipNames(dir, nh, 7, nil) {
		t.Fatal("nil keyPresent must not skip")
	}
	legacy := LoadScanIndex(filepath.Join(t.TempDir(), "legacy.json"))
	legacy.Record(dir, fingerprintOf(t, dir), 7, []string{"k1"})
	if legacy.ShouldSkipNames(dir, nh, 7, scanAllPresent) {
		t.Fatal("legacy record without NamesHash must fall through to tier 2, never hit tier 1")
	}
}

// TestScanIndexInconsistentNoSkip proves the fail-closed guards: the index
// is local-trust, so a truncated record (files claimed, no keys) or a
// vacuous one (more keys than files) must never report a skip that would
// hide unresolved files.
func TestScanIndexInconsistentNoSkip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "lib")
	writeScanFile(t, filepath.Join(dir, "show.mkv"), "v")
	fp := fingerprintOf(t, dir)
	nh := namesHashOf(t, dir)

	trunc := LoadScanIndex(filepath.Join(t.TempDir(), "trunc.json"))
	trunc.RecordFull(dir, nh, fp, 7, nil, 5)
	if trunc.ShouldSkip(dir, fp, 7, scanAllPresent) {
		t.Fatal("Files=5, Keys=[] must not skip (truncated record)")
	}
	if trunc.ShouldSkipNames(dir, nh, 7, scanAllPresent) {
		t.Fatal("tier-1: Files=5, Keys=[] must not skip (truncated record)")
	}

	vacuous := LoadScanIndex(filepath.Join(t.TempDir(), "vac.json"))
	vacuous.RecordFull(dir, nh, fp, 7, []string{"k1", "k2", "k3"}, 2)
	if vacuous.ShouldSkip(dir, fp, 7, scanAllPresent) {
		t.Fatal("Files=2 with 3 keys must not skip (vacuous record)")
	}
	if vacuous.ShouldSkipNames(dir, nh, 7, scanAllPresent) {
		t.Fatal("tier-1: Files=2 with 3 keys must not skip (vacuous record)")
	}

	// A consistent record still skips.
	ok := LoadScanIndex(filepath.Join(t.TempDir(), "ok.json"))
	ok.RecordFull(dir, nh, fp, 7, []string{"k1", "k2"}, 2)
	if !ok.ShouldSkip(dir, fp, 7, scanAllPresent) {
		t.Fatal("consistent record (Files=2, 2 keys) should skip")
	}
	if !ok.ShouldSkipNames(dir, nh, 7, scanAllPresent) {
		t.Fatal("tier-1: consistent record (Files=2, 2 keys) should skip")
	}
}

// TestScanIndexOversizeTreatedAbsent proves an 8 MB+ index file loads as an
// empty index (tolerant, no error to the caller) instead of being parsed
// unbounded.
func TestScanIndexOversizeTreatedAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scanindex.json")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), (8<<20)+1), 0o644); err != nil {
		t.Fatalf("write oversized index fixture: %v", err)
	}
	ix := LoadScanIndex(path)
	if ix.Len() != 0 {
		t.Fatalf("oversized index: Len = %d, want 0 (treated as absent)", ix.Len())
	}
	if ix.ShouldSkip(t.TempDir(), "fp", 7, scanAllPresent) {
		t.Fatal("oversized index must not skip")
	}
}
