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

This repository is mid-rewrite. The implementation is now the Rust workspace
under `rust\` — a Cargo workspace whose member crates under `rust\crates\*`
mirror the Go packages one-for-one. The Go module in `go\` is the **frozen
reference implementation**, retained until the Rust port merges and takes over
the shipped binary; it is not edited and it is not built by the Rust gate. The
original Java sources under `src\main\java\me\potflix\rpc\` (`Main.java`) are
retained in the same way — the reference the cleaner was proven against.

## Layout

- `rust\` — the implementation: a Cargo workspace (`rust\Cargo.toml`) with one
  member crate per Go package under `rust\crates\*`, plus `app`, which owns the
  workspace's `potflix` tray binary. Cargo workspace, edition 2024.
- `go\` — the frozen Go reference implementation (module `potflix`). Retired at
  merge; not built by the Rust gate. See `go\README.md`.
- `src\main\java\me\potflix\rpc\Main.java` — the frozen Java reference the title
  cleaner was proven against.
- `tools\check.ps1` — the single exit command (gate). `tools\rust-build.ps1` —
  the Rust build. `tools\build.ps1` — the frozen Go build.

## Crates

One member crate per Go package; each line is the crate's own ownership.

| Crate | Owns |
|---|---|
| `titleclean` | Dependency-free port of the Java media-title cleaner (`me.potflix.rpc.Main.clean`); JDK-11 Unicode tables ported as data; also carries the `titleclean` parity CLI. |
| `store` | The crash-safe single-file append-only key/value store behind the library index and the metadata cache; hand-rolled CRC32, `CreateFileW` for the share flags. |
| `metadata-core` | The network-free half of metadata: record types, filename parsing, candidate matching, alternate titles, request context. |
| `metadata-http` | The transport half: the shared HTTP client / fetch helpers (ureq over `native-tls`) and the per-host rate limiter. |
| `metadata` | The provider chain, the five keyless providers (TVmaze, AniList, Jikan, iTunes, Wikipedia) and the permanent on-disk cache; owns the `metacheck` CLI. |
| `library` | The shared walk + diff loop behind Scan / Refresh, the background metadata catch-up, and `metawarm`; the store-backed `library.idx`; owns the `metawarm` CLI. |
| `discord` | Local Discord IPC pipe transport over `\\.\pipe\discord-ipc-0..9`: handshake, `SET_ACTIVITY`, `READY` gating, reply matching. |
| `player` | PotPlayer window discovery (`EnumWindows`), the `WM_USER` time/status queries, and the `WM_COPYDATA` play-file path query. |
| `presence` | The presence state machine and the activity payload builder; owns `CardSpec`, the stored card shape. |
| `repair` | The needs-repair report and the sanitized GitHub issue path. |
| `bundle` | Metadata-only cache bundles: export, fill-missing import, count, clear. |
| `app-support` | The non-Win32 support surface split out of the Go root package: client-id resolution, the settings/config store, the debug log, and the single-instance lock. |
| `potflix-win` | The Win32 surface split out of the Go root package: the raw binding table, the tray window, and the settings window. |
| `app` | The tray application — the Rust `app` crate's `main` and `selftest` binaries — and the icon resource (`build.rs`); owns the workspace's `potflix` binary. |

## Build and test

Rust is the implementation, so the gate lives in Rust. `tools\check.ps1` is the
single exit command; from the repo root:

```powershell
powershell -ExecutionPolicy Bypass -File tools\check.ps1                 # clippy + tests + titleclean parity
powershell -ExecutionPolicy Bypass -File tools\check.ps1 -Build -Selftest
```

`tools\check.ps1` fails fast and always runs, from `rust\`:

- `cargo clippy --workspace --all-targets -- -D warnings`
- `cargo test --workspace --release --locked`
- the title-cleaner parity step (below)

Its flags add the app build and the headless self-test:

- `-Build` — runs `tools\rust-build.ps1`, which is `cargo build --workspace
  --release --locked` in `rust\` and copies `rust\target\release\potflix.exe` to
  `dist\potflix.exe`.
- `-Selftest` — runs `dist\potflix.exe --selftest` and requires the success
  marker `M1 SELFTEST PASS`. It needs the binary; run with `-Build` to produce it
  from current source.

Per-crate gates:

```powershell
cargo test  -p <crate> --release
cargo clippy -p <crate> --all-targets -- -D warnings
cargo fmt   -p <crate> -- --check
```

Tests are pure logic — no live PotPlayer or Discord needed. Diagnose one title
with the `metacheck` CLI.

## Verification

All of the following were run on 2026-10-13 on this machine and passed:

- `pwsh -ExecutionPolicy Bypass -File tools\check.ps1` → PASS (clippy `-D warnings`, `cargo test --workspace --release --locked`, titleclean parity).
- `pwsh -ExecutionPolicy Bypass -File tools\check.ps1 -Build -Selftest` → PASS; `dist\potflix.exe` produced, `--selftest` emitted `M1 SELFTEST PASS` (exit 0).
- Build artifact: `dist\potflix.exe`, 3,593,216 bytes, SHA-256 `C73257FD92A7E9E7D1AFA347C9C25CAE221E7F6B7B6FEBE57A58159A5616C2F6` (rebuild 2026-10-13).
- Go frozen reference: `go vet ./...` and `go test ./... -count=1` PASS (10 packages).
- Hygiene: `gitleaks detect --no-git` reports 2 findings, both false positives in ignored build artifacts under `rust\target\`; tracked source is clean. The two content hits (a client id in `tools\epname-fix-report.md`, and a real first name in a test doc-example path in `rust\crates\store\src\sys\windows.rs`) were redacted before publication.
- Rust `target\` and `dist\` remain untracked (gitignored).

The self-test is headless: it proves the hidden window + tray add, log
rotation, second-instance lock, and the full IPC round-trip against an
in-process fake Discord pipe, with no live PotPlayer or Discord.

## The parity contract

This rewrite's core claim: the Rust title cleaner reproduces the frozen Go
implementation's output **byte-for-byte**. On each corpus the two binaries are
run with the same arguments from the repo root and their TSV outputs must hash
identically. The committed contract is [`rust\parity-golden.sha256`](./rust/parity-golden.sha256)
— a 64-hex SHA-256 per corpus, nothing else.

The corpora themselves (`tools\titles-sample4.txt`, `tools\titles-sample.txt`)
are deliberately **not committed**: they carry real media filenames from a real
library, which must not be published. The oracle therefore holds hashes only,
so a missing corpus fails the gate rather than being treated as "nothing to
compare" (`tools\check.ps1` § `Invoke-TitlecleanParity`). Regenerate the oracle
from the frozen Go reference with the commands in the oracle's own header
comment.

## Windows-specific notes

These are load-bearing for the port and easy to break:

- The `PAINTSTRUCT` mirror is **72 bytes on amd64**. 68 is the sum of the field
  widths, not the ABI size; Go rounds to the 8-byte alignment and reports 72,
  exactly like the Windows header. A 68-byte buffer is a stack overwrite by
  `BeginPaint`, so 72 is what the port asserts.
- All UI geometry is integer `scalePx = (v*dpi + 48) / 96` from a 460x680
  logical design, and font heights are negative. An `f32`/`f64` scale would
  change hit-tests versus paint, so the port keeps `i32` everywhere.
- One OS thread owns the tray and main window procedures and the `GetMessageW`
  message loop; workers (poll, scan, report) never touch the UI — their payloads
  ride mutex-guarded snapshots and repaint only via `InvalidateRect` plus an
  async `PostMessageW`.

## Dependencies

Every direct dependency is pinned to an exact version and `rust\Cargo.lock` is
committed (never gitignored); `--locked` is the build norm. Deliberate
exclusions, recorded in `rust\Cargo.toml`:

- **No third-party Unicode / word-segmentation crate.** `titleclean` must
  reproduce the JDK-11 tables exactly, so it carries that data ported verbatim
  from `go\internal\titleclean`; a third-party Unicode table may not enter the
  graph. The regex matcher is part of the port for the same reason.
- `ureq` with `native-tls` (SChannel on Windows), so HTTPS needs no C toolchain
  and no OpenSSL; rustls/ring is deliberately not in the graph.
- CRC32 for the store is hand-rolled (table-driven IEEE), not a crate.

## Features

The behaviour below is the app's **intended contract**, carried over from the
Go reference and targeted by the Rust port. The Rust app builds and its headless
self-test passes (see **Verification**, below), but those gates prove wiring
and the title cleaner only — they do not prove feature-level behaviour. The
features were implemented for the frozen Go reference and are carried into the Rust
port unchanged, so they are expected to behave as described, but they are not
re-verified live against PotPlayer or Discord on the Rust app, and no CI runs them.

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

Prefer an installer to the manual build? `tools\make-installer.ps1` produces
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

## Deviations from the Java reference

These are the frozen Go reference's deliberate deviations from the original
Java flow; the Rust port targets the Go reference, so they are the behaviour it
carries. Mostly proven live for the Go reference:

1. Empty asset text is omitted, not sent empty. `go\internal\discord\activity.go:86-109`
   tags every text field `omitempty` (rationale at `activity.go:42-46`): Java
   set `""` for `large_text` / `small_text` (`Main.java:146,150,183`) and the
   GameSDK dropped it on the wire, so Discord never sees `"large_text":""`.
2. Exit clears the card. `go\main.go:196-199` nulls the activity before
   quitting; Java exited straight (`Main.java:544`) and left the last card on
   the profile.
3. Capped rotating debug log. `go\debuglog.go:20-23,74-97` caps
   `potflix-debug.log` at 5 MB x 3 files beside the exe; Java appended to one
   unbounded file (`Main.java:37-41`).
4. Commands wait for `READY`. `go\internal\discord\pipe.go:70-73,341-343`
   refuses a send with `ErrNotReady` until Discord's `READY` dispatch arrives
   (Discord discards anything sent earlier); Java's GameSDK hid that handshake.
5. `timestamps.end` rides while playing. `go\internal\presence\gate.go:151-160`
   projects the end from the live position and
   `go\internal\discord\activity.go:143-153` serializes start + end, so the
   Discord time bar tracks the real position; Java sent `timestamps.start`
   only (`Main.java:176`).
6. A `?`-family mark between title text folds to `:`. `go\internal\titleclean\clean.go:373,381,385-419`
   rewrites a mark (ASCII `?`, fullwidth `？`, small `﹖`) to `:` when a
   non-space character precedes it and it is not the last character of the
   extracted title, so `Spider-Man? Brand New Day (2026) ...` cleans to
   `Spider-Man: Brand New Day`; a mark that ends the title is a real question
   mark and is kept. The parity CLI therefore diverges from Java on titles
   carrying such a mark, by design.
7. The client-id lookup also searches beside the exe. `go\clientid.go:57-66,80-100`
   looks in the executable's directory before the process working directory;
   Java opened `client-id.txt` / `config.properties` against the CWD only
   (`Main.java:524-526`).
8. The settings window opens on a manual launch. `go\main.go:202-211` creates it
   and calls `showMainWindow` unless the launch carries `--minimized` (the
   tray-only autostart path); Java was headless with a tray only
   (`Main.java:540-553`) and `setUi` is a no-op sink (`Main.java:556`).
9. The activity type is `Watching`, not `Playing`. `go\internal\discord\activity.go:80`
   sets `activityTypeWatching = 3` (rationale at `activity.go:76-79`: Discord's
   time bar requires Listening or Watching); Java set `ActivityType.PLAYING`
   (`Main.java:144,159`).
10. A headless OP/ED with no name renders the tag alone. `go\internal\titleclean\clean.go:196-206`
    sets both the title and the display to the episode tag, so `ED 03.mkv`
    renders `ED 3`; Java fell through to `buildDisplay` (`Main.java:372-377`,
    `:479-483`) and duplicated it as `ED 3 — ED 3`.

## Departures from the frozen Go reference (Rust era)

The Go module above is frozen; the Rust port under `rust\` deliberately goes
further where the owner's live card proved the Go behaviour wrong:

11. Wikipedia disambiguation suffixes are stripped. `rust\crates\metadata\src\wikipedia.rs`
    (`strip_disambiguation`) drops a trailing `(film)` / `(2021 film)` /
    `(TV series)`-style parenthetical from the provider title, so the card
    shows `Ready Player One` with details `Ready Player One (2018)` instead
    of `Ready Player One (film)` / `Ready Player One (film) (2018)`. Cached
    records heal on decode (`rust\crates\metadata\src\cache.rs`,
    `decode_cache_entry`, no schema bump). Go stored the raw page title
    (`go\internal\metadata\wikipedia.go:504`).
12. Truncated card lines end cleanly. `rust\crates\presence\src\lib.rs`
    (`trim_word`, `smart_trim_synopsis`, the `large_text` build) cuts on word
    boundaries, strips trailing `, ; : - – — ·` before the terminator, and ends
    with `…` when cut; the tooltip no longer hard-cuts mid-word. Go kept the
    cut punctuation and hard-cut the tooltip
    (`go\internal\presence\presence.go:573-644,673-675`).
13. Wikipedia artwork accepts the article's own file. `rust\crates\metadata\src\wikipedia.rs`
    (`wiki_pick_poster`) falls back to a file named exactly like the page
    (`File:Ready Player One (film).png`) when no PageImages image and no
    `*poster*` file exist; Go only accepted a literal `poster` word and left
    the card on `logo`.

## Docs

- [`go/README.md`](./go/README.md) — the frozen Go reference: package layout, metadata design, scan-index tiers, rate limiter, M1–M4 status.
- [`go/internal/titleclean/README.md`](./go/internal/titleclean/README.md) — the title-cleaner port and its parity method.
- [`DECISION.md`](./DECISION.md) — why the Cleaner port chose Go, and why the app is being rewritten in Rust on a parity oracle.
- [`THIRD-PARTY-NOTICES.md`](./THIRD-PARTY-NOTICES.md) — upstream BSD-3-Clause text and the bundled logo-font notices.

## Status

**Build- and test-verified** — all gates below pass on this machine. Live
PotPlayer/Discord end-to-end is not exercised by CI; run `--selftest` for the
headless wiring check.

## Credits

- Upstream: [`ayunami2000/PotPlayer-DiscordRPC`](https://github.com/ayunami2000/PotPlayer-DiscordRPC) (BSD-3-Clause). The title-cleaner logic in Potflix derives from that project.
- Logo fonts: the files under `design\fonts\` (Archivo Black, Montserrat, Poppins, Anton, Bebas Neue, Inter), each under the SIL Open Font License 1.1, render the Potflix logo offline via `design\make_logo.py`. They are not the app's typeface and are not redistributed as one.
- Potflix is not affiliated with or endorsed by the upstream authors or the font authors.
- See [THIRD-PARTY-NOTICES.md](./THIRD-PARTY-NOTICES.md) for the full upstream and font notices.

## License

MIT — see [LICENSE](LICENSE). Potflix's own code is MIT. It is a fork of
`ayunami2000/PotPlayer-DiscordRPC` (BSD-3-Clause) — see
[THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md) for the upstream and font
notices.

<!-- nathwn12 house footer: identical in every repo. Do not edit. -->
<p align="center">
  <a href="https://github.com/nathwn12"><img src="assets/signature.svg" width="96" alt="nathwn12" /></a>
</p>
<p align="center">
  <sub>Built by <a href="https://github.com/nathwn12">nathwn12</a> · <a href="https://github.com/nathwn12?tab=repositories">All projects</a></sub>
</p>
