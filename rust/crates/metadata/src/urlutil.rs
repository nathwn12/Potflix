//! `net/url` escaping, hand-rolled.
//!
//! The workspace pins no percent-encoding crate, and the two Go helpers the
//! providers use are fixed and small, so they are transcribed here rather than
//! pulled in.
//!
//! * [`query_escape`] is `url.QueryEscape`: unreserved `A-Za-z0-9-_.~` verbatim,
//!   a space as `+`, everything else `%XX` with UPPERCASE hex. This is the form
//!   TVmaze's `q=`, Jikan's `q=` and Wikipedia's `srsearch=`/`titles=` take.
//! * [`path_escape`] is `url.PathEscape`: `escape(s, encodePathSegment)`, which
//!   keeps the unreserved set plus `:@&=+$` (RFC 3986 sub-delims that carry no
//!   per-segment meaning) and escapes `/`, `;`, `,`, `?`.

/// `url.QueryEscape`.
pub fn query_escape(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for byte in s.as_bytes() {
        match byte {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'~' => {
                out.push(*byte as char);
            }
            b' ' => out.push('+'),
            _ => {
                out.push('%');
                out.push(hex_digit(byte >> 4));
                out.push(hex_digit(byte & 0x0f));
            }
        }
    }
    out
}

/// `url.PathEscape`.
pub fn path_escape(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for byte in s.as_bytes() {
        match byte {
            b'A'..=b'Z'
            | b'a'..=b'z'
            | b'0'..=b'9'
            | b'-'
            | b'_'
            | b'.'
            | b'~'
            | b':'
            | b'@'
            | b'&'
            | b'='
            | b'+'
            | b'$' => out.push(*byte as char),
            _ => {
                out.push('%');
                out.push(hex_digit(byte >> 4));
                out.push(hex_digit(byte & 0x0f));
            }
        }
    }
    out
}

fn hex_digit(nibble: u8) -> char {
    match nibble {
        0..=9 => (b'0' + nibble) as char,
        _ => (b'A' + (nibble - 10)) as char,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn query_escape_matches_go() {
        assert_eq!(query_escape("breaking bad"), "breaking+bad");
        assert_eq!(query_escape("Breaking Bad"), "Breaking+Bad");
        assert_eq!(query_escape("Re:ZERO"), "Re%3AZERO");
        assert_eq!(query_escape("a-b_c.d~e"), "a-b_c.d~e");
        assert_eq!(query_escape("dune film"), "dune+film");
    }

    #[test]
    fn path_escape_matches_go() {
        assert_eq!(path_escape("Dune_(2021_film)"), "Dune_%282021_film%29");
        assert_eq!(path_escape("A/B"), "A%2FB");
        assert_eq!(path_escape("a:b@c&d=e+f$g"), "a:b@c&d=e+f$g");
    }
}
