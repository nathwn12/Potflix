//! `presence::cardspec` -- mirrors `go\internal\presence\cardspec.go`.
//!
//! The card specification: which lines the presence card carries and how the
//! clean title, canonical title and episode tag compose into them.
//!
//! A [`CardSpec`] chooses what the card's two body lines say. Each line picks
//! from a small fixed preset set ([`card_presets`]) - there is no template
//! language. The zero value falls back to the default on every line, so no
//! migration is needed for stored configs.
//!
//! [`default_card_spec`] reproduces today's shipped output BYTE-FOR-BYTE: its
//! [`CardSpec::render`] returns exactly [`build_activity_resolved`]'s
//! details/state for the same inputs. The default state carries no synopsis
//! (genres/length for a movie, the episode tag for a series); the synopsis
//! lives in the tooltip and stays one click away behind [`CARD_STATE_SYNOPSIS`].
//! Non-default presets only reselect among the same already-capped strings;
//! the paused " - Paused" marker path is preserved on every line.

use player::window::{APP_NAME, SUFFIX};
use serde::{Deserialize, Serialize};
use titleclean::{clean, trunc};

use crate::{
    Action, PAUSED_MARKER, Resolved, build_activity_resolved, rp_text, synopsis_state,
    with_state_time,
};

/// Details-line preset id: the shipped default.
pub const CARD_DETAILS_DEFAULT: &str = "default";

/// Details-line preset id: the clean title.
pub const CARD_DETAILS_TITLE: &str = "title";

/// Details-line preset id: the episode label.
pub const CARD_DETAILS_EPISODE: &str = "episode";

/// Details-line preset id: the series name.
pub const CARD_DETAILS_SERIES: &str = "series";

/// State-line preset id: the shipped default.
pub const CARD_STATE_DEFAULT: &str = "default";

/// State-line preset id: the play-state tag.
pub const CARD_STATE_TAG: &str = "tag";

/// State-line preset id: the synopsis.
pub const CARD_STATE_SYNOPSIS: &str = "synopsis";

/// State-line preset id: nothing.
pub const CARD_STATE_NONE: &str = "none";

/// The stored choice of one preset per body line (`cardspec.go:41-44`).
///
/// The serde shape is the stored config contract the app crate embeds as
/// `Card presence.CardSpec` (`go\config.go:22-30`): Go's
/// `json:"details,omitempty"` / `json:"state,omitempty"`, i.e. an empty line is
/// OMITTED from the JSON and an absent line loads as empty (then normalizes to
/// the default).
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct CardSpec {
    /// Details-line preset id.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub details: String,
    /// State-line preset id.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub state: String,
}

/// One named choice in the [`card_presets`] set (`cardspec.go:47-50`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CardPreset {
    /// The stored preset id.
    pub id: String,
    /// The short owner-drawn button label.
    pub label: String,
}

/// Today's shipped output: both lines on default (`cardspec.go:53`).
pub fn default_card_spec() -> CardSpec {
    CardSpec {
        details: CARD_DETAILS_DEFAULT.to_string(),
        state: CARD_STATE_DEFAULT.to_string(),
    }
}

impl CardSpec {
    /// Maps empty/unknown ids back to the default line, so a zero value (or a
    /// stored typo) can never reach the renderer (`cardspec.go:59`).
    pub fn normalized(&self) -> CardSpec {
        let mut out = self.clone();
        match out.details.as_str() {
            CARD_DETAILS_DEFAULT | CARD_DETAILS_TITLE | CARD_DETAILS_EPISODE
            | CARD_DETAILS_SERIES => {}
            _ => out.details = CARD_DETAILS_DEFAULT.to_string(),
        }
        match out.state.as_str() {
            CARD_STATE_DEFAULT | CARD_STATE_TAG | CARD_STATE_SYNOPSIS | CARD_STATE_NONE => {}
            _ => out.state = CARD_STATE_DEFAULT.to_string(),
        }
        out
    }

    /// Whether both lines are the shipped default (`cardspec.go:75`).
    pub fn is_default(&self) -> bool {
        let n = self.normalized();
        n.details == CARD_DETAILS_DEFAULT && n.state == CARD_STATE_DEFAULT
    }

    /// Returns the (details, state) body lines for the given inputs under this
    /// spec (`cardspec.go:147`). The default spec returns
    /// [`build_activity_resolved`]'s lines exactly (no synopsis in the state);
    /// [`CARD_STATE_SYNOPSIS`] renders the synopsis-bearing line instead and
    /// hides the slot when there is no synopsis; every other preset reselects
    /// among the same capped strings. The idle card keeps its shipped lines under
    /// any spec.
    pub fn render(&self, raw_title: &str, a: Option<&Action>, r: &Resolved) -> (String, String) {
        let base = build_activity_resolved(raw_title, a, r);
        if is_idle_title(raw_title) {
            return (base.details, base.state);
        }
        let spec = self.normalized();
        let mut details = base.details;
        let mut state = base.state;
        match spec.details.as_str() {
            CARD_DETAILS_TITLE => {
                // The header title: canonical series when known, else the
                // filename-derived header (base.name), else today's line.
                if !r.series.is_empty() {
                    details = trunc(&r.series, 128);
                } else if !base.name.is_empty() {
                    details = base.name;
                }
            }
            CARD_DETAILS_EPISODE => {
                if !r.episode_name.is_empty() {
                    details = trunc(&r.episode_name, 128);
                }
            }
            CARD_DETAILS_SERIES => {
                if !r.series.is_empty() && r.is_movie && r.year > 0 {
                    details = trunc(&format!("{} ({})", r.series, r.year), 128);
                } else if !r.series.is_empty() {
                    details = trunc(&r.series, 128);
                }
            }
            _ => {}
        }
        match spec.state.as_str() {
            CARD_STATE_TAG => state = episode_tag_only(raw_title, a),
            CARD_STATE_SYNOPSIS => state = synopsis_state(raw_title, a, r),
            CARD_STATE_NONE => state = String::new(),
            _ => {}
        }
        (details, state)
    }
}

/// The small named set per line (`cardspec.go:83`): 4 Details choices and 4
/// State choices. Labels stay short so the owner-drawn preset buttons ellipsize
/// cleanly at any DPI.
pub fn card_presets() -> (Vec<CardPreset>, Vec<CardPreset>) {
    let details = vec![
        CardPreset {
            id: CARD_DETAILS_DEFAULT.to_string(),
            label: "Default".to_string(),
        },
        CardPreset {
            id: CARD_DETAILS_TITLE.to_string(),
            label: "Title".to_string(),
        },
        CardPreset {
            id: CARD_DETAILS_EPISODE.to_string(),
            label: "Episode".to_string(),
        },
        CardPreset {
            id: CARD_DETAILS_SERIES.to_string(),
            label: "Series".to_string(),
        },
    ];
    let states = vec![
        CardPreset {
            id: CARD_STATE_DEFAULT.to_string(),
            label: "Default".to_string(),
        },
        CardPreset {
            id: CARD_STATE_TAG.to_string(),
            label: "Tag".to_string(),
        },
        CardPreset {
            id: CARD_STATE_SYNOPSIS.to_string(),
            label: "Synopsis".to_string(),
        },
        CardPreset {
            id: CARD_STATE_NONE.to_string(),
            label: "Empty".to_string(),
        },
    ];
    (details, states)
}

/// Mirrors [`build_activity`](crate::build_activity)'s idle gate
/// (`cardspec.go:99-107`): the title carries no media name. The idle card
/// ("Browsing"/"Idle") is never restyled by presets.
pub(crate) fn is_idle_title(raw_title: &str) -> bool {
    let raw = raw_title.strip_suffix(SUFFIX).unwrap_or(raw_title);
    raw.trim().is_empty() || raw == APP_NAME
}

/// Renders the filename episode tag with the paused marker path preserved
/// (`cardspec.go:109`): paused with no tag reads "Paused", as
/// [`build_activity`](crate::build_activity) does.
fn episode_tag_only(raw_title: &str, a: Option<&Action>) -> String {
    let raw = raw_title.strip_suffix(SUFFIX).unwrap_or(raw_title);
    let mut tag_raw = String::new();
    if !(raw.trim().is_empty() || raw == APP_NAME) {
        tag_raw = clean(raw).episode_tag;
    }
    let tag_t = rp_text(&tag_raw, 128);
    if a.is_some_and(|x| x.paused) {
        return with_state_time(&tag_t, PAUSED_MARKER);
    }
    tag_t
}
