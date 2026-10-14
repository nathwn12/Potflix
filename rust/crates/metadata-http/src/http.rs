//! `metadata::http` -- mirrors `go\internal\metadata\http.go`.
//!
//! The shared HTTP client and fetch helpers: one wire contract for every
//! provider, one bounded retry, and a capped decoder.
//!
//! In Rust this is `ureq` with the `native-tls` provider (SChannel on
//! Windows) -- OS-native TLS, no C toolchain -- rooted in the platform
//! verifier (`RootCerts::PlatformVerifier`: the OS trust store, exactly like
//! Go's `http.Client`). `TlsConfig::builder()` starts from
//! `TlsConfig::default()`, whose `root_certs` default is `WebPki` (the
//! vendored Mozilla set, meant for the rustls provider); for the native-tls
//! backend that default switches the OS store OFF (`disable_built_in_roots`),
//! so the provider MUST be paired with `PlatformVerifier` or chains the OS
//! already trusts stop verifying (Cloudflare/Google-Trust hosts failed while
//! curl and .NET succeeded). Verification is never disabled: no
//! `disable_verification`, no `danger_*`. `ureq`'s other defaults are the
//! deliberate match for Go's `http.Client` here: `http_status_as_error(false)`
//! so a 429 reaches the retry instead of surfacing as a transport error, and
//! `timeout_global` as the whole-request budget. `gzip` is off (the workspace
//! pins `default-features = false`), so no `Accept-Encoding` is sent and the
//! providers answer identity -- also deliberate.
//!
//! Wire constants are contract, not tuning: `User-Agent:
//! potflix-metadata/1.0`, `Accept: application/json`, a 4 MiB body cap, a 10 s
//! retry-wait cap and a 512 KiB post-decode drain. A response body is NEVER
//! logged and never carried in an error message.

use std::fmt;
use std::io::{self, Read};
use std::time::{Duration, SystemTime};

use serde::de::DeserializeOwned;
use serde::Serialize;
use ureq::http::{Response, StatusCode, Uri};
use ureq::tls::{RootCerts, TlsConfig, TlsProvider};
use ureq::Body;

use crate::cancel::{Cancel, CancelError};
use crate::ratelimit::wait_for_host;

/// The shared `User-Agent` every provider sends (http.go:16).
pub const METADATA_USER_AGENT: &str = "potflix-metadata/1.0";
/// The shared `Accept` (http.go:106).
pub const ACCEPT_JSON: &str = "application/json";
/// The POST payload media type (http.go:124).
pub const CONTENT_TYPE_JSON: &str = "application/json";
/// Response body cap (http.go:17).
pub const MAX_BODY_BYTES: u64 = 4 << 20;
/// Upper bound on an honored `Retry-After` (http.go:18).
pub const MAX_RETRY_WAIT: Duration = Duration::from_secs(10);
/// Post-decode drain cap that returns a connection to the pool (http.go:22).
pub const DRAIN_BODY_CAP: u64 = 512 << 10;
/// Pre-close drain cap (http.go:75, :133, :144).
const CLOSE_DRAIN_BYTES: u64 = 64 << 10;
/// `defaultHTTPTimeout` (http.go:25).
pub const DEFAULT_HTTP_TIMEOUT: Duration = Duration::from_secs(8);

/// A transport, request-build, cancellation, or decode failure.
///
/// `Status` and `Decode` carry only the status line or the JSON error: a
/// response body must never reach an error message or a log.
#[derive(Debug)]
pub enum Error {
    /// `ctx.Err()`: the scope was cancelled or its deadline passed.
    Cancelled(CancelError),
    /// The request could not be built (an invalid URL).
    Url(String),
    /// The POST payload could not be serialized (`json.Marshal` failure).
    Encode(serde_json::Error),
    /// The request failed in transport (`client.Do` failure).
    Transport(ureq::Error),
    /// A non-2xx status (`decodeCapped`).
    Status(StatusCode),
    /// The JSON body did not decode (`json.Decode` failure).
    Decode(serde_json::Error),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::Cancelled(reason) => write!(f, "metadata: {reason}"),
            Error::Url(reason) => write!(f, "metadata: invalid request URL: {reason}"),
            Error::Encode(err) => write!(f, "metadata: encode request: {err}"),
            Error::Transport(err) => write!(f, "metadata: request failed: {err}"),
            Error::Status(status) => {
                write!(f, "metadata: unexpected HTTP status {status}")
            }
            Error::Decode(err) => write!(f, "metadata: decode response: {err}"),
        }
    }
}

impl std::error::Error for Error {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Error::Cancelled(reason) => Some(reason),
            Error::Encode(err) | Error::Decode(err) => Some(err),
            Error::Transport(err) => Some(err),
            Error::Url(_) | Error::Status(_) => None,
        }
    }
}

/// `newHTTPClient`: the shared client every provider fetches through. Cloning
/// it shares the connection pool, so clone it rather than building another
/// (http.go:27).
#[derive(Clone, Debug)]
pub struct HttpClient {
    agent: ureq::Agent,
}

impl HttpClient {
    /// A client whose whole-request budget is `timeout`; a zero timeout takes
    /// [`DEFAULT_HTTP_TIMEOUT`] (http.go:28).
    pub fn new(timeout: Duration) -> Self {
        let timeout = if timeout.is_zero() {
            DEFAULT_HTTP_TIMEOUT
        } else {
            timeout
        };
        let config = ureq::Agent::config_builder()
            .timeout_global(Some(timeout))
            .tls_config(
                TlsConfig::builder()
                    .provider(TlsProvider::NativeTls)
                    // PlatformVerifier is the OS trust store (SChannel here),
                    // i.e. Go's `http.Client` posture. Without it the builder
                    // keeps ureq's `WebPki` default, which for native-tls
                    // disables the OS roots and breaks chains the OS trusts.
                    .root_certs(RootCerts::PlatformVerifier)
                    .build(),
            )
            // Go's `client.Do` hands back every status; non-2xx is
            // `decode_capped`'s error and a 429 is the retry's trigger.
            .http_status_as_error(false)
            .build();
        HttpClient {
            agent: config.into(),
        }
    }

    /// `fetchGet`: a GET with the shared `User-Agent`/`Accept` and one bounded
    /// 429 retry (http.go:99).
    pub fn get(&self, cancel: &Cancel, url: &str) -> Result<Response<Body>, Error> {
        self.fetch(cancel, url, None)
    }

    /// `fetchPostJSON`: a JSON POST with the shared `User-Agent`/`Accept`, a
    /// `Content-Type: application/json` and one bounded 429 retry
    /// (http.go:112).
    pub fn post_json<T: Serialize>(
        &self,
        cancel: &Cancel,
        url: &str,
        payload: &T,
    ) -> Result<Response<Body>, Error> {
        let body = serde_json::to_vec(payload).map_err(Error::Encode)?;
        self.fetch(cancel, url, Some(body))
    }

    /// `fetchWithRetry`: runs the request once, and on HTTP 429 retries
    /// exactly once, honoring `Retry-After` bounded by [`MAX_RETRY_WAIT`].
    /// Every attempt is spaced by the per-host limiter (unknown hosts go
    /// ungated) (http.go:56).
    fn fetch(
        &self,
        cancel: &Cancel,
        url: &str,
        payload: Option<Vec<u8>>,
    ) -> Result<Response<Body>, Error> {
        let uri: Uri = url
            .parse()
            .map_err(|err: ureq::http::uri::InvalidUri| Error::Url(err.to_string()))?;
        let response = self.attempt(cancel, &uri, payload.as_deref())?;
        if response.status().as_u16() != StatusCode::TOO_MANY_REQUESTS.as_u16() {
            return Ok(response);
        }
        let retry_after = response
            .headers()
            .get("Retry-After")
            .and_then(|value| value.to_str().ok())
            .unwrap_or_default();
        let wait = parse_retry_after(retry_after, SystemTime::now()).min(MAX_RETRY_WAIT);
        drain_and_close(response);
        // Go bounds the wait, then a zero wait still checks `ctx.Err()` - which
        // is exactly what a zero-length `wait` does here.
        cancel.wait(wait).map_err(Error::Cancelled)?;
        self.attempt(cancel, &uri, payload.as_deref())
    }

    fn attempt(
        &self,
        cancel: &Cancel,
        uri: &Uri,
        payload: Option<&[u8]>,
    ) -> Result<Response<Body>, Error> {
        wait_for_host(cancel, uri.host().unwrap_or_default()).map_err(Error::Cancelled)?;
        // Go's `client.Do` honors the request context: an already-cancelled or
        // expired scope fails the round trip instead of reaching the wire, and
        // the limiter does not check the scope for an ungated host.
        if let Some(reason) = cancel.err() {
            return Err(Error::Cancelled(reason));
        }
        // `ureq` type-states the request body, so the two wire shapes are
        // built separately: same headers, same contract as Go's one closure.
        let result = match payload {
            Some(body) => self
                .agent
                .post(uri.clone())
                .header("User-Agent", METADATA_USER_AGENT)
                .header("Accept", ACCEPT_JSON)
                .header("Content-Type", CONTENT_TYPE_JSON)
                .send(body),
            None => self
                .agent
                .get(uri.clone())
                .header("User-Agent", METADATA_USER_AGENT)
                .header("Accept", ACCEPT_JSON)
                .call(),
        };
        result.map_err(Error::Transport)
    }
}

/// `parseRetryAfter`: seconds, or an HTTP-date, or zero when the value is
/// empty, malformed, or already in the past (http.go:34).
pub fn parse_retry_after(value: &str, now: SystemTime) -> Duration {
    let value = value.trim();
    if value.is_empty() {
        return Duration::ZERO;
    }
    if let Ok(secs) = value.parse::<i64>() {
        return if secs < 0 {
            Duration::ZERO
        } else {
            Duration::from_secs(secs as u64)
        };
    }
    if let Some(at) = parse_http_date(value)
        && let Ok(wait) = at.duration_since(now)
    {
        return wait;
    }
    Duration::ZERO
}

/// `drainAndClose`: reads a bounded prefix of the body and drops the response,
/// so a body the caller did not consume does not cost the pooled connection
/// (http.go:129).
pub fn drain_and_close(response: Response<Body>) {
    let mut body = response.into_body();
    drain(&mut body, CLOSE_DRAIN_BYTES);
}

/// `decodeCapped`: decodes a JSON body capped at [`MAX_BODY_BYTES`]. A non-2xx
/// status is an error; the body is never logged. The remainder is drained
/// (bounded by [`DRAIN_BODY_CAP`]) before the response is dropped so the
/// connection is returned to the pool; a body past the cap is legitimately
/// discarded (http.go:141).
pub fn decode_capped<T: DeserializeOwned>(response: Response<Body>) -> Result<T, Error> {
    let status = response.status();
    let mut body = response.into_body();
    if !status.is_success() {
        drain(&mut body, CLOSE_DRAIN_BYTES);
        return Err(Error::Status(status));
    }
    // Stream the value out of a capped reader: the decoder stops at the end of
    // the first JSON value, exactly like `json.Decoder.Decode`, leaving the
    // rest of the body for the drain below. Trailing content is not an error
    // here, which is also Go's behaviour.
    let decoded = {
        let reader = body.as_reader().take(MAX_BODY_BYTES);
        let mut deserializer = serde_json::Deserializer::from_reader(reader);
        T::deserialize(&mut deserializer)
    };
    drain(&mut body, DRAIN_BODY_CAP);
    decoded.map_err(Error::Decode)
}

fn drain(body: &mut Body, cap: u64) {
    let mut reader = body.as_reader().take(cap);
    let _ = io::copy(&mut reader, &mut io::sink());
}

/// `http.ParseTime`: RFC 1123 (`Sun, 06 Nov 1994 08:49:37 GMT`), RFC 850
/// (`Sunday, 06-Nov-94 08:49:37 GMT`) and ANSI C asctime
/// (`Sun Nov  6 08:49:37 1994`).
///
/// Named reading: Go tries the three layouts with `time.Parse` and also
/// accepts a named zone in place of `GMT`; this accepts a `GMT`/`UTC` zone
/// only, which is what RFC 7231 requires of a `Retry-After` date.
fn parse_http_date(value: &str) -> Option<SystemTime> {
    if !value.is_ascii() {
        return None;
    }
    let normalized = value.replace([',', '-'], " ");
    let fields: Vec<&str> = normalized.split_whitespace().collect();
    let (year, month, day, time) = match fields.as_slice() {
        // RFC 1123 / RFC 850, both with a zone.
        [_weekday, day, month, year, time, zone] => {
            if !is_gmt(zone) {
                return None;
            }
            (*year, *month, *day, *time)
        }
        // ANSI C asctime, no zone.
        [_weekday, month, day, time, year] => (*year, *month, *day, *time),
        _ => return None,
    };

    let year: i64 = match year.len() {
        4 => year.parse().ok()?,
        // Go's stdYear rule: 2-digit years below 69 mean 2000+, the rest 1900+.
        2 => {
            let short: i64 = year.parse().ok()?;
            if short >= 69 {
                1900 + short
            } else {
                2000 + short
            }
        }
        _ => return None,
    };
    let month = month_number(month)?;
    let day: i64 = day.parse().ok()?;
    if !(1..=31).contains(&day) {
        return None;
    }
    let (hour, minute, second) = parse_clock(time)?;
    // Day-of-month is not checked against the month length: Go's parser
    // normalizes an overlong day rather than rejecting it, and a `Retry-After`
    // date in the wild is always well formed.
    let days = days_from_civil(year, month, day);
    let secs = days * 86_400 + hour * 3600 + minute * 60 + second;
    if secs < 0 {
        return None;
    }
    Some(SystemTime::UNIX_EPOCH + Duration::from_secs(secs as u64))
}

fn is_gmt(zone: &str) -> bool {
    zone.eq_ignore_ascii_case("GMT") || zone.eq_ignore_ascii_case("UTC")
}

fn month_number(name: &str) -> Option<i64> {
    const MONTHS: [&str; 12] = [
        "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec",
    ];
    MONTHS
        .iter()
        .position(|month| month.eq_ignore_ascii_case(name))
        .map(|index| index as i64 + 1)
}

fn parse_clock(time: &str) -> Option<(i64, i64, i64)> {
    let mut parts = time.split(':');
    // `time.Parse` accepts one- or two-digit fields.
    let hour: i64 = parts.next()?.parse().ok()?;
    let minute: i64 = parts.next()?.parse().ok()?;
    let second: i64 = parts.next()?.parse().ok()?;
    if parts.next().is_some() {
        return None;
    }
    // 60 is a leap second; it is accepted and treated as the following minute.
    if !(0..=23).contains(&hour) || !(0..=59).contains(&minute) || !(0..=60).contains(&second) {
        return None;
    }
    Some((hour, minute, second))
}

/// Days from 1970-01-01 for a proleptic Gregorian date (Howard Hinnant's
/// `days_from_civil`), verified against the epoch arithmetic of Go's `time`.
fn days_from_civil(year: i64, month: i64, day: i64) -> i64 {
    let year = if month <= 2 { year - 1 } else { year };
    let era = if year >= 0 { year } else { year - 399 } / 400;
    let year_of_era = year - era * 400; // [0, 399]
    let month_index = (month + 9) % 12; // Mar = 0
    let day_of_year = (153 * month_index + 2) / 5 + day - 1; // [0, 365]
    let day_of_era = year_of_era * 365 + year_of_era / 4 - year_of_era / 100 + day_of_year; // [0, 146096]
    era * 146_097 + day_of_era - 719_468
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn retry_after_seconds_dates_and_junk() {
        let now = SystemTime::UNIX_EPOCH + Duration::from_secs(1_000_000);
        assert_eq!(parse_retry_after("", now), Duration::ZERO);
        assert_eq!(parse_retry_after("   ", now), Duration::ZERO);
        assert_eq!(parse_retry_after("5", now), Duration::from_secs(5));
        assert_eq!(parse_retry_after(" 12 ", now), Duration::from_secs(12));
        assert_eq!(parse_retry_after("-3", now), Duration::ZERO);
        assert_eq!(parse_retry_after("soon", now), Duration::ZERO);
        // 2001-09-09 01:46:40 UTC, from the 1_000_000_000 s epoch second.
        let at = SystemTime::UNIX_EPOCH + Duration::from_secs(1_000_000_000);
        assert_eq!(
            parse_retry_after("Sun, 09 Sep 2001 01:46:40 GMT", at),
            Duration::ZERO
        );
        assert_eq!(
            parse_retry_after(
                "Sun, 09 Sep 2001 01:46:40 GMT",
                at - Duration::from_secs(30)
            ),
            Duration::from_secs(30)
        );
        // RFC 850 and asctime name the same instant as RFC 1123.
        assert_eq!(
            parse_retry_after(
                "Sunday, 09-Sep-01 01:46:40 GMT",
                at - Duration::from_secs(30)
            ),
            Duration::from_secs(30)
        );
        assert_eq!(
            parse_retry_after("Sun Sep  9 01:46:40 2001", at - Duration::from_secs(30)),
            Duration::from_secs(30)
        );
        assert_eq!(
            parse_retry_after(
                "Sun, 09 Sep 2001 01:46:40 PST",
                at - Duration::from_secs(30)
            ),
            Duration::ZERO
        );
    }
}
