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

## Project name and description

Potflix is a Windows tray app that watches the PotPlayer window and mirrors
what is playing as a Discord Rich Presence card (`Watching <title>`). It needs
no Java runtime, no API keys, and no account: filenames are cleaned into real
titles (codecs, tags, and release groups stripped, years parenthesized,
`SxxEyy` parsed), series and episode names resolve from free keyless sources
and cache on disk, and a settings window holds the library folders, scan
controls, and card preview.

## Technology stack

- **Rust** — Cargo workspace under `rust/` (`rust/Cargo.toml`): edition 2024,
  workspace version `1.0.0`, `rust-version = "1.99"` (measured `rustc 1.99.1`
  on the author's machine). `Cargo.lock` is committed and `--locked` is the
  build norm.
- **Pinned dependencies** (exact versions in `rust/Cargo.toml`):
  `serde =1.0.229` (+derive), `serde_json =1.0.154`,
  `ureq =3.4.2` (`default-features = false`, `native-tls` → OS SChannel, no
  OpenSSL/C toolchain), `windows-sys =0.61.2` (raw Win32 FFI, feature-gated).
- **Deliberate omissions:** no Unicode/word-segmentation crate (`titleclean`
  ports the JDK-11 tables verbatim as data) and no `rustls`/`ring`; CRC32 for
  the store is hand-rolled.
- **Keyless metadata sources:** TVmaze (TV); AniList, Tenrai, Kitsu, Cinemeta,
  Episodate, Bangumi, LiveChart.me, ANN (anime); Cinemeta, Wikipedia,
  JustWatch (movies); Wikipedia artwork fallback. No accounts, no keys.
- **Installer:** Inno Setup via `tools/make-installer.ps1` + `tools/potflix.iss`
  → `dist/Potflix-Setup.exe` (per-user, no UAC, installs to
  `%LOCALAPPDATA%\Programs\Potflix` with a Start Menu entry).

## Architecture

One member crate per ported package under `rust/crates/*`, plus `app`, which
owns the workspace's `potflix` tray binary:

| Crate | Owns |
|---|---|
| `titleclean` | Media-title cleaner port + `titleclean` parity CLI; carries the verbatim JDK-11 tables (`jdk11tables.rs`) |
| `store` | Crash-safe append-only single-file key/value store (hand-rolled CRC32) behind the library index and metadata cache |
| `metadata-core` | Network-free half of metadata: record types, filename parsing, candidate matching |
| `metadata-http` | Transport half: shared HTTP client / fetch helpers (`ureq` over `native-tls`), per-host rate limiter |
| `metadata` | Provider chain, the eleven keyless providers, the on-disk cache; owns the `metacheck` CLI |
| `library` | Walk + diff loop behind Scan / Refresh, background metadata catch-up; owns the `metawarm` CLI |
| `discord` | Local Discord IPC pipe transport (`discord-ipc-0..9`): handshake, `SET_ACTIVITY`, `READY` gating |
| `player` | PotPlayer window discovery (`EnumWindows`), `WM_USER` time/status queries, `WM_COPYDATA` path query |
| `presence` | Card assembly: episode name → episode tag → title fallback, ` - Paused` count-up, `Browsing` / `Idle` |
| `repair` | Needs-repair inventory the user can file |
| `bundle` | Metadata-cache export / import as one JSON bundle (fill-missing-only import) |
| `app-support` | Client-ID resolution, settings state under `%LOCALAPPDATA%\Potflix` (atomic writes, tolerant loads) |
| `potflix-win` | Windows tray window plumbing |
| `app` | The shipped `potflix` tray binary; `--selftest` headless wiring proof |

> [!NOTE]
> The Rust app builds and its headless self-test passes, but those gates prove
> wiring and the title cleaner only — feature-level behaviour against live
> PotPlayer/Discord is carried over from the retired reference and not
> re-verified here (see [RUST-PORT.md](./RUST-PORT.md)).

## Getting started

Prerequisites: **PotPlayer** open and playing, **Discord** desktop open, and a
Rust toolchain with `cargo` for the build. Windows-only.

1. Build the exe:

   ```powershell
   powershell -ExecutionPolicy Bypass -File tools\rust-build.ps1
   ```

   This runs `cargo build --workspace --release --locked` in `rust\` and copies
   `rust\target\release\potflix.exe` to `dist\potflix.exe`.
2. Run it (`run.bat` works too):

   ```powershell
   .\dist\potflix.exe
   ```

3. Right-click the tray icon for **Open Potflix** / **Exit Potflix**; closing
   the window parks it back to the tray. First run: the `Potflix` window
   opens — `Add folder`, then `Scan`.
4. Verify without PotPlayer or Discord (exit a running Potflix first —
   single-instance lock):

   ```powershell
   .\dist\potflix.exe --selftest
   ```

   `--selftest` (headless wiring proof) and `--minimized` (autostart: tray
   only) are the only honoured switches; everything else is ignored.
5. Optional, your own Discord app name + logo (~2 min): without this, presence
   uses the built-in app and works out of the box. Create an application named
   **Potflix** at the Discord developer portal, upload `assets/discord/logo.png`
   as `logo` plus `play.png` / `pause.png` / `stop.png` under Rich Presence art
   assets, then save the Application ID into `client-id.txt` (digits only,
   gitignored) or set env `POTFLIX_CLIENT_ID` and restart. Resolution order:
   env `POTFLIX_CLIENT_ID` → `client-id.txt` → `config.properties` → built-in
   default (files searched exe directory first, then working directory). Only
   the source is ever logged, never the value.

Prefer an installer to the manual build? `tools\make-installer.ps1` produces
`dist\Potflix-Setup.exe` — tick `desktopicon` (or pass `/TASKS=desktopicon` to
a silent install) for a desktop icon.

## Project structure

```text
rust/                  Cargo workspace (edition 2024, Cargo.lock committed)
  crates/*/            One member crate per package (see Architecture table)
  parity-golden.sha256 FROZEN title-cleaner parity oracle (SHA-256 per corpus)
tools/
  rust-build.ps1       Build: workspace release → dist\potflix.exe
  check.ps1            The gate: clippy + tests + parity (-Build, -Selftest)
  make-installer.ps1 + potflix.iss   Inno Setup installer
  make-icons.ps1       Regenerate the exe/tray icon resources
dist/                  Build output (potflix.exe, Potflix-Setup.exe)
assets/                icon.svg (header mark), discord/ art, app icons
design/                Logo sources (make_logo.py + fonts; not the app typeface)
RUST-PORT.md           Crate map, parity contract, verification, deviations
DECISION.md            Why the cleaner port chose Go, why the app is Rust now
THIRD-PARTY-NOTICES.md Upstream licence text and logo-font notices
CHANGELOG.md           Keep a Changelog history
```

State lives outside the repo under `%LOCALAPPDATA%\Potflix` (settings, the
`meta` metadata cache — hits and misses kept 30 days — and the library
index). The debug log `potflix-debug.log` sits beside the exe and rotates at
5 MB × 3.

## Key features

- Clean titles always: codecs, tags, and release groups stripped, years
  parenthesized, `SxxEyy` parsed.
- A card that tells the truth: episode name when known, else episode tag, else
  title; ` - Paused` appended while paused with a count-up timer;
  `Browsing` / `Idle` when idle. Quiet while paused: the paused card is sent
  once and held — not even the 30 s heartbeat re-pushes it until resume.
- Canonical names, keyless, cached on disk; offline it shows the clean
  filename title.
- Library window: media folders (`Add folder` / `Remove`), incremental `Scan`
  (unchanged folders skipped), `Refresh` (re-walks everything, re-fetches
  every entry), export / import of the metadata cache as one JSON bundle.
- Settings that stick: scan speed (`Turbo` / `Balanced` / `Gentle`, applied
  immediately), Start-with-Windows toggle, card line presets.
- Card preview: the window shows the exact card last handed to Discord
  (recorded on send, never rebuilt for display).
- Local only: one instance (a second launch prints
  `Potflix is already running.` and exits 0); `--selftest` proves the headless
  wiring with no live PotPlayer or Discord.

## Development workflow

- Behaviour changes go in `rust\` — the Go and Java reference trees are retired
  and removed; their history remains in git. Never recreate them.
- The single exit command, from the repo root (fails fast):

  ```powershell
  powershell -ExecutionPolicy Bypass -File tools\check.ps1 -Build -Selftest
  ```

  This runs clippy (`-D warnings`), the release workspace tests, the
  title-cleaner parity step against the frozen oracle, the app build, and the
  headless `--selftest` (which requires the `M1 SELFTEST PASS` marker).
- Per-crate gates:

  ```powershell
  cargo test  -p <crate> --release
  cargo clippy -p <crate> --all-targets -- -D warnings
  cargo fmt   -p <crate> -- --check
  ```

- Diagnose one title with the `metacheck` CLI; pre-warm a library walk with
  `metawarm`; corpus parity TSV via the `titleclean` CLI.
- No CI runs: `.github/workflows/ci.yml` is held at `workflow_dispatch` only —
  every gate is proven locally.
- Changes are recorded in [CHANGELOG.md](./CHANGELOG.md) (Keep a Changelog).

## Coding standards

- `cargo clippy --workspace --all-targets -- -D warnings` must pass — warnings
  are errors, enforced by the gate.
- `--locked` builds against the committed `Cargo.lock`; dependency versions
  are pinned exact in `rust/Cargo.toml` and relaxed only by recorded decision.
- `rust/crates/titleclean/src/jdk11tables.rs` (the verbatim JDK-11 tables) is
  never hand-edited — the compiler's newer Unicode would break byte-parity.
- `rust/parity-golden.sha256` is frozen: match it, never regenerate it (its
  reproducer is gone). A missing title corpus or a hash mismatch fails the
  gate — never "nothing to compare".
- Never log the Discord client-ID value, only its source.

## Testing

- `cargo test --workspace --release --locked` (via `tools\check.ps1`) — the
  suite is pure logic; no live PotPlayer or Discord needed (Discord transport
  tests use an in-process fake pipe, `rust\crates\discord\src\fakepipe.rs`).
- Title-cleaner parity: the gate verifies the built cleaner byte-identical to
  the frozen oracle on two corpora (the corpora themselves are gitignored and
  uncommitted, carrying real media filenames).
- `.\dist\potflix.exe --selftest` proves headless wiring end to end: hidden
  window + tray add, log rotation, second-instance lock, full IPC round-trip
  against the in-process fake pipe.

**Unverified** — as-is, use at your own risk. The Rust build, workspace tests,
and headless `--selftest` pass on the author's machine; live PotPlayer/Discord
behaviour is not re-verified here.

## Contributing

1. Make the change under `rust\` (one member crate owns each behaviour — see
   Architecture).
2. Prove it with the gate: `powershell -ExecutionPolicy Bypass -File
   tools\check.ps1 -Build -Selftest` from the repo root.
3. Respect the frozen pieces: the parity oracle, the JDK-11 tables, and the
   deliberate deviations recorded in [RUST-PORT.md](./RUST-PORT.md) ("Deviations
   from the original Java flow") — do not "fix" those.
4. Record user-visible changes in [CHANGELOG.md](./CHANGELOG.md) under
   `[Unreleased]`.

## License

MIT — see [LICENSE](LICENSE).

## Credits

- Derived from [ayunami2000/PotPlayer-DiscordRPC](https://github.com/ayunami2000/PotPlayer-DiscordRPC)
  by ayunami2000 (BSD-3-Clause). Potflix is not affiliated with or endorsed by
  the upstream author or the font authors.
- Logo fonts: the files under `design\fonts\` (Archivo Black, Montserrat,
  Poppins, Anton, Bebas Neue, Inter), each under the SIL Open Font License 1.1,
  render the Potflix logo offline via `design\make_logo.py`. They are not the
  app's typeface and are not redistributed as one.
- See [THIRD-PARTY-NOTICES.md](./THIRD-PARTY-NOTICES.md) for the full upstream
  and font notices.

<!-- nathwn12 house footer: identical in every repo. Do not edit. -->
<p align="center">
  <a href="https://github.com/nathwn12"><img src="assets/signature.svg" width="96" alt="nathwn12" /></a>
</p>
<p align="center">
  <sub>Built by <a href="https://github.com/nathwn12">nathwn12</a> · <a href="https://github.com/nathwn12?tab=repositories">All projects</a></sub>
</p>
