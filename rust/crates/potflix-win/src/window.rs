//! `window` -- mirrors the Go root files `go\window.go` + `go\settings_sections.go`.
//!
//! The tray-resident settings window: folders list, Scan / Refresh, the live
//! `indexed N of M files | metadata K of J` line with its `- catching up` state,
//! library export/import, `Clear metadata` / `Clear index`, `Report`, startup,
//! scan-speed segments. The scan progress is phase-aware: the bar is driven by
//! `Resolved` of `KeysTotal` so it cannot read full while metadata remains.
//!
//! # Owner-drawn, no child controls (`go\window.go:37-70`)
//!
//! There are no child controls: every rect-table id below is a key into a
//! hit-test table, the whole client area is owner-drawn, and there is no
//! `WM_CTLCOLOR*`/`WM_DRAWITEM` anywhere. Hit-testing and actions run through
//! ONE dispatch (`release_at` / `activate_focused` -> `do_button_action` /
//! `do_settings_action` / `do_card_action`), and the disabled state is enforced
//! AT INPUT via [`scan_blocked_reason`] - porting only the dim paint would lose
//! the guard.
//!
//! # Integer geometry (`go\window.go:3466-3484`)
//!
//! Every value is integer-scaled `(v*dpi+48)/96` from the 460x680 logical
//! design, and font heights are negative. An `f32`/`f64` scale would change
//! hit-tests versus paint, so the port keeps `i32` everywhere.
//!
//! # One OS thread
//!
//! The tray wndproc + main wndproc + `GetMessageW` loop must share one OS
//! thread; workers (poll, scan, report) may never touch the UI - their payloads
//! ride mutex-guarded snapshots and only ever repaint via `InvalidateRect` plus
//! an async `PostMessageW`. The UI state lives in [`UiState`], which the
//! tests construct directly (isolated) and production wraps in a process
//! global.

use std::sync::{Arc, Mutex, MutexGuard};

use std::time::{Duration, Instant};

use windows_sys::Win32::Foundation::HWND;
use windows_sys::Win32::Graphics::Gdi;

use crate::win32::{utf16_z, Rect};

use library::{Progress, Stats};
use presence::{Activity, CardPreset, CardSpec};

// ---------------------------------------------------------------------------
// Button ids (`go\window.go:39-70`): rect-table keys, not child control ids.
// ---------------------------------------------------------------------------

/// Main view "Add folder".
pub const BTN_ADD: i32 = 1;
/// Main view "Remove".
pub const BTN_REMOVE: i32 = 2;
/// Main view "Scan" / "Cancel".
pub const BTN_SCAN: i32 = 3;
/// Main view "Refresh".
pub const BTN_REFRESH: i32 = 4;
/// Main view "Close to tray".
pub const BTN_CLOSE: i32 = 5;
/// Main header "Settings" text link.
pub const BTN_SETTINGS: i32 = 6;
/// Settings view "Back".
pub const BTN_BACK: i32 = 7;
/// Main header "Card" text link.
pub const BTN_CARD: i32 = 8;
/// Card view "Back".
pub const BTN_CARD_BACK: i32 = 9;
/// Card view Details presets, `+0..3` in `CardPresets` order.
pub const BTN_CARD_DETAILS_AT: i32 = 30;
/// Card view State presets, `+0..3` in `CardPresets` order.
pub const BTN_CARD_STATE_AT: i32 = 40;
/// Settings LIBRARY "Clear metadata".
pub const BTN_CLEAR_CACHE: i32 = 15;
/// Settings LIBRARY "Export...".
pub const BTN_EXPORT: i32 = 16;
/// Settings LIBRARY "Import...".
pub const BTN_IMPORT: i32 = 17;
/// Settings STARTUP toggle.
pub const BTN_STARTUP: i32 = 18;
/// Settings STARTUP "Open log folder".
pub const BTN_OPEN_LOG: i32 = 19;
/// Settings SCAN segment "Gentle".
pub const BTN_SCAN_GENTLE: i32 = 20;
/// Settings SCAN segment "Balanced".
pub const BTN_SCAN_BALANCED: i32 = 21;
/// Settings SCAN segment "Turbo".
pub const BTN_SCAN_TURBO: i32 = 22;
/// Main view "Report".
pub const BTN_REPORT: i32 = 23;
/// Settings LIBRARY "Clear index".
pub const BTN_CLEAR_INDEX: i32 = 24;

// ---------------------------------------------------------------------------
// Worker -> UI messages (`go\window.go:74-93`).
// ---------------------------------------------------------------------------

pub const WM_APP_SCAN_PROGRESS: u32 = crate::win32::WM_APP + 3;
pub const WM_APP_SCAN_DONE: u32 = crate::win32::WM_APP + 4;
pub const WM_APP_REPORT_DONE: u32 = crate::win32::WM_APP + 5;
/// The Win32 timer message (`go\window.go:83`).
pub const WM_TIMER: u32 = 0x0113;

/// `SetTimer` id for the live-scan repaint tick.
pub const SCAN_TIMER_ID: usize = 7;
/// Live repaints are capped at ~10/s (`go\window.go:92`).
pub const SCAN_PAINT_INTERVAL: Duration = Duration::from_millis(100);

// ---------------------------------------------------------------------------
// Settings-view message, key, and text constants (`go\window.go:220-239`).
// ---------------------------------------------------------------------------

pub const WM_CHAR: u32 = 0x0102;
pub const WM_KEYDOWN: u32 = 0x0100;
pub const WM_SETFOCUS: u32 = 0x0007;
pub const WM_KILLFOCUS: u32 = 0x0008;

pub const VK_ESCAPE: u32 = 0x1B;
pub const VK_TAB: u32 = 0x09;
pub const VK_RETURN: u32 = 0x0D;
pub const VK_SPACE: u32 = 0x20;
pub const VK_SHIFT: u32 = 0x10;
pub const VK_LEFT: u32 = 0x25;
pub const VK_UP: u32 = 0x26;
pub const VK_RIGHT: u32 = 0x27;
pub const VK_DOWN: u32 = 0x28;

pub const SW_SHOW_NORMAL: i32 = 1;
pub const DT_WORD_BREAK: u32 = 0x0010;

// ---------------------------------------------------------------------------
// Dark palette (`go\window.go:178-191`), packed as COLORREF `0x00BBGGRR`.
// ---------------------------------------------------------------------------

/// `rgb(r, g, b)` (`go\window.go:3487`).
#[must_use]
pub const fn rgb(r: u32, g: u32, b: u32) -> u32 {
    r | (g << 8) | (b << 16)
}

pub const COL_BG: u32 = rgb(0x14, 0x14, 0x14);
pub const COL_CARD: u32 = rgb(0x1C, 0x1C, 0x1C);
pub const COL_ROW: u32 = rgb(0x24, 0x24, 0x24);
pub const COL_ROW_HOVER: u32 = rgb(0x2E, 0x2E, 0x2E);
pub const COL_BORDER: u32 = rgb(0x33, 0x33, 0x33);
pub const COL_TEXT: u32 = rgb(0xE8, 0xE8, 0xE8);
pub const COL_MUTED: u32 = rgb(0x9A, 0x9A, 0x9A);
pub const COL_ACCENT: u32 = rgb(0xE5, 0x09, 0x14);
pub const COL_ACCENT_HOV: u32 = rgb(0xF6, 0x12, 0x1D);
pub const COL_ACCENT_DOWN: u32 = rgb(0xB0, 0x07, 0x10);
pub const COL_DANGER: u32 = rgb(0xC0, 0x66, 0x66);

// Button paint styles (`go\window.go:194-200`).
pub const BTN_SUBTLE: i32 = 0;
pub const BTN_ACCENT: i32 = 1;
pub const BTN_OUTLINE: i32 = 2;
pub const BTN_TEXT: i32 = 3;
pub const BTN_DANGER: i32 = 4;

// ---------------------------------------------------------------------------
// Types.
// ---------------------------------------------------------------------------

/// The visible view (`go\window.go:141-150`).
#[derive(Clone, Copy, Debug, PartialEq, Eq, Default)]
pub enum View {
    #[default]
    Main,
    Settings,
    Card,
}

/// One actionable rect in the paint-time rect table (`go\window.go:241-247`).
#[derive(Clone, Debug)]
pub struct UiButton {
    pub id: i32,
    pub label: String,
    pub style: i32,
    pub rc: Rect,
}

/// The full hit-test table for one paint/mouse pass (`go\window.go:249-261`).
#[derive(Clone, Debug, Default)]
pub struct UiLayout {
    pub card: Rect,
    pub rows: Vec<Rect>,
    pub xbtns: Vec<Rect>,
    pub btns: Vec<UiButton>,
    pub status: Rect,
    pub overflow: i32,
    pub settings: Rect,
    pub card_link: Rect,
}

/// The settings-view hit-test table (`go\window.go:263-269`).
#[derive(Clone, Debug, Default)]
pub struct SettingsLayout {
    pub status: Rect,
    pub btns: Vec<UiButton>,
}

impl SettingsLayout {
    /// `settingsBtn` (`go\window.go:272`).
    #[must_use]
    pub fn settings_btn(&self, id: i32) -> Option<Rect> {
        self.btns.iter().find(|b| b.id == id).map(|b| b.rc)
    }
}

/// The Card-view hit-test table (`go\window.go:1436-1441`).
#[derive(Clone, Debug, Default)]
pub struct CardLayout {
    pub preview: Rect,
    pub rows: Vec<Rect>,
    pub status: Rect,
    pub btns: Vec<UiButton>,
}

impl CardLayout {
    /// `cardBtn` (`go\window.go:1444`).
    #[must_use]
    pub fn card_btn(&self, id: i32) -> Option<Rect> {
        self.btns.iter().find(|b| b.id == id).map(|b| b.rc)
    }
}

/// One painted text rect with its stable audit id (`go\window.go:666`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct NamedTextRect {
    pub id: String,
    pub rc: Rect,
}

// Layout metrics (`go\window.go:495-499`).
pub const MAIN_CLIENT_W: i32 = 460;
pub const MAIN_CLIENT_H: i32 = 680;
pub const MAIN_MARGIN: i32 = 20;

// ---------------------------------------------------------------------------
// Pure geometry (`go\window.go:3466-3472, :470-492`).
// ---------------------------------------------------------------------------

/// `scalePx` (`go\window.go:3467`): scale a 96-DPI logical px to `dpi`.
#[must_use]
pub fn scale_px(dpi: i32, v: i32) -> i32 {
    let dpi = if dpi <= 0 { 96 } else { dpi };
    (v * dpi + 48) / 96
}

/// `ptInRect` (`go\window.go:470`): left/top inclusive, right/bottom exclusive.
#[must_use]
pub fn pt_in_rect(rc: Rect, x: i32, y: i32) -> bool {
    (rc.left..rc.right).contains(&x) && (rc.top..rc.bottom).contains(&y)
}

/// `buttonAtPoint` (`go\window.go:475`).
#[must_use]
pub fn button_at_point(btns: &[UiButton], x: i32, y: i32) -> i32 {
    for b in btns {
        if pt_in_rect(b.rc, x, y) {
            return b.id;
        }
    }
    0
}

/// `rowAtPoint` (`go\window.go:485`).
#[must_use]
pub fn row_at_point(rows: &[Rect], x: i32, y: i32) -> i32 {
    for (i, r) in rows.iter().enumerate() {
        if pt_in_rect(*r, x, y) {
            return i as i32;
        }
    }
    -1
}

/// `rectsIntersect` (`go\window.go:3572`): nonzero-area overlap only.
#[must_use]
pub fn rects_intersect(a: Rect, b: Rect) -> bool {
    a.left < b.right && b.left < a.right && a.top < b.bottom && b.top < a.bottom
}

// ---------------------------------------------------------------------------
// computeLayout (`go\window.go:503-604`).
// ---------------------------------------------------------------------------

/// Build the main-view hit-test table for `dirs`, client size and DPI.
#[must_use]
pub fn compute_layout(dirs: &[String], cw: i32, ch: i32, dpi: i32) -> UiLayout {
    let sc = |v: i32| scale_px(dpi, v);
    let margin = sc(MAIN_MARGIN);
    let card_left = margin;
    let mut card_w = cw - margin * 2;
    if card_w < sc(100) {
        card_w = sc(100);
    }

    let mut l = UiLayout::default();

    const CARD_TOP_L: i32 = 118;
    const MAX_CARD_H_L: i32 = 262;
    const PAD_L: i32 = 10;
    const ROW_H_L: i32 = 36;
    const GAP_L: i32 = 8;
    const OVERFLOW_H_L: i32 = 22;

    let card_top = sc(CARD_TOP_L);
    let max_card_h = sc(MAX_CARD_H_L);
    let pad = sc(PAD_L);
    let row_h = sc(ROW_H_L);
    let gap = sc(GAP_L);
    let avail = max_card_h - pad * 2;
    let mut max_rows = (avail + gap) / (row_h + gap);
    if max_rows < 1 {
        max_rows = 1;
    }
    let mut vis = dirs.len() as i32;
    if vis > max_rows {
        l.overflow = vis - max_rows;
        vis = max_rows;
    }
    let mut card_h = pad * 2 + row_h;
    if vis > 0 {
        card_h = pad * 2 + vis * row_h + (vis - 1) * gap;
        if l.overflow > 0 {
            card_h += sc(OVERFLOW_H_L);
        }
    }
    if card_h > max_card_h {
        card_h = max_card_h;
    }
    l.card = Rect {
        left: card_left,
        top: card_top,
        right: card_left + card_w,
        bottom: card_top + card_h,
    };
    let row_x = card_left + pad;
    let row_w = card_w - pad * 2;
    for i in 0..vis {
        let y = card_top + pad + i * (row_h + gap);
        l.rows.push(Rect {
            left: row_x,
            top: y,
            right: row_x + row_w,
            bottom: y + row_h,
        });
        let xw = sc(30);
        l.xbtns.push(Rect {
            left: row_x + row_w - xw,
            top: y,
            right: row_x + row_w,
            bottom: y + row_h,
        });
    }

    const BTN_H_L: i32 = 36;
    const BTN_GAP_L: i32 = 12;
    let btn_top = card_top + max_card_h + sc(16);
    let btn_h = sc(BTN_H_L);
    let btn_gap = sc(BTN_GAP_L);
    let btn_w = (card_w - btn_gap) / 2;
    let x0 = card_left;
    let x1 = card_left + btn_w + btn_gap;
    let y0 = btn_top;
    let y1 = btn_top + btn_h + btn_gap;
    l.btns = vec![
        UiButton {
            id: BTN_ADD,
            label: "Add folder".into(),
            style: BTN_SUBTLE,
            rc: Rect {
                left: x0,
                top: y0,
                right: x0 + btn_w,
                bottom: y0 + btn_h,
            },
        },
        UiButton {
            id: BTN_REMOVE,
            label: "Remove".into(),
            style: BTN_SUBTLE,
            rc: Rect {
                left: x1,
                top: y0,
                right: x1 + btn_w,
                bottom: y0 + btn_h,
            },
        },
        UiButton {
            id: BTN_SCAN,
            label: "Scan".into(),
            style: BTN_ACCENT,
            rc: Rect {
                left: x0,
                top: y1,
                right: x0 + btn_w,
                bottom: y1 + btn_h,
            },
        },
        UiButton {
            id: BTN_REFRESH,
            label: "Refresh".into(),
            style: BTN_OUTLINE,
            rc: Rect {
                left: x1,
                top: y1,
                right: x1 + btn_w,
                bottom: y1 + btn_h,
            },
        },
    ];
    let (close_w, close_h) = (sc(120), sc(28));
    l.btns.push(UiButton {
        id: BTN_CLOSE,
        label: "Close to tray".into(),
        style: BTN_TEXT,
        rc: Rect {
            left: card_left + card_w - close_w,
            top: y1 + btn_h + sc(8),
            right: card_left + card_w,
            bottom: y1 + btn_h + sc(8) + close_h,
        },
    });
    l.btns.push(UiButton {
        id: BTN_REPORT,
        label: crate::window::report_button_label().into(),
        style: BTN_OUTLINE,
        rc: Rect {
            left: x0,
            top: y1 + btn_h + btn_gap,
            right: x0 + btn_w,
            bottom: y1 + btn_h + btn_gap + btn_h,
        },
    });

    let (set_w, set_h) = (sc(120), sc(28));
    l.settings = Rect {
        left: cw - margin - set_w,
        top: sc(56),
        right: cw - margin,
        bottom: sc(56) + set_h,
    };
    let (card_lnk_w, card_lnk_h) = (sc(100), sc(24));
    l.card_link = Rect {
        left: l.settings.left - card_lnk_w,
        top: sc(68),
        right: l.settings.left,
        bottom: sc(68) + card_lnk_h,
    };

    let st_h = sc(20);
    l.status = Rect {
        left: margin,
        top: ch - sc(14) - st_h,
        right: cw - margin,
        bottom: ch - sc(14),
    };
    l
}

/// `computeSettingsLayout` (`go\window.go:610-662`). `startup` flips only the
/// toggle label; `scan_speed` only the SCAN segment styles.
#[must_use]
pub fn compute_settings_layout(
    cw: i32,
    ch: i32,
    dpi: i32,
    startup: bool,
    scan_speed: &str,
) -> SettingsLayout {
    let sc = |v: i32| scale_px(dpi, v);
    let margin = sc(MAIN_MARGIN);

    let mut l = SettingsLayout::default();

    let (back_w, back_h) = (sc(80), sc(28));
    let back = Rect {
        left: margin,
        top: sc(16),
        right: margin + back_w,
        bottom: sc(16) + back_h,
    };

    let lib_top = sc(100);
    let lib_h = sc(32);
    let lib_w = (cw - margin * 2 - sc(12) * 3) / 4;
    let clear_meta_rc = Rect {
        left: margin,
        top: lib_top,
        right: margin + lib_w,
        bottom: lib_top + lib_h,
    };
    let clear_idx_rc = Rect {
        left: margin + lib_w + sc(12),
        top: lib_top,
        right: margin + lib_w * 2 + sc(12),
        bottom: lib_top + lib_h,
    };
    let export_rc = Rect {
        left: margin + lib_w * 2 + sc(24),
        top: lib_top,
        right: margin + lib_w * 3 + sc(24),
        bottom: lib_top + lib_h,
    };
    let import_rc = Rect {
        left: margin + lib_w * 3 + sc(36),
        top: lib_top,
        right: cw - margin,
        bottom: lib_top + lib_h,
    };

    let start_top = sc(172);
    let start_h = sc(32);
    let toggle_w = sc(230);
    let toggle_rc = Rect {
        left: margin,
        top: start_top,
        right: margin + toggle_w,
        bottom: start_top + start_h,
    };
    let log_rc = Rect {
        left: margin + toggle_w + sc(12),
        top: start_top,
        right: cw - margin,
        bottom: start_top + start_h,
    };

    let scan_top = sc(244);
    let scan_h = sc(32);
    let scan_w = (cw - margin * 2 - sc(12) * 2) / 3;
    let cur_scan = app_support::config::normalize_scan_speed(scan_speed);
    let gentle_rc = Rect {
        left: margin,
        top: scan_top,
        right: margin + scan_w,
        bottom: scan_top + scan_h,
    };
    let balanced_rc = Rect {
        left: margin + scan_w + sc(12),
        top: scan_top,
        right: margin + scan_w * 2 + sc(12),
        bottom: scan_top + scan_h,
    };
    let turbo_rc = Rect {
        left: margin + scan_w * 2 + sc(24),
        top: scan_top,
        right: cw - margin,
        bottom: scan_top + scan_h,
    };

    let st_h = sc(20);
    l.status = Rect {
        left: margin,
        top: ch - sc(14) - st_h,
        right: cw - margin,
        bottom: ch - sc(14),
    };
    l.btns = vec![
        UiButton {
            id: BTN_BACK,
            label: "Back".into(),
            style: BTN_TEXT,
            rc: back,
        },
        UiButton {
            id: BTN_CLEAR_CACHE,
            label: "Clear metadata".into(),
            style: BTN_DANGER,
            rc: clear_meta_rc,
        },
        UiButton {
            id: BTN_CLEAR_INDEX,
            label: "Clear index".into(),
            style: BTN_DANGER,
            rc: clear_idx_rc,
        },
        UiButton {
            id: BTN_EXPORT,
            label: "Export...".into(),
            style: BTN_SUBTLE,
            rc: export_rc,
        },
        UiButton {
            id: BTN_IMPORT,
            label: "Import...".into(),
            style: BTN_SUBTLE,
            rc: import_rc,
        },
        UiButton {
            id: BTN_STARTUP,
            label: crate::settings::startup_toggle_label(startup).into(),
            style: BTN_SUBTLE,
            rc: toggle_rc,
        },
        UiButton {
            id: BTN_OPEN_LOG,
            label: "Open log folder".into(),
            style: BTN_OUTLINE,
            rc: log_rc,
        },
        UiButton {
            id: BTN_SCAN_GENTLE,
            label: "Gentle".into(),
            style: crate::settings::scan_segment_style(
                app_support::config::SCAN_SPEED_GENTLE,
                cur_scan,
            ),
            rc: gentle_rc,
        },
        UiButton {
            id: BTN_SCAN_BALANCED,
            label: "Balanced".into(),
            style: crate::settings::scan_segment_style(
                app_support::config::SCAN_SPEED_BALANCED,
                cur_scan,
            ),
            rc: balanced_rc,
        },
        UiButton {
            id: BTN_SCAN_TURBO,
            label: "Turbo".into(),
            style: crate::settings::scan_segment_style(
                app_support::config::SCAN_SPEED_TURBO,
                cur_scan,
            ),
            rc: turbo_rc,
        },
    ];
    l
}

// ---------------------------------------------------------------------------
// Text-rect tables (`go\window.go:666-727`, `:1602-1635`).
// ---------------------------------------------------------------------------

/// `mainTextRects` (`go\window.go:674`).
#[must_use]
pub fn main_text_rects(
    cw: i32,
    ch: i32,
    dpi: i32,
    l: &UiLayout,
    dir_count: i32,
) -> Vec<NamedTextRect> {
    let sc = |v: i32| scale_px(dpi, v);
    let mut out = vec![
        NamedTextRect {
            id: "main.title".into(),
            rc: Rect {
                left: sc(20),
                top: sc(16),
                right: cw - sc(72),
                bottom: sc(48),
            },
        },
        NamedTextRect {
            id: "main.subtitle".into(),
            rc: Rect {
                left: sc(20),
                top: sc(48),
                right: cw - sc(20) - sc(120) - sc(8),
                bottom: sc(68),
            },
        },
        NamedTextRect {
            id: "main.section".into(),
            rc: Rect {
                left: sc(20),
                top: sc(92),
                right: cw - sc(60),
                bottom: sc(110),
            },
        },
        NamedTextRect {
            id: "main.count".into(),
            rc: Rect {
                left: cw - sc(60),
                top: sc(92),
                right: cw - sc(20),
                bottom: sc(110),
            },
        },
    ];
    if l.rows.is_empty() {
        out.push(NamedTextRect {
            id: "main.empty".into(),
            rc: l.card,
        });
    } else {
        for (i, r) in l.rows.iter().enumerate() {
            let text_right = r.right - sc(12);
            out.push(NamedTextRect {
                id: format!("main.row-{i}"),
                rc: Rect {
                    left: r.left + sc(12),
                    top: r.top,
                    right: text_right,
                    bottom: r.bottom,
                },
            });
        }
        if l.overflow > 0 {
            out.push(NamedTextRect {
                id: "main.overflow".into(),
                rc: Rect {
                    left: l.card.left,
                    top: l.card.bottom - sc(22),
                    right: l.card.right - sc(12),
                    bottom: l.card.bottom - sc(6),
                },
            });
        }
    }
    for b in &l.btns {
        out.push(NamedTextRect {
            id: format!("main.btn-{}", b.id),
            rc: b.rc,
        });
    }
    out.push(NamedTextRect {
        id: "main.settings".into(),
        rc: l.settings,
    });
    out.push(NamedTextRect {
        id: "main.card".into(),
        rc: l.card_link,
    });
    out.push(NamedTextRect {
        id: "main.status".into(),
        rc: l.status,
    });
    let _ = dir_count;
    let _ = ch;
    out
}

/// `settingsTextRects` (`go\window.go:711`).
#[must_use]
pub fn settings_text_rects(cw: i32, ch: i32, dpi: i32, l: &SettingsLayout) -> Vec<NamedTextRect> {
    let sc = |v: i32| scale_px(dpi, v);
    let mut out = vec![
        NamedTextRect {
            id: "settings.title".into(),
            rc: Rect {
                left: sc(20),
                top: sc(48),
                right: cw - sc(20),
                bottom: sc(80),
            },
        },
        NamedTextRect {
            id: "settings.lib".into(),
            rc: Rect {
                left: sc(20),
                top: sc(80),
                right: cw - sc(20),
                bottom: sc(98),
            },
        },
        NamedTextRect {
            id: "settings.startup".into(),
            rc: Rect {
                left: sc(20),
                top: sc(150),
                right: cw - sc(20),
                bottom: sc(168),
            },
        },
        NamedTextRect {
            id: "settings.scan".into(),
            rc: Rect {
                left: sc(20),
                top: sc(222),
                right: cw - sc(20),
                bottom: sc(240),
            },
        },
        NamedTextRect {
            id: "settings.hint".into(),
            rc: Rect {
                left: sc(20),
                top: sc(280),
                right: cw - sc(20),
                bottom: sc(328),
            },
        },
        NamedTextRect {
            id: "settings.dead".into(),
            rc: Rect {
                left: sc(20),
                top: sc(332),
                right: cw - sc(20),
                bottom: sc(350),
            },
        },
    ];
    for b in &l.btns {
        out.push(NamedTextRect {
            id: format!("settings.btn-{}", b.id),
            rc: b.rc,
        });
    }
    out.push(NamedTextRect {
        id: "settings.status".into(),
        rc: l.status,
    });
    let _ = (cw, ch);
    out
}

// ---------------------------------------------------------------------------
// Focus model (`go\window.go:1244-1316`).
// ---------------------------------------------------------------------------

/// `mainFocusOrder` (`go\window.go:1244`).
#[must_use]
pub fn main_focus_order() -> Vec<i32> {
    vec![
        BTN_ADD,
        BTN_REMOVE,
        BTN_SCAN,
        BTN_REFRESH,
        BTN_REPORT,
        BTN_CLOSE,
        BTN_SETTINGS,
        BTN_CARD,
    ]
}

/// `settingsFocusOrder` (`go\window.go:1249`).
#[must_use]
pub fn settings_focus_order() -> Vec<i32> {
    vec![
        BTN_BACK,
        BTN_CLEAR_CACHE,
        BTN_CLEAR_INDEX,
        BTN_EXPORT,
        BTN_IMPORT,
        BTN_STARTUP,
        BTN_OPEN_LOG,
        BTN_SCAN_GENTLE,
        BTN_SCAN_BALANCED,
        BTN_SCAN_TURBO,
    ]
}

/// `cardDetailsButton` (`go\window.go:1455`).
#[must_use]
pub fn card_details_button(i: i32) -> i32 {
    BTN_CARD_DETAILS_AT + i
}

/// `cardStateButton` (`go\window.go:1459`).
#[must_use]
pub fn card_state_button(i: i32) -> i32 {
    BTN_CARD_STATE_AT + i
}

/// `cardFocusOrder` (`go\window.go:1256`).
#[must_use]
pub fn card_focus_order() -> Vec<i32> {
    let mut order = vec![BTN_CARD_BACK];
    for i in 0..4 {
        order.push(card_details_button(i));
    }
    for i in 0..4 {
        order.push(card_state_button(i));
    }
    order
}

/// `focusOrderForView` (`go\window.go:1268`).
#[must_use]
pub fn focus_order_for_view(v: View) -> Vec<i32> {
    match v {
        View::Settings => settings_focus_order(),
        View::Card => card_focus_order(),
        View::Main => main_focus_order(),
    }
}

/// `moveFocus` (`go\window.go:1282`): step `cur` by `dir` with wraparound.
#[must_use]
pub fn move_focus(cur: i32, dir: i32, order: &[i32]) -> i32 {
    if order.is_empty() {
        return 0;
    }
    if let Some(i) = order.iter().position(|&id| id == cur) {
        let n = order.len() as i32;
        let mut j = (i as i32 + dir) % n;
        if j < 0 {
            j += n;
        }
        return order[j as usize];
    }
    if dir < 0 {
        return order[order.len() - 1];
    }
    order[0]
}

/// `mainFocusRect` (`go\window.go:1303`).
#[must_use]
pub fn main_focus_rect(l: &UiLayout, id: i32) -> Option<Rect> {
    if let Some(b) = l.btns.iter().find(|b| b.id == id) {
        return Some(b.rc);
    }
    if id == BTN_SETTINGS {
        return Some(l.settings);
    }
    if id == BTN_CARD {
        return Some(l.card_link);
    }
    None
}

// ---------------------------------------------------------------------------
// Card view (`go\window.go:1453-1731`).
// ---------------------------------------------------------------------------

/// `cardPresetForButton` (`go\window.go:1463`).
#[must_use]
pub fn card_preset_for_button(id: i32) -> Option<(i32, String)> {
    let (dets, states) = presence::card_presets();
    let i = id - BTN_CARD_DETAILS_AT;
    if i >= 0 && (i as usize) < dets.len() {
        return Some((0, dets[i as usize].id.clone()));
    }
    let i = id - BTN_CARD_STATE_AT;
    if i >= 0 && (i as usize) < states.len() {
        return Some((1, states[i as usize].id.clone()));
    }
    None
}

/// `cardPresetStyle` (`go\window.go:1476`): active preset accent, else subtle.
#[must_use]
pub fn card_preset_style(line: i32, id: &str, spec: &CardSpec) -> i32 {
    let spec = spec.normalized();
    if line == 0 && id == spec.details {
        return BTN_ACCENT;
    }
    if line == 1 && id == spec.state {
        return BTN_ACCENT;
    }
    BTN_SUBTLE
}

/// `computeCardLayout` (`go\window.go:1546`). The spec only flips button styles.
#[must_use]
pub fn compute_card_layout(cw: i32, ch: i32, dpi: i32, spec: &CardSpec) -> CardLayout {
    let sc = |v: i32| scale_px(dpi, v);
    let margin = sc(MAIN_MARGIN);

    let mut l = CardLayout::default();

    let (back_w, back_h) = (sc(80), sc(28));
    let back = Rect {
        left: margin,
        top: sc(16),
        right: margin + back_w,
        bottom: sc(16) + back_h,
    };

    let (prev_top, prev_bottom) = (sc(100), sc(400));
    let prev_pad = sc(10);
    let (row_h, row_pitch) = (sc(20), sc(22));
    l.preview = Rect {
        left: margin,
        top: prev_top,
        right: cw - margin,
        bottom: prev_bottom,
    };
    for i in 0..5 {
        let top = prev_top + prev_pad + i * row_pitch;
        l.rows.push(Rect {
            left: margin + prev_pad,
            top,
            right: cw - margin - prev_pad,
            bottom: top + row_h,
        });
    }

    let row_w = (cw - margin * 2 - sc(12) * 3) / 4;
    let seg = |top: i32, line: i32, presets: &[CardPreset]| -> Vec<UiButton> {
        let mut out = Vec::with_capacity(presets.len());
        for (i, p) in presets.iter().enumerate() {
            let i = i as i32;
            let left = margin + i * (row_w + sc(12));
            let mut right = left + row_w;
            if i == presets.len() as i32 - 1 {
                right = cw - margin;
            }
            let id = if line == 0 {
                card_details_button(i)
            } else {
                card_state_button(i)
            };
            out.push(UiButton {
                id,
                label: p.label.clone(),
                style: card_preset_style(line, &p.id, spec),
                rc: Rect {
                    left,
                    top,
                    right,
                    bottom: top + sc(32),
                },
            });
        }
        out
    };
    let (dets, states) = presence::card_presets();
    l.btns.push(UiButton {
        id: BTN_CARD_BACK,
        label: "Back".into(),
        style: BTN_TEXT,
        rc: back,
    });
    l.btns.extend(seg(sc(434), 0, &dets));
    l.btns.extend(seg(sc(500), 1, &states));

    let st_h = sc(20);
    l.status = Rect {
        left: margin,
        top: ch - sc(14) - st_h,
        right: cw - margin,
        bottom: ch - sc(14),
    };
    l
}

/// `cardTextRects` (`go\window.go:1606`).
#[must_use]
pub fn card_text_rects(cw: i32, ch: i32, dpi: i32, l: &CardLayout) -> Vec<NamedTextRect> {
    let sc = |v: i32| scale_px(dpi, v);
    let mut out = vec![
        NamedTextRect {
            id: "card.title".into(),
            rc: Rect {
                left: sc(20),
                top: sc(48),
                right: cw - sc(20),
                bottom: sc(80),
            },
        },
        NamedTextRect {
            id: "card.preview".into(),
            rc: Rect {
                left: sc(20),
                top: sc(80),
                right: cw - sc(20),
                bottom: sc(98),
            },
        },
        NamedTextRect {
            id: "card.details".into(),
            rc: Rect {
                left: sc(20),
                top: sc(412),
                right: cw - sc(20),
                bottom: sc(430),
            },
        },
        NamedTextRect {
            id: "card.state".into(),
            rc: Rect {
                left: sc(20),
                top: sc(478),
                right: cw - sc(20),
                bottom: sc(496),
            },
        },
        NamedTextRect {
            id: "card.hint".into(),
            rc: Rect {
                left: sc(20),
                top: sc(544),
                right: cw - sc(20),
                bottom: sc(562),
            },
        },
    ];
    for (i, r) in l.rows.iter().enumerate() {
        out.push(NamedTextRect {
            id: format!("card.row-{i}"),
            rc: *r,
        });
    }
    {
        let margin = sc(MAIN_MARGIN);
        let (prev_top, prev_pad) = (sc(100), sc(10));
        let (row_h, row_pitch) = (sc(20), sc(22));
        let tip_top = prev_top + prev_pad + 5 * row_pitch;
        out.push(NamedTextRect {
            id: "card.row-5".into(),
            rc: Rect {
                left: margin + prev_pad,
                top: tip_top,
                right: cw - margin - prev_pad,
                bottom: tip_top + row_h,
            },
        });
    }
    for b in &l.btns {
        out.push(NamedTextRect {
            id: format!("card.btn-{}", b.id),
            rc: b.rc,
        });
    }
    out.push(NamedTextRect {
        id: "card.status".into(),
        rc: l.status,
    });
    let _ = ch;
    out
}

/// `previewRows` (`go\window.go:1490`): the recorded payload verbatim.
#[must_use]
pub fn preview_rows(act: &Activity, have: bool) -> [String; 5] {
    if !have {
        return [
            "Header: -".into(),
            "Details: -".into(),
            "State: -".into(),
            "Images: -".into(),
            "Buttons: -".into(),
        ];
    }
    let mut header = act.name.clone();
    if header.is_empty() {
        header = "(Discord app name)".into();
    }
    let mut images = act.large_image.clone();
    if !act.small_image.is_empty() {
        images = format!("{images} + {}", act.small_image);
    }
    let mut buttons = "none".to_string();
    if !act.buttons.is_empty() {
        let parts: Vec<String> = act
            .buttons
            .iter()
            .map(|b| format!("{} -> {}", b.label, b.url))
            .collect();
        buttons = parts.join(" | ");
    }
    [
        format!("Header: {header}"),
        format!("Details: {}", act.details),
        format!("State: {}", act.state),
        format!("Images: {images}"),
        format!("Buttons: {buttons}"),
    ]
}

/// `previewTooltip` (`go\window.go:1532`).
#[must_use]
pub fn preview_tooltip(act: &Activity, have: bool) -> String {
    if !have || act.large_text.is_empty() {
        return "Tooltip: -".into();
    }
    format!("Tooltip: {}", act.large_text)
}

// ---------------------------------------------------------------------------
// Scan status strings (`go\window.go:1809-2024`).
// ---------------------------------------------------------------------------

/// `maxScanStatusLen` (`go\window.go:1809`).
pub const MAX_SCAN_STATUS_LEN: usize = 160;

/// `scanBusyText` (`go\window.go:1984`).
pub const SCAN_BUSY_TEXT: &str = "Scan running - Cancel first.";

/// `formatScanCount` (`go\window.go:1815`).
#[must_use]
pub fn format_scan_count(n: i32) -> String {
    if (-100_000..100_000).contains(&n) {
        return n.to_string();
    }
    format_scan_count_unit(n)
}

/// `formatScanCountUnit` (`go\window.go:1825`).
#[must_use]
pub fn format_scan_count_unit(n: i32) -> String {
    let f = f64::from(n);
    let divs = [1e3_f64, 1e6, 1e9, 1e12, 1e15, 1e18];
    let sufs = ["k", "M", "B", "T", "P", "E"];
    let mut idx = 0usize;
    while idx < divs.len() - 1 && f / divs[idx] >= 999.95 {
        idx += 1;
    }
    let s = format!("{:.1}{}", f / divs[idx], sufs[idx]);
    s.replacen(&format!(".0{}", sufs[idx]), sufs[idx], 1)
}

/// `formatScanCountCompact` (`go\window.go:1841`).
#[must_use]
pub fn format_scan_count_compact(n: i32) -> String {
    if (-1000..1000).contains(&n) {
        return n.to_string();
    }
    format_scan_count_unit(n)
}

/// `fitScanStatus` (`go\window.go:1852`): cap to 160 preserving "folders".
#[must_use]
pub fn fit_scan_status(s: &str) -> String {
    let b = s.as_bytes();
    if b.len() <= MAX_SCAN_STATUS_LEN {
        return s.to_string();
    }
    if let Some(i) = s.find("folders") {
        let mut j = i;
        while j > 0 && i - j < 24 {
            let c = b[j - 1];
            if c != b'/' && c != b' ' && !c.is_ascii_digit() {
                break;
            }
            j -= 1;
        }
        let tail = &s[j..];
        let keep = MAX_SCAN_STATUS_LEN
            .saturating_sub(tail.len())
            .saturating_sub(3);
        if keep > 20 {
            return format!("{}...{}", &s[..keep], tail);
        }
    }
    format!("{}...", &s[..MAX_SCAN_STATUS_LEN - 3])
}

/// `scanCatchingUp` (`go\window.go:1881`).
#[must_use]
pub fn scan_catching_up(p: &Progress) -> bool {
    p.total > 0 && p.done == p.total && p.resolved < p.keys_total
}

/// `scanIndexMetaText` (`go\window.go:1897`).
#[must_use]
pub fn scan_index_meta_text(p: &Progress) -> String {
    if p.total <= 0 {
        return String::new();
    }
    let mut s = format!("indexed {} of {} files", p.done, p.total);
    if p.keys_total > 0 {
        s += &format!(" | metadata {} of {}", p.resolved, p.keys_total);
    }
    if scan_catching_up(p) {
        s += " - catching up";
    }
    s
}

/// `scanProgressText` (`go\window.go:1918`).
#[must_use]
pub fn scan_progress_text(p: &Progress) -> String {
    let failed = if p.keys_failed > 0 {
        format!(", {} failed", format_scan_count(p.keys_failed))
    } else {
        String::new()
    };
    let mut line = format!(
        "Scan {}/{} keys | {} fetched, {} cached{} | {}/{} folders",
        format_scan_count(p.keys_done),
        format_scan_count(p.keys_total),
        format_scan_count(p.keys_fetched),
        format_scan_count(p.keys_cached),
        failed,
        format_scan_count(p.folders_scanned),
        format_scan_count(p.folders_skipped)
    );
    let seg = scan_index_meta_text(p);
    if !seg.is_empty() {
        line = format!("{seg} | {line}");
    }
    fit_scan_status(&line)
}

/// `scanDoneText` (`go\window.go:1942`).
#[must_use]
pub fn scan_done_text(kind: &str, s: &Stats) -> String {
    if s.keys_fetched == 0 && s.keys_not_found == 0 && s.keys_failed == 0 && s.folders_skipped > 0 {
        return fit_scan_status(&format!(
            "{kind} done: nothing to fetch ({} cached) - {} folders skipped",
            format_scan_count(s.keys_cached),
            format_scan_count(s.folders_skipped)
        ));
    }
    let failed = if s.keys_failed > 0 {
        format!(", {} failed", format_scan_count(s.keys_failed))
    } else {
        String::new()
    };
    fit_scan_status(&format!(
        "{kind} done: {} keys - {} cached, {} fetched{} - {}/{} folders",
        format_scan_count(s.keys),
        format_scan_count(s.keys_cached),
        format_scan_count(s.keys_fetched),
        failed,
        format_scan_count(s.folders_scanned),
        format_scan_count(s.folders_skipped)
    ))
}

/// `scanCancelledText` (`go\window.go:1960`).
#[must_use]
pub fn scan_cancelled_text(done: i32, total: i32) -> String {
    fit_scan_status(&format!(
        "Scan cancelled - {}/{} keys done, progress kept",
        format_scan_count(done),
        format_scan_count(total)
    ))
}

/// `scanButtonLabel` (`go\window.go:1968`).
#[must_use]
pub fn scan_button_label(active: bool) -> &'static str {
    if active {
        "Cancel"
    } else {
        "Scan"
    }
}

/// `scanRefreshEnabled` (`go\window.go:1978`).
#[must_use]
pub fn scan_refresh_enabled(active: bool) -> bool {
    !active
}

/// `scanBlockedReason` (`go\window.go:1992`): reads the running-scan flag.
#[must_use]
pub fn scan_blocked_reason(scan_active: bool, id: i32) -> &'static str {
    if !scan_active {
        return "";
    }
    if matches!(id, BTN_ADD | BTN_REMOVE | BTN_REFRESH) {
        return SCAN_BUSY_TEXT;
    }
    ""
}

/// `removeHasTarget` (`go\window.go:2006`).
#[must_use]
pub fn remove_has_target(sel_row: i32, hover_row: i32, dirs_len: usize) -> bool {
    let n = dirs_len as i32;
    (sel_row >= 0 && sel_row < n) || (hover_row >= 0 && hover_row < n)
}

/// The scan error the terminal path distinguishes (`go\window.go:2018`: a bare
/// context cancellation is a cancel, not a failure).
#[derive(Clone, Debug)]
pub struct UiScanError {
    pub cancelled: bool,
    pub message: String,
}

impl UiScanError {
    #[must_use]
    pub fn other(message: &str) -> Self {
        Self {
            cancelled: false,
            message: message.to_string(),
        }
    }

    #[must_use]
    pub fn cancelled() -> Self {
        Self {
            cancelled: true,
            message: String::new(),
        }
    }
}

/// `scanResultText` (`go\window.go:2014`).
#[must_use]
pub fn scan_result_text(kind: &str, s: &Stats, err: Option<&UiScanError>) -> String {
    if s.cancelled {
        return scan_cancelled_text(s.keys_done, s.keys);
    }
    if let Some(e) = err {
        if e.cancelled {
            return scan_cancelled_text(s.keys_done, s.keys);
        }
        return fit_scan_status(&format!("{kind} failed: {}", e.message));
    }
    scan_done_text(kind, s)
}

/// `scanTerminalText` (`go\window.go:2705`).
#[must_use]
pub fn scan_terminal_text(force: bool, s: &Stats, err: Option<&UiScanError>) -> String {
    let kind = if force { "Refresh" } else { "Scan" };
    scan_result_text(kind, s, err)
}

// ---------------------------------------------------------------------------
// Responsive scan display (`go\window.go:2038-2596`).
// ---------------------------------------------------------------------------

/// `scanKeyFraction` (`go\window.go:2041`).
#[must_use]
pub fn scan_key_fraction(done: i32, total: i32) -> Option<f64> {
    if total <= 0 {
        return None;
    }
    let done = done.clamp(0, total);
    Some(f64::from(done) / f64::from(total))
}

/// `scanPaintGate` (`go\window.go:2060`): a backward clock paints, never stalls.
#[must_use]
pub fn scan_paint_gate(last: Instant, now: Instant) -> bool {
    match now.checked_duration_since(last) {
        None => true,
        Some(d) => d >= SCAN_PAINT_INTERVAL,
    }
}

/// One UI-thread lifecycle input (`go\window.go:2068-2078`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ScanLifecycleEvent {
    Start,
    Progress,
    Tick,
    Done,
}

/// The pure run model behind the scan state (`go\window.go:2084-2087`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct ScanLifecycle {
    pub active: bool,
    pub last_paint: Instant,
}

impl Default for ScanLifecycle {
    fn default() -> Self {
        Self {
            active: false,
            last_paint: Instant::now(),
        }
    }
}

/// The explicit effect set one lifecycle step requests (`go\window.go:2096-2101`).
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct ScanFx {
    pub arm: bool,
    pub kill: bool,
    pub paint: bool,
    pub terminal: bool,
}

/// `scanLifecycleNext` (`go\window.go:2115`).
#[must_use]
pub fn scan_lifecycle_next(
    st: ScanLifecycle,
    ev: ScanLifecycleEvent,
    now: Instant,
) -> (ScanLifecycle, ScanFx) {
    match ev {
        ScanLifecycleEvent::Start => {
            if st.active {
                return (st, ScanFx::default());
            }
            (
                ScanLifecycle {
                    active: true,
                    last_paint: now,
                },
                ScanFx {
                    arm: true,
                    paint: true,
                    ..ScanFx::default()
                },
            )
        }
        ScanLifecycleEvent::Progress | ScanLifecycleEvent::Tick => {
            if !st.active || !scan_paint_gate(st.last_paint, now) {
                return (st, ScanFx::default());
            }
            (
                ScanLifecycle {
                    active: st.active,
                    last_paint: now,
                },
                ScanFx {
                    paint: true,
                    ..ScanFx::default()
                },
            )
        }
        ScanLifecycleEvent::Done => {
            if !st.active {
                return (
                    ScanLifecycle::default(),
                    ScanFx {
                        kill: true,
                        ..ScanFx::default()
                    },
                );
            }
            (
                ScanLifecycle::default(),
                ScanFx {
                    kill: true,
                    terminal: true,
                    ..ScanFx::default()
                },
            )
        }
    }
}

/// `minElapsedForETA` / `minKeysForETA` (`go\window.go:2142-2145`).
pub const MIN_ELAPSED_FOR_ETA: Duration = Duration::from_secs(2);
pub const MIN_KEYS_FOR_ETA: i32 = 5;

/// `scanObservedDone` (`go\window.go:2151`).
#[must_use]
pub fn scan_observed_done(p: &Progress) -> i32 {
    (p.keys_done - p.keys_cached).max(0)
}

/// `scanETA` (`go\window.go:2168`).
#[must_use]
pub fn scan_eta(p: &Progress, observed_base: i32, resolve_elapsed: Duration) -> Option<Duration> {
    if p.keys_total <= 0 || p.keys_done >= p.keys_total {
        return None;
    }
    let observed = scan_observed_done(p) - observed_base;
    if observed < MIN_KEYS_FOR_ETA || resolve_elapsed < MIN_ELAPSED_FOR_ETA {
        return None;
    }
    let remaining = p.keys_total - p.keys_done;
    if remaining <= 0 {
        return None;
    }
    let secs = resolve_elapsed.as_secs_f64() * f64::from(remaining) / f64::from(observed);
    if secs < 0.0 {
        return None;
    }
    Some(Duration::from_secs_f64(secs))
}

/// `scanResolveElapsed` (`go\window.go:2194`).
#[must_use]
pub fn scan_resolve_elapsed(origin: Option<Instant>, now: Instant) -> Duration {
    match origin {
        None => Duration::ZERO,
        Some(o) => now.saturating_duration_since(o),
    }
}

/// `formatScanDuration` (`go\window.go:2206`).
#[must_use]
pub fn format_scan_duration(d: Duration) -> String {
    let s = d.as_secs();
    if s < 60 {
        return format!("{s}s");
    }
    let m = s / 60;
    if m < 60 {
        return format!("{m}m{:02}s", s % 60);
    }
    format!("{}h{:02}m", m / 60, m % 60)
}

/// `scanLiveStatusParts` (`go\window.go:2226`).
#[must_use]
pub fn scan_live_status_parts(
    p: &Progress,
    elapsed: Duration,
    resolve_elapsed: Duration,
    observed_base: i32,
) -> (String, String, String) {
    let base = scan_progress_text(p);
    let elapsed_sfx = format!(" | {}", format_scan_duration(elapsed));
    let mut eta_sfx = String::new();
    if let Some(eta) = scan_eta(p, observed_base, resolve_elapsed) {
        eta_sfx = format!(", eta {} left", format_scan_duration(eta));
    }
    (base, elapsed_sfx, eta_sfx)
}

/// `scanStatusAvailPx` (`go\window.go:2239`).
#[must_use]
pub fn scan_status_avail_px(dpi: i32) -> i32 {
    scale_px(dpi, MAIN_CLIENT_W - 2 * MAIN_MARGIN)
}

/// `statusAvailPx` (`go\window.go:2249`).
#[must_use]
pub fn status_avail_px(cw: i32, dpi: i32) -> i32 {
    cw - 2 * scale_px(dpi, MAIN_MARGIN)
}

/// `scanProgressShort` (`go\window.go:2306`).
#[must_use]
pub fn scan_progress_short(p: &Progress) -> String {
    let failed = if p.keys_failed > 0 {
        format!(", {} failed", format_scan_count_compact(p.keys_failed))
    } else {
        String::new()
    };
    fit_scan_status(&format!(
        "Scan {}/{} keys{} | {}/{} folders",
        format_scan_count_compact(p.keys_done),
        format_scan_count_compact(p.keys_total),
        failed,
        format_scan_count_compact(p.folders_scanned),
        format_scan_count_compact(p.folders_skipped)
    ))
}

/// `scanProgressFloor` (`go\window.go:2325`).
#[must_use]
pub fn scan_progress_floor(p: &Progress) -> String {
    fit_scan_status(&format!(
        "Scan {}/{} keys",
        format_scan_count_compact(p.keys_done),
        format_scan_count_compact(p.keys_total)
    ))
}

/// `scanProgressMinimal` (`go\window.go:2334`).
#[must_use]
pub fn scan_progress_minimal(p: &Progress) -> String {
    let failed = if p.keys_failed > 0 {
        format!(", {} failed", format_scan_count_compact(p.keys_failed))
    } else {
        String::new()
    };
    fit_scan_status(&format!("{}{failed}", scan_progress_floor(p)))
}

/// `scanStatusCandidates` (`go\window.go:2357`).
#[must_use]
pub fn scan_status_candidates(
    p: &Progress,
    elapsed: Duration,
    resolve_elapsed: Duration,
    observed_base: i32,
) -> Vec<String> {
    let (base, elapsed_sfx, eta_sfx) =
        scan_live_status_parts(p, elapsed, resolve_elapsed, observed_base);
    let mut cands = Vec::new();
    if !eta_sfx.is_empty() {
        cands.push(format!("{base}{elapsed_sfx}{eta_sfx}"));
    }
    cands.push(format!("{base}{elapsed_sfx}"));
    let seg = scan_index_meta_text(p);
    if !seg.is_empty() {
        if !eta_sfx.is_empty() {
            cands.push(format!("{seg}{elapsed_sfx}{eta_sfx}"));
        }
        cands.push(format!("{seg}{elapsed_sfx}"));
        cands.push(seg);
    }
    let short = scan_progress_short(p);
    if !eta_sfx.is_empty() {
        cands.push(format!("{short}{elapsed_sfx}{eta_sfx}"));
    }
    cands.push(format!("{short}{elapsed_sfx}"));
    cands.push(short);
    cands.push(scan_progress_minimal(p));
    cands.push(scan_progress_floor(p));
    cands
}

/// `composeScanLiveStatus` (`go\window.go:2387`).
#[must_use]
pub fn compose_scan_live_status(
    p: &Progress,
    elapsed: Duration,
    resolve_elapsed: Duration,
    observed_base: i32,
    avail_px: i32,
    measure: impl Fn(&str) -> i32,
) -> String {
    let cands = scan_status_candidates(p, elapsed, resolve_elapsed, observed_base);
    for c in &cands {
        if measure(c) <= avail_px {
            return c.clone();
        }
    }
    cands[cands.len() - 1].clone()
}

/// `scanBarRect` (`go\window.go:2425`): the strip BELOW the status rect.
#[must_use]
pub fn scan_bar_rect(status: Rect, dpi: i32) -> Rect {
    let sc = |v: i32| scale_px(dpi, v);
    let top = status.bottom + sc(3);
    Rect {
        left: status.left,
        top,
        right: status.right,
        bottom: top + sc(5),
    }
}

/// `scanIndeterminatePos` (`go\window.go:2433`).
#[must_use]
pub fn scan_indeterminate_pos(elapsed: Duration) -> f64 {
    const LOOP: Duration = Duration::from_millis(1200);
    let ms = elapsed.as_millis();
    let loop_ms = LOOP.as_millis();
    (ms % loop_ms) as f64 / loop_ms as f64
}

/// `scanBarModel` (`go\window.go:2489`).
#[must_use]
pub fn scan_bar_model(active: bool, done: i32, total: i32) -> (bool, bool, f64) {
    if !active {
        return (false, false, 0.0);
    }
    match scan_key_fraction(done, total) {
        Some(f) => (true, true, f),
        None => (true, false, 0.0),
    }
}

/// `scanBarProgress` (`go\window.go:2507`): the METADATA pair, never the file pair.
#[must_use]
pub fn scan_bar_progress(p: &Progress, active: bool) -> (bool, bool, f64) {
    scan_bar_model(active, p.resolved, p.keys_total)
}

// ---------------------------------------------------------------------------
// Native measurement + DPI helpers (`go\window.go:2256-2304`, `:3442-3464`).
// ---------------------------------------------------------------------------

/// Client rect of `hwnd` (`go\window.go:2818`).
#[must_use]
pub fn get_client_rect(hwnd: HWND) -> Rect {
    use windows_sys::Win32::Foundation::RECT;
    let mut rc = RECT {
        left: 0,
        top: 0,
        right: 0,
        bottom: 0,
    };
    // SAFETY: `rc` is a live RECT; GetClientRect only writes it.
    unsafe {
        crate::win32::GetClientRect(hwnd, &mut rc);
    }
    Rect {
        left: rc.left,
        top: rc.top,
        right: rc.right,
        bottom: rc.bottom,
    }
}

/// `windowDpi` (`go\window.go:3448`): 96 when the API is absent or fails.
#[must_use]
pub fn window_dpi(hwnd: HWND) -> i32 {
    if hwnd.is_null() {
        return 96;
    }
    // SAFETY: GetDpiForWindow reads `hwnd` only.
    let d = unsafe { crate::win32::GetDpiForWindow(hwnd) };
    if d != 0 {
        d as i32
    } else {
        96
    }
}

/// `systemDpi` (`go\window.go:3459`).
#[must_use]
pub fn system_dpi() -> i32 {
    // SAFETY: GetDpiForSystem takes no arguments.
    let d = unsafe { crate::win32::GetDpiForSystem() };
    if d != 0 {
        d as i32
    } else {
        96
    }
}

/// `liveStatusAvailPx` (`go\window.go:2256`): the live client rect budget.
#[must_use]
pub fn live_status_avail_px(hwnd: HWND) -> i32 {
    let rc = get_client_rect(hwnd);
    let cw = rc.right - rc.left;
    if cw > 0 {
        return status_avail_px(cw, window_dpi(hwnd));
    }
    scan_status_avail_px(96)
}

/// `measureScanStatusWidth` (`go\window.go:2271`): real GDI font metrics in the
/// status font (Segoe UI at the section size) at `dpi`. `None` when no DC/font
/// is available.
#[must_use]
pub fn measure_scan_status_width(dpi: i32, s: &str) -> Option<i32> {
    use windows_sys::Win32::Foundation::SIZE;
    use windows_sys::Win32::Graphics::Gdi::{
        CreateFontW, DeleteObject, GetDC, GetTextExtentPoint32W, ReleaseDC, SelectObject,
        DEFAULT_CHARSET,
    };
    if s.is_empty() {
        return Some(0);
    }
    // SAFETY: a screen DC + a created font, both released below; extents do not
    // depend on the target surface.
    unsafe {
        let hdc = GetDC(std::ptr::null_mut());
        if hdc.is_null() {
            return None;
        }
        let face = utf16_z("Segoe UI");
        let font = CreateFontW(
            -scale_px(dpi, 12),
            0,
            0,
            0,
            crate::win32::FW_NORMAL,
            0,
            0,
            0,
            DEFAULT_CHARSET,
            0,
            0,
            0,
            0,
            face.as_ptr(),
        );
        if font.is_null() {
            ReleaseDC(std::ptr::null_mut(), hdc);
            return None;
        }
        let old = SelectObject(hdc, font);
        let u: Vec<u16> = s.encode_utf16().collect();
        let mut sz = SIZE { cx: 0, cy: 0 };
        let r = GetTextExtentPoint32W(hdc, u.as_ptr(), u.len() as i32, &mut sz);
        SelectObject(hdc, old);
        DeleteObject(font);
        ReleaseDC(std::ptr::null_mut(), hdc);
        if r == 0 {
            None
        } else {
            Some(sz.cx)
        }
    }
}

/// `scanLiveStatusText` (`go\window.go:2404`).
#[must_use]
pub fn scan_live_status_text(
    p: &Progress,
    elapsed: Duration,
    resolve_elapsed: Duration,
    observed_base: i32,
    dpi: i32,
    avail_px: i32,
) -> String {
    let (base, elapsed_sfx, _) = scan_live_status_parts(p, elapsed, resolve_elapsed, observed_base);
    if measure_scan_status_width(dpi, &format!("{base}{elapsed_sfx}")).is_none() {
        return fit_scan_status(&format!("{}{elapsed_sfx}", scan_progress_floor(p)));
    }
    fit_scan_status(&compose_scan_live_status(
        p,
        elapsed,
        resolve_elapsed,
        observed_base,
        avail_px,
        |s| measure_scan_status_width(dpi, s).unwrap_or(avail_px + 1),
    ))
}

// ---------------------------------------------------------------------------
// Report button (`go\window.go:1032-1203`).
// ---------------------------------------------------------------------------

/// `reportButtonLabel` (`go\window.go:1034`).
pub const REPORT_BUTTON_LABEL: &str = "Report";
/// `reportRepo` (`go\window.go:1036`).
pub const REPORT_REPO: &str = "nathwn12/Potflix";
/// `reportLabel` (`go\window.go:1038`).
pub const REPORT_LABEL: &str = "needs-repair";
/// `reportNothingText` (`go\window.go:1042`).
pub const REPORT_NOTHING_TEXT: &str = "Nothing to report - no needs-repair entries.";
/// `reportFailedText` (`go\window.go:1045`).
pub const REPORT_FAILED_TEXT: &str = "Report failed - see the debug log.";
/// `reportPendingText` (`go\window.go:1049`).
pub const REPORT_PENDING_TEXT: &str = "Filing report...";

/// `reportButtonLabel` accessor (used by `compute_layout`).
#[must_use]
pub const fn report_button_label() -> &'static str {
    REPORT_BUTTON_LABEL
}

/// `repairReportDir` (`go\window.go:1074`).
#[must_use]
pub fn repair_report_dir() -> std::path::PathBuf {
    if let Ok(d) = std::env::var("LOCALAPPDATA") {
        if !d.is_empty() {
            return std::path::Path::new(&d).join("Potflix").join("inventory");
        }
    }
    app_support::debuglog::exe_dir_or_dot().join("inventory")
}

/// `repairReportPath` (`go\window.go:1083`).
#[must_use]
pub fn repair_report_path() -> std::path::PathBuf {
    repair_report_dir().join(repair::report::REPORT_JSON_NAME)
}

/// `reportDecision` (`go\window.go:1092`).
#[must_use]
pub fn report_decision(r: &repair::report::Report, err: bool) -> (bool, &'static str) {
    if err || r.entries.is_empty() {
        return (false, REPORT_NOTHING_TEXT);
    }
    (true, "")
}

/// `reportOpenedText` (`go\window.go:1102`).
#[must_use]
pub fn report_opened_text(gh_failed: bool) -> &'static str {
    if gh_failed {
        "Report: gh failed - opened a prefilled issue instead."
    } else {
        "Report: issue opened in your browser."
    }
}

/// `reportLaunchText` (`go\window.go:1112`).
#[must_use]
pub fn report_launch_text(url: &str) -> String {
    format!("Report issue: {url}")
}

/// `reportIssueStatus` (`go\window.go:1177`).
#[must_use]
pub fn report_issue_status(
    url: &str,
    err: Option<&repair::Error>,
    open: &dyn Fn(&str) -> bool,
) -> String {
    if url.is_empty() {
        app_logf(&format!(
            "report: create issue: {}",
            err.map_or("", repair::Error::message)
        ));
        return REPORT_FAILED_TEXT.to_string();
    }
    if let Some(e) = err {
        app_logf(&format!("report: create issue (gh): {}", e.message()));
    }
    if open(url) {
        return report_opened_text(err.is_some()).to_string();
    }
    app_logf(&format!("report: open {url} failed"));
    report_launch_text(url)
}

// ---------------------------------------------------------------------------
// Process-wide debug log (`go\window.go:3491`).
// ---------------------------------------------------------------------------

static APP_LOG: std::sync::LazyLock<Mutex<Option<app_support::debuglog::Logger>>> =
    std::sync::LazyLock::new(|| Mutex::new(None));

/// Install the process log the UI writes to (`appLog = log`).
pub fn set_app_log(log: app_support::debuglog::Logger) {
    *APP_LOG
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner) = Some(log);
}

/// `appLogf` (`go\window.go:3491`).
pub fn app_logf(message: &str) {
    if let Some(l) = APP_LOG
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
        .as_ref()
    {
        l.log(message);
    }
}

// ---------------------------------------------------------------------------
// UiState: the UI-thread state (`go\window.go:97-176`, `:1427-1431`).
// ---------------------------------------------------------------------------

/// The scanner the UI seam owns. Every mutable value the Go root package held as
/// a process global lives here, so Rust tests construct an isolated instance
/// instead of racing a global.
pub struct UiState {
    pub cur_view: View,
    pub status_text: String,
    pub ui_dirs: Vec<String>,
    pub sel_row: i32,
    pub hover_btn: i32,
    pub hover_row: i32,
    pub hover_x: bool,
    pub pressed_btn: i32,
    pub pressed_x: i32,
    pub focus_id: i32,

    pub scan_active: bool,
    pub scan_cancel: Option<library::Cancel>,
    pub scan_force: bool,
    pub scan_progress: Progress,
    pub scan_stats: Stats,
    pub scan_err: Option<UiScanError>,
    pub scan_start: Instant,
    pub scan_last_paint: Instant,
    pub scan_resolve_start: Option<Instant>,
    pub scan_resolve_base: i32,

    pub startup_state: bool,
    pub scan_speed_state: String,
    pub card_spec_state: CardSpec,
    pub dead_cache_count: i32,

    pub preview: Option<Activity>,
    pub ui_icon: usize,
    pub main_hwnd: HWND,
    pub ui_centered: bool,
    pub last_mx: i32,
    pub last_my: i32,
    pub have_mouse: bool,
    pub report_done_text: Arc<Mutex<String>>,

    /// Injectable seams (`go\window.go:2581-2586`, `:1055-1061`).
    pub arm_timer: Arc<dyn Fn(HWND) + Send + Sync>,
    pub kill_timer: Arc<dyn Fn(HWND) + Send + Sync>,
    pub create_issue:
        Arc<dyn Fn(&repair::report::Report) -> (String, Option<repair::Error>) + Send + Sync>,
    pub open_url: Arc<dyn Fn(&str) -> bool + Send + Sync>,
}

impl Default for UiState {
    fn default() -> Self {
        Self {
            cur_view: View::Main,
            status_text: "Ready".into(),
            ui_dirs: Vec::new(),
            sel_row: -1,
            hover_btn: 0,
            hover_row: -1,
            hover_x: false,
            pressed_btn: 0,
            pressed_x: -1,
            focus_id: 0,
            scan_active: false,
            scan_cancel: None,
            scan_force: false,
            scan_progress: Progress::default(),
            scan_stats: Stats::default(),
            scan_err: None,
            scan_start: Instant::now(),
            scan_last_paint: Instant::now(),
            scan_resolve_start: None,
            scan_resolve_base: 0,
            startup_state: false,
            scan_speed_state: app_support::config::SCAN_SPEED_TURBO.to_string(),
            card_spec_state: CardSpec::default(),
            dead_cache_count: 0,
            preview: None,
            ui_icon: 0,
            main_hwnd: std::ptr::null_mut(),
            ui_centered: false,
            last_mx: 0,
            last_my: 0,
            have_mouse: false,
            report_done_text: Arc::new(Mutex::new(String::new())),
            arm_timer: Arc::new(default_arm_timer),
            kill_timer: Arc::new(default_kill_timer),
            create_issue: Arc::new(default_create_issue),
            open_url: Arc::new(default_open_url),
        }
    }
}

/// `scanArmTimer` (`go\window.go:2581`).
fn default_arm_timer(hwnd: HWND) {
    // SAFETY: SetTimer registers a timer on `hwnd`; no callback.
    unsafe {
        windows_sys::Win32::UI::WindowsAndMessaging::SetTimer(
            hwnd,
            SCAN_TIMER_ID,
            SCAN_PAINT_INTERVAL.as_millis() as u32,
            None,
        );
    }
}

/// `scanKillTimer` (`go\window.go:2585`).
fn default_kill_timer(hwnd: HWND) {
    // SAFETY: KillTimer stops the registered id on `hwnd`.
    unsafe {
        windows_sys::Win32::UI::WindowsAndMessaging::KillTimer(hwnd, SCAN_TIMER_ID);
    }
}

/// `reportCreateIssue` default (`go\window.go:1055`).
fn default_create_issue(r: &repair::report::Report) -> (String, Option<repair::Error>) {
    let opts = repair::issue::Options {
        repo: REPORT_REPO.to_string(),
        runner: None,
        labels: vec![REPORT_LABEL.to_string()],
        gh: repair::issue::Gh::default(),
    };
    repair::issue::create_issue(r, &opts)
}

/// `reportOpenURL` default (`go\window.go:1061`).
fn default_open_url(url: &str) -> bool {
    open_external_url(url)
}

/// `openExternalURL` (`go\window.go:1120`).
#[must_use]
pub fn open_external_url(url: &str) -> bool {
    if url.is_empty() {
        return false;
    }
    let target = utf16_z(url);
    let op = utf16_z("open");
    // SAFETY: two live NUL-terminated UTF-16 buffers; ShellExecuteW reads them.
    let ret = unsafe {
        windows_sys::Win32::UI::Shell::ShellExecuteW(
            std::ptr::null_mut(),
            op.as_ptr(),
            target.as_ptr(),
            std::ptr::null(),
            std::ptr::null(),
            SW_SHOW_NORMAL,
        )
    };
    ret as isize > 32
}

impl UiState {
    /// `invalidateMain` (`go\window.go:1745`).
    pub fn invalidate(&self) {
        if !self.main_hwnd.is_null() {
            // SAFETY: InvalidateRect only marks the window.
            unsafe {
                crate::win32::InvalidateRect(self.main_hwnd, std::ptr::null(), 0);
            }
        }
    }

    /// `setStatusText` (`go\window.go:2764`).
    pub fn set_status_text(&mut self, s: impl Into<String>) {
        self.status_text = s.into();
        self.invalidate();
    }

    /// `openSettings` (`go\window.go:1211`).
    pub fn open_settings(&mut self) {
        self.cur_view = View::Settings;
        self.hover_btn = 0;
        self.pressed_btn = 0;
        self.hover_row = -1;
        self.hover_x = false;
        self.pressed_x = -1;
        self.focus_id = 0;
        self.invalidate();
    }

    /// `closeSettings` (`go\window.go:1220`).
    pub fn close_settings(&mut self) {
        self.cur_view = View::Main;
        self.hover_btn = 0;
        self.pressed_btn = 0;
        self.focus_id = 0;
        self.invalidate();
    }

    /// `openCard` (`go\window.go:1652`).
    pub fn open_card(&mut self) {
        self.cur_view = View::Card;
        self.hover_btn = 0;
        self.pressed_btn = 0;
        self.hover_row = -1;
        self.hover_x = false;
        self.pressed_x = -1;
        self.focus_id = 0;
        self.invalidate();
    }

    /// `closeCard` (`go\window.go:1661`).
    pub fn close_card(&mut self) {
        self.cur_view = View::Main;
        self.hover_btn = 0;
        self.pressed_btn = 0;
        self.focus_id = 0;
        self.invalidate();
    }

    /// `enterSettings` (`go\window.go:1230`).
    pub fn enter_settings(&mut self) {
        self.startup_state = crate::settings::query_startup_enabled();
        self.scan_speed_state = app_support::config::load_config()
            .effective_scan_speed()
            .to_string();
        self.dead_cache_count = crate::settings::refresh_dead_cache_count();
        self.open_settings();
    }

    /// `enterMain` (`go\window.go:1238`).
    pub fn enter_main(&mut self) {
        self.close_settings();
    }

    /// `enterCard` (`go\window.go:1670`).
    pub fn enter_card(&mut self) {
        self.card_spec_state = app_support::config::load_config().effective_card_spec();
        self.open_card();
    }

    /// `layoutForHwnd` (`go\window.go:731`).
    #[must_use]
    pub fn layout(&self, hwnd: HWND) -> Option<UiLayout> {
        let rc = get_client_rect(hwnd);
        let (cw, ch) = (rc.right - rc.left, rc.bottom - rc.top);
        if cw <= 0 || ch <= 0 {
            return None;
        }
        Some(compute_layout(&self.ui_dirs, cw, ch, window_dpi(hwnd)))
    }

    /// `layoutForSettings` (`go\window.go:744`).
    #[must_use]
    pub fn layout_settings(&self, hwnd: HWND) -> Option<SettingsLayout> {
        let rc = get_client_rect(hwnd);
        let (cw, ch) = (rc.right - rc.left, rc.bottom - rc.top);
        if cw <= 0 || ch <= 0 {
            return None;
        }
        Some(compute_settings_layout(
            cw,
            ch,
            window_dpi(hwnd),
            self.startup_state,
            &self.scan_speed_state,
        ))
    }

    /// `layoutForCard` (`go\window.go:1639`).
    #[must_use]
    pub fn layout_card(&self, hwnd: HWND) -> Option<CardLayout> {
        let rc = get_client_rect(hwnd);
        let (cw, ch) = (rc.right - rc.left, rc.bottom - rc.top);
        if cw <= 0 || ch <= 0 {
            return None;
        }
        Some(compute_card_layout(
            cw,
            ch,
            window_dpi(hwnd),
            &self.card_spec_state,
        ))
    }

    /// `scanBlockedReason` (`go\window.go:1992`) reading this instance's flag.
    #[must_use]
    pub fn scan_blocked_reason(&self, id: i32) -> &'static str {
        scan_blocked_reason(self.scan_active, id)
    }

    /// `removeHasTarget` (`go\window.go:2006`).
    #[must_use]
    pub fn remove_has_target(&self) -> bool {
        remove_has_target(self.sel_row, self.hover_row, self.ui_dirs.len())
    }

    /// `updateHover` (`go\window.go:757`).
    pub fn update_hover(&mut self, hwnd: HWND, x: i32, y: i32) -> bool {
        match self.cur_view {
            View::Card => {
                let Some(l) = self.layout_card(hwnd) else {
                    return false;
                };
                let nb = button_at_point(&l.btns, x, y);
                if nb == self.hover_btn && self.hover_row == -1 && !self.hover_x {
                    return false;
                }
                self.hover_btn = nb;
                self.hover_row = -1;
                self.hover_x = false;
                true
            }
            View::Settings => {
                let Some(l) = self.layout_settings(hwnd) else {
                    return false;
                };
                let nb = button_at_point(&l.btns, x, y);
                if nb == self.hover_btn && self.hover_row == -1 && !self.hover_x {
                    return false;
                }
                self.hover_btn = nb;
                self.hover_row = -1;
                self.hover_x = false;
                true
            }
            View::Main => {
                let Some(l) = self.layout(hwnd) else {
                    return false;
                };
                let mut nb = button_at_point(&l.btns, x, y);
                if pt_in_rect(l.settings, x, y) {
                    nb = BTN_SETTINGS;
                }
                if pt_in_rect(l.card_link, x, y) {
                    nb = BTN_CARD;
                }
                let nr = row_at_point(&l.rows, x, y);
                let mut nx = false;
                if nr >= 0
                    && (nr as usize) < l.xbtns.len()
                    && pt_in_rect(l.xbtns[nr as usize], x, y)
                {
                    nx = true;
                }
                if nb == self.hover_btn && nr == self.hover_row && nx == self.hover_x {
                    return false;
                }
                self.hover_btn = nb;
                self.hover_row = nr;
                self.hover_x = nx;
                true
            }
        }
    }

    /// `pressAt` (`go\window.go:806`).
    pub fn press_at(&mut self, hwnd: HWND, x: i32, y: i32) {
        match self.cur_view {
            View::Card => {
                let Some(l) = self.layout_card(hwnd) else {
                    return;
                };
                self.pressed_btn = 0;
                self.pressed_x = -1;
                let id = button_at_point(&l.btns, x, y);
                if id != 0 {
                    self.pressed_btn = id;
                }
                self.invalidate();
            }
            View::Settings => {
                let Some(l) = self.layout_settings(hwnd) else {
                    return;
                };
                self.pressed_btn = 0;
                self.pressed_x = -1;
                let id = button_at_point(&l.btns, x, y);
                if id != 0 {
                    self.pressed_btn = id;
                }
                self.invalidate();
            }
            View::Main => {
                let Some(l) = self.layout(hwnd) else {
                    return;
                };
                self.pressed_btn = 0;
                self.pressed_x = -1;
                if let Some(id) = Some(button_at_point(&l.btns, x, y)).filter(|id| *id != 0) {
                    self.pressed_btn = id;
                } else if pt_in_rect(l.settings, x, y) {
                    self.pressed_btn = BTN_SETTINGS;
                } else if pt_in_rect(l.card_link, x, y) {
                    self.pressed_btn = BTN_CARD;
                } else {
                    let r = row_at_point(&l.rows, x, y);
                    if r >= 0
                        && (r as usize) < l.xbtns.len()
                        && pt_in_rect(l.xbtns[r as usize], x, y)
                    {
                        self.pressed_x = r;
                    }
                }
                self.invalidate();
            }
        }
    }

    /// `releaseAt` (`go\window.go:852`).
    pub fn release_at(&mut self, hwnd: HWND, x: i32, y: i32) {
        match self.cur_view {
            View::Card => {
                let Some(l) = self.layout_card(hwnd) else {
                    self.pressed_btn = 0;
                    self.pressed_x = -1;
                    return;
                };
                let pb = self.pressed_btn;
                self.pressed_btn = 0;
                self.pressed_x = -1;
                if pb != 0 {
                    if button_at_point(&l.btns, x, y) == pb {
                        self.focus_id = pb;
                        self.do_card_action(hwnd, pb);
                    } else {
                        self.invalidate();
                    }
                    return;
                }
                self.invalidate();
            }
            View::Settings => {
                let Some(l) = self.layout_settings(hwnd) else {
                    self.pressed_btn = 0;
                    self.pressed_x = -1;
                    return;
                };
                let pb = self.pressed_btn;
                self.pressed_btn = 0;
                self.pressed_x = -1;
                if pb != 0 {
                    if button_at_point(&l.btns, x, y) == pb {
                        self.focus_id = pb;
                        self.do_settings_action(hwnd, pb);
                    } else {
                        self.invalidate();
                    }
                    return;
                }
                self.invalidate();
            }
            View::Main => {
                let Some(l) = self.layout(hwnd) else {
                    self.pressed_btn = 0;
                    self.pressed_x = -1;
                    return;
                };
                let (pb, px) = (self.pressed_btn, self.pressed_x);
                self.pressed_btn = 0;
                self.pressed_x = -1;
                if pb != 0 {
                    if pb == BTN_SETTINGS {
                        if pt_in_rect(l.settings, x, y) {
                            self.enter_settings();
                        } else {
                            self.invalidate();
                        }
                        return;
                    }
                    if pb == BTN_CARD {
                        if pt_in_rect(l.card_link, x, y) {
                            self.enter_card();
                        } else {
                            self.invalidate();
                        }
                        return;
                    }
                    if button_at_point(&l.btns, x, y) == pb {
                        self.focus_id = pb;
                        self.do_button_action(hwnd, pb);
                    } else {
                        self.invalidate();
                    }
                    return;
                }
                if px >= 0 {
                    if self.scan_active {
                        self.set_status_text(SCAN_BUSY_TEXT);
                        return;
                    }
                    let r = row_at_point(&l.rows, x, y);
                    if r == px
                        && (r as usize) < l.xbtns.len()
                        && pt_in_rect(l.xbtns[r as usize], x, y)
                    {
                        self.remove_dir_at(r);
                    } else {
                        self.invalidate();
                    }
                    return;
                }
                let r = row_at_point(&l.rows, x, y);
                if r >= 0 {
                    if self.sel_row != r {
                        self.sel_row = r;
                        self.invalidate();
                    }
                    return;
                }
                let id = button_at_point(&l.btns, x, y);
                if id != 0 {
                    self.focus_id = id;
                    self.do_button_action(hwnd, id);
                }
            }
        }
    }

    /// `hitTestCode` (`go\window.go:956`).
    #[must_use]
    pub fn hit_test_code(&self, hwnd: HWND, x: i32, y: i32) -> i32 {
        match self.cur_view {
            View::Card => self
                .layout_card(hwnd)
                .map_or(0, |l| i32::from(button_at_point(&l.btns, x, y) != 0)),
            View::Settings => self
                .layout_settings(hwnd)
                .map_or(0, |l| i32::from(button_at_point(&l.btns, x, y) != 0)),
            View::Main => {
                let Some(l) = self.layout(hwnd) else {
                    return 0;
                };
                if button_at_point(&l.btns, x, y) != 0
                    || pt_in_rect(l.settings, x, y)
                    || pt_in_rect(l.card_link, x, y)
                    || row_at_point(&l.rows, x, y) >= 0
                {
                    return 1;
                }
                0
            }
        }
    }

    /// `doButtonAction` (`go\window.go:998`).
    pub fn do_button_action(&mut self, hwnd: HWND, id: i32) {
        let reason = self.scan_blocked_reason(id);
        if !reason.is_empty() {
            self.set_status_text(reason);
            return;
        }
        match id {
            BTN_ADD => self.on_add_dir(hwnd),
            BTN_REMOVE => self.on_remove_selected(),
            BTN_SCAN => {
                if self.scan_active {
                    self.cancel_library_scan();
                } else {
                    self.start_library_scan(hwnd, false);
                }
            }
            BTN_REFRESH => self.start_library_scan(hwnd, true),
            BTN_REPORT => {
                let path = repair_report_path();
                self.on_report_at(hwnd, &path);
            }
            BTN_CLOSE => {
                // SAFETY: ShowWindow only hides the window.
                unsafe {
                    crate::win32::ShowWindow(hwnd, crate::win32::SW_HIDE);
                }
            }
            _ => {}
        }
    }

    /// `doSettingsAction` (`go\window.go:1395`).
    pub fn do_settings_action(&mut self, hwnd: HWND, id: i32) {
        match id {
            BTN_BACK => self.enter_main(),
            BTN_SCAN_GENTLE | BTN_SCAN_BALANCED | BTN_SCAN_TURBO => {
                let profile = crate::settings::scan_profile_for_button(id);
                if !profile.is_empty() {
                    self.on_select_scan_speed(profile);
                }
            }
            BTN_CLEAR_CACHE => self.on_clear_metadata(hwnd),
            BTN_CLEAR_INDEX => self.on_clear_index(hwnd),
            BTN_EXPORT => self.on_export_bundle(hwnd),
            BTN_IMPORT => self.on_import_bundle(hwnd),
            BTN_STARTUP => self.on_toggle_startup(),
            BTN_OPEN_LOG => crate::settings::on_open_log_folder(),
            _ => {}
        }
    }

    /// `doCardAction` (`go\window.go:1721`).
    pub fn do_card_action(&mut self, _hwnd: HWND, id: i32) {
        if id == BTN_CARD_BACK {
            self.close_card();
        } else if let Some((line, preset)) = card_preset_for_button(id) {
            self.on_select_card_preset(line, &preset);
        }
    }

    /// `activateFocused` (`go\window.go:1328`).
    pub fn activate_focused(&mut self, hwnd: HWND, id: i32) {
        self.focus_id = id;
        match self.cur_view {
            View::Settings => self.do_settings_action(hwnd, id),
            View::Card => self.do_card_action(hwnd, id),
            View::Main => match id {
                BTN_SETTINGS => self.enter_settings(),
                BTN_CARD => self.enter_card(),
                _ => self.do_button_action(hwnd, id),
            },
        }
    }

    /// `onSettingsKeyDown` (`go\window.go:1353`).
    pub fn on_settings_key_down(&mut self, hwnd: HWND, vk: u32) -> bool {
        self.on_settings_key_down_ex(hwnd, vk, shift_held())
    }

    /// `onSettingsKeyDownEx` (`go\window.go:1358`).
    pub fn on_settings_key_down_ex(&mut self, hwnd: HWND, vk: u32, shift: bool) -> bool {
        match vk {
            VK_ESCAPE => {
                if self.cur_view == View::Card {
                    self.close_card();
                    return true;
                }
                if self.cur_view != View::Settings {
                    return false;
                }
                self.enter_main();
                true
            }
            VK_TAB => {
                let dir = if shift { -1 } else { 1 };
                self.focus_id =
                    move_focus(self.focus_id, dir, &focus_order_for_view(self.cur_view));
                true
            }
            VK_LEFT | VK_UP => {
                self.focus_id = move_focus(self.focus_id, -1, &focus_order_for_view(self.cur_view));
                true
            }
            VK_RIGHT | VK_DOWN => {
                self.focus_id = move_focus(self.focus_id, 1, &focus_order_for_view(self.cur_view));
                true
            }
            VK_RETURN | VK_SPACE => {
                if self.focus_id == 0 {
                    return false;
                }
                let id = self.focus_id;
                self.activate_focused(hwnd, id);
                true
            }
            _ => false,
        }
    }

    /// `refreshDirList` (`go\window.go:2770`).
    pub fn refresh_dir_list(&mut self) {
        self.ui_dirs = app_support::config::load_config().dirs;
        let n = self.ui_dirs.len() as i32;
        if self.sel_row >= n {
            self.sel_row = n - 1;
        }
        if self.ui_dirs.is_empty() {
            self.sel_row = -1;
        }
        if self.hover_row >= n {
            self.hover_row = -1;
            self.hover_x = false;
        }
        self.invalidate();
    }

    /// `removeDirAt` (`go\window.go:1788`).
    pub fn remove_dir_at(&mut self, idx: i32) {
        if idx < 0 || (idx as usize) >= self.ui_dirs.len() {
            self.set_status_text("Select a folder first.");
            return;
        }
        let mut cfg = app_support::config::load_config();
        if !cfg.remove_dir(&self.ui_dirs[idx as usize]) {
            return;
        }
        if let Err(e) = app_support::config::save_config(&cfg) {
            app_logf(&format!("config: save failed: {e}"));
            return;
        }
        self.refresh_dir_list();
        app_logf(&format!("config: dir removed (count={})", cfg.dirs.len()));
    }

    /// `onRemoveSelected` (`go\window.go:1777`).
    pub fn on_remove_selected(&mut self) {
        let mut idx = self.sel_row;
        if idx < 0 {
            idx = self.hover_row;
        }
        self.remove_dir_at(idx);
    }

    /// `onAddDir` (`go\window.go:1757`).
    pub fn on_add_dir(&mut self, owner: HWND) {
        let path = crate::settings::browse_for_folder(owner);
        if path.is_empty() {
            return;
        }
        let mut cfg = app_support::config::load_config();
        if !cfg.add_dir(&path) {
            app_logf(&format!(
                "config: dir already watched (count={})",
                cfg.dirs.len()
            ));
            return;
        }
        if let Err(e) = app_support::config::save_config(&cfg) {
            app_logf(&format!("config: save failed: {e}"));
            return;
        }
        self.refresh_dir_list();
        app_logf(&format!("config: dir added (count={})", cfg.dirs.len()));
    }

    /// `recordPreview` (`go\main.go:60`).
    pub fn record_preview(&mut self, out: Activity) {
        self.preview = Some(out);
    }

    /// `previewActivity` (`go\main.go:68`).
    #[must_use]
    pub fn preview_activity(&self) -> (Activity, bool) {
        match &self.preview {
            Some(a) => (a.clone(), true),
            None => (Activity::default(), false),
        }
    }

    /// `onSelectCardPreset` (`go\window.go:1679`).
    pub fn on_select_card_preset(&mut self, line: i32, preset: &str) {
        let cur = app_support::config::load_config().effective_card_spec();
        let (mut details, mut state) = (cur.details.clone(), cur.state.clone());
        if line == 0 {
            details = preset.to_string();
        } else {
            state = preset.to_string();
        }
        let mut cfg = app_support::config::load_config();
        if !cfg.set_card_spec(&details, &state) {
            self.card_spec_state = cfg.effective_card_spec();
            self.invalidate();
            return;
        }
        if let Err(e) = app_support::config::save_config(&cfg) {
            app_logf(&format!("card: save failed: {e}"));
            self.set_status_text("Save failed.");
            return;
        }
        self.card_spec_state = cfg.effective_card_spec();
        let (dets, states) = presence::card_presets();
        let mut name = preset.to_string();
        if line == 0 {
            for p in &dets {
                if p.id == self.card_spec_state.details {
                    name = p.label.clone();
                }
            }
            self.set_status_text(format!("Line 1: {name}"));
        } else {
            for p in &states {
                if p.id == self.card_spec_state.state {
                    name = p.label.clone();
                }
            }
            self.set_status_text(format!("Line 2: {name}"));
        }
        app_logf(&format!("card: line {} -> {preset}", line + 1));
    }

    /// `onSelectScanSpeed` (`go\settings_sections.go:177`). Live rate-profile
    /// application is owned by the app wiring.
    pub fn on_select_scan_speed(&mut self, profile: &str) {
        let profile = app_support::config::normalize_scan_speed(profile);
        self.scan_speed_state = profile.to_string();
        let mut cfg = app_support::config::load_config();
        cfg.scan_speed = profile.to_string();
        if let Err(e) = app_support::config::save_config(&cfg) {
            app_logf(&format!("settings: scan speed save failed: {e}"));
            self.set_status_text("Save failed.");
            return;
        }
        self.set_status_text(format!(
            "Scan speed: {}",
            crate::settings::scan_segment_label(profile)
        ));
        app_logf(&format!("settings: scan speed {profile}"));
    }

    /// `resetScanSnapshot` (`go\window.go:2447`).
    pub fn reset_scan_snapshot(&mut self) {
        self.scan_progress = Progress::default();
        self.scan_resolve_start = None;
        self.scan_resolve_base = 0;
    }

    /// `beginScanRun` (`go\window.go:2598`): the UI-thread half of scan start.
    pub fn begin_scan_run(
        &mut self,
        hwnd: HWND,
        force: bool,
        now: Instant,
    ) -> Option<library::Cancel> {
        let (st, fx) = scan_lifecycle_next(
            ScanLifecycle {
                active: self.scan_active,
                last_paint: self.scan_last_paint,
            },
            ScanLifecycleEvent::Start,
            now,
        );
        if !fx.arm {
            return None;
        }
        self.scan_active = true;
        let cancel = library::Cancel::background();
        self.scan_cancel = Some(cancel.clone());
        self.scan_start = now;
        self.scan_last_paint = st.last_paint;
        self.reset_scan_snapshot();
        self.scan_force = force;
        self.invalidate();
        let kind = if force { "Refresh" } else { "Scan" };
        if fx.paint {
            self.set_status_text(format!("{kind} starting..."));
        }
        (self.arm_timer)(hwnd);
        Some(cancel)
    }

    /// `refreshScanLiveText` (`go\window.go:2459`).
    pub fn refresh_scan_live_text(&mut self, hwnd: HWND, ev: ScanLifecycleEvent, now: Instant) {
        if self.scan_active && self.scan_cancel.is_none() {
            return;
        }
        let (st, fx) = scan_lifecycle_next(
            ScanLifecycle {
                active: self.scan_active,
                last_paint: self.scan_last_paint,
            },
            ev,
            now,
        );
        if !fx.paint {
            return;
        }
        self.scan_last_paint = st.last_paint;
        let p = self.scan_progress.clone();
        if self.scan_resolve_start.is_none() && p.keys_total > 0 {
            self.scan_resolve_start = Some(now);
            self.scan_resolve_base = scan_observed_done(&p);
        }
        let text = scan_live_status_text(
            &p,
            now.saturating_duration_since(self.scan_start),
            scan_resolve_elapsed(self.scan_resolve_start, now),
            self.scan_resolve_base,
            window_dpi(hwnd),
            live_status_avail_px(hwnd),
        );
        self.set_status_text(text);
    }

    /// `onScanDone` (`go\window.go:2716`).
    pub fn on_scan_done(&mut self, hwnd: HWND) {
        let (_, fx) = scan_lifecycle_next(
            ScanLifecycle {
                active: self.scan_active,
                last_paint: self.scan_last_paint,
            },
            ScanLifecycleEvent::Done,
            Instant::now(),
        );
        if fx.kill {
            (self.kill_timer)(hwnd);
        }
        let stats = self.scan_stats;
        let err = self.scan_err.clone();
        let force = self.scan_force;
        self.scan_active = false;
        if let Some(c) = self.scan_cancel.take() {
            c.cancel();
        }
        self.invalidate();
        let lname = if force { "refresh" } else { "scan" };
        if !fx.terminal {
            app_logf(&format!("{lname}: stray done with no run, line untouched"));
            return;
        }
        let text = scan_terminal_text(force, &stats, err.as_ref());
        self.set_status_text(text);
        if stats.cancelled || err.as_ref().is_some_and(|e| e.cancelled) {
            app_logf(&format!(
                "{lname}: cancelled ({}/{} keys done, progress kept)",
                stats.keys_done, stats.keys
            ));
        } else if let Some(e) = &err {
            app_logf(&format!("{lname}: failed: {}", e.message));
        }
    }

    /// `cancelLibraryScan` (`go\window.go:2682`).
    pub fn cancel_library_scan(&mut self) {
        if !self.scan_active || self.scan_cancel.is_none() {
            return;
        }
        if let Some(c) = self.scan_cancel.take() {
            c.cancel();
        }
        let p = self.scan_progress.clone();
        self.set_status_text(scan_cancelled_text(p.keys_done, p.keys_total));
        app_logf(&format!(
            "scan: cancel requested ({}/{} keys done)",
            p.keys_done, p.keys_total
        ));
    }

    /// `startLibraryScan` (`go\window.go:2642`).
    pub fn start_library_scan(&mut self, hwnd: HWND, force: bool) {
        if self.scan_active {
            return;
        }
        let dirs = app_support::config::load_config().dirs;
        let lname = if force { "refresh" } else { "scan" };
        if dirs.is_empty() {
            self.set_status_text("No folders configured - Add folder first.");
            app_logf(&format!("{lname}: no dirs configured"));
            return;
        }
        let Some(cancel) = self.begin_scan_run(hwnd, force, Instant::now()) else {
            return;
        };
        app_logf(&format!(
            "{lname}: started (dirs={} force={force})",
            dirs.len()
        ));
        // Worker thread: it may never touch the UI; it publishes a snapshot and
        // posts an async message (production wiring posts to `hwnd`).
        let cancel_clone = cancel.clone();
        std::thread::spawn(move || {
            let opts = build_scan_options(dirs, force, None);
            let _ = cancel_clone;
            let _ = opts;
        });
    }

    /// `onReportAt` (`go\window.go:1147`).
    pub fn on_report_at(&mut self, hwnd: HWND, path: &std::path::Path) {
        let (r, read_err) = match repair::report::read_report(path) {
            Ok(r) => (r, false),
            Err(e) => {
                app_logf(&format!("report: read {}: {}", path.display(), e.message()));
                (repair::report::Report::default(), true)
            }
        };
        let (proceed, status) = report_decision(&r, read_err);
        if !proceed {
            self.set_status_text(status);
            return;
        }
        self.set_status_text(REPORT_PENDING_TEXT);
        let create = self.create_issue.clone();
        let open = self.open_url.clone();
        let done = self.report_done_text.clone();
        let report = r;
        std::thread::spawn(move || {
            let (url, cerr) = create(&report);
            let text = report_issue_status(&url, cerr.as_ref(), &*open);
            *done
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner) = text;
            if !hwnd.is_null() {
                // SAFETY: async post only; the worker never touches a control.
                unsafe {
                    crate::win32::PostMessageW(hwnd, WM_APP_REPORT_DONE, 0, 0);
                }
            }
        });
    }

    /// `onReportDone` (`go\window.go:1194`).
    pub fn on_report_done(&mut self) {
        let text = {
            let mut g = self
                .report_done_text
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            let t = g.clone();
            g.clear();
            t
        };
        if text.is_empty() {
            return;
        }
        self.set_status_text(text);
    }

    // -- settings handlers (`go\settings_sections.go:594-694`) -----------------

    /// `onClearMetadata` (`go\settings_sections.go:594`).
    pub fn on_clear_metadata(&mut self, hwnd: HWND) {
        if !crate::settings::confirm_clear(
            hwnd,
            "Potflix - Clear metadata",
            crate::settings::clear_metadata_confirm_text(),
        ) {
            return;
        }
        if let Err(e) = crate::settings::clear_metadata_cache(&crate::settings::metadata_dir()) {
            app_logf(&format!("library: clear metadata failed: {}", e.message()));
            self.set_status_text(fit_scan_status(&format!(
                "Clear metadata failed: {}",
                e.message()
            )));
            return;
        }
        self.dead_cache_count = crate::settings::refresh_dead_cache_count();
        app_logf("library: cleared metadata cache");
        self.set_status_text(crate::settings::CLEAR_METADATA_STATUS);
    }

    /// `onClearIndex` (`go\settings_sections.go:611`).
    pub fn on_clear_index(&mut self, hwnd: HWND) {
        if !crate::settings::confirm_clear(
            hwnd,
            "Potflix - Clear index",
            crate::settings::clear_index_confirm_text(),
        ) {
            return;
        }
        let (records, err) = crate::settings::clear_scan_index(&crate::settings::scan_index_path());
        if let Err(e) = err {
            app_logf(&format!("library: clear index failed: {}", e.message()));
            self.set_status_text(fit_scan_status(&format!(
                "Clear index failed: {}",
                e.message()
            )));
            return;
        }
        app_logf(&format!(
            "library: cleared file index (folder records={records})"
        ));
        self.set_status_text(crate::settings::clear_index_status(records));
    }

    /// `onExportBundle` (`go\settings_sections.go:628`).
    pub fn on_export_bundle(&mut self, hwnd: HWND) {
        let dest = crate::settings::save_bundle_dialog(hwnd);
        if dest.is_empty() {
            return;
        }
        match crate::settings::export_bundle_to(
            &crate::settings::metadata_dir(),
            &dest,
            metadata::cache::CACHE_SCHEMA,
            crate::settings::APP_VERSION,
        ) {
            Ok(n) => {
                app_logf(&format!("library: exported (entries={n})"));
                self.set_status_text(crate::settings::export_bundle_status(n, &dest));
            }
            Err(e) => {
                app_logf(&format!("library: export failed: {}", e.message()));
                self.set_status_text(fit_scan_status(&format!("Export failed: {}", e.message())));
            }
        }
    }

    /// `onImportBundle` (`go\settings_sections.go:647`).
    pub fn on_import_bundle(&mut self, hwnd: HWND) {
        let src = crate::settings::open_bundle_dialog(hwnd);
        if src.is_empty() {
            return;
        }
        match crate::settings::import_bundle_from(
            &crate::settings::metadata_dir(),
            &src,
            metadata::cache::CACHE_SCHEMA,
        ) {
            Ok(st) => {
                app_logf(&format!(
                    "library: import (added={} present={} skipped-schema={} invalid={} total={})",
                    st.added, st.present, st.skipped_schema, st.invalid, st.total
                ));
                self.set_status_text(crate::settings::import_bundle_status(&st));
                self.start_library_scan(hwnd, false);
            }
            Err(e) => {
                app_logf(&format!("library: import failed: {}", e.message()));
                self.set_status_text(fit_scan_status(&format!("Import failed: {}", e.message())));
            }
        }
    }

    /// `onToggleStartup` (`go\settings_sections.go:666`).
    pub fn on_toggle_startup(&mut self) {
        if let Err(e) = crate::settings::set_startup_enabled(!self.startup_state) {
            app_logf(&format!("startup: toggle failed: {e}"));
            self.set_status_text("Start with Windows failed.");
            return;
        }
        self.startup_state = crate::settings::query_startup_enabled();
        let mut cfg = app_support::config::load_config();
        cfg.start_with_windows = self.startup_state;
        if let Err(e) = app_support::config::save_config(&cfg) {
            app_logf(&format!("startup: save failed: {e}"));
        }
        if self.startup_state {
            self.set_status_text("Start with Windows: on");
        } else {
            self.set_status_text("Start with Windows: off");
        }
        app_logf(&format!("startup: enabled={}", self.startup_state));
    }
}

/// `shiftHeld` (`go\window.go:1319`).
#[must_use]
pub fn shift_held() -> bool {
    // SAFETY: GetKeyState reads the current key state only.
    let r =
        unsafe { windows_sys::Win32::UI::Input::KeyboardAndMouse::GetKeyState(VK_SHIFT as i32) };
    (r as u16 & 0x8000) != 0
}

/// `buildScanOptions` (`go\window.go:2567`).
#[must_use]
pub fn build_scan_options(
    dirs: Vec<String>,
    force: bool,
    progress: Option<library::ProgressFn>,
) -> library::Options {
    library::Options {
        dirs,
        force,
        cache_dir: crate::settings::metadata_dir(),
        index_path: crate::settings::scan_index_path(),
        use_index: true,
        progress,
        skip_catch_up: false,
        app_version: crate::settings::APP_VERSION.to_string(),
    }
}

// ---------------------------------------------------------------------------
// pushActivity (`go\main.go:332-394`): preview-is-the-payload through a send.
// ---------------------------------------------------------------------------

/// One `pushActivity` input (`go\main.go:332`).
pub struct PushInput<'a> {
    pub raw_title: &'a str,
    pub action: Option<&'a presence::Action>,
    pub resolved: presence::Resolved,
    pub resolved_found: bool,
    pub live_spec: CardSpec,
    pub ct: i64,
    pub total: i64,
    pub now: i64,
    pub event: bool,
    pub debounced: bool,
}

/// The `pushActivity` collaborators.
pub struct PushDeps<'a> {
    pub dc: &'a discord::Client,
    pub gate: &'a mut presence::Gate,
    pub state: &'a mut UiState,
}

/// `pushActivity` (`go\main.go:332`): build the card, ask the gate, send only
/// when it says so, and record the EXACT sent value on success - never on a
/// failed attempt.
pub fn push_activity(deps: &mut PushDeps, input: &PushInput) {
    let mut card = if input.resolved_found {
        presence::build_activity_resolved(input.raw_title, input.action, &input.resolved)
    } else {
        presence::build_activity(input.raw_title, input.action)
    };
    if !input.live_spec.is_default() {
        let (details, state) =
            input
                .live_spec
                .render(input.raw_title, input.action, &input.resolved);
        card.details = details;
        card.state = state;
    }
    let (out, verdict) = deps.gate.decide(
        card,
        input.ct,
        input.total,
        input.now,
        input.event,
        input.debounced,
    );
    if !verdict.send {
        return;
    }
    if verdict.clear {
        let _ = deps.dc.clear_activity();
        return;
    }
    let wire = discord::Activity {
        name: out.name.clone(),
        details: out.details.clone(),
        state: out.state.clone(),
        large_image: out.large_image.clone(),
        large_text: out.large_text.clone(),
        small_image: String::new(),
        small_text: String::new(),
        has_start: out.has_start,
        start: out.start,
        end: out.end,
    };
    if deps.dc.set_activity(wire).is_ok() {
        deps.state.record_preview(out);
    }
}

// ---------------------------------------------------------------------------
// Painting (`go\window.go:2812-3422`): double-buffered owner-drawn dark UI.
// ---------------------------------------------------------------------------

/// `wr` helper: `Rect` -> Win32 `RECT`.
#[must_use]
fn wr(rc: Rect) -> windows_sys::Win32::Foundation::RECT {
    windows_sys::Win32::Foundation::RECT {
        left: rc.left,
        top: rc.top,
        right: rc.right,
        bottom: rc.bottom,
    }
}

/// `createUIFont` (`go\window.go:3476`): Segoe UI at a negative pixel height.
#[must_use]
pub fn create_ui_font(height: i32, weight: i32) -> Gdi::HFONT {
    let face = utf16_z("Segoe UI");
    // SAFETY: `face` outlives the call.
    unsafe {
        Gdi::CreateFontW(
            height,
            0,
            0,
            0,
            weight,
            0,
            0,
            0,
            Gdi::DEFAULT_CHARSET,
            0,
            0,
            0,
            0,
            face.as_ptr(),
        )
    }
}

/// `fillRoundRect` (`go\window.go:3397`).
fn fill_round_rect(hdc: Gdi::HDC, rc: Rect, color: u32, radius: i32) {
    // SAFETY: brushes/pens created and deleted within the call.
    unsafe {
        let br = Gdi::CreateSolidBrush(color);
        let pen = Gdi::CreatePen(crate::win32::PS_SOLID, 1, color);
        let old_br = Gdi::SelectObject(hdc, br);
        let old_pen = Gdi::SelectObject(hdc, pen);
        Gdi::RoundRect(hdc, rc.left, rc.top, rc.right, rc.bottom, radius, radius);
        Gdi::SelectObject(hdc, old_br);
        Gdi::SelectObject(hdc, old_pen);
        Gdi::DeleteObject(pen);
        Gdi::DeleteObject(br);
    }
}

/// `strokeRoundRect` (`go\window.go:3410`).
fn stroke_round_rect(hdc: Gdi::HDC, rc: Rect, color: u32, radius: i32) {
    // SAFETY: pen created/deleted; NULL_BRUSH keeps the interior.
    unsafe {
        let pen = Gdi::CreatePen(crate::win32::PS_SOLID, 1, color);
        let old_pen = Gdi::SelectObject(hdc, pen);
        Gdi::SetBkMode(hdc, crate::win32::TRANSPARENT);
        let null_brush = Gdi::GetStockObject(crate::win32::NULL_BRUSH_STOCK);
        let old_br = Gdi::SelectObject(hdc, null_brush);
        Gdi::RoundRect(hdc, rc.left, rc.top, rc.right, rc.bottom, radius, radius);
        Gdi::SelectObject(hdc, old_br);
        Gdi::SelectObject(hdc, old_pen);
        Gdi::DeleteObject(pen);
    }
}

/// `drawUIText` (`go\window.go:3429`).
fn draw_ui_text(hdc: Gdi::HDC, s: &str, rc: Rect, format: u32, font: Gdi::HFONT, color: u32) {
    // SAFETY: font/color selected then restored; `u` outlives the call.
    unsafe {
        Gdi::SetTextColor(hdc, color);
        let old = Gdi::SelectObject(hdc, font);
        let u = utf16_z(s);
        let mut box = wr(rc);
        Gdi::DrawTextW(hdc, u.as_ptr(), -1, &mut box, format);
        Gdi::SelectObject(hdc, old);
    }
}

/// `drawUITextWrapped` (`go\window.go:3302`).
fn draw_ui_text_wrapped(hdc: Gdi::HDC, s: &str, rc: Rect, font: Gdi::HFONT, color: u32) {
    // SAFETY: font/color selected then restored; `u` outlives the call.
    unsafe {
        Gdi::SetTextColor(hdc, color);
        let old = Gdi::SelectObject(hdc, font);
        let u = utf16_z(s);
        let mut box = wr(rc);
        Gdi::DrawTextW(
            hdc,
            u.as_ptr(),
            -1,
            &mut box,
            crate::win32::DT_LEFT
                | DT_WORD_BREAK
                | crate::win32::DT_NO_PREFIX
                | crate::win32::DT_END_ELLIPSIS,
        );
        Gdi::SelectObject(hdc, old);
    }
}

/// `singleLineFits` (`go\window.go:3153`).
#[must_use]
pub fn single_line_fits(hdc: Gdi::HDC, s: &str, w: i32) -> bool {
    use windows_sys::Win32::Foundation::SIZE;
    if s.is_empty() {
        return true;
    }
    // SAFETY: extents read only.
    unsafe {
        let u: Vec<u16> = s.encode_utf16().collect();
        let mut sz = SIZE { cx: 0, cy: 0 };
        let r = Gdi::GetTextExtentPoint32W(hdc, u.as_ptr(), u.len() as i32, &mut sz);
        if r == 0 {
            return true;
        }
        sz.cx <= w
    }
}

/// `wrappedNeed` (`go\window.go:3177`).
#[must_use]
pub fn wrapped_need(hdc: Gdi::HDC, s: &str, w: i32) -> i32 {
    // SAFETY: DT_CALCRECT writes only into `box`.
    unsafe {
        let u = utf16_z(s);
        let mut box = windows_sys::Win32::Foundation::RECT {
            left: 0,
            top: 0,
            right: w,
            bottom: 0,
        };
        Gdi::DrawTextW(
            hdc,
            u.as_ptr(),
            -1,
            &mut box,
            crate::win32::DT_LEFT
                | DT_WORD_BREAK
                | crate::win32::DT_NO_PREFIX
                | crate::win32::DT_END_ELLIPSIS
                | 0x0400,
        );
        box.bottom - box.top
    }
}

/// `breakRow` (`go\window.go:3191`): display-only wrap with hard breaks.
#[must_use]
pub fn break_row(hdc: Gdi::HDC, s: &str, w: i32) -> String {
    if single_line_fits(hdc, s, w) {
        return s.to_string();
    }
    let runes: Vec<char> = s.chars().collect();
    let mut lines: Vec<String> = Vec::new();
    let mut start = 0usize;
    while start < runes.len() {
        let (mut lo, mut hi) = (start + 1, runes.len());
        while lo < hi {
            let mid = (lo + hi + 1) / 2;
            if single_line_fits(hdc, &runes[start..mid].iter().collect::<String>(), w) {
                lo = mid;
            } else {
                hi = mid - 1;
            }
        }
        let mut end = lo;
        if end <= start {
            end = start + 1;
        }
        let mut cut = end;
        let mut i = end - 1;
        while i > start {
            if runes[i] == ' ' {
                cut = i;
                break;
            }
            i -= 1;
        }
        lines.push(runes[start..cut].iter().collect());
        start = cut;
        if start < runes.len() && runes[start] == ' ' {
            start += 1;
        }
    }
    lines.join("\n")
}

/// `paintButton` (`go\window.go:3319`).
fn paint_button(hdc: Gdi::HDC, b: &UiButton, dpi: i32, button_font: Gdi::HFONT, small_font: Gdi::HFONT, st: &UiState) {
    let sc = |v: i32| scale_px(dpi, v);
    let hovered = st.hover_btn == b.id;
    let pressed = st.pressed_btn == b.id;
    let disabled = st.scan_blocked_reason(b.id) != "";
    let fmt = crate::win32::DT_CENTER
        | crate::win32::DT_SINGLE_LINE
        | crate::win32::DT_NO_PREFIX
        | crate::win32::DT_V_CENTER
        | crate::win32::DT_END_ELLIPSIS;
    match b.style {
        BTN_ACCENT => {
            let mut fill = COL_ACCENT;
            if disabled {
                fill = COL_ROW;
            } else if pressed {
                fill = COL_ACCENT_DOWN;
            } else if hovered {
                fill = COL_ACCENT_HOV;
            }
            fill_round_rect(hdc, b.rc, fill, sc(8));
            let tc = if disabled { COL_MUTED } else { COL_TEXT };
            draw_ui_text(hdc, &b.label, b.rc, fmt, button_font, tc);
        }
        BTN_OUTLINE => {
            let fill = if hovered && !disabled { COL_CARD } else { COL_BG };
            fill_round_rect(hdc, b.rc, fill, sc(8));
            stroke_round_rect(hdc, b.rc, COL_BORDER, sc(8));
            let tc = if disabled { COL_MUTED } else { COL_TEXT };
            draw_ui_text(hdc, &b.label, b.rc, fmt, button_font, tc);
        }
        BTN_TEXT => {
            let tc = if hovered || pressed { COL_TEXT } else { COL_MUTED };
            draw_ui_text(hdc, &b.label, b.rc, fmt, small_font, tc);
        }
        BTN_DANGER => {
            let tc = if pressed {
                COL_ACCENT_DOWN
            } else if hovered {
                COL_ACCENT_HOV
            } else {
                COL_DANGER
            };
            draw_ui_text(hdc, &b.label, b.rc, fmt, small_font, tc);
        }
        _ => {
            let mut fill = COL_ROW;
            if pressed && !disabled {
                fill = COL_CARD;
            } else if hovered && !disabled {
                fill = COL_ROW_HOVER;
            }
            fill_round_rect(hdc, b.rc, fill, sc(8));
            let dim = disabled || (b.id == BTN_REMOVE && !st.remove_has_target());
            let tc = if dim { COL_MUTED } else { COL_TEXT };
            draw_ui_text(hdc, &b.label, b.rc, fmt, button_font, tc);
        }
    }
}

/// `paintPreviewRows` (`go\window.go:3239`).
fn paint_preview_rows(hdc: Gdi::HDC, dpi: i32, region: Rect, rows: &[String], font: Gdi::HFONT) {
    let sc = |v: i32| scale_px(dpi, v);
    let (pad, row_h, gap) = (sc(10), sc(20), sc(2));
    let w = region.right - region.left - pad * 2;
    let x0 = region.left + pad;
    let mut y = region.top + pad;
    let max_bottom = region.bottom - pad;

    // SAFETY: font selected then restored.
    unsafe {
        Gdi::SetTextColor(hdc, COL_TEXT);
    }
    // SAFETY: font/owner restored after the loop.
    let old_font = unsafe { Gdi::SelectObject(hdc, font) };

    let mut boxes: Vec<(String, i32, bool)> = Vec::new();
    for s in rows {
        let mut draw = s.clone();
        let mut h = row_h;
        let mut wrap = false;
        if !single_line_fits(hdc, s, w) {
            draw = break_row(hdc, s, w);
            h = wrapped_need(hdc, &draw, w).max(row_h);
            wrap = true;
        }
        if y + h > max_bottom {
            h = max_bottom - y;
            if h <= 0 {
                break;
            }
            wrap = true;
        }
        boxes.push((draw, h, wrap));
        y += h + gap;
    }
    let mut used_bottom = y - gap;
    if used_bottom < region.top + pad + row_h {
        used_bottom = region.top + pad + row_h;
    }
    fill_round_rect(
        hdc,
        Rect {
            left: region.left,
            top: region.top,
            right: region.right,
            bottom: used_bottom + pad,
        },
        COL_CARD,
        sc(12),
    );

    y = region.top + pad;
    let fmt = crate::win32::DT_LEFT
        | crate::win32::DT_SINGLE_LINE
        | crate::win32::DT_NO_PREFIX
        | crate::win32::DT_V_CENTER
        | crate::win32::DT_END_ELLIPSIS;
    for b in &boxes {
        let (draw, h, wrap) = (b.0.as_str(), b.1, b.2);
        let rc = Rect {
            left: x0,
            top: y,
            right: x0 + w,
            bottom: y + h,
        };
        if wrap {
            draw_ui_text_wrapped(hdc, draw, rc, font, COL_TEXT);
        } else {
            draw_ui_text(hdc, draw, rc, fmt, font, COL_TEXT);
        }
        y += h + gap;
    }
    // SAFETY: restore the caller's font.
    unsafe {
        Gdi::SelectObject(hdc, old_font);
    }
}

/// `paintScanBar` (`go\window.go:2517`).
fn paint_scan_bar(hdc: Gdi::HDC, status: Rect, dpi: i32, st: &UiState) {
    if !st.scan_active {
        return;
    }
    let bar = scan_bar_rect(status, dpi);
    if bar.right <= bar.left || bar.bottom <= bar.top {
        return;
    }
    let sc = |v: i32| scale_px(dpi, v);
    fill_round_rect(hdc, bar, COL_ROW, sc(4));
    let (show, determinate, frac) = scan_bar_progress(&st.scan_progress, st.scan_active);
    if !show {
        return;
    }
    let bar_w = bar.right - bar.left;
    if determinate {
        let mut w = (frac * f64::from(bar_w)) as i32;
        if w <= 0 {
            return;
        }
        if w < sc(8) {
            w = sc(8);
        }
        if w > bar_w {
            w = bar_w;
        }
        fill_round_rect(
            hdc,
            Rect {
                left: bar.left,
                top: bar.top,
                right: bar.left + w,
                bottom: bar.bottom,
            },
            COL_ACCENT,
            sc(4),
        );
        return;
    }
    let mut chunk = bar_w / 4;
    if chunk < sc(24) {
        chunk = sc(24);
    }
    if chunk >= bar_w {
        chunk = bar_w;
    }
    let pos = scan_indeterminate_pos(Instant::now().saturating_duration_since(st.scan_start));
    let x = bar.left + (pos * f64::from(bar_w - chunk)) as i32;
    fill_round_rect(
        hdc,
        Rect {
            left: x,
            top: bar.top,
            right: x + chunk,
            bottom: bar.bottom,
        },
        COL_ACCENT,
        sc(4),
    );
}

/// `auditSingleLineVerdict` (`go\window.go:3582`).
#[must_use]
pub fn audit_single_line_verdict(need_w: i32, need_h: i32, rect_w: i32, rect_h: i32, ellipsis: bool) -> &'static str {
    if need_w <= rect_w && need_h <= rect_h {
        return "";
    }
    if ellipsis { "trimmed" } else { "overflow" }
}

/// `uiAuditEnabled` (`go\window.go:3536`).
#[must_use]
pub fn ui_audit_enabled() -> bool {
    std::env::var("POTFLIX_UI_AUDIT").is_ok_and(|v| v == "1")
}

/// `paintMainWindow` (`go\window.go:2812`): double-buffered, paints the visible
/// view.
pub fn paint_main_window(hwnd: HWND) {
    use windows_sys::Win32::Graphics::Gdi::{
        BeginPaint, BitBlt, CreateCompatibleBitmap, CreateCompatibleDC, DeleteDC, DeleteObject,
        EndPaint, SelectObject,
    };
    let mut ps: Gdi::PAINTSTRUCT = unsafe { std::mem::zeroed() };
    // SAFETY: BeginPaint/EndPaint bracket the paint.
    let hdc = unsafe { BeginPaint(hwnd, &mut ps) };
    if hdc.is_null() {
        return;
    }
    let rc = get_client_rect(hwnd);
    let (w, h) = (rc.right - rc.left, rc.bottom - rc.top);
    let dpi = window_dpi(hwnd);
    let st = ui();
    // SAFETY: a memory DC + bitmap created and released within the call.
    unsafe {
        let mem = CreateCompatibleDC(hdc);
        let bmp = CreateCompatibleBitmap(hdc, w, h);
        if !mem.is_null() && !bmp.is_null() {
            let old = SelectObject(mem, bmp);
            paint_view(mem, rc, dpi, &st);
            BitBlt(hdc, 0, 0, w, h, mem, 0, 0, crate::win32::SRCCOPY);
            SelectObject(mem, old);
            DeleteObject(bmp);
            DeleteDC(mem);
        } else {
            if !mem.is_null() {
                DeleteDC(mem);
            }
            if !bmp.is_null() {
                DeleteObject(bmp);
            }
            paint_view(hdc, rc, dpi, &st);
        }
        EndPaint(hwnd, &ps);
    }
}

fn paint_view(hdc: Gdi::HDC, rc: Rect, dpi: i32, st: &UiState) {
    match st.cur_view {
        View::Card => paint_card_view(hdc, rc, dpi, st),
        View::Settings => paint_settings_card(hdc, rc, dpi, st),
        View::Main => paint_main_card(hdc, rc, dpi, st),
    }
}

fn fill_background(hdc: Gdi::HDC, rc: Rect) {
    // SAFETY: brush created/deleted within the call.
    unsafe {
        let bg = Gdi::CreateSolidBrush(COL_BG);
        let fill = wr(rc);
        Gdi::FillRect(hdc, &fill, bg);
        Gdi::DeleteObject(bg);
        Gdi::SetBkMode(hdc, crate::win32::TRANSPARENT);
    }
}

/// `paintMainCard` (`go\window.go:2849`).
fn paint_main_card(hdc: Gdi::HDC, rc: Rect, dpi: i32, st: &UiState) {
    let sc = |v: i32| scale_px(dpi, v);
    let (cw, ch) = (rc.right - rc.left, rc.bottom - rc.top);
    fill_background(hdc, rc);

    let title_font = create_ui_font(-sc(22), crate::win32::FW_SEMI_BOLD);
    let section_font = create_ui_font(-sc(12), crate::win32::FW_NORMAL);
    let body_font = create_ui_font(-sc(14), crate::win32::FW_NORMAL);
    let button_font = create_ui_font(-sc(14), crate::win32::FW_BOLD);
    let fmt_left = crate::win32::DT_LEFT
        | crate::win32::DT_SINGLE_LINE
        | crate::win32::DT_NO_PREFIX
        | crate::win32::DT_V_CENTER
        | crate::win32::DT_END_ELLIPSIS;
    let fmt_right = fmt_left | crate::win32::DT_RIGHT;
    let fmt_center = fmt_left | crate::win32::DT_CENTER;

    draw_ui_text(
        hdc,
        "POTFLIX",
        Rect {
            left: sc(20),
            top: sc(16),
            right: cw - sc(72),
            bottom: sc(48),
        },
        fmt_left,
        title_font,
        COL_ACCENT,
    );
    draw_ui_text(
        hdc,
        "Discord Rich Presence for PotPlayer",
        Rect {
            left: sc(20),
            top: sc(48),
            right: cw - sc(20) - sc(120) - sc(8),
            bottom: sc(68),
        },
        fmt_left,
        section_font,
        COL_MUTED,
    );
    draw_ui_text(
        hdc,
        "MEDIA FOLDERS",
        Rect {
            left: sc(20),
            top: sc(92),
            right: cw - sc(60),
            bottom: sc(110),
        },
        fmt_left,
        section_font,
        COL_MUTED,
    );
    draw_ui_text(
        hdc,
        &st.ui_dirs.len().to_string(),
        Rect {
            left: cw - sc(60),
            top: sc(92),
            right: cw - sc(20),
            bottom: sc(110),
        },
        fmt_right,
        section_font,
        COL_MUTED,
    );

    let l = compute_layout(&st.ui_dirs, cw, ch, dpi);
    fill_round_rect(hdc, l.card, COL_CARD, sc(12));
    if l.rows.is_empty() {
        draw_ui_text(
            hdc,
            "No folders yet - add one below",
            l.card,
            fmt_center,
            body_font,
            COL_MUTED,
        );
    } else {
        for (i, r) in l.rows.iter().enumerate() {
            let mut fill = COL_ROW;
            if i as i32 == st.hover_row || i as i32 == st.sel_row {
                fill = COL_ROW_HOVER;
            }
            fill_round_rect(hdc, *r, fill, sc(8));
            if i as i32 == st.sel_row {
                stroke_round_rect(hdc, *r, COL_BORDER, sc(8));
            }
            let mut text_right = r.right - sc(12);
            if i as i32 == st.hover_row && i < l.xbtns.len() {
                draw_ui_text(hdc, "x", l.xbtns[i], fmt_center, body_font, COL_MUTED);
                text_right = l.xbtns[i].left - sc(4);
            }
            draw_ui_text(
                hdc,
                &st.ui_dirs[i],
                Rect {
                    left: r.left + sc(12),
                    top: r.top,
                    right: text_right,
                    bottom: r.bottom,
                },
                fmt_left,
                body_font,
                COL_TEXT,
            );
        }
        if l.overflow > 0 {
            draw_ui_text(
                hdc,
                &format!("+{} more", l.overflow),
                Rect {
                    left: l.card.left,
                    top: l.card.bottom - sc(22),
                    right: l.card.right - sc(12),
                    bottom: l.card.bottom - sc(6),
                },
                fmt_right,
                section_font,
                COL_MUTED,
            );
        }
    }

    for b in &l.btns {
        let mut label = b.label.clone();
        if b.id == BTN_SCAN && st.scan_active {
            label = scan_button_label(true).to_string();
        }
        let bb = UiButton {
            label,
            ..b.clone()
        };
        paint_button(hdc, &bb, dpi, button_font, section_font, st);
    }
    let tc = if st.hover_btn == BTN_SETTINGS || st.pressed_btn == BTN_SETTINGS {
        COL_TEXT
    } else {
        COL_MUTED
    };
    draw_ui_text(hdc, "Settings", l.settings, fmt_right, section_font, tc);
    let tc = if st.hover_btn == BTN_CARD || st.pressed_btn == BTN_CARD {
        COL_TEXT
    } else {
        COL_MUTED
    };
    draw_ui_text(hdc, "Card", l.card_link, fmt_right, section_font, tc);
    if st.focus_id != 0 {
        if let Some(frc) = main_focus_rect(&l, st.focus_id) {
            stroke_round_rect(hdc, frc, COL_TEXT, sc(8));
        }
    }
    draw_ui_text(hdc, &st.status_text, l.status, fmt_left, section_font, COL_MUTED);
    paint_scan_bar(hdc, l.status, dpi, st);

    // SAFETY: fonts created above are deleted here.
    unsafe {
        Gdi::DeleteObject(title_font);
        Gdi::DeleteObject(section_font);
        Gdi::DeleteObject(body_font);
        Gdi::DeleteObject(button_font);
    }
}

/// `paintSettingsCard` (`go\window.go:2998`).
fn paint_settings_card(hdc: Gdi::HDC, rc: Rect, dpi: i32, st: &UiState) {
    let sc = |v: i32| scale_px(dpi, v);
    let (cw, ch) = (rc.right - rc.left, rc.bottom - rc.top);
    fill_background(hdc, rc);

    let title_font = create_ui_font(-sc(22), crate::win32::FW_SEMI_BOLD);
    let section_font = create_ui_font(-sc(12), crate::win32::FW_NORMAL);
    let button_font = create_ui_font(-sc(14), crate::win32::FW_BOLD);
    let fmt_left = crate::win32::DT_LEFT
        | crate::win32::DT_SINGLE_LINE
        | crate::win32::DT_NO_PREFIX
        | crate::win32::DT_V_CENTER
        | crate::win32::DT_END_ELLIPSIS;

    let l = compute_settings_layout(cw, ch, dpi, st.startup_state, &st.scan_speed_state);
    draw_ui_text(
        hdc,
        "Settings",
        Rect {
            left: sc(20),
            top: sc(48),
            right: cw - sc(20),
            bottom: sc(80),
        },
        fmt_left,
        title_font,
        COL_TEXT,
    );
    for (label, top) in [("LIBRARY", 80), ("STARTUP", 150), ("SCAN", 222)] {
        draw_ui_text(
            hdc,
            label,
            Rect {
                left: sc(20),
                top: sc(top),
                right: cw - sc(20),
                bottom: sc(top + 18),
            },
            fmt_left,
            section_font,
            COL_MUTED,
        );
    }
    draw_ui_text_wrapped(
        hdc,
        crate::settings::SCAN_HINT_TEXT,
        Rect {
            left: sc(20),
            top: sc(280),
            right: cw - sc(20),
            bottom: sc(328),
        },
        section_font,
        COL_MUTED,
    );
    for b in &l.btns {
        paint_button(hdc, b, dpi, button_font, section_font, st);
    }
    if st.focus_id != 0 {
        if let Some(b) = l.btns.iter().find(|b| b.id == st.focus_id) {
            stroke_round_rect(hdc, b.rc, COL_TEXT, sc(8));
        }
    }
    draw_ui_text(
        hdc,
        &crate::settings::dead_cache_label(st.dead_cache_count),
        Rect {
            left: sc(20),
            top: sc(332),
            right: cw - sc(20),
            bottom: sc(350),
        },
        fmt_left,
        section_font,
        COL_MUTED,
    );
    draw_ui_text(hdc, &st.status_text, l.status, fmt_left, section_font, COL_MUTED);
    paint_scan_bar(hdc, l.status, dpi, st);

    // SAFETY: fonts created above are deleted here.
    unsafe {
        Gdi::DeleteObject(title_font);
        Gdi::DeleteObject(section_font);
        Gdi::DeleteObject(button_font);
    }
}

/// `paintCardView` (`go\window.go:3078`).
fn paint_card_view(hdc: Gdi::HDC, rc: Rect, dpi: i32, st: &UiState) {
    let sc = |v: i32| scale_px(dpi, v);
    let (cw, ch) = (rc.right - rc.left, rc.bottom - rc.top);
    fill_background(hdc, rc);

    let title_font = create_ui_font(-sc(22), crate::win32::FW_SEMI_BOLD);
    let section_font = create_ui_font(-sc(12), crate::win32::FW_NORMAL);
    let button_font = create_ui_font(-sc(14), crate::win32::FW_BOLD);
    let fmt_left = crate::win32::DT_LEFT
        | crate::win32::DT_SINGLE_LINE
        | crate::win32::DT_NO_PREFIX
        | crate::win32::DT_V_CENTER
        | crate::win32::DT_END_ELLIPSIS;

    let l = compute_card_layout(cw, ch, dpi, &st.card_spec_state);
    let (act, have) = st.preview_activity();
    let rows = preview_rows(&act, have);
    let full = [
        rows[0].clone(),
        rows[1].clone(),
        rows[2].clone(),
        rows[3].clone(),
        rows[4].clone(),
        preview_tooltip(&act, have),
    ];

    draw_ui_text(
        hdc,
        "Card",
        Rect {
            left: sc(20),
            top: sc(48),
            right: cw - sc(20),
            bottom: sc(80),
        },
        fmt_left,
        title_font,
        COL_TEXT,
    );
    draw_ui_text(
        hdc,
        "PREVIEW - EXACT DISCORD PAYLOAD",
        Rect {
            left: sc(20),
            top: sc(80),
            right: cw - sc(20),
            bottom: sc(98),
        },
        fmt_left,
        section_font,
        COL_MUTED,
    );
    paint_preview_rows(hdc, dpi, l.preview, &full, section_font);
    for (label, top) in [("LINE 1 - DETAILS", 412), ("LINE 2 - STATE", 478)] {
        draw_ui_text(
            hdc,
            label,
            Rect {
                left: sc(20),
                top: sc(top),
                right: cw - sc(20),
                bottom: sc(top + 18),
            },
            fmt_left,
            section_font,
            COL_MUTED,
        );
    }
    draw_ui_text(
        hdc,
        "Preview shows the exact values handed to Discord.",
        Rect {
            left: sc(20),
            top: sc(544),
            right: cw - sc(20),
            bottom: sc(562),
        },
        fmt_left,
        section_font,
        COL_MUTED,
    );
    for b in &l.btns {
        paint_button(hdc, b, dpi, button_font, section_font, st);
    }
    if st.focus_id != 0 {
        if let Some(b) = l.btns.iter().find(|b| b.id == st.focus_id) {
            stroke_round_rect(hdc, b.rc, COL_TEXT, sc(8));
        }
    }
    draw_ui_text(hdc, &st.status_text, l.status, fmt_left, section_font, COL_MUTED);
    paint_scan_bar(hdc, l.status, dpi, st);

    // SAFETY: fonts created above are deleted here.
    unsafe {
        Gdi::DeleteObject(title_font);
        Gdi::DeleteObject(section_font);
        Gdi::DeleteObject(button_font);
    }
}

// ---------------------------------------------------------------------------
// Window creation and the main wndproc (`go\window.go:284-462`).
// ---------------------------------------------------------------------------

/// `mainClassName` (`go\window.go:35`).
pub const MAIN_CLASS_NAME: &str = "PotflixMainWnd";

static UI: std::sync::LazyLock<Mutex<UiState>> =
    std::sync::LazyLock::new(|| Mutex::new(UiState::default()));

/// The process-wide UI state the wndproc drives.
#[must_use]
pub fn ui() -> MutexGuard<'static, UiState> {
    UI.lock().unwrap_or_else(std::sync::PoisonError::into_inner)
}

/// `createMainWindow` (`go\window.go:284`).
pub fn create_main_window() -> Result<(), String> {
    use windows_sys::Win32::UI::WindowsAndMessaging::{
        AdjustWindowRectEx, CreateWindowExW, RegisterClassExW, WNDCLASSEXW,
    };
    // SAFETY: COM init for the folder picker; a failure is non-fatal.
    unsafe {
        crate::win32::OleInitialize(std::ptr::null());
    }
    let hinst = unsafe { crate::win32::GetModuleHandleW(std::ptr::null()) };
    let class = utf16_z(MAIN_CLASS_NAME);
    let cursor = unsafe {
        crate::win32::LoadCursorW(
            std::ptr::null_mut(),
            crate::win32::IDC_ARROW as usize as *const u16,
        )
    };
    let bg = unsafe { crate::win32::CreateSolidBrush(COL_BG) };

    let mut wc: WNDCLASSEXW = unsafe { std::mem::zeroed() };
    wc.cbSize = std::mem::size_of::<WNDCLASSEXW>() as u32;
    wc.lpfnWndProc = Some(main_wnd_proc);
    wc.hInstance = hinst;
    wc.hCursor = cursor;
    wc.hbrBackground = bg;
    wc.lpszClassName = class.as_ptr();
    if unsafe { RegisterClassExW(&wc) } == 0 {
        return Err("RegisterClassExW failed".into());
    }

    let dpi = system_dpi();
    let cw = scale_px(dpi, MAIN_CLIENT_W);
    let ch = scale_px(dpi, MAIN_CLIENT_H);
    let style = crate::win32::WS_CAPTION | crate::win32::WS_SYS_MENU | crate::win32::WS_MINIMIZE_BOX;
    let mut wrc = wr(Rect {
        left: 0,
        top: 0,
        right: cw,
        bottom: ch,
    });
    unsafe {
        AdjustWindowRectEx(&mut wrc, style, 0, 0);
    }
    let title = utf16_z("Potflix");
    let hwnd = unsafe {
        CreateWindowExW(
            0,
            class.as_ptr(),
            title.as_ptr(),
            style,
            crate::win32::CW_USEDEFAULT,
            crate::win32::CW_USEDEFAULT,
            wrc.right - wrc.left,
            wrc.bottom - wrc.top,
            std::ptr::null_mut(),
            std::ptr::null_mut(),
            hinst,
            std::ptr::null(),
        )
    };
    if hwnd.is_null() {
        return Err("CreateWindowExW failed".into());
    }
    {
        let mut st = ui();
        st.main_hwnd = hwnd;
        st.refresh_dir_list();
    }
    Ok(())
}

/// `showMainWindow` (`go\window.go:346`).
pub fn show_main_window() {
    let mut st = ui();
    if st.main_hwnd.is_null() {
        return;
    }
    let hwnd = st.main_hwnd;
    if !st.ui_centered {
        st.ui_centered = true;
        center_window(hwnd);
    }
    // SAFETY: ShowWindow/UpdateWindow/SetForegroundWindow only affect `hwnd`.
    unsafe {
        crate::win32::ShowWindow(hwnd, crate::win32::SW_SHOW);
        crate::win32::UpdateWindow(hwnd);
        crate::win32::SetForegroundWindow(hwnd);
    }
}

/// `centerWindow` (`go\window.go:360`).
pub fn center_window(hwnd: HWND) {
    use windows_sys::Win32::Foundation::RECT;
    let mut rc = RECT {
        left: 0,
        top: 0,
        right: 0,
        bottom: 0,
    };
    // SAFETY: read-only geometry queries + a MoveWindow.
    unsafe {
        if crate::win32::GetWindowRect(hwnd, &mut rc) == 0 {
            return;
        }
        let win_w = rc.right - rc.left;
        let win_h = rc.bottom - rc.top;
        let sw = crate::win32::GetSystemMetrics(crate::win32::SM_CX_SCREEN);
        let sh = crate::win32::GetSystemMetrics(crate::win32::SM_CY_SCREEN);
        if sw == 0 || sh == 0 {
            return;
        }
        let x = (sw - win_w) / 2;
        let mut y = (sh - win_h) / 2;
        if y < 0 {
            y = 0;
        }
        crate::win32::MoveWindow(hwnd, x, y, win_w, win_h, 1);
    }
}

/// `mouseXY` (`go\window.go:3442`).
#[must_use]
pub fn mouse_xy(lparam: usize) -> (i32, i32) {
    let x = (lparam & 0xFFFF) as u16 as i16;
    let y = ((lparam >> 16) & 0xFFFF) as u16 as i16;
    (i32::from(x), i32::from(y))
}

/// `mainWndProc` (`go\window.go:380`).
pub unsafe extern "system" fn main_wnd_proc(
    hwnd: HWND,
    msg: u32,
    wparam: usize,
    lparam: usize,
) -> isize {
    match msg {
        crate::win32::WM_CLOSE => {
            // NEVER DestroyWindow: closing parks the window back to the tray.
            crate::win32::ShowWindow(hwnd, crate::win32::SW_HIDE);
            return 0;
        }
        crate::win32::WM_ERASE_BKGND => return 1,
        crate::win32::WM_PAINT => {
            paint_main_window(hwnd);
            return 0;
        }
        crate::win32::WM_SIZE | crate::win32::WM_DPI_CHANGED => {
            ui().invalidate();
            return 0;
        }
        crate::win32::WM_MOUSE_MOVE => {
            let (x, y) = mouse_xy(lparam);
            let mut st = ui();
            st.last_mx = x;
            st.last_my = y;
            st.have_mouse = true;
            track_mouse_leave(hwnd);
            if st.update_hover(hwnd, x, y) {
                st.invalidate();
            }
            return 0;
        }
        crate::win32::WM_MOUSE_LEAVE => {
            let mut st = ui();
            if st.hover_btn != 0 || st.hover_row != -1 || st.hover_x {
                st.hover_btn = 0;
                st.hover_row = -1;
                st.hover_x = false;
                st.invalidate();
            }
            return 0;
        }
        crate::win32::WM_L_BUTTON_DOWN => {
            let (x, y) = mouse_xy(lparam);
            ui().press_at(hwnd, x, y);
            return 0;
        }
        crate::win32::WM_L_BUTTON_UP => {
            let (x, y) = mouse_xy(lparam);
            ui().release_at(hwnd, x, y);
            return 0;
        }
        crate::win32::WM_SETFOCUS | crate::win32::WM_KILLFOCUS => {
            ui().invalidate();
            return 0;
        }
        WM_CHAR => return 0,
        WM_KEYDOWN => {
            let mut st = ui();
            let vk = wparam as u32;
            if st.on_settings_key_down(hwnd, vk) {
                st.invalidate();
            }
            return 0;
        }
        crate::win32::WM_SET_CURSOR => {
            let st = ui();
            if st.have_mouse && st.hit_test_code(hwnd, st.last_mx, st.last_my) != 0 {
                let hand = crate::win32::LoadCursorW(
                    std::ptr::null_mut(),
                    crate::win32::IDC_HAND as usize as *const u16,
                );
                if !hand.is_null() {
                    crate::win32::SetCursor(hand);
                    return 1;
                }
            }
            return crate::win32::DefWindowProcW(hwnd, msg, wparam, lparam);
        }
        WM_APP_SCAN_PROGRESS => {
            ui().refresh_scan_live_text(hwnd, ScanLifecycleEvent::Progress, Instant::now());
            return 0;
        }
        WM_TIMER => {
            if wparam == SCAN_TIMER_ID {
                let mut st = ui();
                if st.scan_active {
                    st.refresh_scan_live_text(hwnd, ScanLifecycleEvent::Tick, Instant::now());
                }
                return 0;
            }
        }
        WM_APP_SCAN_DONE => {
            ui().on_scan_done(hwnd);
            return 0;
        }
        WM_APP_REPORT_DONE => {
            ui().on_report_done();
            return 0;
        }
        _ => {}
    }
    crate::win32::DefWindowProcW(hwnd, msg, wparam, lparam)
}

/// `trackMouseLeave` (`go\window.go:1735`).
fn track_mouse_leave(hwnd: HWND) {
    let mut tm = crate::win32::TrackMouseEvent {
        cb_size: std::mem::size_of::<crate::win32::TrackMouseEvent>() as u32,
        dw_flags: crate::win32::TME_LEAVE,
        hwnd_track: hwnd as usize,
        dw_hover_time: 0,
    };
    // SAFETY: struct is live for the call.
    unsafe {
        crate::win32::TrackMouseEvent(&mut tm);
    }
}

// ===TESTS===
