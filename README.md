# Potflix

**Mirror whatever plays in PotPlayer onto your Discord card.**

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

Two switches exist and nothing else is honoured:

```powershell
.\dist\potflix.exe --selftest    # headless wiring proof, no PotPlayer or Discord needed
.\dist\potflix.exe --minimized   # start silent in the tray, for autostart
```

Exit any running copy before `--selftest`: one instance runs at a time. You need no config file and no account. If you want your own Discord app name and artwork instead of the built-in presence, save your Application ID in `client-id.txt` beside the exe or export `POTFLIX_CLIENT_ID` and restart.

## What you get

You get a Discord card that reads `Watching <title>` and is never blank: filenames are cleaned into real titles (codecs, tags, and release groups stripped, years parenthesized, `SxxEyy` parsed), series cards show the episode name with a teaser line, and movie cards show title, year, genres, and length.

You get correct states without touching anything: a count-up timer while paused, Browsing and Idle when nothing plays.

You get metadata with no keys and no accounts, cached on disk for 30 days; offline, the card falls back to the clean filename title. You can export and re-import that cache as one JSON bundle.

## Requirements

You need Windows, PotPlayer, the Discord desktop app (the web client has no local presence pipe), and a Rust toolchain with `cargo` for the one-time build.

## Links

- [Repository](https://github.com/nathwn12/Potflix)
- [License](LICENSE) (MIT)
- [Changelog](CHANGELOG.md)
- [Rust port notes](RUST-PORT.md)
- [Third-party notices](THIRD-PARTY-NOTICES.md)
