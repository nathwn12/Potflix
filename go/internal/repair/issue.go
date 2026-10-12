package repair

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
)

// Runner runs an external command and returns its stdout. It exists so callers
// (and tests) can supply gh, a stub, or nothing at all: CreateIssue never needs
// a real gh to be useful - without one it returns a prefilled issue URL the
// owner can open in a browser.
type Runner func(ctx context.Context, args ...string) (stdout string, err error)

// Options configures issue creation.
type Options struct {
	// Repo is "owner/name". Empty means "the repository gh is already in",
	// which works for the gh path but cannot build a fallback URL.
	Repo string
	// Runner is the gh invocation. Nil uses a real gh when HasGH reports one
	// is on PATH.
	Runner Runner
	// Labels are attached in order as repeated --label flags.
	Labels []string
}

// ghBin is the executable looked up and run. A test seam: pinning it to a name
// that cannot exist exercises the real-gh branch without ever executing gh.
var ghBin = "gh"

// hasGH is HasGH behind a seam, so the no-gh fallback is testable on a machine
// that does have gh installed.
var hasGH = HasGH

// HasGH reports whether a gh executable is on PATH. It looks the file up, it
// does not run it and it never touches the network.
func HasGH() bool {
	_, err := exec.LookPath(ghBin)
	return err == nil
}

// CreateIssue files the report as a GitHub issue and returns the issue URL.
//
// With a usable runner (opts.Runner, else a real gh on PATH) it runs
// `gh issue create --repo <repo> --title <t> --body <b> [--label L]...` and
// returns the URL gh printed. Without one it returns a prefilled
// https://github.com/<repo>/issues/new?title=...&body=... URL and a nil error:
// nothing failed, there is simply no gh to run, and the owner can still file
// the issue in one click.
//
// When a runner exists and fails, both are returned: the prefilled URL and the
// error, so the caller can offer the manual path without hiding the failure.
// The body is sanitized in either case (IssueBody).
func CreateIssue(ctx context.Context, r Report, opts Options) (string, error) {
	title := IssueTitle(r)
	body := IssueBody(r)
	fallback := func() (string, error) { return issueURL(opts.Repo, title, body, opts.Labels) }

	runner := opts.Runner
	if runner == nil {
		if !hasGH() {
			return fallback()
		}
		runner = runGH
	}

	out, err := runner(ctx, issueArgs(opts.Repo, title, body, opts.Labels)...)
	if err != nil {
		u, ferr := fallback()
		if ferr != nil {
			// The failure and the missing fallback are the same failure: no
			// usable gh and no repo to build a URL from.
			return "", fmt.Errorf("repair: CreateIssue: %w (no fallback url: %v)", err, ferr)
		}
		return u, fmt.Errorf("repair: CreateIssue: %w", err)
	}
	if u := firstURL(out); u != "" {
		return u, nil
	}
	u, ferr := fallback()
	if ferr != nil {
		return "", errors.New("repair: CreateIssue: gh printed no issue url and there is no repo for a fallback url")
	}
	return u, errors.New("repair: CreateIssue: gh printed no issue url")
}

// issueArgs is the argv handed to gh. It is built once so the injected-runner
// path and the real-gh path can never drift apart.
func issueArgs(repo, title, body string, labels []string) []string {
	args := []string{"issue", "create"}
	if r := strings.TrimSpace(repo); r != "" {
		args = append(args, "--repo", r)
	}
	args = append(args, "--title", title, "--body", body)
	for _, l := range labels {
		if s := strings.TrimSpace(l); s != "" {
			args = append(args, "--label", s)
		}
	}
	return args
}

// issueURL is the prefilled new-issue URL used when gh is unavailable.
func issueURL(repo, title, body string, labels []string) (string, error) {
	r := strings.Trim(strings.TrimSpace(repo), "/")
	r = strings.TrimPrefix(r, "https://github.com/")
	r = strings.TrimPrefix(r, "github.com/")
	if r == "" {
		return "", errors.New("repair: CreateIssue: empty repo")
	}
	q := url.Values{}
	q.Set("title", title)
	q.Set("body", body)
	if len(labels) > 0 {
		q.Set("labels", strings.Join(labels, ","))
	}
	return "https://github.com/" + r + "/issues/new?" + q.Encode(), nil
}

// runGH is the real Runner: it runs gh and returns its stdout.
func runGH(ctx context.Context, args ...string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("repair: runGH: no args")
	}
	cmd := exec.CommandContext(ctx, ghBin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := oneLine(stderr.String()); msg != "" {
			return "", fmt.Errorf("repair: %s %s: %w: %s", ghBin, args[0], err, msg)
		}
		return "", fmt.Errorf("repair: %s %s: %w", ghBin, args[0], err)
	}
	return stdout.String(), nil
}

// firstURL returns the first http(s) URL in s - gh prints the created issue's
// URL on its own line.
func firstURL(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		i := strings.Index(line, "http://")
		if j := strings.Index(line, "https://"); j >= 0 && (i < 0 || j < i) {
			i = j
		}
		if i < 0 {
			continue
		}
		u := strings.TrimRight(line[i:], " \t\r")
		u = strings.TrimRight(u, ".,;:)]}\"'")
		if u != "" {
			return u
		}
	}
	return ""
}
