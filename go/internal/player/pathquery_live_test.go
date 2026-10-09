package player

import (
	"os"
	"strings"
	"testing"
)

// pathquery_live_test.go - live proof for CurrentFile against the owner's
// running PotPlayer. Skipped unless POTFLIX_LIVE=1.

func TestLiveCurrentFile(t *testing.T) {
	if os.Getenv("POTFLIX_LIVE") != "1" {
		t.Skip("live PotPlayer query: set POTFLIX_LIVE=1 to run")
	}
	hwnd, found := FindFirst()
	if !found {
		t.Skip("live PotPlayer query: no PotPlayer window found")
	}
	path, err := CurrentFile(hwnd)
	if err != nil {
		t.Fatalf("CurrentFile(%#x) error: %v", hwnd, err)
	}
	t.Logf("live current file: %s", path)
	if path == "" || !strings.Contains(path, `:\`) {
		t.Fatalf("CurrentFile(%#x) = %q, want a non-empty absolute path", hwnd, path)
	}
}
