package repair

import (
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

// pinIdentity replaces the machine-derived redaction terms with fixed ones, so
// an assertion never depends on the account name of the machine running the
// tests (a username colliding with a fixture word would change the output).
func pinIdentity(t *testing.T, terms ...string) {
	t.Helper()
	old := identityTermSource
	identityTermSource = func() []string { return terms }
	t.Cleanup(func() { identityTermSource = old })
}

func TestSanitizeErrorStripsAbsolutePaths(t *testing.T) {
	pinIdentity(t, "tester", `C:\Users\tester`)
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "drive path with spaces",
			in:   `open D:\1movies\Some Folder\Some File.mkv: Access is denied`,
			want: `open Some File.mkv: Access is denied`,
		},
		{
			name: "drive path under a user profile",
			in:   `stat C:\Users\tester\Desktop\x.mkv: file does not exist`,
			want: `stat x.mkv: file does not exist`,
		},
		{
			name: "forward slashes",
			in:   `C:/Users/tester/Desktop/x.mkv is unreadable`,
			want: `x.mkv is unreadable`,
		},
		{
			name: "unc share",
			in:   `\\media-server\movies\No Cover.mkv missing`,
			want: `No Cover.mkv missing`,
		},
		{
			name: "extended length path",
			in:   `\\?\C:\very\long\path\x.mkv`,
			want: `x.mkv`,
		},
		{
			name: "drive-less rooted path",
			in:   `read \Users\tester\x.mkv failed`,
			want: `read x.mkv failed`,
		},
		{
			name: "quoted path",
			in:   `open "D:\My Movies\A File.mkv" failed`,
			want: `open "A File.mkv" failed`,
		},
		{
			name: "posix path",
			in:   `/home/tester/media/x.mkv: not found`,
			want: `x.mkv: not found`,
		},
		{
			name: "prose after a path survives",
			in:   `cannot open D:\a\b.mkv while reading`,
			want: `cannot open b.mkv while reading`,
		},
		{
			name: "url is not a path",
			in:   `GET https://api.example.com/v2/movie/123 failed`,
			want: `GET https://api.example.com/v2/movie/123 failed`,
		},
		{
			name: "prose with a colon is untouched",
			in:   `metadata: not found (404) at 22:30:45`,
			want: `metadata: not found (404) at 22:30:45`,
		},
		{
			name: "path holds the basename only",
			in:   `D:\1movies\Some Folder\Some File.mkv`,
			want: `Some File.mkv`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeError(tc.in); got != tc.want {
				t.Fatalf("SanitizeError(%q)\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeErrorNeverLeavesAPublishablePath(t *testing.T) {
	pinIdentity(t, "tester", `C:\Users\tester`)
	inputs := []string{
		`open D:\1movies\Some Folder\Some File.mkv: Access is denied`,
		`C:\Users\tester\Desktop\x.mkv`,
		`\\?\C:\very\long\path\x.mkv`,
		`\\media-server\movies\No Cover.mkv`,
		`error for /home/tester/x.mkv and \Users\tester\y.mkv`,
	}
	for _, in := range inputs {
		got := SanitizeError(in)
		if strings.Contains(got, `\`) || strings.Contains(got, `/`) {
			t.Errorf("SanitizeError(%q) kept a separator: %q", in, got)
		}
		if strings.Contains(strings.ToLower(got), "users") {
			t.Errorf("SanitizeError(%q) kept a directory name: %q", in, got)
		}
		if strings.Contains(strings.ToLower(got), "tester") {
			t.Errorf("SanitizeError(%q) kept the account name: %q", in, got)
		}
	}
}

func TestSanitizeErrorRedactsIdentityTermsAndIDs(t *testing.T) {
	pinIdentity(t, "tester", `C:\Users\tester`)
	got := SanitizeError(`repair tester\AppData for tester (client 1234567890123456789) failed`)
	if strings.Contains(got, "tester") {
		t.Fatalf("account name survived: %q", got)
	}
	if strings.Contains(got, "1234567890123456789") {
		t.Fatalf("id-shaped digits survived: %q", got)
	}
	if !strings.Contains(got, "<redacted>") || !strings.Contains(got, "<id>") {
		t.Fatalf("expected redaction markers, got %q", got)
	}
}

func TestSanitizeErrorFlattensWhitespace(t *testing.T) {
	got := SanitizeError("first line\n\tsecond   line\r\n")
	if got != "first line second line" {
		t.Fatalf("got %q", got)
	}
}

func TestSanitizeErrorEmpty(t *testing.T) {
	if got := SanitizeError(""); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

// The live source is the machine identity; the test only proves it produces
// terms this process can actually redact (never what they are).
func TestIdentityTermsAreUsable(t *testing.T) {
	terms := identityTerms()
	for _, term := range terms {
		if utf8.RuneCountInString(term) < minRedactTerm {
			continue // deliberately not redacted
		}
		rt := redactTerms("prefix "+term+" suffix", terms)
		if strings.Contains(rt, term) {
			t.Fatalf("redactTerms left %q in %q", term, rt)
		}
	}
}

// The pinned cases above are synthetic. This case covers the live identity
// instead: it derives the machine's own home directory at runtime - never
// writing the value down - and proves no part of it reaches the published text.
// The bare account name is asserted on its own, in prose, where no path span
// exists to collapse it: only the term redaction can remove it.
func TestSanitizeErrorRedactsTheLiveHomeDirectory(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		t.Skip("no home directory to derive")
	}
	got := SanitizeError(`open ` + home + `\x.mkv: Access is denied`)
	if strings.Contains(got, home) {
		t.Fatalf("live home directory survived: %q", got)
	}
	if strings.Contains(strings.ToLower(got), "users") {
		t.Fatalf("profile directory survived: %q", got)
	}
	if strings.ContainsAny(got, `\/`) {
		t.Fatalf("a separator survived: %q", got)
	}

	account := basenameOf(home)
	if account == "" || account == "<path>" || utf8.RuneCountInString(account) < minRedactTerm {
		return // deliberately not redacted, or nothing to take from the path
	}
	got = SanitizeError("repair failed for " + account + " (see log)")
	if strings.Contains(strings.ToLower(got), strings.ToLower(account)) {
		t.Fatalf("live account name %q survived: %q", account, got)
	}
}

func TestTruncateBytes(t *testing.T) {
	long := strings.Repeat("a", 300)
	if got := truncateBytes(long, 200); len(got) != 200 || !strings.HasSuffix(got, "...") {
		t.Fatalf("got len %d, suffix ok: %v", len(got), strings.HasSuffix(got, "..."))
	}
	if got := truncateBytes("short", 200); got != "short" {
		t.Fatalf("got %q", got)
	}
	// A multi-byte rune at the cut must not be split.
	uni := strings.Repeat("é", 200)
	got := truncateBytes(uni, 200)
	if len(got) > 200 || !strings.HasSuffix(got, "...") {
		t.Fatalf("got len %d (%q)", len(got), got)
	}
	if strings.ContainsRune(got, '\uFFFD') {
		t.Fatalf("split a rune: %q", got)
	}
}

func TestBasenameOf(t *testing.T) {
	cases := map[string]string{
		`D:\a\b.mkv`:             "b.mkv",
		`D:\a\b.mkv\`:            "b.mkv",
		`C:\`:                    "<path>",
		`\\server\share`:         "share",
		`/home/u/x.mkv`:          "x.mkv",
		`Some File.mkv`:          "Some File.mkv",
		`Movie (2020).mkv)`:      "Movie (2020).mkv",
		`D:\a\.`:                 "<path>",
		`D:\a\b.mkv.`:            "b.mkv",
		`\\?\C:\very\deep\x.m`:   "x.m",
		`D:\My Movies\A (x).mkv`: "A (x).mkv",
	}
	for in, want := range cases {
		if got := basenameOf(in); got != want {
			t.Errorf("basenameOf(%q) = %q, want %q", in, got, want)
		}
	}
}
