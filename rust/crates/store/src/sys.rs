//! The store-file open, split on the target exactly like the Go original
//! (`store_windows.go` / `store_other.go`): Windows needs `CreateFileW` for the
//! share mode, every other target is the portable `OpenOptions` open.

#[cfg(not(windows))]
mod other;
#[cfg(not(windows))]
pub(crate) use other::open_store_file;

#[cfg(windows)]
mod windows;
#[cfg(windows)]
pub(crate) use windows::open_store_file;
