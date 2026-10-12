//! `tray` -- mirrors the Go root file `go\tray.go`.
//!
//! The hidden tray window and its `Shell_NotifyIconW` icon, plus the popup menu
//! (`Open Potflix`, `Exit Potflix`). The icon bytes are the repo's own
//! `assets\icon.ico`, compiled in with `include_bytes!` -- the equivalent of Go's
//! `//go:embed assets/icon.ico` in `go\embed.go` -- and handed to
//! `CreateIconFromResourceEx` as the RAW DIB frame at the ICO's
//! `dwImageOffset`, never as the ICO container. No icon file ships beside the
//! exe. (`rust\crates\app\build.rs` embeds the same file as a Win32 resource so
//! Explorer shows it; the tray does not read that resource.)
//!
//! Contract (`go\tray.go:11-16`): the tray is built before config and the
//! message loop; the tooltip is `Potflix`; the menu carries `Open Potflix` and
//! `Exit Potflix`; an add failure is non-fatal to the caller.
//!
//! # Thread law
//!
//! [`create_tray_window`] MUST be called on the same OS thread that later runs
//! [`message_loop`] (`go\tray.go:31-33`): window messages are delivered to the
//! thread that created the window, so a tray window created on one thread and
//! pumped on another never sees its `WM_APP+1` callback and the icon goes dead.
//! The Go app pins that thread with `runtime.LockOSThread` (`go\main.go:140-143`);
//! in Rust, create the window and pump from one thread.
//!
//! # Deliberate legacy shape
//!
//! `NIM_SETVERSION` is never called (`go\tray.go:139-141`), so the shell uses
//! the legacy `uVersion 0` callback: `wParam` = icon id, `lParam` = the mouse
//! message. [`tray_mouse_message`] therefore reads `LOWORD(lParam)` first and
//! falls back to `LOWORD(wParam)`; reading only `wParam` was a real bug
//! (`go\tray.go:143-147`).

use core::ptr::{null, null_mut};
use std::fmt;
use std::sync::OnceLock;

use app_support::debuglog::Logger;
use windows_sys::Win32::Foundation::{GetLastError, HWND, LPARAM, LRESULT, POINT, WPARAM};
use windows_sys::Win32::UI::Shell::NOTIFYICONDATAW;
use windows_sys::Win32::UI::WindowsAndMessaging::{HICON, MSG, WNDCLASSEXW};

use crate::win32::{
    AppendMenuW, CreateIconFromResourceEx, CreatePopupMenu, CreateWindowExW, DefWindowProcW,
    DestroyMenu, DestroyWindow, DispatchMessageW, GetCursorPos, GetMessageW, GetModuleHandleW,
    ICON_RESOURCE_VERSION, MF_STRING, MsgT, NIF_ICON, NIF_MESSAGE, NIF_TIP, NIM_ADD, NIM_DELETE,
    NotifyIconData, Point, PostMessageW, PostQuitMessage, RegisterClassExW, SetForegroundWindow,
    Shell_NotifyIconW, TPM_BOTTOM_ALIGN, TPM_RETURN_CMD, TPM_RIGHT_BUTTON, TrackPopupMenu,
    TranslateMessage, WM_APP_QUIT, WM_APP_TRAY, WM_CLOSE, WM_CONTEXT_MENU, WM_DESTROY, WM_NULL,
    WM_RBUTTON_UP, WndClassEx, WndProc, utf16_z,
};

/// Go `trayClassName` (`go\tray.go:18`).
pub const TRAY_CLASS_NAME: &str = "PotflixTrayWnd";

/// Go `trayUID` (`go\tray.go:19`).
pub const TRAY_UID: u32 = 1;

/// The tooltip text: `copy(nid.szTip[:], syscall.StringToUTF16("Potflix"))`
/// (`go\tray.go:109`).
pub const TRAY_TIP: &str = "Potflix";

/// Menu item ids (`go\tray.go:194-197`).
pub const MENU_ITEM_OPEN: usize = 2;
pub const MENU_ITEM_EXIT: usize = 1;

/// Go `iconICO` (`go\embed.go:10`, `//go:embed assets/icon.ico`): the repo's
/// `assets\icon.ico` -- 262,206 bytes, a single 256x256 DIB frame. `tools\check.ps1`
/// asserts that file is byte-identical to `go\assets\icon.ico`.
pub const ICON_ICO: &[u8] = include_bytes!("../../../../assets/icon.ico");

/// Go `appLog *logger` (`go\tray.go:22`): the process logger, read from the
/// window procedure and the menu callbacks. Installed once by the app seam; a
/// `&'static` because Go held a pointer, not a value.
static APP_LOG: OnceLock<&'static Logger> = OnceLock::new();

/// Installs the process logger (Go's `appLog = ...`). Returns the rejected
/// logger if one is already installed.
pub fn set_logger(logger: &'static Logger) -> Result<(), &'static Logger> {
    APP_LOG.set(logger)
}

/// Go `showMainWindow()` (`go\window.go`) -- the main window is ported by the
/// `window` seam, so the tray reaches it through this slot instead of a
/// cross-module call. Until the seam installs a function, `Open Potflix` logs
/// and does nothing.
static SHOW_MAIN_WINDOW: OnceLock<fn()> = OnceLock::new();

/// Installs the main-window seam. Returns the rejected function if one is
/// already installed.
pub fn set_show_main_window(open: fn()) -> Result<(), fn()> {
    SHOW_MAIN_WINDOW.set(open)
}

/// Go `appLog.logf(...)` (`go\tray.go:28, :217`): a no-op until the app seam
/// installs the logger.
fn log_line(message: &str) {
    if let Some(logger) = APP_LOG.get() {
        logger.log(message);
    }
}

/// Go `showMainWindow()`: call the installed seam, or report the gap once.
fn show_main_window() {
    match SHOW_MAIN_WINDOW.get() {
        Some(open) => open(),
        None => log_line("open requested (tray menu) - main window seam not installed"),
    }
}

/// The tray window and its icon, Go's `(hwnd, icon, added)` return.
#[derive(Clone, Copy, Debug)]
pub struct TrayWindow {
    pub hwnd: HWND,
    pub icon: HICON,
    /// Go `added`: `NIM_ADD` succeeded. Non-fatal -- the caller decides whether
    /// an icon-less tray is worth continuing with (`go\main.go:148-153`).
    pub added: bool,
}

/// Why the tray window could not be created. Go returned the bare `syscall.Errno`
/// from the failing call; the code here is the same `GetLastError` value.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum TrayError {
    RegisterClass(u32),
    CreateWindow(u32),
}

impl fmt::Display for TrayError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            TrayError::RegisterClass(code) => write!(f, "RegisterClassExW failed: {code}"),
            TrayError::CreateWindow(code) => write!(f, "CreateWindowExW failed: {code}"),
        }
    }
}

impl std::error::Error for TrayError {}

/// Why the embedded icon could not be turned into an `HICON`. The texts match
/// Go's `loadEmbeddedIconDiag` errors (`go\tray.go:73-99`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum IconError {
    /// `icon bytes too short: %d`
    TooShort(usize),
    /// `ico frame count = %d`
    FrameCount(u16),
    /// `bad ico entry: off=%d size=%d total=%d`
    BadEntry {
        off: usize,
        size: usize,
        total: usize,
    },
    /// `CreateIconFromResourceEx(off=%d size=%d) failed: %v`
    CreateFailed {
        off: usize,
        size: usize,
        last_error: u32,
    },
}

impl fmt::Display for IconError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            IconError::TooShort(len) => write!(f, "icon bytes too short: {len}"),
            IconError::FrameCount(count) => write!(f, "ico frame count = {count}"),
            IconError::BadEntry { off, size, total } => {
                write!(f, "bad ico entry: off={off} size={size} total={total}")
            }
            IconError::CreateFailed {
                off,
                size,
                last_error,
            } => write!(
                f,
                "CreateIconFromResourceEx(off={off} size={size}) failed: {last_error}"
            ),
        }
    }
}

impl std::error::Error for IconError {}

/// `createTrayWindow` (`go\tray.go:34-63`): register the class, create the
/// hidden window, load the embedded icon, add the notify icon.
///
/// Same OS thread as [`message_loop`], always -- see the module doc.
pub fn create_tray_window() -> Result<TrayWindow, TrayError> {
    // GetModuleHandleW(0): the module handle of the running exe, which doubles
    // as the HINSTANCE both window calls want.
    let hinstance = unsafe { GetModuleHandleW(null()) };
    let class_name = utf16_z(TRAY_CLASS_NAME);

    let proc: WndProc = wnd_proc;
    let wc = WndClassEx {
        cb_size: core::mem::size_of::<WndClassEx>() as u32,
        style: 0,
        lpfn_wnd_proc: proc as usize,
        cb_cls_extra: 0,
        cb_wnd_extra: 0,
        h_instance: hinstance as usize,
        h_icon: 0,
        h_cursor: 0,
        hbr_background: 0,
        lpsz_menu_name: null(),
        lpsz_class_name: class_name.as_ptr(),
        h_icon_sm: 0,
    };
    // The mirror is byte-identical to `WNDCLASSEXW` (asserted in `crate::win32`).
    let atom = unsafe { RegisterClassExW(&wc as *const WndClassEx as *const WNDCLASSEXW) };
    if atom == 0 {
        return Err(TrayError::RegisterClass(unsafe { GetLastError() }));
    }

    // style 0 = hidden, never shown; zero size.
    let hwnd = unsafe {
        CreateWindowExW(
            0,
            class_name.as_ptr(),
            null(),
            0,
            0,
            0,
            0,
            0,
            null_mut(),
            null_mut(),
            hinstance,
            null(),
        )
    };
    if hwnd.is_null() {
        return Err(TrayError::CreateWindow(unsafe { GetLastError() }));
    }

    let icon = load_embedded_icon();
    let added = shell_notify_icon(NIM_ADD, &make_notify_icon_data(hwnd, icon));
    Ok(TrayWindow { hwnd, icon, added })
}

/// `loadEmbeddedIcon` (`go\tray.go:68-71`): the icon, or a null handle when the
/// bytes do not parse (Go returned 0 and dropped the error).
pub fn load_embedded_icon() -> HICON {
    match load_embedded_icon_diag() {
        Ok(icon) => icon,
        Err(_) => null_mut(),
    }
}

/// `loadEmbeddedIconDiag` (`go\tray.go:73-99`): build an `HICON` straight from
/// the embedded `.ico` bytes. The file holds one BMP/DIB frame, which is exactly
/// the "resource bits" shape `CreateIconFromResourceEx` consumes, so no temp
/// file is needed.
pub fn load_embedded_icon_diag() -> Result<HICON, IconError> {
    let (off, size) = parse_icon_entry(ICON_ICO)?;
    let frame = &ICON_ICO[off..off + size]; // the raw DIB frame, not the ICO container
    let icon = unsafe {
        CreateIconFromResourceEx(
            frame.as_ptr(),
            size as u32,
            1, // fIcon
            ICON_RESOURCE_VERSION,
            0,
            0,
            0,
        )
    };
    if icon.is_null() {
        return Err(IconError::CreateFailed {
            off,
            size,
            last_error: unsafe { GetLastError() },
        });
    }
    Ok(icon)
}

/// The ICO directory's first entry as `(dwImageOffset, dwBytesInRes)`
/// (`go\tray.go:74-87`): frame count at 4, `dwBytesInRes` at `6+8`,
/// `dwImageOffset` at `6+12`, with Go's three bounds checks.
pub fn parse_icon_entry(bytes: &[u8]) -> Result<(usize, usize), IconError> {
    const ENTRY_SIZE: usize = 16;
    if bytes.len() < 6 + ENTRY_SIZE {
        return Err(IconError::TooShort(bytes.len()));
    }
    let count = u16::from_le_bytes([bytes[4], bytes[5]]);
    if count < 1 {
        return Err(IconError::FrameCount(count));
    }
    let size = u32::from_le_bytes([bytes[14], bytes[15], bytes[16], bytes[17]]) as usize;
    let off = u32::from_le_bytes([bytes[18], bytes[19], bytes[20], bytes[21]]) as usize;
    if off < 6 + ENTRY_SIZE || size == 0 || off + size > bytes.len() {
        return Err(IconError::BadEntry {
            off,
            size,
            total: bytes.len(),
        });
    }
    Ok((off, size))
}

/// `makeNotifyIconData` (`go\tray.go:101-111`).
///
/// `cbSize` is the COMPILED struct size, never a literal: it must equal what the
/// shell expects for the Vista+ layout or `NIM_ADD` fails silently. The size is
/// asserted in `crate::win32` (976 on amd64) and again in this module's tests.
pub fn make_notify_icon_data(hwnd: HWND, icon: HICON) -> NotifyIconData {
    let tip = utf16_z(TRAY_TIP);
    let mut sz_tip = [0u16; 128];
    sz_tip[..tip.len()].copy_from_slice(&tip);
    NotifyIconData {
        cb_size: core::mem::size_of::<NotifyIconData>() as u32,
        h_wnd: hwnd as usize,
        u_id: TRAY_UID,
        u_flags: NIF_MESSAGE | NIF_ICON | NIF_TIP,
        u_callback_message: WM_APP_TRAY,
        h_icon: icon as usize,
        sz_tip,
        ..Default::default()
    }
}

/// `shellNotifyIcon` (`go\tray.go:113-116`).
///
/// `NIM_SETVERSION` is deliberately never called (see the module doc).
pub fn shell_notify_icon(verb: u32, nid: &NotifyIconData) -> bool {
    // The mirror is byte-identical to `NOTIFYICONDATAW` (asserted in `crate::win32`).
    unsafe { Shell_NotifyIconW(verb, nid as *const NotifyIconData as *const NOTIFYICONDATAW) != 0 }
}

/// `trayDelete` (`go\tray.go:118-120`): `NIM_DELETE` with a zero icon. Only the
/// selftest path calls this; the exit path relies on process teardown.
pub fn tray_delete(hwnd: HWND) {
    shell_notify_icon(NIM_DELETE, &make_notify_icon_data(hwnd, null_mut()));
}

/// `messageLoop` (`go\tray.go:123-136`): `GetMessageW` / `TranslateMessage` /
/// `DispatchMessageW` until `WM_QUIT`.
pub fn message_loop() {
    let mut msg = MsgT::default();
    loop {
        // The mirror is byte-identical to `MSG` (asserted in `crate::win32`).
        let r = unsafe { GetMessageW(&mut msg as *mut MsgT as *mut MSG, null_mut(), 0, 0) };
        if r == -1 {
            return; // GetMessage error
        }
        if r == 0 {
            return; // WM_QUIT
        }
        unsafe {
            TranslateMessage(&msg as *const MsgT as *const MSG);
            DispatchMessageW(&msg as *const MsgT as *const MSG);
        }
    }
}

/// `trayMouseMessage` (`go\tray.go:149-157`): the mouse/context message from a
/// `Shell_NotifyIcon` callback, or [`WM_NULL`] when neither word is a
/// menu-opening message.
///
/// `LOWORD(lParam)` is read first because that is what the shell sends without
/// `NIM_SETVERSION`; `LOWORD(wParam)` is a defensive fallback. Only the low word
/// is read, so `lParam`'s sign bit (Go passed `uintptr`, Win32 passes `LPARAM`)
/// cannot matter.
pub fn tray_mouse_message(wparam: WPARAM, lparam: LPARAM) -> u16 {
    let low = (lparam as usize & 0xFFFF) as u16;
    if opens_tray_menu(low) {
        return low;
    }
    let low = (wparam & 0xFFFF) as u16;
    if opens_tray_menu(low) {
        return low;
    }
    WM_NULL as u16
}

/// `opensTrayMenu` (`go\tray.go:160-162`).
pub fn opens_tray_menu(m: u16) -> bool {
    m == WM_RBUTTON_UP as u16 || m == WM_CONTEXT_MENU as u16
}

/// `wndProc` (`go\tray.go:164-184`).
///
/// # Safety
///
/// Called by `user32` as a window procedure: `hwnd` must be the tray window this
/// module created.
unsafe extern "system" fn wnd_proc(
    hwnd: HWND,
    msg: u32,
    wparam: WPARAM,
    lparam: LPARAM,
) -> LRESULT {
    match msg {
        WM_APP_TRAY => {
            if opens_tray_menu(tray_mouse_message(wparam, lparam)) {
                unsafe { show_tray_menu(hwnd) };
            }
            0
        }
        WM_APP_QUIT => {
            unsafe { PostQuitMessage(0) };
            0
        }
        WM_CLOSE => {
            unsafe { DestroyWindow(hwnd) };
            0
        }
        WM_DESTROY => {
            unsafe { PostQuitMessage(0) };
            0
        }
        _ => unsafe { DefWindowProcW(hwnd, msg, wparam, lparam) },
    }
}

/// `showTrayMenu` (`go\tray.go:186-214`): one popup, `Open Potflix` = 2,
/// `Exit Potflix` = 1, and the selection dispatch.
///
/// # Safety
///
/// `hwnd` must be a window handle created by [`create_tray_window`] on the
/// calling thread: the popup is owned by that window and the final
/// `PostMessageW` targets it.
pub unsafe fn show_tray_menu(hwnd: HWND) {
    let menu = unsafe { CreatePopupMenu() };
    if menu.is_null() {
        return;
    }

    let open_label = utf16_z("Open Potflix");
    let exit_label = utf16_z("Exit Potflix");
    unsafe {
        AppendMenuW(menu, MF_STRING, MENU_ITEM_OPEN, open_label.as_ptr());
        AppendMenuW(menu, MF_STRING, MENU_ITEM_EXIT, exit_label.as_ptr());

        let mut cursor = Point::default();
        GetCursorPos(&mut cursor as *mut Point as *mut POINT);
        SetForegroundWindow(hwnd);
        let selection = TrackPopupMenu(
            menu,
            TPM_RIGHT_BUTTON | TPM_BOTTOM_ALIGN | TPM_RETURN_CMD,
            cursor.x,
            cursor.y,
            0,
            hwnd,
            null(),
        );
        // Required Win32 forensics (`go\tray.go:207`): without this post the
        // menu can stay up until the next click anywhere on the desktop.
        PostMessageW(hwnd, WM_NULL, 0, 0);

        // Go's `defer procDestroyMenu.Call(menu)` runs after the dispatch, and
        // `on_exit` never returns (`os.Exit`), so the exit path leaves the menu
        // to process teardown -- reproduced by keeping the destroy last.
        match selection as usize {
            MENU_ITEM_OPEN => show_main_window(),
            MENU_ITEM_EXIT => on_exit(),
            _ => {}
        }
        DestroyMenu(menu);
    }
}

/// Go `clearActivity` (`go\tray.go:27-29`): the M1 stub for the clear-on-exit
/// deviation (fleet decision 6). M3 replaces this body with a real IPC clear.
pub fn clear_activity() {
    log_line("cleared (exit)");
}

/// `onExit` (`go\tray.go:216-220`): log, clear, and leave -- no `NIM_DELETE`,
/// no `DestroyWindow`.
pub fn on_exit() -> ! {
    log_line("exit requested (tray menu)");
    clear_activity();
    std::process::exit(0);
}

#[cfg(test)]
mod tests {
    use super::*;
    use core::mem::size_of;

    /// `go\tray_test.go:11-17` -- exactly what a real right-click delivers
    /// without `NIM_SETVERSION`: wParam = trayUID (1), lParam = WM_RBUTTONUP.
    #[test]
    fn tray_mouse_message_legacy_shape() {
        let got = tray_mouse_message(TRAY_UID as usize, WM_RBUTTON_UP as isize);
        assert_eq!(got, WM_RBUTTON_UP as u16);
    }

    /// `go\tray_test.go:19-27` -- the defensive shape: the message in
    /// LOWORD(wParam) with lParam holding coordinates (LOWORD 0x0064 is not a
    /// menu message, so only the wParam fallback can match).
    #[test]
    fn tray_mouse_message_v4_shape() {
        let coords: LPARAM = 0x00C80064;
        let got = tray_mouse_message(WM_CONTEXT_MENU as usize, coords);
        assert_eq!(got, WM_CONTEXT_MENU as u16);
    }

    /// `go\tray_test.go:29-41` -- a callback carrying no menu message opens no
    /// menu: hover, WM_NULL, and a coordinates-only payload.
    #[test]
    fn tray_mouse_message_non_mouse_opens_no_menu() {
        let cases: [(&str, WPARAM, LPARAM); 3] = [
            ("hover", 1, 0x0200),           // WM_MOUSEMOVE
            ("zero", 0, 0),                 // WM_NULL
            ("coords-only", 0, 0x00C80064), // coordinate payload, no message
        ];
        for (name, wparam, lparam) in cases {
            let got = tray_mouse_message(wparam, lparam);
            assert!(
                !opens_tray_menu(got),
                "{name}: tray_mouse_message({wparam:#x}, {lparam:#x}) = {got:#06x} opens a menu, want none"
            );
        }
    }

    /// The `cbSize`-from-`Sizeof` rule (`go\tray.go:103`): the value the struct
    /// carries must be the compiled size, or `NIM_ADD` fails.
    #[test]
    fn notify_icon_data_cb_size_is_the_compiled_size() {
        let nid = make_notify_icon_data(null_mut(), null_mut());
        assert_eq!(nid.cb_size, 976);
        assert_eq!(nid.cb_size as usize, size_of::<NotifyIconData>());
        assert_eq!(size_of::<NotifyIconData>(), size_of::<NOTIFYICONDATAW>());
    }

    /// The rest of the add payload (`go\tray.go:101-111`).
    #[test]
    fn notify_icon_data_carries_the_go_add_payload() {
        let nid = make_notify_icon_data(1usize as HWND, 2usize as HICON);
        assert_eq!(nid.u_id, TRAY_UID);
        assert_eq!(nid.u_flags, NIF_MESSAGE | NIF_ICON | NIF_TIP);
        assert_eq!(nid.u_callback_message, 0x8001);
        assert_eq!(nid.h_wnd, 1);
        assert_eq!(nid.h_icon, 2);
        assert_eq!(
            String::from_utf16(&nid.sz_tip[..TRAY_TIP.len()]).unwrap(),
            TRAY_TIP
        );
        assert_eq!(nid.sz_tip[TRAY_TIP.len()], 0);
    }

    /// The embedded blob is the repo's icon and stays a SINGLE DIB frame.
    #[test]
    fn embedded_icon_is_the_repo_icon() {
        assert_eq!(ICON_ICO.len(), 262_206);
        assert_eq!(u16::from_le_bytes([ICON_ICO[4], ICON_ICO[5]]), 1);
        let (off, size) = parse_icon_entry(ICON_ICO).expect("the embedded icon parses");
        assert_eq!((off, size), (22, 262_184));
        assert_eq!(off + size, ICON_ICO.len());
    }

    /// Every bounds check `go\tray.go:75-86` makes, on synthetic headers.
    #[test]
    fn icon_entry_traps_are_checked() {
        assert_eq!(parse_icon_entry(&[]), Err(IconError::TooShort(0)));
        assert_eq!(parse_icon_entry(&[0u8; 21]), Err(IconError::TooShort(21)));

        let no_frames = [0u8; 22]; // frame count 0
        assert_eq!(parse_icon_entry(&no_frames), Err(IconError::FrameCount(0)));

        let mut before_directory = [0u8; 22];
        before_directory[4] = 1; // count 1
        before_directory[14] = 4; // dwBytesInRes 4
        before_directory[18] = 6; // dwImageOffset 6 < 6+16
        assert_eq!(
            parse_icon_entry(&before_directory),
            Err(IconError::BadEntry {
                off: 6,
                size: 4,
                total: 22
            })
        );

        let mut zero_size = [0u8; 22];
        zero_size[4] = 1; // count 1
        zero_size[18] = 22; // a valid offset, but dwBytesInRes 0
        assert_eq!(
            parse_icon_entry(&zero_size),
            Err(IconError::BadEntry {
                off: 22,
                size: 0,
                total: 22
            })
        );

        let mut past_end = [0u8; 22];
        past_end[4] = 1; // count 1
        past_end[14] = 8; // dwBytesInRes 8
        past_end[18] = 20; // 20 + 8 > 22
        assert_eq!(
            parse_icon_entry(&past_end),
            Err(IconError::BadEntry {
                off: 20,
                size: 8,
                total: 22
            })
        );
    }

    /// The menu ids and flags the popup is built from (`go\tray.go:193-206`).
    #[test]
    fn menu_shape_matches_go() {
        assert_eq!(TRAY_CLASS_NAME, "PotflixTrayWnd");
        assert_eq!(MENU_ITEM_OPEN, 2);
        assert_eq!(MENU_ITEM_EXIT, 1);
        assert_eq!(MF_STRING, 0);
        assert_eq!(TPM_RIGHT_BUTTON | TPM_BOTTOM_ALIGN | TPM_RETURN_CMD, 0x0122);
        assert_eq!(WM_APP_TRAY, 0x8001);
        assert_eq!(WM_APP_QUIT, 0x8002);
        assert_eq!(ICON_RESOURCE_VERSION, 0x0003_0000);
        assert_eq!(NIM_ADD, 0);
        assert_eq!(NIM_DELETE, 2);
    }
}
