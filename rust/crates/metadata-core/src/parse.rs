//! `metadata::parse` -- mirrors `go\internal\metadata\parse.go`.
//!
//! Classifies a cleaned title (plus titleclean's episode tag/name) into a
//! [`Query`]. The PRECEDENCE ORDER is the spec: OP/ED theme first, then a
//! strong SxxEyy tag, then the "Episode N+M" range (FIRST number), then a movie
//! marker, then episode>0, then extras, then no-letter/year-only. Years
//! 1900-2100 are never episodes.
//!
//! # Pattern port
//!
//! The workspace forbids a regex crate, so every Go pattern is transcribed into
//! the subset `titleclean::Regex` (the port's own leftmost-first matcher)
//! understands and compiled through `titleclean::javacompat::port_pattern`,
//! which applies the rewrites that matcher needs: `(?i)` expanded to ASCII
//! letter classes, `^`/`$` to `\A`/`\z`. Two Go constructs have no equivalent
//! and are handled explicitly:
//!
//!   * `\s` -- Go's `\s` is `[\t\n\f\r ]`; the class is spelled out inline in
//!     every transcribed pattern (the matcher has no `\s`);
//!   * `\b` -- Go's ASCII word boundary; re-applied as an explicit check by
//!     [`word_bounded_match`] instead of an assertion. The same helper serves
//!     `context.rs`, which guards its extras pattern the same way.
//!
//! Case folding is ASCII-only, matching `port_pattern` and titleclean's
//! Java-exact convention; Go's `(?i)` also folds a handful of non-ASCII runes
//! (U+017F, U+212A) that no real filename carries.

use std::sync::LazyLock;

use titleclean::Regex;
use titleclean::javacompat::port_pattern;

use crate::types::{Kind, Query};

/// Mirrors `player.Suffix` (" - PotPlayer"). Duplicated in Go so the metadata
/// package imports only stdlib plus titleclean.
pub(crate) const POT_PLAYER_SUFFIX: &str = " - PotPlayer";

/// Compiles a transcribed pattern. `folded` is true exactly for the patterns
/// Go writes with a leading `(?i)`.
fn re(src: &str, folded: bool) -> Regex {
    Regex::new(&port_pattern(src, folded))
}

static SEASON_EPISODE_RE: LazyLock<Regex> = LazyLock::new(|| {
    re(
        r"(?i)^S[\t\n\f\r ]*(\d{1,4})[\t\n\f\r ]*E[\t\n\f\r ]*(\d{1,3})(?:[\t\n\f\r ]*-[\t\n\f\r ]*\d{1,3})?$",
        true,
    )
});
static EPISODE_ONLY_RE: LazyLock<Regex> =
    LazyLock::new(|| re(r"(?i)^E[\t\n\f\r ]*(\d{1,4})$", true));
static YEAR_SUFFIX_RE: LazyLock<Regex> =
    LazyLock::new(|| re(r"^(.*)[\t\n\f\r ]+\((\d{4})\)[\t\n\f\r ]*$", false));
/// `themeRe` spots OP/ED theme tokens (OP 1, ED 01v2, OP1, Show - OP - 001) in a
/// cleaned title or episode tag. The token must stand alone with a number, so
/// "Speed", "Hope", "Top", "Opened", "Operation" never match.
static THEME_RE: LazyLock<Regex> = LazyLock::new(|| {
    re(
        r"(?i)(?:^|[\t\n\f\r ._\-;\[\(])(?:NC)?(OP|ED)[\t\n\f\r ._\-]*(\d{1,3})(?:v\d+)?(?:$|[\t\n\f\r ._\-;\]\)])",
        true,
    )
});
/// `ncThemeRe` spots bare creditless markers (NCOP, NCED) with no number. The
/// NC prefix makes them unambiguous, so unlike bare OP/ED they need no digits
/// (e.g. "NCOP 1.1", whose dotted version defeats the tagger).
static NC_THEME_RE: LazyLock<Regex> = LazyLock::new(|| {
    re(
        r"(?i)(?:^|[\t\n\f\r ._\-;\[\(])(?:NC)(OP|ED)(?:$|[\t\n\f\r ._\-;\]\)])",
        true,
    )
});
/// `episodeRangeRe` re-scans a cleaned title for "Episode 53+54" ranges.
/// titleclean splits those at the wrong "+", so the tag is bogus (E2) while the
/// true range stays in the title: take the FIRST number.
static EPISODE_RANGE_RE: LazyLock<Regex> = LazyLock::new(|| {
    re(
        r"(?i)(?:^|[\t\n\f\r ._\-;:\[])(?:Episode|Ep)\.?[\t\n\f\r ]*(\d{1,4})[\t\n\f\r ]*\+[\t\n\f\r ]*\d{1,4}",
        true,
    )
});
/// `extraRe` lists filename-level extras: KindUnknown, never fetched. Phrase
/// stems only (never a single common word). The Go pattern wraps the
/// alternation in `\b...\b`; those boundaries are applied by
/// [`word_bounded_match`]. Guarded by TestParseExtrasNoRealTitleCollision: no
/// real title in the same cache may match.
static EXTRA_RE: LazyLock<Regex> = LazyLock::new(|| {
    re(
        r"(?i)(deleted scenes?|extended[\t\n\f\r ]+scenes?|trailers?|gallery|alternate endings?|interviews?|behind the scenes|behind[\t\n\f\r ]+the[\t\n\f\r ]+stor(?:y|ies)|featurettes?|music videos?|samples?|extras?|gag[\t\n\f\r ]+reels?|blooper[\t\n\f\r ]+reels?|outtakes?|tv[\t\n\f\r ]+spots?|making[\t\n\f\r ]+of|meet[\t\n\f\r ]+the[\t\n\f\r ]+cast|visual[\t\n\f\r ]+effects|in[\t\n\f\r ]+search[\t\n\f\r ]+of|how[\t\n\f\r ]+to[\t\n\f\r ]+survive|previz|production[\t\n\f\r ]+design|time[\t\n\f\r \-]*lapse[\t\n\f\r ]+videos?|secret[\t\n\f\r ]+auditions?)",
        true,
    )
});
static YEAR_ONLY_RE: LazyLock<Regex> = LazyLock::new(|| re(r"^(\d{4})$", false));
/// `movieMarkerRe` spots a movie marker at the END of a cleaned title: an
/// optional "the", the word "Movie", and an optional trailing number.
/// Deliberately narrow: the marker must be the title's last word.
static MOVIE_MARKER_RE: LazyLock<Regex> = LazyLock::new(|| {
    re(
        r"(?i)^(.*?)[\t\n\f\r ._\-]+(?:the[\t\n\f\r ._\-]+)?movie(?:[\t\n\f\r ._\-]+\d{1,3})?$",
        true,
    )
});

/// Go's `lettersRe` (`[A-Za-z]`): does the text carry a usable ASCII letter?
pub(crate) fn has_ascii_letter(s: &str) -> bool {
    s.bytes().any(|b| b.is_ascii_alphabetic())
}

/// Go's `\b`: an ASCII word character (`\w` is `[0-9A-Za-z_]` in Go).
fn is_word_char(c: char) -> bool {
    c.is_ascii_alphanumeric() || c == '_'
}

/// Go's `\b(inner)\b` without a boundary assertion: the leftmost match of
/// `inner` whose first and last characters both sit on an ASCII word boundary.
///
/// On a boundary failure Go backtracks and retries every later start position,
/// so a rejected match advances by ONE code point rather than to its end -- no
/// start position is ever skipped. For the two patterns guarded here (`extraRe`,
/// `extrasWordRe`) no alternative is a strict prefix of another, so a rejection
/// at a start position cannot hide an acceptable match at that same position.
pub(crate) fn word_bounded_match(re: &Regex, hay: &str) -> bool {
    let mut from = 0usize;
    loop {
        let Some((a, b)) = re.find(&hay[from..]) else {
            return false;
        };
        let start = from + a;
        let end = from + b;
        let start_ok = start == 0 || !hay[..start].chars().next_back().is_some_and(is_word_char);
        let end_ok =
            end == hay.len() || hay[end..].chars().next().is_some_and(|c| !is_word_char(c));
        if start_ok && end_ok {
            return true;
        }
        let Some(c) = hay[start..].chars().next() else {
            return false;
        };
        from = start + c.len_utf8();
    }
}

/// `extraRe` against a cleaned title or path component.
pub(crate) fn extra_re_matches(s: &str) -> bool {
    word_bounded_match(&EXTRA_RE, s)
}

/// `isTheme` reports whether a cleaned title or episode tag is an OP/ED theme
/// (numbered tokens like "OP 1"/"ED 01v2", or bare NCOP/NCED).
pub(crate) fn is_theme(title: &str, tag: &str) -> bool {
    THEME_RE.is_match(title)
        || THEME_RE.is_match(tag)
        || NC_THEME_RE.is_match(title)
        || NC_THEME_RE.is_match(tag)
}

/// `ParseRaw` strips the " - PotPlayer" suffix, runs `titleclean::clean`, then
/// [`parse`].
pub fn parse_raw(raw_title: &str) -> Query {
    let s = raw_title
        .strip_suffix(POT_PLAYER_SUFFIX)
        .unwrap_or(raw_title);
    let c = titleclean::clean(s);
    parse(&c.title, &c.episode_tag, &c.episode_name)
}

/// `Parse` builds a [`Query`] from titleclean fields. `episode_name` is used
/// only to compose a movie's subtitle back onto its base title; the title is
/// otherwise re-scanned only for the "Episode N+M" ranges titleclean
/// mis-splits.
///
/// Classification follows the synthetic library's validated rules:
///
///   * OP/ED theme tokens are themes, never episodes (KindUnknown, so the
///     resolver never fetches for them); themes outrank every episode form.
///   * An episode signal must be contextual: SxxEyy, E##, Episode N, EP N, a
///     dash-number absolute of 2-3 digits, or a single-digit " - N" clearly
///     dash-separated. Bare sequel/version digits (Harbor Run 2, Trucks 3,
///     3.0+1.0), quality tokens (1080p, 5.1, 10bit, x265) and years never
///     count; 19xx/20xx is a year, never an episode.
///   * Ranges use the first number (S02E12-E13 -> 12, Episode 53+54 -> 53).
///   * Extras, Sample, or no usable title are KindUnknown.
pub fn parse(title: &str, episode_tag: &str, episode_name: &str) -> Query {
    let mut t = title.trim().to_string();
    let mut year = 0;
    if let Some(caps) = YEAR_SUFFIX_RE.captures(&t) {
        let y = atoi(group(&caps, 2));
        if is_year_num(y) {
            year = y;
            t = group(&caps, 1).trim().to_string();
        }
    }
    // OP/ED theme outranks every episode form.
    if is_theme(&t, episode_tag.trim()) {
        return Query {
            kind: Kind::UNKNOWN,
            title: t,
            year,
            ..Query::default()
        };
    }
    let mut q = Query {
        kind: Kind::MOVIE,
        title: t,
        year,
        ..Query::default()
    };
    let tag = episode_tag.trim();
    let mut strong = false;
    if !tag.is_empty() {
        if SEASON_EPISODE_RE.is_match(tag) {
            let caps = SEASON_EPISODE_RE.captures(tag).expect("is_match agreed");
            q.season = atoi64(group(&caps, 1));
            q.episode = atoi(group(&caps, 2));
            strong = true;
        } else if EPISODE_ONLY_RE.is_match(tag) {
            // A 19xx/20xx "absolute" is a year, never an episode.
            let caps = EPISODE_ONLY_RE.captures(tag).expect("is_match agreed");
            let n = atoi(group(&caps, 1));
            if !is_year_num(n) {
                q.episode = n;
                q.absolute = true;
            }
        }
        // Unknown tag text (e.g. "Special") carries no signal.
    }
    // "Episode 53+54" ranges: titleclean splits those at the wrong "+", leaving
    // a bogus weak tag (E2) while the true range stays in the title. The FIRST
    // number wins over any weak absolute tag, so this runs before the tag
    // result is accepted.
    if !strong
        && let Some((n, head)) = range_episode(&q.title)
        && n > 0
    {
        q.season = 0;
        q.episode = n;
        q.absolute = true;
        q.kind = Kind::SERIES;
        if !head.is_empty() {
            q.title = head;
        }
        return q;
    }
    // A movie marker outranks the weak absolute tag titleclean's ALT_EP split
    // left behind (see the Go comment for the Dragon Ball Super case). The
    // marker only counts when something follows it -- its number or the
    // subtitle -- so a title that merely ends in the word (e.g. "The Lego
    // Movie" with nothing after it) is left exactly as parsed. Extras keep
    // their skip, and a strong SxxEyy tag always wins.
    if !strong
        && !extra_re_matches(&q.title)
        && let Some(base) = movie_marker(&q.title)
        && (q.season > 0 || q.episode > 0 || !episode_name.trim().is_empty())
    {
        let sub = episode_name.trim();
        let base = if sub.is_empty() {
            base
        } else {
            format!("{base}: {sub}")
        };
        return Query {
            kind: Kind::MOVIE,
            title: base,
            year,
            ..Query::default()
        };
    }
    if q.episode > 0 || q.season > 0 {
        q.kind = Kind::SERIES;
        return q;
    }
    if extra_re_matches(&q.title) {
        return Query {
            kind: Kind::UNKNOWN,
            title: q.title,
            year,
            ..Query::default()
        };
    }
    if q.title.is_empty() || !has_ascii_letter(&q.title) {
        if let Some(caps) = YEAR_ONLY_RE.captures(&q.title) {
            let y = atoi(group(&caps, 1));
            if is_year_num(y) {
                return Query {
                    kind: Kind::MOVIE,
                    title: q.title,
                    year: y,
                    ..Query::default()
                };
            }
        }
        return Query {
            kind: Kind::UNKNOWN,
            title: q.title,
            year,
            ..Query::default()
        };
    }
    q
}

/// `movieMarker` returns the title base before a trailing movie marker
/// ("Show Movie", "Show Movie 01", "Show the Movie 01" -> "Show"). `None` when
/// there is no marker or nothing usable would remain as the base.
fn movie_marker(t: &str) -> Option<String> {
    let caps = MOVIE_MARKER_RE.captures(t)?;
    let base = group(&caps, 1).trim();
    if base.is_empty() || !has_ascii_letter(base) {
        return None;
    }
    Some(base.to_string())
}

/// `rangeEpisode` re-scans a cleaned title for an "Episode N+M" range and
/// returns its FIRST number plus the show head before it (`""` when unusable).
/// `None` when there is no range or the number is year-like.
fn range_episode(t: &str) -> Option<(i32, String)> {
    let (mstart, _) = EPISODE_RANGE_RE.find(t)?;
    let caps = EPISODE_RANGE_RE.captures(t).expect("find agreed");
    let n = atoi(group(&caps, 1));
    if n <= 0 || is_year_num(n) {
        return None;
    }
    let head = t[..mstart]
        .trim()
        .trim_end_matches([' ', '\t', '.', '_', '-', ';', ':', '(', '[']);
    Some((n, head.to_string()))
}

/// `isYearNum` reports whether n looks like a release year (hence never an
/// episode number).
pub(crate) fn is_year_num(n: i32) -> bool {
    (1900..=2100).contains(&n)
}

/// Go's `atoi` where the destination is Go's 64-bit `int` (the season fields):
/// `strconv.Atoi` (`go\internal\metadata\parse.go:200`) drops the error, so a
/// non-numeric capture is 0 and a digits-only capture above `i64::MAX` keeps
/// `ParseInt`'s clamped `MaxInt64` instead of collapsing to 0.
pub(crate) fn atoi64(s: &str) -> i64 {
    match s.parse::<i64>() {
        Ok(n) => n,
        Err(_) if !s.is_empty() && s.bytes().all(|b| b.is_ascii_digit()) => i64::MAX,
        Err(_) => 0,
    }
}

/// Go's `atoi`: a non-numeric string is 0. Every caller slices a `\d` capture
/// that is bounded by the pattern (`\d{1,4}` etc.), so the 32-bit parse holds.
fn atoi(s: &str) -> i32 {
    s.parse().unwrap_or(0)
}

/// A capture group's text, `""` when it did not participate (Go's
/// non-participating group is the empty slice only where the port needs it).
pub(crate) fn group<'a>(caps: &[Option<&'a str>], i: usize) -> &'a str {
    caps.get(i).copied().flatten().unwrap_or("")
}

/// `dateYear` parses the year from a date prefix like "2008-01-20" or
/// "2010-07-16T07:00:00Z". It returns 0 when no sane year leads.
///
/// Go slices four BYTES and runs `strconv.Atoi`, which rejects a non-numeric
/// byte (including a split UTF-8 rune) and any sign that would land in range,
/// so the port reads the same four bytes.
pub fn date_year(s: &str) -> i32 {
    let s = s.trim();
    let b = s.as_bytes();
    if b.len() < 4 {
        return 0;
    }
    let mut y = 0i32;
    for &c in &b[..4] {
        if !c.is_ascii_digit() {
            return 0;
        }
        y = y * 10 + i32::from(c - b'0');
    }
    if is_year_num(y) { y } else { 0 }
}
