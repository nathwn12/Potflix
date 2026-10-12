//! Potflix tray application -- mirrors the Go root package `main` (`go\main.go`).
//!
//! Console-less by design: `#![windows_subsystem = "windows"]` is the equivalent
//! of Go's `-ldflags "-H=windowsgui"`, so `--selftest` output only reaches a
//! parent that redirects stdout -- exactly like the Go build.
//!
//! Phase 0 seam: argv handling only. No stub module is called from a live path.
#![windows_subsystem = "windows"]

use std::io::Write;

fn main() {
    let selftest = std::env::args().skip(1).any(|arg| arg == "--selftest");

    if selftest {
        // stdout may be a pipe/file handle inherited from the parent (the app has
        // no console of its own); a write that cannot be delivered is ignored.
        let mut stdout = std::io::stdout();
        let _ = writeln!(stdout, "POTFLIX RUST SCAFFOLD OK");
        let _ = stdout.flush();
    }

    // Both paths exit 0: `--selftest` proves the scaffold runs, any other argv is
    // inert until the port lands.
    std::process::exit(0);
}
