//! Ports `go\internal\metadata\context_test.go` (2 tests): the required path
//! vectors (the Mr.Harbor release tree, the Zentaro Season folder, a
//! generics-only path) and the `ResolveQuery` merges on top.
//!
//! Note on punctuation: titleclean normalises dots to spaces, so the folder show
//! for the Mr.Harbor tree is "Mr Harbor", not "Mr. Harbor" (the dotted canonical
//! form arrives later from the provider resolution).

use crate::context::{context_from_path, resolve_query, under_extras_folder};
use crate::types::{Kind, Query};

const MR_HARBOR_PATH: &str = r"M:\1ARCHIVE\Mr.Harbor.COMPLETE.1080p.BluRay.AV1.DDP.5.1-dAV1nci\Mr.Harbor.S01.1080p.BluRay.AV1.DDP.5.1-dAV1nci\Mr.Harbor.S01E01.1080p.BluRay.AV1.DDP.5.1-dAV1nci.mkv";
const MR_HARBOR_FILE: &str = "Mr.Harbor.S01E01.1080p.BluRay.AV1.DDP.5.1-dAV1nci.mkv";
const ZENTARO_PATH: &str = r"M:\2SERIES\Zentaro\Season 1\01.mkv";

#[test]
fn context_from_path_cases() {
    let cases: &[(&str, &str, &str, i32)] = &[
        ("mr harbor release tree", MR_HARBOR_PATH, "Mr Harbor", 1),
        ("zentaro season folder", ZENTARO_PATH, "Zentaro", 1),
        ("short season leaf", r"M:\2FILMS\Show\S02\05.mkv", "Show", 2),
        (
            "generics only",
            r"M:\1ARCHIVE\Extras\Featurettes\Trailer.mkv",
            "",
            0,
        ),
        ("bare filename", "Trailer.mkv", "", 0),
        ("empty path", "", "", 0),
    ];
    for (name, path, show, season) in cases {
        let got = context_from_path(path);
        assert_eq!(
            (got.0.as_str(), got.1),
            (*show, *season),
            "ContextFromPath({path:?}) [{name}]"
        );
    }
}

#[test]
fn resolve_query_cases() {
    // bare number takes folder show
    let got = resolve_query("01.mkv", ZENTARO_PATH);
    let want = Query {
        kind: Kind::SERIES,
        title: "Zentaro".into(),
        season: 1,
        episode: 1,
        ..Query::default()
    };
    assert_eq!(got, want, "ResolveQuery(01.mkv, zentaro)");

    // extra filename stays unknown
    let got = resolve_query(
        "Deleted Scenes.mkv",
        r"M:\1ARCHIVE\Show\Extras\Deleted Scenes.mkv",
    );
    assert_eq!(
        got.kind,
        Kind::UNKNOWN,
        "ResolveQuery(Deleted Scenes.mkv, extras path) = {got:?}"
    );

    // mr harbor filename and path
    let got = resolve_query(MR_HARBOR_FILE, MR_HARBOR_PATH);
    let want = Query {
        kind: Kind::SERIES,
        title: "Mr Harbor".into(),
        season: 1,
        episode: 1,
        ..Query::default()
    };
    assert_eq!(got, want, "ResolveQuery(mrharbor, mrharbor)");

    // empty path is filename-only
    let got = resolve_query("01.mkv", "");
    assert_eq!(got.kind, Kind::UNKNOWN, "ResolveQuery(01.mkv, empty)");

    // theme file never rescues
    let got = resolve_query(
        "[Studio Hour] Zentaro - OP - 001 - XA?B?C?D.mkv",
        ZENTARO_PATH,
    );
    assert_eq!(got.kind, Kind::UNKNOWN, "ResolveQuery(Zentaro OP, zentaro)");

    // bare number under extras stays unknown: the synthetic inventory row
    // M:\2SERIES\Harbor Lights\Featurettes\Season 5\Featurettes 5.1\Deleted Scenes\1.mkv
    let got = resolve_query(
        "1.mkv",
        r"M:\2SERIES\Harbor Lights\Featurettes\Season 5\Featurettes 5.1\Deleted Scenes\1.mkv",
    );
    assert_eq!(
        got.kind,
        Kind::UNKNOWN,
        "ResolveQuery(1.mkv, deleted-scenes)"
    );

    // short season leaf fills season
    let got = resolve_query("05.mkv", r"M:\2FILMS\Show\S02\05.mkv");
    let want = Query {
        kind: Kind::SERIES,
        title: "Show".into(),
        season: 2,
        episode: 5,
        ..Query::default()
    };
    assert_eq!(got, want, "ResolveQuery(05.mkv, S02 path)");

    // bonus phrase folder stays unknown; the show above the leaf still resolves
    for leaf in ["Making Of", "Gag Reel", "Behind the Story", "TV Spots"] {
        let path = format!(r"M:\1ARCHIVE\Show\{leaf}\01.mkv");
        let got = resolve_query("01.mkv", &path);
        assert_eq!(got.kind, Kind::UNKNOWN, "ResolveQuery(01.mkv, {path:?})");
        assert!(under_extras_folder(&path), "underExtrasFolder({path:?})");
    }
    let (show, _) = context_from_path(r"M:\1ARCHIVE\Show\Gag Reel\clip.mkv");
    assert_eq!(show, "Show", "ContextFromPath(Gag Reel leaf) show");
}
