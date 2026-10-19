//! `uicapture` -- hidden-desktop proof harness for the Potflix UI redesign.
//!
//! Capture-only: drives the REAL `potflix-win` window (same `create_main_window`
//! / `show_main_window` / `main_wnd_proc` / paint path the tray app uses) on a
//! private desktop and writes PNGs. Never touches the owner's visible desktop:
//! no `SendInput`, no input injection, no `SetForegroundWindow` off the hidden
//! desktop -- only the message path (`PostMessageW`/`SendMessageW`) to windows
//! this harness created, exactly like the proven `desktop-headless.ps1` runner.
//! The full tray binary cannot run headless (its notify-icon gate returns
//! before the window exists when no shell hosts the tray), so this harness
//! bypasses the tray gate and nothing else: same window, same proc, same
//! paint, same config read.
//!
//! Usage (run with the wanted `LOCALAPPDATA`: owner state = populated list,
//! scratch dir = empty state):
//! ```text
//! uicapture window <out.png> <main|settings|card> [focus <id>] [tabs <n>] [wheel <notches>]
//! uicapture menu <out.png>
//! uicapture offscreen <out.png> <dpi> <main|settings|card>
//! uicapture fixmatch <out_unmatched.png> <out_matched.png>
//! uicapture trace
//! ```
//! Every capture prints `MEASURE min/max/mean uniform=<bool>` (a blank bitmap
//! fails the proof), the DPI, and the DPI-awareness context.

use std::ffi::c_void;

use potflix_win::tray;
use potflix_win::win32;
use potflix_win::window;
use windows_sys::Win32::Foundation::{HWND, RECT};
use windows_sys::Win32::Graphics::Gdi::{DeleteDC, DeleteObject, SelectObject};
use windows_sys::Win32::Storage::Xps::PrintWindow;
use windows_sys::Win32::System::StationsAndDesktops::{
    CloseDesktop, CreateDesktopW, GetThreadDesktop, SetThreadDesktop,
};
use windows_sys::Win32::System::Threading::GetCurrentThreadId;
use windows_sys::Win32::UI::WindowsAndMessaging::{
    DispatchMessageW, FindWindowW, PeekMessageW, TranslateMessage, MSG, PM_REMOVE,
};

fn utf16(s: &str) -> Vec<u16> {
    s.encode_utf16().chain(std::iter::once(0)).collect()
}

/// Pump this thread's queue for `ms` milliseconds.
fn pump(ms: u64) {
    let end = std::time::Instant::now() + std::time::Duration::from_millis(ms);
    while std::time::Instant::now() < end {
        unsafe {
            let mut msg: MSG = std::mem::zeroed();
            while PeekMessageW(&mut msg, std::ptr::null_mut(), 0, 0, PM_REMOVE) != 0 {
                TranslateMessage(&msg);
                DispatchMessageW(&msg);
            }
        }
        std::thread::sleep(std::time::Duration::from_millis(10));
    }
}

/// Create a private desktop and move this thread onto it. Returns
/// `(new, previous)` as address-sized values (raw handles are not `Send`);
/// the caller restores `previous` and closes `new`.
fn enter_hidden_desktop(tag: &str) -> Result<(usize, usize), String> {
    let name = utf16(&format!("potflix-cap-{tag}"));
    let prev = unsafe { GetThreadDesktop(GetCurrentThreadId()) } as isize;
    // SAFETY: name is a live null-terminated desktop name; no security attrs.
    // `GENERIC_ALL` (0x10000000) like the proven runner script.
    let desk = unsafe {
        CreateDesktopW(
            name.as_ptr(),
            std::ptr::null(),
            std::ptr::null(),
            0,
            0x1000_0000,
            std::ptr::null(),
        )
    };
    if desk.is_null() {
        return Err("CreateDesktopW failed".into());
    }
    // SAFETY: this thread owns no windows yet, so the switch is legal.
    if unsafe { SetThreadDesktop(desk) } == 0 {
        unsafe { CloseDesktop(desk) };
        return Err("SetThreadDesktop failed (thread owns a window?)".into());
    }
    Ok((desk as usize, prev as usize))
}

/// Leave the private desktop and destroy it.
fn leave_hidden_desktop(desk: usize, prev: usize) {
    unsafe {
        SetThreadDesktop(prev as *mut c_void);
        CloseDesktop(desk as *mut c_void);
    }
}

fn dirs(n: usize) -> Vec<String> {
    (0..n)
        .map(|i| format!("{}:\\Media", char::from(b'A' + (i % 26) as u8)))
        .collect()
}

fn view_of(s: &str) -> Result<window::View, String> {
    match s {
        "main" => Ok(window::View::Main),
        "settings" => Ok(window::View::Settings),
        "card" => Ok(window::View::Card),
        _ => Err(format!("unknown view {s:?}; want main|settings|card")),
    }
}

/// Mean/min/max/uniform of a BGRA buffer; `uniform == true` fails the proof.
fn measure(px: &[u8]) -> (u8, u8, f64, bool) {
    let mut min = 255u8;
    let mut max = 0u8;
    let mut sum = 0u64;
    let mut first = None;
    let mut uniform = true;
    for c in px {
        min = min.min(*c);
        max = max.max(*c);
        sum += *c as u64;
        match first {
            None => first = Some(*c),
            Some(f) => {
                if *c != f {
                    uniform = false;
                }
            }
        }
    }
    let mean = sum as f64 / px.len().max(1) as f64;
    (min, max, mean, uniform)
}

/// Create a 32bpp top-down DIB section; returns `(dc, bitmap, bits_ptr)`.
/// The caller selects, paints, then deletes. Unlike `CreateCompatibleBitmap`
/// (a DDB with no CPU-visible bits), a DIB section's pixels are readable.
fn create_dib(w: i32, h: i32) -> Result<(isize, isize, *const u8), String> {
    use windows_sys::Win32::Graphics::Gdi::{
        CreateCompatibleDC, CreateDIBSection, BITMAPINFO, BITMAPINFOHEADER, BI_RGB, DIB_RGB_COLORS,
    };
    // SAFETY: DC/bitmap/DIB created here, released by the caller.
    unsafe {
        let screen = windows_sys::Win32::Graphics::Gdi::GetDC(std::ptr::null_mut());
        let mem = CreateCompatibleDC(screen);
        windows_sys::Win32::Graphics::Gdi::ReleaseDC(std::ptr::null_mut(), screen);
        if mem.is_null() {
            return Err("capture DC failed".into());
        }
        let mut info: BITMAPINFO = std::mem::zeroed();
        info.bmiHeader.biSize = core::mem::size_of::<BITMAPINFOHEADER>() as u32;
        info.bmiHeader.biWidth = w;
        info.bmiHeader.biHeight = -h; // top-down: row 0 is the top.
        info.bmiHeader.biPlanes = 1;
        info.bmiHeader.biBitCount = 32;
        info.bmiHeader.biCompression = BI_RGB;
        let mut bits: *mut c_void = std::ptr::null_mut();
        let bmp = CreateDIBSection(
            mem,
            &info,
            DIB_RGB_COLORS,
            &mut bits,
            std::ptr::null_mut(),
            0,
        );
        if bmp.is_null() || bits.is_null() {
            DeleteDC(mem);
            return Err("CreateDIBSection failed".into());
        }
        Ok((mem as isize, bmp as isize, bits as *const u8))
    }
}

/// Save an `HBITMAP` as PNG through the GDI+ image encoder (no new crate).
fn save_png(hbm: isize, path: &str) -> Result<(), String> {
    use windows_sys::Win32::Graphics::GdiPlus::{
        GdipCreateBitmapFromHBITMAP, GdipDisposeImage, GdipSaveImageToFile, GdiplusShutdown,
        GdiplusStartup, GdiplusStartupInput, GpBitmap, GpImage,
    };
    // PNG encoder CLSID {557CF406-1A04-11D3-9A73-0000F81EF32E}.
    let clsid = windows_sys::core::GUID {
        data1: 0x557C_F406,
        data2: 0x1A04,
        data3: 0x11D3,
        data4: [0x9A, 0x73, 0x00, 0x00, 0xF8, 0x1E, 0xF3, 0x2E],
    };
    // SAFETY: flat GDI+ calls with live handles; everything released below.
    unsafe {
        let mut token: usize = 0;
        let input = GdiplusStartupInput {
            GdiplusVersion: 1,
            ..Default::default()
        };
        if GdiplusStartup(&mut token, &input, std::ptr::null_mut()) != 0 {
            return Err("GdiplusStartup failed".into());
        }
        let mut img: *mut GpBitmap = std::ptr::null_mut();
        let st = GdipCreateBitmapFromHBITMAP(hbm as *mut c_void, std::ptr::null_mut(), &mut img);
        if st != 0 || img.is_null() {
            GdiplusShutdown(token);
            return Err(format!("GdipCreateBitmapFromHBITMAP status={st}"));
        }
        let wpath = utf16(path);
        let st = GdipSaveImageToFile(
            img as *mut GpImage,
            wpath.as_ptr(),
            &clsid,
            std::ptr::null(),
        );
        GdipDisposeImage(img as *mut GpImage);
        GdiplusShutdown(token);
        if st != 0 {
            return Err(format!("GdipSaveImageToFile status={st}"));
        }
    }
    Ok(())
}

/// Capture `hwnd`'s client area with `PrintWindow` (flag 2, then flag 0) into a
/// fresh bitmap; returns `(pixels, w, h, hbitmap)` for the first non-blank
/// capture, else an error.
fn print_window(hwnd: HWND, w: i32, h: i32) -> Result<(Vec<u8>, isize), String> {
    // SAFETY: DCs/bitmaps created and selected within this call.
    unsafe {
        let (mem, bmp, bits) = create_dib(w, h)?;
        let mem = mem as *mut c_void;
        let old = SelectObject(mem, bmp as *mut c_void);
        for flag in [win32::PW_RENDERFULLCONTENT, 0] {
            PrintWindow(hwnd, mem, flag);
            windows_sys::Win32::Graphics::Gdi::GdiFlush();
            let px = std::slice::from_raw_parts(bits, (w as usize) * (h as usize) * 4).to_vec();
            let (_, _, _, uniform) = measure(&px);
            if !uniform {
                SelectObject(mem, old);
                DeleteDC(mem);
                return Ok((px, bmp));
            }
        }
        SelectObject(mem, old);
        DeleteObject(bmp as *mut c_void);
        DeleteDC(mem);
        Err("PrintWindow returned a blank bitmap on flags 2 and 0".into())
    }
}

fn report_and_save(
    tag: &str,
    px: &[u8],
    hbm: isize,
    path: &str,
    extra: &str,
) -> Result<(), String> {
    let (min, max, mean, uniform) = measure(px);
    println!("MEASURE {tag}: min={min} max={max} mean={mean:.1} uniform={uniform} {extra}");
    if uniform {
        return Err(format!("{tag}: blank capture"));
    }
    save_png(hbm, path)?;
    println!("SAVED {path}");
    Ok(())
}

fn client_size(hwnd: HWND) -> (i32, i32) {
    let mut rc = RECT {
        left: 0,
        top: 0,
        right: 0,
        bottom: 0,
    };
    // SAFETY: `rc` is a live RECT.
    unsafe { windows_sys::Win32::UI::WindowsAndMessaging::GetClientRect(hwnd, &mut rc) };
    (rc.right - rc.left, rc.bottom - rc.top)
}

fn mode_window(args: &[String]) -> Result<(), String> {
    let out = args.get(2).cloned().ok_or("window <out.png> ...")?;
    let view = view_of(&args.get(3).cloned().unwrap_or("main".into()))?;
    let (desk, prev) = enter_hidden_desktop("wnd")?;
    let r = (|| {
        window::create_main_window().map_err(|e| format!("create_main_window: {e}"))?;
        {
            let mut st = window::ui();
            st.cur_view = view;
            st.focus_id = 0;
            st.refresh_dir_list();
        }
        window::show_main_window();
        pump(1200);
        // Optional interactions, all message-path to our own window.
        let mut i = 4;
        while i < args.len() {
            match args[i].as_str() {
                "focus" => {
                    let id: i32 = args
                        .get(i + 1)
                        .ok_or("focus <id>")?
                        .parse()
                        .map_err(|_| "bad id")?;
                    window::ui().focus_id = id;
                    window::ui().invalidate();
                    pump(300);
                    i += 2;
                }
                "tabs" => {
                    let n: i32 = args
                        .get(i + 1)
                        .ok_or("tabs <n>")?
                        .parse()
                        .map_err(|_| "bad n")?;
                    let hwnd = window::ui().main_hwnd as HWND;
                    for _ in 0..n {
                        unsafe {
                            win32::SendMessageW(
                                hwnd,
                                window::WM_KEYDOWN,
                                window::VK_TAB as usize,
                                0,
                            )
                        };
                        pump(150);
                    }
                    i += 2;
                }
                "wheel" => {
                    let n: i32 = args
                        .get(i + 1)
                        .ok_or("wheel <n>")?
                        .parse()
                        .map_err(|_| "bad n")?;
                    let hwnd = window::ui().main_hwnd as HWND;
                    unsafe {
                        win32::SendMessageW(
                            hwnd,
                            win32::WM_MOUSEWHEEL,
                            ((n * win32::WHEEL_DELTA) as u16 as usize) << 16,
                            0,
                        )
                    };
                    pump(300);
                    i += 2;
                }
                other => return Err(format!("unknown window arg {other:?}")),
            }
        }
        let hwnd = window::ui().main_hwnd as HWND;
        if hwnd.is_null() {
            return Err("no main window".into());
        }
        unsafe { win32::UpdateWindow(hwnd) };
        pump(400);
        let (w, h) = client_size(hwnd);
        let dpi = window::window_dpi(hwnd);
        let aware = win32::dpi_awareness();
        let (px, hbm) = print_window(hwnd, w, h)?;
        let extra = format!("dpi={dpi} awareness={aware} client={w}x{h}");
        report_and_save("window", &px, hbm, &out, &extra)?;
        // SAFETY: bitmap/DC released after the save.
        unsafe {
            DeleteObject(hbm as *mut c_void);
        }
        Ok(())
    })();
    leave_hidden_desktop(desk, prev);
    r
}

fn mode_menu(args: &[String]) -> Result<(), String> {
    let out = args.get(2).cloned().ok_or("menu <out.png>")?;
    let (desk, prev) = enter_hidden_desktop("menu")?;
    let r = (|| {
        let tw = tray::create_tray_window().map_err(|e| format!("tray window: {e}"))?;
        let owner = tw.hwnd as usize;
        // Watcher thread: waits for the `#32768` popup on THIS hidden desktop,
        // captures it, then ends menu tracking. Message-path only.
        let out2 = out.clone();
        let watcher = std::thread::spawn(move || -> Result<(), String> {
            // SAFETY: this thread owns no windows, so the switch is legal.
            if unsafe { SetThreadDesktop(desk as *mut c_void) } == 0 {
                return Err("watcher SetThreadDesktop failed".into());
            }
            let cls = utf16("#32768");
            for _ in 0..100 {
                // SAFETY: class string is live.
                let menu_hwnd = unsafe { FindWindowW(cls.as_ptr(), std::ptr::null()) };
                if !menu_hwnd.is_null() {
                    std::thread::sleep(std::time::Duration::from_millis(400));
                    let (w, h) = client_size(menu_hwnd);
                    let (w, h) = (w.max(1), h.max(1));
                    match print_window(menu_hwnd, w, h) {
                        Ok((px, hbm)) => {
                            let res = report_and_save("menu", &px, hbm, &out2, "");
                            // SAFETY: bitmap released after the save.
                            unsafe { DeleteObject(hbm as *mut c_void) };
                            res?;
                        }
                        Err(e) => return Err(e),
                    }
                    break;
                }
                std::thread::sleep(std::time::Duration::from_millis(100));
            }
            // SAFETY: cross-desktop post to our own window; ends tracking.
            unsafe { win32::PostMessageW(owner as HWND, win32::WM_CANCELMODE, 0, 0) };
            Ok(())
        });
        // SAFETY: `owner` is our tray window on this thread; the modal
        // `TrackPopupMenu` loop dispatches our own messages until the watcher
        // cancels tracking (selection 0 = cancel = no dispatch).
        unsafe { tray::show_tray_menu(owner as HWND) };
        watcher.join().map_err(|_| "watcher panicked".to_string())?
    })();
    leave_hidden_desktop(desk, prev);
    r
}

/// `fixmatch <out_unmatched.png> <out_matched.png>` -- capture the REAL "Fix
/// match" picker dialog in BOTH states on the hidden desktop.
///
/// The picker has no `paint_to_hdc` twin: it is stock STATIC/EDIT/BUTTON/
/// LISTBOX controls painted by the dialog manager, not a GDI paint function,
/// so there is no offscreen entry point to reuse. This captures the live
/// dialog with `PrintWindow` instead, the same way `mode_menu` captures the
/// tray popup: seed `live_now_playing`, run the public `open_fix_match()`
/// modal on this thread, and let a watcher thread on the same hidden desktop
/// wait for `fix_match_dialog_hwnd()`, capture it, and close it with
/// `WM_CLOSE` (which ends the modal loop).
///
/// State 1 (unmatched/Search screen) runs with no override store; state 2
/// (matched screen) pre-seeds a provider-free movie latch for the same fake
/// path. A scratch `LOCALAPPDATA` (restored afterwards) keeps the seed out of
/// owner state. No provider is ever contacted: the Search screen lists only
/// after the user clicks Search, and the Matched screen reads the local store.
/// The expected screen is asserted by client height (Search ~= 588px, Matched
/// ~= 244px at 96 DPI), so the two PNGs can never silently show the same
/// state.
fn mode_fixmatch(args: &[String]) -> Result<(), String> {
    let out_un = args
        .get(2)
        .cloned()
        .ok_or("fixmatch <out_unmatched.png> <out_matched.png>")?;
    let out_m = args
        .get(3)
        .cloned()
        .ok_or("fixmatch <out_unmatched.png> <out_matched.png>")?;
    // Scratch the cache root BEFORE any store/config read, restored below.
    let scratch =
        std::env::temp_dir().join(format!("potflix-cap-fixmatch-{}", std::process::id()));
    std::fs::create_dir_all(&scratch).map_err(|e| format!("scratch dir: {e}"))?;
    let prev_localappdata = std::env::var_os("LOCALAPPDATA");
    // SAFETY: this harness owns its process environment for its own lifetime.
    unsafe { std::env::set_var("LOCALAPPDATA", &scratch) };
    let (desk, prev) = enter_hidden_desktop("fixmatch")?;
    let r = (|| {
        window::create_main_window().map_err(|e| format!("create_main_window: {e}"))?;
        {
            let mut st = window::ui();
            st.cur_view = window::View::Main;
            st.focus_id = 0;
            st.refresh_dir_list();
        }
        window::show_main_window();
        pump(800);
        let path = format!("{}:{}", 'C', r"\Media\Dune.Part.Two.2024.1080p.mkv");
        app_support::config::set_live_now_playing(Some(
            app_support::config::NowPlaying {
                raw_title: "Dune.Part.Two.2024.1080p.mkv - PotPlayer".to_string(),
                path: path.clone(),
            },
        ));
        // State 1: no store file on the scratch root -> the Search screen.
        capture_fix_match_once(desk, &out_un, "fixmatch-unmatched", true)?;
        // State 2: a provider-free movie latch for the same path -> Matched.
        let mut store =
            metadata::overrides::Store::empty_at(metadata::overrides::default_path());
        store
            .put(metadata::overrides::Override {
                scope: metadata::overrides::Scope::Movie,
                key: metadata::overrides::movie_key(&path),
                title: "Dune: Part Two".to_string(),
                year: 2024,
                provider: String::new(),
                provider_id: String::new(),
                poster: String::new(),
                media: None,
                episodes: Vec::new(),
                created_at_ms: 0,
            })
            .map_err(|e| format!("seed latch: {e}"))?;
        capture_fix_match_once(desk, &out_m, "fixmatch-matched", false)?;
        app_support::config::set_live_now_playing(None);
        Ok(())
    })();
    leave_hidden_desktop(desk, prev);
    // SAFETY: restoring this process's own environment (see above).
    unsafe {
        match prev_localappdata {
            Some(v) => std::env::set_var("LOCALAPPDATA", v),
            None => std::env::remove_var("LOCALAPPDATA"),
        }
    }
    r
}

/// Run the public picker modal once and capture its dialog: a watcher thread
/// on the same hidden desktop waits for the dialog, captures it with
/// `PrintWindow`, and closes it, which ends the modal loop on this thread.
/// `want_tall` selects the expected screen by client height (Search is the
/// tall screen, Matched the short one); a mismatch fails, so both states are
/// proven distinct.
fn capture_fix_match_once(
    desk: usize,
    out: &str,
    tag: &str,
    want_tall: bool,
) -> Result<(), String> {
    let out2 = out.to_string();
    let tag2 = tag.to_string();
    let watcher = std::thread::spawn(move || -> Result<(), String> {
        // SAFETY: this thread owns no windows, so the switch is legal.
        if unsafe { SetThreadDesktop(desk as *mut c_void) } == 0 {
            return Err("watcher SetThreadDesktop failed".into());
        }
        for _ in 0..150 {
            let dlg = window::fix_match_dialog_hwnd();
            if !dlg.is_null() {
                std::thread::sleep(std::time::Duration::from_millis(700));
                let (w, h) = client_size(dlg);
                let (w, h) = (w.max(1), h.max(1));
                // The tall screen is Search (588 logical px), the short one
                // Matched (244); the midpoint never equals either.
                if (h >= 400) != want_tall {
                    return Err(format!("{tag2}: client {w}x{h} is not the expected screen"));
                }
                let dpi = window::window_dpi(dlg);
                match print_window(dlg, w, h) {
                    Ok((px, hbm)) => {
                        let extra = format!("dpi={dpi} client={w}x{h}");
                        let res = report_and_save(tag2.as_str(), &px, hbm, &out2, &extra);
                        // SAFETY: bitmap released after the save.
                        unsafe { DeleteObject(hbm as *mut c_void) };
                        res?;
                    }
                    Err(e) => return Err(e),
                }
                // SAFETY: our dialog; WM_CLOSE runs its teardown path, which
                // ends the modal loop on the main thread.
                unsafe { win32::PostMessageW(dlg, win32::WM_CLOSE, 0, 0) };
                return Ok(());
            }
            std::thread::sleep(std::time::Duration::from_millis(100));
        }
        Err(format!("{tag2}: dialog never appeared"))
    });
    // The modal picker on THIS thread (it owns the main window).
    window::open_fix_match();
    watcher.join().map_err(|_| "watcher panicked".to_string())?
}

fn mode_offscreen(args: &[String]) -> Result<(), String> {
    let out = args
        .get(2)
        .cloned()
        .ok_or("offscreen <out.png> <dpi> <view>")?;
    let dpi: i32 = args
        .get(3)
        .ok_or("offscreen <out.png> <dpi> <view>")?
        .parse()
        .map_err(|_| "bad dpi")?;
    let view = view_of(&args.get(4).cloned().unwrap_or("main".into()))?;
    {
        let mut st = window::ui();
        st.cur_view = view;
        st.focus_id = window::BTN_ADD;
        st.refresh_dir_list();
    }
    let w = window::scale_px(dpi, window::MAIN_CLIENT_W);
    let h = window::scale_px(dpi, window::MAIN_CLIENT_H);
    // SAFETY: DIB created and released within this block.
    unsafe {
        let (mem, bmp, bits) = create_dib(w, h)?;
        let mem = mem as *mut c_void;
        let old = SelectObject(mem, bmp as *mut c_void);
        let rc = potflix_win::win32::Rect {
            left: 0,
            top: 0,
            right: w,
            bottom: h,
        };
        window::paint_to_hdc(mem, rc, dpi);
        SelectObject(mem, old);
        let px = std::slice::from_raw_parts(bits, (w as usize) * (h as usize) * 4).to_vec();
        let extra = format!("dpi={dpi} size={w}x{h}");
        let r = report_and_save("offscreen", &px, bmp, &out, &extra);
        DeleteObject(bmp as *mut c_void);
        DeleteDC(mem);
        r
    }
}

/// Keyboard-traversal trace through the REAL wndproc: Tab/Shift+Tab order per
/// view, Enter/Space activation shape, Esc behavior. Captured, printed, and
/// asserted (a full Tab cycle must return to its start).
fn mode_trace() -> Result<(), String> {
    let (desk, prev) = enter_hidden_desktop("trace")?;
    let r = (|| {
        window::create_main_window().map_err(|e| format!("create_main_window: {e}"))?;
        for view in [
            window::View::Main,
            window::View::Settings,
            window::View::Card,
        ] {
            {
                let mut st = window::ui();
                st.cur_view = view;
                st.focus_id = 0;
            }
            let hwnd = window::ui().main_hwnd as HWND;
            let order = window::focus_order_for_view(view);
            print!("TRACE view={view:?} order={order:?} walk=");
            let mut walk = vec![0];
            for _ in 0..order.len() {
                unsafe {
                    win32::SendMessageW(hwnd, window::WM_KEYDOWN, window::VK_TAB as usize, 0)
                };
                walk.push(window::ui().focus_id);
            }
            println!("{walk:?}");
            // Full cycle: after len(order) Tabs the walk returns to the first stop.
            if walk.len() != order.len() + 1 || walk[1..] != order[..] {
                return Err(format!(
                    "view {view:?}: Tab walk {walk:?} != order {order:?}"
                ));
            }
            // Shift+Tab from the first stop wraps to the last.
            {
                let mut st = window::ui();
                st.focus_id = order[0];
                st.on_settings_key_down_ex(hwnd, window::VK_TAB, true);
                let last = st.focus_id;
                println!("TRACE view={view:?} shift-tab from {} -> {last}", order[0]);
                if last != order[order.len() - 1] {
                    return Err(format!("view {view:?}: Shift+Tab did not wrap"));
                }
            }
            // Esc: Card closes to Main; Settings returns to Main.
            {
                let mut st = window::ui();
                st.cur_view = view;
                let consumed = st.on_settings_key_down(hwnd, window::VK_ESCAPE);
                println!(
                    "TRACE view={view:?} esc consumed={consumed} now={:?}",
                    st.cur_view
                );
            }
        }
        println!("TRACE-OK");
        Ok(())
    })();
    leave_hidden_desktop(desk, prev);
    r
}

/// `hold <view> [empty]` -- render one view on the hidden desktop and HOLD the
/// window open so an external runner (`scripts\desktop-headless.ps1`) can drive
/// and capture it. The window is the REAL one: same `create_main_window`, same
/// wndproc, same paint.
///
/// `empty` clears the folder list (used with a scratch `LOCALAPPDATA`), so the
/// empty state is a real state and not a mock.
fn mode_hold(args: &[String]) -> Result<(), String> {
    let view = view_of(&args.get(2).cloned().unwrap_or("main".into()))?;
    let empty = args.get(3).is_some_and(|a| a == "empty");
    let hold_ms: u64 = args
        .get(4)
        .map(|s| s.parse().unwrap_or(60_000))
        .unwrap_or(60_000);
    // NO `enter_hidden_desktop` here: the launcher (`desktop-headless.ps1`)
    // already started this process on its own private desktop via
    // `STARTUPINFO.lpDesktop`, and switching this thread to a SECOND desktop
    // would put the window somewhere the launcher cannot see it.
    window::create_main_window().map_err(|e| format!("create_main_window: {e}"))?;
    {
        let mut st = window::ui();
        st.cur_view = view;
        st.focus_id = 0;
        st.refresh_dir_list();
        if empty {
            st.ui_dirs.clear();
            st.sel_row = -1;
            st.scroll_top = 0;
        }
    }
    let hwnd = window::ui().main_hwnd as HWND;
    println!("HOLD view={view:?} empty={empty} hwnd={hwnd:p}");
    println!(
        "HOLD class-icon=0x{:X}",
        potflix_win::brand::class_icon(hwnd) as usize
    );
    // SAFETY: ShowWindow/UpdateWindow only affect `hwnd`.
    unsafe {
        win32::ShowWindow(hwnd, win32::SW_SHOW);
        win32::UpdateWindow(hwnd);
    }
    // Pump this thread's queue so the window is live and responsive; the runner
    // posts WM_CLOSE (which parks the window, never destroys it) and then ends
    // the process.
    let end = std::time::Instant::now() + std::time::Duration::from_millis(hold_ms);
    while std::time::Instant::now() < end {
        pump(100);
    }
    Ok(())
}

/// `facts` -- the redesign's own measurements, read from the real code path:
/// the class-icon handle, the face each brand font actually selects, the action
/// grid's numbers, and the palette's contrast ratios.
fn mode_facts() -> Result<(), String> {
    let (desk, prev) = enter_hidden_desktop("facts")?;
    let r = (|| {
        window::create_main_window().map_err(|e| format!("create_main_window: {e}"))?;
        let hwnd = window::ui().main_hwnd as HWND;
        if hwnd.is_null() {
            return Err("no main window".into());
        }
        let sc = |v: i32| window::scale_px(96, v);

        // --- the icon -------------------------------------------------------
        let cls = potflix_win::brand::class_icon(hwnd);
        println!("FACT class-icon GCLP_HICON=0x{:X} non-zero={}", cls as usize, !cls.is_null());
        let small = unsafe {
            windows_sys::Win32::UI::WindowsAndMessaging::GetClassLongPtrW(
                hwnd,
                windows_sys::Win32::UI::WindowsAndMessaging::GCLP_HICONSM,
            )
        };
        println!("FACT class-icon GCLP_HICONSM=0x{small:X} non-zero={}", small != 0);
        // WM_GETICON answers the WINDOW slots (what the titlebar and alt-tab read).
        for (name, which) in [("ICON_BIG", win32::ICON_BIG), ("ICON_SMALL", win32::ICON_SMALL)] {
            let h = unsafe {
                win32::SendMessageW(hwnd, win32::WM_GET_ICON, which as usize, 0)
            };
            println!("FACT window-icon {name}=0x{h:X} non-zero={}", h != 0);
        }

        // --- the faces ------------------------------------------------------
        let hdc = unsafe { windows_sys::Win32::Graphics::Gdi::GetDC(hwnd) };
        let display = potflix_win::brand::display(sc(20));
        let ui = potflix_win::brand::ui(sc(13), potflix_win::brand::UI_WEIGHT);
        println!(
            "FACT face wordmark={:?} want={:?}",
            potflix_win::brand::face_of(hdc, display),
            potflix_win::brand::DISPLAY_FAMILY
        );
        println!(
            "FACT face ui={:?} want={:?}",
            potflix_win::brand::face_of(hdc, ui),
            potflix_win::brand::UI_FAMILY
        );
        // SAFETY: the fonts and DC were created above.
        unsafe {
            windows_sys::Win32::Graphics::Gdi::DeleteObject(display);
            windows_sys::Win32::Graphics::Gdi::DeleteObject(ui);
            windows_sys::Win32::Graphics::Gdi::ReleaseDC(hwnd, hdc);
        }

        // --- the grid -------------------------------------------------------
        let (cw, ch) = (sc(window::MAIN_CLIENT_W), sc(window::MAIN_CLIENT_H));
        let l = window::compute_layout(&dirs(4), cw, ch, 96);
        let mut grid: Vec<&window::UiButton> = l
            .btns
            .iter()
            .filter(|b| b.style != window::BTN_TAB && b.style != window::BTN_TAB_SEL)
            .collect();
        grid.sort_by_key(|b| (b.rc.top, b.rc.left));
        for b in &grid {
            println!(
                "FACT grid id={} x=[{},{}] y=[{},{}] w={} h={}",
                b.id,
                b.rc.left,
                b.rc.right,
                b.rc.top,
                b.rc.bottom,
                b.rc.right - b.rc.left,
                b.rc.bottom - b.rc.top
            );
        }
        let widths: std::collections::HashSet<i32> =
            grid.iter().map(|b| b.rc.right - b.rc.left).collect();
        let heights: std::collections::HashSet<i32> =
            grid.iter().map(|b| b.rc.bottom - b.rc.top).collect();
        let xs: std::collections::HashSet<i32> = grid.iter().map(|b| b.rc.left).collect();
        println!(
            "FACT grid equal-width={} distinct-widths={widths:?} distinct-heights={heights:?} distinct-x={xs:?}",
            widths.len() == 1
        );
        let tabs = window::tab_strip_rects(cw, 96);
        println!(
            "FACT tabstrip card=[{},{}] settings=[{},{}] gap={} equal-width={}",
            tabs.0.left,
            tabs.0.right,
            tabs.1.left,
            tabs.1.right,
            tabs.1.left - tabs.0.right,
            (tabs.0.right - tabs.0.left) == (tabs.1.right - tabs.1.left)
        );
        println!(
            "FACT panel rows_visible={} overflow={} panel=[{},{},{}]",
            l.list_capacity, l.overflow, l.card.top, l.card.bottom, l.card.right
        );

        // --- contrast -------------------------------------------------------
        use potflix_win::brand as b;
        for (name, fg, bg) in [
            ("CARD_HDR/BLACK", b::CARD_HDR, b::BLACK),
            ("CARD_HDR/CARD", b::CARD_HDR, b::CARD),
            ("CARD_MUT/BLACK", b::CARD_MUT, b::BLACK),
            ("WHITE/RED", b::WHITE, b::RED),
            ("DANGER_TEXT/CARD", b::DANGER_TEXT, b::CARD),
            ("RED/BLACK", b::RED, b::BLACK),
            ("EDGE/BLACK", b::EDGE, b::BLACK),
            ("TRACK/BLACK", b::TRACK, b::BLACK),
        ] {
            println!("FACT contrast {name}={:.2}:1", b::contrast(fg, bg));
        }
        println!("FACTS-OK");
        Ok(())
    })();
    leave_hidden_desktop(desk, prev);
    r
}

fn main() {
    let args: Vec<String> = std::env::args().collect();
    let mode = args.get(1).cloned().unwrap_or_default();
    let r = match mode.as_str() {
        "window" => mode_window(&args),
        "menu" => mode_menu(&args),
        "offscreen" => mode_offscreen(&args),
        "fixmatch" => mode_fixmatch(&args),
        "trace" => mode_trace(),
        "hold" => mode_hold(&args),
        "facts" => mode_facts(),
        _ => Err("usage: uicapture (window|menu|offscreen|fixmatch|trace|hold|facts) ...".into()),
    };
    if let Err(e) = r {
        eprintln!("UICAPTURE-FAIL: {e}");
        std::process::exit(1);
    }
    println!("UICAPTURE-PASS");
}
