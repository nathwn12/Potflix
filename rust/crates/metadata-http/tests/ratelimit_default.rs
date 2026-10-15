//! Port of `go\internal\metadata\ratelimit_default_test.go`.
//!
//! Deliberately its own test binary: it asserts the *initial* process state
//! (turbo, the published intervals) and must observe a process in which
//! nothing has called `set_rate_profile`. A separate integration-test file is
//! a separate process, which is the Rust equivalent of Go keeping this test in
//! its own file so it runs before the tests that mutate the global limiter.

use std::time::Duration;

use metadata_http::{current_rate_profile, rate_interval_for_host};

/// Proves the package default is 1x (turbo): a resolver constructed without an
/// explicit profile runs at the published intervals. It reads only, never
/// calls `set_rate_profile`.
#[test]
fn rate_limiter_initial_profile_is_turbo() {
    let profile = current_rate_profile();
    assert_eq!(
        profile, "turbo",
        "initial profile = {profile:?}, want turbo"
    );

    for (host, want) in [
        ("api.tvmaze.com", Duration::from_millis(500)),
        ("graphql.anilist.co", Duration::from_millis(2000)),
        ("api.jikan.moe", Duration::from_millis(350)),
        ("itunes.apple.com", Duration::from_millis(3000)),
        ("en.wikipedia.org", Duration::from_millis(1000)),
    ] {
        let got = rate_interval_for_host(host);
        assert_eq!(got, want, "initial host {host:?} = {got:?}, want {want:?}");
    }

    // An unlisted host is ungated, in every profile.
    assert_eq!(
        rate_interval_for_host("unknown-host-127-0-0-1.example"),
        Duration::ZERO
    );
}
