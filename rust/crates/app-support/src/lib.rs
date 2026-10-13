//! `app-support` -- the non-Win32 support surface split out of the Go root
//! package: client-id resolution, the settings/config store, the debug log and
//! the single-instance lock (`go\clientid.go`, `go\config.go`, `go\debuglog.go`,
//! `go\instance.go`).
//!
//! Workspace seam: no Win32, and one intra-repo edge -- `presence`, for the
//! `CardSpec` the config store embeds (`config` re-exports it) -- so the
//! settings and logging surface is usable without the window stack. The
//! single-instance lock is a plain std TCP listener on `127.0.0.1:17843` -- the
//! Go port's raw winsock syscalls need no Win32 binding table to reproduce.
//!
//! Never log the client-id VALUE: [`clientid::log_client_id_source`] is the only
//! line that may carry a client-id fact, and it carries the source name alone.

pub mod clientid;
pub mod config;
pub mod debuglog;
pub mod instance;

#[cfg(test)]
mod testutil;
