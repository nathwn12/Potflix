//! `repair` -- mirrors the Go package `go\internal\repair`.
//!
//! `needs-repair.json` + `needs-repair.txt` for the media files that resolved to
//! nothing usable, written into `<dir of index>\inventory` (temp + rename, never
//! a half report), plus the GitHub issue path: `gh issue create` when the GitHub
//! CLI is on PATH, else a prefilled new-issue URL. The published body carries the
//! app version, timestamps, totals and, per entry, reason + basename + attempts +
//! a sanitized error -- NEVER a local path
//! (`TestIssueBodyNeverCarriesALocalPath`); the local report keeps the full path
//! on purpose. Go files: `report.go`, `issue.go`, `sanitize.go`.
//!
//! Two audiences, two forms of the same data:
//!
//!   * The LOCAL report ([`report::write_report`]) never leaves the machine. The
//!     machine form keeps [`report::Entry::path`] so the repair step can open the
//!     file; the human form leads with that path.
//!   * The PUBLISHED form ([`report::issue_body`]) is sanitized.
//!     [`report::Entry::path`] is never written into it, the name is collapsed to
//!     its basename, and every free-text field runs through
//!     [`sanitize::sanitize_error`]. The published issue must never carry a local
//!     path, a drive letter, a UNC path, the Windows username, an environment
//!     value, the Discord client id or a token.
//!
//! Workspace seams, ported 1:1 from the Go files:
//!
//!   * [`sanitize`] -- the published-text filter and its byte-level path grammar.
//!     The pipeline ORDER is the contract; see the module doc.
//!   * [`report`] -- the report types, the staged (temp + rename) writer, the
//!     reader, the issue title and the markdown body.
//!   * [`issue`] -- argv, the prefilled fallback URL, `first_url`, the `gh` seams.
//!   * [`time`] -- the report's timestamp: RFC3339 in UTC, hand-rolled because the
//!     workspace pins no date/time crate (`Cargo.toml` dependency policy).

pub mod issue;
pub mod report;
pub mod sanitize;
pub mod time;

#[cfg(test)]
pub(crate) mod test_support;

use std::fmt;

/// The crate error: a message plus, when there is one, the underlying cause's
/// text.
///
/// Go wrote these as `fmt.Errorf("repair: ...: %w", err)`. [`Error::cause_text`]
/// is the `%w` chain's reach, so a caller (and the tests) can assert the cause
/// the way `errors.Is` did.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Error {
    message: String,
    cause: Option<String>,
}

impl Error {
    /// A failure with no underlying cause.
    pub fn new(message: impl Into<String>) -> Error {
        Error {
            message: message.into(),
            cause: None,
        }
    }

    /// A failure wrapping another one, keeping the cause's text.
    pub fn caused_by(message: impl Into<String>, cause: impl fmt::Display) -> Error {
        Error {
            message: message.into(),
            cause: Some(cause.to_string()),
        }
    }

    /// The message without the cause.
    pub fn message(&self) -> &str {
        &self.message
    }

    /// The wrapped failure's text, if there was one.
    pub fn cause_text(&self) -> Option<&str> {
        self.cause.as_deref()
    }
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match &self.cause {
            Some(cause) => write!(f, "{}: {cause}", self.message),
            None => f.write_str(&self.message),
        }
    }
}

impl std::error::Error for Error {}

impl From<std::io::Error> for Error {
    fn from(e: std::io::Error) -> Error {
        Error::caused_by("repair: io", e)
    }
}

/// `Result` with this crate's [`Error`].
pub type Result<T> = std::result::Result<T, Error>;
