# potflix (Go)

Go module `potflix`. Stdlib only, no external modules, no `go.sum`.
(The only network use is keyless metadata lookups over stdlib `net/http`;
see "Metadata lookup" below.)

- root package `main` -- the Potflix tray app: single-instance lock, hidden
  tray window, `--selftest`, the PotPlayer poll loop, and the Discord IPC
  client. Win32 is reached through `syscall.NewLazyDLL` / `NewLazyProc`; the
  tray icon is embedded with `//go:embed assets/icon.ico`.
  `config.go` is the settings store (`%LOCALAPPDATA%\Potflix\config.json`:
  DIRs, scan speed, startup flag); `window.go` +
  `settings_sections.go` are the tray-resident settings window (folders
  list, Scan / Refresh, library export/import/clear, startup,
  scan-speed segments). Normal launch shows the window; `--selftest` stays
  headless.
- `internal/discord` -- local Discord IPC pipe transport (`SET_ACTIVITY`,
  `READY` gating, reply matching).
- `internal/player` -- PotPlayer window discovery and `WM_USER` queries.
- `internal/presence` -- presence state machine and activity payload builder.
- `internal/titleclean` -- dependency-free port of the repo's Java media-title
  cleaner (`me.potflix.rpc.Main.clean`); see `internal/titleclean/README.md`.
- `internal/metadata` -- canonical series/episode titles from free, keyless
  sources (TVmaze, AniList, Jikan, iTunes) with a permanent on-disk cache;
  the poll loop uses the non-blocking `Resolve`, the CLI the blocking
  `LookupNow` (`Options.Offline` serves cache-only); `ratelimit.go` is
  the per-host fetch-spacing limiter (Gentle 4x / Balanced 2x / Turbo 1x
  the published intervals; unknown hosts ungated).
- `internal/library` -- the shared walk + resolve loop behind the tray
  window's Scan / Refresh and `metawarm`. Plain `Scan` is incremental
  (cached entries served from disk) and never consults the folder index;
  `force=true` (Refresh) bypasses and rewrites every entry; `scanindex.go`
  is the per-directory skip index (tier-1 names hash, tier-2 fingerprint,
  per-key cache guard) used through `ScanWithOptions` / `CollectIndexed` /
  `RecordScan`. The window builds both buttons' options with `UseIndex` on
  (`window.go` `buildScanOptions`: `IndexPath` = `ScanIndexPath()`,
  `CacheDir` = `MetadataDir()`, `Force` per button), so its `Scan` skips
  unchanged folders (no re-walk, no re-parse) while its `Refresh` walks
  everything and re-fetches every cached title, rewriting index records
  either way. Benchmark before/after: `internal/library/BENCH.md`.
- `internal/bundle` -- metadata-only cache bundles: `Export` (whole cache
  to one JSON envelope), `Import` (fill-missing-only merge), `Count`,
  `Clear` (cache files + `.tmp-` orphans only, never recursive). Never
  imports the metadata package; relies only on the documented on-disk
  shape (one `<16-hex>.json` file per key).
- `cmd/metacheck` -- proof/diagnostic CLI over one raw title; prints
  `input / series / season / episode / title / source / cache / dir`.
  Unchanged this session.
- `cmd/metawarm` -- cache pre-warmer over a library walk; prints a
  per-file report plus a summary ending in `folders: N scanned, M skipped`.
  Flags: `-roots` (required), `-cache`, `-dry`, `-offline`, `-limit`,
  `-concurrency`, `-gate <dur>` (extra global fetch spacing, `0` = per-host
  limiter only), `-index <file>` (opt-in folder skips via the scan index;
  empty = full walk). Its usage line is the source of truth.
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
- This session (on disk; human-facing details in the repo `README.md`):
  settings window (`window.go`, `settings_sections.go`, `config.go`),
  `internal/bundle`, `internal/library` (scan + `scanindex.go`), per-host
  limiter + scan profiles (`ratelimit.go`), tray `Open Potflix` item + show-on-launch
  (`tray.go`, `main.go`), `metawarm -index` / `-gate`.

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

Library/metadata hot-path benchmarks (no live cache dir, no real network;
fixed seeds):

```
go test -run '^$' -bench . -benchmem -count=3 ./internal/metadata/ ./internal/library/
```

Before/after table: `internal/library/BENCH.md`; raw `count=3` tails in the
gitignored `bench-baseline.txt`.

## Metadata lookup

The poll loop resolves the clean card's canonical series and episode titles
from free, keyless sources over stdlib `net/http`: TVmaze for TV, AniList +
Jikan for anime, iTunes for movies. Found titles are fetched once and cached
permanently as one JSON file per (show, season) under
`%LOCALAPPDATA%\Potflix\meta` (exe-dir fallback); not-found titles are cached
as negative entries for 30 days under the same key filename, so a later scan
costs zero network for either, and `Refresh` (force) bypasses and re-attempts.
The poll loop itself never
blocks - a cache miss fires one background fetch per key and the card falls
back to the clean title until it lands. Offline-safe: with no cache entry and
no network the card is exactly today's clean card, and `Options.Offline`
serves cache-only. No credentials, no account, nothing personal: the cache
holds public episode titles only, lives outside the repo, and is never
committed. Diagnose with `go run ./cmd/metacheck [-offline] [-dir <cacheDir>]
<rawTitle>` (exit 0 when resolved, 1 when not).

Fetches are spaced by the per-host limiter (`ratelimit.go`): Turbo 1x (default) /
Balanced 2x / Gentle 4x the published provider intervals, switched live from
Settings with no restart.

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

The deliberate behaviour deviations from the Java reference (window on
launch, folder scan index, export/import bundle, empty
asset text omitted on the wire, clear-on-exit, capped debug log, `READY`
handshake requirement) are recorded in the repository `README.md`.
