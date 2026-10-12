// Package repair records the media files Potflix could not resolve (no title,
// cover or synopsis) as needs_repair, writes a local report the owner or an
// agent can read, and files a GitHub issue carrying that report.
//
// Two audiences, two forms of the same data:
//
//   - The LOCAL report (JSON + TXT, WriteReport) never leaves the machine. The
//     machine form keeps Entry.Path so the repair step can actually open the
//     file; the human form leads with that path.
//   - The PUBLISHED form (IssueBody) is sanitized. Entry.Path is never written
//     into it, Entry.Name is collapsed to its basename, and every free-text
//     field runs through SanitizeError. The published issue must never carry a
//     local path, a drive letter, a UNC path, the Windows username, an
//     environment value, the Discord client id or a token.
package repair

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Canonical Reason values. A caller may pass any string; the report only uses
// these to build a grouped title.
const (
	ReasonNoCover    = "no-cover"
	ReasonNoTitle    = "no-title"
	ReasonNoSynopsis = "no-synopsis"
	ReasonNoMetadata = "no-metadata"
)

// Local report file names, both written into the directory handed to
// WriteReport.
const (
	ReportJSONName = "needs-repair.json"
	ReportTXTName  = "needs-repair.txt"
)

// maxIssueEntries caps how many entries IssueBody publishes; the remainder is
// summarized as one "and N more" line.
const maxIssueEntries = 50

// maxErrorBytes caps a single error string published in the issue body.
const maxErrorBytes = 200

// Entry is one media file that could not be resolved.
type Entry struct {
	// Path is the full local path. It NEVER leaves the machine: it is written
	// to the local JSON/TXT report and never to IssueBody.
	Path string
	// Name is the basename - the only identifying field allowed to be
	// published. IssueBody collapses anything path-shaped here to its basename
	// as a second line of defence.
	Name string
	// Kind is the media kind ("movie", "episode", ...). Local only: the
	// published body carries Reason, Name, Attempts and the error, nothing else.
	Kind string
	// Reason is why the file needs repair, normally one of the Reason* values.
	Reason string
	// Attempts counts completed repair attempts for this file.
	Attempts int
	// LastError is the raw provider error from the last attempt. It is
	// sanitized (and truncated to 200 bytes) before publication.
	LastError string
	// LastTry is when the last attempt ran.
	LastTry time.Time
}

// Report is the whole needs-repair picture for one scan.
type Report struct {
	GeneratedAt time.Time
	AppVersion  string
	Totals      Totals
	Entries     []Entry
}

// Totals is the scan count. NeedsRepair should equal len(Entries); the report
// does not enforce that, it prints what the caller counted.
type Totals struct {
	Files       int
	Indexed     int
	Resolved    int
	NeedsRepair int
}

// WriteReport writes the JSON (machine) and TXT (human) report into dir and
// returns their paths. A zero GeneratedAt is stamped with time.Now().
//
// Both files are staged as temps in dir and renamed into place, so a reader
// never sees a half-written report. If either file cannot be staged or
// renamed, no part of this call's output is left behind: the temps are removed
// and a JSON renamed by this call is removed again, because a report that says
// "17 files need repair" is worse than no report at all. A stale report from an
// earlier run is therefore sacrificed on a failed write - reports are
// regenerated on demand.
func WriteReport(dir string, r Report) (jsonPath string, txtPath string, err error) {
	if strings.TrimSpace(dir) == "" {
		return "", "", errors.New("repair: WriteReport: empty dir")
	}
	if r.GeneratedAt.IsZero() {
		r.GeneratedAt = time.Now()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("repair: WriteReport: %w", err)
	}

	jsonPath = filepath.Join(dir, ReportJSONName)
	txtPath = filepath.Join(dir, ReportTXTName)

	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", "", fmt.Errorf("repair: WriteReport: encode json: %w", err)
	}
	data = append(data, '\n')

	jtmp, err := stageFile(jsonPath, data)
	if err != nil {
		return "", "", err
	}
	ttmp, err := stageFile(txtPath, []byte(renderText(r)))
	if err != nil {
		os.Remove(jtmp)
		return "", "", err
	}
	if err := os.Rename(jtmp, jsonPath); err != nil {
		os.Remove(jtmp)
		os.Remove(ttmp)
		return "", "", fmt.Errorf("repair: WriteReport: publish json: %w", err)
	}
	if err := os.Rename(ttmp, txtPath); err != nil {
		os.Remove(ttmp)
		os.Remove(jsonPath)
		return "", "", fmt.Errorf("repair: WriteReport: publish txt: %w", err)
	}
	return jsonPath, txtPath, nil
}

// ReadReport reads back a report written by WriteReport.
func ReadReport(jsonPath string) (Report, error) {
	b, err := os.ReadFile(jsonPath)
	if err != nil {
		return Report{}, fmt.Errorf("repair: ReadReport: %w", err)
	}
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		return Report{}, fmt.Errorf("repair: ReadReport: parse %s: %w", jsonPath, err)
	}
	return r, nil
}

// stageFile writes data to a temp file beside final and returns the temp path.
// The caller owns the temp file and must rename or remove it.
func stageFile(final string, data []byte) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(final), filepath.Base(final)+".*.tmp")
	if err != nil {
		return "", fmt.Errorf("repair: WriteReport: stage %s: %w", filepath.Base(final), err)
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", fmt.Errorf("repair: WriteReport: write %s: %w", filepath.Base(final), err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", fmt.Errorf("repair: WriteReport: sync %s: %w", filepath.Base(final), err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("repair: WriteReport: close %s: %w", filepath.Base(final), err)
	}
	return tmp, nil
}

// renderText is the human summary: a header, then one line per entry.
func renderText(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Potflix needs-repair report\n")
	fmt.Fprintf(&b, "generated:   %s\n", stamp(r.GeneratedAt))
	fmt.Fprintf(&b, "app version: %s\n", oneLine(r.AppVersion))
	fmt.Fprintf(&b, "totals:      %d files, %d indexed, %d resolved, %d needs repair\n",
		r.Totals.Files, r.Totals.Indexed, r.Totals.Resolved, r.Totals.NeedsRepair)
	if len(r.Entries) == 0 {
		b.WriteString("\nnothing needs repair.\n")
		return b.String()
	}
	b.WriteString("\n")
	for _, e := range r.Entries {
		// local= carries Entry.Path on purpose: this file never leaves the
		// machine and an owner list without paths is not actionable.
		fmt.Fprintf(&b, "%s  %s  attempts=%d  local=%s",
			reasonCode(e.Reason), oneLine(e.Name), e.Attempts, oneLine(e.Path))
		if errLine := oneLine(e.LastError); errLine != "" {
			fmt.Fprintf(&b, "  last=%s", errLine)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// IssueTitle summarizes the report for a GitHub issue title, e.g.
// "needs-repair: 17 files with no metadata (12 no cover, 5 no title)".
// It is never empty.
func IssueTitle(r Report) string {
	n := len(r.Entries)
	if n == 0 {
		return "needs-repair: no files need repair"
	}
	title := fmt.Sprintf("needs-repair: %d %s with no metadata", n, plural(n, "file"))
	parts := reasonBreakdown(r)
	if len(parts) > 0 {
		title += " (" + strings.Join(parts, ", ") + ")"
	}
	return title
}

// IssueBody renders the report as markdown. It contains only the app version,
// the generation timestamp, the totals and, per entry, the reason, the
// basename, the attempt count and a shortened error string - every free-text
// field sanitized. Callers may pass unsanitized Entries; that is the point.
func IssueBody(r Report) string {
	var b strings.Builder
	b.WriteString("### Potflix needs-repair report\n\n")
	fmt.Fprintf(&b, "- app version: %s\n", code(SanitizeError(r.AppVersion)))
	fmt.Fprintf(&b, "- generated: %s\n", stamp(r.GeneratedAt))
	fmt.Fprintf(&b, "- totals: %d files, %d indexed, %d resolved, %d needs repair\n",
		r.Totals.Files, r.Totals.Indexed, r.Totals.Resolved, r.Totals.NeedsRepair)

	shown := r.Entries
	hidden := 0
	if len(shown) > maxIssueEntries {
		hidden = len(shown) - maxIssueEntries
		shown = shown[:maxIssueEntries]
	}
	if len(shown) == 0 {
		return b.String()
	}
	b.WriteString("\n| # | reason | file | attempts | last error |\n")
	b.WriteString("|---|--------|------|----------|------------|\n")
	for i, e := range shown {
		fmt.Fprintf(&b, "| %d | %s | %s | %d | %s |\n",
			i+1,
			cell(reasonCode(e.Reason)),
			cell(basenameOf(oneLine(e.Name))),
			e.Attempts,
			truncateBytes(cell(SanitizeError(e.LastError)), maxErrorBytes),
		)
	}
	if hidden > 0 {
		fmt.Fprintf(&b, "\nand %d more\n", hidden)
	}
	return b.String()
}

// reasonBreakdown groups entries by reason, busiest first then alphabetical, so
// the title is stable for the same set of entries.
func reasonBreakdown(r Report) []string {
	counts := map[string]int{}
	for _, e := range r.Entries {
		counts[reasonCode(e.Reason)]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", counts[k], strings.ReplaceAll(k, "-", " ")))
	}
	return parts
}

// reasonCode is the raw reason, defaulted when the caller left it empty.
func reasonCode(reason string) string {
	if r := oneLine(reason); r != "" {
		return r
	}
	return "unspecified"
}

// cell makes a string safe inside a markdown table cell.
func cell(s string) string {
	return strings.ReplaceAll(s, "|", `\|`)
}

// code renders s as inline markdown code.
func code(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "'") + "`"
}

func stamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// oneLine flattens any whitespace run to a single space and trims the ends, so
// a multi-line provider error can never break "one line per entry" or a
// markdown table row.
func oneLine(s string) string {
	var b strings.Builder
	pending := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			pending = true
			continue
		}
		if pending && b.Len() > 0 {
			b.WriteByte(' ')
		}
		pending = false
		b.WriteRune(r)
	}
	return b.String()
}
