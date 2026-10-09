package metadata

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	metadataUserAgent = "potflix-metadata/1.0"
	maxBodyBytes      = 4 << 20
	maxRetryWait      = 10 * time.Second
)

func defaultHTTPTimeout() time.Duration { return 8 * time.Second }

func newHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = defaultHTTPTimeout()
	}
	return &http.Client{Timeout: timeout}
}

func parseRetryAfter(h string, now time.Time) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// fetchWithRetry runs build+Do once, and on HTTP 429 retries exactly once,
// honoring Retry-After bounded by maxRetryWait.
func fetchWithRetry(ctx context.Context, client *http.Client, build func(ctx context.Context) (*http.Request, error)) (*http.Response, error) {
	req, err := build(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		return resp, nil
	}
	wait := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	if wait > maxRetryWait {
		wait = maxRetryWait
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if wait > 0 {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	} else if err := ctx.Err(); err != nil {
		return nil, err
	}
	req2, err := build(ctx)
	if err != nil {
		return nil, err
	}
	return client.Do(req2)
}

// fetchGet performs a GET with the shared User-Agent and one bounded 429 retry.
func fetchGet(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	return fetchWithRetry(ctx, client, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", metadataUserAgent)
		req.Header.Set("Accept", "application/json")
		return req, nil
	})
}

// fetchPostJSON POSTs a JSON payload with one bounded 429 retry.
func fetchPostJSON(ctx context.Context, client *http.Client, url string, payload any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return fetchWithRetry(ctx, client, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", metadataUserAgent)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	})
}

func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}

// decodeCapped decodes a JSON body capped at maxBodyBytes. Non-2xx statuses
// are errors; the body is never logged.
func decodeCapped(resp *http.Response, v any) error {
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("metadata: unexpected HTTP status %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(v); err != nil {
		return fmt.Errorf("metadata: decode response: %w", err)
	}
	return nil
}
