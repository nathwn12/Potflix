//! `metadata-core` -- the shared, network-free half of the Go package
//! `go\internal\metadata`: the record types, the filename parser, candidate
//! matching, alternate titles and the request context.
//!
//! Workspace seam: split out of the single `metadata` module. The transport
//! lives in `metadata-http` and the providers, cache and resolver in
//! `metadata`. Submodule bodies keep their own doc comments.
//!
//! Nothing here opens a socket or touches the cache directory, so the two
//! provider-side seams -- Go's `Source` interface and `Options`, both of which
//! carry a `context.Context` -- stay with the resolver rather than here.

pub mod parse;
pub mod types;
// `match_` carries the trailing underscore because `match` is a Rust keyword;
// it mirrors `go\internal\metadata\match.go`.
pub mod alternate;
pub mod context;
pub mod match_;

#[cfg(test)]
mod alternate_test;
#[cfg(test)]
mod context_test;
#[cfg(test)]
mod match_test;
#[cfg(test)]
mod parse_test;
#[cfg(test)]
mod types_test;
