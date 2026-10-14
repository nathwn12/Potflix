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
    utf16_z, CreateIconFromResourceEx, CreatePopupMenu, CreateWindowExW, DefWindowProcW,
    DestroyMenu, DestroyWindow, DispatchMessageW, GetCursorPos, GetMessageW, GetModuleHandleW,
    MsgT, NotifyIconData, Point, PostMessageW, PostQuitMessage, RegisterClassExW,
    SetForegroundWindow, Shell_NotifyIconW, TrackPopupMenu, TranslateMessage, WndClassEx, WndProc,
    DRAWITEMSTRUCT, ICON_RESOURCE_VERSION, MEASUREITEMSTRUCT, MENUINFO, MENUITEMINFOW,
    MENU_INFO_BACKGROUND, MFS_ENABLED, MFT_OWNERDRAW, MIIM_DATA, MIIM_FTYPE, MIIM_ID, MIIM_STATE,
    MIIM_STRING, NIF_ICON, NIF_MESSAGE, NIF_TIP, NIM_ADD, NIM_DELETE, ODS_SELECTED, ODT_MENU,
    TPM_BOTTOM_ALIGN, TPM_RETURN_CMD, TPM_RIGHT_BUTTON, WM_APP_QUIT, WM_APP_TRAY, WM_CLOSE,
    WM_CONTEXT_MENU, WM_DESTROY, WM_DRAWITEM, WM_MEASUREITEM, WM_NULL, WM_RBUTTON_UP,
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

/// Go `clearActivity` (`go\tray.go:24-29`): a package variable the app overrides
/// at `go\main.go:197-200` with the dc-bound clear. Installed once by the app
/// seam; until then [`clear_activity`] runs the M1 stub.
static CLEAR_ACTIVITY: OnceLock<Box<dyn Fn() + Send + Sync>> = OnceLock::new();

/// Installs the dc-bound clear (Go's `clearActivity = func() {...}` at
/// `go\main.go:197`). Returns the rejected closure if one is already installed.
pub fn set_clear_activity(
    clear: Box<dyn Fn() + Send + Sync>,
) -> Result<(), Box<dyn Fn() + Send + Sync>> {
    CLEAR_ACTIVITY.set(clear)
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
        WM_MEASUREITEM => {
            // Owner-drawn tray items only: `wParam` is 0 for menus and
            // `lParam` is the `MEASUREITEMSTRUCT`. Anything else belongs to a
            // control this window does not own.
            if wparam == 0 && lparam != 0 {
                let mis = unsafe { &mut *(lparam as *mut MEASUREITEMSTRUCT) };
                if mis.CtlType == ODT_MENU {
                    measure_menu_item(mis, mis.itemData, crate::window::system_dpi());
                    return 1;
                }
            }
            unsafe { DefWindowProcW(hwnd, msg, wparam, lparam) }
        }
        WM_DRAWITEM => {
            // Owner-drawn tray items only; see `draw_menu_item`.
            if lparam != 0 {
                let dis = lparam as *const DRAWITEMSTRUCT;
                if unsafe { (*dis).CtlType } == ODT_MENU {
                    unsafe { draw_menu_item(dis) };
                    return 1;
                }
            }
            unsafe { DefWindowProcW(hwnd, msg, wparam, lparam) }
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

/// The tray popup's items in display order: the label and the stable dispatch
/// id. Ids, labels, order and dispatch are the Go contract (`go\tray.go`):
/// only the PAINT is new (owner-drawn dark instead of the stock white popup).
/// `dwItemData` carries the index into this table.
pub fn tray_menu_items() -> [(&'static str, usize); 2] {
    [
        ("Open Potflix", MENU_ITEM_OPEN),
        ("Exit Potflix", MENU_ITEM_EXIT),
    ]
}

/// Menu-item height: the body type size plus vertical padding, at `dpi`.
/// Width is measured from the label; see [`measure_menu_item`].
pub fn menu_item_height(dpi: i32) -> i32 {
    crate::window::scale_px(dpi, crate::window::FS_BODY + 2 * 6)
}

/// Measure one owner-drawn item's text width at `dpi` with a screen DC, like
/// [`crate::window::measure_scan_status_width`]: `None` when no DC/font is
/// available (the caller then falls back to a fixed width).
#[must_use]
pub fn menu_text_width(dpi: i32, label: &str) -> Option<i32> {
    use windows_sys::Win32::Foundation::SIZE;
    use windows_sys::Win32::Graphics::Gdi::{
        DeleteObject, GetDC, GetTextExtentPoint32W, ReleaseDC, SelectObject,
    };
    if label.is_empty() {
        return Some(0);
    }
    // SAFETY: a screen DC + a created font, both released below.
    unsafe {
        let hdc = GetDC(null_mut());
        if hdc.is_null() {
            return None;
        }
        let font = crate::window::create_ui_font(
            -crate::window::scale_px(dpi, crate::window::FS_BODY),
            crate::win32::FW_NORMAL,
        );
        if font.is_null() {
            ReleaseDC(null_mut(), hdc);
            return None;
        }
        let old = SelectObject(hdc, font);
        let u: Vec<u16> = label.encode_utf16().collect();
        let mut sz = SIZE { cx: 0, cy: 0 };
        let r = GetTextExtentPoint32W(hdc, u.as_ptr(), u.len() as i32, &mut sz);
        SelectObject(hdc, old);
        DeleteObject(font);
        ReleaseDC(null_mut(), hdc);
        if r == 0 {
            None
        } else {
            Some(sz.cx)
        }
    }
}

/// Fill a `MEASUREITEMSTRUCT` for item-data `index`: label width plus the
/// check-margin and horizontal padding, at `dpi`. Pure math around
/// [`menu_text_width`]; falls back to a fixed width when unmeasurable.
pub fn measure_menu_item(mis: &mut MEASUREITEMSTRUCT, index: usize, dpi: i32) {
    let items = tray_menu_items();
    let label = items.get(index).map_or("", |(s, _)| *s);
    let pad = crate::window::scale_px(dpi, 28);
    let w = menu_text_width(dpi, label).unwrap_or(crate::window::scale_px(dpi, 140));
    mis.itemWidth = (w + pad) as u32;
    mis.itemHeight = menu_item_height(dpi) as u32;
}

/// Paint one owner-drawn item from its `DRAWITEMSTRUCT`: palette fill (hover =
/// raised + accent bar), palette text in the ClearType body font. Restores
/// every selected GDI object; draws only inside `rcItem`.
///
/// # Safety
///
/// `dis` must point to a live `DRAWITEMSTRUCT` from `WM_DRAWITEM` (its `hDC`
/// and `rcItem` are read; nothing is retained).
pub unsafe fn draw_menu_item(dis: *const DRAWITEMSTRUCT) {
    use windows_sys::Win32::Graphics::Gdi::{
        CreateSolidBrush, DeleteObject, DrawTextW, FillRect, SelectObject, SetBkMode, SetTextColor,
    };
    // SAFETY: caller guarantees a live struct; all GDI objects are released.
    unsafe {
        let dis = &*dis;
        if dis.CtlType != ODT_MENU {
            return;
        }
        let items = tray_menu_items();
        let label = items.get(dis.itemData).map_or("", |(s, _)| *s);
        let dpi = crate::window::system_dpi();
        let selected = dis.itemState & ODS_SELECTED != 0;
        let bg = CreateSolidBrush(if selected {
            crate::window::COL_ROW_HOVER
        } else {
            crate::window::COL_CARD
        });
        FillRect(dis.hDC, &dis.rcItem, bg);
        DeleteObject(bg);
        if selected {
            // Selection bar down the item's left edge, like the folder list.
            let bar = windows_sys::Win32::Foundation::RECT {
                left: dis.rcItem.left,
                top: dis.rcItem.top,
                right: dis.rcItem.left + crate::window::scale_px(dpi, crate::window::SELECT_BAR_W),
                bottom: dis.rcItem.bottom,
            };
            let accent = CreateSolidBrush(crate::window::COL_ACCENT);
            FillRect(dis.hDC, &bar, accent);
            DeleteObject(accent);
        }
        let font = crate::window::create_ui_font(
            -crate::window::scale_px(dpi, crate::window::FS_BODY),
            crate::win32::FW_NORMAL,
        );
        if !font.is_null() {
            SetTextColor(dis.hDC, crate::window::COL_TEXT);
            SetBkMode(dis.hDC, crate::win32::TRANSPARENT);
            let old = SelectObject(dis.hDC, font);
            let u = utf16_z(label);
            let gutter = crate::window::scale_px(dpi, 14);
            let mut bx = windows_sys::Win32::Foundation::RECT {
                left: dis.rcItem.left + gutter,
                top: dis.rcItem.top,
                right: dis.rcItem.right - gutter,
                bottom: dis.rcItem.bottom,
            };
            DrawTextW(
                dis.hDC,
                u.as_ptr(),
                -1,
                &mut bx,
                crate::win32::DT_LEFT
                    | crate::win32::DT_SINGLE_LINE
                    | crate::win32::DT_NO_PREFIX
                    | crate::win32::DT_V_CENTER,
            );
            SelectObject(dis.hDC, old);
            DeleteObject(font);
        }
    }
}

/// `showTrayMenu` (`go\tray.go:186-214`): one popup, `Open Potflix` = 2,
/// `Exit Potflix` = 1, and the selection dispatch.
///
/// Rust-only paint: the popup interior is pinned dark (`SetMenuInfo` /
/// `MIM_BACKGROUND`) and both items are `MFT_OWNERDRAW`, measured in
/// `WM_MEASUREITEM` and painted in `WM_DRAWITEM` by this tray window (its own
/// wndproc, which holds no lock -- `TrackPopupMenu`'s modal loop re-enters it,
/// so there is no `UI`-guard discipline to keep and no re-entrant site to
/// register). Ids, labels, order, flags and dispatch match Go exactly.
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

    // Dark popup interior for the menu's own lifetime (deleted below).
    let bg = unsafe { crate::win32::CreateSolidBrush(crate::window::COL_CARD) };
    unsafe {
        let mut info: MENUINFO = core::mem::zeroed();
        info.cbSize = core::mem::size_of::<MENUINFO>() as u32;
        info.fMask = MENU_INFO_BACKGROUND;
        info.hbrBack = bg;
        crate::win32::SetMenuInfo(menu, &info);

        for (i, (label, id)) in tray_menu_items().iter().enumerate() {
            let text = utf16_z(label);
            let mut item: MENUITEMINFOW = core::mem::zeroed();
            item.cbSize = core::mem::size_of::<MENUITEMINFOW>() as u32;
            item.fMask = MIIM_STRING | MIIM_ID | MIIM_FTYPE | MIIM_STATE | MIIM_DATA;
            item.fType = MFT_OWNERDRAW;
            item.fState = MFS_ENABLED;
            item.wID = *id as u32;
            // `dwTypeData` is unread for `MFT_OWNERDRAW` (the label travels in
            // `dwItemData`); it is passed anyway for debuggers and tests.
            item.dwTypeData = text.as_ptr() as *mut u16;
            item.cch = label.len() as u32;
            item.dwItemData = i;
            // `text` outlives this call; the system copies nothing for
            // owner-draw items and never reads it back.
            crate::win32::InsertMenuItemW(menu, i as u32, 1, &item);
        }

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
        crate::win32::DeleteObject(bg);
    }
}

/// Calls the installed clear seam, or the M1 stub when none is installed
/// (Go `clearActivity`, `go\tray.go:24-29`; the app installs the dc-bound
/// override at `go\main.go:197-200`).
pub fn clear_activity() {
    match CLEAR_ACTIVITY.get() {
        Some(clear) => clear(),
        None => log_line("cleared (exit)"),
    }
}

/// `onExit` body before the terminal exit (`go\tray.go:216-219`): log the
/// request, then fire `clearActivity`. Split from [`on_exit`] so the exit path's
/// clear runs in a test without ending the test process.
pub fn on_exit_steps() {
    log_line("exit requested (tray menu)");
    clear_activity();
}

/// `onExit` (`go\tray.go:216-220`): log, clear, and leave -- no `NIM_DELETE`,
/// no `DestroyWindow`.
pub fn on_exit() -> ! {
    on_exit_steps();
    std::process::exit(0);
}

#[cfg(test)]
mod tests {
    use super::*;
    use core::mem::size_of;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::sync::Arc;

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
        assert_eq!(crate::win32::MF_STRING, 0);
        assert_eq!(TPM_RIGHT_BUTTON | TPM_BOTTOM_ALIGN | TPM_RETURN_CMD, 0x0122);
        assert_eq!(WM_APP_TRAY, 0x8001);
        assert_eq!(WM_APP_QUIT, 0x8002);
        assert_eq!(ICON_RESOURCE_VERSION, 0x0003_0000);
        assert_eq!(NIM_ADD, 0);
        assert_eq!(NIM_DELETE, 2);
    }

    /// The owner-drawn popup keeps the Go contract: same order, same labels,
    /// same dispatch ids; `dwItemData` is the index into this table.
    #[test]
    fn menu_model_matches_go() {
        let items = tray_menu_items();
        assert_eq!(items.len(), 2);
        assert_eq!(items[0], ("Open Potflix", MENU_ITEM_OPEN));
        assert_eq!(items[1], ("Exit Potflix", MENU_ITEM_EXIT));
        assert_eq!(MENU_ITEM_OPEN, 2);
        assert_eq!(MENU_ITEM_EXIT, 1);
    }

    /// The owner-draw measure path sizes every item positively at 96 and 144
    /// DPI and grows with the label: a zero-size item would be untouchable.
    #[test]
    fn menu_measure_sizes_items() {
        for dpi in [96, 144] {
            for (i, (label, _)) in tray_menu_items().iter().enumerate() {
                let mut mis: MEASUREITEMSTRUCT = unsafe { core::mem::zeroed() };
                mis.CtlType = ODT_MENU;
                measure_menu_item(&mut mis, i, dpi);
                println!(
                    "menu item {i} ({label}) at {dpi}dpi: {}x{}",
                    mis.itemWidth, mis.itemHeight
                );
                assert!(mis.itemWidth > 0, "item {i} width at {dpi}dpi");
                assert!(mis.itemHeight > 0, "item {i} height at {dpi}dpi");
            }
            // Unknown index still sizes (fallback width): the menu never
            // collapses on a bad `dwItemData`.
            let mut mis: MEASUREITEMSTRUCT = unsafe { core::mem::zeroed() };
            measure_menu_item(&mut mis, 99, dpi);
            assert!(mis.itemWidth > 0 && mis.itemHeight > 0);
        }
    }

    /// The tray Exit path (`go\tray.go:216-219`, installed by the app at
    /// `go\main.go:197-200`) FIRES an installed clear seam: the exit handler body
    /// calls `clearActivity`, so the dc-bound closure runs. This proves the call
    /// reaches the seam, not merely that a setter stored a value.
    #[test]
    fn installed_clear_fires_from_the_exit_path() {
        let hits = Arc::new(AtomicUsize::new(0));
        let h = Arc::clone(&hits);
        assert!(
            set_clear_activity(Box::new(move || {
                h.fetch_add(1, Ordering::SeqCst);
            }))
            .is_ok(),
            "the clear seam installs once"
        );
        on_exit_steps();
        assert_eq!(
            hits.load(Ordering::SeqCst),
            1,
            "the exit path did not fire the installed clear"
        );
    }
}
