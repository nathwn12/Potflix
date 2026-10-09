package metadata

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestTestTMDBKey proves the cheap key check: 200 is nil, 401/403 is the
// rejected sentinel, a dead server is a network error, and no error string
// carries the key.
func TestTestTMDBKey(t *testing.T) {
	const key = "canary-value-not-a-secret-0000"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	oldBase := tmdbKeyTestBase
	defer func() { tmdbKeyTestBase = oldBase }()

	// 200 -> nil.
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/3/configuration" {
			t.Errorf("path = %q, want /3/configuration", r.URL.Path)
		}
		if r.URL.Query().Get("api_key") != key {
			t.Errorf("api_key missing from query")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"images":{}}`))
	}))
	defer ok.Close()
	tmdbKeyTestBase = ok.URL
	if err := TestTMDBKey(ctx, key); err != nil {
		t.Fatalf("200 TestTMDBKey = %v, want nil", err)
	}

	// 401 and 403 -> rejected.
	for _, code := range []int{401, 403} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"status_code":7}`))
		}))
		tmdbKeyTestBase = srv.URL
		err := TestTMDBKey(ctx, key)
		if !errors.Is(err, ErrTMDBKeyRejected) {
			t.Fatalf("status %d TestTMDBKey = %v, want ErrTMDBKeyRejected", code, err)
		}
		if err != nil && strings.Contains(err.Error(), key) {
			t.Fatalf("status %d error leaks key: %q", code, err)
		}
		srv.Close()
	}

	// Dead server -> network error (not rejected), still key-free.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	tmdbKeyTestBase = deadURL
	err := TestTMDBKey(ctx, key)
	if err == nil {
		t.Fatal("dead-server TestTMDBKey = nil, want a network error")
	}
	if errors.Is(err, ErrTMDBKeyRejected) {
		t.Fatalf("dead-server TestTMDBKey = %v, want a network error, not rejected", err)
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("dead-server error leaks key: %q", err)
	}
}
