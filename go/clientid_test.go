package main

import (
	"os"
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
