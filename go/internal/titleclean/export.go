package titleclean

import "regexp"

// export.go is the package's public surface only; it carries no logic. The
// ported files (clean.go, javacompat.go, jdk11tables.go) and the tests keep
// their original unexported names and are otherwise byte-for-byte the proven
// bake-off port.

// Clean turns a raw media filename into the Netflix-style Cleaned struct. It is
// a faithful port of me.potflix.rpc.Main.clean(String).
func Clean(raw string) Cleaned { return clean(raw) }

// Trunc is the Java-exact Main.trunc(s, n): length is measured in UTF-16 code
// units, the kept prefix is trimmed, and the cut is marked with U+2026. A cut
// through a surrogate pair emits '?' exactly as Java's UTF-8 encoder does.
func Trunc(s string, n int) string { return javaTrunc(s, n) }

// JavaTrim mirrors java.lang.String.trim() (strip code points <= U+0020). The
// parity CLI in cmd/titleclean preprocesses input identically to the Java
// runner, so the Java-exact helper is re-exposed here rather than duplicated.
func JavaTrim(s string) string { return javaTrim(s) }

// JavaDollarReplaceAll emulates Java Matcher.replaceAll for a pattern anchored
// at Java's `$` (compiled here with `\z`): the final line terminator is not
// consumed, but `$` may still match just before it. Used by the parity CLI to
// strip the trailing " - PotPlayer" suffix the same way the Java runner does.
func JavaDollarReplaceAll(re *regexp.Regexp, s, repl string) string {
	return javaDollarReplaceAll(re, s, repl)
}
