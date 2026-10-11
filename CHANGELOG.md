# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Stdlib-only Go tray app (module `potflix`): single-instance lock, hidden
  tray window, capped rotating debug log, and headless `--selftest` wiring
  proof; the Java launcher flow is retired to a frozen reference.
- Media-title cleaner ported from the Java reference and proven byte-identical
  on the full corpus and holdout, including the P1-P12 episode naming patterns.
- Keyless canonical series/episode titles from free sources needing no
  account (TVmaze for TV, AniList + Jikan for anime, iTunes for movies),
  cached permanently on disk with 30-day negative entries; offline-safe
  fallback to the clean filename title.
- Library window: media folders with incremental `Scan` (per-directory skip
  index), full `Refresh`, and metadata-only JSON export/import bundles.
- Scan-speed profiles (`Turbo` / `Balanced` / `Gentle`) applied live with no
  restart, plus a Start-with-Windows toggle and card line presets.
- Netflix-style presence card: clean title header, episode name and tag,
  ` - Paused` count-up card held quietly while paused, `Browsing` / `Idle`
  when idle, `READY`-gated sends, and clear-activity-on-exit.
- Diagnostic CLIs: `metacheck` (resolve one title), `metawarm` (pre-warm the
  cache over a library walk), `titleclean` (corpus parity TSV).
- Inno Setup installer (`tools/potflix.iss`) and embedded exe/tray icon
  resources.

### Fixed

- Arc-suffixed series filenames (`Show - Arc - NN`, e.g. `Dragon Ball
  Super - Beerus - 01`) now resolve: when the full title matches nothing,
  the lookup normalizes the separator (` - ` -> `: `) instead of truncating
  the name, so the card shows the 2026 series, `The God Who Destroys`, and
  artwork instead of the 2015 series or a bare absolute tag.
