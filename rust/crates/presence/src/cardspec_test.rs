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
use crate::{Action, PAUSED_MARKER, Resolved, build_activity_resolved, compose_card};

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
    // Movie with canonical override: details is "Title (Year)", state is
    // genres/length joined (the synopsis lives in the tooltip now).
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
        d == "Dune: Part Two (2024)" && s == "Sci-Fi \u{b7} 2h 46m",
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
            want_s: mid("Sci-Fi", "2h 46m"),
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
            want_s: mid("Sci-Fi", "2h 46m"),
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
            want_s: mid("Sci-Fi", "2h 46m"),
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
            // No filename tag, so the tag preset falls down the ladder to
            // genres/length.
            want_s: mid("Sci-Fi", "2h 46m"),
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
            want_s: mid("Sci-Fi", "2h 46m") + MID + "Paul unites the Fremen",
        },
        Case {
            name: "series default",
            raw: s_raw,
            res: series.clone(),
            spec: default_card_spec(),
            want_d: "Canonical Episode",
            // The series default carries the synopsis teaser after the tag.
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
            want_s: mid("S1 E2", "A brave synopsis"),
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
            want_s: mid("S1 E1", "An elf mourns"),
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

/// Pins the fill-ladder contract: no preset but `none` blanks a slot (the
/// details line never blanks: per-kind fallbacks keep the base line, and the
/// state ladder ends on the clean filename title). `none` always hides, even
/// with content and even when paused. The paused marker path is preserved:
/// paused-with-content keeps the suffix, paused with nothing still reads
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
    let m_raw = "Movie.mkv - PotPlayer";
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
            name: "movie tag falls to clean title",
            raw: m_raw,
            act: run.clone(),
            res: bare_movie.clone(),
            spec: spec(CARD_DETAILS_DEFAULT, CARD_STATE_TAG),
            want_d: "Dune",
            want_s: "Movie",
        },
        Case {
            name: "movie synopsis falls to clean title",
            raw: m_raw,
            act: run.clone(),
            res: bare_movie.clone(),
            spec: spec(CARD_DETAILS_DEFAULT, CARD_STATE_SYNOPSIS),
            want_d: "Dune",
            want_s: "Movie",
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
            name: "series tag falls to clean title with no tag",
            raw: m_raw,
            act: run.clone(),
            res: Resolved::default(),
            spec: spec(CARD_DETAILS_DEFAULT, CARD_STATE_TAG),
            want_d: "Movie",
            want_s: "Movie",
        },
        Case {
            name: "synopsis falls to clean title with no synopsis",
            raw: m_raw,
            act: run.clone(),
            res: Resolved::default(),
            spec: spec(CARD_DETAILS_DEFAULT, CARD_STATE_SYNOPSIS),
            want_d: "Movie",
            want_s: "Movie",
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
            name: "paused tag with no tag reads clean title paused",
            raw: m_raw,
            act: paused.clone(),
            res: bare_movie.clone(),
            spec: spec(CARD_DETAILS_DEFAULT, CARD_STATE_TAG),
            want_d: "Dune",
            want_s: "Movie - Paused",
        },
        Case {
            name: "paused synopsis with none reads clean title paused",
            raw: m_raw,
            act: paused.clone(),
            res: bare_movie.clone(),
            spec: spec(CARD_DETAILS_DEFAULT, CARD_STATE_SYNOPSIS),
            want_d: "Dune",
            want_s: "Movie - Paused",
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

/// S3 (revised): the movie default still never duplicates the tooltip
/// synopsis, while the series default now carries the synopsis teaser right
/// after the episode tag (the owner asked for the synopsis on the card, right
/// after the `S1E1`); the hover tooltip keeps the whole note. Drives the real
/// builder for a movie and a series, and the default spec must agree
/// byte-for-byte with the builder on both.
#[test]
fn default_state_carries_no_synopsis() {
    let run = Action {
        running: true,
        has_start: true,
        start: 12345,
        ct: 1000,
        ..Action::default()
    };
    let movie = Resolved {
        series: "Dune: Part Two".to_string(),
        year: 2024,
        is_movie: true,
        genres: "Sci-Fi".to_string(),
        length: "2h 46m".to_string(),
        synopsis: "Paul unites the Fremen".to_string(),
        ..Resolved::default()
    };
    let got = build_activity_resolved(
        "Dune.Part.Two.2024.1080p.mkv - PotPlayer",
        Some(&run),
        &movie,
    );
    assert_eq!(got.state, "Sci-Fi \u{b7} 2h 46m", "movie state");
    assert!(
        !got.state.contains("Paul unites the Fremen"),
        "movie state duplicates the synopsis: {:?}",
        got.state
    );
    assert_eq!(
        got.large_text, "Paul unites the Fremen",
        "movie tooltip must still carry the synopsis"
    );
    let (d, s) = default_card_spec().render(
        "Dune.Part.Two.2024.1080p.mkv - PotPlayer",
        Some(&run),
        &movie,
    );
    assert_eq!(
        (d.as_str(), s.as_str()),
        (got.details.as_str(), got.state.as_str()),
        "default spec must agree with the builder"
    );
    let series = Resolved {
        series: "Canonical Series".to_string(),
        episode_name: "Canonical Episode".to_string(),
        synopsis: "A brave synopsis".to_string(),
        ..Resolved::default()
    };
    let got = build_activity_resolved(
        "Show.S01E02.Pilot.1080p.mkv - PotPlayer",
        Some(&run),
        &series,
    );
    assert_eq!(got.state, "S1 E2 \u{b7} A brave synopsis", "series state");
    assert!(
        got.state.contains("A brave synopsis"),
        "series state must carry the teaser after the tag: {:?}",
        got.state
    );
    assert_eq!(
        got.large_text, "A brave synopsis",
        "series tooltip must still carry the synopsis"
    );
    let (d, s) = default_card_spec().render(
        "Show.S01E02.Pilot.1080p.mkv - PotPlayer",
        Some(&run),
        &series,
    );
    assert_eq!(
        (d.as_str(), s.as_str()),
        (got.details.as_str(), got.state.as_str()),
        "default spec must agree with the builder"
    );
}

/// S3: the opt-in SYNOPSIS preset keeps the old synopsis-bearing line shapes --
/// clause cuts end on the word with the ellipsis terminator, long lines end at
/// a word boundary, all within the 128-unit ceiling.
#[test]
fn synopsis_preset_keeps_old_line_shapes() {
    let run = Action {
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
    // Comma cut (the live `...Majin Buu,` shape).
    let syn = "Years after the battle with Earth's greatest enemy, Majin Buu, the world faces a new threat from beyond the stars and more";
    let (_, s) = syn_spec.render(
        "Dragon.Ball.Movie.1990.1080p.mkv - PotPlayer",
        Some(&run),
        &Resolved {
            series: "Dragon Ball".to_string(),
            year: 1990,
            is_movie: true,
            synopsis: syn.to_string(),
            ..Resolved::default()
        },
    );
    assert_eq!(
        s,
        "Years after the battle with Earth's greatest enemy, Majin Buu\u{2026}",
        "comma-cut preset state"
    );
    // Semicolon cut (the live `...meet Elliot;` shape).
    let syn = "Allsafe is controlled; the Dark Army is ready to meet Elliot; the fsociety plan falls apart at midnight somehow";
    let (_, s) = syn_spec.render(
        "Show.S02E04.Episode.1080p.mkv - PotPlayer",
        Some(&run),
        &Resolved {
            series: "Show".to_string(),
            synopsis: syn.to_string(),
            ..Resolved::default()
        },
    );
    assert_eq!(
        s,
        "S2 E4 \u{b7} Allsafe is controlled; the Dark Army is ready to meet Elliot\u{2026}",
        "semicolon-cut preset state"
    );
    // Long movie line ends on a whole word with the terminator.
    let (_, s) = syn_spec.render(
        "Dune.Part.Two.2024.1080p.mkv - PotPlayer",
        Some(&run),
        &Resolved {
            series: "Dune".to_string(),
            is_movie: true,
            genres: "Adventure".to_string(),
            length: "2h 46m".to_string(),
            synopsis: format!("{}finale", "epic ".repeat(40)),
            ..Resolved::default()
        },
    );
    assert!(
        s.ends_with('\u{2026}'),
        "long preset state = {s:?}, want trailing ellipsis"
    );
    let n = s.encode_utf16().count();
    assert!(
        n <= 128,
        "long preset state length {n} exceeds 128: {s:?}"
    );
    let body = s.trim_end_matches('\u{2026}');
    assert!(
        body.ends_with("epic") || body.ends_with("finale"),
        "long preset state ends mid-word: {s:?}"
    );
}

/// The slot-completeness gate: over {series, anime, movie} x {full with
/// synopsis, resolved without, partial (no genres/length), unresolved,
/// nospace (300-char single token), cjk (CJK run), sentence (`x`x120 + ". " +
/// `y`x20)} x {poster, no poster} x
/// {playing, paused} x {each of the 4 details presets} x {each of the 4 state
/// presets}, every slot of the composed card is non-empty for every preset
/// except `none` (whose state hides), every line stays within its budget (128;
/// 72 for a synopsis-bearing state), and a truncated line is always signalled:
/// either a single trailing U+2026 whose body ends on a word character
/// (pinned for the no-space and CJK cuts too) or a complete sentence keeping
/// its own `.`/`!`/`?` terminator with no ellipsis. A line is never silently
/// truncated.
#[test]
fn slot_completeness_gate() {
    const POSTER: &str = "https://example.com/poster.jpg";
    const LONG_SYN: &str = "epic ";
    let long_syn = format!("{}finale", LONG_SYN.repeat(40));
    let short_syn = "A brave teaser unfolds".to_string();
    // No-space fixtures: a 300-character single token and a run of CJK text.
    // Both exceed every budget, carry no sentence terminator, and contain no
    // word boundary, so they pin the hard-cut tail: the cut must still end
    // with a single U+2026 within budget.
    let nospace_syn = "x".repeat(300);
    let cjk_syn = "\u{6f22}".repeat(300);
    // Sentence-end fixture: the word-boundary cut at the 128-unit tooltip
    // budget lands on `.` followed by a space, so the tooltip keeps the
    // complete sentence with its own terminator and no ellipsis.
    let sentence_syn = format!("{}. {}", "x".repeat(120), "y".repeat(20));
    struct Kind {
        label: &'static str,
        raw: &'static str,
        is_movie: bool,
    }
    let kinds = [
        Kind {
            label: "series",
            raw: "Show.S01E02.Pilot.1080p.mkv - PotPlayer",
            is_movie: false,
        },
        Kind {
            label: "anime",
            raw: "Frieren.S01E01.The.Heros.Rest.1080p.mkv - PotPlayer",
            is_movie: false,
        },
        Kind {
            label: "movie",
            raw: "Dune.Part.Two.2024.1080p.mkv - PotPlayer",
            is_movie: true,
        },
    ];
    let variants_for = |kind: &Kind| -> Vec<(&'static str, Resolved)> {
        let full = match kind.label {
            "movie" => Resolved {
                series: "Dune: Part Two".to_string(),
                year: 2024,
                is_movie: true,
                genres: "Sci-Fi".to_string(),
                length: "2h 46m".to_string(),
                synopsis: long_syn.clone(),
                ..Resolved::default()
            },
            "anime" => Resolved {
                series: "Frieren".to_string(),
                episode_name: "The Hero's Rest".to_string(),
                synopsis: long_syn.clone(),
                year: 2023,
                ..Resolved::default()
            },
            _ => Resolved {
                series: "Canonical Series".to_string(),
                episode_name: "Canonical Episode".to_string(),
                synopsis: long_syn.clone(),
                ..Resolved::default()
            },
        };
        let nosyn = match kind.label {
            "movie" => Resolved {
                series: "Dune: Part Two".to_string(),
                year: 2024,
                is_movie: true,
                genres: "Sci-Fi".to_string(),
                length: "2h 46m".to_string(),
                ..Resolved::default()
            },
            "anime" => Resolved {
                series: "Frieren".to_string(),
                episode_name: "The Hero's Rest".to_string(),
                year: 2023,
                ..Resolved::default()
            },
            _ => Resolved {
                series: "Canonical Series".to_string(),
                episode_name: "Canonical Episode".to_string(),
                ..Resolved::default()
            },
        };
        let partial = match kind.label {
            "movie" => Resolved {
                series: "Dune: Part Two".to_string(),
                year: 2024,
                is_movie: true,
                synopsis: short_syn.clone(),
                ..Resolved::default()
            },
            "anime" => Resolved {
                series: "Frieren".to_string(),
                synopsis: short_syn.clone(),
                year: 2023,
                ..Resolved::default()
            },
            _ => Resolved {
                series: "Canonical Series".to_string(),
                synopsis: short_syn.clone(),
                ..Resolved::default()
            },
        };
        let nospace = match kind.label {
            "movie" => Resolved {
                series: "Dune: Part Two".to_string(),
                year: 2024,
                is_movie: true,
                genres: "Sci-Fi".to_string(),
                length: "2h 46m".to_string(),
                synopsis: nospace_syn.clone(),
                ..Resolved::default()
            },
            "anime" => Resolved {
                series: "Frieren".to_string(),
                episode_name: "The Hero's Rest".to_string(),
                synopsis: nospace_syn.clone(),
                year: 2023,
                ..Resolved::default()
            },
            _ => Resolved {
                series: "Canonical Series".to_string(),
                episode_name: "Canonical Episode".to_string(),
                synopsis: nospace_syn.clone(),
                ..Resolved::default()
            },
        };
        let cjk = match kind.label {
            "movie" => Resolved {
                series: "Dune: Part Two".to_string(),
                year: 2024,
                is_movie: true,
                genres: "Sci-Fi".to_string(),
                length: "2h 46m".to_string(),
                synopsis: cjk_syn.clone(),
                ..Resolved::default()
            },
            "anime" => Resolved {
                series: "Frieren".to_string(),
                episode_name: "The Hero's Rest".to_string(),
                synopsis: cjk_syn.clone(),
                year: 2023,
                ..Resolved::default()
            },
            _ => Resolved {
                series: "Canonical Series".to_string(),
                episode_name: "Canonical Episode".to_string(),
                synopsis: cjk_syn.clone(),
                ..Resolved::default()
            },
        };
        let sentence = match kind.label {
            "movie" => Resolved {
                series: "Dune: Part Two".to_string(),
                year: 2024,
                is_movie: true,
                genres: "Sci-Fi".to_string(),
                length: "2h 46m".to_string(),
                synopsis: sentence_syn.clone(),
                ..Resolved::default()
            },
            "anime" => Resolved {
                series: "Frieren".to_string(),
                episode_name: "The Hero's Rest".to_string(),
                synopsis: sentence_syn.clone(),
                year: 2023,
                ..Resolved::default()
            },
            _ => Resolved {
                series: "Canonical Series".to_string(),
                episode_name: "Canonical Episode".to_string(),
                synopsis: sentence_syn.clone(),
                ..Resolved::default()
            },
        };
        vec![
            ("full", full),
            ("nosyn", nosyn),
            ("partial", partial),
            ("unresolved", Resolved::default()),
            ("nospace", nospace),
            ("cjk", cjk),
            ("sentence", sentence),
        ]
    };
    let state_presets = [
        CARD_STATE_DEFAULT,
        CARD_STATE_TAG,
        CARD_STATE_SYNOPSIS,
        CARD_STATE_NONE,
    ];
    let details_presets = [
        CARD_DETAILS_DEFAULT,
        CARD_DETAILS_TITLE,
        CARD_DETAILS_EPISODE,
        CARD_DETAILS_SERIES,
    ];
    for kind in &kinds {
        for (vlabel, base) in variants_for(kind) {
            for poster in [true, false] {
                let mut res = base.clone();
                res.artwork = if poster {
                    POSTER.to_string()
                } else {
                    String::new()
                };
                for playing in [true, false] {
                    let act = if playing {
                        Action {
                            running: true,
                            has_start: true,
                            start: 1,
                            ct: 2,
                            ..Action::default()
                        }
                    } else {
                        Action {
                            paused: true,
                            ct: 1000,
                            ..Action::default()
                        }
                    };
                    for dpreset in details_presets {
                        for spreset in state_presets {
                            let spec = CardSpec {
                                details: dpreset.to_string(),
                                state: spreset.to_string(),
                            };
                            let card = compose_card(
                                kind.raw,
                                Some(&act),
                                &res,
                                kind.is_movie,
                                &spec,
                            );
                            let ctx = format!(
                                "{} {vlabel} poster={poster} {} details={dpreset} state={spreset}",
                                kind.label,
                                if playing { "playing" } else { "paused" }
                            );
                            assert!(
                                !card.name.is_empty(),
                                "{ctx}: name blank"
                            );
                            assert!(
                                !card.details.is_empty(),
                                "{ctx}: details blank"
                            );
                            assert!(
                                !card.large_image.is_empty(),
                                "{ctx}: large_image blank"
                            );
                            assert!(
                                !card.large_text.is_empty(),
                                "{ctx}: large_text blank"
                            );
                            if poster {
                                assert_eq!(
                                    card.large_image, POSTER,
                                    "{ctx}: large_image, want poster"
                                );
                            } else {
                                assert_eq!(
                                    card.large_image, "logo",
                                    "{ctx}: large_image, want logo"
                                );
                            }
                            if spreset == CARD_STATE_NONE {
                                assert!(
                                    card.state.is_empty(),
                                    "{ctx}: none state = {:?}, want empty",
                                    card.state
                                );
                            } else {
                                assert!(
                                    !card.state.is_empty(),
                                    "{ctx}: state blank"
                                );
                                if !playing {
                                    assert!(
                                        card.state.ends_with(PAUSED_MARKER)
                                            || card.state == "Paused",
                                        "{ctx}: paused state = {:?}, want the marker",
                                        card.state
                                    );
                                }
                            }
                            for (slot, line, budget) in [
                                ("name", card.name.as_str(), 128usize),
                                ("details", card.details.as_str(), 128usize),
                                ("large_text", card.large_text.as_str(), 128usize),
                            ] {
                                let n = line.encode_utf16().count();
                                assert!(
                                    n <= budget,
                                    "{ctx}: {slot} length {n} exceeds {budget}: {line:?}"
                                );
                                assert_gate_cut(slot, line, budget, &ctx);
                            }
                            let state_budget =
                                if state_carries_synopsis(&card.state, &res.synopsis) {
                                    72usize
                                } else {
                                    128usize
                                };
                            let n = card.state.encode_utf16().count();
                            assert!(
                                n <= state_budget,
                                "{ctx}: state length {n} exceeds {state_budget}: {:?}",
                                card.state
                            );
                            assert_gate_cut("state", &card.state, state_budget, &ctx);
                            // No-space pins: the 300-char token and the CJK run
                            // carry no terminator and no word boundary, so the
                            // tooltip must end with the signalled ellipsis, and
                            // a synopsis-bearing state must carry it too (before
                            // the paused marker when paused).
                            if (vlabel == "nospace" || vlabel == "cjk")
                                && spreset != CARD_STATE_NONE
                            {
                                assert!(
                                    card.large_text.ends_with('\u{2026}'),
                                    "{ctx}: large_text = {:?}, want trailing ellipsis",
                                    card.large_text
                                );
                                if state_carries_synopsis(&card.state, &res.synopsis) {
                                    if playing {
                                        assert!(
                                            card.state.ends_with('\u{2026}'),
                                            "{ctx}: state = {:?}, want trailing ellipsis",
                                            card.state
                                        );
                                    } else {
                                        assert!(
                                            card.state.contains('\u{2026}'),
                                            "{ctx}: state = {:?}, want the ellipsis before the marker",
                                            card.state
                                        );
                                    }
                                }
                            }
                            // Sentence-end pin: the tooltip cut lands on `.`
                            // followed by a space, so it keeps the complete
                            // sentence with its own terminator and no ellipsis.
                            if vlabel == "sentence" && spreset != CARD_STATE_NONE {
                                let want = "x".repeat(120) + ".";
                                assert_eq!(
                                    card.large_text, want,
                                    "{ctx}: large_text, want the complete sentence"
                                );
                                assert!(
                                    !card.large_text.contains('\u{2026}'),
                                    "{ctx}: large_text = {:?}, want no ellipsis",
                                    card.large_text
                                );
                            }
                            // Truncation-signal pins against the synopsis source:
                            // a tooltip shorter than its synopsis, and a
                            // synopsis-bearing state shorter than its synopsis,
                            // must end signalled (ellipsis, possibly before the
                            // paused marker, or a sentence terminator with no
                            // ellipsis).
                            if !res.synopsis.is_empty() {
                                assert_gate_signal(
                                    "large_text",
                                    &card.large_text,
                                    &res.synopsis,
                                    &ctx,
                                );
                                if spreset != CARD_STATE_NONE
                                    && state_carries_synopsis(&card.state, &res.synopsis)
                                {
                                    assert_gate_signal(
                                        "state",
                                        &card.state,
                                        &res.synopsis,
                                        &ctx,
                                    );
                                }
                            }
                        }
                    }
                }
            }
        }
    }
}

/// Whether `state` carries the synopsis: the first distinctive synopsis word
/// (longer than 3 chars, so "A"/"the" cannot false-positive) appears in it. A
/// truncated single-token or CJK run carries only a prefix of its long word,
/// so a missing whole word falls back to an 8-char prefix check.
fn state_carries_synopsis(state: &str, synopsis: &str) -> bool {
    let Some(word) = synopsis.split_whitespace().find(|w| w.len() > 3) else {
        let flat: String = synopsis.split_whitespace().collect();
        if flat.is_empty() {
            return false;
        }
        let prefix: String = flat.chars().take(8).collect();
        return !prefix.is_empty() && state.contains(&prefix);
    };
    if state.contains(word) {
        return true;
    }
    let prefix: String = word.chars().take(8).collect();
    !prefix.is_empty() && state.contains(&prefix)
}

/// A gate line is never silently truncated: never three dots, never more than
/// one ellipsis, and an ellipsis-terminated cut ends on a word character
/// (never `,` `;` `:` `-` `–` `—` `·` or blank). A complete sentence keeping
/// its own `.`/`!`/`?` with no ellipsis is the other valid signal; anything
/// else must fit below its budget (at cap without a signal is a silent cut).
fn assert_gate_cut(slot: &str, line: &str, budget: usize, ctx: &str) {
    assert!(
        !line.contains("..."),
        "{ctx}: {slot} never three dots: {line:?}"
    );
    let ell = line.chars().filter(|&c| c == '\u{2026}').count();
    assert!(
        ell <= 1,
        "{ctx}: {slot} multiple ellipsis: {line:?}"
    );
    if line.ends_with('\u{2026}') {
        let body = line.trim_end_matches('\u{2026}');
        assert!(
            !body.is_empty(),
            "{ctx}: {slot} empty cut body: {line:?}"
        );
        let last = body.chars().next_back().unwrap();
        assert!(
            last.is_alphanumeric(),
            "{ctx}: {slot} cut ends on {last:?}, want a word character: {line:?}"
        );
        return;
    }
    if line.contains('\u{2026}') {
        // Paused-truncated shape (`xxx… - Paused`): the single ellipsis sits
        // before the marker. The cut body must still end on a word character.
        let idx = line.find('\u{2026}').unwrap();
        let body = &line[..idx];
        assert!(
            !body.is_empty(),
            "{ctx}: {slot} empty cut body: {line:?}"
        );
        let last = body.chars().next_back().unwrap();
        assert!(
            last.is_alphanumeric(),
            "{ctx}: {slot} cut ends on {last:?}, want a word character: {line:?}"
        );
        return;
    }
    if (line.ends_with('.') || line.ends_with('!') || line.ends_with('?'))
        && !line.contains('\u{2026}')
    {
        return;
    }
    let n = line.encode_utf16().count();
    assert!(
        line.is_empty() || n < budget,
        "{ctx}: {slot} at cap without a signal (silent truncation): {line:?}"
    );
}

/// A line shorter than its synopsis source must end signalled: either with a
/// single U+2026 (trailing, or before the paused marker) whose body ends on a
/// word character, or with a sentence terminator `.`/`!`/`?` and no ellipsis.
/// A line that already carries its source whole needs no signal.
fn assert_gate_signal(slot: &str, line: &str, synopsis: &str, ctx: &str) {
    let full: String = synopsis.split_whitespace().collect::<Vec<_>>().join(" ");
    if line == full || full.is_empty() {
        return;
    }
    let line_units = line.encode_utf16().count();
    let full_units = full.encode_utf16().count();
    if line_units >= full_units {
        return;
    }
    if line.ends_with('\u{2026}') {
        let body = line.trim_end_matches('\u{2026}');
        assert!(
            !body.is_empty(),
            "{ctx}: {slot} empty cut body: {line:?}"
        );
        let last = body.chars().next_back().unwrap();
        assert!(
            last.is_alphanumeric(),
            "{ctx}: {slot} cut ends on {last:?}, want a word character: {line:?}"
        );
        return;
    }
    if line.contains('\u{2026}') {
        return;
    }
    assert!(
        (line.ends_with('.') || line.ends_with('!') || line.ends_with('?'))
            && !line.contains('\u{2026}'),
        "{ctx}: {slot} shorter than its source without a signal: {line:?}"
    );
}
