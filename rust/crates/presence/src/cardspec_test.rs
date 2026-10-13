//! `cardspec_test` -- mirrors `go\internal\presence\cardspec_test.go`.
//!
//! Proves the Card spec seam stays safe: the default spec renders BYTE-FOR-BYTE
//! what `build_activity_resolved` produces today, the zero value falls back to
//! it, and every preset stays within the 128-unit cap with the paused marker
//! preserved.

use crate::cardspec::{
    CARD_DETAILS_DEFAULT, CARD_DETAILS_EPISODE, CARD_DETAILS_SERIES, CARD_DETAILS_TITLE,
    CARD_STATE_DEFAULT, CARD_STATE_NONE, CARD_STATE_SYNOPSIS, CARD_STATE_TAG, CardPreset, CardSpec,
    card_presets, default_card_spec, is_idle_title,
};
use crate::{Action, PAUSED_MARKER, Resolved, build_activity_resolved};

struct CardSpecCase {
    name: &'static str,
    raw: String,
    act: Option<Action>,
    res: Resolved,
}

fn run_action(start: i64, ct: i64) -> Option<Action> {
    Some(Action {
        running: true,
        has_start: true,
        start,
        ct,
        ..Action::default()
    })
}

fn paused_action() -> Option<Action> {
    Some(Action {
        paused: true,
        ct: 1000,
        ..Action::default()
    })
}

fn card_spec_cases() -> Vec<CardSpecCase> {
    const TV_RAW: &str = "Show.S01E02.Pilot.1080p.mkv - PotPlayer";
    let resolved_series = Resolved {
        series: "Canonical Series".to_string(),
        episode_name: "Canonical Episode".to_string(),
        synopsis: "A brave synopsis".to_string(),
        ..Resolved::default()
    };
    vec![
        CardSpecCase {
            name: "idle-empty",
            raw: String::new(),
            act: None,
            res: Resolved::default(),
        },
        CardSpecCase {
            name: "idle-app",
            raw: "PotPlayer".to_string(),
            act: Some(Action::default()),
            res: Resolved::default(),
        },
        CardSpecCase {
            name: "series-run",
            raw: TV_RAW.to_string(),
            act: run_action(12345, 1000),
            res: Resolved::default(),
        },
        CardSpecCase {
            name: "series-tagonly",
            raw: "Show.S01E02.1080p.mkv - PotPlayer".to_string(),
            act: run_action(12345, 1000),
            res: Resolved::default(),
        },
        CardSpecCase {
            name: "series-paused",
            raw: TV_RAW.to_string(),
            act: paused_action(),
            res: Resolved::default(),
        },
        CardSpecCase {
            name: "series-resolved",
            raw: TV_RAW.to_string(),
            act: run_action(12345, 1000),
            res: resolved_series.clone(),
        },
        CardSpecCase {
            name: "series-resolved-paused",
            raw: TV_RAW.to_string(),
            act: paused_action(),
            res: resolved_series,
        },
        CardSpecCase {
            name: "series-synonly",
            raw: "Movie.mkv - PotPlayer".to_string(),
            act: run_action(1, 2),
            res: Resolved {
                synopsis: "Just a synopsis".to_string(),
                ..Resolved::default()
            },
        },
        CardSpecCase {
            name: "movie-full",
            raw: "Dune.Part.Two.2024.1080p.mkv - PotPlayer".to_string(),
            act: run_action(12345, 1000),
            res: Resolved {
                series: "Dune: Part Two".to_string(),
                year: 2024,
                is_movie: true,
                genres: "Sci-Fi".to_string(),
                length: "2h 46m".to_string(),
                synopsis: "Paul unites the Fremen".to_string(),
                ..Resolved::default()
            },
        },
        CardSpecCase {
            name: "movie-paused",
            raw: "Dune.Part.Two.2024.1080p.mkv - PotPlayer".to_string(),
            act: paused_action(),
            res: Resolved {
                series: "Dune".to_string(),
                year: 2024,
                is_movie: true,
                genres: "Sci-Fi".to_string(),
                length: "2h 46m".to_string(),
                synopsis: "Paul unites the Fremen".to_string(),
                ..Resolved::default()
            },
        },
        CardSpecCase {
            name: "movie-noyear",
            raw: "Dune.Part.Two.2024.1080p.mkv - PotPlayer".to_string(),
            act: run_action(1, 2),
            res: Resolved {
                series: "Dune: Part Two".to_string(),
                is_movie: true,
                ..Resolved::default()
            },
        },
        CardSpecCase {
            name: "artwork-buttons",
            raw: TV_RAW.to_string(),
            act: run_action(1, 2),
            res: Resolved {
                series: "S".to_string(),
                artwork: "https://example.com/poster.jpg".to_string(),
                buttons: vec![crate::Button {
                    label: "Trailer".to_string(),
                    url: "https://example.com/trailer".to_string(),
                }],
                ..Resolved::default()
            },
        },
        CardSpecCase {
            name: "long",
            raw: format!(
                "{}.S01E02.{}.mkv - PotPlayer",
                "L".repeat(200),
                "E".repeat(200)
            ),
            act: run_action(1, 2),
            res: Resolved {
                series: "y".repeat(200),
                episode_name: "z".repeat(200),
                synopsis: format!("{}tail", "word ".repeat(40)),
                ..Resolved::default()
            },
        },
        CardSpecCase {
            name: "stop",
            raw: TV_RAW.to_string(),
            act: Some(Action {
                ct: 5,
                ..Action::default()
            }),
            res: Resolved::default(),
        },
        CardSpecCase {
            name: "nil-action",
            raw: TV_RAW.to_string(),
            act: None,
            res: Resolved::default(),
        },
    ]
}

/// The safety gate for the Card view: for a table of inputs,
/// `default_card_spec().render` must equal what `build_activity_resolved`
/// produces today.
///
/// NOTE: this is a change-detector only (render calls `build_activity_resolved`
/// for the default case), so preview-is-the-payload is proven separately by the
/// card view's own wire test, which captures the real payload and asserts the
/// preview equals it.
#[test]
fn card_spec_default_equivalence() {
    let def = default_card_spec();
    for c in card_spec_cases() {
        let want = build_activity_resolved(&c.raw, c.act.as_ref(), &c.res);
        let (got_d, got_s) = def.render(&c.raw, c.act.as_ref(), &c.res);
        assert_eq!(
            (got_d.as_str(), got_s.as_str()),
            (want.details.as_str(), want.state.as_str()),
            "{}: render mismatch",
            c.name
        );
    }
}

/// Breaks the tautology: instead of comparing render to `build_activity_resolved`
/// (which render calls for the default case), it asserts hardcoded golden lines
/// computed independently. If render drifts from the shipped output, this fails
/// even though the change-detector above would still pass.
#[test]
fn card_spec_default_golden_independent() {
    let def = default_card_spec();
    // Series filename: details is the episode name, state is the tag.
    let (d, s) = def.render(
        "Show.S01E02.Pilot.1080p.mkv - PotPlayer",
        run_action(12345, 1000).as_ref(),
        &Resolved::default(),
    );
    assert!(
        d == "Pilot" && s == "S1 E2",
        "series render = ({d:?}, {s:?}), want (\"Pilot\", \"S1 E2\")"
    );
    // Movie with canonical override: details is "Title (Year)", state joins
    // genres/length/synopsis with " . ".
    let (d, s) = def.render(
        "Dune.Part.Two.2024.1080p.mkv - PotPlayer",
        run_action(12345, 1000).as_ref(),
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
        d == "Dune: Part Two (2024)" && s == "Sci-Fi \u{b7} 2h 46m \u{b7} Paul unites the Fremen",
        "movie render = ({d:?}, {s:?}), want goldens"
    );
    // Idle card keeps its shipped lines.
    let (d, s) = def.render("", Some(&Action::default()), &Resolved::default());
    assert!(
        d == "Browsing" && s == "Idle",
        "idle render = ({d:?}, {s:?}), want (\"Browsing\", \"Idle\")"
    );
}

/// Proves no migration is needed: the zero value renders exactly the shipped
/// default on every input.
#[test]
fn card_spec_zero_falls_back() {
    let zero = CardSpec::default();
    let def = default_card_spec();
    assert!(
        zero.is_default(),
        "zero CardSpec is_default = false, want true"
    );
    assert_eq!(
        zero.normalized(),
        def,
        "zero normalized = {:?}, want {:?}",
        zero.normalized(),
        def
    );
    for c in card_spec_cases() {
        let (zd, zs) = zero.render(&c.raw, c.act.as_ref(), &c.res);
        let (dd, ds) = def.render(&c.raw, c.act.as_ref(), &c.res);
        assert_eq!(
            (zd.as_str(), zs.as_str()),
            (dd.as_str(), ds.as_str()),
            "{}: zero render mismatch",
            c.name
        );
    }
}

/// Proves a stored typo can never reach the card.
#[test]
fn card_spec_unknown_falls_back() {
    let weird = CardSpec {
        details: "fancy-template".to_string(),
        state: "\u{0}".to_string(),
    };
    let def = default_card_spec();
    for c in card_spec_cases() {
        let (gd, gs) = weird.render(&c.raw, c.act.as_ref(), &c.res);
        let (dd, ds) = def.render(&c.raw, c.act.as_ref(), &c.res);
        assert_eq!(
            (gd.as_str(), gs.as_str()),
            (dd.as_str(), ds.as_str()),
            "{}: unknown render mismatch",
            c.name
        );
    }
}

/// Proves the preset sets stay small, fixed, and default-first (presets, not a
/// template language).
#[test]
fn card_presets_small() {
    let (d, s) = card_presets();
    assert!(
        d.len() == 4 && s.len() == 4,
        "presets = {} details + {} states, want 4+4",
        d.len(),
        s.len()
    );
    assert!(
        d[0].id == CARD_DETAILS_DEFAULT && s[0].id == CARD_STATE_DEFAULT,
        "first presets = {:?}/{:?}, want both {CARD_DETAILS_DEFAULT:?}",
        d[0].id,
        s[0].id
    );
    for p in d.iter().chain(s.iter()) {
        assert!(
            !p.id.is_empty() && !p.label.is_empty(),
            "preset {p:?} has an empty id or label"
        );
    }
}

/// Proves every preset line stays within the 128-unit Discord cap and the paused
/// marker survives where expected.
#[test]
fn card_spec_presets_capped() {
    let (dets, states) = card_presets();
    for c in card_spec_cases() {
        for d in &dets {
            for st in &states {
                let spec = CardSpec {
                    details: d.id.clone(),
                    state: st.id.clone(),
                };
                let (gd, gs) = spec.render(&c.raw, c.act.as_ref(), &c.res);
                let n = java_units(&gd);
                assert!(
                    n <= 128,
                    "{} {}/{}: details {n} units > 128: {gd:?}",
                    c.name,
                    d.id,
                    st.id
                );
                let n = java_units(&gs);
                assert!(
                    n <= 128,
                    "{} {}/{}: state {n} units > 128: {gs:?}",
                    c.name,
                    d.id,
                    st.id
                );
                if st.id == CARD_STATE_NONE && !is_idle_title(&c.raw) && !gs.is_empty() {
                    panic!("{}: Empty state = {gs:?}, want empty", c.name);
                }
                if c.act.as_ref().is_some_and(|a| a.paused)
                    && !is_idle_title(&c.raw)
                    && st.id != CARD_STATE_NONE
                    && gs != "Paused"
                    && !gs.ends_with(PAUSED_MARKER)
                {
                    panic!(
                        "{} {}/{}: paused state = {gs:?}, want suffix {PAUSED_MARKER:?}",
                        c.name, d.id, st.id
                    );
                }
            }
        }
    }
}

fn java_units(s: &str) -> usize {
    s.encode_utf16().count()
}

/// Pins kind-driven line selection through the active preset: the movie branch
/// (`is_movie`) appends the year on the series preset and keeps the base line on
/// the episode preset (movies have no episodes); the series branch serves
/// episodes; anime (`is_movie=false`, even with a year set) resolves through the
/// series branch, never the movie one.
#[test]
fn card_spec_kind_adapt() {
    let run = Action {
        running: true,
        has_start: true,
        start: 1,
        ct: 2,
        ..Action::default()
    };
    let m_raw = "Dune.Part.Two.2024.1080p.mkv - PotPlayer";
    let movie = Resolved {
        series: "Dune: Part Two".to_string(),
        year: 2024,
        is_movie: true,
        genres: "Sci-Fi".to_string(),
        length: "2h 46m".to_string(),
        synopsis: "Paul unites the Fremen".to_string(),
        ..Resolved::default()
    };
    let s_raw = "Show.S01E02.Pilot.1080p.mkv - PotPlayer";
    let series = Resolved {
        series: "Canonical Series".to_string(),
        episode_name: "Canonical Episode".to_string(),
        synopsis: "A brave synopsis".to_string(),
        ..Resolved::default()
    };
    let a_raw = "Frieren.S01E01.The.Heros.Rest.1080p.mkv - PotPlayer";
    let anime = Resolved {
        series: "Frieren".to_string(),
        episode_name: "The Hero's Rest".to_string(),
        synopsis: "An elf mourns".to_string(),
        year: 2023,
        ..Resolved::default()
    };
    const MID: &str = " \u{b7} ";
    struct Case {
        name: &'static str,
        raw: &'static str,
        res: Resolved,
        spec: CardSpec,
        want_d: &'static str,
        want_s: String,
    }
    let mid = |a: &str, b: &str| format!("{a}{MID}{b}");
    let cases = vec![
        Case {
            name: "movie default",
            raw: m_raw,
            res: movie.clone(),
            spec: default_card_spec(),
            want_d: "Dune: Part Two (2024)",
            want_s: mid("Sci-Fi", "2h 46m") + MID + "Paul unites the Fremen",
        },
        Case {
            name: "movie episode keeps base",
            raw: m_raw,
            res: movie.clone(),
            spec: CardSpec {
                details: CARD_DETAILS_EPISODE.to_string(),
                state: CARD_STATE_DEFAULT.to_string(),
            },
            want_d: "Dune: Part Two (2024)",
            want_s: mid("Sci-Fi", "2h 46m") + MID + "Paul unites the Fremen",
        },
        Case {
            name: "movie series appends year",
            raw: m_raw,
            res: movie.clone(),
            spec: CardSpec {
                details: CARD_DETAILS_SERIES.to_string(),
                state: CARD_STATE_DEFAULT.to_string(),
            },
            want_d: "Dune: Part Two (2024)",
            want_s: mid("Sci-Fi", "2h 46m") + MID + "Paul unites the Fremen",
        },
        Case {
            name: "movie title drops year",
            raw: m_raw,
            res: movie,
            spec: CardSpec {
                details: CARD_DETAILS_TITLE.to_string(),
                state: CARD_STATE_TAG.to_string(),
            },
            want_d: "Dune: Part Two",
            want_s: String::new(),
        },
        Case {
            name: "movie synopsis",
            raw: m_raw,
            res: Resolved {
                series: "Dune: Part Two".to_string(),
                year: 2024,
                is_movie: true,
                genres: "Sci-Fi".to_string(),
                length: "2h 46m".to_string(),
                synopsis: "Paul unites the Fremen".to_string(),
                ..Resolved::default()
            },
            spec: CardSpec {
                details: CARD_DETAILS_DEFAULT.to_string(),
                state: CARD_STATE_SYNOPSIS.to_string(),
            },
            want_d: "Dune: Part Two (2024)",
            want_s: "Paul unites the Fremen".to_string(),
        },
        Case {
            name: "series default",
            raw: s_raw,
            res: series.clone(),
            spec: default_card_spec(),
            want_d: "Canonical Episode",
            want_s: mid("S1 E2", "A brave synopsis"),
        },
        Case {
            name: "series episode",
            raw: s_raw,
            res: series.clone(),
            spec: CardSpec {
                details: CARD_DETAILS_EPISODE.to_string(),
                state: CARD_STATE_TAG.to_string(),
            },
            want_d: "Canonical Episode",
            want_s: "S1 E2".to_string(),
        },
        Case {
            name: "series series keeps title",
            raw: s_raw,
            res: series,
            spec: CardSpec {
                details: CARD_DETAILS_SERIES.to_string(),
                state: CARD_STATE_SYNOPSIS.to_string(),
            },
            want_d: "Canonical Series",
            want_s: "A brave synopsis".to_string(),
        },
        Case {
            name: "anime default is series branch",
            raw: a_raw,
            res: anime.clone(),
            spec: default_card_spec(),
            want_d: "The Hero's Rest",
            want_s: mid("S1 E1", "An elf mourns"),
        },
        Case {
            name: "anime series ignores year",
            raw: a_raw,
            res: anime.clone(),
            spec: CardSpec {
                details: CARD_DETAILS_SERIES.to_string(),
                state: CARD_STATE_TAG.to_string(),
            },
            want_d: "Frieren",
            want_s: "S1 E1".to_string(),
        },
        Case {
            name: "anime title",
            raw: a_raw,
            res: anime,
            spec: CardSpec {
                details: CARD_DETAILS_TITLE.to_string(),
                state: CARD_STATE_SYNOPSIS.to_string(),
            },
            want_d: "Frieren",
            want_s: "An elf mourns".to_string(),
        },
    ];
    for c in &cases {
        let (d, s) = c.spec.render(c.raw, Some(&run), &c.res);
        assert!(
            d == c.want_d && s == c.want_s,
            "{}: render = ({d:?}, {s:?}), want ({:?}, {:?})",
            c.name,
            c.want_d,
            c.want_s
        );
    }
}

/// Pins omitempty honesty: an absent slot renders "" (tag with no episode tag,
/// synopsis with no synopsis, none always) while the details line never blanks
/// (per-kind fallbacks keep the base line). The paused marker path is preserved:
/// paused-with-content keeps the suffix, paused-with-nothing still reads
/// "Paused" - except none, which hides even when paused.
#[test]
fn card_spec_empty_hide() {
    let run = Action {
        running: true,
        has_start: true,
        start: 1,
        ct: 2,
        ..Action::default()
    };
    let paused = Action {
        paused: true,
        ct: 1000,
        ..Action::default()
    };
    let m_raw = "Dune.Part.Two.2024.1080p.mkv - PotPlayer";
    let bare_movie = Resolved {
        series: "Dune".to_string(),
        is_movie: true,
        ..Resolved::default()
    };
    let s_raw = "Show.S01E02.Pilot.1080p.mkv - PotPlayer";
    struct Case {
        name: &'static str,
        raw: &'static str,
        act: Action,
        res: Resolved,
        spec: CardSpec,
        want_d: &'static str,
        want_s: &'static str,
    }
    let spec = |details: &str, state: &str| CardSpec {
        details: details.to_string(),
        state: state.to_string(),
    };
    let cases = vec![
        Case {
            name: "movie tag absent hides",
            raw: m_raw,
            act: run.clone(),
            res: bare_movie.clone(),
            spec: spec(CARD_DETAILS_DEFAULT, CARD_STATE_TAG),
            want_d: "Dune",
            want_s: "",
        },
        Case {
            name: "movie synopsis absent hides",
            raw: m_raw,
            act: run.clone(),
            res: bare_movie.clone(),
            spec: spec(CARD_DETAILS_DEFAULT, CARD_STATE_SYNOPSIS),
            want_d: "Dune",
            want_s: "",
        },
        Case {
            name: "movie none hides",
            raw: m_raw,
            act: run.clone(),
            res: bare_movie.clone(),
            spec: spec(CARD_DETAILS_DEFAULT, CARD_STATE_NONE),
            want_d: "Dune",
            want_s: "",
        },
        Case {
            name: "series preset falls back to base",
            raw: s_raw,
            act: run.clone(),
            res: Resolved::default(),
            spec: spec(CARD_DETAILS_SERIES, CARD_STATE_DEFAULT),
            want_d: "Pilot",
            want_s: "S1 E2",
        },
        Case {
            name: "synopsis absent hides",
            raw: s_raw,
            act: run.clone(),
            res: Resolved::default(),
            spec: spec(CARD_DETAILS_DEFAULT, CARD_STATE_SYNOPSIS),
            want_d: "Pilot",
            want_s: "",
        },
        Case {
            name: "none hides with content",
            raw: s_raw,
            act: run.clone(),
            res: Resolved {
                series: "S".to_string(),
                episode_name: "E".to_string(),
                synopsis: "Syn".to_string(),
                ..Resolved::default()
            },
            spec: spec(CARD_DETAILS_SERIES, CARD_STATE_NONE),
            want_d: "S",
            want_s: "",
        },
        Case {
            name: "paused tag absent reads Paused",
            raw: m_raw,
            act: paused.clone(),
            res: bare_movie.clone(),
            spec: spec(CARD_DETAILS_DEFAULT, CARD_STATE_TAG),
            want_d: "Dune",
            want_s: "Paused",
        },
        Case {
            name: "paused synopsis absent reads Paused",
            raw: m_raw,
            act: paused.clone(),
            res: bare_movie.clone(),
            spec: spec(CARD_DETAILS_DEFAULT, CARD_STATE_SYNOPSIS),
            want_d: "Dune",
            want_s: "Paused",
        },
        Case {
            name: "paused none still hides",
            raw: m_raw,
            act: paused,
            res: bare_movie,
            spec: spec(CARD_DETAILS_DEFAULT, CARD_STATE_NONE),
            want_d: "Dune",
            want_s: "",
        },
    ];
    for c in &cases {
        let (d, s) = c.spec.render(c.raw, Some(&c.act), &c.res);
        assert!(
            d == c.want_d && s == c.want_s,
            "{}: render = ({d:?}, {s:?}), want ({:?}, {:?})",
            c.name,
            c.want_d,
            c.want_s
        );
    }
    // Details never blanks: every preset over every corpus input yields a
    // non-empty details line (fallbacks keep the base line).
    let (dets, states) = card_presets();
    for c in card_spec_cases() {
        for d in &dets {
            for st in &states {
                let (gd, _) = CardSpec {
                    details: d.id.clone(),
                    state: st.id.clone(),
                }
                .render(&c.raw, c.act.as_ref(), &c.res);
                assert!(
                    !gd.is_empty(),
                    "{} {}/{}: details blank, want fallback to base",
                    c.name,
                    d.id,
                    st.id
                );
            }
        }
    }
}

/// Proves the idle card keeps its shipped lines under any preset.
#[test]
fn card_spec_idle_stable() {
    let (dets, states) = card_presets();
    let no_action = Action::default();
    let no_res = Resolved::default();
    for raw in ["", "PotPlayer", "PotPlayer - PotPlayer"] {
        let base = build_activity_resolved(raw, Some(&no_action), &no_res);
        for d in &dets {
            for st in &states {
                let (gd, gs) = CardSpec {
                    details: d.id.clone(),
                    state: st.id.clone(),
                }
                .render(raw, Some(&no_action), &no_res);
                assert!(
                    gd == base.details && gs == base.state,
                    "idle {raw:?} {}/{} = ({gd:?}, {gs:?}), want ({:?}, {:?})",
                    d.id,
                    st.id,
                    base.details,
                    base.state
                );
            }
        }
    }
}

/// The preset type is part of the public surface the Card view iterates.
#[test]
fn card_preset_type_is_public() {
    let p = CardPreset {
        id: "default".to_string(),
        label: "Default".to_string(),
    };
    assert_eq!(p, CardPreset::clone(&p));
}

/// The stored JSON contract the app crate embeds as `Card presence.CardSpec`
/// (`go\config.go:22-30`): Go's `json:"details,omitempty"` /
/// `json:"state,omitempty"`. An empty line is omitted, an absent line loads as
/// empty (and normalizes to the default on use).
#[test]
fn card_spec_serde_matches_go_omitempty() {
    assert_eq!(
        serde_json::to_string(&CardSpec::default()).unwrap(),
        "{}",
        "a zero CardSpec writes no keys (Go omitempty)"
    );
    assert_eq!(
        serde_json::to_string(&default_card_spec()).unwrap(),
        "{\"details\":\"default\",\"state\":\"default\"}",
        "both set lines write both keys"
    );
    assert_eq!(
        serde_json::to_string(&CardSpec {
            details: CARD_DETAILS_TITLE.to_string(),
            ..CardSpec::default()
        })
        .unwrap(),
        "{\"details\":\"title\"}",
        "an empty state line is omitted"
    );
    let parsed: CardSpec = serde_json::from_str("{\"details\":\"tag\"}").unwrap();
    assert_eq!(parsed.details, "tag");
    assert_eq!(parsed.state, "", "an absent line loads empty");
    let parsed: CardSpec = serde_json::from_str("{}").unwrap();
    assert_eq!(parsed, CardSpec::default(), "an empty object loads zero");
    assert!(parsed.is_default(), "and normalizes to the default");
}
