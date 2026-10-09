package metadata

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrTMDBKeyRejected is returned by TestTMDBKey when the key is rejected
// (HTTP 401/403). Callers distinguish it with errors.Is; it never carries
// the key.
var ErrTMDBKeyRejected = errors.New("metadata: tmdb key rejected")

// tmdbKeyTestBase overrides the API base for TestTMDBKey in tests (same
// package). Empty means the production base.
var tmdbKeyTestBase string

func tmdbKeyTestBaseURL() string {
	if tmdbKeyTestBase != "" {
		return tmdbKeyTestBase
	}
	return "https://api.themoviedb.org"
}

// TestTMDBKey performs one cheap authenticated call (GET /3/configuration)
// to validate key. A 2xx is nil; 401/403 is ErrTMDBKeyRejected; transport
// failures and other statuses are redacted generic errors. The returned
// error never contains the key (same redact discipline as tmdbSource).
func TestTMDBKey(ctx context.Context, key string) error {
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("metadata: tmdb test: no key")
	}
	s := &tmdbSource{base: tmdbKeyTestBaseURL(), key: key, client: newHTTPClient(0)}
	resp, err := s.tmdbGet(ctx, "/3/configuration", nil)
	if err != nil {
		return err
	}
	defer drainAndClose(resp)
	switch resp.StatusCode {
	case 401, 403:
		return ErrTMDBKeyRejected
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s.redact(fmt.Errorf("metadata: tmdb test unexpected HTTP status %s", resp.Status))
	}
	return nil
}
