package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"regexp"
	"unicode/utf8"

	"potflix/internal/titleclean"
)

// Parity CLI for the titleclean port. Reads a corpus (UTF-8); per line: trim,
// skip empty, strip a trailing " - PotPlayer" (as the Java runner does), call
// the cleaner, and write
// input \t title \t display \t episodeTag \t episodeName with LF endings.
//
// It is the bake-off harness kept runnable against the shipped package. The
// cleaner itself lives in potflix/internal/titleclean as a verbatim port.
func main() {
	corpus := flag.String("corpus", "", "input corpus path")
	out := flag.String("out", "", "output tsv path")
	flag.Parse()
	if *corpus == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: titleclean -corpus <in.txt> -out <out.tsv>")
		os.Exit(2)
	}
	if err := run(*corpus, *out); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(corpus, out string) error {
	data, err := os.ReadFile(corpus)
	if err != nil {
		return fmt.Errorf("read corpus: %w", err)
	}
	// Java's Files.readAllLines(..., UTF_8) throws MalformedInputException on
	// invalid UTF-8; match that by failing instead of silently mangling bytes.
	if !utf8.Valid(data) {
		return fmt.Errorf("corpus is not valid UTF-8: %s", corpus)
	}
	f, err := os.Create(out)
	if err != nil {
		return fmt.Errorf("create out: %w", err)
	}
	w := bufio.NewWriter(f)

	potSuffix := regexp.MustCompile(` - PotPlayer\z`)
	for _, line := range splitLines(string(data)) {
		input := titleclean.JavaTrim(line)
		if input == "" {
			continue
		}
		c := titleclean.Clean(titleclean.JavaDollarReplaceAll(potSuffix, input, ""))
		if _, err := w.WriteString(input); err != nil {
			return fmt.Errorf("write out: %w", err)
		}
		if _, err := w.WriteString("\t"); err != nil {
			return fmt.Errorf("write out: %w", err)
		}
		if _, err := w.WriteString(c.Title); err != nil {
			return fmt.Errorf("write out: %w", err)
		}
		if _, err := w.WriteString("\t"); err != nil {
			return fmt.Errorf("write out: %w", err)
		}
		if _, err := w.WriteString(c.Display); err != nil {
			return fmt.Errorf("write out: %w", err)
		}
		if _, err := w.WriteString("\t"); err != nil {
			return fmt.Errorf("write out: %w", err)
		}
		if _, err := w.WriteString(c.EpisodeTag); err != nil {
			return fmt.Errorf("write out: %w", err)
		}
		if _, err := w.WriteString("\t"); err != nil {
			return fmt.Errorf("write out: %w", err)
		}
		if _, err := w.WriteString(c.EpisodeName); err != nil {
			return fmt.Errorf("write out: %w", err)
		}
		if err := w.WriteByte('\n'); err != nil {
			return fmt.Errorf("write out: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush out: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close out: %w", err)
	}
	return nil
}

// splitLines mirrors java.nio.file.Files.readAllLines: split on \n, \r\n, or \r,
// with no trailing empty element.
func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\n' || c == '\r' {
			out = append(out, s[start:i])
			if c == '\r' && i+1 < len(s) && s[i+1] == '\n' {
				i++
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
