// Package bundle exports, imports, counts and clears Potflix metadata
// cache bundles.
//
// The live cache is not a directory of files any more: the metadata package
// keeps ONE store file per cache directory (metadata.CacheForEach,
// metadata.CachePutRaw, metadata.ClearCacheEntries; <dir>.idx, one record per
// cache key). This package therefore reads and writes the cache through that
// surface instead of assuming a layout, and never opens the store file itself.
//
// The bundle FILE format is unchanged and self-contained: one JSON envelope
// ({"format":"potflix-metadata","version":1,...}) carrying one entry per cache
// key, {"file":"<16-hex>.json","key":...,"schema":N,"media":{...}}, where
// "file" is the name metadata.CacheFileNameFor derives from the key. A bundle
// written before the store rework imports exactly as it did then: only the
// envelope and the entry fields are read, never a file on disk.
//
// Import is fill-missing-only: an entry whose key the live cache already holds
// is reported Present and its stored bytes are never replaced.
package bundle

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"potflix/internal/metadata"
)

// FormatName identifies bundle payloads. FormatVersion is the bundle
// envelope version (distinct from the cache Schema carried per entry).
const FormatName = "potflix-metadata"

// FormatVersion is the current bundle envelope version.
const FormatVersion = 1

// maxBundleBytes caps how much Import will read: a larger bundle is
// refused so a hostile or huge file cannot exhaust memory.
const maxBundleBytes = 64 << 20 // 64 MB

// maxBundleTotalBytes caps the aggregate bytes Export will carry: beyond it
// Export fails with an error rather than growing the bundle without bound. One
// entry is bounded by the cache itself - the store refuses a record larger than
// metadata's per-record cap when it is written - so only the aggregate needs a
// guard here.
const maxBundleTotalBytes = 64 << 20 // 64 MB

// NoteText is the Export header note. It states the privacy scope of a
// bundle in prose.
const NoteText = "This bundle contains metadata only: titles, synopsis, genres, length, button URLs and poster URLs. It contains no media files, no library paths, and no credentials."

// Header describes a bundle envelope.
type Header struct {
	Format     string    `json:"format"`
	Version    int       `json:"version"`
	Schema     int       `json:"schema"`
	AppVersion string    `json:"app_version"`
	ExportedAt time.Time `json:"exported_at"`
	Count      int       `json:"count"`
	Note       string    `json:"note"`
}

// Entry is one cache file carried inside a bundle. Media is opaque: it is
// passed through without interpretation.
type Entry struct {
	File   string          `json:"file"` // the 16-hex cache filename, e.g. "ab12cd34ef56ab78.json"
	Key    string          `json:"key"`
	Schema int             `json:"schema"`
	Media  json.RawMessage `json:"media"` // opaque passthrough
}

// Bundle is the export/import envelope.
type Bundle struct {
	Header  Header  `json:"header"`
	Entries []Entry `json:"entries"`
}

// ExportStats reports an Export call.
type ExportStats struct {
	Entries int
	Skipped int
	Bytes   int64
}

// ImportStats reports an Import call.
type ImportStats struct {
	Added         int
	Present       int
	SkippedSchema int
	Invalid       int
	Total         int
}

// cacheRecord mirrors the cache envelope the store carries: the record's own
// key, schema and media. Media stays raw so its bytes survive an export/import
// round-trip unchanged, and the fields the store does not model (a negative
// entry's notFound/checkedAt) are not carried - exactly as before the store.
type cacheRecord struct {
	Key    string          `json:"key"`
	Schema int             `json:"schema"`
	Media  json.RawMessage `json:"media"`
}

// requireDir refuses an empty cache directory. Every function here takes the
// directory explicitly, and an empty one would otherwise resolve to the
// metadata package's DEFAULT cache directory - the live one - so a caller that
// lost the argument would count, clear or overwrite the real cache.
func requireDir(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("bundle: no cache directory given")
	}
	return nil
}

// validCacheName reports whether name is a cache filename: 16 lowercase
// hex chars plus ".json". The 16-char rule is owned by
// metadata.cacheFileName (hex of sha256 truncated to 16 chars); this
// package re-states it so an entry's declared File is validated on import
// without a round trip through the metadata package.
func validCacheName(name string) bool {
	const suffix = ".json"
	if len(name) != 16+len(suffix) || !strings.HasSuffix(name, suffix) {
		return false
	}
	for i := 0; i < 16; i++ {
		c := name[i]
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'f' {
			continue
		}
		return false
	}
	return true
}

// checkBundleDir refuses a cache directory that is itself a reparse point
// (a symlink or a Windows junction): operating through one could read or
// delete files outside the cache. A missing directory is not a refusal;
// the caller keeps its normal behaviour (Import creates it, Export reports
// the ReadDir error, Clear treats it as empty).
func checkBundleDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("bundle: refusing to operate on a reparse-point directory %q", dir)
	}
	if dirHasReparseAttribute(dir) {
		return fmt.Errorf("bundle: refusing to operate on a reparse-point directory %q", dir)
	}
	return nil
}

// isReparsePoint reports whether path is itself a reparse point (symlink
// or Windows junction), checked without following it. An unstatable path
// reports false so the caller keeps its normal error path.
func isReparsePoint(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return true
	}
	return dirHasReparseAttribute(path)
}

// Export reads every entry in the live cache under dir and writes one indented
// bundle to w. Entries are sorted by Key so two exports of the same cache are
// byte-identical. A stored record that does not decode as a cache entry (no
// key, or not JSON) counts as Skipped and never fails the export. Each entry's
// File is the 16-hex name metadata.CacheFileNameFor derives from its key, so a
// bundle exported after the store rework is byte-identical in shape to one
// exported from the legacy per-key files. A cache directory that is not there
// holds no entries, so it exports an empty bundle rather than an error: the
// cache is the store file beside the directory, which the directory removal of
// metadata.ClearCache leaves in place.
func Export(dir string, w io.Writer, schema int, appVersion string) (ExportStats, error) {
	var st ExportStats
	if err := requireDir(dir); err != nil {
		return st, err
	}
	if err := checkBundleDir(dir); err != nil {
		return st, err
	}
	entries := []Entry{}
	var total int64
	var capErr error
	err := metadata.CacheForEach(dir, func(_ string, value []byte) bool {
		total += int64(len(value))
		if total > maxBundleTotalBytes {
			capErr = fmt.Errorf("bundle: refusing export larger than 64 MB (aggregate entry bytes exceed the cap)")
			return false
		}
		var rec cacheRecord
		if err := json.Unmarshal(value, &rec); err != nil || rec.Key == "" {
			st.Skipped++
			return true
		}
		entries = append(entries, Entry{
			File:   metadata.CacheFileNameFor(rec.Key),
			Key:    rec.Key,
			Schema: rec.Schema,
			Media:  rec.Media,
		})
		return true
	})
	if capErr != nil {
		return st, capErr
	}
	if err != nil {
		return st, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	// ExportedAt is wall-clock: determinism holds over the ENTRIES (sorted
	// by Key with stable bytes), never over the whole byte stream.
	b := Bundle{
		Header: Header{
			Format:     FormatName,
			Version:    FormatVersion,
			Schema:     schema,
			AppVersion: appVersion,
			ExportedAt: time.Now().UTC(),
			Count:      len(entries),
			Note:       NoteText,
		},
		Entries: entries,
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return st, err
	}
	data = append(data, '\n')
	n, err := w.Write(data)
	st.Bytes = int64(n)
	st.Entries = len(entries)
	if err != nil {
		return st, err
	}
	return st, nil
}

// Import merges a bundle from r into the live cache under dir:
// fill-missing-only. It NEVER replaces an entry the cache already holds - a
// key the store has is reported Present and its stored bytes are untouched.
// Entries whose Schema differs from schema are skipped (SkippedSchema); entries
// with a bad File name or an empty Key are Invalid. A bad header or an
// unparsable body is an error, and in the unparsable case nothing has been
// written, because the body is parsed fully before the first write.
//
// An imported entry is committed (flush + fsync) as it lands, so CacheLive,
// CacheHas and ProbeCache see it the moment this returns. A record the store
// refuses - larger than its per-record cap - fails the import with that key's
// error, exactly as any other write failure does; entries written before it
// stay imported.
func Import(dir string, r io.Reader, schema int) (ImportStats, error) {
	var st ImportStats
	if err := requireDir(dir); err != nil {
		return st, err
	}
	if err := checkBundleDir(dir); err != nil {
		return st, err
	}
	data, err := io.ReadAll(io.LimitReader(r, maxBundleBytes+1))
	if err != nil {
		return st, fmt.Errorf("bundle: read bundle: %w", err)
	}
	if len(data) > maxBundleBytes {
		return st, fmt.Errorf("bundle: refusing bundle larger than 64 MB (%d bytes read)", len(data))
	}
	var b Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return st, fmt.Errorf("bundle: invalid bundle JSON: %w", err)
	}
	if b.Header.Format != FormatName || b.Header.Version != FormatVersion {
		return st, fmt.Errorf("bundle: unsupported bundle header: format %q version %d (want %q version %d)",
			b.Header.Format, b.Header.Version, FormatName, FormatVersion)
	}
	// Nothing is written to the directory itself: the first stored entry
	// creates the store file beside it, exactly as a resolver write does, so
	// an import never leaves a loose JSON file behind and never needs the
	// legacy directory to exist.
	st.Total = len(b.Entries)
	for _, e := range b.Entries {
		if !validCacheName(e.File) || e.Key == "" {
			st.Invalid++
			continue
		}
		if e.Schema != schema {
			st.SkippedSchema++
			continue
		}
		// Fill-missing-only: presence is the store's, read through the same
		// held handle the write uses. The app is single-instance and one
		// import runs at a time, so nothing claims the key between this check
		// and the write; even a rival in-process writer would cost one
		// duplicate count, never a second record, because the store keeps one
		// record per key.
		if metadata.CacheHas(dir, e.Key) {
			st.Present++
			continue
		}
		payload, err := json.Marshal(cacheRecord{Key: e.Key, Schema: e.Schema, Media: e.Media})
		if err != nil {
			st.Invalid++
			continue
		}
		if err := metadata.CachePutRaw(dir, e.Key, payload); err != nil {
			return st, err
		}
		st.Added++
	}
	return st, nil
}

// Count returns how many entries the live cache under dir holds (one record per
// cache key, i.e. one per show/season or movie/year - metadata.CacheKeyFor).
func Count(dir string) (int, error) {
	if err := requireDir(dir); err != nil {
		return 0, err
	}
	n := 0
	err := metadata.CacheForEach(dir, func(_ string, _ []byte) bool {
		n++
		return true
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// Clear empties the live cache under dir (metadata.ClearCacheEntries: every
// stored record is deleted and the store file compacted back to an empty
// snapshot) and removes the legacy per-key cache files the store replaced -
// names matching ^[0-9a-f]{16}\.json$ per metadata.cacheFileName, plus ".tmp-"
// orphans - directly inside dir. It returns how many cache ENTRIES (titles) it
// cleared. It never recurses, never removes dir itself, never touches a
// subdirectory, and never deletes anything else: the legacy meta\ recovery
// directory is removed by metadata.ClearCache alone. The legacy files must go,
// because they are what migration imports into an empty store - leaving them
// would resurrect exactly what was cleared on the next read.
func Clear(dir string) (int, error) {
	if err := requireDir(dir); err != nil {
		return 0, err
	}
	if err := checkBundleDir(dir); err != nil {
		return 0, err
	}
	// Counting and clearing are one operation on the store, and its own
	// enumeration imports any legacy file it finds first, so a title that
	// still lives only as a legacy file is counted once, not twice.
	cleared, err := metadata.ClearCacheEntries(dir)
	if err != nil {
		return cleared, err
	}
	if err := removeLegacyCacheFiles(dir); err != nil {
		return cleared, err
	}
	return cleared, nil
}

// removeLegacyCacheFiles deletes the legacy per-key cache files directly inside
// dir - never another file, never a subdirectory, and never through a reparse
// point. A missing dir removes nothing and is not an error.
func removeLegacyCacheFiles(dir string) error {
	fis, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, fi := range fis {
		if fi.IsDir() {
			continue
		}
		name := fi.Name()
		if !validCacheName(name) && !strings.HasPrefix(name, ".tmp-") {
			continue
		}
		full := filepath.Join(dir, name)
		if isReparsePoint(full) {
			continue // never delete through a reparse point
		}
		if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
