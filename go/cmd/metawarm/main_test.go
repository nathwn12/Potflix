package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"potflix/internal/metadata"
)

// seedLib builds a tiny fake library:
//   - the same episode in two folders (must dedupe to one key)
//   - a second episode of the same show (distinct key)
//   - a trailer under Extras (KindUnknown: skipped)
//   - a movie (distinct key)
func seedLib(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := []string{
		filepath.Join(root, "A", "Breaking.Bad.S01E01.mkv"),
		filepath.Join(root, "B", "Breaking.Bad.S01E01.mkv"),
		filepath.Join(root, "B", "Breaking.Bad.S01E02.mkv"),
		filepath.Join(root, "ShowName", "Extras", "SomeShow trailer.mkv"),
		filepath.Join(root, "Movies", "The.Matrix.1999.mkv"),
	}
	for _, f := range files {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A non-video file must never be collected.
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestBuildPlanSkipsUnknownAndDedupes(t *testing.T) {
	root := seedLib(t)
	files, err := collectFiles([]string{root}, 0)
	if err != nil {
		t.Fatalf("collectFiles: %v", err)
	}
	if len(files) != 5 {
		t.Fatalf("files = %d, want 5 (non-video excluded): %v", len(files), files)
	}
	plans, groups := buildPlan(files)
	skipped := 0
	for _, p := range plans {
		if p.skipped {
			skipped++
			if !strings.HasSuffix(p.path, "SomeShow trailer.mkv") {
				t.Errorf("skipped unexpected file: %s", p.path)
			}
		}
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1", skipped)
	}
	// 5 files - 1 skipped = 4 queries, but the S01E01 pair shares a key:
	// groups = {S01E01, S01E02, movie} = 3.
	if len(groups) != 3 {
		t.Fatalf("unique groups = %d, want 3", len(groups))
		for _, g := range groups {
			t.Logf("key=%s query=%+v paths=%v", g.key, g.query, g.paths)
		}
	}
	for _, g := range groups {
		if len(g.paths) == 2 && !strings.Contains(g.paths[0], "S01E01") {
			t.Errorf("shared group is not the S01E01 pair: %v", g.paths)
		}
	}
}

func TestFetchAllLooksUpOncePerUniqueKey(t *testing.T) {
	root := seedLib(t)
	files, err := collectFiles([]string{root}, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, groups := buildPlan(files)
	var calls atomic.Int32
	stub := func(ctx context.Context, q metadata.Query) (metadata.Resolution, error) {
		calls.Add(1)
		return metadata.Resolution{Found: true}, nil
	}
	results := fetchAll(groups, stub, 3, nil)
	if got := calls.Load(); got != int32(len(groups)) {
		t.Errorf("lookup calls = %d, want %d (once per unique key)", got, len(groups))
	}
	if len(results) != len(groups) {
		t.Errorf("results = %d, want %d", len(results), len(groups))
	}
}

func TestDryRunFetchesNothingAndWritesReport(t *testing.T) {
	root := seedLib(t)
	local := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)
	t.Setenv("OPENCODE_SESSION_ID", "test-session")
	cache := t.TempDir()

	var out, errb strings.Builder
	code := realMain([]string{"-dry", "-roots", root, "-cache", cache}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errb.String())
	}
	// -dry fetches nothing: the cache dir must stay empty.
	entries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("cache dir has %d entries after -dry, want 0", len(entries))
	}
	// Report is written under %LOCALAPPDATA%, never in the repo.
	report := filepath.Join(local, "Potflix", "inventory", "warm-report--hold--test-session.txt")
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("report lines = %d, want 5:\n%s", len(lines), string(data))
	}
	statuses := map[string]int{}
	for _, ln := range lines {
		st, path, ok := strings.Cut(ln, "\t")
		if !ok || path == "" {
			t.Errorf("malformed report line: %q", ln)
			continue
		}
		statuses[st]++
		if !strings.HasPrefix(path, root) {
			t.Errorf("report path escapes the walked root: %q", path)
		}
	}
	if statuses[statusSkipped] != 1 || statuses[statusDry] != 4 {
		t.Errorf("statuses = %v, want map[dry:4 skipped:1]", statuses)
	}
	summary := out.String()
	for _, want := range []string{"metawarm summary:", "files=5", "skipped(unknown)=1", "unique=3", "report: " + report} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}
}

func TestOfflineRunIsCacheOnlyAndWritesReport(t *testing.T) {
	root := seedLib(t)
	local := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)
	t.Setenv("OPENCODE_SESSION_ID", "offline-case")

	var out, errb strings.Builder
	code := realMain([]string{"-offline", "-roots", root, "-cache", t.TempDir()}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errb.String())
	}
	report := filepath.Join(local, "Potflix", "inventory", "warm-report--hold--offline-case.txt")
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	// Empty cache + offline: every fetched file is a miss, never an error.
	statuses := map[string]int{}
	for _, ln := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		st, _, _ := strings.Cut(ln, "\t")
		statuses[st]++
	}
	if statuses[statusNotFound] != 4 || statuses[statusSkipped] != 1 || statuses[statusError] != 0 {
		t.Errorf("statuses = %v, want map[not-found:4 skipped:1]", statuses)
	}
	summary := out.String()
	for _, want := range []string{"hits=0", "missing=4"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}
}

func TestEmptyRootsErrorsWithUsage(t *testing.T) {
	var out, errb strings.Builder
	if code := realMain([]string{}, &out, &errb); code == 0 {
		t.Error("exit = 0 with no roots, want non-zero")
	}
	if !strings.Contains(errb.String(), "usage: metawarm") {
		t.Errorf("stderr missing usage:\n%s", errb.String())
	}
}

func TestUnwalkableRootIsFatal(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	var out, errb strings.Builder
	missing := filepath.Join(t.TempDir(), "no-such-root")
	if code := realMain([]string{"-roots", missing}, &out, &errb); code == 0 {
		t.Error("exit = 0 with unwalkable root, want non-zero")
	}
}
