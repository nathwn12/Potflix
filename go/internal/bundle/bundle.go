// Package bundle exports, imports, counts and clears Potflix metadata
// cache bundles.
//
// It is intentionally self-contained: it never imports the metadata
// package, so it cannot collide with work in progress there. It relies
// only on the documented on-disk shape: one JSON file per key named
// <sha256-hex>.json with content {"key":...,"schema":N,"media":{...}}.
// Import publishes fill-missing-only with no-replace semantics (temp file
// plus hard link onto the final name, falling back to an exclusive
// create); it never renames over an existing file.
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
)

// FormatName identifies bundle payloads. FormatVersion is the bundle
// envelope version (distinct from the cache Schema carried per entry).
const FormatName = "potflix-metadata"

// FormatVersion is the current bundle envelope version.
const FormatVersion = 1

// maxBundleBytes caps how much Import will read: a larger bundle is
// refused so a hostile or huge file cannot exhaust memory.
const maxBundleBytes = 64 << 20 // 64 MB

// maxBundleEntryBytes caps how much Export will read for ONE cache entry:
// a larger entry counts as Skipped so a hostile file cannot exhaust
// memory. maxBundleTotalBytes caps the aggregate bytes read across one
// Export: beyond it Export fails with an error rather than growing the
// bundle without bound.
const maxBundleEntryBytes = 4 << 20  // 4 MB
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

// cacheRecord mirrors the on-disk cache envelope. Media stays raw so its
// bytes survive an export/import round-trip unchanged.
type cacheRecord struct {
	Key    string          `json:"key"`
	Schema int             `json:"schema"`
	Media  json.RawMessage `json:"media"`
}

// validCacheName reports whether name is a cache filename: 16 lowercase
// hex chars plus ".json". The 16-char rule is owned by
// metadata.cacheFileName (hex of sha256 truncated to 16 chars); this
// package re-states it so it never imports the metadata package.
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

// readCappedEntry reads one cache entry capped at maxBundleEntryBytes. It
// reports false (never an error, never unbounded memory) when the file
// cannot be read or exceeds the cap; the caller counts that as Skipped.
func readCappedEntry(path string) ([]byte, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBundleEntryBytes+1))
	if err != nil {
		return nil, false
	}
	if len(data) > maxBundleEntryBytes {
		return nil, false
	}
	return data, true
}

// Export reads every cache entry in dir and writes one indented bundle to
// w. Entries are sorted by Key so two exports of the same cache are
// byte-identical. A corrupt or unreadable entry file counts as Skipped and
// never fails the export. Files that are not cache entries (other names,
// ".tmp-" orphans, subdirectories) are ignored.
func Export(dir string, w io.Writer, schema int, appVersion string) (ExportStats, error) {
	var st ExportStats
	if err := checkBundleDir(dir); err != nil {
		return st, err
	}
	fis, err := os.ReadDir(dir)
	if err != nil {
		return st, err
	}
	entries := []Entry{}
	var total int64
	for _, fi := range fis {
		if fi.IsDir() {
			continue
		}
		name := fi.Name()
		if !validCacheName(name) {
			continue
		}
		full := filepath.Join(dir, name)
		if isReparsePoint(full) {
			st.Skipped++
			continue
		}
		data, ok := readCappedEntry(full)
		if !ok {
			st.Skipped++
			continue
		}
		total += int64(len(data))
		if total > maxBundleTotalBytes {
			return st, fmt.Errorf("bundle: refusing export larger than 64 MB (aggregate entry bytes exceed the cap)")
		}
		var rec cacheRecord
		if err := json.Unmarshal(data, &rec); err != nil || rec.Key == "" {
			st.Skipped++
			continue
		}
		entries = append(entries, Entry{File: name, Key: rec.Key, Schema: rec.Schema, Media: rec.Media})
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

// Import merges a bundle from r into dir: fill-missing-only. It NEVER
// overwrites an existing cache file. Entries whose Schema differs from
// schema are skipped (SkippedSchema); entries with a bad File name or an
// empty Key are Invalid. A bad header or an unparsable body is an error,
// and in the unparsable case nothing has been written, because the body is
// parsed fully before the first write.
func Import(dir string, r io.Reader, schema int) (ImportStats, error) {
	var st ImportStats
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return st, err
	}
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
		dest := filepath.Join(dir, e.File)
		if _, err := os.Lstat(dest); err == nil {
			st.Present++
			continue
		} else if !os.IsNotExist(err) {
			return st, err
		}
		payload, err := json.Marshal(cacheRecord{Key: e.Key, Schema: e.Schema, Media: e.Media})
		if err != nil {
			st.Invalid++
			continue
		}
		tmp, err := os.CreateTemp(dir, ".tmp-*.json")
		if err != nil {
			return st, err
		}
		tmpName := tmp.Name()
		_, werr := tmp.Write(payload)
		cerr := tmp.Close()
		if werr != nil || cerr != nil {
			os.Remove(tmpName)
			if werr != nil {
				return st, werr
			}
			return st, cerr
		}
		// No-replace publish: hard-link the temp file onto the final name.
		// os.Link fails with EEXIST when the name is already taken, so
		// fill-missing-only holds even if a rival claims dest inside the
		// Lstat/publish race above. os.Rename must never be used here: it
		// would silently replace the existing file.
		if err := os.Link(tmpName, dest); err == nil {
			os.Remove(tmpName)
			st.Added++
			continue
		} else if os.IsExist(err) {
			os.Remove(tmpName)
			st.Present++
			continue
		}
		// Filesystems without hard links fall back to an exclusive create,
		// which likewise fails with EEXIST instead of replacing.
		f, oerr := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if oerr != nil {
			os.Remove(tmpName)
			if os.IsExist(oerr) {
				st.Present++
				continue
			}
			return st, oerr
		}
		_, werr = f.Write(payload)
		cerr = f.Close()
		os.Remove(tmpName)
		if werr != nil {
			os.Remove(dest)
			return st, werr
		}
		if cerr != nil {
			os.Remove(dest)
			return st, cerr
		}
		st.Added++
	}
	return st, nil
}

// Count returns how many cache files (names matching
// ^[0-9a-f]{16}\.json$ per metadata.cacheFileName) sit directly inside dir.
func Count(dir string) (int, error) {
	fis, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, fi := range fis {
		if fi.IsDir() {
			continue
		}
		if validCacheName(fi.Name()) {
			n++
		}
	}
	return n, nil
}

// Clear deletes only cache files (names matching ^[0-9a-f]{16}\.json$ per
// metadata.cacheFileName) plus ".tmp-" orphans directly inside dir. It never recurses, never
// touches a subdirectory, and never deletes anything else. It returns how
// many files it removed.
func Clear(dir string) (int, error) {
	if err := checkBundleDir(dir); err != nil {
		return 0, err
	}
	fis, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
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
		if err := os.Remove(full); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return removed, err
		}
		removed++
	}
	return removed, nil
}
