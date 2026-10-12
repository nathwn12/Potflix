package store

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func testKey(i int) string   { return fmt.Sprintf("key-%06d", i) }
func testValue(i int) []byte { return []byte(fmt.Sprintf("value-%04d-payload!!", i)) }

// openTest opens a store in a fresh temp dir and closes it at test end.
func openTest(t *testing.T, name string, opts Options) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	db, err := Open(path, opts)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

// frameEnds parses path's frames from the locked format and returns the file
// offset after each complete frame. It is deliberately a second, independent
// reading of the format, so a bug in replay does not also become the test's
// expectation.
func frameEnds(t *testing.T, path string) []int64 {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(raw) < headerSize {
		t.Fatalf("%s: %d bytes is shorter than the header", path, len(raw))
	}
	var ends []int64
	off := headerSize
	for off < len(raw) {
		if raw[off] != tagPut && raw[off] != tagDelete {
			t.Fatalf("%s: unknown tag %d at %d", path, raw[off], off)
		}
		off++
		if off+8 > len(raw) {
			break
		}
		payloadLen := int(binary.LittleEndian.Uint32(raw[off : off+4]))
		want := binary.LittleEndian.Uint32(raw[off+4 : off+8])
		off += 8
		if off+payloadLen > len(raw) {
			break
		}
		if crc32.Checksum(raw[off:off+payloadLen], crc32.IEEETable) != want {
			break
		}
		off += payloadLen
		ends = append(ends, int64(off))
	}
	return ends
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}

func dump(t *testing.T, db *DB) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte)
	if err := db.ForEach(func(k string, v []byte) bool {
		out[k] = append([]byte{}, v...) // non-nil, so an empty value compares equal
		return true
	}); err != nil {
		t.Fatalf("ForEach: %v", err)
	}
	return out
}

func tempLeftovers(t *testing.T, dir string) []string {
	t.Helper()
	got, err := filepath.Glob(filepath.Join(dir, "*.compact-*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	return got
}

// 1. round trip, including an empty value and binary keys/values.
func TestRoundTrip(t *testing.T) {
	db, _ := openTest(t, "index.pflx", Options{})

	recs := map[string][]byte{
		"plain-key":           []byte(`{"a":1}`),
		"empty-value":         {},
		"":                    []byte("the empty key is byte-exact too"),
		"binary-\x00\xff\xfe": {0x00, 0x01, 0xff, 0xfe, 0x7f, 0x00},
		"unicode-🎬/season-2":  []byte("value\nwith\ttabs"),
		"longish-" + string(bytes.Repeat([]byte("k"), 300)): bytes.Repeat([]byte("v"), 4096),
	}
	for k, v := range recs {
		if err := db.Put(k, v); err != nil {
			t.Fatalf("Put(%q): %v", k, err)
		}
	}
	if got := db.Len(); got != len(recs) {
		t.Fatalf("Len = %d, want %d", got, len(recs))
	}
	for k, want := range recs {
		got, ok := db.Get(k)
		if !ok {
			t.Fatalf("Get(%q): missing", k)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("Get(%q) = %q, want %q", k, got, want)
		}
	}
	if _, ok := db.Get("never-written"); ok {
		t.Fatal("Get(never-written) reported a value")
	}
	if !reflect.DeepEqual(dump(t, db), recs) {
		t.Fatal("ForEach did not reproduce the written map")
	}

	// overwrite and delete
	if err := db.Put("plain-key", []byte("second")); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if got, _ := db.Get("plain-key"); string(got) != "second" {
		t.Fatalf("overwrite: got %q", got)
	}
	if err := db.Delete("plain-key"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := db.Get("plain-key"); ok {
		t.Fatal("deleted key still readable")
	}
	if err := db.Delete("plain-key"); err != nil {
		t.Fatalf("Delete of an absent key: %v", err)
	}
	if got := db.Len(); got != len(recs)-1 {
		t.Fatalf("Len after delete = %d, want %d", got, len(recs)-1)
	}

	// ForEach stops on false
	seen := 0
	if err := db.ForEach(func(string, []byte) bool { seen++; return false }); err != nil {
		t.Fatalf("ForEach: %v", err)
	}
	if seen != 1 {
		t.Fatalf("ForEach visited %d records after returning false, want 1", seen)
	}

	if err := db.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := db.Put("x", []byte("y")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Put after Close = %v, want ErrClosed", err)
	}
	if err := db.Delete("x"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Delete after Close = %v, want ErrClosed", err)
	}
	if err := db.Commit(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Commit after Close = %v, want ErrClosed", err)
	}
	if err := db.Compact(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Compact after Close = %v, want ErrClosed", err)
	}
	if err := db.ForEach(func(string, []byte) bool { return true }); !errors.Is(err, ErrClosed) {
		t.Fatalf("ForEach after Close = %v, want ErrClosed", err)
	}
	if _, ok := db.Get("plain-key"); ok {
		t.Fatal("Get after Close reported a value")
	}
	if db.Len() != len(recs)-1 {
		t.Fatalf("Len after Close = %d, want %d", db.Len(), len(recs)-1)
	}
}

// 2. reopen after Close: the snapshot plus the delta replay to the same state.
func TestReopenSnapshotAndDelta(t *testing.T) {
	db, path := openTest(t, "index.pflx", Options{})

	want := make(map[string][]byte)
	for i := 0; i < 300; i++ {
		if err := db.Put(testKey(i), testValue(i)); err != nil {
			t.Fatalf("Put: %v", err)
		}
		want[testKey(i)] = testValue(i)
	}
	if err := db.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	before := db.Stats() // snapshot boundary, then a delta on top of it
	if err := db.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if db.Stats().Compactions != before.Compactions+1 {
		t.Fatalf("Compactions = %d, want %d", db.Stats().Compactions, before.Compactions+1)
	}
	for i := 300; i < 400; i++ {
		if err := db.Put(testKey(i), testValue(i)); err != nil {
			t.Fatalf("Put: %v", err)
		}
		want[testKey(i)] = testValue(i)
	}
	for i := 100; i < 120; i++ {
		if err := db.Delete(testKey(i)); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		delete(want, testKey(i))
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if got := dump(t, reopened); !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened state differs: %d records, want %d", len(got), len(want))
	}
	st := reopened.Stats()
	if st.Records != int64(len(want)) {
		t.Fatalf("Records = %d, want %d", st.Records, len(want))
	}
	if st.DeltaBytes != 0 {
		t.Fatalf("DeltaBytes after open = %d, want 0", st.DeltaBytes)
	}
	if st.SnapshotBytes != fileSize(t, path) {
		t.Fatalf("SnapshotBytes = %d, want the file size %d", st.SnapshotBytes, fileSize(t, path))
	}
	if st.Compactions != 0 {
		t.Fatalf("Compactions after open = %d, want 0", st.Compactions)
	}

	// A write on the reopened store survives its own reopen.
	if err := reopened.Put("late", []byte("entry")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := reopened.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	third, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("second reopen: %v", err)
	}
	defer third.Close()
	if got, ok := third.Get("late"); !ok || string(got) != "entry" {
		t.Fatalf("Get(late) = %q, %v", got, ok)
	}
	if third.Len() != len(want)+1 {
		t.Fatalf("Len = %d, want %d", third.Len(), len(want)+1)
	}
}

// 3. torn tail: a truncated file opens without error, keeps every complete
// frame, drops the torn one, and is repaired in place.
func TestTornTailRepair(t *testing.T) {
	const records = 200
	for _, cut := range []int64{1, 5, 25} {
		t.Run(fmt.Sprintf("cut-%d-bytes", cut), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "index.pflx")
			db, err := Open(path, Options{})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			for i := 0; i < records; i++ {
				if err := db.Put(testKey(i), testValue(i)); err != nil {
					t.Fatalf("Put: %v", err)
				}
			}
			if err := db.Commit(); err != nil {
				t.Fatalf("Commit: %v", err)
			}
			if err := db.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			ends := frameEnds(t, path)
			if len(ends) != records {
				t.Fatalf("parsed %d frames, want %d", len(ends), records)
			}
			size := fileSize(t, path)
			if err := os.Truncate(path, size-cut); err != nil {
				t.Fatalf("truncate: %v", err)
			}

			repaired, err := Open(path, Options{})
			if err != nil {
				t.Fatalf("Open after truncation: %v", err)
			}
			defer repaired.Close()

			full := 0
			for _, end := range ends {
				if end <= size-cut {
					full++
				}
			}
			if full != records-1 {
				t.Fatalf("%d frames were complete, want %d", full, records-1)
			}
			if got := repaired.Len(); got != full {
				t.Fatalf("Len = %d, want %d", got, full)
			}
			for i := 0; i < full; i++ {
				got, ok := repaired.Get(testKey(i))
				if !ok || !bytes.Equal(got, testValue(i)) {
					t.Fatalf("Get(%s) = %q, %v; want the written value", testKey(i), got, ok)
				}
			}
			if _, ok := repaired.Get(testKey(records - 1)); ok {
				t.Fatal("the torn frame's record survived")
			}
			if got := fileSize(t, path); got != ends[full-1] {
				t.Fatalf("file size after repair = %d, want the last valid boundary %d", got, ends[full-1])
			}

			// The repaired store keeps working.
			if err := repaired.Put("after-repair", []byte("ok")); err != nil {
				t.Fatalf("Put after repair: %v", err)
			}
			if err := repaired.Commit(); err != nil {
				t.Fatalf("Commit after repair: %v", err)
			}
			if err := repaired.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			again, err := Open(path, Options{})
			if err != nil {
				t.Fatalf("reopen after repair: %v", err)
			}
			defer again.Close()
			if got, ok := again.Get("after-repair"); !ok || string(got) != "ok" {
				t.Fatalf("Get(after-repair) = %q, %v", got, ok)
			}
			if again.Len() != full+1 {
				t.Fatalf("Len = %d, want %d", again.Len(), full+1)
			}
		})
	}
}

// 4. corruption: one flipped payload byte drops that frame and everything
// after it, keeps everything before, and is not an error.
func TestCorruptFrameDropsRest(t *testing.T) {
	const records = 20
	const damaged = 4 // zero-based frame index

	path := filepath.Join(t.TempDir(), "index.pflx")
	db, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < records; i++ {
		if err := db.Put(testKey(i), testValue(i)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := db.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ends := frameEnds(t, path)
	if len(ends) != records {
		t.Fatalf("parsed %d frames, want %d", len(ends), records)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	payloadStart := headerSize
	if damaged > 0 {
		payloadStart = int(ends[damaged-1])
	}
	payloadStart += 9 // frame header: tag + payloadLen + crc
	raw[payloadStart] ^= 0x40
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	damaged4, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open after corruption: %v", err)
	}
	defer damaged4.Close()
	if got := damaged4.Len(); got != damaged {
		t.Fatalf("Len = %d, want %d (the damaged frame and everything after it)", got, damaged)
	}
	for i := 0; i < damaged; i++ {
		if got, ok := damaged4.Get(testKey(i)); !ok || !bytes.Equal(got, testValue(i)) {
			t.Fatalf("Get(%s) = %q, %v; want the written value", testKey(i), got, ok)
		}
	}
	if _, ok := damaged4.Get(testKey(damaged)); ok {
		t.Fatal("the damaged frame survived")
	}
	if got := fileSize(t, path); got != ends[damaged-1] {
		t.Fatalf("file size after repair = %d, want %d", got, ends[damaged-1])
	}
}

// 5. compaction: automatic and explicit.
func TestCompaction(t *testing.T) {
	t.Run("automatic", func(t *testing.T) {
		const threshold = int64(16 << 10)
		const records = 5000
		dir := t.TempDir()
		path := filepath.Join(dir, "index.pflx")
		db, err := Open(path, Options{CompactThreshold: threshold})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer db.Close()

		for i := 0; i < records; i++ {
			if err := db.Put(testKey(i), testValue(i)); err != nil {
				t.Fatalf("Put: %v", err)
			}
		}
		st := db.Stats()
		if st.Compactions == 0 {
			t.Fatalf("Compactions = 0 with a %d-byte delta budget, want > 0", threshold)
		}
		if st.DeltaBytes >= threshold {
			t.Fatalf("DeltaBytes = %d, want below the %d-byte budget", st.DeltaBytes, threshold)
		}
		if got := db.Len(); got != records {
			t.Fatalf("Len = %d, want %d", got, records)
		}
		for i := 0; i < records; i++ {
			if got, ok := db.Get(testKey(i)); !ok || !bytes.Equal(got, testValue(i)) {
				t.Fatalf("Get(%s) missing or wrong after %d compactions", testKey(i), st.Compactions)
			}
		}
		if leftovers := tempLeftovers(t, dir); len(leftovers) != 0 {
			t.Fatalf("leftover temp files: %v", leftovers)
		}
		if err := db.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		reopened, err := Open(path, Options{})
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer reopened.Close()
		if got := reopened.Len(); got != records {
			t.Fatalf("Len after reopen = %d, want %d", got, records)
		}
		if got, ok := reopened.Get(testKey(records - 1)); !ok || !bytes.Equal(got, testValue(records-1)) {
			t.Fatalf("Get(%s) after reopen = %q, %v", testKey(records-1), got, ok)
		}
	})

	t.Run("snapshot-smaller-than-delta", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "index.pflx")
		// A budget no test run reaches, so the delta is the whole file.
		db, err := Open(path, Options{CompactThreshold: 1 << 62})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer db.Close()

		const churn = 5000
		const live = 400 // every key is rewritten ~12 times
		want := make(map[string][]byte, live)
		for i := 0; i < churn; i++ {
			k := testKey(i % live)
			v := []byte(fmt.Sprintf("value-%06d-rewritten-payload", i))
			if err := db.Put(k, v); err != nil {
				t.Fatalf("Put: %v", err)
			}
			want[k] = v
		}
		if err := db.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		st := db.Stats()
		before := fileSize(t, path)
		if before != st.DeltaBytes+headerSize {
			t.Fatalf("file size %d != header+delta %d on an uncompacted store", before, st.DeltaBytes+headerSize)
		}

		if err := db.Compact(); err != nil {
			t.Fatalf("Compact: %v", err)
		}
		after := fileSize(t, path)
		if after >= st.DeltaBytes {
			t.Fatalf("compacted file is %d bytes, not smaller than the pre-compaction delta of %d bytes", after, st.DeltaBytes)
		}
		st2 := db.Stats()
		if st2.DeltaBytes != 0 {
			t.Fatalf("DeltaBytes after Compact = %d, want 0", st2.DeltaBytes)
		}
		if st2.Compactions != 1 {
			t.Fatalf("Compactions = %d, want 1", st2.Compactions)
		}
		if st2.SnapshotBytes != after {
			t.Fatalf("SnapshotBytes = %d, want the new file size %d", st2.SnapshotBytes, after)
		}
		if got := dump(t, db); !reflect.DeepEqual(got, want) {
			t.Fatalf("live keys differ after compaction: %d records, want %d", len(got), len(want))
		}
		if leftovers := tempLeftovers(t, dir); len(leftovers) != 0 {
			t.Fatalf("leftover temp files: %v", leftovers)
		}
	})
}

// 6. large values, and MaxValueSize rejecting without breaking the DB.
func TestLargeValueAndMaxValueSize(t *testing.T) {
	big := bytes.Repeat([]byte("0123456789abcdef"), (256<<10)/16)
	path := filepath.Join(t.TempDir(), "blobs.pflx")
	db, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Put("big", big); err != nil {
		t.Fatalf("Put(256 KiB): %v", err)
	}
	if err := db.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got, ok := db.Get("big"); !ok || !bytes.Equal(got, big) {
		t.Fatalf("Get(big) = %d bytes, %v; want %d bytes", len(got), ok, len(big))
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got, ok := reopened.Get("big"); !ok || !bytes.Equal(got, big) {
		t.Fatalf("Get(big) after reopen = %d bytes, %v", len(got), ok)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	limited, err := Open(filepath.Join(t.TempDir(), "meta.pflx"), Options{MaxValueSize: 1024})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer limited.Close()
	if err := limited.Put("too-big", make([]byte, 1025)); !errors.Is(err, ErrValueTooLarge) {
		t.Fatalf("Put(1025 bytes) = %v, want ErrValueTooLarge", err)
	}
	if _, ok := limited.Get("too-big"); ok {
		t.Fatal("a rejected value was stored")
	}
	if err := limited.Put("exact", make([]byte, 1024)); err != nil {
		t.Fatalf("Put(1024 bytes): %v", err)
	}
	if err := limited.Put("small", []byte("still usable")); err != nil {
		t.Fatalf("Put after ErrValueTooLarge: %v", err)
	}
	if err := limited.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got, ok := limited.Get("small"); !ok || string(got) != "still usable" {
		t.Fatalf("Get(small) = %q, %v", got, ok)
	}
	if limited.Len() != 2 {
		t.Fatalf("Len = %d, want 2", limited.Len())
	}
}

// 7. concurrent readers and writers, checked under -race.
func TestConcurrentAccess(t *testing.T) {
	const workers = 8
	const ops = 200

	path := filepath.Join(t.TempDir(), "index.pflx")
	db, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				key := fmt.Sprintf("w%d-key-%02d", w, i%37)
				val := []byte(fmt.Sprintf("w%d-value-%04d", w, i))
				if err := db.Put(key, val); err != nil {
					t.Errorf("worker %d: Put: %v", w, err)
					return
				}
				// No other worker touches this worker's key space, so the
				// value right after Put is the value Get must return.
				if got, ok := db.Get(key); !ok || !bytes.Equal(got, val) {
					t.Errorf("worker %d: Get(%s) = %q, %v", w, key, got, ok)
					return
				}
				_ = db.Len()
				_ = db.Stats()
				if err := db.ForEach(func(string, []byte) bool { return true }); err != nil {
					t.Errorf("worker %d: ForEach: %v", w, err)
					return
				}
				if i%50 == 0 {
					if err := db.Commit(); err != nil {
						t.Errorf("worker %d: Commit: %v", w, err)
						return
					}
				}
				if i%97 == 0 {
					trash := fmt.Sprintf("scratch-%d-%d", w, i)
					if err := db.Put(trash, val); err != nil {
						t.Errorf("worker %d: Put(scratch): %v", w, err)
						return
					}
					if err := db.Delete(trash); err != nil {
						t.Errorf("worker %d: Delete: %v", w, err)
						return
					}
					if _, ok := db.Get(trash); ok {
						t.Errorf("worker %d: deleted key %s is still readable", w, trash)
						return
					}
				}
			}
		}(w)
	}
	wg.Wait()

	if err := db.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if db.Len() == 0 {
		t.Fatal("Len = 0 after concurrent work")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if got, want := reopened.Len(), db.Len(); got != want {
		t.Fatalf("Len after reopen = %d, want %d", got, want)
	}
	// Worker 0 rewrites w0-key-00 on every i%37 == 0, so its last write wins.
	last := 0
	for i := 0; i < ops; i++ {
		if i%37 == 0 {
			last = i
		}
	}
	wantVal := []byte(fmt.Sprintf("w0-value-%04d", last))
	if got, ok := reopened.Get("w0-key-00"); !ok || !bytes.Equal(got, wantVal) {
		t.Fatalf("Get(w0-key-00) = %q, %v; want %q", got, ok, wantVal)
	}
}

// 8. delete-while-open. This is the regression: os.OpenFile shares the file as
// FILE_SHARE_READ|FILE_SHARE_WRITE only, so while a handle is live the store
// file cannot be removed - the exact failure being fixed. t.TempDir cleanup,
// the app's clear actions, and any other party's delete all hit it.
func TestRemoveWhileOpenIsNotBlocked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.pflx")
	db, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Put("kept", []byte("value")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := db.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// The handle is live and the file holds data. Removing the whole directory
	// must not report "being used by another process".
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll with a live store handle: %v (want no error: the store file must be shared with FILE_SHARE_DELETE)", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stat %s after RemoveAll = %v, want the file gone", path, err)
	}
	// The document is released only by Close; the removal above is what a
	// t.TempDir cleanup, "Clear index", or "Clear metadata" does.
	_ = db.Close()
}

// 9. the post-delete contract: reads keep serving memory, the durability
// claims report ErrRemoved, and the report is sticky until Compact repairs.
func TestRemovalWhileOpenReportsErrRemoved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.pflx")
	db, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close() // reports ErrRemoved; Close's contract, not asserted here

	if err := db.Put("kept", []byte("value")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := db.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Removal is allowed, and never observed by the writer as a write error:
	// the bytes go into a file nothing can reach.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll with a live store handle: %v", err)
	}

	// Reads claim no durability, so they keep serving the in-memory records.
	if got, ok := db.Get("kept"); !ok || string(got) != "value" {
		t.Fatalf("Get(kept) after removal = %q, %v; want the in-memory value", got, ok)
	}
	if db.Len() != 1 {
		t.Fatalf("Len after removal = %d, want 1", db.Len())
	}
	if err := db.ForEach(func(string, []byte) bool { return true }); err != nil {
		t.Fatalf("ForEach after removal = %v, want nil (reads stay in memory)", err)
	}

	// The durability claim reports it.
	if err := db.Commit(); !errors.Is(err, ErrRemoved) {
		t.Fatalf("Commit after removal = %v, want ErrRemoved", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stat %s after the failed Commit = %v, want the file still gone", path, err)
	}
	// Sticky, like every other append failure: the DB never silently writes on.
	if err := db.Put("late", []byte("x")); !errors.Is(err, ErrRemoved) {
		t.Fatalf("Put after removal = %v, want ErrRemoved", err)
	}
	if err := db.Delete("kept"); !errors.Is(err, ErrRemoved) {
		t.Fatalf("Delete after removal = %v, want ErrRemoved", err)
	}
	if err := db.Commit(); !errors.Is(err, ErrRemoved) {
		t.Fatalf("second Commit after removal = %v, want ErrRemoved", err)
	}

	// Compact is the repair: it writes a fresh snapshot at the path, which
	// recreates the file, and clears the failure. The in-memory records are
	// the ones it writes - they stayed authoritative while the file was gone.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("recreate dir: %v", err)
	}
	if err := db.Compact(); err != nil {
		t.Fatalf("Compact after removal: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat %s after Compact = %v, want the file rewritten", path, err)
	}
	if err := db.Commit(); err != nil {
		t.Fatalf("Commit after Compact: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if got, ok := reopened.Get("kept"); !ok || string(got) != "value" {
		t.Fatalf("reopened Get(kept) = %q, %v; want the pre-removal value", got, ok)
	}
}

// 10. compaction still works with the new open mode: the swap closes the
// handle before the rename, and both open sites - Open and the reopen the swap
// does - carry FILE_SHARE_DELETE, so a compacted store is removable too.
func TestCompactionAndRemovalWithSharedOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.pflx")
	db, err := Open(path, Options{CompactThreshold: 16 << 10})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	const records = 2000
	for i := 0; i < records; i++ {
		if err := db.Put(testKey(i), testValue(i)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := db.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if n := db.Stats().Compactions; n == 0 {
		t.Fatal("Compactions = 0, want > 0 with a 16 KiB delta budget")
	}
	if got := db.Len(); got != records {
		t.Fatalf("Len = %d, want %d", got, records)
	}
	for i := 0; i < records; i++ {
		if got, ok := db.Get(testKey(i)); !ok || !bytes.Equal(got, testValue(i)) {
			t.Fatalf("Get(%s) after compaction missing or wrong", testKey(i))
		}
	}
	if leftovers := tempLeftovers(t, dir); len(leftovers) != 0 {
		t.Fatalf("leftover temp files after the swap: %v", leftovers)
	}

	// Reopen: the file this handle points at is the renamed snapshot.
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("reopen after compaction: %v", err)
	}
	if got := reopened.Len(); got != records {
		t.Fatalf("Len after reopen = %d, want %d", got, records)
	}
	// The reopened handle shares with delete as well: compaction's second open
	// site must not regress the fix.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll after the compaction reopen: %v", err)
	}
	if err := reopened.Commit(); !errors.Is(err, ErrRemoved) {
		t.Fatalf("Commit on the reopened handle after removal = %v, want ErrRemoved", err)
	}
	_ = reopened.Close()
}
