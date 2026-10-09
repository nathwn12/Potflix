package bundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

const testSchema = 3

func cacheNameFor(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:16] + ".json"
}

func seedCacheFile(t *testing.T, dir, key string, schema int, media string) string {
	t.Helper()
	name := cacheNameFor(key)
	data, err := json.Marshal(cacheRecord{Key: key, Schema: schema, Media: json.RawMessage(media)})
	if err != nil {
		t.Fatalf("marshal seed %q: %v", key, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatalf("write seed %q: %v", key, err)
	}
	return name
}

func readCacheRecord(t *testing.T, path string) cacheRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var rec cacheRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return rec
}

// testBundleBytes builds a minimal well-formed bundle payload for Import tests.
func testBundleBytes(t *testing.T, entries []Entry) []byte {
	t.Helper()
	b := Bundle{
		Header: Header{
			Format:     FormatName,
			Version:    FormatVersion,
			Schema:     testSchema,
			AppVersion: "test",
			Count:      len(entries),
			Note:       NoteText,
		},
		Entries: entries,
	}
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal test bundle: %v", err)
	}
	return data
}

func dirChildNames(t *testing.T, dir string) []string {
	t.Helper()
	fis, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	names := make([]string, 0, len(fis))
	for _, fi := range fis {
		names = append(names, fi.Name())
	}
	return names
}

func TestBundleRoundTrip(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	keys := []string{"movie|alpha|2001", "series|beta|2020|2", "movie|gamma|1999"}
	media := []string{
		`{"title":"Alpha","poster":"https://image.example.com/alpha.jpg"}`,
		`{"title":"Beta","genres":["Drama"]}`,
		`{"title":"Gamma","length":5400}`,
	}
	names := make([]string, len(keys))
	for i := range keys {
		names[i] = seedCacheFile(t, a, keys[i], testSchema, media[i])
	}

	var buf bytes.Buffer
	est, err := Export(a, &buf, testSchema, "test-0.1")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if est.Entries != 3 || est.Skipped != 0 {
		t.Fatalf("Export stats = %+v, want Entries=3 Skipped=0", est)
	}
	if est.Bytes != int64(buf.Len()) {
		t.Fatalf("Export bytes = %d, buffer holds %d", est.Bytes, buf.Len())
	}

	var got Bundle
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("exported bundle does not parse: %v", err)
	}
	if got.Header.Format != FormatName || got.Header.Version != FormatVersion {
		t.Fatalf("header identity = %q v%d", got.Header.Format, got.Header.Version)
	}
	if got.Header.Schema != testSchema || got.Header.Count != 3 {
		t.Fatalf("header = %+v, want Schema=%d Count=3", got.Header, testSchema)
	}
	for _, want := range []string{
		"metadata only", "titles", "synopsis", "genres", "length",
		"button URLs", "poster URLs",
		"no media files", "no library paths", "no credentials",
	} {
		if !strings.Contains(got.Header.Note, want) {
			t.Fatalf("header note missing %q: %q", want, got.Header.Note)
		}
	}

	ist, err := Import(b, &buf, testSchema)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if ist.Added != 3 || ist.Total != 3 || ist.Present != 0 || ist.SkippedSchema != 0 || ist.Invalid != 0 {
		t.Fatalf("Import stats = %+v, want Added=3 Total=3 rest 0", ist)
	}
	if c, err := Count(b); err != nil || c != 3 {
		t.Fatalf("Count(B) = %d, %v; want 3", c, err)
	}

	for i := range keys {
		da, err := os.ReadFile(filepath.Join(a, names[i]))
		if err != nil {
			t.Fatalf("read source %s: %v", names[i], err)
		}
		db, err := os.ReadFile(filepath.Join(b, names[i]))
		if err != nil {
			t.Fatalf("missing imported file %s: %v", names[i], err)
		}
		if !bytes.Equal(da, db) {
			t.Fatalf("file %s differs after round-trip", names[i])
		}
		ra := readCacheRecord(t, filepath.Join(a, names[i]))
		rb := readCacheRecord(t, filepath.Join(b, names[i]))
		if string(ra.Media) != media[i] {
			t.Fatalf("source media mangled for %q: %s", keys[i], ra.Media)
		}
		if string(rb.Media) != string(ra.Media) {
			t.Fatalf("media not byte-identical for %q", keys[i])
		}
	}
}

func TestBundleDeterminism(t *testing.T) {
	dir := t.TempDir()
	seedCacheFile(t, dir, "movie|alpha|2001", testSchema, `{"title":"Alpha"}`)
	seedCacheFile(t, dir, "movie|beta|2002", testSchema, `{"title":"Beta"}`)

	var b1, b2 bytes.Buffer
	if _, err := Export(dir, &b1, testSchema, "test"); err != nil {
		t.Fatalf("first Export: %v", err)
	}
	if _, err := Export(dir, &b2, testSchema, "test"); err != nil {
		t.Fatalf("second Export: %v", err)
	}
	// Determinism is about the ENTRIES (sorted by Key, stable bytes), not
	// the whole byte stream: Export stamps a real ExportedAt, so the two
	// headers differ there and agree everywhere else.
	var g1, g2 Bundle
	if err := json.Unmarshal(b1.Bytes(), &g1); err != nil {
		t.Fatalf("first bundle does not parse: %v", err)
	}
	if err := json.Unmarshal(b2.Bytes(), &g2); err != nil {
		t.Fatalf("second bundle does not parse: %v", err)
	}
	if g1.Header.ExportedAt.IsZero() || g2.Header.ExportedAt.IsZero() {
		t.Fatalf("ExportedAt is zero, want a real export timestamp")
	}
	g1.Header.ExportedAt, g2.Header.ExportedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(g1, g2) {
		t.Fatalf("entry lists differ:\n%+v\n---\n%+v", g1, g2)
	}
}

// TestBundleSixteenHexNames proves the real cache filename rule (16 hex
// chars per metadata.cacheFileName): such a file is exported and is removed
// by Clear, while a stale 64-hex name is ignored by both.
func TestBundleSixteenHexNames(t *testing.T) {
	dir := t.TempDir()
	name := seedCacheFile(t, dir, "movie|sixteen|2001", testSchema, `{"title":"Sixteen"}`)
	if len(name) != 16+len(".json") {
		t.Fatalf("cache name %q is %d chars, want 16 hex + .json", name, len(name))
	}
	stale := strings.Repeat("ab", 32) + ".json" // 64 hex: the old wrong rule
	if err := os.WriteFile(filepath.Join(dir, stale), []byte(`{"key":"stale"}`), 0o644); err != nil {
		t.Fatalf("write stale fixture: %v", err)
	}

	var buf bytes.Buffer
	est, err := Export(dir, &buf, testSchema, "test")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if est.Entries != 1 {
		t.Fatalf("Export entries = %d, want 1 (the 16-hex file only)", est.Entries)
	}
	var got Bundle
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("exported bundle does not parse: %v", err)
	}
	if len(got.Entries) != 1 || got.Entries[0].File != name {
		t.Fatalf("bundle entries = %+v, want the single 16-hex file %q", got.Entries, name)
	}

	removed, err := Clear(dir)
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if removed != 1 {
		t.Fatalf("Clear removed %d, want 1 (the 16-hex file; stale 64-hex kept)", removed)
	}
	if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
		t.Fatalf("16-hex cache file was not removed")
	}
	if _, err := os.Stat(filepath.Join(dir, stale)); err != nil {
		t.Fatalf("stale 64-hex file touched: %v", err)
	}
}

func TestBundleFillMissingOnly(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	keys := []string{"movie|alpha|2001", "movie|beta|2002", "movie|gamma|2003"}
	for _, k := range keys {
		seedCacheFile(t, a, k, testSchema, "{\"title\":\""+k+"\"}")
	}
	var buf bytes.Buffer
	if _, err := Export(a, &buf, testSchema, "test"); err != nil {
		t.Fatalf("Export: %v", err)
	}

	// Pre-create one file in B with different content.
	preName := seedCacheFile(t, b, keys[0], testSchema, `{"title":"stale ogniwa"}`)
	prePath := filepath.Join(b, preName)
	before, err := os.ReadFile(prePath)
	if err != nil {
		t.Fatalf("read pre-created file: %v", err)
	}
	fi0, err := os.Stat(prePath)
	if err != nil {
		t.Fatalf("stat pre-created file: %v", err)
	}

	st, err := Import(b, bytes.NewReader(buf.Bytes()), testSchema)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if st.Present != 1 || st.Added != 2 || st.Total != 3 {
		t.Fatalf("Import stats = %+v, want Present=1 Added=2 Total=3", st)
	}
	after, err := os.ReadFile(prePath)
	if err != nil {
		t.Fatalf("read pre-created file after import: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("pre-existing file bytes changed")
	}
	fi1, err := os.Stat(prePath)
	if err != nil {
		t.Fatalf("stat after import: %v", err)
	}
	if !fi1.ModTime().Equal(fi0.ModTime()) {
		t.Fatalf("pre-existing file mtime changed: %v -> %v", fi0.ModTime(), fi1.ModTime())
	}
}

func TestBundleSchemaMismatch(t *testing.T) {
	dir := t.TempDir()
	name := cacheNameFor("movie|stale|2001")
	data := testBundleBytes(t, []Entry{
		{File: name, Key: "movie|stale|2001", Schema: testSchema + 1, Media: json.RawMessage(`{"title":"Stale"}`)},
	})
	st, err := Import(dir, bytes.NewReader(data), testSchema)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if st.SkippedSchema != 1 || st.Total != 1 || st.Added != 0 || st.Present != 0 || st.Invalid != 0 {
		t.Fatalf("Import stats = %+v, want SkippedSchema=1 Total=1 rest 0", st)
	}
	if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
		t.Fatalf("schema-mismatched entry wrote a file")
	}
	if c, err := Count(dir); err != nil || c != 0 {
		t.Fatalf("Count = %d, %v; want 0", c, err)
	}
}

func TestBundleInvalidEntries(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "cache")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	raw := json.RawMessage(`{"title":"X"}`)
	data := testBundleBytes(t, []Entry{
		{File: "../escape.json", Key: "evil", Schema: testSchema, Media: raw},
		{File: "not-a-cache-name.json", Key: "k2", Schema: testSchema, Media: raw},
		{File: cacheNameFor("k3"), Key: "", Schema: testSchema, Media: raw},
	})
	st, err := Import(dir, bytes.NewReader(data), testSchema)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if st.Invalid != 3 || st.Total != 3 || st.Added != 0 {
		t.Fatalf("Import stats = %+v, want Invalid=3 Total=3 Added=0", st)
	}
	// Nothing may escape dir: the parent must hold only the cache subdir.
	kids := dirChildNames(t, parent)
	if len(kids) != 1 || kids[0] != "cache" {
		t.Fatalf("parent dir gained files: %q", kids)
	}
	if _, err := os.Stat(filepath.Join(parent, "escape.json")); !os.IsNotExist(err) {
		t.Fatalf("escape file exists outside dir")
	}
	if len(dirChildNames(t, dir)) != 0 {
		t.Fatalf("cache dir not empty: %q", dirChildNames(t, dir))
	}
}

func TestBundleCorruptSourceSkipped(t *testing.T) {
	dir := t.TempDir()
	seedCacheFile(t, dir, "movie|good-a|2001", testSchema, `{"title":"A"}`)
	seedCacheFile(t, dir, "movie|good-b|2002", testSchema, `{"title":"B"}`)
	badName := cacheNameFor("movie|bad|2003")
	if err := os.WriteFile(filepath.Join(dir, badName), []byte(`{"key": oops, not json`), 0o644); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}

	var buf bytes.Buffer
	st, err := Export(dir, &buf, testSchema, "test")
	if err != nil {
		t.Fatalf("Export failed on corrupt entry: %v", err)
	}
	if st.Entries != 2 || st.Skipped != 1 {
		t.Fatalf("Export stats = %+v, want Entries=2 Skipped=1", st)
	}
	var got Bundle
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("exported bundle does not parse: %v", err)
	}
	if len(got.Entries) != 2 {
		t.Fatalf("bundle holds %d entries, want 2", len(got.Entries))
	}
}

func TestBundleClear(t *testing.T) {
	dir := t.TempDir()
	n1 := seedCacheFile(t, dir, "movie|keep-a|2001", testSchema, `{"title":"A"}`)
	n2 := seedCacheFile(t, dir, "movie|keep-b|2002", testSchema, `{"title":"B"}`)
	if err := os.WriteFile(filepath.Join(dir, "notes.json"), []byte(`{"keep":true}`), 0o644); err != nil {
		t.Fatalf("write notes.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".tmp-orphan-123.json"), []byte(`partial`), 0o644); err != nil {
		t.Fatalf("write tmp orphan: %v", err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "inner.json"), []byte(`{"in":"sub"}`), 0o644); err != nil {
		t.Fatalf("write sub file: %v", err)
	}

	removed, err := Clear(dir)
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if removed != 3 {
		t.Fatalf("Clear removed %d, want 3 (2 cache + 1 orphan)", removed)
	}
	for _, gone := range []string{n1, n2, ".tmp-orphan-123.json"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s was not removed", gone)
		}
	}
	keep, err := os.ReadFile(filepath.Join(dir, "notes.json"))
	if err != nil || string(keep) != `{"keep":true}` {
		t.Fatalf("non-matching notes.json touched: %q, %v", keep, err)
	}
	inner, err := os.ReadFile(filepath.Join(sub, "inner.json"))
	if err != nil || string(inner) != `{"in":"sub"}` {
		t.Fatalf("subdirectory touched: %q, %v", inner, err)
	}
}

func TestBundlePrivacyGuard(t *testing.T) {
	dir := t.TempDir()
	seedCacheFile(t, dir, "movie|url|2001", testSchema,
		`{"title":"Linked","poster":"https://image.example.com/poster.jpg","watch":"https://example.com/watch"}`)

	var buf bytes.Buffer
	if _, err := Export(dir, &buf, testSchema, "test"); err != nil {
		t.Fatalf("Export: %v", err)
	}
	s := buf.String()
	if re := regexp.MustCompile(`[A-Za-z]:\\`); re.MatchString(s) {
		t.Fatalf("bundle contains a Windows drive path")
	}
	if strings.Contains(s, "LOCALAPPDATA") {
		t.Fatalf("bundle mentions LOCALAPPDATA")
	}
	// The poster URL itself is legitimate metadata and must survive.
	if !strings.Contains(s, "https://image.example.com/poster.jpg") {
		t.Fatalf("poster URL lost from bundle")
	}
}

func TestBundleOversizeRejected(t *testing.T) {
	dir := t.TempDir()
	big := bytes.NewReader(bytes.Repeat([]byte("{"), maxBundleBytes+1))
	_, err := Import(dir, big, testSchema)
	if err == nil {
		t.Fatalf("oversize bundle accepted")
	}
	if !strings.Contains(err.Error(), "64 MB") {
		t.Fatalf("error does not name the limit: %v", err)
	}
	if len(dirChildNames(t, dir)) != 0 {
		t.Fatalf("oversize import wrote files: %q", dirChildNames(t, dir))
	}
}

// TestBundleImportNoReplace proves the import publish path never replaces
// an existing file: a pre-created target keeps its bytes and mtime and the
// entry reports Present, even though the bundle carries different media for
// the same key. The pre-created target makes the guarantee deterministic
// (no race window is needed to exercise it).
func TestBundleImportNoReplace(t *testing.T) {
	dir := t.TempDir()
	key := "movie|noreplace|2001"
	name := cacheNameFor(key)
	original := `{"key":"` + key + `","schema":3,"media":{"title":"ORIGINAL"}}`
	if err := os.WriteFile(filepath.Join(dir, name), []byte(original), 0o644); err != nil {
		t.Fatalf("write pre-created target: %v", err)
	}
	// Pin the mtime back so any replacement is visible even on
	// filesystems with coarse timestamp granularity.
	old := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(filepath.Join(dir, name), old, old); err != nil {
		t.Fatalf("chtimes pre-created target: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read pre-created target: %v", err)
	}

	data := testBundleBytes(t, []Entry{
		{File: name, Key: key, Schema: testSchema, Media: json.RawMessage(`{"title":"BUNDLE"}`)},
	})
	st, err := Import(dir, bytes.NewReader(data), testSchema)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if st.Present != 1 || st.Added != 0 || st.Total != 1 {
		t.Fatalf("Import stats = %+v, want Present=1 Added=0 Total=1", st)
	}
	after, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read target after import: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("pre-existing file bytes changed")
	}
	fi, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("stat target after import: %v", err)
	}
	if !fi.ModTime().Equal(old) {
		t.Fatalf("pre-existing file mtime changed: %v -> %v", old, fi.ModTime())
	}
	for _, n := range dirChildNames(t, dir) {
		if strings.HasPrefix(n, ".tmp-") {
			t.Fatalf("temp file left behind: %q", n)
		}
	}
}

// TestBundleReparsePointDirRefused proves Import/Clear/Export refuse a cache
// directory that is itself a reparse point (symlink or Windows junction)
// instead of operating through it.
func TestBundleReparsePointDirRefused(t *testing.T) {
	parent := t.TempDir()
	real := filepath.Join(parent, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatalf("mkdir real: %v", err)
	}
	seedCacheFile(t, real, "movie|behind-link|2001", testSchema, `{"title":"Behind"}`)
	link := filepath.Join(parent, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("os.Symlink refused (no privilege/Developer Mode?): %v", err)
	}

	var buf bytes.Buffer
	if _, err := Export(link, &buf, testSchema, "test"); err == nil {
		t.Fatalf("Export through a reparse-point dir succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "reparse-point") {
		t.Fatalf("Export error = %v, want a reparse-point refusal", err)
	}
	if n, err := Clear(link); err == nil {
		t.Fatalf("Clear through a reparse-point dir succeeded (removed %d), want refusal", n)
	} else if !strings.Contains(err.Error(), "reparse-point") {
		t.Fatalf("Clear error = %v, want a reparse-point refusal", err)
	}
	data := testBundleBytes(t, []Entry{
		{File: cacheNameFor("movie|new|2002"), Key: "movie|new|2002", Schema: testSchema, Media: json.RawMessage(`{"title":"New"}`)},
	})
	if _, err := Import(link, bytes.NewReader(data), testSchema); err == nil {
		t.Fatalf("Import through a reparse-point dir succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "reparse-point") {
		t.Fatalf("Import error = %v, want a reparse-point refusal", err)
	}
	// The real directory behind the link must be untouched.
	if c, err := Count(real); err != nil || c != 1 {
		t.Fatalf("real dir touched: Count = %d, %v; want 1", c, err)
	}
}

// TestBundleReparsePointEntrySkipped proves a cache entry that is itself a
// reparse point is never read (Export) nor deleted (Clear) through the
// link, while a neighbouring real entry is handled normally.
func TestBundleReparsePointEntrySkipped(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.json")
	if err := os.WriteFile(secret, []byte(`{"key":"movie|secret|2001","schema":3,"media":{"title":"Secret"}}`), 0o644); err != nil {
		t.Fatalf("write outside fixture: %v", err)
	}
	linkName := cacheNameFor("movie|secret|2001")
	if err := os.Symlink(secret, filepath.Join(dir, linkName)); err != nil {
		t.Skipf("os.Symlink refused (no privilege/Developer Mode?): %v", err)
	}
	seedCacheFile(t, dir, "movie|good|2002", testSchema, `{"title":"Good"}`)

	var buf bytes.Buffer
	est, err := Export(dir, &buf, testSchema, "test")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if est.Entries != 1 || est.Skipped != 1 {
		t.Fatalf("Export stats = %+v, want Entries=1 Skipped=1", est)
	}
	var got Bundle
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("exported bundle does not parse: %v", err)
	}
	for _, e := range got.Entries {
		if e.Key == "movie|secret|2001" {
			t.Fatalf("bundle carries the reparse-point entry")
		}
	}

	removed, err := Clear(dir)
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if removed != 1 {
		t.Fatalf("Clear removed %d, want 1 (the good file; the link kept)", removed)
	}
	if _, err := os.Lstat(filepath.Join(dir, linkName)); err != nil {
		t.Fatalf("reparse-point entry was deleted through the link: %v", err)
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("outside link target touched: %v", err)
	}
}

// TestBundleExportEntryCap proves an entry larger than 4 MB is skipped per
// the cap instead of being read unbounded into memory.
func TestBundleExportEntryCap(t *testing.T) {
	dir := t.TempDir()
	seedCacheFile(t, dir, "movie|good|2001", testSchema, `{"title":"Good"}`)
	bigName := cacheNameFor("movie|big|2002")
	if err := os.WriteFile(filepath.Join(dir, bigName), bytes.Repeat([]byte("x"), (4<<20)+1), 0o644); err != nil {
		t.Fatalf("write oversized fixture: %v", err)
	}

	var buf bytes.Buffer
	est, err := Export(dir, &buf, testSchema, "test")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if est.Entries != 1 || est.Skipped != 1 {
		t.Fatalf("Export stats = %+v, want Entries=1 Skipped=1", est)
	}
}
