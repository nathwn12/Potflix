//! `discord::activity_test` -- mirrors `go\internal\discord\activity_test.go`:
//! the exact SET_ACTIVITY payload shape (entity fields, no timestamps when there
//! is no timer, a null-activity clear). Assertions check the key set explicitly,
//! so a stray field fails.

use serde_json::{Map, Value};

use crate::activity::{new_activity, Activity, ActivityButton};
use crate::pipe_test::connect_pair;
use crate::{wire_json, Config, OP_FRAME};

fn decode_frame(payload: &[u8]) -> Map<String, Value> {
    serde_json::from_slice(payload)
        .unwrap_or_else(|e| panic!("unmarshal {}: {e}", String::from_utf8_lossy(payload)))
}

fn want_keys(where_: &str, m: &Map<String, Value>, keys: &[&str]) {
    if m.len() != keys.len() {
        panic!("{where_} keys = {:?}, want exactly {keys:?}", keys_of(m));
    }
    for k in keys {
        if !m.contains_key(*k) {
            panic!("{where_} missing key {k:?} (has {:?})", keys_of(m));
        }
    }
}

fn keys_of(m: &Map<String, Value>) -> Vec<String> {
    m.keys().cloned().collect()
}

fn as_map<'a>(v: &'a Value, where_: &str) -> &'a Map<String, Value> {
    v.as_object()
        .unwrap_or_else(|| panic!("{where_} = {v}, want object"))
}

/// `args.activity` of a decoded command frame.
fn activity_of(top: &Map<String, Value>) -> &Map<String, Value> {
    let args = as_map(&top["args"], "args");
    as_map(&args["activity"], "activity")
}

#[test]
fn test_set_activity_payload() {
    let (c, sc, _) = connect_pair(0, Config::default());

    // SmallImage is caller-compat only: even when set it must not reach the
    // wire - the pristine card carries no status icon.
    let act = Activity {
        details: "Show".to_string(),
        state: r#"S1 E2 "Pilot""#.to_string(),
        small_image: "play".to_string(),
        has_start: true,
        start: 12345,
        ..Activity::default()
    };
    c.set_activity(act)
        .unwrap_or_else(|e| panic!("SetActivity: {e}"));
    let (op, payload) = sc
        .read_frame()
        .unwrap_or_else(|e| panic!("read activity frame: {e}"));
    assert_eq!(op, OP_FRAME, "activity opcode");

    let top = decode_frame(&payload);
    want_keys("command", &top, &["cmd", "args", "nonce"]);
    assert_eq!(top["cmd"].as_str(), Some("SET_ACTIVITY"), "cmd");
    assert!(
        !top["nonce"].as_str().unwrap_or("").is_empty(),
        "nonce missing in {}",
        String::from_utf8_lossy(&payload)
    );

    let args = as_map(&top["args"], "args");
    want_keys("args", args, &["pid", "activity"]);
    assert_eq!(
        args["pid"].as_i64(),
        Some(i64::from(std::process::id())),
        "pid"
    );

    let a = activity_of(&top);
    want_keys(
        "activity",
        a,
        &["type", "details", "state", "timestamps", "assets"],
    );
    assert_eq!(a["type"].as_i64(), Some(3), "type (WATCHING)");
    assert_eq!(a["details"].as_str(), Some("Show"), "details");
    assert_eq!(a["state"].as_str(), Some(r#"S1 E2 "Pilot""#), "state");

    let ts = as_map(&a["timestamps"], "timestamps");
    want_keys("timestamps", ts, &["start"]);
    assert_eq!(ts["start"].as_i64(), Some(12345), "timestamps.start");

    // Empty text is OMITTED, not sent as "" (Discord rejects an empty large_text
    // with code 4000); the large image always lands. The pristine card has no
    // small icon: small_image/small_text keys must be absent even though the
    // caller set SmallImage.
    let assets = as_map(&a["assets"], "assets");
    want_keys("assets", assets, &["large_image"]);
    assert_eq!(assets["large_image"].as_str(), Some("logo"), "large_image");
    assert!(
        !assets.contains_key("large_text"),
        "large_text present while empty, want omitted: {assets:?}"
    );
    assert!(
        !assets.contains_key("small_image"),
        "small_image present, want omitted (pristine card): {assets:?}"
    );
    assert!(
        !assets.contains_key("small_text"),
        "small_text present, want omitted (pristine card): {assets:?}"
    );
    sc.close();
}

#[test]
fn test_set_activity_text_present_when_non_empty() {
    // The wire object omits empty large text but must still carry non-empty
    // large text when a caller sets it. Small text is never carried, even when
    // set: the pristine card has no status icon slot.
    let mut p = new_activity(&Activity {
        details: "Show".to_string(),
        state: "S1 E2".to_string(),
        small_image: "play".to_string(),
        ..Activity::default()
    });
    p.assets.large_text = "Potflix".to_string();
    let b = serde_json::to_vec(&p).unwrap_or_else(|e| panic!("marshal: {e}"));

    let a = decode_frame(&b);
    let assets = as_map(&a["assets"], "assets");
    want_keys("assets", assets, &["large_image", "large_text"]);
    assert_eq!(assets["large_text"].as_str(), Some("Potflix"), "large_text");
}

#[test]
fn test_large_image_https_honored_verbatim() {
    // An https LargeImage is honored verbatim in the wire JSON. The caller also
    // sets SmallImage, which must not appear: no icon keys on the wire.
    const ART: &str = "https://static.tvmaze.com/uploads/images/medium_portrait/1/2.jpg";
    let got = wire_json(&Activity {
        details: "Show".to_string(),
        state: "S1 E2".to_string(),
        large_image: ART.to_string(),
        small_image: "play".to_string(),
        ..Activity::default()
    });
    let want = format!(
        r#"{{"type":3,"details":"Show","state":"S1 E2","assets":{{"large_image":"{ART}"}}}}"#
    );
    assert_eq!(got, want, "WireJSON");
}

#[test]
fn test_large_image_fallback_to_logo() {
    // Empty or non-http(s) LargeImage falls back to "logo", never empty.
    for img in [
        "",
        "logo",
        "not-a-url",
        "ftp://example.com/a.jpg",
        "data:image/png;base64,xx",
    ] {
        let got = wire_json(&Activity {
            details: "Show".to_string(),
            small_image: "play".to_string(),
            large_image: img.to_string(),
            ..Activity::default()
        });
        let a = decode_frame(got.as_bytes());
        let assets = as_map(&a["assets"], "assets");
        assert_eq!(
            assets["large_image"].as_str(),
            Some("logo"),
            "LargeImage {img:?}"
        );
    }
    // http (non-s) is also honored verbatim.
    const HTTP_ART: &str = "http://example.com/cover.jpg";
    let got = wire_json(&Activity {
        details: "Show".to_string(),
        small_image: "play".to_string(),
        large_image: HTTP_ART.to_string(),
        ..Activity::default()
    });
    let a = decode_frame(got.as_bytes());
    assert_eq!(
        as_map(&a["assets"], "assets")["large_image"].as_str(),
        Some(HTTP_ART),
        "http LargeImage not honored: {got}"
    );
}

#[test]
fn test_set_activity_no_timer() {
    // The idle card: details Browsing / state Idle and no timestamps object at
    // all. No small icon either: the pristine idle card is icon-free.
    let (c, sc, _) = connect_pair(0, Config::default());

    c.set_activity(Activity {
        details: "Browsing".to_string(),
        state: "Idle".to_string(),
        small_image: "stop".to_string(),
        ..Activity::default()
    })
    .unwrap_or_else(|e| panic!("SetActivity: {e}"));
    let (_, payload) = sc
        .read_frame()
        .unwrap_or_else(|e| panic!("read activity frame: {e}"));

    let top = decode_frame(&payload);
    let a = activity_of(&top);
    want_keys("activity", a, &["type", "details", "state", "assets"]);
    assert_eq!(a["details"].as_str(), Some("Browsing"), "details");
    assert_eq!(a["state"].as_str(), Some("Idle"), "state");
    let assets = as_map(&a["assets"], "assets");
    want_keys("assets", assets, &["large_image"]);
    assert!(
        !assets.contains_key("small_image"),
        "idle small_image present, want omitted (pristine card): {assets:?}"
    );
    assert!(
        !assets.contains_key("large_text"),
        "large_text present while empty, want omitted: {assets:?}"
    );
    assert!(
        !assets.contains_key("small_text"),
        "small_text present while empty, want omitted: {assets:?}"
    );
    sc.close();
}

#[test]
fn test_set_activity_playing_carries_start_and_end() {
    // While playing the payload carries BOTH timestamps.start and
    // timestamps.end (the remaining-time math is computed by the send gate:
    // end = now + (total - pos)), so Discord renders the time bar against the
    // real media position.
    let (c, sc, _) = connect_pair(0, Config::default());

    c.set_activity(Activity {
        details: "Show".to_string(),
        state: "S1 E2".to_string(),
        small_image: "play".to_string(),
        has_start: true,
        start: 111,
        end: 222,
        ..Activity::default()
    })
    .unwrap_or_else(|e| panic!("SetActivity: {e}"));
    let (_, payload) = sc
        .read_frame()
        .unwrap_or_else(|e| panic!("read activity frame: {e}"));

    let top = decode_frame(&payload);
    let a = activity_of(&top);
    assert_eq!(a["type"].as_i64(), Some(3), "type (WATCHING)");
    let ts = as_map(&a["timestamps"], "timestamps");
    want_keys("timestamps", ts, &["start", "end"]);
    assert_eq!(ts["start"].as_i64(), Some(111), "timestamps.start");
    assert_eq!(ts["end"].as_i64(), Some(222), "timestamps.end");
    sc.close();
}

#[test]
fn test_set_activity_paused_start_only_no_end() {
    // A paused card carries timestamps.start anchored at the pause moment and NO
    // timestamps.end, so the client counts the pause UP ("paused for M:SS").
    let (c, sc, _) = connect_pair(0, Config::default());

    c.set_activity(Activity {
        details: "Show".to_string(),
        state: r#"S1 E2 "Pilot" - Paused"#.to_string(),
        small_image: "pause".to_string(),
        has_start: true,
        start: 5000,
        ..Activity::default()
    })
    .unwrap_or_else(|e| panic!("SetActivity: {e}"));
    let (_, payload) = sc
        .read_frame()
        .unwrap_or_else(|e| panic!("read activity frame: {e}"));
    assert!(
        !String::from_utf8_lossy(&payload).contains(r#""end""#),
        "paused payload carries end: {}",
        String::from_utf8_lossy(&payload)
    );

    let top = decode_frame(&payload);
    let a = activity_of(&top);
    assert_eq!(a["type"].as_i64(), Some(3), "type (WATCHING)");
    let ts = as_map(&a["timestamps"], "timestamps");
    want_keys("timestamps", ts, &["start"]);
    assert_eq!(ts["start"].as_i64(), Some(5000), "timestamps.start");
    sc.close();
}

#[test]
fn test_wire_json_mapping() {
    // The Netflix-style card on the exact wire object: header `name`, two body
    // lines (`details`, `state`), brand text, and no buttons/party/secrets - and
    // no small icon keys. Callers still set SmallImage/SmallText; the wire drops
    // them.
    let cases: Vec<(&str, Activity, &str)> = vec![
        (
            "movie",
            Activity {
                name: "Movie".to_string(),
                details: "Movie".to_string(),
                large_text: "PotFlix".to_string(),
                small_image: "play".to_string(),
                small_text: "Playing".to_string(),
                has_start: true,
                start: 12345,
                end: 67890,
                ..Activity::default()
            },
            r#"{"type":3,"name":"Movie","details":"Movie","timestamps":{"start":12345,"end":67890},"assets":{"large_image":"logo","large_text":"PotFlix"}}"#,
        ),
        (
            "tv with episode name and tag",
            Activity {
                name: "Show".to_string(),
                details: "Pilot".to_string(),
                state: "S1 E2".to_string(),
                large_text: "PotFlix".to_string(),
                small_image: "play".to_string(),
                small_text: "Playing".to_string(),
                has_start: true,
                start: 111,
                end: 222,
                ..Activity::default()
            },
            r#"{"type":3,"name":"Show","details":"Pilot","state":"S1 E2","timestamps":{"start":111,"end":222},"assets":{"large_image":"logo","large_text":"PotFlix"}}"#,
        ),
        (
            "tv with tag only",
            Activity {
                name: "Show".to_string(),
                details: "S1 E2".to_string(),
                large_text: "PotFlix".to_string(),
                small_image: "play".to_string(),
                small_text: "Playing".to_string(),
                has_start: true,
                start: 111,
                end: 222,
                ..Activity::default()
            },
            r#"{"type":3,"name":"Show","details":"S1 E2","timestamps":{"start":111,"end":222},"assets":{"large_image":"logo","large_text":"PotFlix"}}"#,
        ),
        (
            "paused",
            Activity {
                name: "Show".to_string(),
                details: "Pilot".to_string(),
                state: "S1 E2 - Paused".to_string(),
                large_text: "PotFlix".to_string(),
                small_image: "pause".to_string(),
                small_text: "Paused".to_string(),
                has_start: true,
                start: 5000,
                ..Activity::default()
            },
            r#"{"type":3,"name":"Show","details":"Pilot","state":"S1 E2 - Paused","timestamps":{"start":5000},"assets":{"large_image":"logo","large_text":"PotFlix"}}"#,
        ),
        (
            "idle",
            Activity {
                details: "Browsing".to_string(),
                state: "Idle".to_string(),
                large_text: "PotFlix".to_string(),
                small_image: "stop".to_string(),
                small_text: "Idle".to_string(),
                ..Activity::default()
            },
            r#"{"type":3,"details":"Browsing","state":"Idle","assets":{"large_image":"logo","large_text":"PotFlix"}}"#,
        ),
    ];
    for (name, act, want) in cases {
        let got = wire_json(&act);
        assert_eq!(got, want, "{name}: WireJSON");
    }
}

#[test]
fn test_clear_activity_is_null() {
    let (c, sc, _) = connect_pair(0, Config::default());

    c.clear_activity()
        .unwrap_or_else(|e| panic!("ClearActivity: {e}"));
    let (op, payload) = sc
        .read_frame()
        .unwrap_or_else(|e| panic!("read clear frame: {e}"));
    assert_eq!(op, OP_FRAME, "clear opcode");
    assert!(
        String::from_utf8_lossy(&payload).contains(r#""activity":null"#),
        "clear payload = {}, want activity null",
        String::from_utf8_lossy(&payload)
    );

    let top = decode_frame(&payload);
    want_keys("command", &top, &["cmd", "args", "nonce"]);
    let args = as_map(&top["args"], "args");
    want_keys("args", args, &["pid", "activity"]);
    assert!(
        args["activity"].is_null(),
        "activity = {}, want null",
        args["activity"]
    );
    sc.close();
}

#[test]
fn test_wire_json_buttons_always_omitted() {
    // Pristine card: even fully valid buttons never reach the wire - the field is
    // omitted, not sent as null or [].
    let got = wire_json(&Activity {
        details: "Show".to_string(),
        state: "S1 E2".to_string(),
        small_image: "play".to_string(),
        buttons: vec![
            ActivityButton {
                label: "View Episode".to_string(),
                url: "https://example.com/ep/1".to_string(),
            },
            ActivityButton {
                label: "IMDb".to_string(),
                url: "https://example.com/imdb".to_string(),
            },
        ],
        ..Activity::default()
    });
    assert!(
        !got.contains(r#""buttons""#),
        "valid buttons present, want omitted: {got}"
    );
    for buttons in [Vec::new(), vec![]] {
        let got = wire_json(&Activity {
            details: "Show".to_string(),
            state: "S1 E2".to_string(),
            small_image: "play".to_string(),
            buttons,
            ..Activity::default()
        });
        assert!(
            !got.contains(r#""buttons""#),
            "empty buttons present, want omitted: {got}"
        );
    }
}

/// The clean-card proof: a full playing card with every legacy field set (icons,
/// texts, buttons) serializes to exactly the pristine object - header, two body
/// lines, time bar, brand art - with no buttons key and no small_image/small_text
/// keys anywhere.
#[test]
fn test_wire_json_pristine_golden() {
    let got = wire_json(&Activity {
        name: "Show".to_string(),
        details: "Pilot".to_string(),
        state: "S1 E2".to_string(),
        large_image: "https://example.com/poster.jpg".to_string(),
        large_text: "PotFlix".to_string(),
        small_image: "play".to_string(),
        small_text: "Playing".to_string(),
        has_start: true,
        start: 111,
        end: 222,
        buttons: vec![ActivityButton {
            label: "Trailer".to_string(),
            url: "https://example.com/trailer".to_string(),
        }],
    });
    let want = r#"{"type":3,"name":"Show","details":"Pilot","state":"S1 E2","timestamps":{"start":111,"end":222},"assets":{"large_image":"https://example.com/poster.jpg","large_text":"PotFlix"}}"#;
    assert_eq!(got, want, "WireJSON");
    for key in [r#""buttons""#, r#""small_image""#, r#""small_text""#] {
        assert!(!got.contains(key), "pristine payload carries {key}: {got}");
    }
}
