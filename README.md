# Potflix — Discord presence for PotPlayer

Netflix-styled fork of `ayunami2000/PotPlayer-DiscordRPC`. Same engine
(PotPlayer polling + Discord GameSDK), new face:

- **Clean titles always** — `Breaking.Bad.S01E02.1080p.WEB-DL.DD5.1.x264-CtrlHD.mkv`
  shows as **Breaking Bad — S1 E2**. Codecs, tags, release groups stripped,
  years parenthesized (`Dune Part Two (2024)`), `SxxEyy` parsed.
- **Standardized presence** — `PLAYING` type, `Details` = clean title,
  `State` = `Playing • S1 E2 • 12:34 / 45:00` / `Paused • …` / `Stopped`.
- **Netflix look** — red-on-black tray icon + dark status window
  (double-click the tray icon).
- **Your own app name/logo (optional)** — without this, presence uses the
  upstream Discord app (works out of the box, art keys `logo`/`play`/`pause`/`stop`).

## Run

Double-click `run.bat` (or the Desktop shortcut), or:

```powershell
javaw -jar dist\Potflix.jar
```

Needs: PotPlayer open, Discord desktop open, Java 11+.

## Use your own Discord app (2 min, for custom name + logo)

1. https://discord.com/developers/applications → New Application named **Potflix**
2. Rich Presence → Art Assets → upload `assets/discord/logo.png` as `logo`,
   `play.png` / `pause.png` / `stop.png` with those exact names
3. Copy the Application ID → save it into `client-id.txt` (digits only)
   or set env `POTFLIX_CLIENT_ID`, then restart Potflix

## Dev

No Gradle needed — `javac` against the upstream fat jar:

```powershell
powershell -ExecutionPolicy Bypass -File tools\build.ps1
```

Outputs `dist\Potflix.jar`. Plain-JVM tests (no Discord, no PotPlayer) live in
`tools\` and run against `build\classes`:

```powershell
cd tools
# cleaner playground
javac -cp "$env:TEMP\pot-rpc\PotPlayer-DiscordRPC.jar;..\build\classes" -d ..\build\classes TestClean.java
java -cp "..\build\classes;$env:TEMP\pot-rpc\PotPlayer-DiscordRPC.jar" TestClean
# presence timing/state logic (elapsed-timer fix)
javac -cp "$env:TEMP\pot-rpc\PotPlayer-DiscordRPC.jar;..\build\classes" -d ..\build\classes TestPresence.java
java -cp "..\build\classes;$env:TEMP\pot-rpc\PotPlayer-DiscordRPC.jar" TestPresence
```

Upstream: https://github.com/ayunami2000/PotPlayer-DiscordRPC (MIT-style? check LICENSE before publishing).
