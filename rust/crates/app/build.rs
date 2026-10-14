//! Embeds the Potflix icon into every binary as a Win32 resource.
//!
//! Replaces Go's `go\rsrc_windows_amd64.syso` (see `go\README.md` § Windows exe
//! icon). The icon source is the repo's shared `..\..\..\assets\icon.ico`; it is
//! copied into
//! `$OUT_DIR` and referenced by bare filename so `rc.exe` finds it either next
//! to the `.rc` or through the `/I $OUT_DIR` that `embed-resource` passes.
//!
//! Strict on purpose: if `rc.exe` is missing or fails, the build fails rather
//! than silently shipping an icon-less exe.

use std::env;
use std::fs;
use std::path::PathBuf;

/// The exe manifest: `asInvoker`, Windows 10/11 compatibility, PerMonitorV2.
/// `supportedOS` id `{8e0f7a12-bfb3-4fe8-b9a5-48fd50a15a9a}` is Windows 10/11;
/// the `dpiAwareness` element needs the `asm.v3` + SMI/2016 namespaces.
const MANIFEST_XML: &str = r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0">
  <trustInfo xmlns="urn:schemas-microsoft-com:asm.v3">
    <security>
      <requestedPrivileges>
        <requestedExecutionLevel level="asInvoker" uiAccess="false"/>
      </requestedPrivileges>
    </security>
  </trustInfo>
  <compatibility xmlns="urn:schemas-microsoft-com:compatibility.v1">
    <application>
      <supportedOS Id="{8e0f7a12-bfb3-4fe8-b9a5-48fd50a15a9a}"/>
    </application>
  </compatibility>
  <application xmlns="urn:schemas-microsoft-com:asm.v3">
    <windowsSettings>
      <dpiAwareness xmlns="http://schemas.microsoft.com/SMI/2016/WindowsSettings">PerMonitorV2</dpiAwareness>
    </windowsSettings>
  </application>
</assembly>
"#;

fn main() {
    let manifest_dir =
        PathBuf::from(env::var("CARGO_MANIFEST_DIR").expect("CARGO_MANIFEST_DIR is set by cargo"));
    // `rust\crates\app` -> `rust\crates` -> `rust` -> repo root: three levels up
    // from this crate's manifest dir (four from this file) is the repo's own
    // `assets\icon.ico` -- never `go\assets\icon.ico`.
    let icon = manifest_dir
        .parent()
        .and_then(|dir| dir.parent())
        .and_then(|dir| dir.parent())
        .expect("the app crate lives three levels below the repo root")
        .join("assets")
        .join("icon.ico");
    println!("cargo:rerun-if-changed={}", icon.display());
    println!("cargo:rerun-if-changed=build.rs");

    if !icon.is_file() {
        panic!("icon resource missing: {}", icon.display());
    }

    let out_dir = PathBuf::from(env::var("OUT_DIR").expect("OUT_DIR is set by cargo"));
    let staged_icon = out_dir.join("icon.ico");
    fs::copy(&icon, &staged_icon)
        .unwrap_or_else(|err| panic!("cannot stage {}: {err}", icon.display()));

    // Resource id 1 -- the first ICON group, which is what Explorer and the tray
    // loader pick (`win32.go` iconResourceVersion = 0x00030000). Resource id 1
    // type 24 is the exe manifest: PerMonitorV2 DPI awareness (no Toolkit, no
    // runtime call -- the OS reads it at process start) so the owner-drawn
    // client area gets real per-monitor pixels at 100-200% instead of a
    // bitmap-stretched frame. Go declares no manifest (system-default
    // awareness); the layout math is DPI-parameterised either way, so this
    // changes crispness, not layout or behaviour.
    let manifest = out_dir.join("potflix.manifest");
    fs::write(&manifest, MANIFEST_XML).expect("cannot write the generated manifest");
    let rc = out_dir.join("potflix.rc");
    fs::write(&rc, "1 ICON \"icon.ico\"\n1 24 \"potflix.manifest\"\n")
        .expect("cannot write the generated .rc");

    match embed_resource::compile(&rc, embed_resource::NONE) {
        embed_resource::CompilationResult::Ok => {}
        other => panic!("icon embedding failed: {other}"),
    }
}
