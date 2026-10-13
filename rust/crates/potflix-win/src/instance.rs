//! `instance` -- mirrors the Go root file `go\instance.go`.
//!
//! The single-instance lock: a named mutex so a second launch hands off to the
//! running tray app instead of starting a second copy.
//!
//! TODO(seam): port `instance.go` on top of `win32` (`CreateMutexW` +
//! `GetLastError` == `ERROR_ALREADY_EXISTS`).
