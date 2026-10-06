package main

import (
	"os"
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
// Files open relative to the process CWD, are read as raw text, and the first
// regex match of 17-20 digits wins. config.properties is NOT key/value parsed;
// no keys exist. The VALUE is never logged - only the source (sourceName).
//
// Ported from Main.java:515-536; the default is the Java literal at :516.

const defaultClientID int64 = 1146489019925004359

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

	// (2) client-id.txt, (3) config.properties.
	for _, p := range []struct {
		name string
		src  clientIDSource
	}{
		{"client-id.txt", sourceClientIDFile},
		{"config.properties", sourceConfigProps},
	} {
		b, err := os.ReadFile(p.name)
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

	// (4) hardcoded default.
	return defaultClientID, sourceDefault
}
