<div align="center">

<img src="assets/appicon.png" width="128" alt="Potflix logo" />

# Potflix

**Whatever is playing in PotPlayer, on your Discord card.**
*Netflix-styled presence for PotPlayer - a Windows tray app, no Java, no keys.*

[![windows](https://img.shields.io/badge/windows-only-blue)](./go/README.md) [![go 1.27](https://img.shields.io/badge/go-%3E%3D1.27-green)](./go/go.mod) [![stdlib only](https://img.shields.io/badge/deps-stdlib_only-lightgrey)](./go/README.md)

</div>

---

## ⚡ Quick start

Needs: **PotPlayer** open and playing, **Discord** desktop open. No Java runtime required.

```powershell
powershell -ExecutionPolicy Bypass -File tools\build.ps1
.\dist\potflix.exe
```

Double-click `run.bat` works too. Right-click the red-on-black tray icon for **Open Potflix** / **Exit Potflix**; closing the window parks it back to the tray.
First run: the `Potflix` window opens - `Add folder`, then `Scan`.
To verify without PotPlayer or Discord (exit a running Potflix first - single-instance lock): `.\dist\potflix.exe --selftest` (see Development).

---

## 🧰 What it does

Watches the PotPlayer window and mirrors it as a Discord Rich Presence card (`Watching <title>`):

- **Clean titles always** - codecs, tags, and release groups stripped, years parenthesized, `SxxEyy` parsed. A raw filename shows as a real title with its episode tag (e.g. `Show.Name.S02E07.1080p.WEB-DL.x264-GROUP.mkv` becomes **Show Name - S2 E7**).
- **A card that tells the truth** - episode name when known, else episode tag, else title; ` - Paused` appended while paused (with a count-up pause timer), `Browsing` / `Idle` when idle.
- **Quiet while paused** - the paused card is sent once and held; not even the 30 s heartbeat re-pushes it until resume.
- **Canonical names, keyless** - series and episode titles resolved from free sources that need no account: TVmaze for TV, AniList + Jikan for anime, iTunes for movies. Found titles cache permanently, misses cache for 30 days, all under `%LOCALAPPDATA%\Potflix\meta`; offline it simply shows the clean filename title.
- **Library window** - normal launch shows the `Potflix` settings window: media folders (`Add folder` / `Remove`), incremental `Scan` (unchanged folders skipped, cached titles served from disk), `Refresh` (walks everything, re-fetches every entry), plus export / import of the metadata cache as one JSON bundle (fill-missing-only import). Scan prints a live progress line and a completion summary (keys cached / fetched / not-found, folders scanned / skipped).
- **Settings that stick** - scan speed (`Turbo` / `Balanced` / `Gentle`, applied immediately; Turbo is each provider's published limit, Gentle a quarter of it), Start-with-Windows toggle, card line presets, log-folder shortcut. State lives outside the repo under `%LOCALAPPDATA%\Potflix` (`config.json`, `meta\`, `scanindex.json`), written atomically and loaded tolerantly.
- **Card preview** - the window shows the exact card last handed to Discord (recorded on send, never rebuilt for display).

Data sources: TVmaze (CC BY-SA) for TV, AniList + Jikan (unofficial MyAnimeList API) for anime, iTunes Search API for movies. Artwork and synopses are linked to, not stored.

Deep internals (package layout, scan-index tiers, rate limiter, M1-M4 status) live in [`go/README.md`](./go/README.md), not here.

---

## ⚙️ Configure

**You don't need any.** Build, run, and the card appears - no config file, no keys.

Two things you may want:

- **Media folders** - open the `Potflix` window, `Add folder`, `Scan`. `Refresh` forces a full re-walk and re-fetch.
- **Start with Windows** - one toggle in Settings writes the `Potflix` value under `HKCU\...\Run` (it shows the actual registry state).

**Your own Discord app name + logo (optional, ~2 min).** Without this, presence uses the built-in app and works out of the box.

1. [New Application](https://discord.com/developers/applications) named **Potflix**.
2. Rich Presence -> Art Assets -> upload `assets/discord/logo.png` as `logo`, `play.png` / `pause.png` / `stop.png` with those exact names.
3. Save the Application ID into `client-id.txt` (digits only) or set env `POTFLIX_CLIENT_ID`, then restart.

Client-id resolution order (`go/clientid.go`): env `POTFLIX_CLIENT_ID` -> `client-id.txt` -> `config.properties` (raw text, first 17-20 digit run) -> built-in default. Only the source is ever logged, never the value. Treat `client-id.txt` as secret-ish: it is gitignored.

---

## 🛡️ Safety

**It watches PotPlayer and talks to Discord on your own machine. Nothing else.**

- **Keyless, accountless.** Metadata lookups need no credentials and the cache holds public episode titles only - no media files, no credentials, nothing personal. Export bundles are metadata-only too.
- **Local only.** State stays under `%LOCALAPPDATA%\Potflix`; the debug log (`potflix-debug.log` beside the exe) rotates at 5 MB * 3 files. Nothing is committed to the repo.
- **One instance.** The first run binds `127.0.0.1:17843`; a second prints `Potflix is already running.` and exits 0 without touching the log.
- **Polite on the wire.** Commands wait for Discord's `READY` dispatch, empty asset text is omitted (Discord rejects present-but-empty text), and Exit clears the activity first so no stale card lingers.
- **`--selftest` is the only argv switch**; everything else on the command line is ignored.

---

## 🧹 Uninstall

Right-click the tray icon -> **Exit Potflix**, then:

1. Turn off Start with Windows in Settings first (deletes the `Run` value) - skip if you never enabled it.
2. Delete the checkout (or just `dist\potflix.exe`).
3. Optionally delete `%LOCALAPPDATA%\Potflix` (settings, metadata cache, scan index) and `potflix-debug.log*` beside the exe.

No installer wrote anything anywhere else.

---

## 🛠️ Development

Stdlib-only Go under `go\` (module `potflix`, Go 1.27, no `go.sum`). Build from the repo root:

```powershell
powershell -ExecutionPolicy Bypass -File tools\build.ps1   # -> dist\potflix.exe
```

That runs, from `go\`: `go build -trimpath -buildvcs=false -ldflags "-H=windowsgui" -o ..\dist\potflix.exe .` - reproducible from any directory or commit, console-less tray app. Requires Go 1.27+.

```powershell
cd go
go vet ./...
go test ./... -count=1
```

Or one gate from the root (exit a running Potflix first - the selftest needs the single-instance lock): `powershell -ExecutionPolicy Bypass -File tools\check.ps1 -Build -Selftest`. Tests are pure logic - no live PotPlayer or Discord needed (Discord transport tests use an in-process fake pipe). If `go env GOTMPDIR` points at a missing directory, create it first or every go command fails.

```powershell
.\dist\potflix.exe --selftest
```

proves the headless wiring (exit a running Potflix first - single-instance lock): hidden window + tray add, log rotation, second-instance lock, full IPC round-trip. Title-cleaner parity CLI (from `go\`): `go build ./cmd/titleclean` gives `titleclean -corpus <in.txt> -out <out.tsv>` (columns: input/title/display/episodeTag/episodeName).

Diagnose one title: `go run ./cmd/metacheck [-offline] <rawTitle>` (exit 0 when resolved, 1 when not). Package layout, metadata design, and benchmarks: [`go/README.md`](./go/README.md).

---

## 🧭 Compatibility

| | |
|---|---|
| OS | Windows only (win32 via `syscall.NewLazyDLL`) |
| Build | Go 1.27+, stdlib only, no `go.sum` |
| Needs at runtime | PotPlayer open, Discord desktop open (local IPC pipe) |
| Needs not | No Java runtime, no API keys, no account |
| Java under `src\` | Frozen reference only (pinned `540a1d1`), never a build path |

---

## Deviations from the Java reference

Deliberate behaviour changes, not parity claims. Upstream: `ayunami2000/PotPlayer-DiscordRPC` (BSD-3, see `THIRD-PARTY-NOTICES.md`).

- **A window on launch.** Java was tray-only; the Go app shows the `Potflix` settings window (closing parks to the tray, `--selftest` stays headless).
- **Folder scan index.** Per-directory skip index (`scanindex.json`) behind the window's `Scan`; `Refresh` walks everything and rewrites it. Any doubt falls back to a full walk - it only ever skips, never resolves.
- **Export / import bundle.** One metadata-only JSON bundle, fill-missing-only import followed by an incremental scan.
- **Empty asset text omitted on the wire** (`omitempty`) - Discord rejects present-but-empty text and discards the whole activity.
- **Clear activity on exit**, so no stale card lingers.
- **Capped debug log**: 5 MB per file, 3 files.
- **`READY`-gated sends** with ack/timeout logging instead of fire-and-forget.
- **`Watching` (type 3) with `timestamps.start` + `timestamps.end`** anchored to the live position (start-only when duration is unknown); seek debounce, divergence re-anchor, and a 30 s heartbeat live in `internal/presence/gate.go`.
- **Netflix-style card mapping**: header is the clean title, details/state carry episode name and tag, ` - Paused` parks a count-up pause card that never clears until resume or segment end.

---

## 📄 License

Fork of [`ayunami2000/PotPlayer-DiscordRPC`](https://github.com/ayunami2000/PotPlayer-DiscordRPC) (BSD 3-Clause) - see [THIRD-PARTY-NOTICES.md](./THIRD-PARTY-NOTICES.md) for the full text and logo-font notices.

The title-cleaner logic derives from that project; the bundled logo fonts are SIL OFL 1.1 (per-family texts live under `design\fonts\`).
