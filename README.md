# Potflix - Discord presence for PotPlayer

Netflix-styled fork of `ayunami2000/PotPlayer-DiscordRPC`. The app is a
Windows tray program (a window opens on launch; `--selftest` stays
headless) that shows the media currently playing in
PotPlayer as a Discord Rich Presence card.

- **Clean titles always** - `Breaking.Bad.S01E02.1080p.WEB-DL.DD5.1.x264-CtrlHD.mkv`
  shows as **Breaking Bad - S1 E2**. Codecs, tags, release groups stripped,
  years parenthesized (`Dune Part Two (2024)`), `SxxEyy` parsed.
- **Standardized presence** - `Watching` type (3); header (`name`) = clean
  title, `Details` = episode name / episode tag / title, `State` = episode
  tag (unless already the details line; ` - Paused` appended while paused,
  `Browsing` / `Idle` when idle); tooltips `PotFlix` + `Playing` / `Paused` /
  `Stopped` (`Idle` when idle).
- **Netflix look** - red-on-black tray icon; right-click it for **Open Potflix** / **Exit Potflix**.
- **Your own app name/logo (optional)** - without this, presence uses the
  upstream Discord app (works out of the box, art keys `logo`/`play`/`pause`/`stop`).

## Run

Double-click `run.bat` (or the Desktop shortcut), or:

```powershell
.\dist\potflix.exe
```

Needs: PotPlayer open, Discord desktop open. No Java runtime is required.

## Build

The app is Go (stdlib only) under `go\`; build it with the repo's script:

```powershell
powershell -ExecutionPolicy Bypass -File tools\build.ps1
```

Outputs `dist\potflix.exe`. The script runs, from `go\`:

```powershell
go build -trimpath -buildvcs=false -ldflags "-H=windowsgui" -o ..\dist\potflix.exe .
```

`-trimpath` and `-buildvcs=false` keep the output identical from any checkout
directory and at any commit. Requires Go 1.27+; the build itself needs no
network (at runtime the app does keyless metadata lookups - see "Metadata
lookup" below).

The former Java flow - `tools\build.ps1` downloading an upstream fat jar from
the mutable release tag `v1` and re-packaging it with `javac` - is **retired**
and is no longer a build path. The Java sources under `src\` stay as the
reference implementation the Go port was proven against; see `DECISION.md`.

## Metadata lookup

The card shows canonical series and episode titles resolved from free,
keyless sources: TVmaze for TV, AniList + Jikan for anime, iTunes for movies.
Each season is fetched once and cached under
`%LOCALAPPDATA%\Potflix\meta`: found titles permanently, not-found titles as
negative entries for 30 days under the same key filename, so a later scan
costs zero network for either; `Refresh` (force) bypasses and re-attempts.
With no cache entry and no connection the card simply shows the clean
filename-based title. No credentials, no account, nothing personal: the cache holds public
episode titles only, lives outside the repo, and is never committed.

Data sources and attribution: [TVmaze](https://www.tvmaze.com/) (data under
CC BY-SA), [AniList](https://anilist.co/), [Jikan](https://jikan.moe/) (an
unofficial MyAnimeList API), the Apple
[iTunes Search API](https://performance-partners.apple.com/search-api).
Artwork and synopses are linked to, not stored.

## Library window

Normal launch shows the `Potflix` window (the tray icon stays; closing the
window parks it back to the tray). `--selftest` never creates it.

Main view (`MEDIA FOLDERS` + count): the watched DIR list, one row per
folder. `Add folder` (native folder picker) / `Remove` edit the list. `Scan`
resolves metadata incrementally through the folder index: unchanged folders
are skipped (no re-walk, no re-parse) and cached titles are served from
disk, never re-fetched; `Refresh` walks everything and force-refetches
every cached entry while rewriting index records. `Close to tray` hides
the window. Tray menu: `Open Potflix` / `Exit Potflix`.

Live status line while scanning:

```text
Scanning <done>/<total> keys | <fetched> fetched - <cached> cached | folders: <S> scanned, <K> skipped
```

Completion line (`Scan` or `Refresh` prefix):

```text
<Scan|Refresh> complete: <keys> keys - <cached> cached - <fetched> fetched - <notfound> not found | <files> files - folders: <S> scanned, <K> skipped
```

When there is nothing to fetch:

```text
<Scan|Refresh> complete: nothing to fetch (<cached> cached) - <K> folders skipped
```

(Huge counts elide to `k` / `M` / ...; the string is capped at 160 chars
with the `folders:` suffix preserved.)

`Settings` view:

- `LIBRARY` - `Clear cache` (native confirm; deletes cached titles plus the
  folder index; folders are kept), `Export...` / `Import...`
  (native Save-As / Open dialogs over `*.json`; Save-As defaults to
  `~/Desktop/potflix-metadata-YYYY-MM-DD.json`).
- `STARTUP` - `[x] Start with Windows` toggle (HKCU `...\Run` value
  `Potflix`; it shows the actual registry state), `Open log folder` (the
  exe's directory, where `potflix-debug.log` lives).
- `SCAN` - speed profile: `Turbo` (default) / `Balanced` / `Gentle`,
  applied immediately without a restart. Turbo = each provider's published
  limit, Balanced = half speed, Gentle = quarter speed - pick one if you
  want to be gentler on the services.

## Folder scan index

Per-directory scan accelerator at `%LOCALAPPDATA%\Potflix\scanindex.json`
(exe-dir fallback). Each record holds the tier-2 fingerprint, the tier-1
names hash, the metadata schema, the cache keys found beneath the folder,
and the video file count.

An unchanged folder is skipped without a walk: tier 1 compares dir mtime +
entry count + sorted entry names (no child stat); on mismatch, tier 2
compares the full fingerprint (name + size + mtime per child, subdirectories
included, so a change inside a subfolder invalidates the parent). The skip
guard then requires every recorded key to still verify against the metadata
cache. The guard is stat-based, so negative entries count: a folder whose
recorded keys are all not-found is still skipped. Anything that invalidates - schema bump, a missing key, a
corrupt/absent index file, or `Refresh` / force - falls back to a full walk.
Any hash doubt (stat/read error) also walks normally: the index only ever
skips, never resolves.

The window's `Scan` consults the index: unchanged folders are skipped (no
re-walk, no re-parse). `Refresh` (force) bypasses skipping - it walks
everything and re-fetches every cached title - while still rewriting index
records. The index is therefore not `metawarm -index`-only: `-index <file>`
remains the CLI opt-in, and `library.ScanWithOptions` (with `UseIndex`) is
the API.

## Export / Import / Clear

`Export...` writes one JSON bundle of the whole metadata cache. A bundle
holds metadata only: titles, synopsis, genres, length, button + poster URLs;
no media files, no library paths, no credentials. `Import...` merges a
bundle fill-missing-only (existing entries are never overwritten;
schema-mismatched and invalid entries are counted as skipped) and then
starts an incremental scan so missing keys are fetched online.

## Durability

State lives outside the repo under `%LOCALAPPDATA%\Potflix`: `config.json`
(folders, scan speed, startup flag), `meta\` (one JSON file per
cached title), `scanindex.json` (folder index). All three are written
atomically (temp file + rename) and loaded tolerantly (missing / corrupt =
defaults or a cache miss), so an update, a crash, or a restart keeps them.
The debug log beside the exe rotates at 5 MB x 3 files.

## Performance (measured 2026-10-09)

Benchmarks on `go1.27.0 windows/amd64` (AMD Ryzen 7 5800X); before/after
recorded in `go/internal/library/BENCH.md` (raw `count=3` tails in the
gitignored `go/bench-baseline.txt`):

- warm re-scan of 50 cached keys: 19.6 s -> ~20 ms (~962x), after rate
  limiting moved out of the scan loop into the per-host HTTP limiter
  (unknown hosts ungated);
- cold fake-provider scan of 20 keys: 7.6 s -> ~26 ms (~290x);
- parsing 7,847 filenames: ~830 ms, unchanged (~-1%, noise).
- indexed warm re-scan of 50 cached files: ~17.1 ms without the index ->
  ~11.7 ms with a populated index (~1.5x; the second pass skips every
  subtree while the skip guard still verifies cached keys). Table + raw
  `count=3` output in `go/internal/library/BENCH.md`
  (`BenchmarkScanIndexSecondPass`):

```powershell
go test -run '^$' -bench BenchmarkScanIndexSecondPass -benchmem -count=3 ./internal/library/
```

Command (from `go\`):

```powershell
go test -run '^$' -bench . -benchmem -count=3 ./internal/metadata/ ./internal/library/
```

## Library CLIs

`metawarm` pre-warms the cache (its usage line is the source of truth):

```powershell
metawarm -roots "D:\Movies;D:\Shows" [-cache <dir>] [-dry] [-offline] [-limit N] [-concurrency N] [-gate <dur>] [-index <file>]
```

`-index <file>` opts into folder skips via the scan index (empty = full
walk; the summary ends with `folders: N scanned, M skipped`). `-gate <dur>`
adds an extra global spacing between fetches (`0` = per-host limiter only).
`metacheck` is unchanged this session.

## Use your own Discord app (2 min, for custom name + logo)

1. https://discord.com/developers/applications -> New Application named **Potflix**
2. Rich Presence -> Art Assets -> upload `assets/discord/logo.png` as `logo`,
   `play.png` / `pause.png` / `stop.png` with those exact names
3. Copy the Application ID -> save it into `client-id.txt` (digits only)
   or set env `POTFLIX_CLIENT_ID`, then restart Potflix

## Deviations from the Java reference

Recorded deliberately; each is a behaviour change, not a parity claim.

- **The app shows a window on launch.** The Java app was headless (tray
  only). The Go app creates the `Potflix` settings window hidden and shows
  it on normal launch; closing it parks back to the tray, and `--selftest`
  stays headless.
- **Folder scan index.** No Java equivalent: a per-directory skip
  index (`scanindex.json`; tier-1 names hash, tier-2 fingerprint, cache-key
  guard) consulted by the window's `Scan` (`Refresh` walks everything and
  rewrites it), opt-in elsewhere via `library.ScanWithOptions` /
  `metawarm -index`. Any doubt falls back to a full walk.
- **Export / import bundle.** No Java equivalent: the metadata cache can be
  exported to / imported from one metadata-only JSON bundle
  (fill-missing-only import, then an incremental scan).
- **Empty asset text is omitted on the wire.** The Java app set
  `large_text=""` / `small_text=""`, but the Discord GameSDK dropped empty
  fields, and a present-but-empty `assets.large_text` is rejected by Discord
  (`code 4000`, `large_text is not allowed to be empty`), discarding the whole
  activity. The Go port marshals the activity with `omitempty`, so empty text
  fields are absent rather than empty strings. The app's live log shows the
  resulting SET_ACTIVITY accepted (`push ack nonce=...`); the `code 4000`
  rejection of a present-but-empty `large_text` was reproduced by an independent
  IPC probe, not observed in the app's own log.
- **Clear activity on exit.** Java's tray `Exit` was a bare `System.exit(0)`,
  which could leave a stale Discord card. The Go tray Exit clears the activity
  first.
- **Capped debug log.** Java's `potflix-debug.log` grew without limit (observed
  1,272,183 B). The Go logger rotates by size: 5 MB per file, 3 files
  (`potflix-debug.log`, `.1`, `.2`).
- **`READY` handshake required before sending.** Discord discards commands sent
  before its `READY` dispatch. The Go client holds commands back until `READY`
  (logging `send error: discord: not ready` before it), logs
  `connected clientId=<redacted>` on `READY`, and logs `push ack timeout` if a
  command is still unanswered after 2 s rather than assuming it was sent.
- **The activity type is Watching (3), and playing cards carry both
  `timestamps.start` and `timestamps.end`.** The Java reference sent type 0
  (Playing) and only `timestamps.start`. Discord draws an activity time bar only
  for Listening (2) / Watching (3), and the type also picks the card's verb
  ("Watching <title>"), so the Go port sets type 3. While playing with a known
  duration it sends both `start` (the media's real position anchor, `now - pos`)
  and `end = now + (total - pos)`, so Discord renders the bar against the true
  playback position rather than a bare elapsed count; when the duration is
  unknown only `start` is sent. The wire decision lives in one place
  (`internal/discord/activity.go`, `newActivity`). The drift-avoiding machinery
  is unchanged: the tracker's seek debounce is trailing-edge (a suppressed seek
  is held and lands when the window expires), the gate re-anchors at once when
  the live position diverges from the last-sent anchor by more than 2 s
  (throttled to one per 5 s), and a 30 s heartbeat re-pushes a steady card. Not
  rate-limit mitigation: the tracker emits 0/min while steady, the heartbeat
  adds 2/min, and a scrub is ~9/min.
- **Netflix-style card mapping.** SET_ACTIVITY `name` is the cleaned media title, so the header reads `Watching <title>` instead of the app name; `details` is the episode name when known, else the episode tag, else the title; `state` is the episode tag when it adds information beyond `details`; `large_text` is `PotFlix`, `small_text` is `Playing` / `Paused` / `Stopped` (`Idle` when idle). The `name` override is isolated behind one const (`activityNameHeader` in `presence.go`): setting it to false drops the override and the header falls back to the app name.
- **While paused the card is parked with a count-UP pause timer; the pause path
  never clears the activity.** The paused card keeps the title in the header/details lines and
  marks `state` (the episode tag; bare `Paused` with no tag) with the ` - Paused` suffix, sets `small_image` to `pause`, and
  anchors `timestamps.start` at the pause moment with no `timestamps.end`, so
  the client counts the pause UP ("paused for M:SS"). The parked card is sent
  once; while the player stays paused no further card goes out - not even the
  30 s heartbeat - so the pause timer is neither restarted nor spammed. On
  resume the card is re-set immediately with a fresh `start` (and `end`) anchored
  to the live position. The pause send and the resume re-set are part of the
  transition, not separate gate decisions, so neither can be debounced or
  throttled (the pause branch in `Gate.Decide` returns before the suppression
  paths, and the resume is a content change). This supersedes the earlier
  "clear the presence so there is no timer at all" behaviour, which was chosen
  when the owner asked for no timer while paused; the current requirement is the
  parked count-up card. The dead ` - MM:SS` position suffix (which produced a
  frozen `00:00` on the paused card) was removed. Only the pause path parks a
  card; the app still clears the activity on the paths that end a segment - app
  exit, no PotPlayer window found, and the player window disappearing.

## Dev

Module and package layout: `go\README.md`. Tests and vet:

```powershell
cd go
go vet ./...
go test ./... -count=1
```

One-command gate: `powershell -ExecutionPolicy Bypass -File tools\check.ps1` (add `-Build -Selftest` to also rebuild and self-test).

`.\dist\potflix.exe --selftest` proves the headless wiring (window/icon/tray
add, log rotation, second-instance lock, and a full IPC round-trip against an
in-process fake pipe).

Upstream: https://github.com/ayunami2000/PotPlayer-DiscordRPC (MIT-style? check LICENSE before publishing).
