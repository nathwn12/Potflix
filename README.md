<p align="center">
  <img src="assets/icon.ico" width="120" height="120" alt="Potflix icon" />
</p>
<h1 align="center">Potflix</h1>
<p align="center"><strong>Mirror whatever plays in PotPlayer onto your Discord card.</strong></p>

## Install

You build from source. On Windows, with a Rust toolchain installed:

```powershell
git clone https://github.com/nathwn12/Potflix.git
cd Potflix
powershell -ExecutionPolicy Bypass -File tools\rust-build.ps1
.\dist\potflix.exe
```

First run opens the settings window: add a media folder, then Scan. Keep PotPlayer playing and the Discord desktop app open and your card goes live.

## Use it

Run `.\dist\potflix.exe` (or `run.bat`) and look for the tray icon. Right-click it for Open and Exit; closing the window parks it back in the tray.

You add library folders with Add folder, keep them fresh with Scan for new files or Refresh for a full re-walk, and preview the card in the Card view before it reaches Discord.

While something plays and the card shows the wrong title, use Fix match…: type the name (plus the year when you know it), Search, pick the candidate, choose this season or the whole show, and Match. The match is stored on your machine and wins from then on. The folder list answers Up/Down/Home/End, and Enter opens the selected folder.

Two switches exist and nothing else is honoured:

```powershell
.\dist\potflix.exe --selftest    # headless wiring proof, no PotPlayer or Discord needed
.\dist\potflix.exe --minimized   # start silent in the tray, for autostart
```

Exit any running copy before `--selftest`: one instance runs at a time. You need no config file and no account. If you want your own Discord app name and artwork instead of the built-in presence, save your Application ID in `client-id.txt` beside the exe or export `POTFLIX_CLIENT_ID` and restart.

## What you get

You get a Discord card whose header is the title and nothing else: filenames are cleaned into real titles (codecs, tags, and release groups stripped, years parenthesized, `SxxEyy` parsed), and no body line from a current preset ever repeats the header (a retired Title/Series line still in flight from an older build renders its stored line until it migrates to Default on load and save -- see the Card view note below). Series cards show the episode title on the first line (empty when no episode title is known -- never the show name) and `SxxExx - synopsis` on the second (the filename tag, then the clean title unless it repeats the header -- then the line stays empty for a deliberate one-line card, `Paused` when paused -- when there is no synopsis). Movie cards show the synopsis on the first line (the year when there is no synopsis and a year is known, else genres and length, else empty) and, when a synopsis is known, `Year · Genres · Runtime` on the second -- genres and length beside a year-showing first line, empty beside a genres/length first line, never the same text twice, and with no tooltip, since the synopsis already sits on the first line.

You get correct states without touching anything: a count-up timer while paused, and a cleared card when nothing plays -- no `Browsing`/`Idle` activity, no `Watching PotFlix` header.

The Card view previews exactly what Discord gets and follows the live card's kind on its own: there is no Series/Movie toggle, there is no Details row (line 1 is fixed by the shipped mapping), State offers Default, Tag, Synopsis, or Minimal, and a Title/Series/Episode choice saved by an older build quietly becomes Default.

You get metadata with no keys and no accounts, cached on disk for 30 days; offline, the card falls back to the clean filename title. You can export and re-import that cache as one JSON bundle.

## Requirements

You need Windows, PotPlayer, the Discord desktop app (the web client has no local presence pipe), and a Rust toolchain with `cargo` for the one-time build.

## Links

- [Repository](https://github.com/nathwn12/Potflix)
- [Changelog](CHANGELOG.md)
- [Rust port notes](RUST-PORT.md)
- [Third-party notices](THIRD-PARTY-NOTICES.md)
- [Issues](https://github.com/nathwn12/Potflix/issues)
- [Releases](https://github.com/nathwn12/Potflix/releases)
- [License](LICENSE) (MIT)

<!-- nathwn12 house footer: identical in every repo. Do not edit. -->
<p align="center">
  <a href="https://github.com/nathwn12"><img src="assets/signature.svg" width="96" alt="nathwn12" /></a>
</p>
<p align="center">
  <sub>Built by <a href="https://github.com/nathwn12">nathwn12</a> · <a href="https://github.com/nathwn12?tab=repositories">All projects</a></sub>
</p>
