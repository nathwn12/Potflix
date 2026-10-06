# DECISION: media-title cleaner port

**Verdict: Go.**

Three ports of the repository's Java title cleaner (`me.potflix.rpc.Main.clean`)
were benchmarked against one regenerated Java golden set. All figures below are
the measured bake-off results; they are not re-derived here.

## Baseline

- Java golden regenerated from source, pinned at commit `540a1d1`.
- 2604 rows, split 2083 train / 521 holdout.
- Golden sha256: `CCDB3E76…90AE`.

## Results

### Go

- Diff vs golden: 0 diffs on every set (corpus and holdout).
- Repairs before green: five mechanisms repaired, then JDK-11 Unicode tables,
  error paths, and a 20k-input fuzz run.
- Binary: 2,190,848 bytes stripped (3,248,128 bytes plain).
- Timing: 101.6 / 99.7 / 95.7 ms; best 95.7 ms.
- LOC: `clean.go` 422, `javacompat.go` 336, `jdk11tables.go` 251,
  `main.go` 111, tests 305.
- Stdlib only.

### C#

- Diff vs golden: 0 on corpus and holdout, but 24 adversarial cases remained
  open, then 17 of 161 reviewer rows.
- `ECMAScript-ASCII` folding and JB-exactness were both refuted.
- Binary: 162,304 bytes framework-dependent vs 73,537,251 bytes
  self-contained.
- LOC: 654. Target: `net10.0`.

### Java (incumbent)

- Jar: 3,349,076 bytes, with a mutable v1 download.

## Verdict

Go. It is the only port that is diff-clean across the corpus, the holdout,
and the adversarial set, with no external dependencies and predictable
runtime.

## Residuals

- Unicode behaviour is pinned to JDK-11 / Unicode-10 tables; a future JDK
  would move the target.
- Astral uppercase drift is unreachable through `clean()` but would diverge
  if that path were exercised.
- The Go writer is strict about UTF-8 where Java's writer would replace
  malformed bytes; the difference only shows on inputs the Java baseline
  cannot emit.
