//! Non-Windows stub -- mirrors `reparse_other.go`.
//!
//! Reparse points do not exist off Windows, and the `symlink_metadata` check in
//! `check_bundle_dir` already covers symlinks on every platform.

use std::path::Path;

/// `dirHasReparseAttribute`'s non-Windows stub: always false.
pub(super) fn dir_has_reparse_attribute(_path: &Path) -> bool {
    false
}
