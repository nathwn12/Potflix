// Package store is a crash-safe single-file key/value store for the Potflix
// library index and the metadata cache.
//
// It replaces two shapes that both paid full cost for a small change: a
// 565,380-byte JSON index rewritten in full on every dirty checkpoint, and
// 1,118 loose one-JSON-per-key cache files. Here ~25,000 records of ~150
// bytes live in one append-only frame log behind a 24-byte header: Open
// reads the file sequentially and rebuilds the map in tens of milliseconds,
// a checkpoint appends only the keys that changed, and the delta folds back
// into a snapshot once it grows past Options.CompactThreshold.
//
// # File format (version 1, locked)
//
//	header, 24 bytes:
//	  bytes  0..8   magic "PFLXST01"
//	  u32    8..12  formatVersion = 1
//	  u32   12..16  flags (0)
//	  u32   16..20  crc32-IEEE over bytes 0..16
//	  u32   20..24  reserved (0)
//
//	frames, appended after the header, one per mutation:
//	  u8    tag (1 = put, 2 = delete/tombstone)
//	  u32   payloadLen
//	  u32   crc32-IEEE(payload)
//	  payload
//
//	  tag 1 payload: uvarint keyLen, key, uvarint valueLen, value
//	  tag 2 payload: uvarint keyLen, key
//
// Every integer is little-endian; lengths are LEB128 unsigned varints.
//
// # Crash safety
//
// A frame is accepted only when its tag is known, its payloadLen fits in the
// bytes left in the file, its CRC matches, and its payload decodes exactly.
// The first frame that fails any of those checks ends replay: every earlier
// record is kept, the invalid tail is discarded, and the file is truncated
// back to the last valid frame boundary. A torn tail is the normal outcome
// of an abrupt process kill and is never reported as an error.
//
// # Compaction
//
// Compact writes a fresh file (header plus one put frame per live key, in
// sorted key order) to a temp file in the same directory, syncs it, and
// renames it over the original; the temp file is removed on failure. The
// delta accounting resets on every successful open repair and compaction.
//
// # Concurrency and process model
//
// One mutex guards the in-memory map and the append file, so Get, Put,
// Delete, Len, ForEach, Commit, Compact, and Stats are safe from concurrent
// goroutines. ForEach holds that mutex for the whole iteration, so fn must
// not call back into the DB. Values returned by Get and ForEach alias stored
// memory and must be treated as read-only (Put copies the value it is given).
// The file is held open read-write for the DB's lifetime and the store
// assumes a single writing process - the app holds a single-instance lock on
// 127.0.0.1:17843 - so opening the same file from a second process is
// unsupported.
//
// # Removal while open
//
// The file is opened with a share mode that lets another party delete it while
// this process still holds it (FILE_SHARE_DELETE on Windows, store_windows.go),
// so removing the store file - or the directory holding it - never fails
// because this DB is open. The removal is reported rather than papered over:
// reads (Get, Len, ForEach) keep serving the in-memory records, because they
// claim no durability, and the next durability claim - Commit, a mutation
// under SyncOnCommit, or Close - returns ErrRemoved once the file is no longer
// at db.path. Bytes appended before that report went into the orphaned file
// and are not visible at the path; the error is the only signal, and the
// in-memory records stay authoritative. From then on every mutation and Commit
// keeps reporting it, like any other append failure, until Compact writes a
// fresh snapshot at the path (recreating the file, when its directory still
// exists) and clears the failure.
package store

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const (
	magic         = "PFLXST01"
	formatVersion = 1
	headerSize    = 24
	tagPut        = 1
	tagDelete     = 2

	defaultCompactThreshold = 1 << 20 // 1 MiB of delta before an automatic compact
	appendBufSize           = 64 << 10
	replayBufSize           = 1 << 20
)

// Errors reported by DB methods.
var (
	// ErrClosed is returned by every mutating method after Close. Get and
	// Len keep working from memory; Get reports false for every key.
	ErrClosed = errors.New("store: database is closed")
	// ErrValueTooLarge is returned by Put when Options.MaxValueSize is
	// positive and the value is longer than that. The DB stays usable.
	ErrValueTooLarge = errors.New("store: value exceeds MaxValueSize")
	// ErrRemoved is returned by the durability claims - Commit, a mutation
	// under SyncOnCommit, and Close - once another party has removed the
	// store file, or its directory, while this DB held it open (legal since
	// Open asks for FILE_SHARE_DELETE). Reads keep serving the in-memory
	// records; Compact writes a fresh snapshot at the path and clears the
	// failure. See the package doc's "Removal while open".
	ErrRemoved = errors.New("store: file was removed while open")
)

// Options configures Open. The zero value is usable: no per-write fsync, a
// 1 MiB delta budget, and no size limit on values.
type Options struct {
	// SyncOnCommit makes every Put and Delete fsync before returning, as if
	// each were followed by Commit. Off by default; an abrupt process kill
	// is already survivable without it, since a torn tail is repaired on the
	// next Open. Turn it on only for host crashes or power loss.
	SyncOnCommit bool

	// CompactThreshold is the delta budget in bytes: once that many bytes
	// have been appended since the last snapshot, the next Put or Delete
	// rewrites the file as a fresh snapshot (an O(records) rewrite inside
	// that one call). Zero or negative means the 1 MiB default.
	CompactThreshold int64

	// MaxValueSize rejects a longer value with ErrValueTooLarge. Zero or
	// negative means unlimited.
	MaxValueSize int
}

// Stats is a point-in-time snapshot of the DB's accounting.
type Stats struct {
	Records       int64 // live keys
	SnapshotBytes int64 // file bytes at the last baseline (Open or Compact)
	DeltaBytes    int64 // frame bytes appended since that baseline
	Compactions   int64 // rewrites this session (not persisted)
}

// DB is one open store file. It is safe for concurrent use.
type DB struct {
	mu   sync.Mutex
	path string
	opts Options

	f *os.File      // append file, held open read-write until Close
	w *bufio.Writer // buffered appends over f

	records map[string][]byte // authoritative in-memory state

	snapshotBytes int64
	deltaBytes    int64
	compactions   int64
	closed        bool
	failErr       error // sticky append/flush failure; Compact clears it

	payloadBuf []byte // scratch for one frame payload
	frameBuf   []byte // scratch for one whole frame
}

// Open opens or creates the store at path, loading the snapshot and
// replaying the delta log. A truncated or corrupt tail is discarded and the
// file is truncated back to the last valid frame boundary, which is not an
// error. A file whose header is wrong - bad magic, unexpected version, or a
// header CRC mismatch - is an error: it is not this store's file.
//
// The file is opened so that another party may still remove it while this DB
// holds it; the durability claims then report ErrRemoved (see the package
// doc's "Removal while open").
func Open(path string, opts Options) (*DB, error) {
	opts = normalizeOptions(opts)
	db := &DB{
		path:    path,
		opts:    opts,
		records: make(map[string][]byte, 256),
	}
	f, err := openStoreFile(path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	db.f = f

	valid, err := db.replay()
	if err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(valid, io.SeekStart); err != nil {
		f.Close()
		return nil, fmt.Errorf("store: seek %s: %w", path, err)
	}
	db.w = bufio.NewWriterSize(f, appendBufSize)
	db.snapshotBytes = valid
	return db, nil
}

// Get reports the value stored under key. The returned slice aliases stored
// memory: treat it as read-only.
func (db *DB) Get(key string) ([]byte, bool) {
	db.mu.Lock()
	defer db.mu.Unlock()
	v, ok := db.records[key]
	return v, ok
}

// Len reports the number of live keys. It remains valid after Close.
func (db *DB) Len() int {
	db.mu.Lock()
	defer db.mu.Unlock()
	return len(db.records)
}

// Put stores value under key, appending one frame. The value is copied, so
// the caller may reuse the slice; a zero-length value is legal. If the delta
// budget is exhausted the call also rewrites the file as a fresh snapshot.
func (db *DB) Put(key string, value []byte) error {
	if db.opts.MaxValueSize > 0 && len(value) > db.opts.MaxValueSize {
		return ErrValueTooLarge
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	if db.failErr != nil {
		return db.failedLocked()
	}
	// Update memory first so a compaction triggered by this very append
	// already sees the new record.
	val := make([]byte, len(value))
	copy(val, value)
	db.records[key] = val
	if err := db.appendPutLocked(key, value); err != nil {
		return db.setFailLocked(err)
	}
	return db.afterAppendLocked()
}

// Delete removes key with a tombstone frame. Deleting an absent key is a
// no-op and writes nothing.
func (db *DB) Delete(key string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	if _, ok := db.records[key]; !ok {
		return nil
	}
	if db.failErr != nil {
		return db.failedLocked()
	}
	delete(db.records, key)
	if err := db.appendDeleteLocked(key); err != nil {
		return db.setFailLocked(err)
	}
	return db.afterAppendLocked()
}

// afterAppendLocked honours SyncOnCommit for one mutation.
func (db *DB) afterAppendLocked() error {
	if !db.opts.SyncOnCommit {
		return nil
	}
	if err := db.flushLocked(true); err != nil {
		return db.setFailLocked(err)
	}
	return nil
}

// ForEach calls fn for every live record in unspecified order. fn returns
// false to stop early. It runs with the DB locked: fn must not call any DB
// method. The value aliases stored memory and must not be modified.
func (db *DB) ForEach(fn func(key string, value []byte) bool) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	for k, v := range db.records {
		if !fn(k, v) {
			break
		}
	}
	return nil
}

// Commit flushes buffered appends and fsyncs them: after Commit returns, the
// records are on disk. It is the explicit durability barrier and always
// fsyncs, whatever SyncOnCommit says (that option only makes every Put and
// Delete do the same). It reports ErrRemoved when the file was removed while
// this DB held it open (see the package doc's "Removal while open").
func (db *DB) Commit() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	if db.failErr != nil {
		return db.failedLocked()
	}
	if err := db.flushLocked(true); err != nil {
		return db.setFailLocked(err)
	}
	return nil
}

// Compact rewrites the file as a fresh snapshot of the live records: a temp
// file in the same directory, synced, then renamed over the original. It
// also clears a sticky append failure. deltaBytes resets to zero.
func (db *DB) Compact() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	return db.compactLocked()
}

// Stats reports the current counters.
func (db *DB) Stats() Stats {
	db.mu.Lock()
	defer db.mu.Unlock()
	return Stats{
		Records:       int64(len(db.records)),
		SnapshotBytes: db.snapshotBytes,
		DeltaBytes:    db.deltaBytes,
		Compactions:   db.compactions,
	}
}

// Close flushes and fsyncs the pending appends and releases the file. A
// second Close is a no-op. The in-memory records remain readable through
// Get and Len. Like Commit, it reports ErrRemoved when the file is no longer
// at its path.
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil
	}
	db.closed = true
	err := db.flushLocked(true)
	if cerr := db.f.Close(); err == nil {
		err = cerr
	}
	db.f = nil
	db.w = nil
	return err
}

// --- replay ---------------------------------------------------------------

// replay rebuilds the in-memory map from the file and returns the offset of
// the last valid frame boundary, truncating any invalid tail.
func (db *DB) replay() (int64, error) {
	info, err := db.f.Stat()
	if err != nil {
		return 0, fmt.Errorf("store: stat %s: %w", db.path, err)
	}
	size := info.Size()

	switch {
	case size == 0:
		if err := db.writeHeaderLocked(); err != nil {
			return 0, err
		}
		return headerSize, nil
	case size < headerSize:
		// A header write that never finished. The store owns the file, so
		// start it over rather than fail.
		if err := db.f.Truncate(0); err != nil {
			return 0, fmt.Errorf("store: truncate %s: %w", db.path, err)
		}
		if err := db.writeHeaderLocked(); err != nil {
			return 0, err
		}
		return headerSize, nil
	}

	hdr := make([]byte, headerSize)
	if _, err := db.f.ReadAt(hdr, 0); err != nil {
		return 0, fmt.Errorf("store: read header %s: %w", db.path, err)
	}
	if err := validateHeader(hdr); err != nil {
		return 0, fmt.Errorf("store: %s: %w", db.path, err)
	}
	if _, err := db.f.Seek(headerSize, io.SeekStart); err != nil {
		return 0, fmt.Errorf("store: seek %s: %w", db.path, err)
	}

	valid := int64(headerSize)
	fr := &frameReader{
		r:    bufio.NewReaderSize(db.f, replayBufSize),
		off:  headerSize,
		size: size,
	}
	var scratch [8]byte
	for {
		tag, err := fr.r.ReadByte()
		if err != nil {
			break // clean end, or a frame header that was never finished
		}
		fr.off++
		if tag != tagPut && tag != tagDelete {
			break
		}
		if _, err := io.ReadFull(fr.r, scratch[:]); err != nil {
			break
		}
		fr.off += 8
		payloadLen := int64(binary.LittleEndian.Uint32(scratch[0:4]))
		want := binary.LittleEndian.Uint32(scratch[4:8])
		if payloadLen > fr.size-fr.off {
			break // the payload was never fully written
		}
		payload := db.payloadBuf[:0]
		if int64(cap(payload)) < payloadLen {
			payload = make([]byte, payloadLen)
		} else {
			payload = payload[:payloadLen]
		}
		if _, err := io.ReadFull(fr.r, payload); err != nil {
			break
		}
		fr.off += payloadLen
		if crc32.Checksum(payload, crc32.IEEETable) != want {
			break
		}
		if !db.applyPayload(tag, payload) {
			break
		}
		db.payloadBuf = payload
		valid = fr.off
	}

	if valid != size {
		if err := db.f.Truncate(valid); err != nil {
			return 0, fmt.Errorf("store: truncate torn tail of %s: %w", db.path, err)
		}
	}
	return valid, nil
}

// frameReader tracks the offset of the bytes read from a buffered file.
type frameReader struct {
	r    *bufio.Reader
	off  int64
	size int64
}

// applyPayload decodes one frame payload and applies it to the map. It
// reports false for a payload that does not decode exactly, which ends
// replay.
func (db *DB) applyPayload(tag byte, payload []byte) bool {
	klen, n := binary.Uvarint(payload)
	if n <= 0 || klen > uint64(len(payload)-n) {
		return false
	}
	key := string(payload[n : n+int(klen)])
	n += int(klen)

	switch tag {
	case tagDelete:
		if n != len(payload) {
			return false
		}
		delete(db.records, key)
	case tagPut:
		vlen, m := binary.Uvarint(payload[n:])
		if m <= 0 || vlen > uint64(len(payload)-(n+m)) {
			return false
		}
		n += m
		val := make([]byte, vlen)
		copy(val, payload[n:n+int(vlen)])
		n += int(vlen)
		if n != len(payload) {
			return false
		}
		db.records[key] = val
	}
	return true
}

// --- appending ------------------------------------------------------------

func (db *DB) appendPutLocked(key string, value []byte) error {
	p := db.payloadBuf[:0]
	p = binary.AppendUvarint(p, uint64(len(key)))
	p = append(p, key...)
	p = binary.AppendUvarint(p, uint64(len(value)))
	p = append(p, value...)
	db.payloadBuf = p
	return db.appendFrameLocked(tagPut, p)
}

func (db *DB) appendDeleteLocked(key string) error {
	p := db.payloadBuf[:0]
	p = binary.AppendUvarint(p, uint64(len(key)))
	p = append(p, key...)
	db.payloadBuf = p
	return db.appendFrameLocked(tagDelete, p)
}

// appendFrameLocked writes one frame through the buffer, then compacts if
// the delta budget is now exhausted.
func (db *DB) appendFrameLocked(tag byte, payload []byte) error {
	f := db.frameBuf[:0]
	f = append(f, tag)
	f = binary.LittleEndian.AppendUint32(f, uint32(len(payload)))
	f = binary.LittleEndian.AppendUint32(f, crc32.Checksum(payload, crc32.IEEETable))
	f = append(f, payload...)
	db.frameBuf = f

	if _, err := db.w.Write(f); err != nil {
		return fmt.Errorf("store: append frame: %w", err)
	}
	db.deltaBytes += int64(len(f))
	if db.deltaBytes >= db.opts.CompactThreshold {
		return db.compactLocked()
	}
	return nil
}

func (db *DB) flushLocked(sync bool) error {
	if err := db.w.Flush(); err != nil {
		return fmt.Errorf("store: flush: %w", err)
	}
	if sync {
		if err := db.f.Sync(); err != nil {
			return fmt.Errorf("store: fsync: %w", err)
		}
	}
	// The bytes reached the handle, not necessarily db.path: a share mode
	// that lets another party delete the file means this handle can outlive
	// it, and a write into a deleted file reports no error at all. Claiming
	// durability is only honest when the file is still there, so this is the
	// one check every durability claim goes through.
	return db.removedLocked()
}

// removedLocked reports ErrRemoved when the store file is no longer at
// db.path: the bytes just flushed went somewhere no reader can reach, and the
// caller must not be told they are durable.
func (db *DB) removedLocked() error {
	if _, err := os.Stat(db.path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s: %w", db.path, ErrRemoved)
		}
		return fmt.Errorf("store: stat %s: %w", db.path, err)
	}
	return nil
}

// setFailLocked records the first append/flush failure. Memory is
// authoritative from then on: reads keep working, every mutation and Commit
// reports the failure, and Compact clears it by rewriting the file.
func (db *DB) setFailLocked(err error) error {
	if db.failErr == nil {
		db.failErr = err
	}
	return err
}

func (db *DB) failedLocked() error {
	return fmt.Errorf("store: earlier append failed, Compact to repair: %w", db.failErr)
}

// --- compaction -----------------------------------------------------------

// compactLocked rewrites the whole file from the live map. The caller holds
// the mutex.
func (db *DB) compactLocked() error {
	tmp, err := os.CreateTemp(filepath.Dir(db.path), filepath.Base(db.path)+".compact-*")
	if err != nil {
		return fmt.Errorf("store: compact temp: %w", err)
	}
	tmpName := tmp.Name()
	discard := func() {
		tmp.Close()
		os.Remove(tmpName)
	}

	bw := bufio.NewWriterSize(tmp, appendBufSize)
	hdr := make([]byte, headerSize)
	encodeHeader(hdr)
	if _, err := bw.Write(hdr); err != nil {
		discard()
		return fmt.Errorf("store: compact header: %w", err)
	}
	keys := make([]string, 0, len(db.records))
	for k := range db.records {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic snapshot
	written := int64(headerSize)
	var frame []byte
	for _, k := range keys {
		frame = appendPutFrame(frame[:0], k, db.records[k])
		if _, err := bw.Write(frame); err != nil {
			discard()
			return fmt.Errorf("store: compact frame: %w", err)
		}
		written += int64(len(frame))
	}
	if err := bw.Flush(); err != nil {
		discard()
		return fmt.Errorf("store: compact flush: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		discard()
		return fmt.Errorf("store: compact sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("store: compact close: %w", err)
	}

	// Swap the file: get our pending bytes out, drop the handle, rename,
	// reopen. The temp file is removed on every failure path.
	if err := db.w.Flush(); err != nil {
		os.Remove(tmpName)
		return db.setFailLocked(fmt.Errorf("store: compact flush: %w", err))
	}
	if err := db.f.Close(); err != nil {
		os.Remove(tmpName)
		reopenErr := db.reopenLocked()
		if reopenErr != nil {
			return fmt.Errorf("store: compact close (%v) and reopen: %w", err, reopenErr)
		}
		return fmt.Errorf("store: compact close: %w", err)
	}
	if err := os.Rename(tmpName, db.path); err != nil {
		os.Remove(tmpName)
		if reopenErr := db.reopenLocked(); reopenErr != nil {
			return fmt.Errorf("store: compact rename (%v) and reopen: %w", err, reopenErr)
		}
		return fmt.Errorf("store: compact rename: %w", err)
	}
	if err := db.reopenLocked(); err != nil {
		return fmt.Errorf("store: compact reopen: %w", err)
	}

	db.snapshotBytes = written
	db.deltaBytes = 0
	db.compactions++
	db.failErr = nil
	return nil
}

// reopenLocked reopens db.path for appending after the handle was dropped.
func (db *DB) reopenLocked() error {
	f, err := openStoreFile(db.path)
	if err != nil {
		db.f, db.w = nil, nil
		return fmt.Errorf("store: reopen %s: %w", db.path, err)
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		f.Close()
		db.f, db.w = nil, nil
		return fmt.Errorf("store: seek %s: %w", db.path, err)
	}
	db.f = f
	db.w = bufio.NewWriterSize(f, appendBufSize)
	return nil
}

// --- encoding -------------------------------------------------------------

func normalizeOptions(opts Options) Options {
	if opts.CompactThreshold <= 0 {
		opts.CompactThreshold = defaultCompactThreshold
	}
	if opts.MaxValueSize < 0 {
		opts.MaxValueSize = 0
	}
	return opts
}

func encodeHeader(dst []byte) {
	copy(dst[0:8], magic)
	binary.LittleEndian.PutUint32(dst[8:12], formatVersion)
	binary.LittleEndian.PutUint32(dst[12:16], 0) // flags
	binary.LittleEndian.PutUint32(dst[16:20], crc32.Checksum(dst[0:16], crc32.IEEETable))
	binary.LittleEndian.PutUint32(dst[20:24], 0) // reserved
}

func validateHeader(hdr []byte) error {
	if string(hdr[0:8]) != magic {
		return fmt.Errorf("bad magic %q, not a store file", hdr[0:8])
	}
	if v := binary.LittleEndian.Uint32(hdr[8:12]); v != formatVersion {
		return fmt.Errorf("unsupported format version %d", v)
	}
	got := binary.LittleEndian.Uint32(hdr[16:20])
	want := crc32.Checksum(hdr[0:16], crc32.IEEETable)
	if got != want {
		return fmt.Errorf("header CRC mismatch: got %08x want %08x", got, want)
	}
	return nil
}

func (db *DB) writeHeaderLocked() error {
	hdr := make([]byte, headerSize)
	encodeHeader(hdr)
	if _, err := db.f.WriteAt(hdr, 0); err != nil {
		return fmt.Errorf("store: write header %s: %w", db.path, err)
	}
	if err := db.f.Sync(); err != nil {
		return fmt.Errorf("store: sync header %s: %w", db.path, err)
	}
	return nil
}

// appendPutFrame appends one complete put frame to dst.
func appendPutFrame(dst []byte, key string, value []byte) []byte {
	payload := make([]byte, 0, len(key)+len(value)+12)
	payload = binary.AppendUvarint(payload, uint64(len(key)))
	payload = append(payload, key...)
	payload = binary.AppendUvarint(payload, uint64(len(value)))
	payload = append(payload, value...)

	dst = append(dst, tagPut)
	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(payload)))
	dst = binary.LittleEndian.AppendUint32(dst, crc32.Checksum(payload, crc32.IEEETable))
	return append(dst, payload...)
}
