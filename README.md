<p align="center">
  <img src="assets/icon.svg" width="120" height="120" alt="Potflix icon" />
</p>
<h1 align="center">Potflix</h1>
<p align="center"><strong>Mirror whatever plays in PotPlayer on your Discord card — Go.</strong></p>
<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/github/license/nathwn12/Potflix" alt="License" /></a>
  <img src="https://img.shields.io/github/languages/top/nathwn12/Potflix" alt="Language" />
  <img src="https://img.shields.io/badge/platform-windows--64bit-2b3137?logo=windows&logoColor=white" alt="Platform" />
  <a href="https://github.com/nathwn12/Potflix/actions/workflows/ci.yml"><img src="https://github.com/nathwn12/Potflix/actions/workflows/ci.yml/badge.svg" alt="CI" /></a>
</p>

## What it is

Potflix is a Windows tray app that watches the PotPlayer window and mirrors
it as a Discord Rich Presence card (`Watching <title>`). It is stdlib-only Go
(module `potflix`, Go 1.27, no `go.sum`): no Java runtime, no API keys, no
account. Filenames are cleaned into real titles (codecs, tags, and release
groups stripped, years parenthesized, `SxxEyy` parsed), series and episode
names resolve from free keyless sources, and a settings window holds the
library folders, scan controls, and card preview.

## Why

The retired Java flow compiled against an upstream fat jar downloaded from a
mutable release tag and needed a Java runtime on every machine. The Go port
builds to one self-contained `dist\potflix.exe` with nothing to install, keeps
the title-cleaner byte-identical to the frozen Java reference, and resolves
canonical names without credentials.

## Features

- Clean titles always: codecs, tags, and release groups stripped, years
  parenthesized, `SxxEyy` parsed (a raw filename shows as a real title with
  its episode tag).
- A card that tells the truth: episode name when known, else episode tag,
  else title; ` - Paused` appended while paused with a count-up pause timer;
  `Browsing` / `Idle` when idle.
- Quiet while paused: the paused card is sent once and held; not even the
  30 s heartbeat re-pushes it until resume.
- Canonical names, keyless: TVmaze for TV, AniList + Jikan for anime, iTunes
  for movies. Found titles cache permanently, misses cache for 30 days, all
  under `%LOCALAPPDATA%\Potflix\meta`; offline it shows the clean filename
  title.
- Library window: media folders (`Add folder` / `Remove`), incremental
  `Scan` (unchanged folders skipped, cached titles served from disk),
  `Refresh` (walks everything, re-fetches every entry), plus export / import
  of the metadata cache as one JSON bundle (fill-missing-only import).
- Settings that stick: scan speed (`Turbo` / `Balanced` / `Gentle`, applied
  immediately), Start-with-Windows toggle, card line presets. State lives
  outside the repo under `%LOCALAPPDATA%\Potflix`, written atomically and
  loaded tolerantly.
- Card preview: the window shows the exact card last handed to Discord
  (recorded on send, never rebuilt for display).
- Local only: one instance (second launch prints `Potflix is already
  running.` and exits 0), capped rotating debug log beside the exe,
  `--selftest` proves the headless wiring with no live PotPlayer or Discord.

## Install

1. Needs: **PotPlayer** open and playing, **Discord** desktop open.
2. Build the exe (requires Go 1.27+):

```powershell
powershell -ExecutionPolicy Bypass -File tools\build.ps1
```

3. Run it (double-click `run.bat` works too):

```powershell
.\dist\potflix.exe
```

4. Right-click the tray icon for **Open Potflix** / **Exit Potflix**; closing
   the window parks it back to the tray. First run: the `Potflix` window
   opens — `Add folder`, then `Scan`.

## Usage

Normal launch shows the `Potflix` settings window; the card appears on your
Discord profile on its own. To verify without PotPlayer or Discord (exit a
running Potflix first — single-instance lock):

```powershell
.\dist\potflix.exe --selftest
```

`--selftest` is the only command-line switch; everything else is ignored.

Your own Discord app name + logo (optional, ~2 min): without this, presence
uses the built-in app and works out of the box. Create an application named
**Potflix** at the Discord developer portal, upload `assets/discord/logo.png`
as `logo` plus `play.png` / `pause.png` / `stop.png` under Rich Presence art
assets, then save the Application ID into `client-id.txt` (digits only,
gitignored) or set env `POTFLIX_CLIENT_ID` and restart. Resolution order:
env `POTFLIX_CLIENT_ID` → `client-id.txt` → `config.properties` → built-in
default. Only the source is ever logged, never the value.

## Build from source

Requires Go 1.27+ on Windows (stdlib only; no modules are downloaded). From
the repo root:

```powershell
powershell -ExecutionPolicy Bypass -File tools\build.ps1   # -> dist\potflix.exe
```

That runs, from `go\`:

```text
go build -trimpath -buildvcs=false -ldflags "-H=windowsgui" -o ..\dist\potflix.exe .
```

Reproducible from any directory or commit, console-less tray app. Then, from
`go\`:

```powershell
go vet ./...
go test ./... -count=1
```

Or one gate from the root: `powershell -ExecutionPolicy Bypass -File tools\check.ps1 -Build -Selftest`. Tests are pure logic — no live PotPlayer or
Discord needed. Diagnose one title: `go run ./cmd/metacheck [-offline] <rawTitle>` (exit 0 when resolved, 1 when not).

## Docs

- [`go/README.md`](./go/README.md) — package layout, metadata design, scan-index tiers, rate limiter, M1–M4 status.
- [`go/internal/titleclean/README.md`](./go/internal/titleclean/README.md) — the title-cleaner port and its parity method.
- [`DECISION.md`](./DECISION.md) — why the Java flow was retired and what the Go port was proven against.
- [`THIRD-PARTY-NOTICES.md`](./THIRD-PARTY-NOTICES.md) — upstream BSD-3-Clause text and the bundled logo-font notices.

## Status

**Unverified** — as-is, use at your own risk.

## Credits

- Upstream: [`ayunami2000/PotPlayer-DiscordRPC`](https://github.com/ayunami2000/PotPlayer-DiscordRPC) (BSD-3-Clause). The title-cleaner logic in Potflix derives from that project.
- Logo fonts: the files under `design\fonts\` (Archivo Black, Montserrat, Poppins, Anton, Bebas Neue, Inter), each under the SIL Open Font License 1.1, render the Potflix logo offline via `design\make_logo.py`. They are not the app's typeface and are not redistributed as one.
- Potflix is not affiliated with or endorsed by the upstream authors or the font authors.
- See [THIRD-PARTY-NOTICES.md](./THIRD-PARTY-NOTICES.md) for the full upstream and font notices.

## License

MIT — see [LICENSE](LICENSE).

<!-- nathwn12 house footer: identical in every repo. Do not edit. -->
<p align="center">
  <a href="https://github.com/nathwn12"><img src="assets/signature.svg" width="96" alt="nathwn12" /></a>
</p>
<p align="center">
  <sub>Built by <a href="https://github.com/nathwn12">nathwn12</a> · <a href="https://github.com/nathwn12?tab=repositories">All projects</a></sub>
</p>
