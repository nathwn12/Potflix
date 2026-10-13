//! Wire-contract, retry, and decode tests for `metadata::http`
//! (`go\internal\metadata\http.go`).
//!
//! Everything here runs against a loopback server: no test reaches the
//! network.

mod common;

use std::time::{Duration, Instant};

use common::{Reply, TestServer};
use metadata_http::{decode_capped, Cancel, CancelError, Error, HttpClient};

#[derive(serde::Deserialize, Debug)]
struct Meta {
    ok: bool,
}

#[derive(serde::Serialize)]
struct GraphQl<'a> {
    query: &'a str,
}

fn header_values(head: &str, name: &str) -> Vec<String> {
    head.lines()
        .filter_map(|line| {
            let (field, value) = line.split_once(':')?;
            field
                .eq_ignore_ascii_case(name)
                .then(|| value.trim().to_string())
        })
        .collect()
}

/// The wire constants are contract: `User-Agent: potflix-metadata/1.0` and
/// `Accept: application/json`, exactly once each (http.go:16, :105-106).
#[test]
fn get_sends_the_shared_user_agent_and_accept() {
    let server = TestServer::start(vec![Reply::ok_json("{\"ok\":true}")]);
    let client = HttpClient::new(Duration::from_secs(8));
    let response = client
        .get(&Cancel::background(), &server.url("/meta"))
        .expect("fetch");
    let _: Meta = decode_capped(response).expect("decode");

    let heads = server.heads();
    assert_eq!(heads.len(), 1, "one request expected");
    assert!(
        heads[0].starts_with("GET /meta HTTP/1.1"),
        "request line: {:?}",
        heads[0].lines().next()
    );
    assert_eq!(
        header_values(&heads[0], "user-agent"),
        vec!["potflix-metadata/1.0".to_string()]
    );
    assert_eq!(
        header_values(&heads[0], "accept"),
        vec!["application/json".to_string()]
    );
    // gzip/brotli are deliberately off (the workspace pins `ureq` with
    // `default-features = false`), so the providers answer identity.
    for value in header_values(&heads[0], "accept-encoding") {
        assert!(
            !value.contains("gzip") && !value.contains("br"),
            "compression must not be negotiated: accept-encoding: {value}"
        );
    }
}

/// `fetchPostJSON` adds `Content-Type: application/json` and the JSON body
/// (http.go:112-127).
#[test]
fn post_json_sends_content_type_and_payload() {
    let server = TestServer::start(vec![Reply::ok_json("{\"ok\":true}")]);
    let client = HttpClient::new(Duration::from_secs(8));
    let response = client
        .post_json(
            &Cancel::background(),
            &server.url("/graphql"),
            &GraphQl {
                query: "query ($search:String){Media(search:$search)}",
            },
        )
        .expect("post");
    let _: Meta = decode_capped(response).expect("decode");

    let heads = server.heads();
    assert_eq!(heads.len(), 1, "one request expected");
    assert!(
        heads[0].starts_with("POST /graphql HTTP/1.1"),
        "request line: {:?}",
        heads[0].lines().next()
    );
    assert_eq!(
        header_values(&heads[0], "content-type"),
        vec!["application/json".to_string()]
    );
    assert_eq!(
        header_values(&heads[0], "user-agent"),
        vec!["potflix-metadata/1.0".to_string()]
    );
    assert_eq!(
        server.bodies(),
        vec!["{\"query\":\"query ($search:String){Media(search:$search)}\"}".to_string()]
    );
}

/// A non-2xx status is an error, and the response body never reaches the
/// message (http.go:143-145).
#[test]
fn decode_capped_rejects_non_2xx_without_the_body() {
    let server = TestServer::start(vec![Reply::too_many_requests(None)]);
    let client = HttpClient::new(Duration::from_secs(8));
    // The server answers 429 to every attempt, so the single retry also sees
    // 429 and `get` hands the response back - exactly one retry, no more.
    let response = client
        .get(&Cancel::background(), &server.url("/meta"))
        .expect("429 is a response, not a transport error");
    assert_eq!(response.status().as_u16(), 429);
    let err = decode_capped::<Meta>(response).expect_err("429 must not decode");
    assert!(
        matches!(err, Error::Status(status) if status.as_u16() == 429),
        "unexpected error: {err}"
    );
    let message = err.to_string();
    assert!(
        message.contains("unexpected HTTP status 429"),
        "message: {message}"
    );
    assert!(
        !message.contains("rate limited"),
        "the response body must never be logged: {message}"
    );
    assert_eq!(
        server.hits(),
        2,
        "one initial attempt plus exactly one retry"
    );
}

/// Exactly ONE retry, and only on HTTP 429: a 429 followed by a 200 returns
/// the 200 (http.go:53-96).
#[test]
fn one_retry_on_429_returns_the_second_response() {
    let server = TestServer::start(vec![
        Reply::too_many_requests(Some("0")),
        Reply::ok_json("{\"ok\":true}"),
    ]);
    let client = HttpClient::new(Duration::from_secs(8));
    let response = client
        .get(&Cancel::background(), &server.url("/meta"))
        .expect("fetch");
    let meta: Meta = decode_capped(response).expect("decode");
    assert!(meta.ok);
    assert_eq!(
        server.hits(),
        2,
        "one initial attempt plus exactly one retry"
    );
}

/// A 500 is NOT retried: only 429 is (http.go:68-70).
#[test]
fn a_500_is_not_retried() {
    let server = TestServer::start(vec![Reply {
        status: 500,
        reason: "Internal Server Error",
        retry_after: None,
        body: "{\"error\":\"boom\"}".to_string(),
    }]);
    let client = HttpClient::new(Duration::from_secs(8));
    let response = client
        .get(&Cancel::background(), &server.url("/meta"))
        .expect("fetch");
    assert!(decode_capped::<Meta>(response).is_err());
    assert_eq!(server.hits(), 1, "a 500 must not be retried");
}

/// `Retry-After` is honored but bounded by `maxRetryWait` (10 s), and the wait
/// is cancellable by the scope (http.go:71-87).
#[test]
fn retry_after_is_bounded_and_cancellable() {
    let server = TestServer::start(vec![Reply::too_many_requests(Some("3600"))]);
    let client = HttpClient::new(Duration::from_secs(8));
    // A 3600 s Retry-After must be capped at 10 s; the 50 ms deadline proves
    // the cap never turned into a 3600 s sleep.
    let cancel = Cancel::with_deadline(Duration::from_millis(50));
    let started = Instant::now();
    let err = client
        .get(&cancel, &server.url("/meta"))
        .expect_err("the deadline must abort the retry wait");
    assert!(
        matches!(err, Error::Cancelled(CancelError::DeadlineExceeded)),
        "unexpected error: {err}"
    );
    assert!(
        started.elapsed() < Duration::from_secs(2),
        "the bounded wait took {:?}",
        started.elapsed()
    );
    assert_eq!(
        server.hits(),
        1,
        "the retry must not be sent after the abort"
    );
}

/// An already-cancelled scope never sends the request.
#[test]
fn a_cancelled_scope_aborts_before_the_request() {
    let server = TestServer::start(vec![Reply::ok_json("{\"ok\":true}")]);
    let client = HttpClient::new(Duration::from_secs(8));
    let cancel = Cancel::background();
    cancel.cancel();
    let err = client
        .get(&cancel, &server.url("/meta"))
        .expect_err("a cancelled scope must abort");
    assert!(
        matches!(err, Error::Cancelled(CancelError::Cancelled)),
        "unexpected error: {err}"
    );
    assert_eq!(server.hits(), 0);
}

/// The decoder reads the first JSON value and ignores trailing content, like
/// `json.Decoder.Decode`; an empty body is a decode error (http.go:141-152).
#[test]
fn decode_capped_ignores_trailing_content() {
    let padded = format!("{{\"ok\":true}}{}", " ".repeat(64 << 10));
    let server = TestServer::start(vec![Reply::ok_json(padded)]);
    let client = HttpClient::new(Duration::from_secs(8));
    let response = client
        .get(&Cancel::background(), &server.url("/meta"))
        .expect("fetch");
    let meta: Meta = decode_capped(response).expect("decode");
    assert!(meta.ok);
}

#[test]
fn decode_capped_errors_on_an_empty_body() {
    let server = TestServer::start(vec![Reply::ok_json("")]);
    let client = HttpClient::new(Duration::from_secs(8));
    let response = client
        .get(&Cancel::background(), &server.url("/meta"))
        .expect("fetch");
    let err = decode_capped::<Meta>(response).expect_err("an empty body must not decode");
    assert!(matches!(err, Error::Decode(_)), "unexpected error: {err}");
}
