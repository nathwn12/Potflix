# titleclean

A dependency-free Go port of the media-title cleaner in this repository:
`me.potflix.rpc.Main.clean(String)` and its helpers (`Main.trunc`,
`stripTags`, `buildDisplay`, the `\b` boundary rule), source
`src/main/java/me/potflix/rpc/Main.java`.

The port is behavioural, not a rewrite: the package keeps the Java semantics
that a naive Go translation drops -- Java's `String.trim`, Java's `\s`
(`[ \t\n\x0B\f\r]`), Java's `$` end-of-input positions before a line
terminator, ASCII-only `CASE_INSENSITIVE` regex folding, `Matcher.replaceAll`
group-substitution rules, UTF-16 length and surrogate handling in `trunc`,
Unicode-aware `\b`, and JDK-11 `Character` classification and case tables.
`javacompat.go` holds the regex/UTF-16 machinery; `jdk11tables.go` holds the
generated Unicode 10 tables; `export.go` is the public surface.

## Parity method

- The golden set was regenerated from the Java source itself (not from
  pre-existing sample output), pinned at commit `540a1d1`.
- Corpus: the 2604-line title list, split 2083 train / 521 holdout. The
  holdout is never used to tune the port.
- Per-line parity is exact output equality against the Java golden: input,
  title, display, episode tag, episode name.
- Two independent reviewers each built their own goldens from the Java
  source and compared the full corpus and holdout; disagreements drove
  repair rounds until the diff reached zero.
- Five distinct mechanisms were repaired before the tables and error paths
  landed; a 20k-input fuzz run followed.

## JDK-11 table provenance

`jdk11tables.go` is generated, not hand-written, and its header says so. It
is produced by `tools/jdkgen/jdkgen.java` run on an installed JDK 11
(Unicode 10) -- the same runtime the Java cleaner ships against -- and
captures `Character.isLetterOrDigit`, `Character.getType ==
NON_SPACING_MARK`, `Character.toUpperCase` (simple), and the full
`toUpperCase` expansions. Go 1.27's own Unicode is newer, so these explicit
tables are required for byte-parity with the Java baseline. See
`tools/jdkgen/README.md`.
