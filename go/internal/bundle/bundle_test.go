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

	"potflix/internal/metadata"
)

const testSchema = 3

// cacheNameFor restates the 16-hex cache filename rule independently of the
// package under test; TestBundleCacheNameRuleMatchesMetadata pins it against
// the metadata package's own export.
func cacheNameFor(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:16] + ".json"
}

// seedLiveEntry stores one live cache entry the way the resolver does: the
// store record is the cache envelope's bytes (metadata.CachePutRaw), so media
// is opaque here exactly as it is to the store.
func seedLiveEntry(t *testing.T, dir, key string, schema int, media string) {
	t.Helper()
	payload, err := json.Marshal(cacheRecord{Key: key, Schema: schema, Media: json.RawMessage(media)})
	if err != nil {
		t.Fatalf("marshal live seed %q: %v", key, err)
	}
	if err := metadata.CachePutRaw(dir, key, payload); err != nil {
		t.Fatalf("seed live entry %q: %v", key, err)
	}
}

// seedCacheFile writes one LEGACY loose per-key cache file (the pre-store
// layout, still the recovery copy migration imports): a 16-hex .json name with
// a cache-shaped body.
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

// liveValue returns a copy of the stored bytes for key in dir's live cache.
func liveValue(t *testing.T, dir, key string) ([]byte, bool) {
	t.Helper()
	var out []byte
	found := false
	err := metadata.CacheForEach(dir, func(k string, v []byte) bool {
		if k == key {
			out = append([]byte(nil), v...)
			found = true
			return false
		}
		return true
	})
	if err != nil {
		t.Fatalf("CacheForEach(%s): %v", dir, err)
	}
	return out, found
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

// TestBundleRoundTrip is the integration contract the store rework left open:
// export the LIVE cache, import into an empty live cache, and the imported
// entries are live and counted there - no file in either directory is written
// or read.
func TestBundleRoundTrip(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	keys := []string{"movie|alpha|2001", "series|beta|2020|2", "movie|gamma|1999"}
	media := []string{
		`{"source":"tvmaze","title":"Alpha","artwork":"https://image.example.com/alpha.jpg"}`,
		`{"source":"tvmaze","title":"Beta","genres":"Drama"}`,
		`{"source":"itunes","title":"Gamma","length":"1h 30m"}`,
	}
	for i := range keys {
		seedLiveEntry(t, a, keys[i], testSchema, media[i])
	}
	if c, err := Count(a); err != nil || c != 3 {
		t.Fatalf("Count(A) = %d, %v; want 3", c, err)
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
		// Live for the reader that matters: the resolver's own gates
		// (schema, key, non-empty Source).
		if !metadata.CacheLive(b, keys[i]) {
			t.Fatalf("imported entry %q is not live in the destination cache", keys[i])
		}
		// Byte-identical media through the store, like the loose files were.
		da, ok := liveValue(t, a, keys[i])
		if !ok {
			t.Fatalf("source entry %q vanished from the live cache", keys[i])
		}
		db, ok := liveValue(t, b, keys[i])
		if !ok {
			t.Fatalf("imported entry %q missing from the destination cache", keys[i])
		}
		if !bytes.Equal(da, db) {
			t.Fatalf("entry %q differs after round-trip:\n%s\n%s", keys[i], da, db)
		}
	}
	// Visible to the resolver's own probe path too, immediately afterwards:
	// the Settings import runs against the resolver's live cache dir.
	res := metadata.New(metadata.Options{Dir: b, Offline: true})
	for i := range keys {
		if !res.ProbeCache(keys[i]) {
			t.Fatalf("ProbeCache(%q) missed right after the import", keys[i])
		}
	}
	// Neither directory gained a cache file: the cache is the store beside it.
	for _, dir := range []string{a, b} {
		for _, n := range dirChildNames(t, dir) {
			if validCacheName(n) {
				t.Fatalf("bundle wrote a loose cache file %q into %s", n, dir)
			}
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

// TestBundleEntryFileNameFollowsMetadataRule proves each exported entry
// carries the 16-hex legacy cache name derived from its key
// (metadata.CacheFileNameFor). The store keys entries by the raw cache key, so
// the bundle's "file" field is derived rather than copied - and stays the name
// a pre-rework bundle carries, which is what keeps the FILE format compatible.
func TestBundleEntryFileNameFollowsMetadataRule(t *testing.T) {
	if got := cacheNameFor("movie|tracked|2001"); got != metadata.CacheFileNameFor("movie|tracked|2001") {
		t.Fatalf("local name rule %q disagrees with metadata.CacheFileNameFor", got)
	}
	dir := t.TempDir()
	key := "movie|sixteen|2001"
	seedLiveEntry(t, dir, key, testSchema, `{"source":"tvmaze","title":"Sixteen"}`)

	var buf bytes.Buffer
	est, err := Export(dir, &buf, testSchema, "test")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if est.Entries != 1 {
		t.Fatalf("Export entries = %d, want 1", est.Entries)
	}
	var got Bundle
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("exported bundle does not parse: %v", err)
	}
	if len(got.Entries) != 1 {
		t.Fatalf("bundle holds %d entries, want 1", len(got.Entries))
	}
	want := metadata.CacheFileNameFor(key)
	if got.Entries[0].File != want {
		t.Fatalf("entry file = %q, want %q", got.Entries[0].File, want)
	}
	if !validCacheName(want) || len(want) != 16+len(".json") {
		t.Fatalf("entry file %q is not 16-hex + .json", want)
	}
}

// TestBundleLegacyBundleFileStillImports proves a bundle file written before
// the cache rework imports unchanged: the envelope and the entry fields are the
// same, and the entry lands in the LIVE cache (no loose file, immediately live).
func TestBundleLegacyBundleFileStillImports(t *testing.T) {
	key := "series|legacy|2016|1"
	file := cacheNameFor(key)
	media := json.RawMessage(`{"source":"tvmaze","title":"Legacy","genres":"Drama"}`)
	legacy := Bundle{
		Header: Header{
			Format:     FormatName,
			Version:    FormatVersion,
			Schema:     testSchema,
			AppVersion: "potflix-legacy",
			ExportedAt: time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC),
			Count:      1,
			Note:       NoteText,
		},
		Entries: []Entry{{File: file, Key: key, Schema: testSchema, Media: media}},
	}
	raw, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatalf("marshal legacy bundle: %v", err)
	}
	raw = append(raw, '\n')
	path := filepath.Join(t.TempDir(), "potflix-metadata-legacy.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write legacy bundle: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open legacy bundle: %v", err)
	}
	defer f.Close()

	dst := t.TempDir()
	st, err := Import(dst, f, testSchema)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if st.Added != 1 || st.Total != 1 || st.Present != 0 || st.SkippedSchema != 0 || st.Invalid != 0 {
		t.Fatalf("Import stats = %+v, want Added=1 Total=1 rest 0", st)
	}
	if !metadata.CacheLive(dst, key) {
		t.Fatalf("legacy-bundle entry %q is not live after import", key)
	}
	got, ok := liveValue(t, dst, key)
	if !ok {
		t.Fatalf("legacy-bundle entry %q missing from the live cache", key)
	}
	var rec cacheRecord
	if err := json.Unmarshal(got, &rec); err != nil {
		t.Fatalf("stored record does not parse: %v", err)
	}
	if rec.Key != key || rec.Schema != testSchema || string(rec.Media) != string(media) {
		t.Fatalf("stored record = %+v, want key %q schema %d media %s", rec, key, testSchema, media)
	}
	if _, ok := liveValue(t, dst, "other"); ok {
		t.Fatalf("import invented an extra entry")
	}
}

func TestBundleFillMissingOnly(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	keys := []string{"movie|alpha|2001", "movie|beta|2002", "movie|gamma|2003"}
	for _, k := range keys {
		seedLiveEntry(t, a, k, testSchema, `{"source":"tvmaze","title":"`+k+`"}`)
	}
	var buf bytes.Buffer
	if _, err := Export(a, &buf, testSchema, "test"); err != nil {
		t.Fatalf("Export: %v", err)
	}

	// Pre-populate B with a DIFFERENT record for the first key.
	seedLiveEntry(t, b, keys[0], testSchema, `{"source":"tvmaze","title":"stale ogniwa"}`)
	before, ok := liveValue(t, b, keys[0])
	if !ok {
		t.Fatalf("pre-seeded entry missing")
	}

	st, err := Import(b, bytes.NewReader(buf.Bytes()), testSchema)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if st.Present != 1 || st.Added != 2 || st.Total != 3 {
		t.Fatalf("Import stats = %+v, want Present=1 Added=2 Total=3", st)
	}
	after, ok := liveValue(t, b, keys[0])
	if !ok {
		t.Fatalf("pre-existing entry vanished from the store")
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("pre-existing stored bytes replaced:\n%s\n%s", before, after)
	}
	if c, err := Count(b); err != nil || c != 3 {
		t.Fatalf("Count(B) = %d, %v; want 3", c, err)
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
	if metadata.CacheHas(dir, "movie|stale|2001") {
		t.Fatalf("schema-mismatched entry was stored")
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

// TestBundleCorruptSourceSkipped proves a stored record that does not decode as
// a cache entry is counted Skipped and never fails the export, and that a
// corrupt LEGACY file (no key) never becomes an entry at all: the store's
// migration is the only reader of the old layout now.
func TestBundleCorruptSourceSkipped(t *testing.T) {
	dir := t.TempDir()
	seedLiveEntry(t, dir, "movie|good-a|2001", testSchema, `{"source":"tvmaze","title":"A"}`)
	seedLiveEntry(t, dir, "movie|good-b|2002", testSchema, `{"source":"tvmaze","title":"B"}`)
	if err := metadata.CachePutRaw(dir, "movie|broken|2003", []byte(`{"key": oops, not json`)); err != nil {
		t.Fatalf("seed undecodable record: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, cacheNameFor("movie|legacy-corrupt|2004")),
		[]byte(`{"key": oops, not json`), 0o644); err != nil {
		t.Fatalf("write corrupt legacy file: %v", err)
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
	for _, e := range got.Entries {
		if strings.Contains(e.Key, "broken") || strings.Contains(e.Key, "corrupt") {
			t.Fatalf("bundle carries a record that does not decode: %+v", e)
		}
	}
}

// TestBundleClear proves Clear empties the LIVE cache (metadata's store), leaves
// everything that is not a cache entry alone, and takes the legacy cache files
// with it so the next read cannot re-import what was cleared.
func TestBundleClear(t *testing.T) {
	dir := t.TempDir()
	seedLiveEntry(t, dir, "movie|keep-a|2001", testSchema, `{"source":"tvmaze","title":"A"}`)
	seedLiveEntry(t, dir, "movie|keep-b|2002", testSchema, `{"source":"tvmaze","title":"B"}`)
	legacy := seedCacheFile(t, dir, "movie|legacy-c|2003", testSchema, `{"title":"C"}`)
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
	// Two titles: the stored entries. The legacy file is not one of them -
	// migration imports only into an EMPTY store, so a cache that already
	// holds entries never picks up a loose file dropped beside it - but Clear
	// still removes it, so the next read cannot resurrect it.
	if removed != 2 {
		t.Fatalf("Clear cleared %d titles, want 2", removed)
	}
	if metadata.CacheHas(dir, "movie|keep-a|2001") || metadata.CacheHas(dir, "movie|keep-b|2002") ||
		metadata.CacheHas(dir, "movie|legacy-c|2003") {
		t.Fatalf("cleared titles still report present in the live cache")
	}
	n := 0
	if err := metadata.CacheForEach(dir, func(string, []byte) bool { n++; return true }); err != nil {
		t.Fatalf("CacheForEach after Clear: %v", err)
	}
	if n != 0 {
		t.Fatalf("live cache still holds %d entries, want 0", n)
	}
	if c, err := Count(dir); err != nil || c != 0 {
		t.Fatalf("Count after Clear = %d, %v; want 0", c, err)
	}
	for _, gone := range []string{legacy, ".tmp-orphan-123.json"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s was not removed", gone)
		}
	}
	if keep, err := os.ReadFile(filepath.Join(dir, "notes.json")); err != nil || string(keep) != `{"keep":true}` {
		t.Fatalf("non-cache notes.json touched: %q, %v", keep, err)
	}
	if inner, err := os.ReadFile(filepath.Join(sub, "inner.json")); err != nil || string(inner) != `{"in":"sub"}` {
		t.Fatalf("subdirectory touched: %q, %v", inner, err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("Clear removed the cache directory itself: %v", err)
	}
}

func TestBundlePrivacyGuard(t *testing.T) {
	dir := t.TempDir()
	seedLiveEntry(t, dir, "movie|url|2001", testSchema,
		`{"source":"tvmaze","title":"Linked","artwork":"https://image.example.com/poster.jpg","movieUrl":"https://example.com/watch"}`)

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
	if c, err := Count(dir); err != nil || c != 0 {
		t.Fatalf("Count after rejected import = %d, %v; want 0", c, err)
	}
}

// TestBundleImportNoReplace proves the import publish path never replaces a
// stored entry: a key the live cache already holds keeps its bytes and reports
// Present, even though the bundle carries different media for the same key.
func TestBundleImportNoReplace(t *testing.T) {
	dir := t.TempDir()
	key := "movie|noreplace|2001"
	name := cacheNameFor(key)
	original := `{"source":"tvmaze","title":"ORIGINAL"}`
	seedLiveEntry(t, dir, key, testSchema, original)
	before, ok := liveValue(t, dir, key)
	if !ok {
		t.Fatalf("pre-seeded entry missing")
	}

	data := testBundleBytes(t, []Entry{
		{File: name, Key: key, Schema: testSchema, Media: json.RawMessage(`{"source":"tvmaze","title":"BUNDLE"}`)},
	})
	st, err := Import(dir, bytes.NewReader(data), testSchema)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if st.Present != 1 || st.Added != 0 || st.Total != 1 {
		t.Fatalf("Import stats = %+v, want Present=1 Added=0 Total=1", st)
	}
	after, ok := liveValue(t, dir, key)
	if !ok {
		t.Fatalf("pre-existing entry vanished")
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("pre-existing stored bytes replaced:\n%s\n%s", before, after)
	}
	for _, n := range dirChildNames(t, dir) {
		if strings.HasPrefix(n, ".tmp-") {
			t.Fatalf("temp file left behind: %q", n)
		}
	}
}

// TestBundleForeignFilesIgnored proves a file in the cache directory that is not
// a cache record - the old notes.json case - is neither imported into the live
// cache nor exported, and that Clear leaves it alone.
func TestBundleForeignFilesIgnored(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.json"), []byte(`{"keep":true}`), 0o644); err != nil {
		t.Fatalf("write notes.json: %v", err)
	}
	seedLiveEntry(t, dir, "movie|real|2001", testSchema, `{"source":"tvmaze","title":"Real"}`)

	var buf bytes.Buffer
	est, err := Export(dir, &buf, testSchema, "test")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if est.Entries != 1 || est.Skipped != 0 {
		t.Fatalf("Export stats = %+v, want Entries=1 Skipped=0 (notes.json is not a cache record)", est)
	}
	if _, err := Clear(dir); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if keep, err := os.ReadFile(filepath.Join(dir, "notes.json")); err != nil || string(keep) != `{"keep":true}` {
		t.Fatalf("non-cache notes.json touched: %q, %v", keep, err)
	}
}

// TestBundleOversizeLegacyEntryNeverBecomesAnEntry proves the store's own
// per-record cap is the size guard on the read path: a legacy file larger than
// a record is refused by the store, so it is neither cached nor exported, and
// the bundle never reads it whole.
func TestBundleOversizeLegacyEntryNeverBecomesAnEntry(t *testing.T) {
	dir := t.TempDir()
	seedLiveEntry(t, dir, "movie|good|2001", testSchema, `{"source":"tvmaze","title":"Good"}`)
	key := "movie|big|2002"
	if err := os.WriteFile(filepath.Join(dir, cacheNameFor(key)), bytes.Repeat([]byte("x"), (4<<20)+1), 0o644); err != nil {
		t.Fatalf("write oversized fixture: %v", err)
	}

	var buf bytes.Buffer
	est, err := Export(dir, &buf, testSchema, "test")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if est.Entries != 1 || est.Skipped != 0 {
		t.Fatalf("Export stats = %+v, want Entries=1 Skipped=0", est)
	}
	if metadata.CacheHas(dir, key) {
		t.Fatalf("oversize legacy file was stored as a cache entry")
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
		t.Fatalf("Clear through a reparse-point dir succeeded (cleared %d), want refusal", n)
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
	if !metadata.CacheHas(real, "movie|behind-link|2001") {
		t.Fatalf("the entry behind the link is not in the live cache")
	}
}

// TestBundleClearKeepsReparsePointEntries proves Clear never deletes a cache
// file through a reparse point, while a neighbouring real legacy cache file is
// removed as usual. The linked file's title is imported like any other legacy
// file (the store's migration reads it), which is why the cleared count covers
// both titles - what must survive is the link and its target.
func TestBundleClearKeepsReparsePointEntries(t *testing.T) {
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
	good := seedCacheFile(t, dir, "movie|good|2002", testSchema, `{"title":"Good"}`)

	removed, err := Clear(dir)
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if removed != 2 {
		t.Fatalf("Clear cleared %d titles, want 2 (the real file and the linked one)", removed)
	}
	if _, err := os.Lstat(filepath.Join(dir, linkName)); err != nil {
		t.Fatalf("reparse-point entry was deleted through the link: %v", err)
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("outside link target touched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, good)); !os.IsNotExist(err) {
		t.Fatalf("neighbouring legacy cache file was not removed")
	}
}

// TestBundleEmptyDirRefused proves every entry point refuses an empty directory
// instead of silently resolving it to the metadata package's default cache
// directory (the live one).
func TestBundleEmptyDirRefused(t *testing.T) {
	var buf bytes.Buffer
	if _, err := Export("", &buf, testSchema, "test"); err == nil {
		t.Fatalf("Export with no directory succeeded")
	}
	if _, err := Count(""); err == nil {
		t.Fatalf("Count with no directory succeeded")
	}
	if _, err := Clear(""); err == nil {
		t.Fatalf("Clear with no directory succeeded (this would clear the live cache)")
	}
	data := testBundleBytes(t, []Entry{{File: cacheNameFor("k"), Key: "k", Schema: testSchema, Media: json.RawMessage(`{}`)}})
	if _, err := Import("", bytes.NewReader(data), testSchema); err == nil {
		t.Fatalf("Import with no directory succeeded")
	}
}

// TestBundleMissingDirIsEmpty proves the store layout's absence semantics: a
// cache directory that was never created (or was removed by the Clear-metadata
// action) holds no entries, so Count reports 0 and Export writes an empty
// bundle rather than failing - the cache is the store file beside the
// directory, and it may well be there while the directory is not.
func TestBundleMissingDirIsEmpty(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "meta")
	if c, err := Count(dir); err != nil || c != 0 {
		t.Fatalf("Count(missing) = %d, %v; want 0", c, err)
	}
	if removed, err := Clear(dir); err != nil || removed != 0 {
		t.Fatalf("Clear(missing) = %d, %v; want 0", removed, err)
	}
	var buf bytes.Buffer
	est, err := Export(dir, &buf, testSchema, "test")
	if err != nil {
		t.Fatalf("Export(missing): %v", err)
	}
	if est.Entries != 0 {
		t.Fatalf("Export(missing) entries = %d, want 0", est.Entries)
	}
}
