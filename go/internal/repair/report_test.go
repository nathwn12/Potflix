package repair

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func sampleReport() Report {
	return Report{
		GeneratedAt: time.Date(2026, 10, 12, 21, 30, 0, 0, time.UTC),
		AppVersion:  "1.2.3",
		Totals:      Totals{Files: 40, Indexed: 38, Resolved: 21, NeedsRepair: 2},
		Entries: []Entry{
			{
				Path:      `D:\1movies\Some Folder\Some File.mkv`,
				Name:      "Some File.mkv",
				Kind:      "movie",
				Reason:    ReasonNoCover,
				Attempts:  3,
				LastError: `open D:\1movies\Some Folder\Some File.mkv: Access is denied`,
				LastTry:   time.Date(2026, 10, 12, 21, 0, 0, 0, time.UTC),
			},
			{
				Path:      `C:\Users\tester\Desktop\x.mkv`,
				Name:      "x.mkv",
				Kind:      "episode",
				Reason:    ReasonNoTitle,
				Attempts:  1,
				LastError: "metadata: no match",
				LastTry:   time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC),
			},
		},
	}
}

func TestWriteReportReadReportRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := sampleReport()

	jsonPath, txtPath, err := WriteReport(dir, want)
	if err != nil {
		t.Fatalf("WriteReport: %v", err)
	}
	if jsonPath != filepath.Join(dir, ReportJSONName) {
		t.Errorf("json path = %q", jsonPath)
	}
	if txtPath != filepath.Join(dir, ReportTXTName) {
		t.Errorf("txt path = %q", txtPath)
	}

	got, err := ReadReport(jsonPath)
	if err != nil {
		t.Fatalf("ReadReport: %v", err)
	}
	if !got.GeneratedAt.Equal(want.GeneratedAt) {
		t.Errorf("GeneratedAt = %v, want %v", got.GeneratedAt, want.GeneratedAt)
	}
	if got.AppVersion != want.AppVersion || got.Totals != want.Totals {
		t.Errorf("header = %q %+v, want %q %+v", got.AppVersion, got.Totals, want.AppVersion, want.Totals)
	}
	if len(got.Entries) != len(want.Entries) {
		t.Fatalf("entries = %d, want %d", len(got.Entries), len(want.Entries))
	}
	for i := range want.Entries {
		w, g := want.Entries[i], got.Entries[i]
		w.LastTry, g.LastTry = g.LastTry, w.LastTry // normalized below
		if w.Path != g.Path || w.Name != g.Name || w.Kind != g.Kind ||
			w.Reason != g.Reason || w.Attempts != g.Attempts || w.LastError != g.LastError {
			t.Errorf("entry %d = %+v, want %+v", i, got.Entries[i], want.Entries[i])
		}
		if !got.Entries[i].LastTry.Equal(want.Entries[i].LastTry) {
			t.Errorf("entry %d LastTry = %v, want %v", i, got.Entries[i].LastTry, want.Entries[i].LastTry)
		}
	}

	txt, err := os.ReadFile(txtPath)
	if err != nil {
		t.Fatalf("read txt: %v", err)
	}
	body := string(txt)
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	entryLines := 0
	for _, ln := range lines {
		if strings.Contains(ln, "attempts=") {
			entryLines++
		}
	}
	if entryLines != len(want.Entries) {
		t.Fatalf("txt has %d entry lines, want %d:\n%s", entryLines, len(want.Entries), body)
	}
	for _, e := range want.Entries {
		if !strings.Contains(body, e.Name) {
			t.Errorf("txt is missing %q:\n%s", e.Name, body)
		}
		if !strings.Contains(body, e.Path) {
			t.Errorf("txt (local) is missing path %q:\n%s", e.Path, body)
		}
	}
}

// A failed write must leave nothing behind: no half-written report, no temp
// file. The failure is forced by occupying the TXT target with a directory,
// which makes the second rename fail after the JSON rename has already
// happened.
func TestWriteReportFailedWriteLeavesNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ReportTXTName), 0o755); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}

	if _, _, err := WriteReport(dir, sampleReport()); err == nil {
		t.Fatal("WriteReport succeeded with the txt target blocked, want error")
	}
	if _, err := os.Stat(filepath.Join(dir, ReportJSONName)); !os.IsNotExist(err) {
		t.Fatalf("json report left behind: %v", err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
	if len(ents) != 1 || ents[0].Name() != ReportTXTName {
		t.Errorf("dir contents = %v, want only %s", ents, ReportTXTName)
	}
}

// A directory that cannot be created (it is a file) fails before anything is
// staged, and the file it collided with is untouched.
func TestWriteReportUnwritableDir(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("keep me"), 0o644); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	if _, _, err := WriteReport(filepath.Join(blocker, "sub"), sampleReport()); err == nil {
		t.Fatal("WriteReport succeeded under a file path, want error")
	}
	b, err := os.ReadFile(blocker)
	if err != nil || string(b) != "keep me" {
		t.Fatalf("blocker changed: %q %v", b, err)
	}
}

func TestReadReportMissingOrBroken(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadReport(filepath.Join(dir, "nope.json")); err == nil {
		t.Error("ReadReport on a missing file returned no error")
	}
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadReport(broken); err == nil {
		t.Error("ReadReport on broken json returned no error")
	}
}

// The published body is the only thing that leaves the machine. This is the
// sanitization proof: paths shaped like a real user profile (a drive path
// with spaces, a path under a user profile) must not survive in any form, in
// any field, and the basename must.
func TestIssueBodyNeverCarriesALocalPath(t *testing.T) {
	pinIdentity(t, "tester", `C:\Users\tester`)

	r := sampleReport()
	// Worst case: a caller hands over the full path as Name too.
	r.Entries[1].Name = `C:\Users\tester\Desktop\x.mkv`
	r.Entries[0].LastError = `open D:\1movies\Some Folder\Some File.mkv: Access is denied`
	r.Entries[1].LastError = `metadata: lookup failed for C:\Users\tester\Desktop\x.mkv`

	body := IssueBody(r)
	t.Logf("body:\n%s", body)

	forbidden := []struct {
		name string
		re   *regexp.Regexp
	}{
		{"drive-letter path", regexp.MustCompile(`(?i)[a-z]:\\`)},
		{"profile directory", regexp.MustCompile(`(?i)Users[\\/]`)},
	}
	for _, f := range forbidden {
		if loc := f.re.FindString(body); loc != "" {
			t.Errorf("body carries a %s: %q", f.name, loc)
		}
	}
	for _, bad := range []string{"tester", `\`, "/", "1movies", "Desktop", "Some Folder", `D:`, `C:`} {
		if strings.Contains(body, bad) {
			t.Errorf("body carries %q:\n%s", bad, body)
		}
	}
	for _, want := range []string{"Some File.mkv", "x.mkv", "1.2.3", ReasonNoCover, ReasonNoTitle} {
		if !strings.Contains(body, want) {
			t.Errorf("body is missing %q:\n%s", want, body)
		}
	}
	// Nothing outside the report's own field set was added.
	if strings.Contains(body, r.Entries[0].Path) || strings.Contains(body, r.Entries[1].Path) {
		t.Errorf("body carries a full path:\n%s", body)
	}
}

func TestIssueBodyCapsEntriesAtFifty(t *testing.T) {
	pinIdentity(t, "tester")
	r := sampleReport()
	r.Entries = nil
	for i := 1; i <= 80; i++ {
		r.Entries = append(r.Entries, Entry{
			Path:     `D:\1movies\file.mkv`,
			Name:     "file-" + pad2(i) + ".mkv",
			Reason:   ReasonNoCover,
			Attempts: 1,
		})
	}
	body := IssueBody(r)

	rows := regexp.MustCompile(`(?m)^\| [0-9]+ \|`)
	if n := len(rows.FindAllString(body, -1)); n != 50 {
		t.Errorf("entry rows = %d, want 50", n)
	}
	if !strings.Contains(body, "and 30 more") {
		t.Errorf("missing the and-30-more line:\n%s", body)
	}
	if !strings.Contains(body, "file-50.mkv") || strings.Contains(body, "file-51.mkv") {
		t.Errorf("cap boundary is wrong:\n%s", body)
	}
}

func TestIssueBodyTruncatesASingleErrorTo200Bytes(t *testing.T) {
	pinIdentity(t, "tester")
	long := strings.Repeat("z", 300)
	body := IssueBody(Report{Entries: []Entry{{Name: "a.mkv", Reason: ReasonNoCover, LastError: long}}})
	if !strings.Contains(body, strings.Repeat("z", 197)+"...") {
		t.Errorf("expected a 200-byte truncated cell:\n%s", body)
	}
	if strings.Contains(body, strings.Repeat("z", 201)) {
		t.Errorf("error was not truncated:\n%s", body)
	}
}

func TestIssueBodyZeroEntries(t *testing.T) {
	pinIdentity(t, "tester")
	r := sampleReport()
	r.Entries = nil
	body := IssueBody(r)
	if strings.Contains(body, "| # |") || strings.Contains(body, "more") {
		t.Errorf("empty report rendered a table or a remainder line:\n%s", body)
	}
	if !strings.Contains(body, "totals: 40 files, 38 indexed, 21 resolved, 2 needs repair") {
		t.Errorf("empty report lost the totals:\n%s", body)
	}
	if !strings.Contains(body, "1.2.3") {
		t.Errorf("empty report lost the app version:\n%s", body)
	}
}

func TestIssueTitle(t *testing.T) {
	pinIdentity(t, "tester")

	zero := IssueTitle(Report{})
	if zero == "" {
		t.Fatal("IssueTitle returned an empty string for an empty report")
	}
	if !strings.Contains(zero, "no files need repair") {
		t.Errorf("zero title = %q", zero)
	}

	one := IssueTitle(Report{Entries: []Entry{{Reason: ReasonNoTitle}}})
	if one != "needs-repair: 1 file with no metadata (1 no title)" {
		t.Errorf("one title = %q", one)
	}

	r := Report{}
	for i := 0; i < 12; i++ {
		r.Entries = append(r.Entries, Entry{Path: `D:\1movies\x.mkv`, Name: "x.mkv", Reason: ReasonNoCover})
	}
	for i := 0; i < 5; i++ {
		r.Entries = append(r.Entries, Entry{Path: `C:\Users\tester\y.mkv`, Name: "y.mkv", Reason: ReasonNoTitle})
	}
	got := IssueTitle(r)
	want := "needs-repair: 17 files with no metadata (12 no cover, 5 no title)"
	if got != want {
		t.Errorf("title = %q, want %q", got, want)
	}
	if strings.ContainsAny(got, `\/`) || strings.Contains(strings.ToLower(got), "tester") {
		t.Errorf("title carries a local path: %q", got)
	}
}

func TestIssueTitleUnsortedCounts(t *testing.T) {
	r := Report{Entries: []Entry{
		{Reason: ReasonNoTitle},
		{Reason: ReasonNoCover},
		{Reason: ReasonNoCover},
		{Reason: ""},
	}}
	got := IssueTitle(r)
	want := "needs-repair: 4 files with no metadata (2 no cover, 1 no title, 1 unspecified)"
	if got != want {
		t.Errorf("title = %q, want %q", got, want)
	}
}

func pad2(i int) string {
	if i < 10 {
		return "0" + string(rune('0'+i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}
