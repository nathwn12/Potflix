# potflix (Go) — FROZEN REFERENCE

> **Frozen.** This tree is the retired-at-merge reference implementation of the
> Rust rewrite under `rust\`. The Rust title cleaner is proven byte-identical to
> this tree's cleaner, and the committed oracle `rust\parity-golden.sha256`
> records that contract. The tree is **not edited** and **not built by the Rust
> gate** (`tools\check.ps1`); its own build (`tools\build.ps1`, the `go build`
> lines below) is retained only so the reference can still be reproduced. New
> work happens in `rust\`.

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
  list, Scan / Refresh, the live `indexed N of M files | metadata K of J`
  line with its `- catching up` state, library export/import,
  `Clear metadata` / `Clear index`, `Report`, startup, scan-speed
  segments). `Report` files the needs-repair inventory as a GitHub issue
  through `internal/repair`; `Clear metadata` empties the metadata cache
  only (the file index survives) while `Clear index` drops the index and
  forces a re-index. Normal launch shows the window; `--selftest` stays
  headless.
- `internal/discord` -- local Discord IPC pipe transport (`SET_ACTIVITY`,
  `READY` gating, reply matching).
- `internal/player` -- PotPlayer window discovery and `WM_USER` queries.
- `internal/presence` -- presence state machine and activity payload builder.
- `internal/titleclean` -- dependency-free port of the repo's Java media-title
  cleaner (`me.potflix.rpc.Main.clean`); see `internal/titleclean/README.md`.
- `internal/store` -- the crash-safe single-file key/value store both the
  library index and the metadata cache are built on. A 24-byte header
  (magic, format version, header CRC) then one `tag | len | crc32 | payload`
  frame per mutation (put / delete-tombstone); `Open` replays sequentially,
  truncates a torn tail back to the last valid frame instead of erroring, and
  `Commit` fsyncs; `Compact` writes a temp file, syncs it, and renames it over
  the original. On Windows the file opens through `syscall.CreateFile` with
  `FILE_SHARE_READ|FILE_SHARE_WRITE|FILE_SHARE_DELETE`, so another party's
  delete never fails on this handle: reads keep serving memory and the
  durability claims (`Commit`, `Close`, a mutation under `SyncOnCommit`) report
  `ErrRemoved` until `Compact` writes a fresh snapshot. Authoritative format
  description: the package doc in `store.go`.
- `internal/repair` -- `needs-repair.json` + `needs-repair.txt` for the media
  files that resolved to nothing usable, written into `<dir of
  index>\inventory` (temp + rename, never a half report), plus the GitHub issue
  path: `gh issue create` when the GitHub CLI is on PATH, else a prefilled
  new-issue URL. The published body carries the app version, timestamps,
  totals and, per entry, reason + basename + attempts + a sanitized error -
  never a local path (`TestIssueBodyNeverCarriesALocalPath`); the local report
  keeps the full path on purpose.
- `internal/metadata` -- canonical series/episode titles from free, keyless
  sources (TVmaze, AniList, Jikan, iTunes, Wikipedia) with a permanent on-disk cache;
  the poll loop uses the non-blocking `Resolve`, the CLI the blocking
  `LookupNow` (`Options.Offline` serves cache-only); `ratelimit.go` is
  the per-host fetch-spacing limiter (Gentle 4x / Balanced 2x / Turbo 1x
  the published intervals; unknown hosts ungated). The cache is one store file
  per directory - `<dir>.idx`, so `%LOCALAPPDATA%\Potflix\meta.idx` for the
  default dir - behind one handle held for the process lifetime, so a read is a
  map lookup. There is no per-entry size cap any more (the retired reader
  refused any file over 64 KiB, which made four real entries of 96 KB - 232 KB
  permanently read as "not cached"); a stored record is bounded only by the
  store's 1 MiB value cap. `migrateLegacyCache` imports the retired per-key
  `meta\*.json` directory once, when the store is empty, and keeps that
  directory as the recovery copy; `ClearCache(dir)` is the only code that
  removes it.
- `internal/library` -- the shared walk + diff loop behind the tray
  window's Scan / Refresh, the background metadata catch-up, and `metawarm`.
  The index is the store-backed `library.idx` beside the configured index path
  (`index.go`: one record per video file - path, name, type, size, mtime,
  parsed title/kind/season/episode, metadata key, state, attempts, last error,
  next try - plus a rollup per folder), not the JSON file it replaces; an
  indexed `Scan` is enumerate + a `(size, mtime)` diff, so unchanged files keep
  their stored record and are never re-parsed, vanished files are pruned, and
  `Scan` performs zero network fetches. `catchup.go` is the metadata work:
  `StartCatchUp` (called once at launch from `main.go`, and again by a scan)
  resolves whatever the index still owes, 3 lookups at a time, resumable and
  idempotent because the pending set is the index's own state; a key that stays
  unresolved after 3 attempts parks its files as `needs_repair` and is retried
  only after a 6-hour cooldown. Migration: the first open of an empty store
  imports the legacy `scanindex.json` rollups (per-file data does not exist
  there, so those folders re-walk once) and renames it to
  `scanindex.json.bak` - never deleted. One process-wide index per store path,
  with its records in memory: the file is opened only for a commit and closed
  again, so a hard kill loses at most the staged, uncommitted change and the
  file is never held locked; `ClearStoreIndex` is what the window's `Clear
  index` calls (the legacy path is not the index any more).
  Plain `Scan` is incremental and never consults the folder index; `force=true`
  (Refresh) bypasses and rewrites every entry, re-parses every file, and
  re-resolves from the providers; `scanindex.go` is the folder tier (tier-1
  names hash, tier-2 fingerprint, rollups) used through `ScanWithOptions` /
  `CollectIndexed` / `RecordScan`. The window builds both buttons' options with
  `UseIndex` on (`window.go` `buildScanOptions`: `IndexPath` =
  `ScanIndexPath()`, `CacheDir` = `MetadataDir()`, `Force` per button). The
  window's scan progress is phase-aware: the `indexed N of M files | metadata
  K of J` pair leads once a file total is known, a `- catching up` suffix
  covers the metadata phase, the bar is driven by `Resolved` of `KeysTotal` so
  it cannot read full while metadata remains, and the sweep/keys split,
  counters, elapsed, ETA and repaint coalescing are as before. Benchmark
  before/after: `internal/library/BENCH.md`, including the real-shape corpus
  (reduced 2,300-file default, full 23k via `POTFLIX_BENCH_REALSHAPE=full`).
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
- Storage rework (on disk; human-facing details in the repo `README.md` and
  `CHANGELOG.md`): `internal/store` (crash-safe append-only key/value store),
  the store-backed library index (`internal/library/index.go` + `scan.go` ->
  `library.idx` beside the configured index path), the one-file metadata cache
  (`internal/metadata/cache.go` -> `meta.idx`), the background catch-up worker
  (`internal/library/catchup.go`, started at launch from `main.go`), and
  `internal/repair` (needs-repair inventory + sanitized GitHub issue).

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
gitignored `bench-baseline.txt`. The store's own put/load bench
(`./internal/store`) and the store-backed index numbers are in the same file.

## Metadata lookup

The poll loop resolves the clean card's canonical series and episode titles
from free, keyless sources over stdlib `net/http`: TVmaze for TV, AniList +
Jikan for anime, iTunes then Wikipedia for movies (movie order ranks iTunes
first, Wikipedia immediately after). iTunes' search deliberately omits
`media=movie` - that filter returns `resultCount 0` - so a film outside the
iTunes catalog reaches Wikipedia, which supplies the artwork itself:
`prop=images&imlimit=max` (`wikipedia.go:337`) lists the article's files, and
the page image comes from `piprop=thumbnail|original` (`wikipedia.go:452`) -
the thumbnail, else the original page image, else the poster-like file from
that image list (`wikiPickPoster`). Found titles are fetched once and cached
permanently in ONE store file per cache directory
(`%LOCALAPPDATA%\Potflix\meta.idx` for the default `meta` dir, exe-dir
fallback) - one entry per key, `series|<show>|<year>|<season>` or
`movie|<title>|<year>`, where each used to be its own `<16-hex>.json` file;
not-found titles are cached as negative entries for 30 days under the same
key, so a later scan
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

The deliberate behaviour deviations from the Java reference are recorded in the
repository `README.md` § Deviations from the Java reference (10 entries as of
this writing).
Headless OP/ED with no episode name (`ED 03.mkv` -> `ED 3`, naming-report
P12) shows the tag alone; the frozen Java cleaner yields `ED 3 - ED 3`.
