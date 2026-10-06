# Potflix - Discord presence for PotPlayer

Netflix-styled fork of `ayunami2000/PotPlayer-DiscordRPC`. The app is a
headless Windows tray program that shows the media currently playing in
PotPlayer as a Discord Rich Presence card.

- **Clean titles always** - `Breaking.Bad.S01E02.1080p.WEB-DL.DD5.1.x264-CtrlHD.mkv`
  shows as **Breaking Bad - S1 E2**. Codecs, tags, release groups stripped,
  years parenthesized (`Dune Part Two (2024)`), `SxxEyy` parsed.
- **Standardized presence** - `PLAYING` type, `Details` = clean title,
  `State` = `Playing S1 E2 12:34 / 45:00` / `Paused .` / `Stopped`.
- **Netflix look** - red-on-black tray icon; right-click it for **Exit Potflix**.
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
directory and at any commit. Requires Go 1.27+; no network access.

The former Java flow - `tools\build.ps1` downloading an upstream fat jar from
the mutable release tag `v1` and re-packaging it with `javac` - is **retired**
and is no longer a build path. The Java sources under `src\` stay as the
reference implementation the Go port was proven against; see `DECISION.md`.

## Use your own Discord app (2 min, for custom name + logo)

1. https://discord.com/developers/applications -> New Application named **Potflix**
2. Rich Presence -> Art Assets -> upload `assets/discord/logo.png` as `logo`,
   `play.png` / `pause.png` / `stop.png` with those exact names
3. Copy the Application ID -> save it into `client-id.txt` (digits only)
   or set env `POTFLIX_CLIENT_ID`, then restart Potflix

## Deviations from the Java reference

Recorded deliberately; each is a behaviour change, not a parity claim.

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
- **`timestamps.end` is sent while playing (deviation 5).** The Java reference
  and fleet decision 7 set only `timestamps.start` (contract section 5). The Go
  port sends `start` and, when the duration is known, `end = now + (total - pos)`
  (`end` is omitted when `total` is 0). This gives Discord's progress bar a real
  duration and makes every push self-correcting: Discord extrapolates the clock
  between pushes instead of freezing on a stale anchor. The send gate
  (`internal/presence/gate.go`) re-anchors at once when the live position
  diverges from the last-sent anchor by more than 2 s (throttled to one per 5 s)
  and re-pushes on a 30 s heartbeat; the tracker's seek debounce is trailing-edge
  so a suppressed seek is held and lands when the window expires rather than
  being dropped forever. This is not rate-limit mitigation: the tracker emits
  0/min while steady (change/seek events only), the 30 s heartbeat adds 2/min,
  and a scrub is ~9/min - all far below Discord's update limit.

## Dev

Module and package layout: `go\README.md`. Tests and vet:

```powershell
cd go
go vet ./...
go test ./... -count=1
```

`.\dist\potflix.exe --selftest` proves the headless wiring (window/icon/tray
add, log rotation, second-instance lock, and a full IPC round-trip against an
in-process fake pipe).

Upstream: https://github.com/ayunami2000/PotPlayer-DiscordRPC (MIT-style? check LICENSE before publishing).
