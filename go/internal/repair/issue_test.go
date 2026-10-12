package repair

import (
	"context"
	"errors"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// pinGH replaces the gh seams: the "is gh installed" answer and the executable
// name. Tests never depend on whether the machine really has gh, and the real
// gh is never executed.
func pinGH(t *testing.T, installed bool, bin string) {
	t.Helper()
	oldHas, oldBin := hasGH, ghBin
	hasGH = func() bool { return installed }
	ghBin = bin
	t.Cleanup(func() { hasGH, ghBin = oldHas, oldBin })
}

func TestCreateIssueRunnerArgvAndURL(t *testing.T) {
	pinIdentity(t, "tester", `C:\Users\tester`)
	pinGH(t, true, "gh")

	r := sampleReport()
	var calls [][]string
	runner := func(ctx context.Context, args ...string) (string, error) {
		calls = append(calls, args)
		return "Creating issue in nathwn12/Potflix\nhttps://github.com/nathwn12/Potflix/issues/12\n", nil
	}

	opts := Options{Repo: "nathwn12/Potflix", Runner: runner, Labels: []string{"needs-repair", "bug"}}
	got, err := CreateIssue(context.Background(), r, opts)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if got != "https://github.com/nathwn12/Potflix/issues/12" {
		t.Errorf("url = %q", got)
	}
	want := []string{
		"issue", "create",
		"--repo", "nathwn12/Potflix",
		"--title", IssueTitle(r),
		"--body", IssueBody(r),
		"--label", "needs-repair",
		"--label", "bug",
	}
	if len(calls) != 1 {
		t.Fatalf("runner called %d times, want 1", len(calls))
	}
	if !reflect.DeepEqual(calls[0], want) {
		t.Errorf("argv\n got: %q\nwant: %q", calls[0], want)
	}
}

func TestCreateIssueWithoutLabelsOmitsLabelFlags(t *testing.T) {
	pinIdentity(t, "tester")
	r := sampleReport()
	var got []string
	runner := func(ctx context.Context, args ...string) (string, error) {
		got = args
		return "https://github.com/o/r/issues/1", nil
	}
	if _, err := CreateIssue(context.Background(), r, Options{Repo: "o/r", Runner: runner}); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	for _, a := range got {
		if a == "--label" {
			t.Fatalf("argv has an unexpected --label: %q", got)
		}
	}
	want := []string{"issue", "create", "--repo", "o/r", "--title", IssueTitle(r), "--body", IssueBody(r)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv\n got: %q\nwant: %q", got, want)
	}
}

// The no-gh path is deterministic: the seam says gh is missing, so CreateIssue
// must return a prefilled URL and no error, whether or not gh really exists.
func TestCreateIssueFallsBackToAPrefilledURL(t *testing.T) {
	pinIdentity(t, "tester", `C:\Users\tester`)
	pinGH(t, false, "gh")

	r := sampleReport()
	got, err := CreateIssue(context.Background(), r, Options{Repo: "nathwn12/Potflix"})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	u, perr := url.Parse(got)
	if perr != nil {
		t.Fatalf("url.Parse(%q): %v", got, perr)
	}
	if u.Scheme != "https" || u.Host != "github.com" || u.Path != "/nathwn12/Potflix/issues/new" {
		t.Fatalf("url = %q", got)
	}
	q := u.Query()
	if q.Get("title") != IssueTitle(r) {
		t.Errorf("title = %q", q.Get("title"))
	}
	if q.Get("body") != IssueBody(r) {
		t.Errorf("body round-trip differs")
	}
	if strings.Contains(q.Get("body"), `\`) {
		t.Errorf("fallback body carries a separator:\n%s", q.Get("body"))
	}
	if q.Get("labels") != "" {
		t.Errorf("labels = %q, want empty", q.Get("labels"))
	}
}

func TestCreateIssueFallbackCarriesLabels(t *testing.T) {
	pinIdentity(t, "tester")
	pinGH(t, false, "gh")
	got, err := CreateIssue(context.Background(), sampleReport(), Options{
		Repo:   "o/r",
		Labels: []string{"needs-repair", "bug"},
	})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("labels") != "needs-repair,bug" {
		t.Errorf("labels = %q", u.Query().Get("labels"))
	}
}

func TestCreateIssueEmptyRepoWithoutGHIsAnError(t *testing.T) {
	pinGH(t, false, "gh")
	if _, err := CreateIssue(context.Background(), sampleReport(), Options{}); err == nil {
		t.Fatal("CreateIssue with no repo and no gh returned no error")
	}
}

// A runner that fails still hands back the prefilled URL, alongside the error.
func TestCreateIssueRunnerFailureReturnsFallbackAndError(t *testing.T) {
	pinIdentity(t, "tester")
	pinGH(t, true, "gh")
	boom := errors.New("gh: not logged in")
	runner := func(ctx context.Context, args ...string) (string, error) { return "", boom }

	got, err := CreateIssue(context.Background(), sampleReport(), Options{Repo: "o/r", Runner: runner})
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if !strings.HasPrefix(got, "https://github.com/o/r/issues/new?") {
		t.Fatalf("url = %q, want a prefilled fallback", got)
	}
}

// With no injected runner and a gh name that cannot exist, the real-gh branch
// runs, LookPath fails inside exec, and the call degrades to the fallback. No
// gh process is ever started.
func TestCreateIssueRealGHBranchDegradesWhenGHIsMissing(t *testing.T) {
	pinIdentity(t, "tester")
	pinGH(t, true, "potflix-no-such-gh-binary-"+t.Name())

	got, err := CreateIssue(context.Background(), sampleReport(), Options{Repo: "o/r"})
	if err == nil {
		t.Fatal("want an error from the failed gh exec")
	}
	if !strings.HasPrefix(got, "https://github.com/o/r/issues/new?") {
		t.Fatalf("url = %q, want a prefilled fallback", got)
	}
}

func TestFirstURL(t *testing.T) {
	cases := map[string]string{
		"https://github.com/o/r/issues/3\n":                 "https://github.com/o/r/issues/3",
		"Creating issue in o/r\nhttps://github.com/o/r/1\n": "https://github.com/o/r/1",
		"no url here":                            "",
		"":                                       "",
		"see (https://github.com/o/r/issues/9).": "https://github.com/o/r/issues/9",
		"http://example.test/x":                  "http://example.test/x",
	}
	for in, want := range cases {
		if got := firstURL(in); got != want {
			t.Errorf("firstURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHasGHDoesNotPanic(t *testing.T) {
	pinGH(t, false, "potflix-no-such-gh-binary")
	if HasGH() {
		t.Error("HasGH found a binary that cannot exist")
	}
}
