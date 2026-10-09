# potflix (Go)

Go module `potflix`. Stdlib only, no external modules, no `go.sum`.
(The only network use is keyless metadata lookups over stdlib `net/http`;
see "Metadata lookup" below.)

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
- `internal/metadata` -- canonical series/episode titles from free, keyless
  sources (TVmaze, AniList, Jikan, iTunes) with a permanent on-disk cache;
  the poll loop uses the non-blocking `Resolve`, the CLI the blocking
  `LookupNow` (`Options.Offline` serves cache-only).
- `cmd/metacheck` -- proof/diagnostic CLI over one raw title; prints
  `input / series / season / episode / title / source / cache / dir`.
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

All run from this directory. No external modules; the only network use is
the app's keyless metadata lookups at runtime (see "Metadata lookup" below) -
`go vet` / `go test` / the `titleclean` build itself need no network.

## Metadata lookup

The poll loop resolves the clean card's canonical series and episode titles
from free, keyless sources over stdlib `net/http`: TVmaze for TV, AniList +
Jikan for anime, iTunes for movies. Each season is fetched once and cached
permanently as one JSON file per (show, season) under
`%LOCALAPPDATA%\Potflix\meta` (exe-dir fallback); the poll loop itself never
blocks - a cache miss fires one background fetch per key and the card falls
back to the clean title until it lands. Offline-safe: with no cache entry and
no network the card is exactly today's clean card, and `Options.Offline`
serves cache-only. No credentials, no account, nothing personal: the cache
holds public episode titles only, lives outside the repo, and is never
committed. Diagnose with `go run ./cmd/metacheck [-offline] [-dir <cacheDir>]
<rawTitle>` (exit 0 when resolved, 1 when not).

## Windows exe icon

`rsrc_windows_amd64.syso` carries the Explorer icon resource (built from
`..\assets\icon.ico`). `go build` picks up the `*.syso` automatically, so the
build needs no extra downloads. Regenerate only when the ico changes (needs network once
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
