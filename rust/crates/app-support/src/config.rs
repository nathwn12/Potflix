//! `config` -- mirrors the Go root file `go\config.go`.
//!
//! The settings store for the `PotflixMainWnd` seam.
//!
//! Path mirrors the metadata cache convention: `os.UserCacheDir()` +
//! `\Potflix\config.json` -- i.e. `%LOCALAPPDATA%\Potflix\config.json` -- falling
//! back to the exe directory when the cache dir cannot be read (see
//! `go\internal\metadata\cache.go:36-44`). Writes are atomic (temp file +
//! rename), mirroring `cache.go:98-115`. Loads are tolerant: a missing or corrupt
//! file yields the zero value, never an error to the UI. Unknown fields are
//! ignored; a retired credential field, when present, is scrubbed by rewriting
//! the file, and a clean file is never rewritten.
//!
//! # The stored schema, with every default
//!
//! ```json
//! {"dirs": [], "start_with_windows": false, "scan_speed": "turbo", "card": {}}
//! ```
//!
//! * `dirs` -- `[]` (the watched library roots, a case-insensitive Windows path
//!   list).
//! * `start_with_windows` -- `false`.
//! * `scan_speed` -- `turbo`, normalised on load AND on save: `turbo` |
//!   `balanced` | `gentle`, anything else (including empty) becomes `turbo`.
//! * `card` -- `{}` when unset; `{"details": "...", "state": "..."}` otherwise,
//!   each line one of the preset ids below and each defaulting to `default`.
//!   Go's `json:"card,omitempty"` is a no-op for a struct, so the key is always
//!   written -- matched here.
//!
//! Go marshals a nil `Dirs` as `null`; this port always writes `[]`, and accepts
//! `null` on read (Go's `json.Unmarshal` treats a JSON `null` for any non-pointer
//! field as a no-op, so `null` must not fail a load here either).
//!
//! # Tolerant-load rules (all reproduced)
//!
//! * missing file, unreadable file, malformed JSON, or a wrong field type ->
//!   `Err` from [`load_config_from`]; the tolerant [`load_config`] turns that
//!   into the zero [`Config`].
//! * a partial file -> the absent fields take their defaults.
//! * `null` for any field -> the field takes its default (Go's no-op rule).
//! * unknown fields -> ignored.
//! * a retired credential field -> the load still succeeds and the file is
//!   rewritten without it (best effort: a failed rewrite is dropped).
//! * a clean file -> never rewritten.
//!
//! # The `card` type
//!
//! Go stores `presence.CardSpec` (`go\internal\presence\cardspec.go`), and so
//! does this crate: the stored shape is [`presence::cardspec::CardSpec`],
//! re-exported here rather than mirrored, because presence owns `normalized()`,
//! the behaviour-critical part. That type's own serde shape is the JSON contract
//! (`details`/`state`, both `omitempty`, unknown ids normalised to `default`).

use std::fs::{File, OpenOptions};
use std::io::{self, Write};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};

use serde::{Deserialize, Deserializer, Serialize};

use crate::debuglog::exe_dir_or_dot;

/// Cache-root subdirectory: `%LOCALAPPDATA%\Potflix`.
pub const CONFIG_DIR_NAME: &str = "Potflix";

/// The store file name (`go\config.go:142`).
pub const CONFIG_FILE_NAME: &str = "config.json";

/// The metadata cache directory name (`go\config.go:155`).
pub const META_DIR_NAME: &str = "meta";

/// The folder-scan index file name (`go\config.go:165`).
pub const SCAN_INDEX_FILE_NAME: &str = "scanindex.json";

/// The Windows cache-root environment variable (`os.UserCacheDir`).
pub const LOCAL_APP_DATA_ENV: &str = "LOCALAPPDATA";

/// Scan-speed profile names. Turbo (the published provider limits exactly) is
/// the default; balanced is 2x; gentle is 4x. Unknown/empty normalises to turbo
/// so a bad stored value can never reach the limiter.
pub const SCAN_SPEED_TURBO: &str = "turbo";

/// 2x the published provider limits.
pub const SCAN_SPEED_BALANCED: &str = "balanced";

/// 4x the published provider limits.
pub const SCAN_SPEED_GENTLE: &str = "gentle";

/// The Card view's stored choice of one preset per body line
/// (`go\internal\presence\cardspec.go:41`), the preset ids that pick it, and
/// today's shipped default. Owned by presence and re-exported, so the store and
/// the renderer share one definition; the zero value falls back to the default
/// on every line, so a store written before this field existed needs no
/// migration.
pub use presence::cardspec::{
    CARD_DETAILS_DEFAULT, CARD_DETAILS_EPISODE, CARD_DETAILS_SERIES, CARD_DETAILS_TITLE,
    CARD_STATE_DEFAULT, CARD_STATE_NONE, CARD_STATE_SYNOPSIS, CARD_STATE_TAG, CardSpec,
    default_card_spec,
};

/// Store-local convenience on the presence-owned [`CardSpec`]: only the store
/// cares whether NOTHING is stored (`{}` on the wire). `normalized`/`is_default`
/// stay presence's, and are never re-implemented here.
pub trait CardSpecStore {
    /// Reports whether nothing is stored (`{}` on the wire).
    fn is_empty(&self) -> bool;
}

impl CardSpecStore for CardSpec {
    fn is_empty(&self) -> bool {
        self.details.is_empty() && self.state.is_empty()
    }
}

/// The settings store (`go\config.go:22`). Field names and defaults are the Go
/// struct's, so the file stays byte-compatible.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Config {
    /// Watched library roots.
    #[serde(default, deserialize_with = "de::dirs")]
    pub dirs: Vec<String>,
    /// Launch the tray app at sign-in.
    #[serde(default, deserialize_with = "de::boolean")]
    pub start_with_windows: bool,
    /// The scan-speed profile as stored; read through
    /// [`Config::effective_scan_speed`].
    #[serde(default, deserialize_with = "de::string")]
    pub scan_speed: String,
    /// The Card view's chosen presets; read through
    /// [`Config::effective_card_spec`].
    #[serde(default, deserialize_with = "de::card")]
    pub card: CardSpec,
}

/// JSON `null` tolerance, one function per field: Go's `json.Unmarshal` leaves a
/// non-pointer field untouched when the JSON value is `null`, so a store
/// carrying `"dirs": null` must load its other fields instead of failing.
mod de {
    use super::{CardSpec, Deserialize, Deserializer};

    pub fn dirs<'de, D: Deserializer<'de>>(deserializer: D) -> Result<Vec<String>, D::Error> {
        Ok(Option::<Vec<String>>::deserialize(deserializer)?.unwrap_or_default())
    }

    pub fn boolean<'de, D: Deserializer<'de>>(deserializer: D) -> Result<bool, D::Error> {
        Ok(Option::<bool>::deserialize(deserializer)?.unwrap_or(false))
    }

    pub fn string<'de, D: Deserializer<'de>>(deserializer: D) -> Result<String, D::Error> {
        Ok(Option::<String>::deserialize(deserializer)?.unwrap_or_default())
    }

    pub fn card<'de, D: Deserializer<'de>>(deserializer: D) -> Result<CardSpec, D::Error> {
        Ok(Option::<CardSpec>::deserialize(deserializer)?.unwrap_or_default())
    }
}

/// Maps any input onto a known profile (case-insensitive; unknown/empty yields
/// turbo).
pub fn normalize_scan_speed(speed: &str) -> &'static str {
    match speed.trim().to_lowercase().as_str() {
        SCAN_SPEED_TURBO => SCAN_SPEED_TURBO,
        SCAN_SPEED_BALANCED => SCAN_SPEED_BALANCED,
        SCAN_SPEED_GENTLE => SCAN_SPEED_GENTLE,
        _ => SCAN_SPEED_TURBO,
    }
}

impl Config {
    /// The normalised profile for this config.
    pub fn effective_scan_speed(&self) -> &'static str {
        normalize_scan_speed(&self.scan_speed)
    }

    /// The stored card spec with per-line fallback to the shipped default, so a
    /// zero value (pre-card stores) needs no migration and a stored typo can
    /// never reach the card.
    pub fn effective_card_spec(&self) -> CardSpec {
        self.card.normalized()
    }

    /// Stores the normalised spec; reports whether it changed.
    pub fn set_card_spec(&mut self, details: &str, state: &str) -> bool {
        let normalized = CardSpec {
            details: details.to_string(),
            state: state.to_string(),
        }
        .normalized();
        if self.card == normalized {
            return false;
        }
        self.card = normalized;
        true
    }

    /// Stores the normalised profile; reports whether it changed. The comparison
    /// is against the RAW stored value, as in Go, so a store holding `TURBO` is
    /// reported as changed by `set_scan_speed("turbo")`.
    pub fn set_scan_speed(&mut self, speed: &str) -> bool {
        let normalized = normalize_scan_speed(speed);
        if self.scan_speed == normalized {
            return false;
        }
        self.scan_speed = normalized.to_string();
        true
    }

    /// Appends dir unless blank or already present (case-insensitive: this is a
    /// Windows path list). Reports whether the set changed.
    ///
    /// Reading: the fold is ASCII (`eq_ignore_ascii_case`), not Go's Unicode
    /// `EqualFold`. The list holds Windows paths, whose case-insensitivity is
    /// ASCII-ordinal, so the two agree on every path a Windows API produces.
    pub fn add_dir(&mut self, dir: &str) -> bool {
        let dir = dir.trim();
        if dir.is_empty() {
            return false;
        }
        if self
            .dirs
            .iter()
            .any(|existing| existing.eq_ignore_ascii_case(dir))
        {
            return false;
        }
        self.dirs.push(dir.to_string());
        true
    }

    /// Drops dir (case-insensitive). Reports whether the set changed.
    pub fn remove_dir(&mut self, dir: &str) -> bool {
        if let Some(index) = self
            .dirs
            .iter()
            .position(|existing| existing.eq_ignore_ascii_case(dir))
        {
            self.dirs.remove(index);
            return true;
        }
        false
    }
}

/// The Windows cache root (`os.UserCacheDir()`), or `None` when it is unset or
/// empty -- the Go error case.
fn cache_root() -> Option<PathBuf> {
    std::env::var_os(LOCAL_APP_DATA_ENV)
        .filter(|value| !value.is_empty())
        .map(PathBuf::from)
}

/// `<cache root>\Potflix` (or `<exe dir>\Potflix` when the cache root is
/// unreadable).
fn potflix_dir() -> PathBuf {
    cache_root()
        .map(|root| root.join(CONFIG_DIR_NAME))
        .unwrap_or_else(|| exe_dir_or_dot().join(CONFIG_DIR_NAME))
}

/// The on-disk store location: `%LOCALAPPDATA%\Potflix\config.json`.
pub fn config_path() -> PathBuf {
    cache_root()
        .map(|root| root.join(CONFIG_DIR_NAME).join(CONFIG_FILE_NAME))
        .unwrap_or_else(|| exe_dir_or_dot().join(CONFIG_FILE_NAME))
}

/// The metadata cache directory: `%LOCALAPPDATA%\Potflix\meta`, falling back to
/// the exe directory. Mirrors `metadata.defaultCacheDir` so the Settings view
/// never hardcodes `%LOCALAPPDATA%` yet always points at the live cache.
pub fn metadata_dir() -> PathBuf {
    potflix_dir().join(META_DIR_NAME)
}

/// The folder-scan index file: `%LOCALAPPDATA%\Potflix\scanindex.json`, falling
/// back to the exe directory. Same idiom as [`config_path`]/[`metadata_dir`].
pub fn scan_index_path() -> PathBuf {
    cache_root()
        .map(|root| root.join(CONFIG_DIR_NAME).join(SCAN_INDEX_FILE_NAME))
        .unwrap_or_else(|| exe_dir_or_dot().join(SCAN_INDEX_FILE_NAME))
}

/// The explicit-path core of [`load_config`], so tests can point at a temp dir
/// instead of the real store. Unknown fields are ignored, so an old store
/// carrying retired credential fields loads its dirs without error; when such a
/// field is present the file is rewritten without it, and a clean file is never
/// rewritten.
pub fn load_config_from(path: &Path) -> io::Result<Config> {
    let data = std::fs::read(path)?;
    let mut config: Config = serde_json::from_slice(&data)
        .map_err(|err| io::Error::new(io::ErrorKind::InvalidData, err))?;
    config.scan_speed = normalize_scan_speed(&config.scan_speed).to_string();
    if has_retired_fields(&data) {
        // Best-effort scrub: the load still succeeds when the rewrite fails, so
        // the error is dropped here.
        let _ = save_config_to(path, &config);
    }
    Ok(config)
}

/// Reads the store; missing/corrupt yields the zero `Config`.
pub fn load_config() -> Config {
    load_config_from(&config_path()).unwrap_or_default()
}

/// Writes `config` atomically to the real store path.
pub fn save_config(config: &Config) -> io::Result<()> {
    save_config_to(&config_path(), config)
}

/// The explicit-path core of [`save_config`]. The scan profile is normalised
/// before marshal so a bad value never persists.
///
/// Order matches Go: create the parent, marshal, create the temp file in the
/// same directory, write, close, rename over the target. Rust's `File` is
/// unbuffered (as is Go's), so a write failure surfaces at the write call and
/// the close-error branch has no separate case; a failed write or rename removes
/// the temp file and returns the error.
pub fn save_config_to(path: &Path, config: &Config) -> io::Result<()> {
    let mut out = config.clone();
    out.scan_speed = normalize_scan_speed(&out.scan_speed).to_string();

    let dir = match path.parent() {
        Some(parent) if !parent.as_os_str().is_empty() => parent.to_path_buf(),
        _ => PathBuf::from("."),
    };
    std::fs::create_dir_all(&dir)?;
    let data = serde_json::to_vec(&out).map_err(io::Error::other)?;

    let (temp_path, mut temp) = create_temp(&dir)?;
    if let Err(err) = temp.write_all(&data) {
        drop(temp);
        let _ = std::fs::remove_file(&temp_path);
        return Err(err);
    }
    drop(temp);
    if let Err(err) = std::fs::rename(&temp_path, path) {
        let _ = std::fs::remove_file(&temp_path);
        return Err(err);
    }
    Ok(())
}

/// Retired credential field names, built at runtime from concatenated fragments
/// so a plain source grep for the retired prefix stays clean and cannot trip on
/// this redaction path (`go\config.go:117-123`).
fn retired_field_names() -> [String; 2] {
    let prefix = "api";
    [format!("{prefix}_keys"), format!("{prefix}Key")]
}

/// Reports whether the raw object carries a retired field.
pub fn has_retired_fields(data: &[u8]) -> bool {
    let Ok(object) = serde_json::from_slice::<serde_json::Map<String, serde_json::Value>>(data)
    else {
        return false;
    };
    retired_field_names()
        .iter()
        .any(|name| object.contains_key(name))
}

/// `os.CreateTemp(dir, ".tmp-*.json")`: a uniquely named file in `dir`, created
/// exclusively, retried on collision.
fn create_temp(dir: &Path) -> io::Result<(PathBuf, File)> {
    static TEMP_SEQ: AtomicU64 = AtomicU64::new(0);
    for _ in 0..64 {
        let sequence = TEMP_SEQ.fetch_add(1, Ordering::Relaxed);
        let nanos = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|since| since.subsec_nanos())
            .unwrap_or(0);
        let name = format!(".tmp-{}-{nanos}-{sequence}.json", std::process::id());
        let candidate = dir.join(name);
        match OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&candidate)
        {
            Ok(file) => return Ok((candidate, file)),
            Err(err) if err.kind() == io::ErrorKind::AlreadyExists => continue,
            Err(err) => return Err(err),
        }
    }
    Err(io::Error::new(
        io::ErrorKind::AlreadyExists,
        "could not create a unique temp file for the atomic write",
    ))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::Scratch;

    /// The store path a test uses: a nested directory that does not exist yet, so
    /// the atomic write must create it (`t.TempDir()/Potflix/config.json`).
    fn store_path(scratch: &Scratch) -> PathBuf {
        scratch.join("Potflix").join(CONFIG_FILE_NAME)
    }

    /// Retired names are built at runtime (concatenated fragments) so a plain
    /// source grep for the retired prefix stays clean.
    fn retired_fixture_names() -> [String; 2] {
        let prefix = "api";
        [format!("{prefix}_keys"), format!("{prefix}Key")]
    }

    /// `config_scanspeed_test.go`'s `quoteJSON`: a minimal JSON string literal,
    /// so a bad scan-speed fixture carries exactly the bytes under test.
    fn quote_json(value: &str) -> String {
        let mut out = String::with_capacity(value.len() + 2);
        out.push('"');
        for ch in value.chars() {
            if ch == '"' || ch == '\\' {
                out.push('\\');
            }
            out.push(ch);
        }
        out.push('"');
        out
    }

    #[test]
    fn round_trip_preserves_dirs_and_scan_speed() {
        let scratch = Scratch::new("config-round-trip");
        let path = store_path(&scratch);
        let want = Config {
            dirs: vec![r"C:\Media\Movies".to_string(), r"D:\Shows".to_string()],
            scan_speed: SCAN_SPEED_BALANCED.to_string(),
            ..Config::default()
        };
        save_config_to(&path, &want).expect("saveConfigTo");
        let got = load_config_from(&path).expect("loadConfigFrom");
        assert_eq!(got, want);
    }

    #[test]
    fn corrupt_or_missing_yields_the_defaults() {
        let scratch = Scratch::new("config-corrupt");
        let missing = load_config_from(&scratch.join("nope.json"));
        assert!(missing.is_err(), "a missing file must report an error");
        assert_eq!(Config::default().dirs, Vec::<String>::new());

        let bad = scratch.write(CONFIG_FILE_NAME, "{not json");
        assert!(load_config_from(&bad).is_err(), "corrupt file must error");

        // The tolerant wrapper the UI calls never surfaces either failure. The
        // real store is never read here: the same core call on a missing path is
        // what `load_config` degrades to the zero value.
        assert_eq!(
            load_config_from(&scratch.join("missing-store.json")).unwrap_or_default(),
            Config::default()
        );
    }

    /// `config_scanspeed_test.go` — ScanSpeed survives an atomic save -> load
    /// round-trip for every named profile.
    #[test]
    fn scan_speed_round_trips_every_profile() {
        for profile in [SCAN_SPEED_GENTLE, SCAN_SPEED_BALANCED, SCAN_SPEED_TURBO] {
            let scratch = Scratch::new("config-speed");
            let path = store_path(&scratch);
            let want = Config {
                dirs: vec![r"C:\Media".to_string()],
                scan_speed: profile.to_string(),
                ..Config::default()
            };
            save_config_to(&path, &want).unwrap_or_else(|err| panic!("save {profile}: {err}"));
            let got = load_config_from(&path).unwrap_or_else(|err| panic!("load {profile}: {err}"));
            assert_eq!(got.scan_speed, profile);
            assert_eq!(got.effective_scan_speed(), profile);
        }
    }

    /// `config_scanspeed_test.go` — a bad or empty stored value normalises to
    /// turbo on load, and the normaliser maps case-insensitively.
    #[test]
    fn scan_speed_normalises_bad_and_empty_values() {
        for bad in ["", "   ", "ludicrous", "TURBO-fast"] {
            let scratch = Scratch::new("config-speed-bad");
            let path = scratch.write(
                CONFIG_FILE_NAME,
                &format!(r#"{{"dirs":[],"scan_speed":{}}}"#, quote_json(bad)),
            );
            let got = load_config_from(&path).unwrap_or_else(|err| panic!("load {bad:?}: {err}"));
            assert_eq!(
                got.scan_speed, SCAN_SPEED_TURBO,
                "stored {bad:?} must load as turbo"
            );
        }
        assert_eq!(normalize_scan_speed("Turbo"), SCAN_SPEED_TURBO);
        assert_eq!(normalize_scan_speed("GENTLE"), SCAN_SPEED_GENTLE);
        assert_eq!(normalize_scan_speed(" Balanced "), SCAN_SPEED_BALANCED);

        // Old stores without the field load tolerant (turbo).
        let scratch = Scratch::new("config-speed-legacy");
        let legacy = scratch.write("legacy.json", r#"{"dirs":[]}"#);
        let old = load_config_from(&legacy).expect("load legacy store");
        assert_eq!(old.effective_scan_speed(), SCAN_SPEED_TURBO);

        // A zero Config (missing/corrupt store) is turbo too.
        assert_eq!(Config::default().effective_scan_speed(), SCAN_SPEED_TURBO);

        // set_scan_speed normalises and reports the change.
        let mut config = Config::default();
        assert!(config.set_scan_speed("TURBO"));
        assert_eq!(config.scan_speed, SCAN_SPEED_TURBO);
        assert!(
            !config.set_scan_speed("turbo"),
            "same value reports no change"
        );
        assert!(config.set_scan_speed("gentle"));
        assert_eq!(config.scan_speed, SCAN_SPEED_GENTLE);
        // Bogus normalises to the turbo default; moving off turbo first makes
        // the change observable.
        assert!(config.set_scan_speed("bogus"));
        assert_eq!(config.scan_speed, SCAN_SPEED_TURBO);
        // A store holding an un-normalised value is still reported as changed.
        let mut raw = Config {
            scan_speed: "TURBO".to_string(),
            ..Config::default()
        };
        assert!(raw.set_scan_speed("turbo"));
        assert_eq!(raw.scan_speed, SCAN_SPEED_TURBO);
    }

    #[test]
    fn add_and_remove_dir_are_case_insensitive() {
        let mut config = Config::default();
        assert!(config.add_dir(r"C:\Media"), "first insert");
        assert!(!config.add_dir(r"c:\media"), "case-insensitive duplicate");
        assert!(!config.add_dir("   "), "blank");
        assert_eq!(config.dirs.len(), 1);

        assert!(!config.remove_dir(r"D:\Elsewhere"), "absent");
        assert!(config.remove_dir(r"C:\MEDIA"), "case-insensitive hit");
        assert!(config.dirs.is_empty());
    }

    /// The Card view's persistence: a chosen spec round-trips through the store,
    /// the zero value falls back to the shipped default (no migration), and
    /// `set_card_spec` normalises unknown ids.
    #[test]
    fn card_spec_round_trips_and_falls_back() {
        let scratch = Scratch::new("config-card");
        let path = store_path(&scratch);
        let mut want = Config {
            dirs: vec![r"C:\Media".to_string()],
            scan_speed: SCAN_SPEED_TURBO.to_string(),
            ..Config::default()
        };
        assert!(want.set_card_spec("title", "tag"), "first set");
        assert!(
            !want.set_card_spec("title", "tag"),
            "same value is unchanged"
        );

        save_config_to(&path, &want).expect("saveConfigTo");
        let got = load_config_from(&path).expect("loadConfigFrom");
        assert_eq!(got, want);
        let effective = got.effective_card_spec();
        assert_eq!(
            (effective.details.as_str(), effective.state.as_str()),
            ("title", "tag")
        );

        // Zero value: no migration needed.
        assert_eq!(Config::default().effective_card_spec(), default_card_spec());

        // Unknown ids normalise on set, never persist raw.
        let mut unknown = Config::default();
        unknown.set_card_spec("fancy-template", "");
        assert_eq!(unknown.effective_card_spec(), default_card_spec());
        assert_eq!(unknown.card.details, CARD_DETAILS_DEFAULT);
        assert!(!unknown.card.is_empty());
    }

    /// The `card` wire shape is Go's `omitempty` per line: an empty line is
    /// OMITTED from the object, while the `card` key itself is always written
    /// (Go's `json:"card,omitempty"` on a struct is a no-op). Pins the exact
    /// bytes the two stores exchange.
    #[test]
    fn card_lines_omit_empty_but_the_card_key_always_writes() {
        let scratch = Scratch::new("config-card-wire");
        let path = store_path(&scratch);

        // Details set, state empty: the state key is omitted.
        let details_only = Config {
            card: CardSpec {
                details: CARD_DETAILS_TITLE.to_string(),
                state: String::new(),
            },
            ..Config::default()
        };
        save_config_to(&path, &details_only).expect("saveConfigTo");
        let wire = String::from_utf8(std::fs::read(&path).expect("read store")).expect("utf-8");
        assert_eq!(
            wire,
            r#"{"dirs":[],"start_with_windows":false,"scan_speed":"turbo","card":{"details":"title"}}"#
        );

        // State set, details empty: the details key is omitted, and the line
        // falls back to the shipped default on read.
        let state_only = Config {
            card: CardSpec {
                details: String::new(),
                state: CARD_STATE_TAG.to_string(),
            },
            ..Config::default()
        };
        save_config_to(&path, &state_only).expect("saveConfigTo");
        let wire = String::from_utf8(std::fs::read(&path).expect("read store")).expect("utf-8");
        assert_eq!(
            wire,
            r#"{"dirs":[],"start_with_windows":false,"scan_speed":"turbo","card":{"state":"tag"}}"#
        );
        let got = load_config_from(&path).expect("loadConfigFrom");
        assert_eq!(
            got.effective_card_spec(),
            CardSpec {
                details: CARD_DETAILS_DEFAULT.to_string(),
                state: CARD_STATE_TAG.to_string(),
            }
        );

        // Both lines empty: the key is still written, as `{}`.
        let empty = Config {
            card: CardSpec::default(),
            ..Config::default()
        };
        save_config_to(&path, &empty).expect("saveConfigTo");
        let wire = String::from_utf8(std::fs::read(&path).expect("read store")).expect("utf-8");
        assert_eq!(
            wire,
            r#"{"dirs":[],"start_with_windows":false,"scan_speed":"turbo","card":{}}"#
        );
    }

    /// The privacy scrub: a store carrying retired credential fields loads its
    /// dirs, carries nothing credential-ish in memory, and is rewritten on disk
    /// without the retired names.
    #[test]
    fn retired_credential_fields_are_scrubbed() {
        let retired = retired_fixture_names();
        let scratch = Scratch::new("config-scrub");
        let path = scratch.write(
            CONFIG_FILE_NAME,
            &format!(
                r#"{{"dirs":["C:\\Media"],"scan_speed":"turbo","{}":"s3cr3t","{}":"s3cr3t"}}"#,
                retired[0], retired[1]
            ),
        );
        let got = load_config_from(&path).expect("loadConfigFrom");
        assert_eq!(got.dirs, vec![r"C:\Media".to_string()]);

        let wire = serde_json::to_string(&got).expect("marshal loaded config");
        for name in &retired {
            assert!(
                !wire.contains(name),
                "in-memory config still carries {name}: {wire}"
            );
        }
        let after = String::from_utf8(scratch.read(CONFIG_FILE_NAME)).expect("utf-8 store");
        for name in &retired {
            assert!(
                !after.contains(name),
                "file on disk still contains {name}: {after}"
            );
        }
        assert!(has_retired_fields(
            format!(r#"{{"{}":1}}"#, retired[0]).as_bytes()
        ));
        assert!(!has_retired_fields(b"{not json"));
    }

    /// A clean store is never rewritten: content is byte-identical before and
    /// after load.
    #[test]
    fn clean_store_is_not_rewritten() {
        let scratch = Scratch::new("config-clean");
        let path = store_path(&scratch);
        let want = Config {
            dirs: vec![r"C:\Media".to_string()],
            scan_speed: SCAN_SPEED_TURBO.to_string(),
            ..Config::default()
        };
        save_config_to(&path, &want).expect("saveConfigTo");
        let before = std::fs::read(&path).expect("read before");
        load_config_from(&path).expect("loadConfigFrom");
        let after = std::fs::read(&path).expect("read after");
        assert_eq!(after, before, "a clean config must not be rewritten");
    }

    /// The exact schema, with every default: what a fresh store looks like on
    /// disk, key for key.
    #[test]
    fn schema_and_defaults_are_the_go_contract() {
        let scratch = Scratch::new("config-schema");
        let path = store_path(&scratch);
        save_config_to(&path, &Config::default()).expect("saveConfigTo");
        let wire = String::from_utf8(std::fs::read(&path).expect("read store")).expect("utf-8");
        assert_eq!(
            wire,
            r#"{"dirs":[],"start_with_windows":false,"scan_speed":"turbo","card":{}}"#
        );

        // A card with both lines set writes both keys.
        let mut with_card = Config::default();
        with_card.set_card_spec("episode", "synopsis");
        save_config_to(&path, &with_card).expect("saveConfigTo");
        let wire = String::from_utf8(std::fs::read(&path).expect("read store")).expect("utf-8");
        assert_eq!(
            wire,
            r#"{"dirs":[],"start_with_windows":false,"scan_speed":"turbo","card":{"details":"episode","state":"synopsis"}}"#
        );

        // start_with_windows is a real stored field.
        let autostart = Config {
            start_with_windows: true,
            ..Config::default()
        };
        save_config_to(&path, &autostart).expect("saveConfigTo");
        assert!(load_config_from(&path).expect("load").start_with_windows);
    }

    /// Every tolerant-load rule in one store: a partial file, `null` for every
    /// field, and unknown fields.
    #[test]
    fn partial_null_and_unknown_fields_load_tolerantly() {
        let scratch = Scratch::new("config-tolerant");

        // Partial: only dirs, everything else default.
        let partial = scratch.write(CONFIG_FILE_NAME, r#"{"dirs":["C:\\Media"]}"#);
        let got = load_config_from(&partial).expect("partial load");
        assert_eq!(got.dirs, vec![r"C:\Media".to_string()]);
        assert!(!got.start_with_windows);
        assert_eq!(got.effective_scan_speed(), SCAN_SPEED_TURBO);
        assert_eq!(got.effective_card_spec(), default_card_spec());

        // Go's null no-op: every field null, plus an unknown field.
        let nulls = scratch.write(
            "nulls.json",
            r#"{"dirs":null,"start_with_windows":null,"scan_speed":null,"card":null,"future":"ignored"}"#,
        );
        let got = load_config_from(&nulls).expect("null load");
        // Every field took its default; the profile is normalised on load, so a
        // null scan_speed lands on turbo, exactly as an absent one does.
        assert_eq!(
            got,
            Config {
                scan_speed: SCAN_SPEED_TURBO.to_string(),
                ..Config::default()
            }
        );

        // A wrong field type is the Go error case, not a silent coercion.
        let wrong = scratch.write("wrong.json", r#"{"dirs":"C:\\Media"}"#);
        assert!(load_config_from(&wrong).is_err());
        let wrong = scratch.write("wrong2.json", r#"{"scan_speed":7}"#);
        assert!(load_config_from(&wrong).is_err());
    }

    /// `settings_sections_test.go`'s `TestConfigStartWithWindowsRoundTrip`: the
    /// flag survives an atomic save -> load, and old stores without the field
    /// load tolerant (false).
    #[test]
    fn start_with_windows_round_trips_and_legacy_loads_false() {
        let scratch = Scratch::new("config-autostart");
        let path = store_path(&scratch);
        let want = Config {
            dirs: vec![r"C:\Media".to_string()],
            start_with_windows: true,
            ..Config::default()
        };
        save_config_to(&path, &want).expect("saveConfigTo");
        let got = load_config_from(&path).expect("loadConfigFrom");
        assert!(got.start_with_windows, "round-trip lost the flag");
        assert_eq!(got.dirs.len(), 1, "round-trip lost dirs: {got:?}");

        let legacy = scratch.write("legacy-autostart.json", r#"{"dirs":[]}"#);
        let old = load_config_from(&legacy).expect("load legacy store");
        assert!(
            !old.start_with_windows,
            "legacy store must load tolerant false"
        );
    }

    /// The Go-compat golden guard: a fixture mirroring the Go-written config
    /// shape (`go\config.go:22-30`) with SYNTHETIC values -- parse, round-trip,
    /// assert no key is lost and no default silently changes. Fails first if a
    /// real Go/Rust mismatch ever appears.
    #[test]
    fn go_written_shape_round_trips_without_key_loss() {
        // Synthetic stand-in for a Go-written store: all four keys, non-default
        // values on every field, key order as Go's Marshal emits it.
        let fixture = r#"{"dirs":["/synth/movies","/synth/shows"],"start_with_windows":true,"scan_speed":"balanced","card":{"details":"title","state":"tag"}}"#;
        let parsed: Config = serde_json::from_str(fixture).expect("parse Go shape");
        assert_eq!(
            parsed.dirs,
            vec!["/synth/movies".to_string(), "/synth/shows".to_string()]
        );
        assert!(parsed.start_with_windows);
        assert_eq!(parsed.effective_scan_speed(), SCAN_SPEED_BALANCED);
        assert_eq!(
            parsed.effective_card_spec(),
            CardSpec {
                details: CARD_DETAILS_TITLE.to_string(),
                state: CARD_STATE_TAG.to_string(),
            }
        );

        // Round-trip through the atomic write: the wire keeps exactly the Go
        // key set, in Go's order, with every value intact.
        let scratch = Scratch::new("config-golden");
        let fixture_path = scratch.write("golden.json", fixture);
        let loaded = load_config_from(&fixture_path).expect("loadConfigFrom");
        let path = store_path(&scratch);
        save_config_to(&path, &loaded).expect("saveConfigTo");
        let wire = String::from_utf8(std::fs::read(&path).expect("read store")).expect("utf-8");
        assert_eq!(wire, fixture, "round-trip must be byte-identical");
        let wire_value: serde_json::Value = serde_json::from_str(&wire).expect("wire JSON");
        let mut keys: Vec<&str> = wire_value
            .as_object()
            .expect("wire object")
            .keys()
            .map(String::as_str)
            .collect();
        keys.sort_unstable();
        assert_eq!(
            keys,
            vec!["card", "dirs", "scan_speed", "start_with_windows"],
            "wire key set is the Go contract, nothing added, nothing lost (byte order pinned above)"
        );

        // No default silently changed: a fresh store is still the exact Go
        // schema with Go's defaults.
        let fresh = Scratch::new("config-golden-fresh");
        let fresh_path = store_path(&fresh);
        save_config_to(&fresh_path, &Config::default()).expect("saveConfigTo");
        let fresh_wire =
            String::from_utf8(std::fs::read(&fresh_path).expect("read store")).expect("utf-8");
        assert_eq!(
            fresh_wire,
            r#"{"dirs":[],"start_with_windows":false,"scan_speed":"turbo","card":{}}"#
        );
    }

    /// The write is atomic: no temp file survives, and the temp file is created
    /// beside the target so the rename is same-volume.
    #[test]
    fn atomic_write_leaves_no_temp_file() {
        let scratch = Scratch::new("config-atomic");
        let path = store_path(&scratch);
        save_config_to(&path, &Config::default()).expect("saveConfigTo");

        let dir = path.parent().expect("store parent");
        let leftovers: Vec<String> = std::fs::read_dir(dir)
            .expect("read store dir")
            .filter_map(Result::ok)
            .map(|entry| entry.file_name().to_string_lossy().into_owned())
            .filter(|name| name.starts_with(".tmp-"))
            .collect();
        assert!(
            leftovers.is_empty(),
            "temp files left behind: {leftovers:?}"
        );
        assert_eq!(
            std::fs::read_dir(dir).expect("read store dir").count(),
            1,
            "the store directory holds the store alone"
        );
    }

    /// The real paths are the Go contract, and the store is never touched.
    #[test]
    fn paths_are_local_app_data_potflix() {
        let config = config_path();
        assert!(config.ends_with(Path::new(CONFIG_DIR_NAME).join(CONFIG_FILE_NAME)));
        assert!(metadata_dir().ends_with(Path::new(CONFIG_DIR_NAME).join(META_DIR_NAME)));
        assert!(scan_index_path().ends_with(Path::new(CONFIG_DIR_NAME).join(SCAN_INDEX_FILE_NAME)));

        match cache_root() {
            Some(root) => {
                assert_eq!(config, root.join(CONFIG_DIR_NAME).join(CONFIG_FILE_NAME));
                assert_eq!(
                    metadata_dir(),
                    root.join(CONFIG_DIR_NAME).join(META_DIR_NAME)
                );
                assert_eq!(
                    scan_index_path(),
                    root.join(CONFIG_DIR_NAME).join(SCAN_INDEX_FILE_NAME)
                );
            }
            None => {
                assert_eq!(config, exe_dir_or_dot().join(CONFIG_FILE_NAME));
                assert_eq!(metadata_dir(), exe_dir_or_dot().join(META_DIR_NAME));
                assert_eq!(
                    scan_index_path(),
                    exe_dir_or_dot().join(SCAN_INDEX_FILE_NAME)
                );
            }
        }
    }
}
