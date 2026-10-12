package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// clientid.go - resolveClientId, in the contract's order (section 7):
//   1. env POTFLIX_CLIENT_ID
//   2. file client-id.txt
//   3. file config.properties (raw text, first run of 17-20 digits)
//   4. hardcoded default
//
// Files are searched beside the running executable FIRST (the install
// directory, where they actually live), then against the process CWD - the
// original behaviour, kept so an install launched from its own directory is
// unaffected. Files are read as raw text, and the first regex match of 17-20
// digits wins. config.properties is NOT key/value parsed; no keys exist. The
// VALUE is never logged - only the source (sourceName).
//
// Ported from Main.java:515-536; the default is the Java literal at :516.
// Deviation: Main.java opened both files against the CWD only, so a launch from
// any other directory (Start-Process, a scheduled task, a shortcut with a
// different "Start in") missed client-id.txt and silently fell through to the
// default id - the wrong Discord application, and no Potflix artwork.

const defaultClientID int64 = 1146489019925004359

// clientIDExeDir yields the directory holding the running binary, so the
// client-id files can be resolved beside the installed executable instead of
// only against the process CWD. Injectable so a test can drive it without a
// real executable on disk.
var clientIDExeDir = func() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Dir(exe), nil
}

// clientIDPattern is the contract regex (\d{17,20}). Java applies it with
// matcher.find(), i.e. leftmost match; Go's FindString is leftmost too.
var clientIDPattern = regexp.MustCompile(`(\d{17,20})`)

type clientIDSource string

const (
	sourceEnv          clientIDSource = "env"
	sourceClientIDFile clientIDSource = "client-id.txt"
	sourceConfigProps  clientIDSource = "config.properties"
	sourceDefault      clientIDSource = "default"
)

// clientIDFileDirs is the search order for the client-id files: the executable's
// directory first, then "" (a bare relative path, i.e. the process CWD). An
// exe-dir error just drops that entry, so the CWD lookup, then the default,
// still run. The executable directory may be the CWD; re-reading is harmless.
func clientIDFileDirs() []string {
	if d, err := clientIDExeDir(); err == nil && d != "" {
		return []string{d, ""}
	}
	return []string{""}
}

// resolveClientID returns the id and the source it came from. Callers must log
// only the source, never the id.
func resolveClientID() (int64, clientIDSource) {
	// (1) environment. Java: null check, then Long.parseLong(env.trim());
	// a set-but-unparseable value falls through. os.Getenv returns "" for both
	// unset and empty, and "" fails ParseInt - same fall-through result.
	if v := os.Getenv("POTFLIX_CLIENT_ID"); v != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			return n, sourceEnv
		}
	}

	// (2) client-id.txt, (3) config.properties, in that order per directory.
	for _, d := range clientIDFileDirs() {
		for _, p := range []struct {
			name string
			src  clientIDSource
		}{
			{"client-id.txt", sourceClientIDFile},
			{"config.properties", sourceConfigProps},
		} {
			b, err := os.ReadFile(filepath.Join(d, p.name))
			if err != nil {
				continue
			}
			t := strings.TrimSpace(string(b))
			if m := clientIDPattern.FindString(t); m != "" {
				if n, err := strconv.ParseInt(m, 10, 64); err == nil {
					return n, p.src
				}
			}
		}
	}

	// (4) hardcoded default.
	return defaultClientID, sourceDefault
}
