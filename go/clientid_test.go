package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Proves resolveClientID's order and raw-scan behaviour (contract section 7):
// env -> client-id.txt -> config.properties (raw 17-20 digit scan) -> default.

func TestResolveClientIDOrder(t *testing.T) {
	orig, had := os.LookupEnv("POTFLIX_CLIENT_ID")
	defer func() {
		if had {
			os.Setenv("POTFLIX_CLIENT_ID", orig)
		} else {
			os.Unsetenv("POTFLIX_CLIENT_ID")
		}
	}()

	// (1) env wins.
	os.Setenv("POTFLIX_CLIENT_ID", "11111111111111111")
	if id, src := resolveClientID(); id != 11111111111111111 || src != sourceEnv {
		t.Fatalf("env: got id=%d src=%s", id, src)
	}

	dir := t.TempDir()
	t.Chdir(dir)
	os.Unsetenv("POTFLIX_CLIENT_ID")

	// (2) client-id.txt beats config.properties.
	if err := os.WriteFile("client-id.txt", []byte("22222222222222222"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("config.properties", []byte("33333333333333333"), 0o644); err != nil {
		t.Fatal(err)
	}
	if id, src := resolveClientID(); id != 22222222222222222 || src != sourceClientIDFile {
		t.Fatalf("client-id.txt: got id=%d src=%s", id, src)
	}

	// (3) config.properties, raw scan (no key/value parsing).
	os.Remove("client-id.txt")
	if id, src := resolveClientID(); id != 33333333333333333 || src != sourceConfigProps {
		t.Fatalf("config.properties: got id=%d src=%s", id, src)
	}

	// (3b) first 17-20 digit run wins, even inside unrelated text.
	os.WriteFile("config.properties", []byte("not a key=value\nclientId = 44444444444444444\n"), 0o644)
	if id, src := resolveClientID(); id != 44444444444444444 || src != sourceConfigProps {
		t.Fatalf("raw scan: got id=%d src=%s", id, src)
	}

	// (4) default when the files are gone.
	os.Remove("config.properties")
	if id, src := resolveClientID(); id != defaultClientID || src != sourceDefault {
		t.Fatalf("default: got id=%d src=%s", id, src)
	}
}

// --- client-id files beside the executable (regression, measured live) ---
//
// resolveClientID used to open client-id.txt / config.properties against the
// process CWD only. A launch from any other directory (Start-Process, a
// scheduled task, a shortcut with a different "Start in") missed the file,
// fell through to the hardcoded default id, and talked to the wrong Discord
// application: the Rich Presence card lost its Potflix artwork.

// stubClientIDExeDir replaces the exe-directory seam for one test.
func stubClientIDExeDir(t *testing.T, dir string, err error) {
	t.Helper()
	prev := clientIDExeDir
	clientIDExeDir = func() (string, error) { return dir, err }
	t.Cleanup(func() { clientIDExeDir = prev })
}

// stubClientIDEnv pins POTFLIX_CLIENT_ID ("" unsets it) and restores it after.
func stubClientIDEnv(t *testing.T, val string) {
	t.Helper()
	orig, had := os.LookupEnv("POTFLIX_CLIENT_ID")
	t.Cleanup(func() {
		if had {
			os.Setenv("POTFLIX_CLIENT_ID", orig)
		} else {
			os.Unsetenv("POTFLIX_CLIENT_ID")
		}
	})
	if val == "" {
		os.Unsetenv("POTFLIX_CLIENT_ID")
		return
	}
	os.Setenv("POTFLIX_CLIENT_ID", val)
}

// knownSources is every name resolveClientID may report as the source.
var knownSources = []clientIDSource{sourceEnv, sourceClientIDFile, sourceConfigProps, sourceDefault}

// wantResolve asserts the id and the source name, and that the source is one of
// the four known names carrying no trace of the id VALUE.
func wantResolve(t *testing.T, wantID int64, wantSrc clientIDSource, ctx string) {
	t.Helper()
	id, src := resolveClientID()
	if src != wantSrc || id != wantID {
		t.Fatalf("%s: got id=%d src=%q, want id=%d src=%q", ctx, id, src, wantID, wantSrc)
	}
	known := false
	for _, s := range knownSources {
		if src == s {
			known = true
		}
	}
	if !known {
		t.Fatalf("%s: source %q is not one of the four known source names", ctx, src)
	}
	if strings.Contains(string(src), strconv.FormatInt(id, 10)) {
		t.Fatalf("%s: source string %q leaks the id value", ctx, src)
	}
}

// TestResolveClientIDFindsFileBesideExeWhenCWDHasNone is the regression test for
// the live defect: client-id.txt sits in the install directory (beside the exe)
// while the process CWD holds no such file, and resolution must still find it.
func TestResolveClientIDFindsFileBesideExeWhenCWDHasNone(t *testing.T) {
	stubClientIDEnv(t, "")

	install := t.TempDir() // the install dir: holds client-id.txt
	cwd := t.TempDir()     // launched from anywhere else: holds nothing
	if err := os.WriteFile(filepath.Join(install, "client-id.txt"), []byte("55555555555555555\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubClientIDExeDir(t, install, nil)
	t.Chdir(cwd)

	wantResolve(t, 55555555555555555, sourceClientIDFile, "client-id.txt beside exe, CWD elsewhere")
}

// TestResolveClientIDFallsBackToCWD proves the pre-existing install keeps
// working: no file beside the exe, the file in the CWD still resolves.
func TestResolveClientIDFallsBackToCWD(t *testing.T) {
	stubClientIDEnv(t, "")

	install := t.TempDir() // no client-id.txt here
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "client-id.txt"), []byte("66666666666666666"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubClientIDExeDir(t, install, nil)
	t.Chdir(cwd)

	wantResolve(t, 66666666666666666, sourceClientIDFile, "CWD fallback")
}

// TestResolveClientIDExeDirErrorFallsThroughToCWDThenDefault proves an exe-dir
// lookup failure is not fatal: CWD, then the hardcoded default.
func TestResolveClientIDExeDirErrorFallsThroughToCWDThenDefault(t *testing.T) {
	stubClientIDEnv(t, "")

	cwd := t.TempDir()
	stubClientIDExeDir(t, "", errors.New("os.Executable: boom"))
	t.Chdir(cwd)

	wantResolve(t, defaultClientID, sourceDefault, "exe-dir error, empty CWD")

	if err := os.WriteFile(filepath.Join(cwd, "config.properties"), []byte("clientId=77777777777777777\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantResolve(t, 77777777777777777, sourceConfigProps, "exe-dir error, CWD config.properties")
}

// TestResolveClientIDEnvWinsOverExeDirAndCWD keeps the precedence head intact.
func TestResolveClientIDEnvWinsOverExeDirAndCWD(t *testing.T) {
	install := t.TempDir()
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(install, "client-id.txt"), []byte("88888888888888888"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "client-id.txt"), []byte("99999999999999999"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubClientIDExeDir(t, install, nil)
	t.Chdir(cwd)
	stubClientIDEnv(t, "12345678901234567")

	wantResolve(t, 12345678901234567, sourceEnv, "env beats exe dir and CWD")
}

// TestResolveClientIDSourceIsOneOfFourKnownNames walks every branch and asserts
// the reported source is a fixed name, never the id VALUE.
func TestResolveClientIDSourceIsOneOfFourKnownNames(t *testing.T) {
	install := t.TempDir()
	cwd := t.TempDir()
	stubClientIDExeDir(t, install, nil)
	t.Chdir(cwd)

	stubClientIDEnv(t, "12345678901234567")
	wantResolve(t, 12345678901234567, sourceEnv, "env")

	stubClientIDEnv(t, "")
	if err := os.WriteFile(filepath.Join(install, "client-id.txt"), []byte("11111111111111112"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantResolve(t, 11111111111111112, sourceClientIDFile, "client-id.txt")

	if err := os.Remove(filepath.Join(install, "client-id.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(install, "config.properties"), []byte("x=22222222222222223"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantResolve(t, 22222222222222223, sourceConfigProps, "config.properties")

	if err := os.Remove(filepath.Join(install, "config.properties")); err != nil {
		t.Fatal(err)
	}
	wantResolve(t, defaultClientID, sourceDefault, "default")
}
