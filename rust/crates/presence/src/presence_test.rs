//! `presence_test` -- mirrors `go\internal\presence\presence_test.go`.
//!
//! Table-driven tests over the transition matrix, plus the debounce deviation and
//! the payload helpers. Expectations are derived from the port contract
//! (section 4) and the Java PresenceTracker ported in `presence.go`.

use player::control::PlayStatus;
use titleclean::trunc;

use crate::{
    Action, Activity, Button, PAUSED_MARKER, Resolved, STATE_SYNOPSIS_BUDGET, Tracker, action_tag,
    build_activity, build_activity_resolved, fmt_ms, rp_text, smart_trim_synopsis, trim_word,
    with_state_time,
};

/// One poll of the transition matrix.
struct Sample {
    title: &'static str,
    status: PlayStatus,
    ct: i64,
    now: i64,
}

/// One expectation: `tag` is `""` for no emit.
#[derive(Clone, Copy)]
struct Want {
    tag: &'static str,
    start: i64,
    has_start: bool,
}

fn none() -> Want {
    Want {
        tag: "",
        start: 0,
        has_start: false,
    }
}

fn run_w(start: i64) -> Want {
    Want {
        tag: "RUN",
        start,
        has_start: true,
    }
}

fn pause_w() -> Want {
    Want {
        tag: "PAUSE",
        start: 0,
        has_start: false,
    }
}

fn stop_w() -> Want {
    Want {
        tag: "STOP",
        start: 0,
        has_start: false,
    }
}

/// Reproduces the contract's `decision ...` log line (events.go's
/// `eventDecision` shape). The tracker itself does not log; this is the test's
/// rendering of each poll, so the matrix produces a readable decision log.
fn decision_line(s: &Sample, a: Option<&Action>) -> String {
    match a {
        None => format!(
            "decision ms={} st={} ct={} title=[{}] action=null",
            s.now, s.status, s.ct, s.title
        ),
        Some(a) => format!(
            "decision ms={} st={} ct={} title=[{}] action={} running={} paused={} hasStart={} start={} suf={} ct={}",
            s.now,
            s.status,
            s.ct,
            s.title,
            a.tag(),
            a.running,
            a.paused,
            a.has_start,
            a.start,
            a.suffix,
            a.ct
        ),
    }
}

/// Runs one sequence and reports every mismatch (Go's `t.Errorf` + `continue`),
/// with the decision log attached.
fn run_sequence(name: &str, samples: &[Sample], wants: &[Want]) {
    assert_eq!(
        samples.len(),
        wants.len(),
        "{name}: {} samples vs {} wants",
        samples.len(),
        wants.len()
    );
    let mut tr = Tracker::default();
    let mut log: Vec<String> = Vec::with_capacity(samples.len());
    let mut failures: Vec<String> = Vec::new();
    for (i, s) in samples.iter().enumerate() {
        let a = tr.poll(s.title, s.status, s.ct, s.now);
        log.push(decision_line(s, a.as_ref()));

        let got_tag = action_tag(a.as_ref());
        let want_tag = if wants[i].tag.is_empty() {
            "null"
        } else {
            wants[i].tag
        };
        if got_tag != want_tag {
            failures.push(format!("{name}[{i}]: tag={got_tag} want={want_tag}"));
            continue;
        }
        let Some(a) = a.as_ref() else {
            continue;
        };
        if a.has_start != wants[i].has_start || a.start != wants[i].start {
            failures.push(format!(
                "{name}[{i}]: hasStart={} start={} want hasStart={} start={}",
                a.has_start, a.start, wants[i].has_start, wants[i].start
            ));
        }
    }
    assert!(
        failures.is_empty(),
        "{name} decision log:\n{}\n{}",
        log.join("\n"),
        failures.join("\n")
    );
}

#[test]
fn transition_matrix_running_advance_pause_stop() {
    const TA: &str = "A - PotPlayer";
    run_sequence(
        "A",
        &[
            Sample {
                title: TA,
                status: PlayStatus::RUNNING,
                ct: 1000,
                now: 100000,
            },
            Sample {
                title: TA,
                status: PlayStatus::RUNNING,
                ct: 1250,
                now: 100250,
            },
            Sample {
                title: TA,
                status: PlayStatus::PAUSED,
                ct: 1250,
                now: 100500,
            },
            Sample {
                title: TA,
                status: PlayStatus::PAUSED,
                ct: 1250,
                now: 100750,
            },
            Sample {
                title: TA,
                status: PlayStatus::RUNNING,
                ct: 1260,
                now: 101000,
            },
            Sample {
                title: TA,
                status: PlayStatus::STOPPED,
                ct: 0,
                now: 101250,
            },
            Sample {
                title: TA,
                status: PlayStatus::STOPPED,
                ct: 0,
                now: 101500,
            },
        ],
        &[
            // Index 2 is a Running->Paused report while ct still moves in the
            // window: flap suppression keeps it Running, so Pause lands on 3.
            run_w(99000),
            none(),
            none(),
            pause_w(),
            run_w(99740),
            stop_w(),
            none(),
        ],
    );
}

#[test]
fn transition_matrix_seek_and_debounce() {
    const TB: &str = "B - PotPlayer";
    run_sequence(
        "B",
        &[
            Sample {
                title: TB,
                status: PlayStatus::RUNNING,
                ct: 1000,
                now: 200000,
            },
            Sample {
                title: TB,
                status: PlayStatus::RUNNING,
                ct: 5000,
                now: 200250,
            },
            Sample {
                title: TB,
                status: PlayStatus::RUNNING,
                ct: 9000,
                now: 201000,
            },
            Sample {
                title: TB,
                status: PlayStatus::RUNNING,
                ct: 9200,
                now: 201250,
            },
        ],
        &[run_w(199000), none(), run_w(192000), none()],
    );
}

#[test]
fn transition_matrix_flap_suppression() {
    const TC: &str = "C - PotPlayer";
    run_sequence(
        "C",
        &[
            Sample {
                title: TC,
                status: PlayStatus::RUNNING,
                ct: 1000,
                now: 300000,
            },
            Sample {
                title: TC,
                status: PlayStatus::RUNNING,
                ct: 1600,
                now: 300250,
            },
            // glitch: still moving -> Running
            Sample {
                title: TC,
                status: PlayStatus::PAUSED,
                ct: 2200,
                now: 300500,
            },
            // still moving -> Running
            Sample {
                title: TC,
                status: PlayStatus::PAUSED,
                ct: 2200,
                now: 300750,
            },
            // window flushed -> real Pause
            Sample {
                title: TC,
                status: PlayStatus::PAUSED,
                ct: 2200,
                now: 301000,
            },
            Sample {
                title: TC,
                status: PlayStatus::PAUSED,
                ct: 2200,
                now: 301250,
            },
            // paused seek
            Sample {
                title: TC,
                status: PlayStatus::PAUSED,
                ct: 4000,
                now: 301500,
            },
            Sample {
                title: TC,
                status: PlayStatus::PAUSED,
                ct: 4000,
                now: 301750,
            },
        ],
        &[
            run_w(299000),
            none(),
            none(),
            none(),
            pause_w(),
            none(),
            pause_w(),
            none(),
        ],
    );
}

#[test]
fn transition_matrix_title_change_reset() {
    const TD: &str = "D - PotPlayer";
    const TE: &str = "E - PotPlayer";
    run_sequence(
        "D",
        &[
            Sample {
                title: TD,
                status: PlayStatus::RUNNING,
                ct: 1000,
                now: 400000,
            },
            Sample {
                title: TE,
                status: PlayStatus::RUNNING,
                ct: 500,
                now: 400250,
            },
            Sample {
                title: TE,
                status: PlayStatus::RUNNING,
                ct: 750,
                now: 400500,
            },
        ],
        &[run_w(399000), run_w(399750), none()],
    );
}

#[test]
fn transition_matrix_undefined_carries_nothing() {
    const TE: &str = "E - PotPlayer";
    run_sequence(
        "E",
        &[
            Sample {
                title: TE,
                status: PlayStatus::RUNNING,
                ct: 1000,
                now: 500000,
            },
            Sample {
                title: TE,
                status: PlayStatus::UNDEFINED,
                ct: 0,
                now: 500250,
            },
            Sample {
                title: TE,
                status: PlayStatus::UNDEFINED,
                ct: 0,
                now: 500500,
            },
            // unchanged: lastStatus still Running
            Sample {
                title: TE,
                status: PlayStatus::RUNNING,
                ct: 1250,
                now: 500750,
            },
            // ct<=0 -> no movement, real transition
            Sample {
                title: TE,
                status: PlayStatus::STOPPED,
                ct: 0,
                now: 501000,
            },
            Sample {
                title: TE,
                status: PlayStatus::STOPPED,
                ct: 0,
                now: 501250,
            },
        ],
        &[run_w(499000), none(), none(), none(), stop_w(), none()],
    );
}

#[test]
fn transition_matrix_undefined_first_then_running() {
    const TF: &str = "F - PotPlayer";
    run_sequence(
        "F",
        &[
            Sample {
                title: TF,
                status: PlayStatus::UNDEFINED,
                ct: 0,
                now: 600000,
            },
            Sample {
                title: TF,
                status: PlayStatus::RUNNING,
                ct: 1000,
                now: 600250,
            },
            Sample {
                title: TF,
                status: PlayStatus::STOPPED,
                ct: 0,
                now: 600500,
            },
            Sample {
                title: TF,
                status: PlayStatus::STOPPED,
                ct: 0,
                now: 600750,
            },
        ],
        &[none(), run_w(599250), stop_w(), none()],
    );
}

#[test]
fn transition_matrix_paused_ct_not_ok() {
    const TG: &str = "G - PotPlayer";
    run_sequence(
        "G",
        &[
            Sample {
                title: TG,
                status: PlayStatus::PAUSED,
                ct: 0,
                now: 700000,
            },
            Sample {
                title: TG,
                status: PlayStatus::PAUSED,
                ct: 0,
                now: 700250,
            },
            Sample {
                title: TG,
                status: PlayStatus::PAUSED,
                ct: 1000,
                now: 700500,
            },
        ],
        &[none(), none(), pause_w()],
    );
}

#[test]
fn transition_matrix_initial_stopped() {
    const TH: &str = "H - PotPlayer";
    run_sequence(
        "H",
        &[
            Sample {
                title: TH,
                status: PlayStatus::STOPPED,
                ct: 0,
                now: 800000,
            },
            Sample {
                title: TH,
                status: PlayStatus::STOPPED,
                ct: 0,
                now: 800250,
            },
        ],
        &[stop_w(), none()],
    );
}

#[test]
fn run_debounce() {
    // A seek inside the ~1 s window, same title, is HELD (trailing edge) and
    // lands when the window expires - it is no longer dropped forever, which is
    // what froze the card on a stale anchor.
    const X: &str = "X - PotPlayer";
    let mut tr = Tracker::default();
    let a = tr.poll(X, PlayStatus::RUNNING, 1000, 0);
    assert!(
        a.as_ref().is_some_and(|a| a.running),
        "first RUN should emit, got {a:?}"
    );
    let a = tr.poll(X, PlayStatus::RUNNING, 5000, 200);
    assert!(
        a.is_none(),
        "seek within 1s same title should be held, got {a:?}"
    );
    assert!(
        tr.pending_debounce(),
        "held seek should report pending_debounce"
    );
    let a = tr.poll(X, PlayStatus::RUNNING, 5000, 900);
    assert!(
        a.is_none(),
        "seek still inside the window should be held, got {a:?}"
    );
    // The window expires: the held seek lands with a fresh anchor.
    let a = tr.poll(X, PlayStatus::RUNNING, 5200, 1001);
    match a.as_ref() {
        Some(a) if a.running => assert_eq!(
            a.start,
            1001 - 5200,
            "trailing-edge anchor start={}, want {}",
            a.start,
            1001 - 5200
        ),
        other => panic!("held seek should land after the window, got {other:?}"),
    }
    assert!(
        !tr.pending_debounce(),
        "pending flag should clear after the held seek lands"
    );

    // A title change bypasses the debounce even inside the window.
    let a = tr.poll("Y - PotPlayer", PlayStatus::RUNNING, 500, 1050);
    assert!(
        a.as_ref().is_some_and(|a| a.running),
        "title change should bypass debounce, got {a:?}"
    );

    // After the window, a seek with the same title emits again.
    const Z: &str = "Z - PotPlayer";
    let mut tr2 = Tracker::default();
    let _ = tr2.poll(Z, PlayStatus::RUNNING, 1000, 0);
    let a = tr2.poll(Z, PlayStatus::RUNNING, 3000, 999);
    assert!(a.is_none(), "seek at +999ms should be held, got {a:?}");
    let a = tr2.poll(Z, PlayStatus::RUNNING, 5000, 1200);
    assert!(
        a.as_ref().is_some_and(|a| a.running),
        "seek after the window should emit, got {a:?}"
    );
}

/// F2: the trailing-edge debounce runs on the monotonic clock too, so a backward
/// wall-clock step cannot strand a held seek until the wall clock catches up.
#[test]
fn run_debounce_backward_clock_step() {
    const X: &str = "X - PotPlayer";
    let mut tr = Tracker::default();
    let a = tr.poll(X, PlayStatus::RUNNING, 1000, 100000);
    assert!(
        a.as_ref().is_some_and(|a| a.running),
        "first RUN should emit, got {a:?}"
    );
    // A seek 200 ms later is held inside the ~1 s window.
    let a = tr.poll(X, PlayStatus::RUNNING, 5000, 100200);
    assert!(
        a.is_none(),
        "seek inside the window should be held, got {a:?}"
    );
    // The wall clock steps back 55 s: the held seek is still inside the window.
    let a = tr.poll(X, PlayStatus::RUNNING, 5000, 45200);
    assert!(
        a.is_none(),
        "seek should still be held right after the step, got {a:?}"
    );
    // ~1 s of monotonic interval later it lands, rather than waiting 55 s.
    let a = tr.poll(X, PlayStatus::RUNNING, 5000, 46300);
    assert!(
        a.as_ref().is_some_and(|a| a.running),
        "held seek stranded after a backward step, got {a:?}"
    );
}

#[test]
fn paused_not_debounced() {
    const S: &str = "T - PotPlayer";
    let mut tr = Tracker::default();
    let a = tr.poll(S, PlayStatus::PAUSED, 1000, 0);
    assert!(
        a.as_ref().is_some_and(|a| a.paused),
        "initial pause should emit, got {a:?}"
    );
    let a = tr.poll(S, PlayStatus::PAUSED, 4000, 100);
    assert!(
        a.as_ref().is_some_and(|a| a.paused),
        "paused seek should emit (no debounce), got {a:?}"
    );
}

#[test]
fn fmt_ms_cases() {
    let cases: [(i64, &str); 10] = [
        (0, "00:00"),
        (999, "00:00"),
        (1000, "00:01"),
        (59000, "00:59"),
        (60000, "01:00"),
        (61000, "01:01"),
        (3599000, "59:59"),
        (3600000, "1:00:00"),
        (3661000, "1:01:01"),
        (7322000, "2:02:02"),
    ];
    for (ms, want) in cases {
        assert_eq!(fmt_ms(ms), want, "fmt_ms({ms})");
    }
}

#[test]
fn with_state_time_cases() {
    let cases: [(&str, &str, &str); 7] = [
        ("S01 E01", " - 01:23", "S01 E01 - 01:23"),
        ("", " - 01:23", "01:23"),
        ("", "", ""),
        ("ctx", "", "ctx"),
        ("", " - ", ""),
        ("", "  -  01:23", "01:23"),
        ("", "\t-\t01:00", "01:00"),
    ];
    for (context, suffix, want) in cases {
        assert_eq!(
            with_state_time(context, suffix),
            want,
            "with_state_time({context:?},{suffix:?})"
        );
    }

    let long = "x".repeat(200);
    let got = with_state_time(&long, " - 00:01");
    assert_eq!(
        got,
        format!("{}{}", trunc(&long, 128 - " - 00:01".len()), " - 00:01"),
        "long context should be truncated to leave room for the suffix: {got:?}"
    );
    assert!(
        got.ends_with(" - 00:01"),
        "the suffix must survive truncation: {got:?}"
    );
    let n = java_units(&got);
    assert!(n <= 128, "state length {n} exceeds the 128 limit: {got:?}");
}

/// `String.length()`: UTF-16 code units, the unit the Java-exact
/// `titleclean::trunc` limit is measured in.
fn java_units(s: &str) -> usize {
    s.encode_utf16().count()
}

/// F3: the paused marker must never be truncated. A 120-unit context plus the
/// 9-unit " - Paused" marker is 129 units; the old concatenate-then-truncate
/// rendered the state as "... - Paus...". The budgeted context keeps the marker
/// whole and the total within 128.
#[test]
fn paused_marker_survives_context_cap() {
    let ctx = "x".repeat(120);
    let got = with_state_time(&ctx, PAUSED_MARKER);
    assert!(
        got.ends_with(PAUSED_MARKER),
        "paused marker was truncated: {got:?}"
    );
    let n = java_units(&got);
    assert!(n <= 128, "state length {n} exceeds the 128 limit: {got:?}");
}

#[test]
fn build_activity_cases() {
    // Netflix-style mapping: the header (name) is the media title, the first
    // body line (details) is the episode name when known, else the episode tag,
    // else the title, and the second body line (state) is the episode tag when it
    // adds information, else empty.
    const TV_RAW: &str = "Show.S01E02.Pilot.1080p.mkv - PotPlayer";
    const TV_TAG_ONLY_RAW: &str = "Show.S01E02.1080p.mkv - PotPlayer";
    const MOVIE_RAW: &str = "Movie.mkv - PotPlayer";
    struct Case {
        name: &'static str,
        raw: String,
        act: Option<Action>,
        want: Activity,
        start: i64,
    }
    let running = |start: i64| {
        Some(Action {
            running: true,
            has_start: true,
            start,
            ct: 1000,
            ..Action::default()
        })
    };
    let cases = vec![
        Case {
            name: "movie running",
            raw: MOVIE_RAW.to_string(),
            act: running(12345),
            want: Activity {
                type_: "PLAYING".to_string(),
                name: "Movie".to_string(),
                details: "Movie".to_string(),
                state: String::new(),
                large_image: "logo".to_string(),
                large_text: "PotFlix".to_string(),
                has_start: true,
                ..Activity::default()
            },
            start: 12345,
        },
        Case {
            name: "tv with episode name and tag",
            raw: TV_RAW.to_string(),
            act: running(12345),
            want: Activity {
                type_: "PLAYING".to_string(),
                name: "Show".to_string(),
                details: "Pilot".to_string(),
                state: "S1 E2".to_string(),
                large_image: "logo".to_string(),
                large_text: "PotFlix".to_string(),
                has_start: true,
                ..Activity::default()
            },
            start: 12345,
        },
        Case {
            name: "tv with tag only",
            raw: TV_TAG_ONLY_RAW.to_string(),
            act: running(12345),
            want: Activity {
                type_: "PLAYING".to_string(),
                name: "Show".to_string(),
                details: "S1 E2".to_string(),
                state: String::new(),
                large_image: "logo".to_string(),
                large_text: "PotFlix".to_string(),
                has_start: true,
                ..Activity::default()
            },
            start: 12345,
        },
        Case {
            name: "tv paused",
            raw: TV_RAW.to_string(),
            act: Some(Action {
                paused: true,
                ct: 83000,
                ..Action::default()
            }),
            want: Activity {
                type_: "PLAYING".to_string(),
                name: "Show".to_string(),
                details: "Pilot".to_string(),
                state: "S1 E2 - Paused".to_string(),
                large_image: "logo".to_string(),
                large_text: "PotFlix".to_string(),
                paused: true,
                ..Activity::default()
            },
            start: 0,
        },
        Case {
            name: "movie paused has marker alone",
            raw: MOVIE_RAW.to_string(),
            act: Some(Action {
                paused: true,
                ct: 83000,
                ..Action::default()
            }),
            want: Activity {
                type_: "PLAYING".to_string(),
                name: "Movie".to_string(),
                details: "Movie".to_string(),
                state: "Paused".to_string(),
                large_image: "logo".to_string(),
                large_text: "PotFlix".to_string(),
                paused: true,
                ..Activity::default()
            },
            start: 0,
        },
        Case {
            name: "tv stopped",
            raw: TV_RAW.to_string(),
            act: Some(Action {
                ct: 0,
                ..Action::default()
            }),
            want: Activity {
                type_: "PLAYING".to_string(),
                name: "Show".to_string(),
                details: "Pilot".to_string(),
                state: "S1 E2".to_string(),
                large_image: "logo".to_string(),
                large_text: "PotFlix".to_string(),
                ..Activity::default()
            },
            start: 0,
        },
        Case {
            name: "idle",
            raw: "PotPlayer".to_string(),
            act: Some(Action::default()),
            want: Activity {
                type_: "PLAYING".to_string(),
                details: "Browsing".to_string(),
                state: "Idle".to_string(),
                large_image: "logo".to_string(),
                large_text: "PotFlix".to_string(),
                ..Activity::default()
            },
            start: 0,
        },
        Case {
            name: "empty media is idle",
            raw: " - PotPlayer".to_string(),
            act: Some(Action::default()),
            want: Activity {
                type_: "PLAYING".to_string(),
                details: "Browsing".to_string(),
                state: "Idle".to_string(),
                large_image: "logo".to_string(),
                large_text: "PotFlix".to_string(),
                ..Activity::default()
            },
            start: 0,
        },
        Case {
            // No episode name, and the over-128-unit title fills the details
            // line on its own: the state line must stay empty rather than
            // repeating the truncated details.
            name: "long title without episode context emits no state",
            raw: format!("{}.mkv - PotPlayer", "A".repeat(200)),
            act: running(12345),
            want: Activity {
                type_: "PLAYING".to_string(),
                name: format!("{}\u{2026}", "A".repeat(99)),
                details: format!("{}\u{2026}", "A".repeat(99)),
                state: String::new(),
                large_image: "logo".to_string(),
                large_text: "PotFlix".to_string(),
                has_start: true,
                ..Activity::default()
            },
            start: 12345,
        },
    ];
    for c in &cases {
        let got = build_activity(&c.raw, c.act.as_ref());
        let mut want = c.want.clone();
        want.start = c.start;
        assert_eq!(got, want, "{}: card = {got:?}, want {want:?}", c.name);
    }
}

#[test]
fn action_tag_forms() {
    assert_eq!(action_tag(None), "null", "nil action should tag as null");
    assert_eq!(
        action_tag(Some(&Action {
            running: true,
            ..Action::default()
        })),
        "RUN",
        "running action should tag RUN"
    );
    assert_eq!(
        action_tag(Some(&Action {
            paused: true,
            ..Action::default()
        })),
        "PAUSE",
        "paused action should tag PAUSE"
    );
    assert_eq!(
        action_tag(Some(&Action::default())),
        "STOP",
        "default action should tag STOP"
    );
}

#[test]
fn build_activity_resolved_overrides() {
    const TV_RAW: &str = "Show.S01E02.Pilot.1080p.mkv - PotPlayer";
    let act = Action {
        running: true,
        has_start: true,
        start: 12345,
        ct: 1000,
        ..Action::default()
    };

    // zero overrides match build_activity
    assert_eq!(
        build_activity_resolved(TV_RAW, Some(&act), &Resolved::default()),
        build_activity(TV_RAW, Some(&act)),
        "Resolved::default() card"
    );

    // series override changes name and large_text
    let base = build_activity(TV_RAW, Some(&act));
    let got = build_activity_resolved(
        TV_RAW,
        Some(&act),
        &Resolved {
            series: "Canonical Show".to_string(),
            ..Resolved::default()
        },
    );
    assert_eq!(
        got.name,
        trunc("Canonical Show", 128),
        "name = {:?}, want {:?}",
        got.name,
        trunc("Canonical Show", 128)
    );
    assert_eq!(
        got.large_text,
        trunc("Canonical Show", 128),
        "large_text = {:?}, want {:?}",
        got.large_text,
        trunc("Canonical Show", 128)
    );
    let mut base = base;
    base.name = got.name.clone();
    base.large_text = got.large_text.clone();
    assert_eq!(
        got, base,
        "only name and large_text should change: got {got:?}, want {base:?}"
    );

    // episode name override changes only details and keeps state tag
    let base = build_activity(TV_RAW, Some(&act));
    let got = build_activity_resolved(
        TV_RAW,
        Some(&act),
        &Resolved {
            episode_name: "Canonical Episode".to_string(),
            ..Resolved::default()
        },
    );
    assert_eq!(
        got.details,
        trunc("Canonical Episode", 128),
        "details = {:?}, want {:?}",
        got.details,
        trunc("Canonical Episode", 128)
    );
    assert_eq!(
        got.state, base.state,
        "state = {:?}, want tag {:?}",
        got.state, base.state
    );
    assert_eq!(base.state, "S1 E2", "base state");
    let mut base = base;
    base.details = got.details.clone();
    assert_eq!(
        got, base,
        "only details should change: got {got:?}, want {base:?}"
    );

    // overrides truncate at 128
    let long = "y".repeat(200);
    let got = build_activity_resolved(
        TV_RAW,
        Some(&act),
        &Resolved {
            series: long.clone(),
            episode_name: long.clone(),
            ..Resolved::default()
        },
    );
    let want = trunc(&long, 128);
    assert!(
        got.name == want && got.details == want,
        "name={:?} details={:?}, want both {want:?}",
        got.name,
        got.details
    );
    let n = java_units(&got.name);
    assert!(n <= 128, "name length {n} exceeds 128: {:?}", got.name);
    let n = java_units(&got.details);
    assert!(
        n <= 128,
        "details length {n} exceeds 128: {:?}",
        got.details
    );
}

#[test]
fn build_activity_resolved_rich() {
    const TV_RAW: &str = "Show.S01E02.Pilot.1080p.mkv - PotPlayer";
    const TV_TAG_ONLY_RAW: &str = "Show.S01E02.1080p.mkv - PotPlayer";
    const MOVIE_RAW: &str = "Movie.mkv - PotPlayer";
    let run_act = Action {
        running: true,
        has_start: true,
        start: 12345,
        ct: 1000,
        ..Action::default()
    };

    // artwork https URL becomes large_image
    let art = "https://example.com/poster.jpg";
    let got = build_activity_resolved(
        TV_RAW,
        Some(&run_act),
        &Resolved {
            artwork: art.to_string(),
            ..Resolved::default()
        },
    );
    assert_eq!(got.large_image, art, "large_image");

    // artwork empty or invalid keeps logo
    for art in [
        "",
        "logo",
        "ftp://example.com/x.jpg",
        "example.com/x.jpg",
        " /https://example.com/x.jpg",
    ] {
        let got = build_activity_resolved(
            TV_RAW,
            Some(&run_act),
            &Resolved {
                artwork: art.to_string(),
                ..Resolved::default()
            },
        );
        assert_eq!(got.large_image, "logo", "artwork {art:?}: large_image");
        assert_ne!(got.large_image, "", "artwork {art:?}: large_image is empty");
    }

    // series episode tag synopsis semantics
    let got = build_activity_resolved(
        TV_RAW,
        Some(&run_act),
        &Resolved {
            series: "Canonical Series".to_string(),
            episode_name: "Canonical Episode".to_string(),
            synopsis: "A brave synopsis".to_string(),
            ..Resolved::default()
        },
    );
    let want = trunc("Canonical Series", 128);
    assert_eq!(got.name, want, "name");
    let want = trunc("Canonical Episode", 128);
    assert_eq!(got.details, want, "details");
    // The series default carries the teaser after the tag; the tooltip keeps
    // the whole note.
    let want = "S1 E2 \u{b7} A brave synopsis";
    assert_eq!(got.state, want, "state");
    assert_eq!(got.state, "S1 E2 \u{b7} A brave synopsis", "state exact");
    // Hover slot: large_text carries the complete short synopsis, not a
    // duplicate of name (the card header already shows the series).
    assert_eq!(
        got.large_text,
        rp_text("A brave synopsis", 128),
        "large_text"
    );

    // The series default carries the teaser when a synopsis is known: with no
    // tag the state is the synopsis alone, and the tooltip keeps the whole note.
    let got = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            synopsis: "Just a synopsis".to_string(),
            ..Resolved::default()
        },
    );
    assert_eq!(got.state, "Just a synopsis", "state");
    assert_eq!(got.large_text, "Just a synopsis", "large_text");

    // tag only becomes state
    let got = build_activity_resolved(TV_TAG_ONLY_RAW, Some(&run_act), &Resolved::default());
    assert_eq!(got.state, "S1 E2", "state");

    // A long synopsis reaches the tooltip (word-aware cut at 128) and the
    // state ladder falls back to the synopsis teaser at 72.
    let long = "s".repeat(200);
    let got = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            synopsis: long.clone(),
            ..Resolved::default()
        },
    );
    assert_eq!(got.state, rp_text(&long, STATE_SYNOPSIS_BUDGET), "state");
    assert_eq!(got.large_text, rp_text(&long, 128), "large_text");
    let n = java_units(&got.state);
    assert!(
        n <= STATE_SYNOPSIS_BUDGET,
        "state length {n} exceeds {STATE_SYNOPSIS_BUDGET}: {:?}",
        got.state
    );
    let n = java_units(&got.state);
    assert!(n <= 128, "state length {n} exceeds 128: {:?}", got.state);
}

/// Discord rejects `large_image` longer than 300 chars (`code=4000`), which a
/// Kitsu-signed Backblaze URL can exceed. The composer must omit the
/// over-budget image (fallback to `"logo"`), never truncate it (a cut signed
/// URL is a broken link) and never emit the oversized field.
#[test]
fn large_image_over_300_falls_back_to_logo() {
    const TV_RAW: &str = "Show.S01E02.Pilot.1080p.mkv - PotPlayer";
    let run_act = Action {
        running: true,
        has_start: true,
        start: 12345,
        ct: 1000,
        ..Action::default()
    };
    let long_art = format!("https://example.com/{}", "x".repeat(400));
    assert!(long_art.len() > 300, "fixture must exceed the Discord cap");
    let got = build_activity_resolved(
        TV_RAW,
        Some(&run_act),
        &Resolved {
            artwork: long_art.clone(),
            ..Resolved::default()
        },
    );
    assert_eq!(
        got.large_image, "logo",
        "over-budget artwork must be omitted, got {:?}",
        got.large_image
    );
    assert!(
        got.large_image.len() <= 300,
        "large_image still oversized: {:?}",
        got.large_image
    );
    // Boundary: exactly 300 chars rides verbatim.
    let edge = format!("https://example.com/{}", "y".repeat(300 - "https://example.com/".len()));
    assert_eq!(edge.len(), 300, "edge fixture must be exactly 300 chars");
    let got_edge = build_activity_resolved(
        TV_RAW,
        Some(&run_act),
        &Resolved {
            artwork: edge.clone(),
            ..Resolved::default()
        },
    );
    assert_eq!(
        got_edge.large_image, edge,
        "300-char artwork must pass through, got {:?}",
        got_edge.large_image
    );
}

#[test]
fn rp_text_cases() {
    assert_eq!(rp_text("a\n\nb  c", 128), "a b c", "collapse");
    assert_eq!(
        rp_text("eps1.1_ones-and-zer0es.mpeg", 128),
        "eps1.1_ones-and-zer0es.mpeg",
        "must preserve underscores/dots/dashes"
    );
    assert_eq!(rp_text("a\tb\rc", 128), "a b c", "tabs/newlines");
    assert_eq!(rp_text("a\x00b\x7Fc", 128), "abc", "C0 strip");
    let long = "z".repeat(200);
    assert_eq!(rp_text(&long, 128), trunc(&long, 128), "trunc");
}

#[test]
fn trim_word_cases() {
    assert_eq!(trim_word("hello world", 128), "hello world", "fits");
    // A long synopsis of whole words must end on "..." at a word boundary,
    // never mid-word: the hard cut would leave a dangling "wo...".
    let syn = format!("{}tail", "word ".repeat(40));
    let got = trim_word(&syn, 128);
    assert!(
        got.ends_with('\u{2026}'),
        "trim_word long = {got:?}, want trailing ellipsis"
    );
    assert!(
        !got.trim_end_matches('\u{2026}').ends_with("wo"),
        "trim_word ended mid-word: {got:?}"
    );
    assert!(
        got.trim_end_matches('\u{2026}').ends_with("word"),
        "trim_word should end on a whole word: {got:?}"
    );
    let n = java_units(&got);
    assert!(n <= 128, "trim_word length {n} exceeds 128: {got:?}");
    // No spaces: the cut is still signalled with a trailing ellipsis within
    // budget (never a silent hard cut, never a split surrogate).
    let nospace = "s".repeat(200);
    let got = trim_word(&nospace, 128);
    assert!(
        got.ends_with('\u{2026}'),
        "trim_word nospace = {got:?}, want trailing ellipsis"
    );
    assert_eq!(
        got,
        format!("{}\u{2026}", "s".repeat(127)),
        "trim_word nospace"
    );
    let n = java_units(&got);
    assert!(
        n <= 128,
        "trim_word nospace length {n} exceeds 128: {got:?}"
    );
    // CJK run (no spaces): the same signalled cut.
    let cjk = "\u{6f22}".repeat(300);
    let got = trim_word(&cjk, 128);
    assert!(
        got.ends_with('\u{2026}'),
        "trim_word cjk = {got:?}, want trailing ellipsis"
    );
    let n = java_units(&got);
    assert!(n <= 128, "trim_word cjk length {n} exceeds 128: {got:?}");
    let body = got.trim_end_matches('\u{2026}');
    let last = body.chars().next_back().unwrap();
    assert!(
        last.is_alphanumeric(),
        "trim_word cjk cut ends on {last:?}, want a word character: {got:?}"
    );
    // Surrogate safety: an emoji straddling the cut is dropped whole, never
    // split (the old UTF-16 hard cut left a `?` from the lone surrogate).
    let split = format!("{}\u{1f600}{}", "a".repeat(126), "b".repeat(100));
    let got = trim_word(&split, 128);
    assert!(
        got.ends_with('\u{2026}'),
        "trim_word surrogate = {got:?}, want trailing ellipsis"
    );
    assert!(
        !got.contains('?'),
        "trim_word split a surrogate (`?`): {got:?}"
    );
    assert_eq!(
        got,
        format!("{}\u{2026}", "a".repeat(126)),
        "trim_word surrogate"
    );
    // Sentence-end cut: the word-boundary cut lands on `.` followed by a
    // space, so the complete sentence is kept with its own terminator and no
    // ellipsis (the `x×120 + ".…"` shape was the live defect).
    let sentence = format!("{}. {}", "x".repeat(120), "y".repeat(20));
    let got = trim_word(&sentence, 128);
    assert_eq!(
        got,
        "x".repeat(120) + ".",
        "trim_word sentence cut"
    );
    assert!(
        !got.contains('\u{2026}'),
        "complete sentence needs no ellipsis: {got:?}"
    );
    let n = java_units(&got);
    assert!(
        n <= 128,
        "trim_word sentence length {n} exceeds 128: {got:?}"
    );
    assert_eq!(trim_word("", 128), "", "trim_word empty");
}

/// The punctuation rule every truncated card line owes: an ellipsis-terminated
/// cut ends on a word character (never `,` `;` `:` `-` `–` `—` `·` or blank),
/// and the cut lands on a word boundary (the source resumes at whitespace, at
/// a stripped cut mark, or not at all).
fn assert_clean_cut(full: &str, got: &str) {
    assert!(
        got.ends_with('\u{2026}'),
        "cut line must end with U+2026: {got:?}"
    );
    assert!(!got.contains("..."), "never three dots: {got:?}");
    let body = got.trim_end_matches('\u{2026}');
    assert!(!body.is_empty(), "empty cut body: {got:?}");
    let last = body.chars().next_back().unwrap();
    assert!(
        !last.is_whitespace()
            && !matches!(last, ',' | ';' | ':' | '-' | '–' | '—' | '·'),
        "cut leaves trailing punctuation: {got:?}"
    );
    let tail = full
        .strip_prefix(body)
        .unwrap_or_else(|| panic!("cut body is not a source prefix: {got:?}"));
    let next = tail
        .chars()
        .next()
        .unwrap_or_else(|| panic!("cut text was not cut: {got:?}"));
    assert!(
        next.is_whitespace() || matches!(next, ',' | ';' | ':' | '-' | '–' | '—' | '·'),
        "cut ends mid-word (source resumes with {next:?}): {got:?}"
    );
}

/// A truncated `trim_word` line strips the cut punctuation too: `word, next`
/// cut at the space ends `word…`, never `word,…`.
#[test]
fn trim_word_strips_cut_punctuation() {
    let s = format!("{}stop, {}", "filler ".repeat(20), "rest ".repeat(20));
    let got = trim_word(&s, 128);
    assert_clean_cut(&s, &got);
    // The 128-unit budget fits 18 whole fillers: the cut lands on the word
    // boundary, never inside `filler` or the `stop,` tail.
    let want = format!("{}\u{2026}", "filler ".repeat(18).trim());
    assert_eq!(got, want, "trim_word cut");
}

/// The default movie ladder falls to the synopsis teaser when genres and
/// length are absent (the live `…Majin Buu,` cut now lives on the default
/// line too, pinned in cardspec_test): the state carries the teaser and the
/// tooltip carries the whole note.
#[test]
fn movie_default_state_ladder_to_synopsis() {
    const MOVIE_RAW: &str = "Dragon.Ball.Movie.1990.1080p.mkv - PotPlayer";
    let run_act = Action {
        running: true,
        has_start: true,
        start: 12345,
        ct: 1000,
        ..Action::default()
    };
    let syn = "Years after the battle with Earth's greatest enemy, Majin Buu, the world faces a new threat from beyond the stars and more";
    let got = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            series: "Dragon Ball".to_string(),
            year: 1990,
            is_movie: true,
            synopsis: syn.to_string(),
            ..Resolved::default()
        },
    );
    assert_eq!(
        got.state, "Years after the battle with Earth's greatest enemy, Majin Buu\u{2026}",
        "state"
    );
    assert_eq!(got.large_text, trim_word(syn, 128), "large_text");
}

/// The default series state carries the synopsis teaser after the tag (the
/// live `…meet Elliot;` cut now lives on the default line too, pinned in
/// cardspec_test).
#[test]
fn series_default_state_carries_teaser() {
    const TV_RAW: &str = "Show.S02E04.Episode.1080p.mkv - PotPlayer";
    let run_act = Action {
        running: true,
        has_start: true,
        start: 12345,
        ct: 1000,
        ..Action::default()
    };
    let syn = "Allsafe is controlled; the Dark Army is ready to meet Elliot; the fsociety plan falls apart at midnight somehow";
    let got = build_activity_resolved(
        TV_RAW,
        Some(&run_act),
        &Resolved {
            series: "Show".to_string(),
            synopsis: syn.to_string(),
            ..Resolved::default()
        },
    );
    assert_eq!(
        got.state,
        "S2 E4 \u{b7} Allsafe is controlled; the Dark Army is ready to meet Elliot\u{2026}",
        "state"
    );
    assert_eq!(got.large_text, trim_word(syn, 128), "large_text");
}

/// The live tooltip shape (`…Zak Penn and Ernest` on the wire): a cut hover
/// line ends on a whole word with U+2026, never mid-word with no terminator.
#[test]
fn hover_tooltip_cut_ends_clean() {
    const MOVIE_RAW: &str = "Movie.mkv - PotPlayer";
    let run_act = Action {
        running: true,
        has_start: true,
        start: 12345,
        ct: 1000,
        ..Action::default()
    };
    let syn = "Ready Player One is a 2018 American science fiction action film directed by Steven Spielberg and cowritten with Zak Penn alongside Ernest Cline. Based on the novel, Wade hunts the prize.";
    let got = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            series: "Ready Player One".to_string(),
            year: 2018,
            is_movie: true,
            synopsis: syn.to_string(),
            ..Resolved::default()
        },
    );
    assert_eq!(got.name, "Ready Player One", "name");
    assert_eq!(got.details, "Ready Player One (2018)", "details");
    // The tooltip cut lands on a whole word with the U+2026 terminator: the
    // live card hard-cut the same text to `...Zak Penn and Ernest`, mid-word
    // with no terminator.
    let want = "Ready Player One is a 2018 American science fiction action film directed by Steven Spielberg and cowritten with Zak Penn\u{2026}";
    assert_eq!(got.large_text, want, "large_text");
    assert_clean_cut(syn, &got.large_text);
    let n = got.large_text.encode_utf16().count();
    assert!(
        n <= 128,
        "large_text length {n} exceeds 128: {:?}",
        got.large_text
    );
}

#[test]
fn build_activity_resolved_movie() {
    const MOVIE_RAW: &str = "Dune.Part.Two.2024.1080p.mkv - PotPlayer";
    let run_act = Action {
        running: true,
        has_start: true,
        start: 12345,
        ct: 1000,
        ..Action::default()
    };

    // details carry Title (Year)
    let got = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            series: "Dune: Part Two".to_string(),
            year: 2024,
            is_movie: true,
            ..Resolved::default()
        },
    );
    assert_eq!(got.details, trunc("Dune: Part Two (2024)", 128), "details");
    assert_eq!(got.name, trunc("Dune: Part Two", 128), "name");

    // details without year are the bare title
    let got = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            series: "Dune: Part Two".to_string(),
            is_movie: true,
            ..Resolved::default()
        },
    );
    assert_eq!(got.details, trunc("Dune: Part Two", 128), "details");

    // state joins Genre Length (no synopsis)
    let got = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            series: "Dune: Part Two".to_string(),
            year: 2024,
            is_movie: true,
            genres: "Sci-Fi".to_string(),
            length: "2h 46m".to_string(),
            synopsis: "Paul unites the Fremen".to_string(),
            ..Resolved::default()
        },
    );
    assert_eq!(got.state, "Sci-Fi \u{b7} 2h 46m", "state");

    // state skips empties
    let got = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            series: "Dune".to_string(),
            is_movie: true,
            length: "2h 19m".to_string(),
            synopsis: "A quest".to_string(),
            ..Resolved::default()
        },
    );
    assert_eq!(got.state, "2h 19m", "state");

    // long movie state carries genres/length only; the synopsis stays in the tooltip
    let syn = format!("{}finale", "epic ".repeat(40));
    let got = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            series: "Dune".to_string(),
            is_movie: true,
            genres: "Adventure".to_string(),
            length: "2h 46m".to_string(),
            synopsis: syn,
            ..Resolved::default()
        },
    );
    assert_eq!(got.state, "Adventure \u{b7} 2h 46m", "state");
    let n = java_units(&got.state);
    assert!(n <= 128, "state length {n} exceeds 128: {:?}", got.state);

    // paused movie keeps the Paused marker
    let got = build_activity_resolved(
        MOVIE_RAW,
        Some(&Action {
            paused: true,
            ct: 1000,
            ..Action::default()
        }),
        &Resolved {
            series: "Dune".to_string(),
            year: 2024,
            is_movie: true,
            genres: "Sci-Fi".to_string(),
            length: "2h 46m".to_string(),
            synopsis: "Paul unites the Fremen".to_string(),
            ..Resolved::default()
        },
    );
    assert!(
        got.state.ends_with(PAUSED_MARKER),
        "paused state = {:?}, want suffix {PAUSED_MARKER:?}",
        got.state
    );
    let n = java_units(&got.state);
    assert!(
        n <= 128,
        "paused state length {n} exceeds 128: {:?}",
        got.state
    );

    // empty artwork stays logo
    let got = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            series: "Dune".to_string(),
            is_movie: true,
            ..Resolved::default()
        },
    );
    assert_eq!(got.large_image, "logo", "large_image");
}

#[test]
fn build_activity_resolved_series_state() {
    const TV_RAW: &str = "Show.S01E02.Pilot.1080p.mkv - PotPlayer";
    let run_act = Action {
        running: true,
        has_start: true,
        start: 12345,
        ct: 1000,
        ..Action::default()
    };
    let got = build_activity_resolved(
        TV_RAW,
        Some(&run_act),
        &Resolved {
            series: "Canonical Series".to_string(),
            episode_name: "Canonical Episode".to_string(),
            synopsis: "A brave new world unfolds".to_string(),
            ..Resolved::default()
        },
    );
    assert_eq!(got.state, "S1 E2 \u{b7} A brave new world unfolds", "state");
}

#[test]
fn build_activity_resolved_buttons() {
    // Pristine card: resolved buttons are always ignored, even valid ones.
    const MOVIE_RAW: &str = "Movie.mkv - PotPlayer";
    let run_act = Action {
        running: true,
        has_start: true,
        start: 12345,
        ct: 1000,
        ..Action::default()
    };

    // valid buttons stripped
    let got = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            buttons: vec![
                Button {
                    label: "Trailer".to_string(),
                    url: "https://example.com/trailer".to_string(),
                },
                Button {
                    label: "IMDb".to_string(),
                    url: "https://example.com/imdb".to_string(),
                },
            ],
            ..Resolved::default()
        },
    );
    assert!(
        got.buttons.is_empty(),
        "buttons = {:?}, want stripped (empty)",
        got.buttons
    );

    // build_activity carries no buttons
    assert!(
        build_activity(MOVIE_RAW, Some(&run_act)).buttons.is_empty(),
        "build_activity buttons, want empty"
    );
}

/// The clean-card proof: across running, paused, stopped and idle (plain and
/// metadata-resolved), the builders emit no small_image/small_text and no
/// buttons, while name/details/state and the timer anchors stay intact.
#[test]
fn pristine_card_no_icons_no_buttons() {
    const TV_RAW: &str = "Show.S01E02.Pilot.1080p.mkv - PotPlayer";
    let run_act = Action {
        running: true,
        has_start: true,
        start: 12345,
        ct: 1000,
        ..Action::default()
    };
    let pause_act = Action {
        paused: true,
        ct: 83000,
        ..Action::default()
    };
    let stop_act = Action {
        ct: 0,
        ..Action::default()
    };
    let res = Resolved {
        series: "Show".to_string(),
        episode_name: "Pilot".to_string(),
        artwork: "https://example.com/poster.jpg".to_string(),
        buttons: vec![Button {
            label: "Trailer".to_string(),
            url: "https://example.com/trailer".to_string(),
        }],
        ..Resolved::default()
    };
    let cards = [
        build_activity(TV_RAW, Some(&run_act)),
        build_activity(TV_RAW, Some(&pause_act)),
        build_activity(TV_RAW, Some(&stop_act)),
        build_activity("PotPlayer", Some(&Action::default())),
        build_activity_resolved(TV_RAW, Some(&run_act), &res),
        build_activity_resolved(TV_RAW, Some(&pause_act), &res),
    ];
    for (i, got) in cards.iter().enumerate() {
        assert!(
            got.small_image.is_empty() && got.small_text.is_empty(),
            "card {i}: smallImage={:?} smallText={:?}, want no icons",
            got.small_image,
            got.small_text
        );
        assert!(
            got.buttons.is_empty(),
            "card {i}: buttons={:?}, want none",
            got.buttons
        );
    }
    // Spot-check the intact lines: header, body, and the running timer.
    let run = &cards[0];
    assert!(
        run.name == "Show" && run.details == "Pilot" && run.state == "S1 E2",
        "running lines = {:?}/{:?}/{:?}, want Show/Pilot/S1 E2",
        run.name,
        run.details,
        run.state
    );
    assert!(
        run.has_start && run.start == 12345,
        "running timer hasStart={} start={}, want true/12345",
        run.has_start,
        run.start
    );
    assert!(
        cards[1].paused,
        "paused card paused=false, want true (the gate's park signal)"
    );
}

#[test]
fn smart_trim_synopsis_sentence_no_ellipsis() {
    let s = "Intro filler words to push length out a bit. Second sentence continues long enough to exceed the seventy two unit budget for sure and more.";
    let got = smart_trim_synopsis(s, STATE_SYNOPSIS_BUDGET);
    let want = "Intro filler words to push length out a bit.";
    assert_eq!(got, want, "sentence cut");
    assert!(
        !got.contains('\u{2026}'),
        "complete sentence needs no ellipsis: {got:?}"
    );
    assert!(!got.contains("..."), "never three dots: {got:?}");
    let n = java_units(&got);
    assert!(
        n <= STATE_SYNOPSIS_BUDGET,
        "length {n} exceeds {STATE_SYNOPSIS_BUDGET}: {got:?}"
    );
}

#[test]
fn smart_trim_synopsis_clause_ellipsis() {
    let s = format!("{}first, {}", "word ".repeat(10), "second ".repeat(20));
    let got = smart_trim_synopsis(&s, STATE_SYNOPSIS_BUDGET);
    assert!(
        got.ends_with('\u{2026}'),
        "clause cut should end with U+2026: {got:?}"
    );
    assert!(!got.contains("..."), "never three dots: {got:?}");
    // The clause comma is a cut mark, not content: the live card carried it
    // into the ellipsis (`Buu,…`), so the cut strips trailing punctuation and
    // the base ends on the word (`first…`).
    let want = format!("{}first\u{2026}", "word ".repeat(10));
    assert_eq!(got, want, "clause cut");
    assert_clean_cut(&s, &got);
    let n = java_units(&got);
    assert!(
        n <= STATE_SYNOPSIS_BUDGET,
        "length {n} exceeds {STATE_SYNOPSIS_BUDGET}: {got:?}"
    );
    // No sentence end in this input, so the comma path must win.
    assert!(
        !got.trim_end_matches('\u{2026}').contains('.'),
        "unexpected sentence cut: {got:?}"
    );
}

#[test]
fn smart_trim_synopsis_word_ellipsis() {
    let s = format!("{}finale", "epic ".repeat(30));
    let got = smart_trim_synopsis(&s, STATE_SYNOPSIS_BUDGET);
    assert!(
        got.ends_with('\u{2026}'),
        "word cut should end with U+2026: {got:?}"
    );
    assert!(!got.contains("..."), "never three dots: {got:?}");
    let body = got.trim_end_matches('\u{2026}');
    assert!(
        body.ends_with("epic"),
        "should land on a whole word: {got:?}"
    );
    let n = java_units(&got);
    assert!(
        n <= STATE_SYNOPSIS_BUDGET,
        "length {n} exceeds {STATE_SYNOPSIS_BUDGET}: {got:?}"
    );
}

/// No boundary anywhere (single token, no spaces): the tail still signals the
/// cut with a single trailing U+2026 within budget, never a silent hard cut.
#[test]
fn smart_trim_synopsis_nospace_ellipsis() {
    let s = "y".repeat(300);
    let got = smart_trim_synopsis(&s, STATE_SYNOPSIS_BUDGET);
    assert!(
        got.ends_with('\u{2026}'),
        "nospace cut should end with U+2026: {got:?}"
    );
    assert!(!got.contains("..."), "never three dots: {got:?}");
    assert_eq!(
        got,
        format!("{}\u{2026}", "y".repeat(STATE_SYNOPSIS_BUDGET - 1)),
        "nospace cut"
    );
    let n = java_units(&got);
    assert!(
        n <= STATE_SYNOPSIS_BUDGET,
        "length {n} exceeds {STATE_SYNOPSIS_BUDGET}: {got:?}"
    );
}

#[test]
fn smart_trim_synopsis_strips_connector() {
    let s = format!("{}and {}", "word ".repeat(13), "tailword ".repeat(20));
    let got = smart_trim_synopsis(&s, STATE_SYNOPSIS_BUDGET);
    assert!(
        got.ends_with('\u{2026}'),
        "connector cut should end with U+2026: {got:?}"
    );
    assert!(
        !got.ends_with("and\u{2026}") && !got.ends_with("and \u{2026}"),
        "dangling connector survived: {got:?}"
    );
    let body = got.trim_end_matches('\u{2026}');
    let fields: Vec<&str> = body.split_whitespace().collect();
    assert!(!fields.is_empty(), "empty body: {got:?}");
    let last = fields[fields.len() - 1].to_lowercase();
    for c in ["and", "or", "but", "the", "a", "of", "to"] {
        assert!(
            last != c,
            "last word {last:?} is a dangling connector: {got:?}"
        );
    }
    let n = java_units(&got);
    assert!(
        n <= STATE_SYNOPSIS_BUDGET,
        "length {n} exceeds {STATE_SYNOPSIS_BUDGET}: {got:?}"
    );
}

#[test]
fn idle_card_budget() {
    let got = build_activity("PotPlayer", Some(&Action::default()));
    assert!(
        got.details == "Browsing" && got.state == "Idle",
        "idle card = {got:?}, want Browsing/Idle"
    );
    let n = java_units(&got.state);
    assert!(
        n <= STATE_SYNOPSIS_BUDGET,
        "idle state length {n} exceeds {STATE_SYNOPSIS_BUDGET}"
    );
}

#[test]
fn synopsis_before_after_longest() {
    const MOVIE_RAW: &str = "Dune.Part.Two.2024.1080p.mkv - PotPlayer";
    let run_act = Action {
        running: true,
        has_start: true,
        start: 12345,
        ct: 1000,
        ..Action::default()
    };
    let long_syn = "Paul Atreides unites with the Fremen and wages war against House Harkonnen. As betrayal surrounds him, he faces a choice between the love of his life and the fate of the known universe, striving to prevent a terrible future only he can foresee and more beyond.";
    let before = trim_word(
        &format!("Sci-Fi \u{b7} 2h 46m \u{b7} {}", rp_text(long_syn, 1 << 28)),
        128,
    );
    let after = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            is_movie: true,
            series: "Dune: Part Two".to_string(),
            year: 2024,
            genres: "Sci-Fi".to_string(),
            length: "2h 46m".to_string(),
            synopsis: long_syn.to_string(),
            ..Resolved::default()
        },
    )
    .state;
    let n = java_units(&after);
    assert!(
        n <= STATE_SYNOPSIS_BUDGET,
        "after length {n} exceeds {STATE_SYNOPSIS_BUDGET}: {after:?}"
    );
    assert!(
        java_units(&before) > java_units(&after),
        "expected reduction: before={} after={}",
        java_units(&before),
        java_units(&after)
    );
}

#[test]
fn synopsis_state_budget_72() {
    const TV_RAW: &str = "Show.S01E02.Pilot.1080p.mkv - PotPlayer";
    const MOVIE_RAW: &str = "Movie.mkv - PotPlayer";
    let run_act = Action {
        running: true,
        has_start: true,
        start: 12345,
        ct: 1000,
        ..Action::default()
    };
    let paused_act = Action {
        paused: true,
        ct: 1000,
        ..Action::default()
    };
    let long_syn = "A retired operative living quietly is pulled back when a ruthless syndicate burns his home. He crosses continents, gathers old allies, and faces a final choice that will decide everything he loves and more.";
    let movie = |act: &Action| {
        build_activity_resolved(
            MOVIE_RAW,
            Some(act),
            &Resolved {
                is_movie: true,
                genres: "Action, Drama".to_string(),
                length: "2h 19m".to_string(),
                synopsis: long_syn.to_string(),
                ..Resolved::default()
            },
        )
    };
    let series = |act: &Action| {
        build_activity_resolved(
            TV_RAW,
            Some(act),
            &Resolved {
                synopsis: long_syn.to_string(),
                ..Resolved::default()
            },
        )
    };
    let paused_movie = build_activity_resolved(
        MOVIE_RAW,
        Some(&paused_act),
        &Resolved {
            is_movie: true,
            genres: "Sci-Fi".to_string(),
            length: "2h 46m".to_string(),
            synopsis: long_syn.to_string(),
            ..Resolved::default()
        },
    );
    let paused_series = build_activity_resolved(
        TV_RAW,
        Some(&paused_act),
        &Resolved {
            synopsis: long_syn.to_string(),
            ..Resolved::default()
        },
    );
    let cases: [(&str, Activity); 5] = [
        ("movie", movie(&run_act)),
        ("series tag+synopsis", series(&run_act)),
        ("movie paused", paused_movie.clone()),
        ("series paused tag+synopsis", paused_series),
        (
            "series synopsis only",
            build_activity_resolved(
                MOVIE_RAW,
                Some(&run_act),
                &Resolved {
                    synopsis: long_syn.to_string(),
                    ..Resolved::default()
                },
            ),
        ),
    ];
    for (name, act) in &cases {
        let n = java_units(&act.state);
        assert!(
            !act.state.contains("..."),
            "{name}: never three dots: {:?}",
            act.state
        );
        assert!(
            n <= STATE_SYNOPSIS_BUDGET,
            "{name}: state length {n} exceeds {STATE_SYNOPSIS_BUDGET}: {:?}",
            act.state
        );
        assert!(
            n <= 128,
            "{name}: state length {n} exceeds 128 ceiling: {:?}",
            act.state
        );
    }
    // Structured prefixes survive in full.
    let movie = movie(&run_act);
    assert!(
        movie.state.contains("Action, Drama") && movie.state.contains("2h 19m"),
        "movie prefix lost: {:?}",
        movie.state
    );
    let series = series(&run_act);
    assert!(
        series.state.starts_with("S1 E2"),
        "episode tag lost: {:?}",
        series.state
    );
    let paused = paused_movie;
    assert!(
        paused.state.ends_with(PAUSED_MARKER),
        "paused marker lost: {:?}",
        paused.state
    );
}

/// Proves the two surfaces agree on one synopsis: the same Resolved input yields
/// a State line within the 72-unit budget and a LargeText hover tooltip within
/// Discord's 128-unit asset-text cap. The mid-length synopsis (~99 units) exceeds
/// the State budget so the card trims it, but fits the tooltip whole.
#[test]
fn hover_tooltip_whole_and_state_budget_agree() {
    const TV_RAW: &str = "Show.S01E02.Pilot.1080p.mkv - PotPlayer";
    const MOVIE_RAW: &str = "Movie.mkv - PotPlayer";
    let run_act = Action {
        running: true,
        has_start: true,
        start: 12345,
        ct: 1000,
        ..Action::default()
    };
    // ~99 units: over the 72 State budget, under the 128 tooltip cap.
    let mid_syn = "word ".repeat(20).trim().to_string();
    let n = java_units(&mid_syn);
    assert!(
        n > STATE_SYNOPSIS_BUDGET && n <= 128,
        "fixture setup: mid_syn units={n}, want 72 < n <= 128"
    );
    let cases: [(&str, Activity, &str); 2] = [
        (
            "movie synopsis-only",
            build_activity_resolved(
                MOVIE_RAW,
                Some(&run_act),
                &Resolved {
                    is_movie: true,
                    series: "Mid Film".to_string(),
                    synopsis: mid_syn.clone(),
                    ..Resolved::default()
                },
            ),
            &mid_syn,
        ),
        (
            "series tag+synopsis",
            build_activity_resolved(
                TV_RAW,
                Some(&run_act),
                &Resolved {
                    series: "Mid Series".to_string(),
                    synopsis: mid_syn.clone(),
                    ..Resolved::default()
                },
            ),
            &mid_syn,
        ),
    ];
    for (name, act, syn) in &cases {
        let n = java_units(&act.state);
        assert!(
            n <= STATE_SYNOPSIS_BUDGET,
            "{name}: state length {n} exceeds {STATE_SYNOPSIS_BUDGET}: {:?}",
            act.state
        );
        let n = java_units(&act.large_text);
        assert!(
            n <= 128,
            "{name}: large_text length {n} exceeds 128: {:?}",
            act.large_text
        );
        // The tooltip carries the short synopsis whole, not the State trim.
        assert_eq!(
            act.large_text,
            rp_text(syn, 128),
            "{name}: large_text, want whole synopsis"
        );
        assert_eq!(
            act.large_text, *syn,
            "{name}: large_text should arrive whole (no ellipsis)"
        );
        assert!(
            act.large_text != act.state,
            "{name}: tooltip must not equal the trimmed State line"
        );
    }
    // Over-cap synopsis still respects both ceilings on the same input.
    let over_syn = "s".repeat(200);
    let over = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            is_movie: true,
            series: "Over Film".to_string(),
            synopsis: over_syn.clone(),
            ..Resolved::default()
        },
    );
    let n = java_units(&over.state);
    assert!(
        n <= STATE_SYNOPSIS_BUDGET,
        "over-cap: state length {n} exceeds {STATE_SYNOPSIS_BUDGET}"
    );
    let n = java_units(&over.large_text);
    assert!(n <= 128, "over-cap: large_text length {n} exceeds 128");
    assert_eq!(
        over.large_text,
        rp_text(&over_syn, 128),
        "over-cap: large_text"
    );
}

/// Proves LargeText no longer duplicates the series title (name, already the card
/// header) when a synopsis exists.
#[test]
fn hover_tooltip_not_series_name() {
    const TV_RAW: &str = "Show.S01E02.Pilot.1080p.mkv - PotPlayer";
    let run_act = Action {
        running: true,
        has_start: true,
        start: 12345,
        ct: 1000,
        ..Action::default()
    };
    let got = build_activity_resolved(
        TV_RAW,
        Some(&run_act),
        &Resolved {
            series: "Canonical Series".to_string(),
            synopsis: "A brave synopsis".to_string(),
            ..Resolved::default()
        },
    );
    assert_eq!(
        got.name,
        trunc("Canonical Series", 128),
        "name, want series title"
    );
    assert!(
        got.large_text != got.name,
        "large_text {:?} duplicates name; want the synopsis",
        got.large_text
    );
    assert_eq!(
        got.large_text,
        rp_text("A brave synopsis", 128),
        "large_text"
    );
}

/// Proves a synopsis-less input keeps today's behaviour: series title when known,
/// else build_activity's "PotFlix".
#[test]
fn hover_tooltip_fallback_without_synopsis() {
    const TV_RAW: &str = "Show.S01E02.Pilot.1080p.mkv - PotPlayer";
    const MOVIE_RAW: &str = "Movie.mkv - PotPlayer";
    let run_act = Action {
        running: true,
        has_start: true,
        start: 12345,
        ct: 1000,
        ..Action::default()
    };
    let with_series = build_activity_resolved(
        TV_RAW,
        Some(&run_act),
        &Resolved {
            series: "Canonical Show".to_string(),
            ..Resolved::default()
        },
    );
    assert_eq!(
        with_series.large_text,
        trunc("Canonical Show", 128),
        "series fallback large_text"
    );
    let bare = build_activity_resolved(MOVIE_RAW, Some(&run_act), &Resolved::default());
    assert_eq!(bare.large_text, "PotFlix", "bare fallback large_text");
    let idle = build_activity_resolved("PotPlayer", Some(&Action::default()), &Resolved::default());
    assert_eq!(idle.large_text, "PotFlix", "idle fallback large_text");
}
