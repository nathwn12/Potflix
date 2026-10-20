//! `metadata-http` -- the transport half of the Go package
//! `go\internal\metadata`: the shared HTTP client and fetch helpers, plus the
//! per-host fetch-spacing limiter.
//!
//! Workspace seam: split out of the single `metadata` module so the transport
//! (ureq over `native-tls`) stays separable from the provider logic. Nothing
//! here knows a provider, a query, or a cache record: it is
//! `metadata\{http.go,ratelimit.go}` only, and both are repo-dependency-free.
//!
//! Port map, Go -> Rust:
//!
//! | `go\internal\metadata` | here |
//! |---|---|
//! | `newHTTPClient(timeout)` | [`HttpClient::new`] |
//! | `fetchGet(ctx, client, url)` | [`HttpClient::get`] |
//! | `fetchPostJSON(ctx, client, url, payload)` | [`HttpClient::post_json`] (never retried) |
//! | read-only GraphQL search POST | [`HttpClient::post_json_idempotent`] (retried like GET) |
//! | `fetchWithRetry(ctx, client, build)` | [`HttpClient::fetch`] (private) |
//! | `decodeCapped(resp, &v)` | [`decode_capped`] |
//! | `drainAndClose(resp)` | [`drain_and_close`] |
//! | `parseRetryAfter(h, now)` | [`parse_retry_after`] |
//! | `waitForHost(ctx, host)` | [`wait_for_host`] |
//! | `ctx context.Context` | [`Cancel`] |
//!
//! The providers call `HttpClient::get` / `HttpClient::post_json_idempotent`
//! (read-only searches) and then `decode_capped`; mutating POSTs would use
//! `HttpClient::post_json` and never retry. `Cancel::background()` is Go's
//! `context.Background()` and a
//! cancelled or expired [`Cancel`] aborts a wait or a `Retry-After` sleep.

pub mod cancel;
pub mod http;
pub mod ratelimit;

pub use cancel::{Cancel, CancelError};
pub use http::{
    backoff_for_attempt, compute_retry_wait, decode_capped, drain_and_close, parse_retry_after,
    retry_wait_for_attempt, Error, HttpClient, ACCEPT_JSON, CONTENT_TYPE_JSON,
    DEFAULT_HTTP_TIMEOUT, DRAIN_BODY_CAP, MAX_429_RETRIES, MAX_BODY_BYTES, MAX_RETRY_WAIT,
    METADATA_USER_AGENT, RETRY_BASE_DELAY, RETRY_JITTER_CAP,
};
pub use ratelimit::{
    current_rate_profile, normalize_rate_host, normalize_rate_profile, rate_interval_for_host,
    reset_rate_limiter, set_host_interval, set_rate_limiting_enabled, set_rate_profile,
    wait_for_host, SCAN_SPEED_BALANCED, SCAN_SPEED_GENTLE, SCAN_SPEED_TURBO,
};
