//! Serde helpers for the provider wire shapes.
//!
//! Go's `encoding/json` is deliberately lenient: unmarshalling `null` into a
//! non-pointer field is a NO-OP that leaves the zero value, while `serde` errors
//! on `null` for `String`, `Vec` and the numeric types. The providers depend on
//! that leniency -- TVmaze really does serve `"summary":null` and
//! `"premiered":null` for unreleased shows -- so every non-`Option` field of a
//! provider response struct goes through [`null_default`], which reads the value
//! as an `Option` and falls back to the type's default. A missing field is
//! handled by `#[serde(default)]` before this runs.

use serde::{Deserialize, Deserializer};

/// Go's null-into-zero-value behaviour: `null` becomes `T::default()`, any
/// other value deserializes as `T`.
pub(crate) fn null_default<'de, D, T>(deserializer: D) -> Result<T, D::Error>
where
    D: Deserializer<'de>,
    T: Deserialize<'de> + Default,
{
    Ok(Option::<T>::deserialize(deserializer)?.unwrap_or_default())
}
