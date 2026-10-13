//! Potflix (Rust) -- crate root and the FROZEN module map.
//!
//! Port of the Go module `potflix` (`..\go`; package layout documented in
//! `go\README.md`). One Rust module mirrors one Go package; the submodules under
//! `metadata`, `library`, `discord` and `presence` mirror the Go files that
//! carry separable seams.
//!
//! # Ownership -- read before editing
//!
//! This file is the SINGLE WRITER for module declarations. It was written once by
//! the Phase 0 seam; later workers fill in module bodies and never edit the
//! declarations here. Adding, renaming, or removing a `mod` line is a seam
//! change, not a porting change.
//!
//! # Deliberate absences
//!
//! No Unicode / word-segmentation crate: [`titleclean`] reproduces the JDK-11
//! tables exactly and ports that data verbatim from `go\internal\titleclean`, so
//! no third-party Unicode tables may enter the dependency graph.
//!
//! Phase 0 note: every module below is a comment-only stub. Nothing in this crate
//! is reachable from a live path yet, and `src\main.rs` calls none of it.
//!
//! Workspace seam: the module map above is now one member crate per Go package
//! under `crates\*`, and each crate's own `src\lib.rs` is the single writer for
//! its declarations. This crate keeps only the app's own seam: `main`, which
//! owns the workspace's single binary, plus `selftest`.

pub mod selftest;
