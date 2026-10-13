//! `player` -- mirrors the Go package `go\internal\player`.
//!
//! PotPlayer window discovery (`EnumWindows`), the `WM_USER` time/status
//! queries and the `WM_COPYDATA` play-file path query. Go files: `window.go`,
//! `control.go`, `pathquery.go`; all Win32 through `windows-sys`, never a C
//! toolchain.

pub mod control;
pub mod pathquery;
pub mod window;
