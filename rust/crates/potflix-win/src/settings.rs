//! `settings` -- mirrors the settings surface split out of the Go root package.
//!
//! In Go the settings window and its sections live in `window.go` /
//! `settings_sections.go`; the persisted values they edit are `config.go`. This
//! module is the Rust seam for the window-side settings surface so the port does
//! not have to keep `window` monolithic.
//!
//! TODO(seam): port the settings sections here (folders list, Scan / Refresh,
//! library export/import, `Clear metadata` / `Clear index`, `Report`, startup,
//! scan-speed segments), reading and writing through `config`.
