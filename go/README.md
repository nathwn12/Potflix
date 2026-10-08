# potflix (Go)

Go module `potflix`. Stdlib only, no external modules, no `go.sum`.

- root package `main` -- the Potflix tray app: single-instance lock, hidden
  tray window, `--selftest`, the PotPlayer poll loop, and the Discord IPC
  client. Win32 is reached through `syscall.NewLazyDLL` / `NewLazyProc`; the
  tray icon is embedded with `//go:embed assets/icon.ico`.
- `internal/discord` -- local Discord IPC pipe transport (`SET_ACTIVITY`,
  `READY` gating, reply matching).
- `internal/player` -- PotPlayer window discovery and `WM_USER` queries.
- `internal/presence` -- presence state machine and activity payload builder.
- `internal/titleclean` -- dependency-free port of the repo's Java media-title
  cleaner (`me.potflix.rpc.Main.clean`); see `internal/titleclean/README.md`.
- `cmd/titleclean` -- parity CLI over a corpus; writes a TSV of
  `input / title / display / episodeTag / episodeName`.
- `tools/jdkgen` -- generator for the JDK-11 Unicode tables; see
  `tools/jdkgen/README.md`.

## Status

- Title cleaner: proven. Byte-identical to the Java baseline on the full
  2604-line corpus and holdout (0 diffs); see `../../DECISION.md`.
- Application (M1-M4): landed. The app builds to a single self-contained
  `dist\potflix.exe`, its `--selftest` passes, and it holds a live Discord ack
  (`connected clientId=<redacted>` then `push ack nonce=...`).

| Module | Scope | Status |
|---|---|---|
| M1 | App skeleton: instance lock, hidden-window tray, debug log, client-id resolution, `--selftest` | landed |
| M2 | PotPlayer integration: `EnumWindows`, `WM_USER` time/status queries, presence state machine, activity payload builder, RUN debounce | landed (`internal/player`, `internal/presence`) |
| M3 | Discord transport: local IPC pipe `\\.\pipe\discord-ipc-0..9`, handshake + `SET_ACTIVITY`, `READY` gating, reply matching; poll loop + real clear-on-exit | landed; live ack proven |
| M4 | Cleaner (`clean()`) integrated into `internal/titleclean`; parity vs the Java baseline | landed; corpus + holdout byte-identical (`DECISION.md`) |

## Build and test

The app is built from the repository root by `tools\build.ps1`, which runs from
this directory:

```
go build -trimpath -buildvcs=false -ldflags "-H=windowsgui" -o ..\dist\potflix.exe .
```

`-trimpath` (no absolute checkout paths) and `-buildvcs=false` (no git revision
stamp) make the output reproducible across directories and commits;
`-H=windowsgui` keeps the tray app console-less.

```
go vet ./...
go test ./... -count=1
go build ./cmd/titleclean
```

All run from this directory. No network or external dependencies.

## Windows exe icon

`rsrc_windows_amd64.syso` carries the Explorer icon resource (built from
`..\assets\icon.ico`). `go build` picks up the `*.syso` automatically, so the
build stays offline. Regenerate only when the ico changes (needs network once
for the generator):

```
go install github.com/akavel/rsrc@latest
rsrc -arch amd64 -ico assets\icon.ico -o go\rsrc_windows_amd64.syso
```

(run from the repo root). Do not alter the `.ico` files; the tray loader
(`embed.go` / `tray.go`) is untouched - this only adds the file icon.

The deliberate behaviour deviations from the Java reference (empty asset text
omitted on the wire, clear-on-exit, capped debug log, `READY` handshake
requirement) are recorded in the repository `README.md`.
