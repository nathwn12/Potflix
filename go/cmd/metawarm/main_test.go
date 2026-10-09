package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"potflix/internal/metadata"
)

func TestDryRunFetchesNothingAndWritesReport(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{
		filepath.Join(root, "A", "Breaking.Bad.S01E01.mkv"),
		filepath.Join(root, "B", "Breaking.Bad.S01E02.mkv"),
	} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
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
	if len(lines) != 2 {
		t.Fatalf("report lines = %d, want 2:\n%s", len(lines), string(data))
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
	if statuses[statusDry] != 2 {
		t.Errorf("statuses = %v, want map[dry:2]", statuses)
	}
	summary := out.String()
	for _, want := range []string{"metawarm summary:", "files=2", "skipped(unknown)=0", "unique=2", "denied=0", "report: " + report} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}
}

func TestOfflineRunIsCacheOnlyAndWritesReport(t *testing.T) {
	root := t.TempDir()
	show := filepath.Join(root, "Breaking.Bad.S01E01.mkv")
	if err := os.WriteFile(show, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
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
	// Empty cache + offline: the fetched file is a miss, never an error.
	statuses := map[string]int{}
	for _, ln := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		st, _, _ := strings.Cut(ln, "\t")
		statuses[st]++
	}
	if statuses[statusNotFound] != 1 || statuses[statusSkipped] != 0 || statuses[statusError] != 0 {
		t.Errorf("statuses = %v, want map[not-found:1]", statuses)
	}
	summary := out.String()
	for _, want := range []string{"hits=0", "missing=1", "denied=0"} {
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

// TestDeniedRootDrySummaryIncludesDenied proves one unreadable root is never
// fatal when another root reads fine: denied=1 flows through realMain into
// the -dry summary (exit 0, fetches nothing).
func TestDeniedRootDrySummaryIncludesDenied(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "good.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	local := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)
	t.Setenv("OPENCODE_SESSION_ID", "denied-case")
	cache := t.TempDir()

	var out, errb strings.Builder
	code := realMain([]string{"-dry", "-roots", root + ";" + missing, "-cache", cache}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errb.String())
	}
	if summary := out.String(); !strings.Contains(summary, "denied=1") {
		t.Errorf("summary missing denied=1:\n%s", summary)
	}
	entries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("cache dir has %d entries after -dry, want 0", len(entries))
	}
}

// blockSource is a ctx-aware fake resolver source for cancel tests: the
// first call signals started, then blocks until ctx is done or release
// closes.
type blockSource struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
	once    atomic.Bool
}

func (b *blockSource) Name() string { return "fake" }

func (b *blockSource) Find(ctx context.Context, q metadata.Query) (*metadata.Media, error) {
	b.calls.Add(1)
	if b.once.CompareAndSwap(false, true) {
		close(b.started)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.release:
	}
	return &metadata.Media{Source: "fake", SourceID: "1", Kind: q.Kind, Title: q.Title, Year: q.Year, FetchedAt: time.Now().UTC()}, nil
}

func seedWarmRoot(t *testing.T, names ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, n := range names {
		f := filepath.Join(root, n)
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestCancelledRunExitsZeroWithCancelledAndKeys proves a cancelled metawarm
// run exits 0, its summary contains "cancelled", and the key line is present
// alongside every existing field.
func TestCancelledRunExitsZeroWithCancelledAndKeys(t *testing.T) {
	root := seedWarmRoot(t, filepath.Join("A", "ShowA.S01E01.mkv"), filepath.Join("B", "ShowB.S01E01.mkv"))
	local := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)
	t.Setenv("OPENCODE_SESSION_ID", "cancel-case")
	cache := t.TempDir()
	index := filepath.Join(t.TempDir(), "scanindex.json")

	orig := newResolver
	defer func() { newResolver = orig }()
	blk := &blockSource{started: make(chan struct{}), release: make(chan struct{})}
	newResolver = func(dir string, offline bool) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Offline: offline, Sources: []metadata.Source{blk}})
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var out, errb strings.Builder
	var code int
	go func() {
		defer close(done)
		code = realMainWithContext(ctx, []string{"-roots", root, "-cache", cache, "-index", index}, &out, &errb)
	}()
	select {
	case <-blk.started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("blocking provider never started")
	}
	cancelAt := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled metawarm did not return")
	}
	t.Logf("metawarm cancel latency=%dms", time.Since(cancelAt).Milliseconds())
	if code != 0 {
		t.Errorf("exit = %d, want 0 for cancelled run (stderr=%s)", code, errb.String())
	}
	summary := out.String()
	for _, want := range []string{"cancelled", "keys:", "files=", "folders:", "report: "} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}
}

// TestSummaryGainsKeyLine keeps every existing field while adding the key
// line, e.g. keys: 2 (1 cached, 1 fetched, 0 not found, 0 failed).
func TestSummaryGainsKeyLine(t *testing.T) {
	root := seedWarmRoot(t, filepath.Join("A", "ShowA.S01E01.mkv"))
	local := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)
	t.Setenv("OPENCODE_SESSION_ID", "keys-case")
	cache := t.TempDir()

	var out, errb strings.Builder
	code := realMain([]string{"-offline", "-roots", root, "-cache", cache}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errb.String())
	}
	summary := out.String()
	for _, want := range []string{"files=", "folders:", "keys:", "cached", "fetched", "not found"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}
}

// TestCancelledWalkRecordsNothingInIndex proves F1 on the metawarm path: a
// run whose walk is cancelled records NOTHING in the index, so a later scan
// cannot skip files that were never walked. The context is already
// cancelled when the walk starts; the run still exits 0 with "cancelled".
func TestCancelledWalkRecordsNothingInIndex(t *testing.T) {
	root := seedWarmRoot(t, filepath.Join("A", "ShowA.S01E01.mkv"), filepath.Join("B", "ShowB.S01E01.mkv"))
	local := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)
	t.Setenv("OPENCODE_SESSION_ID", "walk-cancel-case")
	cache := t.TempDir()
	index := filepath.Join(t.TempDir(), "scanindex.json")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // walk starts already cancelled: partial results, nothing walked
	var out, errb strings.Builder
	code := realMainWithContext(ctx, []string{"-roots", root, "-cache", cache, "-index", index}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 for cancelled run (stderr=%s)", code, errb.String())
	}
	if summary := out.String(); !strings.Contains(summary, "cancelled") {
		t.Errorf("summary missing cancelled:\n%s", summary)
	}
	if fi, err := os.Stat(index); !os.IsNotExist(err) {
		if err == nil {
			t.Errorf("index file exists (%d bytes) after a cancelled walk, want nothing recorded", fi.Size())
		} else {
			t.Errorf("stat index: %v", err)
		}
	}
}

// TestCancelledFetchIsNotAnError proves F5 on the metawarm path: fetches
// that end because the context was cancelled are incomplete, never
// failures: the summary reports errors=0 and the per-file report carries no
// error lines.
func TestCancelledFetchIsNotAnError(t *testing.T) {
	root := seedWarmRoot(t, filepath.Join("A", "ShowA.S01E01.mkv"), filepath.Join("B", "ShowB.S01E01.mkv"))
	local := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)
	t.Setenv("OPENCODE_SESSION_ID", "fetch-cancel-case")
	cache := t.TempDir()
	index := filepath.Join(t.TempDir(), "scanindex.json")

	orig := newResolver
	defer func() { newResolver = orig }()
	blk := &blockSource{started: make(chan struct{}), release: make(chan struct{})}
	newResolver = func(dir string, offline bool) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Offline: offline, Sources: []metadata.Source{blk}})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var out, errb strings.Builder
	var code int
	go func() {
		defer close(done)
		code = realMainWithContext(ctx, []string{"-roots", root, "-cache", cache, "-index", index}, &out, &errb)
	}()
	select {
	case <-blk.started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("blocking provider never started")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled metawarm did not return")
	}
	if code != 0 {
		t.Errorf("exit = %d, want 0 for cancelled run (stderr=%s)", code, errb.String())
	}
	summary := out.String()
	for _, want := range []string{"cancelled", "errors=0"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}
	report := filepath.Join(local, "Potflix", "inventory", "warm-report--hold--fetch-cancel-case.txt")
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	for _, ln := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if ln == "" {
			continue
		}
		if st, _, _ := strings.Cut(ln, "\t"); st == statusError {
			t.Errorf("report marks a cancelled fetch as error: %q (full report:\n%s)", ln, string(data))
			break
		}
	}
}

// errSource fails every lookup transiently (provider outage).
type errSource struct{}

func (errSource) Name() string { return "fake" }

func (errSource) Find(ctx context.Context, q metadata.Query) (*metadata.Media, error) {
	return nil, errors.New("boom")
}

// TestSummaryReportsFailedKeysSeparately proves the CLI cannot bin a
// transient failure as not-found: the summary carries its own failed field,
// the per-file report marks the file error (not not-found), and nothing is
// persisted.
func TestSummaryReportsFailedKeysSeparately(t *testing.T) {
	root := seedWarmRoot(t, filepath.Join("A", "Gone.S01E01.mkv"))
	local := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)
	t.Setenv("OPENCODE_SESSION_ID", "failed-case")
	cache := t.TempDir()

	orig := newResolver
	defer func() { newResolver = orig }()
	newResolver = func(dir string, offline bool) *metadata.Resolver {
		return metadata.New(metadata.Options{Dir: dir, Offline: offline, Sources: []metadata.Source{errSource{}}})
	}

	var out, errb strings.Builder
	code := realMain([]string{"-roots", root, "-cache", cache}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errb.String())
	}
	summary := out.String()
	for _, want := range []string{
		"keys: 1 (0 cached, 0 fetched, 0 not found, 1 failed)",
		"resolved=0 not-found=0",
		"errors=1",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}
	report := filepath.Join(local, "Potflix", "inventory", "warm-report--hold--failed-case.txt")
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if st, _, _ := strings.Cut(strings.TrimRight(string(data), "\n"), "\t"); st != statusError {
		t.Errorf("report status = %q, want %q (full report:\n%s)", st, statusError, string(data))
	}
	entries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("cache dir has %d entries after transient failure, want 0", len(entries))
	}
}
