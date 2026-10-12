//! `selftest` -- mirrors the Go root file `go\selftest.go`.
//!
//! The headless `--selftest` path: prove the parts that need no user, no window
//! and no network.
//!
//! Phase 0 seam: `src\main.rs` handles `--selftest` itself (printing exactly
//! `POTFLIX RUST SCAFFOLD OK`) and deliberately calls nothing here. Once the port
//! lands, the flag is handed to this module and the scaffold string is replaced
//! by the real checks.
//!
//! TODO(seam): port `selftest.go` and re-point `main.rs` at it.
