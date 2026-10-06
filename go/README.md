# potflix (Go)

Go module `potflix`. Stdlib only, no external modules.

- `internal/titleclean` -- dependency-free port of the repo's Java media-title
  cleaner (`me.potflix.rpc.Main.clean`). Exposes `Clean`, `Trunc`, and the
  Java-exact helpers; see `internal/titleclean/README.md`.
- `cmd/titleclean` -- parity CLI over a corpus; writes a TSV of
  `input / title / display / episodeTag / episodeName`.
- `tools/jdkgen` -- generator for the JDK-11 Unicode tables; see
  `tools/jdkgen/README.md`.

## Status

- Title cleaner: proven. Byte-identical to the Java baseline on the full
  2604-line corpus and holdout (0 diffs); see `../../DECISION.md`.
- Application (M1-M4): in progress.

## Build and test

```
go vet ./...
go test ./...
go build ./cmd/titleclean
```

All three run from this directory. No network or external dependencies.
