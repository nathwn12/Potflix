//! Portable store-file open -- the port of `go\internal\store\store_other.go`.
//!
//! A POSIX unlink does not block on an open handle, so the portable open
//! already meets the removal contract the crate doc states; it keeps the crate
//! liftable off Windows. `O_RDWR|O_CREATE` and never truncate, as the Go
//! original.

use std::fs::{File, OpenOptions};
use std::io;
use std::path::Path;

pub(crate) fn open_store_file(path: &Path) -> io::Result<File> {
    OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .open(path)
}
