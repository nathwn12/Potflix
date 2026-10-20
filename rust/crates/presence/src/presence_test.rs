//! `presence_test` -- mirrors `go\internal\presence\presence_test.go`.
//!
//! Table-driven tests over the transition matrix, plus the debounce deviation and
//! the payload helpers. Expectations are derived from the port contract
//! (section 4) and the Java PresenceTracker ported in `presence.go`.

use player::control::PlayStatus;
use titleclean::trunc;

use crate::cardspec::{CARD_DETAILS_DEFAULT, CARD_STATE_DEFAULT, CARD_STATE_SYNOPSIS, CardSpec};
use crate::{
    Action, Activity, Button, PAUSED_MARKER, Resolved, STATE_SYNOPSIS_BUDGET, Tracker, action_tag,
    build_activity, build_activity_resolved, compose_card, fmt_ms, rp_text, smart_trim_synopsis,
    trim_word, with_state_time,
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

    // series override changes name and large_text; details keeps the
    // filename's episode name (never the show header -- the header already
    // carries it).
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
        got.details,
        trunc("Pilot", 128),
        "details = {:?}, want {:?}",
        got.details,
        trunc("Pilot", 128)
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
    base.details = got.details.clone();
    base.large_text = got.large_text.clone();
    assert_eq!(
        got, base,
        "only name and large_text should change: got {got:?}, want {base:?}"
    );

    // episode name override changes details; the default state falls to the
    // filename tag (the synopsis line is off without a synopsis).
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
        got.state, "S1 E2",
        "state = {:?}, want the filename tag",
        got.state
    );
    let mut base = base;
    base.details = got.details.clone();
    base.state = got.state.clone();
    assert_eq!(
        got, base,
        "only details and state should change: got {got:?}, want {base:?}"
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
    // The default state is the code-plus-synopsis line: the episode title
    // stays on the details line and the tooltip keeps the whole note.
    assert_eq!(
        got.state, "S1 E2 - A brave synopsis",
        "state"
    );
    assert!(
        got.state.contains("A brave synopsis"),
        "default state must carry the synopsis: {:?}",
        got.state
    );
    // Hover slot: large_text carries the complete short synopsis, not a
    // duplicate of name (the card header already shows the series).
    assert_eq!(
        got.large_text,
        rp_text("A brave synopsis", 128),
        "large_text"
    );

    // T5-A: a movie with only a synopsis puts it on details; state stays
    // empty (no year/genres/runtime) and the tooltip is omitted.
    let got = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            is_movie: true,
            synopsis: "Just a synopsis".to_string(),
            ..Resolved::default()
        },
    );
    assert_eq!(got.details, "Just a synopsis", "details");
    assert_eq!(got.state, "", "state");
    assert_eq!(got.large_text, "", "large_text");

    // tag only becomes state
    let got = build_activity_resolved(TV_TAG_ONLY_RAW, Some(&run_act), &Resolved::default());
    assert_eq!(got.state, "S1 E2", "state");

    // T5-A: a long movie synopsis reaches details (word-aware cut at 128);
    // state stays empty without year/genres/runtime and the tooltip is omitted.
    let long = "s".repeat(200);
    let got = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            is_movie: true,
            synopsis: long.clone(),
            ..Resolved::default()
        },
    );
    assert_eq!(
        got.details,
        format!("{}\u{2026}", "s".repeat(127)),
        "details"
    );
    assert_eq!(got.state, "", "state");
    assert_eq!(got.large_text, "", "large_text");
    let n = java_units(&got.details);
    assert!(n <= 128, "details length {n} exceeds 128: {:?}", got.details);
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

/// T5-A: the movie details carry the synopsis; the state is the year line and
/// the tooltip is omitted (never the synopsis repeat).
#[test]
fn movie_default_state_skips_synopsis() {
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
    assert_eq!(got.details, trim_word(syn, 128), "details");
    assert_eq!(got.state, "1990", "state");
    assert!(
        !got.state.contains("Majin Buu"),
        "default state must not carry the synopsis: {:?}",
        got.state
    );
    assert_eq!(got.large_text, "", "large_text");
}

/// T4: the default series state is the synopsis line (tag plus synopsis when
/// no `SxxExx` code is known): the synopsis no longer stays off the card.
/// The tooltip keeps the whole note.
#[test]
fn series_default_state_is_code_plus_synopsis() {
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
    assert!(
        got.state.starts_with("S2 E4 - "),
        "state = {:?}, want the tag plus the synopsis",
        got.state
    );
    assert!(
        got.state.contains("Elliot"),
        "default state must carry the synopsis: {:?}",
        got.state
    );
    let n = java_units(&got.state);
    assert!(
        n <= STATE_SYNOPSIS_BUDGET,
        "state length {n} exceeds {STATE_SYNOPSIS_BUDGET}: {:?}",
        got.state
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
    // T5-A: the synopsis lives on details (word-aware cut at 128), never
    // `Title (Year)`; the tooltip is omitted.
    let want = "Ready Player One is a 2018 American science fiction action film directed by Steven Spielberg and cowritten with Zak Penn\u{2026}";
    assert_eq!(got.details, want, "details");
    assert_clean_cut(syn, &got.details);
    let n = got.details.encode_utf16().count();
    assert!(
        n <= 128,
        "details length {n} exceeds 128: {:?}",
        got.details
    );
    assert_eq!(got.state, "2018", "state");
    assert_eq!(got.large_text, "", "large_text");
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

    // T5-A: no synopsis falls back to the year, never the title; the state
    // stays empty beside a year-showing details line instead of repeating it.
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
    assert_eq!(got.details, "2024", "details");
    assert_eq!(got.state, "", "state");
    assert_eq!(got.large_text, "", "large_text");
    assert_eq!(got.name, trunc("Dune: Part Two", 128), "name");

    // T5-A: bare movie details stay empty, never the title.
    let got = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            series: "Dune: Part Two".to_string(),
            is_movie: true,
            ..Resolved::default()
        },
    );
    assert_eq!(got.details, "", "details");
    assert_eq!(got.state, "", "state");
    assert_eq!(got.large_text, "", "large_text");

    // T5-A: details carry the synopsis; state joins year/genres/length.
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
    assert_eq!(got.details, "Paul unites the Fremen", "details");
    assert_eq!(got.state, "2024 \u{b7} Sci-Fi \u{b7} 2h 46m", "state");
    assert_eq!(got.large_text, "", "large_text");

    // T5-A: details carry the synopsis; state skips the missing year/genres.
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
    assert_eq!(got.details, "A quest", "details");
    assert_eq!(got.state, "2h 19m", "state");

    // T5-A: long synopsis reaches details (word-aware 128); state stays the
    // year/genres/runtime join; the tooltip is omitted.
    let syn = format!("{}finale", "epic ".repeat(40));
    let got = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            series: "Dune".to_string(),
            is_movie: true,
            genres: "Adventure".to_string(),
            length: "2h 46m".to_string(),
            synopsis: syn.clone(),
            ..Resolved::default()
        },
    );
    assert_eq!(got.details, trim_word(&syn, 128), "details");
    assert_eq!(got.state, "Adventure \u{b7} 2h 46m", "state");
    assert_eq!(got.large_text, "", "large_text");
    let n = java_units(&got.details);
    assert!(n <= 128, "details length {n} exceeds 128: {:?}", got.details);
    let n = java_units(&got.state);
    assert!(n <= 128, "state length {n} exceeds 128: {:?}", got.state);

    // paused movie keeps the Paused marker on the year/genres/runtime line
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
    assert_eq!(
        got.state, "2024 \u{b7} Sci-Fi \u{b7} 2h 46m - Paused",
        "paused state"
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
    assert_eq!(
        got.details, "Canonical Episode",
        "details keeps the episode title"
    );
    assert_eq!(
        got.state, "S1 E2 - A brave new world unfolds",
        "state"
    );
    assert!(
        got.state.contains("brave"),
        "default state must carry the synopsis: {:?}",
        got.state
    );
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

/// The synopsis-bearing State line (the explicit Synopsis preset) trims the
/// longest note well below its tooltip: the same synopsis fills the 128-unit
/// tooltip but lands on the card state inside the 72-unit budget.
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
    let syn_spec = CardSpec {
        details: CARD_DETAILS_DEFAULT.to_string(),
        state: CARD_STATE_SYNOPSIS.to_string(),
    };
    let long_syn = "Paul Atreides unites with the Fremen and wages war against House Harkonnen. As betrayal surrounds him, he faces a choice between the love of his life and the fate of the known universe, striving to prevent a terrible future only he can foresee and more beyond.";
    let before = trim_word(
        &format!("Sci-Fi \u{b7} 2h 46m \u{b7} {}", rp_text(long_syn, 1 << 28)),
        128,
    );
    let after = compose_card(
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
        true,
        &syn_spec,
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

/// The synopsis-bearing State line (the explicit Synopsis preset; the default
/// state no longer carries the synopsis) stays within the 72-unit
/// [`STATE_SYNOPSIS_BUDGET`] in every running/paused, movie/series shape, and
/// its structured prefixes survive whole.
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
    // The 72-unit synopsis budget lives on the explicit Synopsis preset: the
    // default state carries no synopsis, so compose through the preset.
    let syn_spec = CardSpec {
        details: CARD_DETAILS_DEFAULT.to_string(),
        state: CARD_STATE_SYNOPSIS.to_string(),
    };
    let compose = |raw: &str, act: &Action, res: &Resolved| {
        compose_card(raw, Some(act), res, res.is_movie, &syn_spec)
    };
    let movie = |act: &Action| {
        compose(
            MOVIE_RAW,
            act,
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
        compose(
            TV_RAW,
            act,
            &Resolved {
                synopsis: long_syn.to_string(),
                ..Resolved::default()
            },
        )
    };
    let paused_movie = compose(
        MOVIE_RAW,
        &paused_act,
        &Resolved {
            is_movie: true,
            genres: "Sci-Fi".to_string(),
            length: "2h 46m".to_string(),
            synopsis: long_syn.to_string(),
            ..Resolved::default()
        },
    );
    let paused_series = compose(
        TV_RAW,
        &paused_act,
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
            compose(
                MOVIE_RAW,
                &run_act,
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

/// Proves the two surfaces agree on one synopsis under the explicit Synopsis
/// preset: the same Resolved input yields a State line within the 72-unit
/// budget and a LargeText hover tooltip within Discord's 128-unit asset-text
/// cap. The mid-length synopsis (~99 units) exceeds the State budget so the
/// preset trims it, but fits the tooltip whole.
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
    let syn_spec = CardSpec {
        details: CARD_DETAILS_DEFAULT.to_string(),
        state: CARD_STATE_SYNOPSIS.to_string(),
    };
    let compose = |raw: &str, res: &Resolved| {
        compose_card(raw, Some(&run_act), res, res.is_movie, &syn_spec)
    };
    // ~99 units: over the 72 State budget, under the 128 tooltip cap.
    let mid_syn = "word ".repeat(20).trim().to_string();
    let n = java_units(&mid_syn);
    assert!(
        n > STATE_SYNOPSIS_BUDGET && n <= 128,
        "fixture setup: mid_syn units={n}, want 72 < n <= 128"
    );
    // T5-A: series keeps the whole synopsis in the tooltip; movies omit it
    // (the synopsis already lives on details, and on state under this
    // explicit Synopsis preset).
    let cases: [(&str, Activity, &str); 2] = [
        (
            "movie synopsis-only",
            compose(
                MOVIE_RAW,
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
            compose(
                TV_RAW,
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
        if name.starts_with("movie") {
            assert_eq!(
                act.large_text, "",
                "{name}: movie tooltip is omitted under T5-A"
            );
            assert_eq!(
                act.details, *syn,
                "{name}: movie details carry the whole short synopsis"
            );
            continue;
        }
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
    // T5-A: the movie tooltip is omitted; the Synopsis-preset state still
    // carries the synopsis within the 72 budget.
    let over_syn = "s".repeat(200);
    let over = compose(
        MOVIE_RAW,
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
    assert_eq!(over.large_text, "", "over-cap: movie tooltip is omitted");
    assert_eq!(
        over.details,
        rp_text(&over_syn, 128),
        "over-cap: movie details carry the synopsis"
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

/// T5-A: a synopsis-less series keeps the series-title fallback; a movie
/// omits the tooltip even with a series title; idle keeps "PotFlix".
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
    let bare = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            is_movie: true,
            ..Resolved::default()
        },
    );
    assert_eq!(bare.large_text, "", "movie tooltip is omitted");
    let movie_with_title = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            series: "Some Film".to_string(),
            is_movie: true,
            ..Resolved::default()
        },
    );
    assert_eq!(
        movie_with_title.large_text, "",
        "movie tooltip stays omitted with a title"
    );
    let idle = build_activity_resolved("PotPlayer", Some(&Action::default()), &Resolved::default());
    assert_eq!(idle.large_text, "PotFlix", "idle fallback large_text");
}

/// A signed 396-char Kitsu-style URL that no backfill replaced still degrades
/// to `logo`: over Discord's 300-char `large_image` budget, never truncated.
#[test]
fn unreplaced_signed_artwork_degrades_to_logo() {
    const TV_RAW: &str = "Show.S01E02.Pilot.1080p.mkv - PotPlayer";
    let mut art =
        "https://f000.backblazeb2.com/file/kitsu-prod/50497.jpg?Expires=900&X-Amz-Signature="
            .to_string();
    while art.len() < 396 {
        art.push('a');
    }
    assert_eq!(art.len(), 396, "fixture mirrors live Kitsu 50497");
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
            artwork: art,
            ..Resolved::default()
        },
    );
    assert_eq!(got.large_image, "logo", "over-budget signed artwork");
}

/// T1: the one `SxxExx` formatter. Season+number renders `S01E02`; an
/// absolute-only episode renders `E02` and never fabricates a season -- and a
/// season-0 number never grows one either: the AniList absolute shape
/// `(season 0, number = absolute)` renders `E05`, not `S00E05`. When the two
/// numbers disagree (`season 0, number != absolute`) the absolute number wins
/// (`E05` for `(0, 7, 5)`). A tag
/// already matching `SxxExx` passes through; a missing number omits
/// the code.
#[test]
fn episode_code_formatter() {
    use crate::episode_code;
    assert_eq!(episode_code(1, 2, 0, ""), "S01E02");
    assert_eq!(episode_code(12, 34, 0, ""), "S12E34");
    assert_eq!(
        episode_code(2, 5, 27, ""),
        "S02E05",
        "season wins over absolute"
    );
    assert_eq!(episode_code(0, 0, 2, ""), "E02", "absolute only");
    assert!(
        !episode_code(0, 0, 2, "").contains('S'),
        "absolute must never fabricate a season"
    );
    assert_eq!(
        episode_code(0, 0, 0, ""),
        "",
        "nothing known omits the code"
    );
    assert_eq!(
        episode_code(1, 0, 0, ""),
        "",
        "missing number omits the code"
    );
    assert_eq!(
        episode_code(0, 0, 0, "S01E02"),
        "S01E02",
        "strict tag passes through"
    );
    assert_eq!(
        episode_code(0, 0, 0, "s1e2"),
        "S01E02",
        "case-insensitive passthrough"
    );
    assert_eq!(
        episode_code(0, 0, 0, "S1 E2"),
        "",
        "spaced tag keeps the current tag behavior"
    );
    assert_eq!(
        episode_code(0, 0, 0, "Pilot"),
        "",
        "non-code tag is no code"
    );
    // The real AniList absolute production shape: season 0 with the number
    // set to the absolute episode. No season is fabricated -- there is no
    // distinct signal for a real S00 special in this shape, so it renders
    // the bare absolute number the same way.
    assert_eq!(
        episode_code(0, 5, 5, ""),
        "E05",
        "absolute production shape (season 0, number = absolute)"
    );
    assert!(
        !episode_code(0, 5, 5, "").contains('S'),
        "season 0 must never fabricate S00"
    );
    assert_eq!(
        episode_code(0, 3, 0, ""),
        "E03",
        "season-0 number renders bare, never S00"
    );
    assert_eq!(
        episode_code(0, 7, 5, ""),
        "E05",
        "season 0 with disagreeing numbers: the absolute number wins"
    );
    assert!(
        !episode_code(0, 7, 5, "").contains('S'),
        "disagreeing season-0 numbers must never fabricate S00"
    );
    assert_eq!(
        episode_code(0, 0, 0, ""),
        "",
        "special with no number omits the code"
    );
}

/// T4: series default details is the episode title alone -- never the show
/// title and never the episode code. The show title lives in the header
/// (`name`); the code lives in the state line only.
#[test]
fn series_default_details_show_plus_code() {
    const TV_RAW: &str = "Show.S01E02.Confrontation.1080p.mkv - PotPlayer";
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
            series: "Show".to_string(),
            episode_name: "Confrontation".to_string(),
            synopsis: "A brave synopsis".to_string(),
            season: 1,
            number: 2,
            ..Resolved::default()
        },
    );
    assert_eq!(got.name, "Show", "header keeps the show title");
    assert_eq!(got.details, "Confrontation", "details");
    assert!(
        !got.details.contains("Show"),
        "details must not repeat the show: {:?}",
        got.details
    );
    assert!(
        !got.details.contains("S01E02") && !got.details.contains("S1 E2"),
        "details must not carry the code: {:?}",
        got.details
    );
    assert_eq!(
        got.state, "S01E02 - A brave synopsis",
        "T4: default state carries the code plus the synopsis"
    );
    let n = java_units(&got.details);
    assert!(
        n <= 128,
        "details length {n} exceeds 128: {:?}",
        got.details
    );
}

/// T4: series default details fall back through episode title, filename
/// episode name, then empty -- never the show title -- and truncate at 128.
/// The code never appears here.
#[test]
fn series_default_details_fallbacks_and_truncation() {
    const TV_RAW: &str = "Show.S01E02.Confrontation.1080p.mkv - PotPlayer";
    let run_act = Action {
        running: true,
        has_start: true,
        start: 12345,
        ct: 1000,
        ..Action::default()
    };
    // Episode title wins over the show, even when the code is known.
    let got = build_activity_resolved(
        TV_RAW,
        Some(&run_act),
        &Resolved {
            series: "Show".to_string(),
            episode_name: "Confrontation".to_string(),
            season: 1,
            number: 2,
            ..Resolved::default()
        },
    );
    assert_eq!(got.details, "Confrontation", "episode title first");
    assert!(
        !got.details.contains("S01E02"),
        "code must not leak into details: {:?}",
        got.details
    );
    // No canonical episode title: the filename's own episode name stands in
    // (never the show title -- the header already carries it -- and the code
    // stays out).
    let got = build_activity_resolved(
        TV_RAW,
        Some(&run_act),
        &Resolved {
            series: "Show".to_string(),
            season: 1,
            number: 2,
            ..Resolved::default()
        },
    );
    assert_eq!(got.details, "Confrontation", "filename episode-name fallback");
    assert!(
        !got.details.contains("S01E02"),
        "code must not leak into details: {:?}",
        got.details
    );
    // Episode title when the show is missing.
    let got = build_activity_resolved(
        TV_RAW,
        Some(&run_act),
        &Resolved {
            episode_name: "Confrontation".to_string(),
            ..Resolved::default()
        },
    );
    assert_eq!(got.details, "Confrontation", "episode title fallback");
    // Filename episode name when nothing resolved.
    let got = build_activity_resolved(TV_RAW, Some(&run_act), &Resolved::default());
    assert_eq!(got.details, "Confrontation", "filename episode-name fallback");
    // Long episode title truncates at 128, never silently.
    let long = "D".repeat(200);
    let got = build_activity_resolved(
        TV_RAW,
        Some(&run_act),
        &Resolved {
            series: "Show".to_string(),
            episode_name: long.clone(),
            season: 1,
            number: 2,
            ..Resolved::default()
        },
    );
    let n = java_units(&got.details);
    assert!(
        n <= 128,
        "details length {n} exceeds 128: {:?}",
        got.details
    );
    assert!(
        got.details.starts_with("D"),
        "truncated details must keep the episode head: {:?}",
        got.details
    );
    assert!(
        !got.details.contains("S01E02"),
        "code must not leak into details: {:?}",
        got.details
    );
}

/// T1 regression (T4 shape): unresolved `Show.S01E02.1080p.mkv` (no
/// series/episode title, spaced tag `S1 E2`) leaves details empty -- never the
/// show header, never the clean title, never the tag. Ladder: episode title
/// (canonical, then filename) → empty; the state line still carries the tag.
#[test]
fn series_default_details_skips_spaced_tag() {
    const RAW: &str = "Show.S01E02.1080p.mkv - PotPlayer";
    let run_act = Action {
        running: true,
        has_start: true,
        start: 12345,
        ct: 1000,
        ..Action::default()
    };
    let parsed = titleclean::clean("Show.S01E02.1080p.mkv");
    assert!(
        parsed.episode_name.is_empty(),
        "fixture has no episode name: {:?}",
        parsed.episode_name
    );
    assert_eq!(parsed.episode_tag, "S1 E2", "fixture tag");
    assert_eq!(parsed.title, "Show", "fixture clean title");
    let got = build_activity_resolved(RAW, Some(&run_act), &Resolved::default());
    assert_eq!(
        got.details, "",
        "no episode title leaves details empty instead of repeating the header"
    );
    assert!(
        !got.details.contains("S1 E2"),
        "default must not leak the spaced tag: {:?}",
        got.details
    );
    assert_eq!(got.state, "S1 E2", "state still carries the tag");
    // The episode preset keeps the episode title alone: no code prefix.
    let ep_spec = CardSpec {
        details: crate::cardspec::CARD_DETAILS_EPISODE.to_string(),
        state: CARD_STATE_DEFAULT.to_string(),
    };
    let card = compose_card(
        RAW,
        Some(&run_act),
        &Resolved {
            episode_name: "Confrontation".to_string(),
            season: 1,
            number: 2,
            ..Resolved::default()
        },
        false,
        &ep_spec,
    );
    assert_eq!(
        card.details, "Confrontation",
        "episode preset is the title alone"
    );
}

/// T4: series default state is the synopsis line (`SxxExx` code plus the
/// synopsis, the synopsis-preset shape), keeps the paused marker, and the
/// synopsis preset keeps its `-` join. Details stays the episode title.
#[test]
fn series_default_state_prefers_episode_title() {
    const TV_RAW: &str = "Show.S01E02.Confrontation.1080p.mkv - PotPlayer";
    let run_act = Action {
        running: true,
        has_start: true,
        start: 12345,
        ct: 1000,
        ..Action::default()
    };
    let res = Resolved {
        series: "Show".to_string(),
        episode_name: "Confrontation".to_string(),
        synopsis: "A brave synopsis".to_string(),
        season: 1,
        number: 2,
        ..Resolved::default()
    };
    let got = build_activity_resolved(TV_RAW, Some(&run_act), &res);
    assert_eq!(got.details, "Confrontation", "details stays the episode title");
    assert_eq!(
        got.state, "S01E02 - A brave synopsis",
        "code plus synopsis"
    );
    assert_eq!(
        got.state.matches("S01E02").count(),
        1,
        "the code appears once: {:?}",
        got.state
    );
    // Paused keeps the code plus the synopsis plus the marker.
    let paused = Action {
        paused: true,
        ct: 1000,
        ..Action::default()
    };
    let got = build_activity_resolved(TV_RAW, Some(&paused), &res);
    assert_eq!(
        got.state, "S01E02 - A brave synopsis - Paused",
        "paused marker"
    );
    // Synopsis preset agrees with the default on the same input.
    let syn_spec = CardSpec {
        details: CARD_DETAILS_DEFAULT.to_string(),
        state: CARD_STATE_SYNOPSIS.to_string(),
    };
    let card = compose_card(TV_RAW, Some(&run_act), &res, false, &syn_spec);
    assert_eq!(
        card.state, "S01E02 - A brave synopsis",
        "synopsis state uses the code"
    );
}

/// The retired `episode` preset still renders the episode title alone for old
/// in-flight specs (it is byte-identical to the default) -- the `S01E02 · Title`
/// code prefix is gone. Without a canonical title it keeps the default line.
#[test]
fn episode_preset_is_title_only() {
    const TV_RAW: &str = "Show.S01E02.Confrontation.1080p.mkv - PotPlayer";
    let run_act = Action {
        running: true,
        has_start: true,
        start: 12345,
        ct: 1000,
        ..Action::default()
    };
    let ep_spec = CardSpec {
        details: "episode".to_string(),
        state: CARD_STATE_DEFAULT.to_string(),
    };
    let card = compose_card(
        TV_RAW,
        Some(&run_act),
        &Resolved {
            series: "Show".to_string(),
            episode_name: "Confrontation".to_string(),
            season: 1,
            number: 2,
            ..Resolved::default()
        },
        false,
        &ep_spec,
    );
    assert_eq!(
        card.details, "Confrontation",
        "episode title alone, never the code"
    );
    // No canonical episode title: the default line, as today (the filename's
    // own episode name -- never the show header).
    let card = compose_card(
        TV_RAW,
        Some(&run_act),
        &Resolved {
            series: "Show".to_string(),
            season: 1,
            number: 2,
            ..Resolved::default()
        },
        false,
        &ep_spec,
    );
    assert_eq!(card.details, "Confrontation", "fallback keeps the default line");
}

/// T1: absolute-only renders `E02`, a number-less special omits the code, and
/// a movie card ignores episode-code data entirely.
#[test]
fn absolute_special_and_movie_code_shapes() {
    const TV_RAW: &str = "Show.S01E02.Confrontation.1080p.mkv - PotPlayer";
    const MOVIE_RAW: &str = "Dune.Part.Two.2024.1080p.mkv - PotPlayer";
    let run_act = Action {
        running: true,
        has_start: true,
        start: 12345,
        ct: 1000,
        ..Action::default()
    };
    // Absolute-only: the episode title leads details (the code never does),
    // never S01E02.
    let got = build_activity_resolved(
        TV_RAW,
        Some(&run_act),
        &Resolved {
            series: "Show".to_string(),
            episode_name: "The Second".to_string(),
            absolute: 2,
            ..Resolved::default()
        },
    );
    assert_eq!(got.details, "The Second", "absolute details");
    assert!(
        !got.details.contains("S0") && !got.details.contains("E02"),
        "absolute must never render a code in details: {:?}",
        got.details
    );
    // Special with no number: the code is omitted and no header repeats --
    // details keeps the filename's episode name, never the show title.
    let got = build_activity_resolved(
        TV_RAW,
        Some(&run_act),
        &Resolved {
            series: "Show".to_string(),
            season: 0,
            number: 0,
            ..Resolved::default()
        },
    );
    assert_eq!(
        got.details, "Confrontation",
        "number-less special omits the code without repeating the header"
    );
    // Movie branches ignore episode-code data (T5-A shapes: year fallback on
    // details, year/genres/runtime join on state, empty tooltip).
    let got = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            series: "Dune: Part Two".to_string(),
            year: 2024,
            is_movie: true,
            genres: "Sci-Fi".to_string(),
            length: "2h 46m".to_string(),
            season: 1,
            number: 2,
            ..Resolved::default()
        },
    );
    assert_eq!(got.details, "2024", "movie details ignore the code");
    assert_eq!(
        got.state, "Sci-Fi \u{b7} 2h 46m",
        "movie state ignores the code and never repeats the year-showing details"
    );
    assert_eq!(got.large_text, "", "movie tooltip stays omitted");
}

/// T4 (red first): the series default State is the synopsis line -- the
/// `SxxExx` code plus the synopsis (the synopsis-preset shape) -- while
/// details keeps the episode title alone. The code appears once, in the
/// state only. Without a synopsis the line falls to the tag, then the
/// clean title; it never carries the episode title.
#[test]
fn series_default_state_carries_code_plus_title() {
    const TV_RAW: &str = "Death.Note.S01E03.Dealings.1080p.mkv - PotPlayer";
    let run_act = Action {
        running: true,
        has_start: true,
        start: 12345,
        ct: 1000,
        ..Action::default()
    };
    let syn = "While the world's criminals fall silent";
    // Code + synopsis; details keeps the episode title alone.
    let got = build_activity_resolved(
        TV_RAW,
        Some(&run_act),
        &Resolved {
            series: "Death Note".to_string(),
            episode_name: "Dealings".to_string(),
            synopsis: syn.to_string(),
            season: 1,
            number: 3,
            ..Resolved::default()
        },
    );
    assert_eq!(got.name, "Death Note", "header keeps the show");
    assert_eq!(got.details, "Dealings", "details");
    assert!(
        !got.details.contains("S01E03"),
        "details must not carry the code: {:?}",
        got.details
    );
    assert_eq!(
        got.state,
        format!("S01E03 - {syn}"),
        "code plus synopsis"
    );
    assert_eq!(
        got.state.matches("S01E03").count(),
        1,
        "the code appears once: {:?}",
        got.state
    );
    assert!(
        !got.state.contains("Dealings"),
        "default state must not carry the episode title: {:?}",
        got.state
    );
    let n = java_units(&got.state);
    assert!(
        n <= STATE_SYNOPSIS_BUDGET,
        "synopsis state length {n} exceeds {STATE_SYNOPSIS_BUDGET}: {:?}",
        got.state
    );
    // No code with a synopsis: the filename tag joins the synopsis.
    let got = build_activity_resolved(
        TV_RAW,
        Some(&run_act),
        &Resolved {
            series: "Death Note".to_string(),
            episode_name: "Dealings".to_string(),
            synopsis: syn.to_string(),
            ..Resolved::default()
        },
    );
    assert_eq!(
        got.state,
        format!("S1 E3 - {syn}"),
        "tag joins the synopsis when no code exists"
    );
    // No synopsis with a code: the tag fallback stands (never the title).
    let got = build_activity_resolved(
        TV_RAW,
        Some(&run_act),
        &Resolved {
            series: "Death Note".to_string(),
            episode_name: "Dealings".to_string(),
            season: 1,
            number: 3,
            ..Resolved::default()
        },
    );
    assert_eq!(got.state, "S1 E3", "no synopsis falls back to the tag");
    // Code plus synopsis without an episode title: details stays empty (never
    // the show header -- the header already carries it) while the state
    // carries the code plus the synopsis.
    let got = build_activity_resolved(
        "Show.S01E03.1080p.mkv - PotPlayer",
        Some(&run_act),
        &Resolved {
            series: "Show".to_string(),
            synopsis: syn.to_string(),
            season: 1,
            number: 3,
            ..Resolved::default()
        },
    );
    assert_eq!(
        got.details, "",
        "no episode title leaves details empty instead of repeating the header"
    );
    assert_eq!(
        got.state,
        format!("S01E03 - {syn}"),
        "code plus synopsis without a title"
    );
    // Absolute-only: no fabricated season.
    let got = build_activity_resolved(
        TV_RAW,
        Some(&run_act),
        &Resolved {
            series: "Show".to_string(),
            episode_name: "Title".to_string(),
            synopsis: syn.to_string(),
            absolute: 2,
            ..Resolved::default()
        },
    );
    assert_eq!(
        got.state,
        format!("E02 - {syn}"),
        "absolute code plus synopsis"
    );
    assert!(
        !got.state.contains("S01") && !got.state.contains("S00"),
        "absolute must never fabricate a season: {:?}",
        got.state
    );
    // The real AniList absolute production shape (season 0, number set to the
    // absolute episode): still `E05`, never `S00E05`.
    let got = build_activity_resolved(
        TV_RAW,
        Some(&run_act),
        &Resolved {
            series: "Show".to_string(),
            episode_name: "Title".to_string(),
            synopsis: syn.to_string(),
            season: 0,
            number: 5,
            absolute: 5,
            ..Resolved::default()
        },
    );
    assert_eq!(
        got.state,
        format!("E05 - {syn}"),
        "absolute production shape renders bare, never S00"
    );
    assert!(
        !got.state.contains("S00"),
        "season 0 must never fabricate a season: {:?}",
        got.state
    );
    // One-season shows keep S01.
    let got = build_activity_resolved(
        TV_RAW,
        Some(&run_act),
        &Resolved {
            series: "Death Note".to_string(),
            episode_name: "Dealings".to_string(),
            synopsis: syn.to_string(),
            season: 1,
            number: 1,
            ..Resolved::default()
        },
    );
    assert_eq!(
        got.state,
        format!("S01E01 - {syn}"),
        "one-season keeps the season"
    );
    // Paused keeps the marker within the synopsis budget.
    let paused = Action {
        paused: true,
        ct: 1000,
        ..Action::default()
    };
    let got = build_activity_resolved(
        TV_RAW,
        Some(&paused),
        &Resolved {
            series: "Death Note".to_string(),
            episode_name: "Dealings".to_string(),
            synopsis: syn.to_string(),
            season: 1,
            number: 3,
            ..Resolved::default()
        },
    );
    assert_eq!(
        got.state,
        format!("S01E03 - {syn} - Paused"),
        "paused marker"
    );
    let n = java_units(&got.state);
    assert!(
        n <= STATE_SYNOPSIS_BUDGET,
        "paused state length {n} exceeds {STATE_SYNOPSIS_BUDGET}: {:?}",
        got.state
    );
    assert!(
        n <= 128,
        "paused state length {n} exceeds 128: {:?}",
        got.state
    );
    // Long synopsis truncates at the 72 budget, never silently.
    let long_syn = "A retired operative living quietly is pulled back when a ruthless syndicate burns his home. He crosses continents, gathers old allies, and faces a final choice that will decide everything he loves and more.";
    let got = build_activity_resolved(
        TV_RAW,
        Some(&run_act),
        &Resolved {
            series: "Death Note".to_string(),
            episode_name: "Dealings".to_string(),
            synopsis: long_syn.to_string(),
            season: 1,
            number: 3,
            ..Resolved::default()
        },
    );
    let n = java_units(&got.state);
    assert!(
        n <= STATE_SYNOPSIS_BUDGET,
        "state length {n} exceeds {STATE_SYNOPSIS_BUDGET}: {:?}",
        got.state
    );
    assert!(
        got.state.starts_with("S01E03 - "),
        "truncated state must keep the code head: {:?}",
        got.state
    );
    assert!(
        got.state.ends_with('\u{2026}') || !got.state.contains('\u{2026}'),
        "truncated state must signal the cut: {:?}",
        got.state
    );
    // T5-A movie default ignores episode-code data: no synopsis here, so
    // details fall back to the year and state is genres/length without
    // repeating it.
    let got = build_activity_resolved(
        "Dune.Part.Two.2024.1080p.mkv - PotPlayer",
        Some(&run_act),
        &Resolved {
            series: "Dune: Part Two".to_string(),
            year: 2024,
            is_movie: true,
            genres: "Sci-Fi".to_string(),
            length: "2h 46m".to_string(),
            season: 1,
            number: 3,
            episode_name: "Dealings".to_string(),
            ..Resolved::default()
        },
    );
    assert_eq!(got.details, "2024", "movie details ignore the code");
    assert_eq!(
        got.state, "Sci-Fi \u{b7} 2h 46m",
        "movie default ignores the code and never repeats the year"
    );
    // Custom presets are unchanged.
    for (preset, want) in [
        (crate::cardspec::CARD_STATE_TAG, "S1 E3"),
        (
            crate::cardspec::CARD_STATE_SYNOPSIS,
            "S01E03 - A brave synopsis",
        ),
    ] {
        let spec = CardSpec {
            details: CARD_DETAILS_DEFAULT.to_string(),
            state: preset.to_string(),
        };
        let card = compose_card(
            TV_RAW,
            Some(&run_act),
            &Resolved {
                series: "Death Note".to_string(),
                episode_name: "Dealings".to_string(),
                synopsis: "A brave synopsis".to_string(),
                season: 1,
                number: 3,
                ..Resolved::default()
            },
            false,
            &spec,
        );
        assert_eq!(card.state, want, "preset {preset} unchanged");
    }
    let spec = CardSpec {
        details: CARD_DETAILS_DEFAULT.to_string(),
        state: crate::cardspec::CARD_STATE_NONE.to_string(),
    };
    let card = compose_card(
        TV_RAW,
        Some(&run_act),
        &Resolved {
            series: "Death Note".to_string(),
            episode_name: "Dealings".to_string(),
            season: 1,
            number: 3,
            ..Resolved::default()
        },
        false,
        &spec,
    );
    assert_eq!(card.state, "", "minimal stays empty");
}

/// T5-A (red first): movie defaults. Details is the synopsis (128, word-aware),
/// never the title and never `Title (Year)`; state is
/// `<Year> · <Genres> · <Runtime>` with missing parts skipped, never the title;
/// the movie tooltip is omitted (empty) so the synopsis is not repeated. TV
/// T4 stays: episode title / `SxxExx - synopsis` / full tooltip.
#[test]
fn movie_t5a_details_is_synopsis_never_title() {
    const MOVIE_RAW: &str = "Dune.Part.Two.2024.1080p.mkv - PotPlayer";
    let run_act = Action {
        running: true,
        has_start: true,
        start: 12345,
        ct: 1000,
        ..Action::default()
    };
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
    assert_eq!(got.details, "Paul unites the Fremen", "movie details");
    assert!(
        !got.details.contains("Dune"),
        "details must never carry the title: {:?}",
        got.details
    );
    assert!(
        !got.details.contains("(2024)"),
        "details must never be Title (Year): {:?}",
        got.details
    );
}

#[test]
fn movie_t5a_state_is_year_genres_runtime() {
    const MOVIE_RAW: &str = "Dune.Part.Two.2024.1080p.mkv - PotPlayer";
    let run_act = Action {
        running: true,
        has_start: true,
        start: 12345,
        ct: 1000,
        ..Action::default()
    };
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
    assert_eq!(got.state, "2024 \u{b7} Sci-Fi \u{b7} 2h 46m", "movie state");
    // The brief's shape example.
    let got = build_activity_resolved(
        "Movie.2019.1080p.mkv - PotPlayer",
        Some(&run_act),
        &Resolved {
            series: "Example".to_string(),
            year: 2019,
            is_movie: true,
            genres: "Crime, Drama, Thriller".to_string(),
            length: "2h 3m".to_string(),
            synopsis: "A synopsis".to_string(),
            ..Resolved::default()
        },
    );
    assert_eq!(
        got.state, "2019 \u{b7} Crime, Drama, Thriller \u{b7} 2h 3m",
        "brief example shape"
    );
}

#[test]
fn movie_t5a_details_fallbacks_skip_missing_never_title() {
    const MOVIE_RAW: &str = "Dune.Part.Two.2024.1080p.mkv - PotPlayer";
    let run_act = Action {
        running: true,
        has_start: true,
        start: 12345,
        ct: 1000,
        ..Action::default()
    };
    // No synopsis: year wins.
    let got = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            series: "Dune: Part Two".to_string(),
            year: 2024,
            is_movie: true,
            genres: "Sci-Fi".to_string(),
            length: "2h 46m".to_string(),
            ..Resolved::default()
        },
    );
    assert_eq!(got.details, "2024", "no synopsis falls back to year");
    assert!(
        !got.details.contains("Dune"),
        "fallback must never be the title: {:?}",
        got.details
    );
    // No synopsis, no year: genres/length.
    let got = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            series: "Dune: Part Two".to_string(),
            is_movie: true,
            genres: "Sci-Fi".to_string(),
            length: "2h 46m".to_string(),
            ..Resolved::default()
        },
    );
    assert_eq!(
        got.details, "Sci-Fi \u{b7} 2h 46m",
        "no synopsis/year falls back to genres/length"
    );
    // Nothing: empty, never the title.
    let got = build_activity_resolved(
        "Movie.mkv - PotPlayer",
        Some(&run_act),
        &Resolved {
            series: "Dune".to_string(),
            is_movie: true,
            ..Resolved::default()
        },
    );
    assert_eq!(got.details, "", "bare movie details stays empty");
    assert!(
        !got.details.contains("Dune"),
        "bare details must never be the title: {:?}",
        got.details
    );
    // State skips missing parts -- and never repeats the details line: with no
    // synopsis the details line already shows the genres/length pair, so the
    // state stays empty.
    let got = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            series: "Dune".to_string(),
            is_movie: true,
            length: "2h 19m".to_string(),
            ..Resolved::default()
        },
    );
    assert_eq!(got.details, "2h 19m", "details carries genres/length");
    assert_eq!(
        got.state, "",
        "state stays empty beside a genres/length details line"
    );
    let got = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            series: "Dune".to_string(),
            year: 2024,
            is_movie: true,
            ..Resolved::default()
        },
    );
    assert_eq!(got.details, "2024", "details carries the year");
    assert_eq!(
        got.state, "",
        "state stays empty beside a year details line"
    );
    // State with nothing: empty, never the title.
    let got = build_activity_resolved(
        "Movie.mkv - PotPlayer",
        Some(&run_act),
        &Resolved {
            series: "Dune".to_string(),
            is_movie: true,
            ..Resolved::default()
        },
    );
    assert_eq!(got.state, "", "bare movie state stays empty");
    assert!(
        !got.state.contains("Dune"),
        "bare state must never be the title: {:?}",
        got.state
    );
}

/// The movie default never puts the same text on both lines: with a synopsis
/// the details carry it and the state joins year/genres/runtime; without one
/// the details carry the year (then the state is genres/length, never
/// repeating the year) or genres/length (then the state stays empty).
/// Both lines are asserted together: a details-only assertion cannot catch a
/// state duplication.
#[test]
fn movie_default_details_and_state_never_repeat() {
    const MOVIE_RAW: &str = "Dune.Part.Two.2024.1080p.mkv - PotPlayer";
    const MID: &str = " \u{b7} ";
    let run_act = Action {
        running: true,
        has_start: true,
        start: 12345,
        ct: 1000,
        ..Action::default()
    };
    // Synopsis: details the synopsis, state year · genres · runtime.
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
    assert_eq!(got.details, "Paul unites the Fremen", "synopsis details");
    assert_eq!(
        got.state,
        format!("2024{MID}Sci-Fi{MID}2h 46m"),
        "synopsis state joins year, genres and runtime"
    );
    // No synopsis with a year: details the year, state genres/length without
    // repeating the year.
    let got = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            series: "Dune: Part Two".to_string(),
            year: 2024,
            is_movie: true,
            genres: "Sci-Fi".to_string(),
            length: "2h 46m".to_string(),
            ..Resolved::default()
        },
    );
    assert_eq!(got.details, "2024", "year details");
    assert_eq!(
        got.state,
        format!("Sci-Fi{MID}2h 46m"),
        "state beside a year never repeats it"
    );
    // No synopsis, no year: details genres/length, state empty.
    let got = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            series: "Dune: Part Two".to_string(),
            is_movie: true,
            genres: "Sci-Fi".to_string(),
            length: "2h 46m".to_string(),
            ..Resolved::default()
        },
    );
    assert_eq!(
        got.details,
        format!("Sci-Fi{MID}2h 46m"),
        "genres/length details"
    );
    assert_eq!(
        got.state, "",
        "state stays empty beside a genres/length details line"
    );
}

#[test]
fn movie_t5a_tooltip_empty_tv_unchanged() {
    const MOVIE_RAW: &str = "Movie.mkv - PotPlayer";
    const TV_RAW: &str = "Show.S01E02.Pilot.1080p.mkv - PotPlayer";
    let run_act = Action {
        running: true,
        has_start: true,
        start: 12345,
        ct: 1000,
        ..Action::default()
    };
    let got = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            series: "Some Film".to_string(),
            year: 2024,
            is_movie: true,
            genres: "Sci-Fi".to_string(),
            length: "2h 46m".to_string(),
            synopsis: "Paul unites the Fremen".to_string(),
            ..Resolved::default()
        },
    );
    assert_eq!(got.large_text, "", "movie tooltip is omitted, not repeated");
    let got = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            series: "Some Film".to_string(),
            is_movie: true,
            ..Resolved::default()
        },
    );
    assert_eq!(got.large_text, "", "movie tooltip stays empty without synopsis");
    // TV keeps the full episode synopsis in the tooltip.
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
    assert_eq!(got.large_text, "A brave synopsis", "TV tooltip keeps the synopsis");
}

#[test]
fn movie_t5a_no_title_in_details_or_state() {
    const MOVIE_RAW: &str = "Dune.Part.Two.2024.1080p.mkv - PotPlayer";
    let run_act = Action {
        running: true,
        has_start: true,
        start: 12345,
        ct: 1000,
        ..Action::default()
    };
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
    assert!(
        !got.details.contains("Dune: Part Two"),
        "title must not leak into details: {:?}",
        got.details
    );
    assert!(
        !got.state.contains("Dune"),
        "title must not leak into state: {:?}",
        got.state
    );
    // Header still carries the title (the pill).
    assert_eq!(got.name, "Dune: Part Two", "header keeps the title");
}

#[test]
fn movie_t5a_details_128_word_aware() {
    const MOVIE_RAW: &str = "Movie.mkv - PotPlayer";
    let run_act = Action {
        running: true,
        has_start: true,
        start: 12345,
        ct: 1000,
        ..Action::default()
    };
    let syn = format!("{}tail", "word ".repeat(40));
    let got = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            series: "Some Film".to_string(),
            is_movie: true,
            synopsis: syn.clone(),
            ..Resolved::default()
        },
    );
    let want = trim_word(&syn, 128);
    assert_eq!(got.details, want, "details trims word-aware at 128");
    assert!(
        got.details.ends_with('\u{2026}'),
        "truncated details must signal the cut: {:?}",
        got.details
    );
    assert!(
        !got.details.trim_end_matches('\u{2026}').ends_with("wo"),
        "details ended mid-word: {:?}",
        got.details
    );
    let n = java_units(&got.details);
    assert!(n <= 128, "details length {n} exceeds 128: {:?}", got.details);
    // Spaceless run still signals within budget.
    let nospace = "s".repeat(200);
    let got = build_activity_resolved(
        MOVIE_RAW,
        Some(&run_act),
        &Resolved {
            series: "Some Film".to_string(),
            is_movie: true,
            synopsis: nospace.clone(),
            ..Resolved::default()
        },
    );
    assert_eq!(
        got.details,
        format!("{}\u{2026}", "s".repeat(127)),
        "spaceless details cut"
    );
}

#[test]
fn movie_t5a_paused_keeps_marker_and_budgets() {
    const MOVIE_RAW: &str = "Dune.Part.Two.2024.1080p.mkv - PotPlayer";
    let paused = Action {
        paused: true,
        ct: 1000,
        ..Action::default()
    };
    let got = build_activity_resolved(
        MOVIE_RAW,
        Some(&paused),
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
    assert!(
        got.state.ends_with(PAUSED_MARKER),
        "paused movie state keeps the marker: {:?}",
        got.state
    );
    assert!(
        got.state.starts_with("2024"),
        "paused state keeps the year head: {:?}",
        got.state
    );
    assert!(
        !got.state.contains("Dune"),
        "paused state must not carry the title: {:?}",
        got.state
    );
    let n = java_units(&got.state);
    assert!(n <= 128, "paused state length {n} exceeds 128: {:?}", got.state);
    let n = java_units(&got.details);
    assert!(
        n <= 128,
        "paused details length {n} exceeds 128: {:?}",
        got.details
    );
}

#[test]
fn movie_t5a_tv_t4_unchanged() {
    const TV_RAW: &str = "Show.S01E02.Confrontation.1080p.mkv - PotPlayer";
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
            series: "Show".to_string(),
            episode_name: "Confrontation".to_string(),
            synopsis: "A brave synopsis".to_string(),
            season: 1,
            number: 2,
            ..Resolved::default()
        },
    );
    assert_eq!(got.details, "Confrontation", "TV details stays episode title only");
    assert_eq!(got.state, "S01E02 - A brave synopsis", "TV state stays code-synopsis");
    assert_eq!(got.large_text, "A brave synopsis", "TV tooltip stays the synopsis");
    assert!(
        !got.details.contains("Show"),
        "TV details must not repeat the show: {:?}",
        got.details
    );
    // Absolute-only still renders E02, never a fake season; one-season keeps S01.
    let got = build_activity_resolved(
        TV_RAW,
        Some(&run_act),
        &Resolved {
            series: "Show".to_string(),
            episode_name: "Title".to_string(),
            synopsis: "While the world's criminals fall silent".to_string(),
            absolute: 2,
            ..Resolved::default()
        },
    );
    assert_eq!(
        got.state, "E02 - While the world's criminals fall silent",
        "absolute E02, no fake season"
    );
}
