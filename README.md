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
- **`timestamps.end` is deliberately omitted; only `start` is sent (deviation 5,
  corrected).** The original deviation 5 sent `start` and, when the duration was
  known, `end = now + (total - pos)`. That was wrong: the Discord client renders
  a COUNTDOWN from `timestamps.end` (documented in the Social SDK
  `ActivityTimestamps::End` - "the Discord client will render a countdown timer
  showing how long until the activity ends" - and confirmed by
  `discord-api-docs` PR #8431: "as long as `end` is set the timer displays the
  remaining time"), so the card showed the time REMAINING instead of elapsed.
  The owner observed it directly: the card read `11:50` while PotPlayer showed
  `00:35:22 / 00:47:12`, and `47:12 - 35:22 = 11:50`. The Go port now sends
  `timestamps.start` only, so the client counts UP from it - which is what the
  port contract's section 5 already specified (`timestamps.end` absent) and what
  the Java reference did. The send gate still computes the projected end for its
  own bookkeeping (`internal/presence/gate.go`), but it is never serialized; the
  wire decision lives in one place (`internal/discord/activity.go`,
  `newActivity`). The drift-avoiding machinery is unchanged: the tracker's seek
  debounce is trailing-edge (a suppressed seek is held and lands when the window
  expires), the gate re-anchors at once when the live position diverges from the
  last-sent anchor by more than 2 s (throttled to one per 5 s), and a 30 s
  heartbeat re-pushes a steady card. Not rate-limit mitigation: the tracker
  emits 0/min while steady, the heartbeat adds 2/min, and a scrub is ~9/min.
- **While paused the presence is cleared, so there is no timer at all.** The
  owner requires no timer while paused ("when paused there's no more 00:00 or
  timestamp"). The Discord client renders a timer for ANY activity: `start` =>
  count-up, `end` => countdown, and NEITHER => an auto-generated elapsed timer
  anchored to the last activity update (undocumented, but reported independently
  twice; one report tried `clearPresence()` then `setPresence()` and the timer
  came straight back). So omitting `timestamps` does NOT clear the timer, and
  the only way to show none is to have no presence: on pause the app sends
  `SET_ACTIVITY` with `activity: null` (the existing `ClearActivity` path) and
  sends NO paused card. The card and its timer disappear while paused; on resume
  the card is re-set immediately with a fresh `start` and no `end`. The pause
  clear and the resume re-set are part of the transition, not separate gate
  decisions, so neither can be debounced or throttled (the pause branch in
  `Gate.Decide` returns before the suppression paths, and the resume is a
  content change). Two alternatives were deliberately NOT taken: a `00:00`
  countdown via `timestamps.end = now` (a timer is still shown), and switching
  the activity type to Competing (which disables timestamps but changes the
  card's meaning).

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
