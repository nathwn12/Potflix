<p align="center">
  <img src="assets/icon.svg" width="120" height="120" alt="Potflix icon" />
</p>
<h1 align="center">Potflix</h1>
<p align="center"><strong>Mirror whatever plays in PotPlayer on your Discord card — Rust.</strong></p>
<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/github/license/nathwn12/Potflix" alt="License" /></a>
  <img src="https://img.shields.io/github/languages/top/nathwn12/Potflix" alt="Language" />
  <img src="https://img.shields.io/badge/platform-windows--64bit-2b3137?logo=windows&logoColor=white" alt="Platform" />
</p>

## What it is

Potflix is a Windows tray app that watches the PotPlayer window and mirrors
it as a Discord Rich Presence card (`Watching <title>`): no Java runtime, no
API keys, no account. Filenames are cleaned into real titles (codecs, tags,
and release groups stripped, years parenthesized, `SxxEyy` parsed), series and
episode names resolve from free keyless sources, and a settings window holds
the library folders, scan controls, and card preview.

The implementation is the Rust workspace under `rust/`. The Go module in `go/`
is the frozen reference implementation, retained until the Rust port merges
and takes over the shipped binary; the original Java sources under
`src/main/java/me/potflix/rpc/` (`Main.java`) are retained in the same way.

## Why

PotPlayer has no Discord presence of its own, so whatever is playing there is
invisible on your Discord card. Potflix closes that gap as a local Windows
tray app: no Java runtime, no API keys, no accounts, no telemetry, no server —
filenames are cleaned into real titles and resolved through free keyless
sources, cached on disk so the card survives being offline.

## Features

- Clean titles always: codecs, tags, and release groups stripped, years
  parenthesized, `SxxEyy` parsed (a raw filename shows as a real title with
  its episode tag).
- A card that tells the truth: episode name when known, else episode tag,
  else title; ` - Paused` appended while paused with a count-up pause timer;
  `Browsing` / `Idle` when idle.
- Quiet while paused: the paused card is sent once and held; not even the
  30 s heartbeat re-pushes it until resume.
- Canonical names, keyless: TVmaze for TV, AniList + Cinemeta for anime,
  Cinemeta for movies. Found titles cache permanently, misses cache for 30 days, all
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
2. Build the exe (requires a Rust toolchain with `cargo`):

   ```powershell
   powershell -ExecutionPolicy Bypass -File tools\rust-build.ps1
   ```

3. Run it (double-click `run.bat` works too):

   ```powershell
   .\dist\potflix.exe
   ```

4. Right-click the tray icon for **Open Potflix** / **Exit Potflix**; closing
   the window parks it back to the tray. First run: the `Potflix` window
   opens — `Add folder`, then `Scan`.
5. Prefer an installer to the manual build? `tools\make-installer.ps1` produces
   `dist\Potflix-Setup.exe` — per-user, no UAC (`PrivilegesRequired=lowest`),
   installing to `%LOCALAPPDATA%\Programs\Potflix` with a Start Menu entry. The
   desktop icon is a task: ticked on a first install, but unchecked when Setup
   finds an existing Potflix — tick `desktopicon` (or pass `/TASKS=desktopicon`
   to a silent install) to get one.

## Usage

Normal launch shows the `Potflix` settings window; the card appears on your
Discord profile on its own. To verify without PotPlayer or Discord (exit a
running Potflix first — single-instance lock):

```powershell
.\dist\potflix.exe --selftest
```

`--selftest` (headless wiring proof) and `--minimized` (autostart: keep the
main window hidden, tray only) are the only honoured switches; everything else is
ignored.

Your own Discord app name + logo (optional, ~2 min): without this, presence
uses the built-in app and works out of the box. Create an application named
**Potflix** at the Discord developer portal, upload `assets/discord/logo.png`
as `logo` plus `play.png` / `pause.png` / `stop.png` under Rich Presence art
assets, then save the Application ID into `client-id.txt` (digits only,
gitignored) or set env `POTFLIX_CLIENT_ID` and restart. Resolution order:
env `POTFLIX_CLIENT_ID` → `client-id.txt` → `config.properties` → built-in
default. Only the source is ever logged, never the value.

## Build from source

Requires a Rust toolchain with `cargo`, on Windows. From the repo root:

```powershell
powershell -ExecutionPolicy Bypass -File tools\rust-build.ps1   # builds dist\potflix.exe
powershell -ExecutionPolicy Bypass -File tools\check.ps1        # the gate: clippy + tests + titleclean parity
powershell -ExecutionPolicy Bypass -File tools\check.ps1 -Build -Selftest
```

`tools\check.ps1` is the single exit command; it fails fast and always runs,
from `rust\`:

- `cargo clippy --workspace --all-targets -- -D warnings`
- `cargo test --workspace --release --locked`
- the title-cleaner parity step (see [RUST-PORT.md](./RUST-PORT.md))

`-Build` runs `tools\rust-build.ps1`, which is `cargo build --workspace
--release --locked` in `rust\` and copies `rust\target\release\potflix.exe` to
`dist\potflix.exe`. `-Selftest` runs `dist\potflix.exe --selftest` and requires
the success marker `M1 SELFTEST PASS`; run with `-Build` to produce the binary
from current source.

Per-crate gates:

```powershell
cargo test  -p <crate> --release
cargo clippy -p <crate> --all-targets -- -D warnings
cargo fmt   -p <crate> -- --check
```

Tests are pure logic — no live PotPlayer or Discord needed. Diagnose one title
with the `metacheck` CLI.

## Docs

- [RUST-PORT.md](./RUST-PORT.md) — the Rust port notes: crate layout, the parity
  contract, verification, Windows-specific notes, dependencies, limitations, and
  the deviations from the Java and Go references.
- [`go/README.md`](./go/README.md) — the frozen Go reference: package layout,
  metadata design, scan-index tiers, rate limiter, M1–M4 status.
- [`go/internal/titleclean/README.md`](./go/internal/titleclean/README.md) — the
  title-cleaner port and its parity method.
- [`DECISION.md`](./DECISION.md) — why the cleaner port chose Go, and why the app
  is being rewritten in Rust on a parity oracle.
- [`THIRD-PARTY-NOTICES.md`](./THIRD-PARTY-NOTICES.md) — upstream BSD-3-Clause text
  and the bundled logo-font notices.

## Status

**Unverified** — as-is, use at your own risk.

The Rust build, workspace tests, and headless `--selftest` pass on the author's machine; live PotPlayer/Discord behaviour is not re-verified here.

## Credits

- Derived from [ayunami2000/PotPlayer-DiscordRPC](https://github.com/ayunami2000/PotPlayer-DiscordRPC)
  by ayunami2000 (BSD-3-Clause). Potflix is not affiliated with or endorsed by
  the upstream author or the font authors.
- Logo fonts: the files under `design\fonts\` (Archivo Black, Montserrat, Poppins,
  Anton, Bebas Neue, Inter), each under the SIL Open Font License 1.1, render the
  Potflix logo offline via `design\make_logo.py`. They are not the app's typeface
  and are not redistributed as one.
- See [THIRD-PARTY-NOTICES.md](./THIRD-PARTY-NOTICES.md) for the full upstream and
  font notices.

## License

MIT — see [LICENSE](LICENSE).

<!-- nathwn12 house footer: identical in every repo. Do not edit. -->
<p align="center">
  <a href="https://github.com/nathwn12"><img src="assets/signature.svg" width="96" alt="nathwn12" /></a>
</p>
<p align="center">
  <sub>Built by <a href="https://github.com/nathwn12">nathwn12</a> · <a href="https://github.com/nathwn12?tab=repositories">All projects</a></sub>
</p>
