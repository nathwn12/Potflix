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
- **While paused the card is parked with a count-UP pause timer; the pause path
  never clears the activity.** The paused card keeps the cleaned title/episode context and
  marks the state with a ` - Paused` suffix, sets `small_image` to `pause`, and
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
