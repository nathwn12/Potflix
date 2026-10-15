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

// The Win32 surface is inherently pointer-taking: every entry point accepts an
// `HWND`/`HDC` and hands it to `windows-sys`. The handles are opaque and the
// module never dereferences them itself, so the lint is silenced module-wide.
#![allow(clippy::not_unsafe_ptr_arg_deref)]

use std::sync::{Arc, Mutex, MutexGuard};

use std::time::{Duration, Instant};

use windows_sys::Win32::Foundation::HWND;
use windows_sys::Win32::Graphics::Gdi;

use crate::brand;
use crate::win32::{Rect, utf16_z};

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
pub const VK_PRIOR: u32 = 0x21;
pub const VK_NEXT: u32 = 0x22;
pub const VK_END: u32 = 0x23;
pub const VK_HOME: u32 = 0x24;
pub const VK_LEFT: u32 = 0x25;
pub const VK_UP: u32 = 0x26;
pub const VK_RIGHT: u32 = 0x27;
pub const VK_DOWN: u32 = 0x28;

pub const SW_SHOW_NORMAL: i32 = 1;
pub const DT_WORD_BREAK: u32 = 0x0010;

// ---------------------------------------------------------------------------
// Palette -- the repo's own brand system (`brand`, ported from
// `design\make_logo.py`).
//
// The incumbent ramp was an ad-hoc grey ladder with the Netflix red dropped in:
// it carried the red but none of the identity, because the identity lives in
// `design\` and nothing read it. Every value below is now a brand token, and
// every pair the app paints is measured in `brand`'s `brand_contrast_gate`.
//
// The roles are kept (canvas / chrome / panel / control / text) so no paint site
// had to change shape; only the values moved to the brand palette.
// ---------------------------------------------------------------------------

/// `rgb(r, g, b)` (`go\window.go:3487`).
#[must_use]
pub const fn rgb(r: u32, g: u32, b: u32) -> u32 {
    r | (g << 8) | (b << 16)
}

// --- Surfaces: the elevation ladder, darkest first. -------------------------

/// The canvas: brand `BLACK #0B0B0B`. Also the window's own background brush, so
/// the DWM caption and the client area meet with no seam.
pub const COL_BG: u32 = brand::BLACK;
/// The chrome band (header and tab strip) and the list panel: the identity
/// pass's own sheet background `#1A1B1E`, one luminance step above the canvas.
pub const COL_SURFACE: u32 = brand::CHROME;
/// A raised panel / a row chip: the brand card surface `#232428`.
pub const COL_CARD: u32 = brand::CARD;
/// A control resting on a panel (`elevation 3`): the card surface itself.
pub const COL_ROW: u32 = brand::CARD;
/// The same control, hovered or selected: one luminance step up.
pub const COL_ROW_HOVER: u32 = brand::CARD_HOVER;
/// A control BOUNDARY: the hairline that identifies a secondary control's shape
/// (`4.09:1` on the canvas, `3.22:1` on the card surface -- measured).
pub const COL_BORDER: u32 = brand::EDGE;
/// The same hairline under hover. The brand has one boundary weight; hover is
/// carried by the fill step, so this aliases the boundary rather than inventing
/// a second grey.
pub const COL_BORDER_STRONG: u32 = brand::EDGE;

// --- Text: one step per role, each contrast-checked against its surface. ----

/// Body text: the brand card header `#F2F3F5` (`13.96:1` on the card surface,
/// `17.73:1` on the canvas; see `brand_contrast_gate`).
pub const COL_TEXT: u32 = brand::CARD_HDR;
/// Text ON an accent fill: brand `WHITE` (`4.79:1` on `RED`).
pub const COL_ON_ACCENT: u32 = brand::WHITE;
/// Secondary text: labels, hints, status. The brand card muted `#B5BAC1`
/// (`7.94:1` on the card surface, `10.08:1` on the canvas).
pub const COL_MUTED: u32 = brand::CARD_MUT;
/// Disabled text (`#8A8F98`): still readable (`4.77:1` on the card surface,
/// `6.06:1` on the canvas), clearly out of play. Derived rather than taken from
/// the card quartet, whose muted step IS the secondary-text step, so a disabled
/// control cannot read as an enabled one.
pub const COL_MUTED_DIM: u32 = brand::DISABLED_TEXT;

// --- Accent: the brand red, three interaction steps. -----------------------

pub const COL_ACCENT: u32 = brand::RED;
pub const COL_ACCENT_HOV: u32 = brand::RED_HOVER;
pub const COL_ACCENT_DOWN: u32 = brand::DARK;
/// The selection bar's backing and the selected-row tint: the accent over the
/// card surface. Never carries text.
pub const COL_ACCENT_WASH: u32 = rgb(0x3A, 0x1C, 0x21);

/// Destructive text (Remove, and the clear actions' labels): the brand red
/// lifted until it clears the body gate on every surface it lands on
/// (`4.97:1` on the card surface, `6.31:1` on the canvas).
pub const COL_DANGER: u32 = brand::DANGER_TEXT;

// --- The named state set: every control paints from these, never a literal. --

/// Control, default.
pub const STATE_BG: u32 = COL_ROW;
/// Control, hover.
pub const STATE_BG_HOVER: u32 = COL_ROW_HOVER;
/// Control, pressed: the receding plane, one step down from the card surface.
pub const STATE_BG_PRESSED: u32 = brand::CHROME;
/// Control, disabled.
pub const STATE_BG_DISABLED: u32 = brand::CHROME;
/// Control outline, default / hover / focused.
pub const STATE_BORDER: u32 = COL_BORDER;
pub const STATE_BORDER_HOVER: u32 = COL_BORDER_STRONG;
/// Text, default / hover / pressed / disabled.
pub const STATE_TEXT: u32 = COL_TEXT;
pub const STATE_TEXT_DISABLED: u32 = COL_MUTED_DIM;
/// The keyboard focus ring and the selection bar share the accent.
pub const STATE_FOCUS: u32 = COL_ACCENT;

// --- Geometry tokens: a 4px grid, and the house squircle. -------------------

/// Spacing grid, logical px at 96 DPI. Every layout number is one of these.
pub const SP_1: i32 = 4;
pub const SP_2: i32 = 8;
pub const SP_3: i32 = 12;
pub const SP_4: i32 = 16;
pub const SP_5: i32 = 20;
pub const SP_6: i32 = 24;
pub const SP_8: i32 = 32;

/// The control height every radius below is derived from.
pub const BTN_H: i32 = 36;

/// Corner radii. Each is the house squircle fraction (`brand::squircle`, the
/// app icon's own 112/512) applied to the square it rounds: a control rounds
/// like a `BTN_H` tile, a row like a `BTN_H` chip, a panel like a 64px tile, the
/// scan bar like an 18px one. No radius is a free-hand number.
pub const R_CTRL: i32 = brand::squircle(BTN_H);
pub const R_ROW: i32 = brand::squircle(BTN_H);
pub const R_PANEL: i32 = brand::squircle(64);
pub const R_BAR: i32 = brand::squircle(18);
/// The mark tile in the header: a 32px square.
pub const R_TILE: i32 = brand::squircle(32);

/// Stroke widths, logical px at 96 DPI.
pub const STROKE_HAIR: i32 = 1;
pub const STROKE_FOCUS: i32 = 2;
/// The focus ring's inset from its control's edge: the ring never touches the
/// control outline, so a focused control reads as focused and not as re-outlined.
pub const FOCUS_INSET: i32 = 3;
/// The selection bar down the left edge of a selected row.
pub const SELECT_BAR_W: i32 = 3;
/// The list scrollbar: track/thumb width, its inset from the panel's right edge,
/// and the minimum thumb length (a long list must still show a grabbable thumb).
pub const SCROLLBAR_W: i32 = 5;
pub const SCROLLBAR_INSET: i32 = 6;
pub const SCROLLBAR_MIN_THUMB: i32 = 24;

// --- Type scale: logical px at 96 DPI, one size per role. ------------------

/// The wordmark (`POTFLIX`), in Archivo Black, tracked.
pub const FS_WORDMARK: i32 = brand::FS_WORDMARK;
/// A view's own heading.
pub const FS_TITLE: i32 = brand::FS_TITLE;
/// Rows, labels, values.
pub const FS_BODY: i32 = brand::FS_BODY;
/// A control's label.
pub const FS_CONTROL: i32 = brand::FS_CONTROL;
/// Field hints and the status line.
pub const FS_HINT: i32 = brand::FS_HINT;
/// A section's micro-label: tiny, uppercase, letter-spaced.
pub const FS_LABEL: i32 = brand::FS_LABEL;
/// The count badge's numeral.
pub const FS_BADGE: i32 = brand::FS_BADGE;

// Button paint styles (`go\window.go:194-200`).
pub const BTN_SUBTLE: i32 = 0;
pub const BTN_ACCENT: i32 = 1;
pub const BTN_OUTLINE: i32 = 2;
pub const BTN_TEXT: i32 = 3;
pub const BTN_DANGER: i32 = 4;
/// A segment of a segmented control or a row of mutually-exclusive choices (the
/// tab strip, the scan-speed segments, the card presets): an unlit segment.
pub const BTN_TAB: i32 = 5;
/// The same segment, lit. The lit state is a fill step plus a red indicator under
/// the label -- never a second accent FILL, because each view has exactly one
/// primary control (`Scan`) and selection is not a primary action.
pub const BTN_TAB_SEL: i32 = 6;

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
    /// The chrome header band (mark, wordmark, subtitle).
    pub header: Rect,
    /// The tab strip's outer rect; the two segments are [`UiLayout::card_link`]
    /// (Card) and [`UiLayout::settings`] (Settings).
    pub tab_strip: Rect,
    /// The section micro-label's line (the folder count's badge sits at its
    /// right end).
    pub section: Rect,
    /// The first visible folder index (`0` when the list fits).
    pub list_top: i32,
    /// How many folders the viewport can show.
    pub list_capacity: i32,
    /// The scrollbar track in the card's right gutter; empty when the list fits.
    pub scrollbar: Rect,
    /// The scrollbar thumb inside [`UiLayout::scrollbar`]; empty when it fits.
    pub thumb: Rect,
}

impl UiLayout {
    /// Whether the list has anything hidden and therefore shows a scrollbar.
    #[must_use]
    pub fn scrolled(&self) -> bool {
        self.scrollbar.right > self.scrollbar.left && self.scrollbar.bottom > self.scrollbar.top
    }
}

/// The settings-view hit-test table (`go\window.go:263-269`).
#[derive(Clone, Debug, Default)]
pub struct SettingsLayout {
    pub status: Rect,
    pub btns: Vec<UiButton>,
    /// The section micro-labels, in paint order. Owned by the layout so the
    /// label a paint call draws and the rect the audit measures cannot drift.
    pub labels: Vec<NamedTextRect>,
    /// The scan-speed hint's wrapped text block.
    pub hint: Rect,
    /// The dead-cache line.
    pub dead: Rect,
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
    /// The section micro-labels, in paint order (the preview heading and the two
    /// preset-row headings).
    pub labels: Vec<NamedTextRect>,
    /// The explanatory line under the preset rows.
    pub hint: Rect,
}

impl CardLayout {
    /// `cardBtn` (`go\window.go:1444`).
    #[must_use]
    pub fn card_btn(&self, id: i32) -> Option<Rect> {
        self.btns.iter().find(|b| b.id == id).map(|b| b.rc)
    }
}

/// One painted text rect with its stable audit id (`go\window.go:666`).
#[derive(Clone, Debug)]
pub struct NamedTextRect {
    pub id: String,
    pub rc: Rect,
}

// Layout metrics (`go\window.go:495-499`).
pub const MAIN_CLIENT_W: i32 = 460;
pub const MAIN_CLIENT_H: i32 = 680;
pub const MAIN_MARGIN: i32 = 20;

// --- The main view's vertical stack, logical px at 96 DPI. ------------------
//
// These are the composition's skeleton, shared by `compute_layout_total` (the
// hit-test table) and the paint path, so a rect can never disagree with what is
// drawn in it.

/// The chrome header band: mark, wordmark, subtitle.
pub const HEADER_H_L: i32 = 64;
/// The tab strip's top edge and height.
pub const TAB_TOP_L: i32 = 76;
pub const TAB_H_L: i32 = 34;
/// A section micro-label's line height.
pub const SECTION_H_L: i32 = 18;
/// The folder panel's inner padding.
pub const PAD_L: i32 = 10;
/// A folder row's height and the gap between rows.
pub const ROW_H_L: i32 = 36;
pub const ROW_GAP_L: i32 = 8;
/// The `+N more` line's reserved height.
pub const OVERFLOW_H_L: i32 = 22;
/// The action grid: one column gap, one row gap, three rows.
pub const BTN_GAP_L: i32 = 12;
pub const FOOTER_ROWS_L: i32 = 3;
/// The status line's height and the bottom margin below it.
pub const STATUS_H_L: i32 = 20;
pub const BOTTOM_L: i32 = 14;
/// The header mark tile's side.
pub const MARK_TILE_L: i32 = 32;

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

/// Build the main-view hit-test table for `dirs`, client size and DPI, with the
/// list viewport at its top (`list_top = 0`).
#[must_use]
pub fn compute_layout(dirs: &[String], cw: i32, ch: i32, dpi: i32) -> UiLayout {
    compute_layout_from(dirs, cw, ch, dpi, 0)
}

/// The scrollbar thumb inside `track`, or an empty rect when nothing is hidden.
///
/// Pure so it can be pinned: the thumb's length is the visible fraction of the
/// list, never shorter than `min_thumb` (a 200-folder list must still offer a
/// grabbable thumb), and its offset is the scroll fraction of the thumb's travel.
#[must_use]
pub fn scrollbar_thumb(track: Rect, total: i32, capacity: i32, top: i32, min_thumb: i32) -> Rect {
    let h = track.bottom - track.top;
    if total <= capacity || capacity <= 0 || h <= 0 {
        return Rect::default();
    }
    let mut th = h * capacity / total;
    if th > h {
        th = h;
    }
    if th < min_thumb {
        th = min_thumb.min(h);
    }
    let travel = h - th;
    let max_top = total - capacity;
    let off = if max_top > 0 {
        (top.clamp(0, max_top) * travel) / max_top
    } else {
        0
    };
    Rect {
        left: track.left,
        top: track.top + off,
        right: track.right,
        bottom: track.top + off + th,
    }
}

/// The tab strip's two segments as `(card, settings)`, in every view.
///
/// One function owns the strip's geometry so the three views cannot drift: the
/// strip is the same control, in the same place, on every view, and switching
/// views never moves the control the user just clicked.
#[must_use]
pub fn tab_strip_rects(cw: i32, dpi: i32) -> (Rect, Rect) {
    let sc = |v: i32| scale_px(dpi, v);
    let margin = sc(MAIN_MARGIN);
    let content_w = (cw - margin * 2).max(sc(100));
    let top = sc(TAB_TOP_L);
    let bottom = top + sc(TAB_H_L);
    // Equal segments with one gap: the same rule the action grid uses, so both
    // segments are exactly equal at every DPI.
    let seg_w = (content_w - sc(SP_2)) / 2;
    (
        Rect {
            left: margin,
            top,
            right: margin + seg_w,
            bottom,
        },
        Rect {
            left: margin + seg_w + sc(SP_2),
            top,
            right: margin + seg_w + sc(SP_2) + seg_w,
            bottom,
        },
    )
}

/// The two tab-strip segments as controls, for any view's button table.
#[must_use]
pub fn tab_strip_buttons(cw: i32, dpi: i32) -> Vec<UiButton> {
    let (card, settings) = tab_strip_rects(cw, dpi);
    vec![
        UiButton {
            id: BTN_CARD,
            label: "Card".into(),
            style: BTN_TAB,
            rc: card,
        },
        UiButton {
            id: BTN_SETTINGS,
            label: "Settings".into(),
            style: BTN_TAB,
            rc: settings,
        },
    ]
}

/// The same table with the list scrolled to `top` (the index of the first visible
/// folder). `top` is clamped to the real scroll range, and the card keeps the
/// height of a full viewport once the list overflows, so scrolling never resizes
/// the card under the pointer.
#[must_use]
pub fn compute_layout_from(
    dirs: &[String],
    cw: i32,
    ch: i32,
    dpi: i32,
    top: i32,
) -> UiLayout {
    compute_layout_total(dirs.len() as i32, cw, ch, dpi, top)
}

/// The same table from a folder COUNT instead of the folders: the scroll
/// clamp for [`UiState::scroll_list`], which owns no folder slice. Pure.
#[must_use]
pub fn compute_layout_total(total: i32, cw: i32, ch: i32, dpi: i32, top: i32) -> UiLayout {
    let sc = |v: i32| scale_px(dpi, v);
    let margin = sc(MAIN_MARGIN);
    let content_w = (cw - margin * 2).max(sc(100));

    let mut l = UiLayout::default();
    // --- The vertical stack -------------------------------------------------
    //
    // One column, top to bottom: chrome header, tab strip, section micro-label,
    // the folder panel, then the action grid and the status line. Every number
    // is a token or a sum of tokens, so the composition moves as one piece and
    // the panel's height is whatever the grid leaves it.
    let header_h = sc(HEADER_H_L);
    let tab_top = sc(TAB_TOP_L);
    let tab_h = sc(TAB_H_L);
    let section_top = tab_top + tab_h + sc(SP_3);
    let section_h = sc(SECTION_H_L);
    let panel_top = section_top + section_h + sc(SP_2);

    let pad = sc(PAD_L);
    let row_h = sc(ROW_H_L);
    let gap = sc(ROW_GAP_L);

    let btn_h = sc(BTN_H);
    let btn_gap = sc(BTN_GAP_L);
    let footer_h = FOOTER_ROWS_L * btn_h + (FOOTER_ROWS_L - 1) * btn_gap;
    let status_h = sc(STATUS_H_L);
    let bottom = sc(BOTTOM_L);
    let status_top = ch - bottom - status_h;
    let footer_top = status_top - sc(SP_3) - footer_h;
    let panel_max_h = (footer_top - sc(SP_4) - panel_top).max(sc(80));

    l.header = Rect {
        left: 0,
        top: 0,
        right: cw,
        bottom: header_h,
    };

    // --- The tab strip ------------------------------------------------------
    let (card_tab, settings_tab) = tab_strip_rects(cw, dpi);
    l.tab_strip = Rect {
        left: card_tab.left,
        top: card_tab.top,
        right: settings_tab.right,
        bottom: card_tab.bottom,
    };
    l.card_link = card_tab;
    l.settings = settings_tab;
    l.section = Rect {
        left: margin,
        top: section_top,
        right: margin + content_w,
        bottom: section_top + section_h,
    };

    // --- The folder panel ---------------------------------------------------
    let avail = panel_max_h - pad * 2;
    let mut max_rows = (avail + gap) / (row_h + gap);
    if max_rows < 1 {
        max_rows = 1;
    }

    let top = top.clamp(0, (total - max_rows).max(0));
    let mut vis = total - top;
    if vis > max_rows {
        l.overflow = vis - max_rows;
        vis = max_rows;
    }
    if vis < 0 {
        vis = 0;
    }
    l.list_top = top;
    l.list_capacity = max_rows;
    // The viewport is full-height once anything is hidden, whichever offset the
    // list is scrolled to: the `+N more` line's height is reserved whenever the
    // list overflows at all, so scrolling never resizes the panel (and its
    // scrollbar track) under the pointer. The line itself still paints only
    // while something hides below (`l.overflow > 0` at the paint site).
    let viewport = if total > max_rows { max_rows } else { vis };
    let mut panel_h = pad * 2 + row_h;
    if viewport > 0 {
        panel_h = pad * 2 + viewport * row_h + (viewport - 1) * gap;
        if total > max_rows {
            panel_h += sc(OVERFLOW_H_L);
        }
    }
    if panel_h > panel_max_h {
        panel_h = panel_max_h;
    }
    l.card = Rect {
        left: margin,
        top: panel_top,
        right: margin + content_w,
        bottom: panel_top + panel_h,
    };
    let row_x = l.card.left + pad;
    let row_w = content_w - pad * 2;
    for i in 0..vis {
        let y = panel_top + pad + i * (row_h + gap);
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
    // The scrollbar lives inside the panel's right padding gutter, so it never
    // overlaps a row or the `+N more` line. Gated on real overflow (`total >
    // max_rows`), not on `l.overflow` (hidden-below): the track must persist
    // at the last viewport too, or it blinks out exactly when reached.
    if total > max_rows {
        let sb_w = sc(SCROLLBAR_W);
        let inset = sc(SCROLLBAR_INSET);
        let track = Rect {
            left: l.card.right - inset,
            top: l.card.top + pad,
            right: l.card.right - inset + sb_w,
            bottom: l.card.bottom - pad,
        };
        l.scrollbar = track;
        l.thumb = scrollbar_thumb(track, total, max_rows, top, sc(SCROLLBAR_MIN_THUMB));
    }

    // --- The action grid ----------------------------------------------------
    //
    // ONE grid for every action, all three rows of it. Equal columns
    // (`col_w = (content_w - gap) / 2`), one gap between columns and one between
    // rows, and exactly one primary control (`Scan`, accent) -- the rest are
    // secondary. This replaces the incumbent's two off-grid columns plus a
    // right-aligned bare `Close to tray` text and a stray left-aligned `Report`
    // on a fourth, unaligned row.
    let col_w = (content_w - btn_gap) / 2;
    let (x0, x1) = (margin, margin + col_w + btn_gap);
    let y_of = |row: i32| footer_top + row * (btn_h + btn_gap);
    let cell = |x: i32, y: i32| Rect {
        left: x,
        top: y,
        right: x + col_w,
        bottom: y + btn_h,
    };
    l.btns = vec![
        // The tab strip's two segments are controls too, so they hover, press,
        // focus and paint through the same machinery as the grid above.
        UiButton {
            id: BTN_CARD,
            label: "Card".into(),
            style: BTN_TAB,
            rc: l.card_link,
        },
        UiButton {
            id: BTN_SETTINGS,
            label: "Settings".into(),
            style: BTN_TAB,
            rc: l.settings,
        },
        // Row 1: the run controls. `Scan` is the view's single primary.
        UiButton {
            id: BTN_SCAN,
            label: "Scan".into(),
            style: BTN_ACCENT,
            rc: cell(x0, y_of(0)),
        },
        UiButton {
            id: BTN_REFRESH,
            label: "Refresh".into(),
            style: BTN_OUTLINE,
            rc: cell(x1, y_of(0)),
        },
        // Row 2: the folder controls.
        UiButton {
            id: BTN_ADD,
            label: "Add folder".into(),
            style: BTN_OUTLINE,
            rc: cell(x0, y_of(1)),
        },
        UiButton {
            id: BTN_REMOVE,
            label: "Remove".into(),
            style: BTN_OUTLINE,
            rc: cell(x1, y_of(1)),
        },
        // Row 3: the two window actions, same anatomy as every other control.
        UiButton {
            id: BTN_REPORT,
            label: report_button_label().into(),
            style: BTN_OUTLINE,
            rc: cell(x0, y_of(2)),
        },
        UiButton {
            id: BTN_CLOSE,
            label: "Close to tray".into(),
            style: BTN_OUTLINE,
            rc: cell(x1, y_of(2)),
        },
    ];

    let st_h = sc(20);
    l.status = Rect {
        left: margin,
        top: status_top,
        right: margin + content_w,
        bottom: status_top + st_h,
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
    let content_w = (cw - margin * 2).max(sc(100));

    let mut l = SettingsLayout::default();

    // The same vertical stack and the same two-column grid the main view uses:
    // header band, tab strip, then one labelled group per concern, then the
    // footer row. `label_top` is the first section label's top edge, which is
    // exactly where the tab strip ends.
    let label_top = sc(TAB_TOP_L) + sc(TAB_H_L) + sc(SP_3);
    let row_pitch = sc(BTN_H) + sc(BTN_GAP_L);
    let group_gap = sc(SP_4);
    let label_h = sc(SECTION_H_L);
    let after_label = label_h + sc(SP_2);

    let lib_row1 = label_top + after_label;
    let lib_row2 = lib_row1 + row_pitch;
    let startup_label = lib_row2 + sc(BTN_H) + group_gap;
    let startup_row = startup_label + after_label;
    let scan_label = startup_row + sc(BTN_H) + group_gap;
    let scan_row = scan_label + after_label;
    let hint_top = scan_row + sc(BTN_H) + sc(SP_2) + sc(SP_1);
    let hint_h = sc(48);
    let dead_top = hint_top + hint_h + sc(SP_1);

    l.labels = vec![
        NamedTextRect {
            id: "settings.lib".into(),
            rc: Rect {
                left: margin,
                top: lib_row1 - after_label,
                right: margin + content_w,
                bottom: lib_row1 - after_label + label_h,
            },
        },
        NamedTextRect {
            id: "settings.startup".into(),
            rc: Rect {
                left: margin,
                top: startup_label,
                right: margin + content_w,
                bottom: startup_label + label_h,
            },
        },
        NamedTextRect {
            id: "settings.scan".into(),
            rc: Rect {
                left: margin,
                top: scan_label,
                right: margin + content_w,
                bottom: scan_label + label_h,
            },
        },
    ];
    l.hint = Rect {
        left: margin,
        top: hint_top,
        right: margin + content_w,
        bottom: hint_top + hint_h,
    };
    l.dead = Rect {
        left: margin,
        top: dead_top,
        right: margin + content_w,
        bottom: dead_top + label_h,
    };

    // The same two-column grid as the main view: equal columns, one gap.
    let col_w = (content_w - sc(BTN_GAP_L)) / 2;
    let (x0, x1) = (margin, margin + col_w + sc(BTN_GAP_L));
    let cell = |x: i32, y: i32| Rect {
        left: x,
        top: y,
        right: x + col_w,
        bottom: y + sc(BTN_H),
    };

    // The scan-speed segments: three equal cells, the last absorbing the
    // remainder so the group ends exactly on the content edge.
    let seg_gap = sc(SP_2);
    let seg_w = (content_w - seg_gap * 2) / 3;
    let seg = |i: i32| {
        let left = margin + i * (seg_w + seg_gap);
        Rect {
            left,
            top: scan_row,
            right: if i == 2 { margin + content_w } else { left + seg_w },
            bottom: scan_row + sc(BTN_H),
        }
    };

    let status_h = sc(STATUS_H_L);
    let status_top = ch - sc(BOTTOM_L) - status_h;
    let footer_top = status_top - sc(SP_3) - sc(BTN_H);
    l.status = Rect {
        left: margin,
        top: status_top,
        right: margin + content_w,
        bottom: status_top + status_h,
    };

    let cur_scan = app_support::config::normalize_scan_speed(scan_speed);
    l.btns = tab_strip_buttons(cw, dpi);
    l.btns.extend([
        UiButton {
            id: BTN_CLEAR_CACHE,
            label: "Clear metadata".into(),
            style: BTN_DANGER,
            rc: cell(x0, lib_row1),
        },
        UiButton {
            id: BTN_CLEAR_INDEX,
            label: "Clear index".into(),
            style: BTN_DANGER,
            rc: cell(x1, lib_row1),
        },
        UiButton {
            id: BTN_EXPORT,
            label: "Export...".into(),
            style: BTN_OUTLINE,
            rc: cell(x0, lib_row2),
        },
        UiButton {
            id: BTN_IMPORT,
            label: "Import...".into(),
            style: BTN_OUTLINE,
            rc: cell(x1, lib_row2),
        },
        UiButton {
            id: BTN_STARTUP,
            label: crate::settings::startup_toggle_label(startup).into(),
            style: BTN_OUTLINE,
            rc: Rect {
                left: margin,
                top: startup_row,
                right: margin + content_w,
                bottom: startup_row + sc(BTN_H),
            },
        },
        UiButton {
            id: BTN_SCAN_GENTLE,
            label: "Gentle".into(),
            style: crate::settings::scan_segment_style(
                app_support::config::SCAN_SPEED_GENTLE,
                cur_scan,
            ),
            rc: seg(0),
        },
        UiButton {
            id: BTN_SCAN_BALANCED,
            label: "Balanced".into(),
            style: crate::settings::scan_segment_style(
                app_support::config::SCAN_SPEED_BALANCED,
                cur_scan,
            ),
            rc: seg(1),
        },
        UiButton {
            id: BTN_SCAN_TURBO,
            label: "Turbo".into(),
            style: crate::settings::scan_segment_style(
                app_support::config::SCAN_SPEED_TURBO,
                cur_scan,
            ),
            rc: seg(2),
        },
        UiButton {
            id: BTN_BACK,
            label: "Back".into(),
            style: BTN_OUTLINE,
            rc: cell(x0, footer_top),
        },
        UiButton {
            id: BTN_OPEN_LOG,
            label: "Open log folder".into(),
            style: BTN_OUTLINE,
            rc: cell(x1, footer_top),
        },
    ]);
    l
}

// ---------------------------------------------------------------------------
// Text-rect tables (`go\window.go:666-727`, `:1602-1635`).
// ---------------------------------------------------------------------------

/// `mainTextRects` (`go\window.go:674`): the stable audit id for every string
/// the main view paints, in the same rect the paint path draws it in.
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
            id: "main.wordmark".into(),
            rc: Rect {
                left: l.header.left + sc(MAIN_MARGIN) + sc(MARK_TILE_L) + sc(SP_3),
                top: l.header.top + sc(SP_3),
                right: l.header.right - sc(MAIN_MARGIN),
                bottom: l.header.top + sc(SP_3) + sc(26),
            },
        },
        NamedTextRect {
            id: "main.subtitle".into(),
            rc: Rect {
                left: l.header.left + sc(MAIN_MARGIN) + sc(MARK_TILE_L) + sc(SP_3),
                top: l.header.top + sc(SP_3) + sc(26),
                right: l.header.right - sc(MAIN_MARGIN),
                bottom: l.header.bottom - sc(SP_2),
            },
        },
        NamedTextRect {
            id: "main.section".into(),
            rc: Rect {
                left: l.section.left,
                top: l.section.top,
                right: l.section.right - sc(18) - sc(STROKE_FOCUS) - sc(SP_2),
                bottom: l.section.bottom,
            },
        },
        NamedTextRect {
            id: "main.badge".into(),
            rc: Rect {
                left: l.section.right - sc(18) - sc(STROKE_FOCUS),
                top: l.section.top + (l.section.bottom - l.section.top - sc(18)) / 2,
                right: l.section.right,
                bottom: l.section.top + (l.section.bottom - l.section.top - sc(18)) / 2 + sc(18),
            },
        },
    ];
    let _ = dir_count;
    if l.rows.is_empty() {
        out.push(NamedTextRect {
            id: "main.empty".into(),
            rc: l.card,
        });
    } else {
        for (i, r) in l.rows.iter().enumerate() {
            let text_right = r.right - sc(SP_3);
            out.push(NamedTextRect {
                id: format!("main.row-{i}"),
                rc: Rect {
                    left: r.left + sc(SP_3),
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
                    left: l.card.left + sc(SP_3),
                    top: l.card.bottom - sc(OVERFLOW_H_L),
                    right: l.card.right - sc(SP_3),
                    bottom: l.card.bottom - sc(SP_1),
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
        id: "main.status".into(),
        rc: l.status,
    });
    let _ = (cw, ch);
    out
}

/// `settingsTextRects` (`go\window.go:711`).
#[must_use]
pub fn settings_text_rects(cw: i32, ch: i32, dpi: i32, l: &SettingsLayout) -> Vec<NamedTextRect> {
    let mut out = l.labels.clone();
    out.push(NamedTextRect {
        id: "settings.hint".into(),
        rc: l.hint,
    });
    out.push(NamedTextRect {
        id: "settings.dead".into(),
        rc: l.dead,
    });
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
    let _ = (cw, ch, dpi);
    out
}

// ---------------------------------------------------------------------------
// Focus model (`go\window.go:1244-1316`).
// ---------------------------------------------------------------------------

/// `mainFocusOrder` (`go\window.go:1244`), in visual order: the tab strip first
/// (it is the topmost control row), then the action grid row by row.
#[must_use]
pub fn main_focus_order() -> Vec<i32> {
    vec![
        BTN_CARD,
        BTN_SETTINGS,
        BTN_SCAN,
        BTN_REFRESH,
        BTN_ADD,
        BTN_REMOVE,
        BTN_REPORT,
        BTN_CLOSE,
    ]
}

/// `settingsFocusOrder` (`go\window.go:1249`), in visual order.
#[must_use]
pub fn settings_focus_order() -> Vec<i32> {
    vec![
        BTN_CARD,
        BTN_SETTINGS,
        BTN_CLEAR_CACHE,
        BTN_CLEAR_INDEX,
        BTN_EXPORT,
        BTN_IMPORT,
        BTN_STARTUP,
        BTN_SCAN_GENTLE,
        BTN_SCAN_BALANCED,
        BTN_SCAN_TURBO,
        BTN_BACK,
        BTN_OPEN_LOG,
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

/// `cardFocusOrder` (`go\window.go:1256`), in visual order: the strip, the way
/// back, then the two preset rows.
#[must_use]
pub fn card_focus_order() -> Vec<i32> {
    let mut order = vec![BTN_CARD, BTN_SETTINGS, BTN_CARD_BACK];
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

/// `mainFocusRect` (`go\window.go:1303`). The tab strip's two segments are
/// controls in the same table as the action grid, so one lookup covers every
/// focus stop -- there is no second, rect-less focus vocabulary any more.
#[must_use]
pub fn main_focus_rect(l: &UiLayout, id: i32) -> Option<Rect> {
    l.btns.iter().find(|b| b.id == id).map(|b| b.rc)
}

// ---------------------------------------------------------------------------
// Card view (`go\window.go:1453-1731`).
// ---------------------------------------------------------------------------

/// `cardPresetForButton` (`go\window.go:1463`).
#[must_use]
pub fn card_preset_for_button(id: i32) -> Option<(i32, String)> {
    let (dets, states) = presence::cardspec::card_presets();
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

/// `cardPresetStyle` (`go\window.go:1476`): the active preset is a LIT segment,
/// not an accent fill. Each view has exactly one primary control, and a preset
/// selection is not an action.
#[must_use]
pub fn card_preset_style(line: i32, id: &str, spec: &CardSpec) -> i32 {
    let spec = spec.normalized();
    if (line == 0 && id == spec.details) || (line == 1 && id == spec.state) {
        return BTN_TAB_SEL;
    }
    BTN_TAB
}

/// `computeCardLayout` (`go\window.go:1546`). The spec only flips button styles.
#[must_use]
pub fn compute_card_layout(cw: i32, ch: i32, dpi: i32, spec: &CardSpec) -> CardLayout {
    let sc = |v: i32| scale_px(dpi, v);
    let margin = sc(MAIN_MARGIN);
    let content_w = (cw - margin * 2).max(sc(100));

    let mut l = CardLayout::default();

    // Same stack, same grid: header band, tab strip, then the preview panel and
    // the two preset rows, each under its own micro-label.
    let label_top = sc(TAB_TOP_L) + sc(TAB_H_L) + sc(SP_3);
    let label_h = sc(SECTION_H_L);
    let after_label = label_h + sc(SP_2);

    let prev_top = label_top + after_label;
    let prev_h = sc(254);
    let prev_pad = sc(SP_3);
    let (row_h, row_pitch) = (sc(20), sc(22));
    l.preview = Rect {
        left: margin,
        top: prev_top,
        right: margin + content_w,
        bottom: prev_top + prev_h,
    };
    for i in 0..5 {
        let top = prev_top + prev_pad + i * row_pitch;
        l.rows.push(Rect {
            left: margin + prev_pad,
            top,
            right: margin + content_w - prev_pad,
            bottom: top + row_h,
        });
    }

    let det_label = prev_top + prev_h + sc(SP_4);
    let det_row = det_label + after_label;
    let state_label = det_row + sc(BTN_H) + sc(SP_4);
    let state_row = state_label + after_label;
    let hint_top = state_row + sc(BTN_H) + sc(SP_2);
    l.labels = vec![
        NamedTextRect {
            id: "card.preview-label".into(),
            rc: Rect {
                left: margin,
                top: label_top,
                right: margin + content_w,
                bottom: label_top + label_h,
            },
        },
        NamedTextRect {
            id: "card.line1".into(),
            rc: Rect {
                left: margin,
                top: det_label,
                right: margin + content_w,
                bottom: det_label + label_h,
            },
        },
        NamedTextRect {
            id: "card.line2".into(),
            rc: Rect {
                left: margin,
                top: state_label,
                right: margin + content_w,
                bottom: state_label + label_h,
            },
        },
    ];
    l.hint = Rect {
        left: margin,
        top: hint_top,
        right: margin + content_w,
        bottom: hint_top + sc(SP_4),
    };

    // The preset rows are a segmented control per line: mutually exclusive
    // choices, one row of equal cells, so they carry the segment anatomy rather
    // than the accent fill (which belongs to the view's single primary action).
    let seg_gap = sc(SP_2);
    let seg = |top: i32, line: i32, presets: &[CardPreset]| -> Vec<UiButton> {
        let n = presets.len() as i32;
        let w = (content_w - seg_gap * (n - 1)) / n;
        let mut out = Vec::with_capacity(presets.len());
        for (i, p) in presets.iter().enumerate() {
            let i = i as i32;
            let left = margin + i * (w + seg_gap);
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
                    right: if i == n - 1 { margin + content_w } else { left + w },
                    bottom: top + sc(BTN_H),
                },
            });
        }
        out
    };
    let (dets, states) = presence::cardspec::card_presets();
    l.btns = tab_strip_buttons(cw, dpi);
    l.btns.extend(seg(det_row, 0, &dets));
    l.btns.extend(seg(state_row, 1, &states));

    let status_h = sc(STATUS_H_L);
    let status_top = ch - sc(BOTTOM_L) - status_h;
    let footer_top = status_top - sc(SP_3) - sc(BTN_H);
    l.status = Rect {
        left: margin,
        top: status_top,
        right: margin + content_w,
        bottom: status_top + status_h,
    };
    let col_w = (content_w - sc(BTN_GAP_L)) / 2;
    l.btns.push(UiButton {
        id: BTN_CARD_BACK,
        label: "Back".into(),
        style: BTN_OUTLINE,
        rc: Rect {
            left: margin,
            top: footer_top,
            right: margin + col_w,
            bottom: footer_top + sc(BTN_H),
        },
    });
    l
}

/// `cardTextRects` (`go\window.go:1606`).
#[must_use]
pub fn card_text_rects(cw: i32, ch: i32, dpi: i32, l: &CardLayout) -> Vec<NamedTextRect> {
    let sc = |v: i32| scale_px(dpi, v);
    let margin = sc(MAIN_MARGIN);
    let content_w = (cw - margin * 2).max(sc(100));
    let mut out = l.labels.clone();
    for (i, r) in l.rows.iter().enumerate() {
        out.push(NamedTextRect {
            id: format!("card.row-{i}"),
            rc: *r,
        });
    }
    {
        // The tooltip line is the sixth preview row: the panel reserves five
        // rows, and `paint_preview_rows` draws the tooltip into the sixth slot
        // the layout's pitch defines.
        let (prev_pad, row_h, row_pitch) = (sc(SP_3), sc(20), sc(22));
        let tip_top = l.preview.top + prev_pad + 5 * row_pitch;
        out.push(NamedTextRect {
            id: "card.row-5".into(),
            rc: Rect {
                left: l.preview.left + prev_pad,
                top: tip_top,
                right: l.preview.right - prev_pad,
                bottom: tip_top + row_h,
            },
        });
    }
    out.push(NamedTextRect {
        id: "card.hint".into(),
        rc: l.hint,
    });
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
    let _ = (ch, content_w);
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
pub fn format_scan_count(n: i64) -> String {
    if (-100_000..100_000).contains(&n) {
        return n.to_string();
    }
    format_scan_count_unit(n)
}

/// `formatScanCountUnit` (`go\window.go:1825`).
#[must_use]
pub fn format_scan_count_unit(n: i64) -> String {
    let f = n as f64;
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
pub fn format_scan_count_compact(n: i64) -> String {
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
///
/// Reads the per-KEY counters (`keys_fetched` / `keys_not_found` / `keys_failed`),
/// not the per-FILE `stats.not_found` the completion log line carries below: the
/// two agree only when a file yields exactly one key. (`scan.rs:180`/`183` carries
/// a third meaning under the same name — `not_found` = `files_pending`, the
/// index-only pending count.) Done-text wording is unchanged.
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
pub fn scan_cancelled_text(done: i64, total: i64) -> String {
    fit_scan_status(&format!(
        "Scan cancelled - {}/{} keys done, progress kept",
        format_scan_count(done),
        format_scan_count(total)
    ))
}

/// `scanButtonLabel` (`go\window.go:1968`).
#[must_use]
pub fn scan_button_label(active: bool) -> &'static str {
    if active { "Cancel" } else { "Scan" }
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

/// The worker-published scan snapshot (`go\window.go:161-163`): `scanMu` plus
/// the progress/stats/err package vars. The worker owns THIS lock and posts
/// `WM_APP_SCAN_PROGRESS` / `WM_APP_SCAN_DONE`; it must never lock the
/// process-wide `UI` mutex, which tests hold while a worker runs.
#[derive(Clone, Debug, Default)]
pub struct ScanSnapshot {
    pub progress: Progress,
    pub stats: Stats,
    pub err: Option<UiScanError>,
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
pub fn scan_key_fraction(done: i64, total: i64) -> Option<f64> {
    if total <= 0 {
        return None;
    }
    let done = done.clamp(0, total);
    Some(done as f64 / total as f64)
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
pub const MIN_KEYS_FOR_ETA: i64 = 5;

/// `scanObservedDone` (`go\window.go:2151`).
#[must_use]
pub fn scan_observed_done(p: &Progress) -> i64 {
    (p.keys_done - p.keys_cached).max(0)
}

/// `scanETA` (`go\window.go:2168`).
#[must_use]
pub fn scan_eta(p: &Progress, observed_base: i64, resolve_elapsed: Duration) -> Option<Duration> {
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
    let secs = resolve_elapsed.as_secs_f64() * remaining as f64 / observed as f64;
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
    observed_base: i64,
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
    observed_base: i64,
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
    observed_base: i64,
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
pub fn scan_bar_model(active: bool, done: i64, total: i64) -> (bool, bool, f64) {
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
    if d != 0 { d as i32 } else { 96 }
}

/// `systemDpi` (`go\window.go:3459`).
#[must_use]
pub fn system_dpi() -> i32 {
    // SAFETY: GetDpiForSystem takes no arguments.
    let d = unsafe { crate::win32::GetDpiForSystem() };
    if d != 0 { d as i32 } else { 96 }
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
            crate::win32::DEFAULT_CHARSET,
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
        if r == 0 { None } else { Some(sz.cx) }
    }
}

/// `scanLiveStatusText` (`go\window.go:2404`).
#[must_use]
pub fn scan_live_status_text(
    p: &Progress,
    elapsed: Duration,
    resolve_elapsed: Duration,
    observed_base: i64,
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
    if let Ok(d) = std::env::var("LOCALAPPDATA")
        && !d.is_empty()
    {
        return std::path::Path::new(&d).join("Potflix").join("inventory");
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

static APP_LOG: std::sync::OnceLock<&'static app_support::debuglog::Logger> =
    std::sync::OnceLock::new();

/// Install the process log the UI writes to (`appLog = log`).
///
/// Takes a `&'static` reference -- the same logger the tray holds via
/// `tray::set_logger` -- so both seams write to one logger with no second
/// file handle. Returns the rejected logger if one is already installed.
pub fn set_app_log(log: &'static app_support::debuglog::Logger) -> Result<(), &'static app_support::debuglog::Logger> {
    APP_LOG.set(log)
}

/// `appLogf` (`go\window.go:3491`).
pub fn app_logf(message: &str) {
    if let Some(l) = APP_LOG.get() {
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
    /// First visible folder index in the Main list (`0` when the list fits).
    /// Selection (`sel_row`) and hover (`hover_row`) stay ABSOLUTE folder
    /// indices; only the viewport moves, so scrolling never retargets them.
    pub scroll_top: i32,

    pub scan_active: bool,
    pub scan_cancel: Option<library::Cancel>,
    pub scan_force: bool,
    pub scan_progress: Progress,
    pub scan_stats: Stats,
    pub scan_err: Option<UiScanError>,
    pub scan_start: Instant,
    pub scan_last_paint: Instant,
    pub scan_resolve_start: Option<Instant>,
    pub scan_resolve_base: i64,
    /// The worker-published snapshot slot: the worker writes here under this
    /// lock and the wndproc arms publish it onto `UiState`.
    pub scan_shared: Arc<Mutex<ScanSnapshot>>,

    pub startup_state: bool,
    pub scan_speed_state: String,
    pub card_spec_state: CardSpec,
    pub dead_cache_count: i32,

    pub preview: Option<Activity>,
    pub ui_icon: usize,
    pub main_hwnd: usize,
    pub ui_centered: bool,
    pub last_mx: i32,
    pub last_my: i32,
    pub have_mouse: bool,
    /// A blocking UI action requested by the last dispatch, run by the wndproc
    /// AFTER the `UI` guard is released (`run_blocking_ui`); see [`BlockingUi`].
    pub pending_blocking: Option<BlockingUi>,
    pub report_done_text: Arc<Mutex<String>>,

    /// Injectable seams (`go\window.go:2581-2586`, `:1055-1061`).
    pub arm_timer: Arc<dyn Fn(HWND) + Send + Sync>,
    pub kill_timer: Arc<dyn Fn(HWND) + Send + Sync>,
    #[allow(clippy::type_complexity)]
    pub create_issue:
        Arc<dyn Fn(&repair::report::Report) -> (String, Option<repair::Error>) + Send + Sync>,
    pub open_url: Arc<dyn Fn(&str) -> bool + Send + Sync>,
    /// `startLibraryScan` runner seam (`go\window.go:2662`
    /// `library.ScanWithOptions`): the default runs the real scan; tests inject
    /// a fake so no network is touched.
    #[allow(clippy::type_complexity)]
    pub scan_run:
        Arc<dyn Fn(&library::Cancel, library::Options) -> Result<library::Stats, library::Error>
            + Send
            + Sync>,
    /// `startLibraryScan` dirs seam (`go\window.go:2646` `loadConfig().Dirs`):
    /// the default reads the live store; tests inject a temp dir.
    pub scan_dirs: Arc<dyn Fn() -> Vec<String> + Send + Sync>,
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
            scroll_top: 0,
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
            scan_shared: Arc::new(Mutex::new(ScanSnapshot::default())),
            startup_state: false,
            scan_speed_state: app_support::config::SCAN_SPEED_TURBO.to_string(),
            card_spec_state: CardSpec::default(),
            dead_cache_count: 0,
            preview: None,
            ui_icon: 0,
            main_hwnd: 0,
            ui_centered: false,
            last_mx: 0,
            last_my: 0,
            have_mouse: false,
            pending_blocking: None,
            report_done_text: Arc::new(Mutex::new(String::new())),
            arm_timer: Arc::new(default_arm_timer),
            kill_timer: Arc::new(default_kill_timer),
            create_issue: Arc::new(default_create_issue),
            open_url: Arc::new(default_open_url),
            scan_run: Arc::new(default_scan_run),
            scan_dirs: Arc::new(default_scan_dirs),
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

/// `library.ScanWithOptions` default for the [`UiState::scan_run`] seam.
fn default_scan_run(
    cancel: &library::Cancel,
    opts: library::Options,
) -> Result<library::Stats, library::Error> {
    library::scan_with_options(cancel, opts)
}

/// `loadConfig().Dirs` default for the [`UiState::scan_dirs`] seam.
fn default_scan_dirs() -> Vec<String> {
    app_support::config::load_config().dirs
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

/// A UI action that BLOCKS: it opens a modal dialog (`SHBrowseForFolderW`,
/// `MessageBoxW`, `GetOpen/SaveFileNameW`) or shells out (`ShellExecuteW`) /
/// shows a window (`ShowWindow`). Each pumps the message queue, so the OS
/// re-enters `main_wnd_proc` on this same UI thread; its handlers
/// (`paint_main_window`, ...) re-lock `UI` via `ui()`. The wndproc therefore
/// returns one of these out of the `ui()` scope and runs it with the guard
/// RELEASED (`run_blocking_ui`): under the non-reentrant `UI` guard it
/// self-deadlocks the UI thread, the shipped "(Not Responding)" hang.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum BlockingUi {
    AddDir,
    ClearMetadata,
    ClearIndex,
    ExportBundle,
    ImportBundle,
    OpenLogFolder,
    HideWindow,
}

impl UiState {
    /// `invalidateMain` (`go\window.go:1745`).
    pub fn invalidate(&self) {
        if self.main_hwnd != 0 {
            // SAFETY: InvalidateRect only marks the window.
            unsafe {
                crate::win32::InvalidateRect(self.main_hwnd as HWND, std::ptr::null(), 0);
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

    /// Rows per wheel notch (the `SPI_GETWHEELSCROLLLINES` default): one notch
    /// moves three folder rows.
    pub const WHEEL_SCROLL_ROWS: i32 = 3;

    /// Scroll the Main folder list by `rows` (positive = toward the end),
    /// clamped to the real scroll range; clears the transient row hover so a
    /// highlight never silently points at a different folder after the
    /// viewport moves. Selection (`sel_row`) is an absolute folder index and
    /// survives scrolling. Returns whether the viewport moved. Only
    /// `InvalidateRect` (asynchronous, never dispatches `WM_PAINT`) runs here,
    /// so this is NOT a re-entrant site: no guard or regression-test change.
    pub fn scroll_list(&mut self, hwnd: HWND, rows: i32) -> bool {
        if self.cur_view != View::Main || rows == 0 {
            return false;
        }
        let rc = get_client_rect(hwnd);
        let (cw, ch) = (rc.right - rc.left, rc.bottom - rc.top);
        if cw <= 0 || ch <= 0 {
            return false;
        }
        let total = self.ui_dirs.len() as i32;
        let want = self.scroll_top.saturating_add(rows);
        let top = compute_layout_total(total, cw, ch, window_dpi(hwnd), want).list_top;
        if top == self.scroll_top {
            return false;
        }
        self.scroll_top = top;
        self.hover_row = -1;
        self.hover_x = false;
        self.invalidate();
        true
    }

    /// One viewport of folder rows at the window's DPI: the PageUp/PageDown
    /// step.
    fn page_rows(&self, hwnd: HWND) -> i32 {
        let rc = get_client_rect(hwnd);
        let (cw, ch) = (rc.right - rc.left, rc.bottom - rc.top);
        if cw <= 0 || ch <= 0 {
            return 1;
        }
        compute_layout_total(
            self.ui_dirs.len() as i32,
            cw,
            ch,
            window_dpi(hwnd),
            self.scroll_top,
        )
        .list_capacity
        .max(1)
    }

    /// `layoutForHwnd` (`go\window.go:731`).
    #[must_use]
    pub fn layout(&self, hwnd: HWND) -> Option<UiLayout> {
        let rc = get_client_rect(hwnd);
        let (cw, ch) = (rc.right - rc.left, rc.bottom - rc.top);
        if cw <= 0 || ch <= 0 {
            return None;
        }
        Some(compute_layout_from(&self.ui_dirs, cw, ch, window_dpi(hwnd), self.scroll_top))
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
                // Absolute: the viewport row plus the scroll offset, so the
                // highlight tracks the folder, not the slot.
                let want = if nr >= 0 { l.list_top + nr } else { -1 };
                if nb == self.hover_btn && want == self.hover_row && nx == self.hover_x {
                    return false;
                }
                self.hover_btn = nb;
                self.hover_row = want;
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
                        self.pressed_x = l.list_top + r;
                    }
                }
                self.invalidate();
            }
        }
    }

    /// `releaseAt` (`go\window.go:852`).
    ///
    /// Returns a [`BlockingUi`] the wndproc must run with `UI` RELEASED: the
    /// dispatched action opens a modal dialog / shows a window, which pumps the
    /// queue and re-enters `main_wnd_proc`; holding the non-reentrant `UI`
    /// guard across it self-deadlocks the UI thread.
    #[must_use]
    pub fn release_at(&mut self, hwnd: HWND, x: i32, y: i32) -> Option<BlockingUi> {
        match self.cur_view {
            View::Card => {
                let Some(l) = self.layout_card(hwnd) else {
                    self.pressed_btn = 0;
                    self.pressed_x = -1;
                    return None;
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
                    return None;
                }
                self.invalidate();
                None
            }
            View::Settings => {
                let Some(l) = self.layout_settings(hwnd) else {
                    self.pressed_btn = 0;
                    self.pressed_x = -1;
                    return None;
                };
                let pb = self.pressed_btn;
                self.pressed_btn = 0;
                self.pressed_x = -1;
                if pb != 0 {
                    if button_at_point(&l.btns, x, y) == pb {
                        self.focus_id = pb;
                        return self.do_settings_action(hwnd, pb);
                    }
                    self.invalidate();
                    return None;
                }
                self.invalidate();
                None
            }
            View::Main => {
                let Some(l) = self.layout(hwnd) else {
                    self.pressed_btn = 0;
                    self.pressed_x = -1;
                    return None;
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
                        return None;
                    }
                    if pb == BTN_CARD {
                        if pt_in_rect(l.card_link, x, y) {
                            self.enter_card();
                        } else {
                            self.invalidate();
                        }
                        return None;
                    }
                    if button_at_point(&l.btns, x, y) == pb {
                        self.focus_id = pb;
                        return self.do_button_action(hwnd, pb);
                    }
                    self.invalidate();
                    return None;
                }
                if px >= 0 {
                    if self.scan_active {
                        self.set_status_text(SCAN_BUSY_TEXT);
                        return None;
                    }
                    let r = row_at_point(&l.rows, x, y);
                    if l.list_top + r == px
                        && (r as usize) < l.xbtns.len()
                        && pt_in_rect(l.xbtns[r as usize], x, y)
                    {
                        self.remove_dir_at(px);
                    } else {
                        self.invalidate();
                    }
                    return None;
                }
                let r = row_at_point(&l.rows, x, y);
                if r >= 0 {
                    let abs = l.list_top + r;
                    if self.sel_row != abs {
                        self.sel_row = abs;
                        self.invalidate();
                    }
                    return None;
                }
                let id = button_at_point(&l.btns, x, y);
                if id != 0 {
                    self.focus_id = id;
                    return self.do_button_action(hwnd, id);
                }
                None
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
    ///
    /// Returns a [`BlockingUi`] the wndproc must run with `UI` RELEASED; `None`
    /// when the action is pure state work.
    #[must_use]
    pub fn do_button_action(&mut self, hwnd: HWND, id: i32) -> Option<BlockingUi> {
        let reason = self.scan_blocked_reason(id);
        if !reason.is_empty() {
            self.set_status_text(reason);
            return None;
        }
        match id {
            BTN_ADD => Some(BlockingUi::AddDir),
            BTN_REMOVE => {
                self.on_remove_selected();
                None
            }
            BTN_SCAN => {
                if self.scan_active {
                    self.cancel_library_scan();
                } else {
                    self.start_library_scan(hwnd, false);
                }
                None
            }
            BTN_REFRESH => {
                self.start_library_scan(hwnd, true);
                None
            }
            BTN_REPORT => {
                let path = repair_report_path();
                self.on_report_at(hwnd, &path);
                None
            }
            BTN_CLOSE => Some(BlockingUi::HideWindow),
            _ => None,
        }
    }

    /// `doSettingsAction` (`go\window.go:1395`).
    ///
    /// Returns a [`BlockingUi`] the wndproc must run with `UI` RELEASED.
    #[must_use]
    pub fn do_settings_action(&mut self, _hwnd: HWND, id: i32) -> Option<BlockingUi> {
        match id {
            BTN_BACK => {
                self.enter_main();
                None
            }
            BTN_SCAN_GENTLE | BTN_SCAN_BALANCED | BTN_SCAN_TURBO => {
                let profile = crate::settings::scan_profile_for_button(id);
                if !profile.is_empty() {
                    self.on_select_scan_speed(profile);
                }
                None
            }
            BTN_CLEAR_CACHE => Some(BlockingUi::ClearMetadata),
            BTN_CLEAR_INDEX => Some(BlockingUi::ClearIndex),
            BTN_EXPORT => Some(BlockingUi::ExportBundle),
            BTN_IMPORT => Some(BlockingUi::ImportBundle),
            BTN_STARTUP => {
                self.on_toggle_startup();
                None
            }
            BTN_OPEN_LOG => Some(BlockingUi::OpenLogFolder),
            _ => None,
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
    ///
    /// Returns a [`BlockingUi`] the wndproc must run with `UI` RELEASED.
    #[must_use]
    pub fn activate_focused(&mut self, hwnd: HWND, id: i32) -> Option<BlockingUi> {
        self.focus_id = id;
        match self.cur_view {
            View::Settings => self.do_settings_action(hwnd, id),
            View::Card => {
                self.do_card_action(hwnd, id);
                None
            }
            View::Main => match id {
                BTN_SETTINGS => {
                    self.enter_settings();
                    None
                }
                BTN_CARD => {
                    self.enter_card();
                    None
                }
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
            VK_PRIOR => self.scroll_list(hwnd, -self.page_rows(hwnd)),
            VK_NEXT => self.scroll_list(hwnd, self.page_rows(hwnd)),
            VK_HOME => self.scroll_list(hwnd, -i32::MAX),
            VK_END => self.scroll_list(hwnd, i32::MAX),
            VK_RIGHT | VK_DOWN => {
                self.focus_id = move_focus(self.focus_id, 1, &focus_order_for_view(self.cur_view));
                true
            }
            VK_RETURN | VK_SPACE => {
                if self.focus_id == 0 {
                    return false;
                }
                let id = self.focus_id;
                // Stash the blocking action for the wndproc to run with `UI`
                // RELEASED (`run_blocking_ui`); never run it under this guard.
                self.pending_blocking = self.activate_focused(hwnd, id);
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

    /// `onAddDir` (`go\window.go:1757`), post-dialog half only: the modal
    /// `browse_for_folder` runs OUTSIDE the `UI` guard in `run_blocking_ui`.
    fn apply_add_dir(&mut self, path: &str) {
        let mut cfg = app_support::config::load_config();
        if !cfg.add_dir(path) {
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
        let (dets, states) = presence::cardspec::card_presets();
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

    /// `onSelectScanSpeed` (`go\settings_sections.go:177`). Persistence and
    /// live rate-profile application live in the settings seam; this method
    /// keeps the view's own state, status line, and log.
    pub fn on_select_scan_speed(&mut self, profile: &str) {
        let profile = app_support::config::normalize_scan_speed(profile);
        self.scan_speed_state = profile.to_string();
        match crate::settings::on_select_scan_speed(&app_support::config::config_path(), profile) {
            Ok(status) => {
                self.set_status_text(status);
                app_logf(&format!("settings: scan speed {profile}"));
            }
            Err(e) => {
                app_logf(&format!("settings: scan speed save failed: {e}"));
                self.set_status_text("Save failed.");
            }
        }
    }

    /// `resetScanSnapshot` (`go\window.go:2447`).
    pub fn reset_scan_snapshot(&mut self) {
        self.scan_progress = Progress::default();
        self.scan_resolve_start = None;
        self.scan_resolve_base = 0;
    }

    /// Publish the worker's progress snapshot onto the UI state. The
    /// `WM_APP_SCAN_PROGRESS` arm runs this before `refresh_scan_live_text`,
    /// exactly as Go's arm reads the mutex-guarded package var.
    pub fn publish_scan_progress(&mut self) {
        self.scan_progress = self
            .scan_shared
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .progress
            .clone();
    }

    /// Publish the worker's terminal snapshot onto the UI state. The
    /// `WM_APP_SCAN_DONE` arm runs this before `on_scan_done`, exactly as Go's
    /// arm reads the mutex-guarded package vars.
    pub fn publish_scan_done(&mut self) {
        let snap = self
            .scan_shared
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .clone();
        self.scan_stats = snap.stats;
        self.scan_err = snap.err;
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
        } else {
            // `onScanDone` (`go\window.go:2749`). The `unresolved-at-scan-time` field
            // is the per-FILE `stats.not_found`, not the per-KEY `keys_not_found`
            // the done-text gate above reads — the two agree only when a file
            // yields exactly one key (`scan.rs:180`/`183` carries a third meaning
            // under the same name: `not_found` = `files_pending`). The summary is
            // sampled before the async catch-up pass runs, so an honest nonzero
            // here can coexist with a later successful fetch. Deliberate
            // departure from Go's `not-found` field name, which reads as a
            // verdict; this one states the sampling instant.
            app_logf(&format!(
                "{lname}: complete files={} skipped={} unique={} resolved={} cached={} fetched={} unresolved-at-scan-time={} errors={} denied={} foldersScanned={} foldersSkipped={} filesSkipped={}",
                stats.files, stats.skipped, stats.unique, stats.resolved, stats.cached, stats.fetched, stats.not_found, stats.errors, stats.denied, stats.folders_scanned, stats.folders_skipped, stats.files_skipped
            ));
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
        let dirs = (self.scan_dirs)();
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
        // Worker thread: it may never touch the UI nor lock the process-wide
        // `UI` mutex; it publishes a snapshot and posts async messages
        // (production wiring posts to `hwnd`).
        let cancel_clone = cancel.clone();
        let shared = self.scan_shared.clone();
        let run = self.scan_run.clone();
        let hwnd_raw = hwnd as usize;
        std::thread::spawn(move || {
            let shared_progress = shared.clone();
            let opts = build_scan_options(
                dirs,
                force,
                Some(Arc::new(move |p: Progress| {
                    shared_progress
                        .lock()
                        .unwrap_or_else(std::sync::PoisonError::into_inner)
                        .progress = p;
                    if hwnd_raw != 0 {
                        // SAFETY: async post only; the worker never touches a control.
                        unsafe {
                            crate::win32::PostMessageW(
                                hwnd_raw as HWND,
                                WM_APP_SCAN_PROGRESS,
                                0,
                                0,
                            );
                        }
                    }
                })),
            );
            let outcome = run(&cancel_clone, opts);
            {
                let mut snap = shared
                    .lock()
                    .unwrap_or_else(std::sync::PoisonError::into_inner);
                match outcome {
                    Ok(stats) => {
                        snap.stats = stats;
                        snap.err = None;
                    }
                    Err(e) => {
                        let message = e.message().to_string();
                        // `library::Error` carries only a message and no
                        // cancel flag, so a failed run is a cancel exactly
                        // when the cancel fired.
                        let cancelled = cancel_clone.is_cancelled();
                        snap.stats = Stats {
                            cancelled,
                            ..Stats::default()
                        };
                        snap.err = Some(if cancelled {
                            UiScanError::cancelled()
                        } else {
                            UiScanError::other(&message)
                        });
                    }
                }
            }
            // SAFETY: async post only; the worker never touches a control.
            unsafe {
                crate::win32::PostMessageW(hwnd_raw as HWND, WM_APP_SCAN_DONE, 0, 0);
            }
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
        let hwnd_raw = hwnd as usize;
        std::thread::spawn(move || {
            let (url, cerr) = create(&report);
            let text = report_issue_status(&url, cerr.as_ref(), &*open);
            *done
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner) = text;
            if hwnd_raw != 0 {
                // SAFETY: async post only; the worker never touches a control.
                unsafe {
                    crate::win32::PostMessageW(hwnd_raw as HWND, WM_APP_REPORT_DONE, 0, 0);
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

    /// `onClearMetadata` (`go\settings_sections.go:594`), post-confirm half only:
    /// the modal `confirm_clear` runs OUTSIDE the `UI` guard in `run_blocking_ui`.
    fn apply_clear_metadata(&mut self) {
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

    /// `onClearIndex` (`go\settings_sections.go:611`), post-confirm half only:
    /// the modal `confirm_clear` runs OUTSIDE the `UI` guard in `run_blocking_ui`.
    fn apply_clear_index(&mut self) {
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

    /// `onExportBundle` (`go\settings_sections.go:628`), post-dialog half only:
    /// the modal `save_bundle_dialog` runs OUTSIDE the `UI` guard in
    /// `run_blocking_ui`.
    fn apply_export_bundle(&mut self, dest: &str) {
        match crate::settings::export_bundle_to(
            &crate::settings::metadata_dir(),
            dest,
            metadata::cache::CACHE_SCHEMA,
            crate::settings::APP_VERSION,
        ) {
            Ok(n) => {
                app_logf(&format!("library: exported (entries={n})"));
                self.set_status_text(crate::settings::export_bundle_status(n, dest));
            }
            Err(e) => {
                app_logf(&format!("library: export failed: {}", e.message()));
                self.set_status_text(fit_scan_status(&format!("Export failed: {}", e.message())));
            }
        }
    }

    /// `onImportBundle` (`go\settings_sections.go:647`), post-dialog half only:
    /// the modal `open_bundle_dialog` runs OUTSIDE the `UI` guard in
    /// `run_blocking_ui`.
    fn apply_import_bundle(&mut self, hwnd: HWND, src: &str) {
        match crate::settings::import_bundle_from(
            &crate::settings::metadata_dir(),
            src,
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

/// The app's one font factory: the brand UI face (Inter, `design\fonts\
/// Inter-Variable.ttf`, embedded and installed by `brand::install_fonts`) at a
/// negative pixel height, `OUT_TT_PRECIS` + `CLEARTYPE_QUALITY`.
///
/// The port originally asked for Segoe UI at `OUT_DEFAULT_PRECIS` +
/// `DEFAULT_QUALITY` ("appearance of the font does not matter"). The identity
/// pass ships the app's own face, so the client area now renders in it -- and
/// the quality pair keeps subpixel antialiasing on the TrueType outline.
/// (learn.microsoft.com/windows/win32/api/wingdi/nf-wingdi-createfontw,
/// accessed 2026-10-14.)
#[must_use]
pub fn create_ui_font(height: i32, weight: i32) -> Gdi::HFONT {
    brand::ui(height.abs(), weight)
}

// ---------------------------------------------------------------------------
// Antialiased geometry (GDI+).
//
// The whole client area is already painted into one memory DC and blitted, so
// one GDI+ `Graphics` wrapped around that DC's HDC smooths every rounded fill
// and ring in the frame. It lives in a thread-local because the paint is
// single-threaded on the UI thread and because `fill_round_rect` is called from
// a dozen paint sites that have no business threading a context through.
// ---------------------------------------------------------------------------

thread_local! {
    /// The current frame's `GpGraphics*` as a raw address (0 = none).
    static PAINT_GRAPHICS: std::cell::Cell<isize> = const { std::cell::Cell::new(0) };
}

/// Start GDI+ once per process. `false` (no GDI+ available) is a supported
/// state: every helper below falls back to plain GDI.
fn gdiplus_start() -> bool {
    use std::sync::OnceLock;
    static STARTED: OnceLock<bool> = OnceLock::new();
    *STARTED.get_or_init(|| {
        let mut token: usize = 0;
        let input = crate::win32::GdiplusStartupInput::default();
        // SAFETY: `input`/`token` outlive the call; GDI+ copies the input and
        // writes only the token. The output struct is optional.
        let status = unsafe {
            crate::win32::GdiplusStartup(
                std::ptr::from_mut(&mut token),
                std::ptr::from_ref(&input).cast(),
                std::ptr::null_mut(),
            )
        };
        status == 0
    })
}

/// Wrap `hdc` in a smoothing `Graphics` for the duration of `f`, then restore
/// the previous context. Nested calls (there are none today) restore correctly.
fn with_antialias<R>(hdc: Gdi::HDC, f: impl FnOnce() -> R) -> R {
    use windows_sys::Win32::Graphics::GdiPlus as gp;
    if !gdiplus_start() || hdc.is_null() {
        return f();
    }
    let mut g: *mut gp::GpGraphics = std::ptr::null_mut();
    // SAFETY: `g` is written only by GDI+ on success and checked before use.
    let ok = unsafe { gp::GdipCreateFromHDC(hdc, std::ptr::from_mut(&mut g)) } == 0 && !g.is_null();
    if !ok {
        return f();
    }
    // SAFETY: a live Graphics from the call above; the mode is a documented const.
    unsafe {
        gp::GdipSetSmoothingMode(g, gp::SmoothingModeAntiAlias);
    }
    let prev = PAINT_GRAPHICS.with(|c| c.replace(g as isize));
    let out = f();
    PAINT_GRAPHICS.with(|c| c.set(prev));
    // SAFETY: the Graphics was created above and is released exactly once.
    unsafe {
        gp::GdipDeleteGraphics(g);
    }
    out
}

/// A `COLORREF` (`0x00BBGGRR`) as GDI+'s `ARGB` (`0xAARRGGBB`), fully opaque.
fn argb(color: u32) -> u32 {
    let r = color & 0xFF;
    let g = (color >> 8) & 0xFF;
    let b = (color >> 16) & 0xFF;
    0xFF00_0000 | (r << 16) | (g << 8) | b
}

/// Fill a rounded rect through the current GDI+ context, if there is one.
/// Returns `false` when the caller must fall back to GDI.
fn gdip_fill_round_rect(rc: Rect, color: u32, radius: i32) -> bool {
    use windows_sys::Win32::Graphics::GdiPlus as gp;
    let g = PAINT_GRAPHICS.with(|c| c.get()) as *mut gp::GpGraphics;
    if g.is_null() {
        return false;
    }
    let r = radius.clamp(0, (rc.right - rc.left).min(rc.bottom - rc.top) / 2);    // SAFETY: every handle is created and released within this call, and `g` is
    // the frame's live Graphics while the paint is in flight.
    unsafe {
        let mut path: *mut gp::GpPath = std::ptr::null_mut();
        if gp::GdipCreatePath(gp::FillModeAlternate, std::ptr::from_mut(&mut path)) != 0 {
            return false;
        }
        if r <= 0 {
            let pts = [
                gp::Point { X: rc.left, Y: rc.top },
                gp::Point { X: rc.right, Y: rc.top },
                gp::Point { X: rc.right, Y: rc.bottom },
                gp::Point { X: rc.left, Y: rc.bottom },
            ];
            let kinds = [0u8, 1, 1, 3];
            gp::GdipDeletePath(path);
            let mut poly: *mut gp::GpPath = std::ptr::null_mut();
            if gp::GdipCreatePath2I(
                pts.as_ptr(),
                kinds.as_ptr(),
                pts.len() as i32,
                gp::FillModeAlternate,
                std::ptr::from_mut(&mut poly),
            ) != 0
            {
                return false;
            }
            let mut br: *mut gp::GpSolidFill = std::ptr::null_mut();
            let ok = gp::GdipCreateSolidFill(argb(color), std::ptr::from_mut(&mut br)) == 0;
            if ok {
                gp::GdipFillPath(g, br as *mut gp::GpBrush, poly);
            }
            if !br.is_null() {
                gp::GdipDeleteBrush(br as *mut gp::GpBrush);
            }
            gp::GdipDeletePath(poly);
            return true;
        }
        let (l, t, rt, b) = (rc.left, rc.top, rc.right, rc.bottom);
        let d = r * 2;
        gp::GdipAddPathArcI(path, l, t, d, d, 180.0, 90.0);
        gp::GdipAddPathArcI(path, rt - d, t, d, d, 270.0, 90.0);
        gp::GdipAddPathArcI(path, rt - d, b - d, d, d, 0.0, 90.0);
        gp::GdipAddPathArcI(path, l, b - d, d, d, 90.0, 90.0);
        gp::GdipClosePathFigure(path);
        let mut br: *mut gp::GpSolidFill = std::ptr::null_mut();
        let ok = gp::GdipCreateSolidFill(argb(color), std::ptr::from_mut(&mut br)) == 0;
        if ok {
            gp::GdipFillPath(g, br as *mut gp::GpBrush, path);
        }
        if !br.is_null() {
            gp::GdipDeleteBrush(br as *mut gp::GpBrush);
        }
        gp::GdipDeletePath(path);
    }
    true
}

/// `fillRoundRect` (`go\window.go:3397`), antialiased when GDI+ is live.
fn fill_round_rect(hdc: Gdi::HDC, rc: Rect, color: u32, radius: i32) {
    if rc.right <= rc.left || rc.bottom <= rc.top {
        return;
    }
    if gdip_fill_round_rect(rc, color, radius) {
        return;
    }
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

/// `strokeRoundRect` (`go\window.go:3410`) at the hairline width.
fn stroke_round_rect(hdc: Gdi::HDC, rc: Rect, color: u32, radius: i32) {
    stroke_round_rect_w(hdc, rc, color, radius, STROKE_HAIR);
}

/// A rounded outline of `width` logical px, antialiased when GDI+ is live.
fn stroke_round_rect_w(hdc: Gdi::HDC, rc: Rect, color: u32, radius: i32, width: i32) {
    use windows_sys::Win32::Graphics::GdiPlus as gp;
    if rc.right <= rc.left || rc.bottom <= rc.top || width <= 0 {
        return;
    }
    let g = PAINT_GRAPHICS.with(|c| c.get()) as *mut gp::GpGraphics;
    if !g.is_null() {
        let r = radius.clamp(0, (rc.right - rc.left).min(rc.bottom - rc.top) / 2);
        // SAFETY: path/pen created and released within the call.
        unsafe {
            let mut path: *mut gp::GpPath = std::ptr::null_mut();
            if gp::GdipCreatePath(gp::FillModeAlternate, std::ptr::from_mut(&mut path)) != 0 {
                return;
            }
            let (l, t, rt, b) = (rc.left, rc.top, rc.right, rc.bottom);
            if r <= 0 {
                let pts = [
                    gp::Point { X: l, Y: t },
                    gp::Point { X: rt, Y: t },
                    gp::Point { X: rt, Y: b },
                    gp::Point { X: l, Y: b },
                ];
                let kinds = [0u8, 1, 1, 3];
                gp::GdipDeletePath(path);
                let mut poly: *mut gp::GpPath = std::ptr::null_mut();
                if gp::GdipCreatePath2I(
                    pts.as_ptr(),
                    kinds.as_ptr(),
                    pts.len() as i32,
                    gp::FillModeAlternate,
                    std::ptr::from_mut(&mut poly),
                ) != 0
                {
                    return;
                }
                let mut pen: *mut gp::GpPen = std::ptr::null_mut();
                if gp::GdipCreatePen1(argb(color), width as f32, gp::UnitPixel, std::ptr::from_mut(&mut pen)) == 0 {
                    gp::GdipDrawPath(g, pen, poly);
                }
                if !pen.is_null() {
                    gp::GdipDeletePen(pen);
                }
                gp::GdipDeletePath(poly);
                return;
            }
            let d = r * 2;
            gp::GdipAddPathArcI(path, l, t, d, d, 180.0, 90.0);
            gp::GdipAddPathArcI(path, rt - d, t, d, d, 270.0, 90.0);
            gp::GdipAddPathArcI(path, rt - d, b - d, d, d, 0.0, 90.0);
            gp::GdipAddPathArcI(path, l, b - d, d, d, 90.0, 90.0);
            gp::GdipClosePathFigure(path);
            let mut pen: *mut gp::GpPen = std::ptr::null_mut();
            if gp::GdipCreatePen1(argb(color), width as f32, gp::UnitPixel, std::ptr::from_mut(&mut pen)) == 0 {
                gp::GdipDrawPath(g, pen, path);
            }
            if !pen.is_null() {
                gp::GdipDeletePen(pen);
            }
            gp::GdipDeletePath(path);
        }
        return;
    }
    // SAFETY: pen created/deleted; NULL_BRUSH keeps the interior.
    unsafe {
        let pen = Gdi::CreatePen(crate::win32::PS_SOLID, width, color);
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

/// The keyboard focus ring: a 2px accent rounded outline inset inside `rc` so it
/// never collides with the control's own edge. Drawn for mouse focus too -- the
/// ring is the app's single focus affordance.
fn draw_focus_ring(hdc: Gdi::HDC, rc: Rect, dpi: i32) {
    let inset = scale_px(dpi, FOCUS_INSET);
    let ring = Rect {
        left: rc.left + inset,
        top: rc.top + inset,
        right: rc.right - inset,
        bottom: rc.bottom - inset,
    };
    if ring.right <= ring.left || ring.bottom <= ring.top {
        return;
    }
    stroke_round_rect_w(
        hdc,
        ring,
        STATE_FOCUS,
        scale_px(dpi, R_CTRL),
        scale_px(dpi, STROKE_FOCUS),
    );
}

/// `drawUIText` (`go\window.go:3429`).
fn draw_ui_text(hdc: Gdi::HDC, s: &str, rc: Rect, format: u32, font: Gdi::HFONT, color: u32) {
    // SAFETY: font/color selected then restored; `u` outlives the call.
    unsafe {
        Gdi::SetTextColor(hdc, color);
        let old = Gdi::SelectObject(hdc, font);
        let u = utf16_z(s);
        let mut bx = wr(rc);
        Gdi::DrawTextW(hdc, u.as_ptr(), -1, &mut bx, format);
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
        let mut bx = wr(rc);
        Gdi::DrawTextW(
            hdc,
            u.as_ptr(),
            -1,
            &mut bx,
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
        let mut bx = windows_sys::Win32::Foundation::RECT {
            left: 0,
            top: 0,
            right: w,
            bottom: 0,
        };
        Gdi::DrawTextW(
            hdc,
            u.as_ptr(),
            -1,
            &mut bx,
            crate::win32::DT_LEFT
                | DT_WORD_BREAK
                | crate::win32::DT_NO_PREFIX
                | crate::win32::DT_END_ELLIPSIS
                | 0x0400,
        );
        bx.bottom - bx.top
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
            let mid = (lo + hi).div_ceil(2);
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
/// A solid rectangular fill, no rounding (the chrome band, the panel).
fn fill_rect_solid(hdc: Gdi::HDC, rc: Rect, color: u32) {
    if rc.right <= rc.left || rc.bottom <= rc.top {
        return;
    }
    // SAFETY: brush created and released within the call; FillRect takes it as a
    // borrowed handle.
    unsafe {
        let br = Gdi::CreateSolidBrush(color);
        let r = wr(rc);
        Gdi::FillRect(hdc, &r, br);
        Gdi::DeleteObject(br);
    }
}

/// `DrawTextW` with GDI's own inter-character spacing (`SetTextCharacterExtra`),
/// then the previous spacing restored. The brand's wordmark and micro-labels are
/// letter-spaced (`design\make_logo.py` tracks the lockup at 0.08em); this is the
/// only letter-spacing control GDI offers, and it is uniform, which is exactly
/// what the brand uses.
fn draw_ui_text_tracked(
    hdc: Gdi::HDC,
    s: &str,
    rc: Rect,
    format: u32,
    font: Gdi::HFONT,
    color: u32,
    extra_px: i32,
) {
    // SAFETY: the spacing value is restored before returning.
    let old = unsafe { crate::win32::SetTextCharacterExtra(hdc, extra_px) };
    draw_ui_text(hdc, s, rc, format, font, color);
    // SAFETY: restores the caller's spacing.
    unsafe {
        crate::win32::SetTextCharacterExtra(hdc, old);
    }
}

/// Tracking in logical px for a face of `size_px` at `em` em.
#[must_use]
fn tracked_px(size_px: i32, em: f32) -> i32 {
    (size_px as f32 * em).round() as i32
}

/// The chrome header band: the Ribbon-P mark on its tile, the Archivo Black
/// wordmark, and the Inter subtitle.
///
/// The mark is drawn from the SAME `HICON` the window applies to its class and
/// `WM_SETICON` slots (`brand::mark_icon`), so the mark in the header, in the
/// titlebar and in the taskbar is one asset, not three approximations.
fn paint_header(hdc: Gdi::HDC, rc: Rect, dpi: i32) {
    let sc = |v: i32| scale_px(dpi, v);
    fill_rect_solid(hdc, rc, COL_SURFACE);
    // A hairline under the band so the chrome and the canvas meet on a line, not
    // a guess.
    fill_rect_solid(
        hdc,
        Rect {
            left: rc.left,
            top: rc.bottom - sc(STROKE_HAIR),
            right: rc.right,
            bottom: rc.bottom,
        },
        COL_CARD,
    );

    let tile = sc(MARK_TILE_L);
    let tile_rc = Rect {
        left: rc.left + sc(MAIN_MARGIN),
        top: rc.top + sc(SP_4),
        right: rc.left + sc(MAIN_MARGIN) + tile,
        bottom: rc.top + sc(SP_4) + tile,
    };
    let icon = brand::mark_icon();
    if !icon.is_null() {
        // SAFETY: `icon` is the process's live mark handle; DrawIconEx reads it.
        unsafe {
            crate::win32::DrawIconEx(
                hdc,
                tile_rc.left,
                tile_rc.top,
                icon,
                tile,
                tile,
                0,
                std::ptr::null_mut(),
                crate::win32::DI_NORMAL,
            );
        }
    }

    let word_font = brand::display(sc(FS_WORDMARK));
    let sub_font = brand::ui(sc(FS_HINT), brand::UI_WEIGHT);
    let fmt = crate::win32::DT_LEFT
        | crate::win32::DT_SINGLE_LINE
        | crate::win32::DT_NO_PREFIX
        | crate::win32::DT_V_CENTER;
    let text_left = tile_rc.right + sc(SP_3);
    draw_ui_text_tracked(
        hdc,
        "POTFLIX",
        Rect {
            left: text_left,
            top: rc.top + sc(SP_3),
            right: rc.right - sc(MAIN_MARGIN),
            bottom: rc.top + sc(SP_3) + sc(26),
        },
        fmt,
        word_font,
        COL_TEXT,
        tracked_px(sc(FS_WORDMARK), brand::TRACK_WORDMARK),
    );
    draw_ui_text(
        hdc,
        "Discord Rich Presence for PotPlayer",
        Rect {
            left: text_left,
            top: rc.top + sc(SP_3) + sc(26),
            right: rc.right - sc(MAIN_MARGIN),
            bottom: rc.bottom - sc(SP_2),
        },
        fmt,
        sub_font,
        COL_MUTED,
    );

    // SAFETY: fonts created above are deleted here.
    unsafe {
        Gdi::DeleteObject(word_font);
        Gdi::DeleteObject(sub_font);
    }
}

/// A section micro-label -- tiny, uppercase, letter-spaced, in **Archivo
/// Black** (the brand's display face, and the brief's own instruction that it
/// drives the wordmark and section headings) -- with the count badge V1 at the
/// line's right end.
///
/// The badge is the brand's own disc system (`design\preview\badges-variants.png`
/// V1: black disc + red ring): a black disc with a 2px red ring and the count
/// centred in it. The numeral is `COL_TEXT` rather than V1's red glyph, because
/// red on the disc measures `4.11:1` and a NUMERAL is text, which the gate holds
/// at `4.5:1`; the ring keeps the red, so V1's structure survives and the gate
/// passes. Both numbers are in `brand_contrast_gate`.
fn paint_section_label(hdc: Gdi::HDC, rc: Rect, dpi: i32, label: &str, count: i32) {
    let sc = |v: i32| scale_px(dpi, v);
    let label_font = brand::display(sc(FS_LABEL));
    let fmt = crate::win32::DT_LEFT
        | crate::win32::DT_SINGLE_LINE
        | crate::win32::DT_NO_PREFIX
        | crate::win32::DT_V_CENTER;

    let disc = sc(18);
    let ring = sc(STROKE_FOCUS);
    let gap = sc(SP_2);
    let badge_left = rc.right - disc - ring;
    let text_right = badge_left - gap;

    draw_ui_text_tracked(
        hdc,
        &label.to_uppercase(),
        Rect {
            left: rc.left,
            top: rc.top,
            right: text_right,
            bottom: rc.bottom,
        },
        fmt,
        label_font,
        COL_MUTED,
        tracked_px(sc(FS_LABEL), brand::TRACK_LABEL),
    );

    // A count of zero has nothing to report; the badge appears with the first
    // folder and the section header alone is the empty state.
    if count > 0 {
        let badge = Rect {
            left: badge_left,
            top: rc.top + (rc.bottom - rc.top - disc) / 2,
            right: badge_left + disc,
            bottom: rc.top + (rc.bottom - rc.top - disc) / 2 + disc,
        };
        let r = disc / 2;
        fill_round_rect(hdc, badge, brand::BLACK, r);
        stroke_round_rect_w(hdc, badge, brand::RED, r, ring);
        let num_font = brand::display(sc(FS_BADGE));
        let num = count.to_string();
        draw_ui_text(
            hdc,
            &num,
            badge,
            crate::win32::DT_CENTER
                | crate::win32::DT_SINGLE_LINE
                | crate::win32::DT_NO_PREFIX
                | crate::win32::DT_V_CENTER,
            num_font,
            COL_TEXT,
        );
        // SAFETY: font created above.
        unsafe {
            Gdi::DeleteObject(num_font);
        }
    }

    // SAFETY: font created above.
    unsafe {
        Gdi::DeleteObject(label_font);
    }
}

/// The folder panel: a `CHROME` well holding one `CARD`-surface chip per folder,
/// the `+N more` line, and the proportional scrollbar in its right gutter.
fn paint_panel(hdc: Gdi::HDC, l: &UiLayout, dpi: i32, st: &UiState) {
    let sc = |v: i32| scale_px(dpi, v);
    let body_font = brand::ui(sc(FS_BODY), brand::UI_WEIGHT);
    let hint_font = brand::ui(sc(FS_HINT), brand::UI_WEIGHT);
    let fmt_left = crate::win32::DT_LEFT
        | crate::win32::DT_SINGLE_LINE
        | crate::win32::DT_NO_PREFIX
        | crate::win32::DT_V_CENTER
        | crate::win32::DT_END_ELLIPSIS;
    let fmt_right = fmt_left | crate::win32::DT_RIGHT;
    let fmt_center = fmt_left | crate::win32::DT_CENTER;

    fill_round_rect(hdc, l.card, COL_SURFACE, sc(R_PANEL));

    if l.rows.is_empty() {
        draw_ui_text(
            hdc,
            "No folders yet - add one below",
            l.card,
            fmt_center,
            hint_font,
            COL_MUTED,
        );
    } else {
        for (i, r) in l.rows.iter().enumerate() {
            // Absolute folder index: the viewport row plus the scroll offset.
            // `abs` is below `ui_dirs.len()` by construction (`vis` rows from
            // `list_top` cover `list_top..list_top + vis`, clamped to `total`).
            let abs = l.list_top + i as i32;
            let selected = abs == st.sel_row;
            let mut fill = COL_ROW;
            if abs == st.hover_row || selected {
                fill = COL_ROW_HOVER;
            }
            fill_round_rect(hdc, *r, fill, sc(R_ROW));
            if selected {
                // The selection indicator is the accent bar down the row's left
                // edge (`4.11:1` on the panel) plus the fill step -- two cues, so
                // selection never rests on colour alone.
                fill_round_rect(
                    hdc,
                    Rect {
                        left: r.left,
                        top: r.top,
                        right: r.left + sc(SELECT_BAR_W),
                        bottom: r.bottom,
                    },
                    COL_ACCENT,
                    sc(R_ROW),
                );
            }
            let mut text_right = r.right - sc(SP_3);
            if abs == st.hover_row && i < l.xbtns.len() {
                draw_ui_text(
                    hdc,
                    "Remove",
                    l.xbtns[i],
                    fmt_center,
                    hint_font,
                    COL_DANGER,
                );
                text_right = l.xbtns[i].left - sc(SP_1);
            }
            draw_ui_text(
                hdc,
                &st.ui_dirs[abs as usize],
                Rect {
                    left: r.left + sc(SP_3),
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
                    left: l.card.left + sc(SP_3),
                    top: l.card.bottom - sc(OVERFLOW_H_L),
                    right: l.card.right - sc(SP_3),
                    bottom: l.card.bottom - sc(SP_1),
                },
                fmt_right,
                hint_font,
                COL_MUTED,
            );
        }
        if l.scrolled() {
            // Position-only scrollbar in the panel's right gutter: the track
            // plus the proportional thumb mirroring `list_top`. Wheel,
            // PageUp/PageDown, Home/End scroll; the thumb is paint, not drag.
            fill_round_rect(hdc, l.scrollbar, brand::TRACK, sc(R_BAR));
            if l.thumb.right > l.thumb.left && l.thumb.bottom > l.thumb.top {
                fill_round_rect(hdc, l.thumb, COL_MUTED, sc(R_BAR));
            }
        }
    }

    // SAFETY: fonts created above.
    unsafe {
        Gdi::DeleteObject(body_font);
        Gdi::DeleteObject(hint_font);
    }
}

/// The scan progress bar over the status line: an unfilled `TRACK` well with the
/// brand red as the fill, both rounded by the house squircle.
fn paint_button(
    hdc: Gdi::HDC,
    b: &UiButton,
    dpi: i32,
    button_font: Gdi::HFONT,
    st: &UiState,
) {
    let sc = |v: i32| scale_px(dpi, v);
    let hovered = st.hover_btn == b.id;
    let pressed = st.pressed_btn == b.id;
    let disabled = !st.scan_blocked_reason(b.id).is_empty();
    let fmt = crate::win32::DT_CENTER
        | crate::win32::DT_SINGLE_LINE
        | crate::win32::DT_NO_PREFIX
        | crate::win32::DT_V_CENTER
        | crate::win32::DT_END_ELLIPSIS;
    let r = sc(R_CTRL);
    match b.style {
        BTN_ACCENT => {
            // The view's one primary control. Resting: the brand red. Hover: one
            // step up. Pressed: the receding plane INSIDE a red edge -- the
            // Ribbon P's own two-facet language, and the reason the silhouette
            // stays red (`4.11:1` on the canvas) while the interior recedes.
            let fill = if disabled {
                COL_SURFACE
            } else if pressed {
                COL_ACCENT_DOWN
            } else if hovered {
                COL_ACCENT_HOV
            } else {
                COL_ACCENT
            };
            fill_round_rect(hdc, b.rc, fill, r);
            if pressed && !disabled {
                stroke_round_rect_w(hdc, b.rc, COL_ACCENT, r, sc(STROKE_FOCUS));
            }
            let tc = if disabled { COL_MUTED_DIM } else { COL_ON_ACCENT };
            draw_ui_text(hdc, &b.label, b.rc, fmt, button_font, tc);
        }
        BTN_TAB | BTN_TAB_SEL => {
            // A segment of a segmented control: one anatomy for the tab strip,
            // the scan-speed segments and the card presets. Lit = the card fill
            // plus the red indicator under the label; unlit = flat on the canvas
            // with the boundary hairline and muted text.
            let lit = b.style == BTN_TAB_SEL;
            if lit || (hovered && !disabled) {
                fill_round_rect(hdc, b.rc, COL_ROW, r);
            }
            if !lit {
                stroke_round_rect(hdc, b.rc, COL_BORDER, r);
            }
            let tc = if disabled {
                COL_MUTED_DIM
            } else if lit || hovered || pressed {
                COL_TEXT
            } else {
                COL_MUTED
            };
            draw_ui_text(hdc, &b.label, b.rc, fmt, button_font, tc);
            if lit {
                let ind = Rect {
                    left: b.rc.left + sc(SP_4),
                    top: b.rc.bottom - sc(SP_1),
                    right: b.rc.right - sc(SP_4),
                    bottom: b.rc.bottom - sc(SP_1) + sc(STROKE_FOCUS),
                };
                fill_round_rect(hdc, ind, COL_ACCENT, sc(R_BAR));
            }
        }
        BTN_DANGER => {
            // A destructive action is still a real control on the grid; only its
            // label carries the warning, in a red that clears the body gate.
            let fill = if pressed { COL_SURFACE } else if hovered { COL_ROW_HOVER } else { COL_CARD };
            fill_round_rect(hdc, b.rc, fill, r);
            stroke_round_rect(hdc, b.rc, COL_BORDER, r);
            let tc = if disabled { COL_MUTED_DIM } else { COL_DANGER };
            draw_ui_text(hdc, &b.label, b.rc, fmt, button_font, tc);
        }
        _ => {
            // BTN_OUTLINE / BTN_SUBTLE: the secondary control. The fill is the
            // card surface and the EDGE hairline is the boundary that carries the
            // non-text gate (`4.09:1` on the canvas).
            let fill = if disabled || pressed {
                COL_SURFACE
            } else if hovered {
                COL_ROW_HOVER
            } else {
                COL_CARD
            };
            fill_round_rect(hdc, b.rc, fill, r);
            stroke_round_rect(hdc, b.rc, COL_BORDER, r);
            let dim = disabled || (b.id == BTN_REMOVE && !st.remove_has_target());
            let tc = if dim { COL_MUTED_DIM } else { COL_TEXT };
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
        COL_SURFACE,
        sc(R_PANEL),
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
    fill_round_rect(hdc, bar, brand::TRACK, sc(R_BAR));
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
pub fn audit_single_line_verdict(
    need_w: i32,
    need_h: i32,
    rect_w: i32,
    rect_h: i32,
    ellipsis: bool,
) -> &'static str {
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
            with_antialias(mem, || paint_view(mem, rc, dpi, &st));
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
            with_antialias(hdc, || paint_view(hdc, rc, dpi, &st));
        }
        EndPaint(hwnd, &ps);
    }
}

/// Paint the current view into any memory DC at any DPI: the offscreen twin of
/// [`paint_main_window`] minus `BeginPaint`/`EndPaint`, for captures and
/// high-DPI proof renders. Holds `UI` only across GDI blits (never across a
/// dispatching call), exactly like `paint_main_window`: NOT a re-entrant site.
pub fn paint_to_hdc(hdc: Gdi::HDC, rc: Rect, dpi: i32) {
    if hdc.is_null() {
        return;
    }
    let st = ui();
    with_antialias(hdc, || paint_view(hdc, rc, dpi, &st));
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

    let mut l = compute_layout_from(&st.ui_dirs, cw, ch, dpi, st.scroll_top);
    // The tab strip's lit segment is a function of the CURRENT view, not of the
    // layout, so it is resolved in one place for both segments.
    for b in l.btns.iter_mut() {
        if b.style == BTN_TAB || b.style == BTN_TAB_SEL {
            let lit = (b.id == BTN_CARD && st.cur_view == View::Card)
                || (b.id == BTN_SETTINGS && st.cur_view == View::Settings);
            b.style = if lit { BTN_TAB_SEL } else { BTN_TAB };
        }
    }

    let button_font = brand::ui(sc(FS_CONTROL), brand::UI_WEIGHT);
    let hint_font = brand::ui(sc(FS_HINT), brand::UI_WEIGHT);
    let fmt_left = crate::win32::DT_LEFT
        | crate::win32::DT_SINGLE_LINE
        | crate::win32::DT_NO_PREFIX
        | crate::win32::DT_V_CENTER
        | crate::win32::DT_END_ELLIPSIS;

    paint_header(hdc, l.header, dpi);
    // The folder count lives badged on the section header it counts, which is
    // where it belongs; the incumbent painted it as a bare right-aligned
    // numeral 340px from the label it described.
    paint_section_label(hdc, l.section, dpi, "Media folders", st.ui_dirs.len() as i32);
    paint_panel(hdc, &l, dpi, st);

    for b in &l.btns {
        let mut label = b.label.clone();
        if b.id == BTN_SCAN && st.scan_active {
            label = scan_button_label(true).to_string();
        }
        let bb = UiButton { label, ..b.clone() };
        paint_button(hdc, &bb, dpi, button_font, st);
    }
    if let Some(frc) = main_focus_rect(&l, st.focus_id) {
        draw_focus_ring(hdc, frc, dpi);
    }
    draw_ui_text(
        hdc,
        &st.status_text,
        l.status,
        fmt_left,
        hint_font,
        COL_MUTED,
    );
    paint_scan_bar(hdc, l.status, dpi, st);

    // SAFETY: fonts created above are deleted here.
    unsafe {
        Gdi::DeleteObject(button_font);
        Gdi::DeleteObject(hint_font);
    }
}

/// `paintSettingsCard` (`go\window.go:2998`).
fn paint_settings_card(hdc: Gdi::HDC, rc: Rect, dpi: i32, st: &UiState) {
    let sc = |v: i32| scale_px(dpi, v);
    let (cw, ch) = (rc.right - rc.left, rc.bottom - rc.top);
    fill_background(hdc, rc);

    let button_font = brand::ui(sc(FS_CONTROL), brand::UI_WEIGHT);
    let hint_font = brand::ui(sc(FS_HINT), brand::UI_WEIGHT);
    let fmt_left = crate::win32::DT_LEFT
        | crate::win32::DT_SINGLE_LINE
        | crate::win32::DT_NO_PREFIX
        | crate::win32::DT_V_CENTER
        | crate::win32::DT_END_ELLIPSIS;

    let mut l = compute_settings_layout(cw, ch, dpi, st.startup_state, &st.scan_speed_state);
    // The tab strip's lit segment is the current view's, resolved once.
    for b in l.btns.iter_mut() {
        if b.style == BTN_TAB || b.style == BTN_TAB_SEL {
            b.style = if b.id == BTN_SETTINGS { BTN_TAB_SEL } else { BTN_TAB };
        }
    }

    let header = Rect {
        left: 0,
        top: 0,
        right: cw,
        bottom: sc(HEADER_H_L),
    };
    paint_header(hdc, header, dpi);

    for lab in &l.labels {
        let text = match lab.id.as_str() {
            "settings.lib" => "Library",
            "settings.startup" => "Startup",
            _ => "Scan speed",
        };
        paint_section_label(hdc, lab.rc, dpi, text, 0);
    }
    draw_ui_text_wrapped(
        hdc,
        crate::settings::SCAN_HINT_TEXT,
        l.hint,
        hint_font,
        COL_MUTED,
    );
    draw_ui_text(
        hdc,
        &crate::settings::dead_cache_label(st.dead_cache_count),
        l.dead,
        fmt_left,
        hint_font,
        COL_MUTED,
    );
    for b in &l.btns {
        paint_button(hdc, b, dpi, button_font, st);
    }
    if let Some(b) = l.btns.iter().find(|b| b.id == st.focus_id) {
        draw_focus_ring(hdc, b.rc, dpi);
    }
    draw_ui_text(
        hdc,
        &st.status_text,
        l.status,
        fmt_left,
        hint_font,
        COL_MUTED,
    );
    paint_scan_bar(hdc, l.status, dpi, st);

    // SAFETY: fonts created above are deleted here.
    unsafe {
        Gdi::DeleteObject(button_font);
        Gdi::DeleteObject(hint_font);
    }
}

/// `paintCardView` (`go\window.go:3078`).
fn paint_card_view(hdc: Gdi::HDC, rc: Rect, dpi: i32, st: &UiState) {
    let sc = |v: i32| scale_px(dpi, v);
    let (cw, ch) = (rc.right - rc.left, rc.bottom - rc.top);
    fill_background(hdc, rc);

    let button_font = brand::ui(sc(FS_CONTROL), brand::UI_WEIGHT);
    let body_font = brand::ui(sc(FS_BODY), brand::UI_WEIGHT);
    let hint_font = brand::ui(sc(FS_HINT), brand::UI_WEIGHT);
    let fmt_left = crate::win32::DT_LEFT
        | crate::win32::DT_SINGLE_LINE
        | crate::win32::DT_NO_PREFIX
        | crate::win32::DT_V_CENTER
        | crate::win32::DT_END_ELLIPSIS;

    let mut l = compute_card_layout(cw, ch, dpi, &st.card_spec_state);
    for b in l.btns.iter_mut() {
        if b.style == BTN_TAB || b.style == BTN_TAB_SEL {
            b.style = if b.id == BTN_CARD { BTN_TAB_SEL } else { BTN_TAB };
        }
    }
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

    let header = Rect {
        left: 0,
        top: 0,
        right: cw,
        bottom: sc(HEADER_H_L),
    };
    paint_header(hdc, header, dpi);
    for lab in &l.labels {
        let text = match lab.id.as_str() {
            "card.preview-label" => "Preview - exact Discord payload",
            "card.line1" => "Line 1 - details",
            _ => "Line 2 - state",
        };
        paint_section_label(hdc, lab.rc, dpi, text, 0);
    }
    paint_preview_rows(hdc, dpi, l.preview, &full, body_font);
    draw_ui_text(
        hdc,
        "Preview shows the exact values handed to Discord.",
        l.hint,
        fmt_left,
        hint_font,
        COL_MUTED,
    );
    for b in &l.btns {
        paint_button(hdc, b, dpi, button_font, st);
    }
    if st.focus_id != 0
        && let Some(b) = l.btns.iter().find(|b| b.id == st.focus_id)
    {
        draw_focus_ring(hdc, b.rc, dpi);
    }
    draw_ui_text(
        hdc,
        &st.status_text,
        l.status,
        fmt_left,
        hint_font,
        COL_MUTED,
    );
    paint_scan_bar(hdc, l.status, dpi, st);

    // SAFETY: fonts created above are deleted here.
    unsafe {
        Gdi::DeleteObject(button_font);
        Gdi::DeleteObject(body_font);
        Gdi::DeleteObject(hint_font);
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
pub fn ui() -> MutexGuard<'static, UiState> {
    UI.lock().unwrap_or_else(std::sync::PoisonError::into_inner)
}

/// Test-only seam: when armed, [`run_blocking_ui`] hands the action to this
/// callback instead of opening the real modal (`SHBrowseForFolderW` /
/// `MessageBoxW`) or shelling out. The callback runs at exactly the point the
/// real blocking call would, so it can observe whether the process-wide `UI`
/// mutex is free there.
#[cfg(test)]
static TEST_BLOCKING_PROBE: Mutex<Option<fn(BlockingUi)>> = Mutex::new(None);

/// Test-only seam for the one blocking action NOT routed through
/// [`run_blocking_ui`]: [`show_main_window`]'s `center_window`/`ShowWindow`
/// step. When armed, the callback runs at that exact blocking point instead of
/// making the Win32 calls, so a test can observe whether `UI` is free there.
#[cfg(test)]
static TEST_SHOW_PROBE: Mutex<Option<fn()>> = Mutex::new(None);

/// Serializes the tests that drive the process-wide `UI` global through the
/// real wndproc: they set `scan_active` and the shared snapshot on `ui()`,
/// which any concurrent global-state test would observe mid-flight.
#[cfg(test)]
static UI_GLOBAL_SERIAL: Mutex<()> = Mutex::new(());

/// Runs a [`BlockingUi`] action with `UI` RELEASED. Every arm either opens a
/// modal dialog or makes a Win32 call that can pump/dispatch a message and
/// re-enter `main_wnd_proc`; the guard must be dropped (never `ui()`) across
/// that call, then re-taken only for the short, non-blocking apply step.
fn run_blocking_ui(hwnd: HWND, action: BlockingUi) {
    #[cfg(test)]
    if let Some(probe) =
        *TEST_BLOCKING_PROBE.lock().unwrap_or_else(std::sync::PoisonError::into_inner)
    {
        probe(action);
        return;
    }
    match action {
        BlockingUi::AddDir => {
            let path = crate::settings::browse_for_folder(hwnd);
            if !path.is_empty() {
                ui().apply_add_dir(&path);
            }
        }
        BlockingUi::ClearMetadata => {
            if crate::settings::confirm_clear(
                hwnd,
                "Potflix - Clear metadata",
                crate::settings::clear_metadata_confirm_text(),
            ) {
                ui().apply_clear_metadata();
            }
        }
        BlockingUi::ClearIndex => {
            if crate::settings::confirm_clear(
                hwnd,
                "Potflix - Clear index",
                crate::settings::clear_index_confirm_text(),
            ) {
                ui().apply_clear_index();
            }
        }
        BlockingUi::ExportBundle => {
            let dest = crate::settings::save_bundle_dialog(hwnd);
            if !dest.is_empty() {
                ui().apply_export_bundle(&dest);
            }
        }
        BlockingUi::ImportBundle => {
            let src = crate::settings::open_bundle_dialog(hwnd);
            if !src.is_empty() {
                ui().apply_import_bundle(hwnd, &src);
            }
        }
        BlockingUi::OpenLogFolder => crate::settings::on_open_log_folder(),
        BlockingUi::HideWindow => {
            // SAFETY: ShowWindow only hides the window.
            unsafe {
                crate::win32::ShowWindow(hwnd, crate::win32::SW_HIDE);
            }
        }
    }
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
    // The Ribbon P, loaded from the embedded `.ico` BEFORE the class is
    // registered: a class's icon slots are read at registration, and a class
    // that declares none gets the generic application icon from the window
    // manager -- which is exactly the defect this fixes.
    let mark = brand::mark_icon();
    let mark_small = brand::mark_icon_small();

    let mut wc: WNDCLASSEXW = unsafe { std::mem::zeroed() };
    wc.cbSize = std::mem::size_of::<WNDCLASSEXW>() as u32;
    wc.lpfnWndProc = Some(main_wnd_proc);
    wc.hInstance = hinst;
    wc.hCursor = cursor;
    wc.hbrBackground = bg;
    wc.hIcon = mark;
    wc.hIconSm = if mark_small.is_null() { mark } else { mark_small };
    wc.lpszClassName = class.as_ptr();
    if unsafe { RegisterClassExW(&wc) } == 0 {
        return Err("RegisterClassExW failed".into());
    }

    let dpi = system_dpi();
    let cw = scale_px(dpi, MAIN_CLIENT_W);
    let ch = scale_px(dpi, MAIN_CLIENT_H);
    let style =
        crate::win32::WS_CAPTION | crate::win32::WS_SYS_MENU | crate::win32::WS_MINIMIZE_BOX;
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
    // Rust-only chrome: pin the OS caption, title text and 1px frame to the
    // dark palette so the stock white title bar never appears. `WS_CAPTION`
    // stays (custom-drawn chrome would re-open the reentrancy class of
    // `3503ddb`): `DwmSetWindowAttribute` copies four bytes and dispatches
    // nothing to this wndproc, so this is NOT a re-entrant site -- no `UI`
    // guard or regression-test change (cf. `run_blocking_ui`).
    crate::win32::apply_dark_caption(hwnd, COL_BG, COL_TEXT);
    // Apply the mark to the WINDOW slots as well as the class slots. The class
    // slots are what `GetClassLongPtrW(GCLP_HICON)` reports; `WM_SETICON` is
    // what the titlebar, alt-tab and the taskbar actually read, and the two are
    // separate. Both are set from the one asset, and the handle the write
    // produced is recorded so the proof can quote it.
    let class_icon = brand::apply_window_icon(hwnd);
    {
        let mut st = ui();
        st.main_hwnd = hwnd as usize;
        st.ui_icon = class_icon as usize;
        st.refresh_dir_list();
    }
    if class_icon.is_null() {
        return Err("the Ribbon P could not be applied to the window".into());
    }
    app_logf(&format!(
        "window icon: class HICON={class_icon:p} brand={} fonts={}",
        brand::DISPLAY_FAMILY,
        brand::fonts_installed()
    ));
    Ok(())
}

/// `showMainWindow` (`go\window.go:346`).
pub fn show_main_window() {
    // `UI` is RELEASED before every Win32 call below. `center_window`'s
    // `MoveWindow` and `UpdateWindow` both synchronously re-enter
    // `main_wnd_proc` (`WM_WINDOWPOSCHANGED`/`WM_SIZE`, `WM_PAINT`), whose
    // handlers re-lock `UI` via `ui()`. Holding this non-reentrant guard across
    // them self-deadlocks the UI thread -- the shipped blank-white "(Not
    // Responding)" startup hang. Go's `showMainWindow` (`go\window.go:346`) holds
    // no lock, so this is a port-introduced regression.
    let (hwnd, needs_center) = {
        let mut st = ui();
        if st.main_hwnd == 0 {
            return;
        }
        let hwnd = st.main_hwnd as HWND;
        let needs_center = !st.ui_centered;
        if needs_center {
            st.ui_centered = true;
        }
        (hwnd as HWND, needs_center)
    };
    #[cfg(test)]
    if let Some(probe) =
        *TEST_SHOW_PROBE.lock().unwrap_or_else(std::sync::PoisonError::into_inner)
    {
        probe();
        return;
    }
    if needs_center {
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
///
/// # Safety
/// Called by the OS as the window procedure for `MAIN_CLASS_NAME`; `hwnd` must be
/// the live main window handle passed at class registration.
pub unsafe extern "system" fn main_wnd_proc(
    hwnd: HWND,
    msg: u32,
    wparam: usize,
    lparam: isize,
) -> isize {
    match msg {
        crate::win32::WM_CLOSE => {
            // NEVER DestroyWindow: closing parks the window back to the tray.
            unsafe {
                crate::win32::ShowWindow(hwnd, crate::win32::SW_HIDE);
            }
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
            let (x, y) = mouse_xy(lparam as usize);
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
            let (x, y) = mouse_xy(lparam as usize);
            ui().press_at(hwnd, x, y);
            return 0;
        }
        crate::win32::WM_L_BUTTON_UP => {
            let (x, y) = mouse_xy(lparam as usize);
            // The action dispatch may open a modal dialog / show the window,
            // which pumps the queue and re-enters this wndproc: the `UI` guard
            // is dropped before `run_blocking_ui` (never held across it).
            let blocking = ui().release_at(hwnd, x, y);
            if let Some(b) = blocking {
                run_blocking_ui(hwnd, b);
            }
            return 0;
        }
        crate::win32::WM_MOUSEWHEEL => {
            // Wheel over the folder list scrolls it; anywhere else (or an
            // unscrollable list) it is a no-op. `scroll_list` only
            // invalidates (async), so this is NOT a re-entrant site.
            let notches = crate::win32::wheel_delta(wparam) / crate::win32::WHEEL_DELTA;
            ui().scroll_list(hwnd, -notches * UiState::WHEEL_SCROLL_ROWS);
            return 0;
        }
        WM_SETFOCUS | WM_KILLFOCUS => {
            ui().invalidate();
            return 0;
        }
        WM_CHAR => return 0,
        WM_KEYDOWN => {
            let blocking = {
                let mut st = ui();
                let vk = wparam as u32;
                if st.on_settings_key_down(hwnd, vk) {
                    st.invalidate();
                }
                // `VK_RETURN` may focus a button whose action is blocking; run
                // it after the guard drops (`run_blocking_ui`).
                st.pending_blocking.take()
            };
            if let Some(b) = blocking {
                run_blocking_ui(hwnd, b);
            }
            return 0;
        }
        crate::win32::WM_SET_CURSOR => {
            let st = ui();
            if st.have_mouse && st.hit_test_code(hwnd, st.last_mx, st.last_my) != 0 {
                unsafe {
                    let hand = crate::win32::LoadCursorW(
                        std::ptr::null_mut(),
                        crate::win32::IDC_HAND as usize as *const u16,
                    );
                    if !hand.is_null() {
                        crate::win32::SetCursor(hand);
                        return 1;
                    }
                }
            }
            return unsafe { crate::win32::DefWindowProcW(hwnd, msg, wparam, lparam) };
        }
        WM_APP_SCAN_PROGRESS => {
            let mut st = ui();
            st.publish_scan_progress();
            st.refresh_scan_live_text(hwnd, ScanLifecycleEvent::Progress, Instant::now());
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
            let mut st = ui();
            st.publish_scan_done();
            st.on_scan_done(hwnd);
            return 0;
        }
        WM_APP_REPORT_DONE => {
            ui().on_report_done();
            return 0;
        }
        _ => {}
    }
    unsafe { crate::win32::DefWindowProcW(hwnd, msg, wparam, lparam) }
}

/// `trackMouseLeave` (`go\window.go:1735`).
fn track_mouse_leave(hwnd: HWND) {
    let mut tm = crate::win32::TrackMouseEvent {
        cb_size: std::mem::size_of::<crate::win32::TrackMouseEvent>() as u32,
        dw_flags: crate::win32::TME_LEAVE,
        hwnd_track: hwnd as usize,
        dw_hover_time: 0,
    };
    // SAFETY: struct is live for the call (same ABI as TRACKMOUSEEVENT).
    unsafe {
        crate::win32::TrackMouseEvent(
            std::ptr::from_mut(&mut tm)
                .cast::<windows_sys::Win32::UI::Input::KeyboardAndMouse::TRACKMOUSEEVENT>(),
        );
    }
}

// ===TESTS===

#[cfg(test)]
mod tests {
    #![allow(clippy::field_reassign_with_default)]
    use super::*;
    use std::sync::Arc;
    use std::sync::atomic::{AtomicBool, AtomicU32, Ordering};
    use std::time::Duration;

    struct Scratch {
        dir: std::path::PathBuf,
    }
    impl Scratch {
        fn new(tag: &str) -> Self {
            let n = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map_or(0, |d| d.as_nanos());
            let dir =
                std::env::temp_dir().join(format!("potflix-win-{tag}-{}-{n}", std::process::id()));
            std::fs::create_dir_all(&dir).unwrap();
            Self { dir }
        }
        fn path(&self) -> &std::path::Path {
            &self.dir
        }
        fn join(&self, rel: &str) -> std::path::PathBuf {
            self.dir.join(rel)
        }
        fn write(&self, name: &str, contents: &str) -> std::path::PathBuf {
            let p = self.dir.join(name);
            std::fs::write(&p, contents).unwrap();
            p
        }
    }
    impl Drop for Scratch {
        fn drop(&mut self) {
            let _ = std::fs::remove_dir_all(&self.dir);
        }
    }

    fn r_eq(a: Rect, b: Rect) -> bool {
        a.left == b.left && a.top == b.top && a.right == b.right && a.bottom == b.bottom
    }

    fn dirs(n: usize) -> Vec<String> {
        (0..n)
            .map(|i| format!("{}:\\Media", char::from(b'A' + (i % 26) as u8)))
            .collect()
    }

    const NULL: HWND = std::ptr::null_mut();

    // -- window_test.go -----------------------------------------------------

    #[test]
    fn pt_in_rect_boundaries() {
        let r = Rect {
            left: 10,
            top: 20,
            right: 30,
            bottom: 40,
        };
        for p in [(10, 20), (29, 39), (15, 25)] {
            assert!(pt_in_rect(r, p.0, p.1), "inside {p:?}");
        }
        for p in [(9, 20), (10, 19), (30, 20), (10, 40), (0, 0), (100, 100)] {
            assert!(!pt_in_rect(r, p.0, p.1), "outside {p:?}");
        }
    }

    #[test]
    fn button_at_point_lookup() {
        let btns = vec![
            UiButton {
                id: BTN_ADD,
                label: "Add folder".into(),
                style: BTN_SUBTLE,
                rc: Rect {
                    left: 20,
                    top: 400,
                    right: 224,
                    bottom: 436,
                },
            },
            UiButton {
                id: BTN_REMOVE,
                label: "Remove".into(),
                style: BTN_SUBTLE,
                rc: Rect {
                    left: 236,
                    top: 400,
                    right: 440,
                    bottom: 436,
                },
            },
            UiButton {
                id: BTN_SCAN,
                label: "Scan".into(),
                style: BTN_ACCENT,
                rc: Rect {
                    left: 20,
                    top: 448,
                    right: 224,
                    bottom: 484,
                },
            },
        ];
        assert_eq!(button_at_point(&btns, 100, 410), BTN_ADD);
        assert_eq!(button_at_point(&btns, 300, 410), BTN_REMOVE);
        assert_eq!(button_at_point(&btns, 100, 460), BTN_SCAN);
        assert_eq!(button_at_point(&btns, 230, 410), 0);
        assert_eq!(button_at_point(&btns, 10, 10), 0);
        assert_eq!(button_at_point(&[], 100, 410), 0);
    }

    #[test]
    fn row_at_point_lookup() {
        let rows = vec![
            Rect {
                left: 30,
                top: 128,
                right: 430,
                bottom: 164,
            },
            Rect {
                left: 30,
                top: 172,
                right: 430,
                bottom: 208,
            },
        ];
        assert_eq!(row_at_point(&rows, 100, 140), 0);
        assert_eq!(row_at_point(&rows, 100, 180), 1);
        assert_eq!(row_at_point(&rows, 100, 168), -1);
        assert_eq!(row_at_point(&[], 100, 140), -1);
    }

    #[test]
    fn compute_layout_buttons() {
        let l = compute_layout(&dirs(2), MAIN_CLIENT_W, MAIN_CLIENT_H, 96);
        // Six actions on the grid plus the tab strip's two segments, which are
        // real controls in the same table.
        assert_eq!(l.btns.len(), 8);
        let mut seen = std::collections::HashSet::new();
        for b in &l.btns {
            assert!(seen.insert(b.id), "duplicate id {}", b.id);
            assert!(
                b.rc.left >= 0
                    && b.rc.top >= 0
                    && b.rc.right <= MAIN_CLIENT_W
                    && b.rc.bottom <= MAIN_CLIENT_H,
                "button {} escapes the client: {:?}",
                b.id,
                b.rc
            );
        }
        for id in [
            BTN_ADD,
            BTN_REMOVE,
            BTN_SCAN,
            BTN_REFRESH,
            BTN_REPORT,
            BTN_CLOSE,
        ] {
            assert!(seen.contains(&id), "button {id} missing");
        }
        for (i, a) in l.btns.iter().enumerate() {
            for b in l.btns.iter().skip(i + 1) {
                assert!(
                    !rects_intersect(a.rc, b.rc),
                    "buttons {} and {} overlap",
                    a.id,
                    b.id
                );
            }
        }
        assert_eq!(l.rows.len(), 2);
        for (i, r) in l.rows.iter().enumerate() {
            assert_eq!(
                row_at_point(&l.rows, (r.left + r.right) / 2, (r.top + r.bottom) / 2),
                i as i32
            );
        }
        for b in &l.btns {
            assert_eq!(
                button_at_point(
                    &l.btns,
                    (b.rc.left + b.rc.right) / 2,
                    (b.rc.top + b.rc.bottom) / 2
                ),
                b.id
            );
        }
    }

    /// The action grid is ONE grid: equal columns, one column gap, one row gap,
    /// all six actions on it, and exactly one primary control.
    #[test]
    fn action_grid_is_one_grid() {
        for dpi in [96, 120, 144, 192] {
            let cw = scale_px(dpi, MAIN_CLIENT_W);
            let ch = scale_px(dpi, MAIN_CLIENT_H);
            let l = compute_layout(&dirs(2), cw, ch, dpi);
            let sc = |v: i32| scale_px(dpi, v);
            let margin = sc(MAIN_MARGIN);
            let want_w = ((cw - margin * 2) - sc(BTN_GAP_L)) / 2;
            let want_h = sc(BTN_H);
            let gap = sc(BTN_GAP_L);

            let mut rows: Vec<Vec<&UiButton>> = Vec::new();
            for b in l.btns.iter().filter(|b| b.style != BTN_TAB && b.style != BTN_TAB_SEL) {
                match rows.iter_mut().find(|r| r[0].rc.top == b.rc.top) {
                    Some(r) => r.push(b),
                    None => rows.push(vec![b]),
                }
            }
            rows.sort_by_key(|r| r[0].rc.top);
            assert_eq!(rows.len() as i32, FOOTER_ROWS_L, "dpi {dpi}: {rows:?}");
            for (i, row) in rows.iter().enumerate() {
                assert_eq!(row.len(), 2, "dpi {dpi}: row {i} is not two columns");
                let (a, b) = (row[0], row[1]);
                // Equal widths, on the grid's own column pitch.
                assert_eq!(a.rc.right - a.rc.left, want_w, "dpi {dpi}: col 0 width");
                assert_eq!(b.rc.right - b.rc.left, want_w, "dpi {dpi}: col 1 width");
                assert_eq!(a.rc.bottom - a.rc.top, want_h);
                assert_eq!(b.rc.bottom - b.rc.top, want_h);
                assert_eq!(a.rc.left, margin, "dpi {dpi}: row {i} col 0 left");
                assert_eq!(b.rc.left, margin + want_w + gap, "dpi {dpi}: row {i} col 1");
                assert_eq!(b.rc.right, margin + want_w * 2 + gap, "dpi {dpi}: row {i} right edge");
                assert_eq!(a.rc.top, b.rc.top, "dpi {dpi}: row {i} is not level");
                assert_eq!(a.rc.bottom, b.rc.bottom);
                if i > 0 {
                    assert_eq!(
                        a.rc.top - rows[i - 1][0].rc.bottom,
                        gap,
                        "dpi {dpi}: row gap"
                    );
                }
            }
            // Exactly one primary, and it is `Scan`.
            let primaries: Vec<i32> = l
                .btns
                .iter()
                .filter(|b| b.style == BTN_ACCENT)
                .map(|b| b.id)
                .collect();
            assert_eq!(primaries, vec![BTN_SCAN], "dpi {dpi}");
        }
    }

    /// The tab strip is a segmented control: two equal segments spanning the
    /// content width, contiguous, in the strip band -- not the incumbent's two
    /// bare right-aligned text links.
    #[test]
    fn tab_strip_is_a_segmented_control() {
        for dpi in [96, 120, 144, 192] {
            let cw = scale_px(dpi, MAIN_CLIENT_W);
            let ch = scale_px(dpi, MAIN_CLIENT_H);
            let l = compute_layout(&dirs(2), cw, ch, dpi);
            let sc = |v: i32| scale_px(dpi, v);
            let margin = sc(MAIN_MARGIN);
            let _ = cw;
            let card = l.card_link;
            let settings = l.settings;
            assert_eq!(card.left, margin, "dpi {dpi}");
            assert_eq!(
                settings.right,
                margin + (card.right - card.left) * 2 + sc(SP_2),
                "dpi {dpi}: the strip spans both segments plus the one gap"
            );
            assert_eq!(
                card.right + sc(SP_2),
                settings.left,
                "dpi {dpi}: one gap between the segments"
            );
            assert_eq!(
                card.right - card.left,
                settings.right - settings.left,
                "dpi {dpi}: segments are equal"
            );
            assert_eq!(card.top, sc(TAB_TOP_L), "dpi {dpi}");
            assert_eq!(card.bottom, sc(TAB_TOP_L) + sc(TAB_H_L), "dpi {dpi}");
            // The strip sits below the header band and above the section label.
            assert!(card.top >= l.header.bottom, "dpi {dpi}");
            assert!(card.bottom <= l.section.top, "dpi {dpi}");
            for b in l.btns.iter().filter(|b| b.style == BTN_TAB || b.style == BTN_TAB_SEL) {
                let want = if b.id == BTN_CARD { card } else { settings };
                assert!(r_eq(b.rc, want), "dpi {dpi}: segment {} off the strip", b.id);
            }
            // The folder count's badge shares the section line, at its right end.
            let rects = main_text_rects(cw, ch, dpi, &l, 2);
            let sec = rects.iter().find(|r| r.id == "main.section").unwrap();
            let badge = rects.iter().find(|r| r.id == "main.badge").unwrap();
            assert_eq!(badge.rc.right, l.section.right, "dpi {dpi}");
            assert!(sec.rc.right < badge.rc.left, "dpi {dpi}: label and badge overlap");
            assert!(!rects_intersect(sec.rc, badge.rc));
        }
    }

    #[test]
    fn compute_layout_card_follows_rows() {
        let card_h = |n: usize| {
            let d = dirs(n);
            let l = compute_layout(&d, MAIN_CLIENT_W, MAIN_CLIENT_H, 96);
            let h = l.card.bottom - l.card.top;
            (l, h)
        };
        let (l1, h1) = card_h(1);
        let (l4, h4) = card_h(4);
        assert!(h1 < h4, "1-row {h1} vs 4-row {h4}");
        assert!(l1.rows.len() == 1 && l4.rows.len() == 4);
        let (l20, h20) = card_h(20);
        let (_, h5) = card_h(5);
        // The panel grows to the height the action grid leaves it, then clamps:
        // the viewport holds `list_capacity` rows and the rest is `+N more`.
        assert!(h20 > h5, "the 20-folder panel {h20} must exceed the 5-folder {h5}");
        assert_eq!(l20.rows.len(), l20.list_capacity as usize);
        assert_eq!(l20.list_capacity, 7, "the grid leaves room for 7 rows");
        assert_eq!(l20.overflow, 20 - 7);
        let grid_top = l20
            .btns
            .iter()
            .filter(|b| b.style != BTN_TAB && b.style != BTN_TAB_SEL)
            .map(|b| b.rc.top)
            .min()
            .unwrap();
        assert!(l20.card.bottom <= grid_top, "panel overruns the grid");
        for (i, r) in l20.rows.iter().enumerate() {
            assert_eq!(
                row_at_point(&l20.rows, (r.left + r.right) / 2, (r.top + r.bottom) / 2),
                i as i32
            );
        }
        for (a, b) in l1.btns.iter().zip(l20.btns.iter()) {
            assert!(r_eq(a.rc, b.rc), "button {} moved", a.id);
        }
        assert!(r_eq(l1.status, l20.status));
    }

    #[test]
    fn main_settings_affordance() {
        let l = compute_layout(&dirs(2), MAIN_CLIENT_W, MAIN_CLIENT_H, 96);
        assert_eq!(l.btns.len(), 8);
        let s = l.settings;
        assert!(s.right <= MAIN_CLIENT_W && s.bottom <= MAIN_CLIENT_H && s.left >= 0 && s.top >= 0);
        assert!(s.right > s.left && s.bottom > s.top);
        assert!(
            s.top >= TAB_TOP_L && s.bottom <= TAB_TOP_L + TAB_H_L,
            "settings tab {s:?} leaves the tab strip"
        );
        assert!(
            s.top >= l.header.bottom,
            "the tab strip must sit below the header band"
        );
        // The tab segment is itself a control, so it is excluded from the
        // overlap scan the way every other control is compared.
        for b in l
            .btns
            .iter()
            .filter(|b| b.style != BTN_TAB && b.style != BTN_TAB_SEL)
        {
            assert!(
                !rects_intersect(s, b.rc),
                "settings overlaps button {}",
                b.id
            );
        }
        for r in &l.rows {
            assert!(!rects_intersect(s, *r));
        }
        assert!(!rects_intersect(s, l.status));
        assert!(!rects_intersect(s, l.card));
    }

    #[test]
    fn view_switch() {
        let mut st = UiState::default();
        assert_eq!(st.cur_view, View::Main);
        st.open_settings();
        assert_eq!(st.cur_view, View::Settings);
        st.close_settings();
        assert_eq!(st.cur_view, View::Main);
    }

    #[test]
    fn settings_hit_test() {
        let l = compute_settings_layout(MAIN_CLIENT_W, MAIN_CLIENT_H, 96, false, "balanced");
        assert_eq!(l.btns.len(), 12);
        for id in [
            BTN_BACK,
            BTN_CLEAR_CACHE,
            BTN_CLEAR_INDEX,
            BTN_EXPORT,
            BTN_IMPORT,
            BTN_STARTUP,
            BTN_OPEN_LOG,
        ] {
            let rc = l
                .settings_btn(id)
                .unwrap_or_else(|| panic!("id {id} missing"));
            assert!(
                rc.right <= MAIN_CLIENT_W
                    && rc.bottom <= MAIN_CLIENT_H
                    && rc.left >= 0
                    && rc.top >= 0
            );
            assert_eq!(
                button_at_point(&l.btns, (rc.left + rc.right) / 2, (rc.top + rc.bottom) / 2),
                id
            );
        }
        for (i, a) in l.btns.iter().enumerate() {
            for b in l.btns.iter().skip(i + 1) {
                assert!(
                    !rects_intersect(a.rc, b.rc),
                    "{} and {} overlap",
                    a.id,
                    b.id
                );
            }
        }
        let off = compute_settings_layout(MAIN_CLIENT_W, MAIN_CLIENT_H, 96, false, "balanced");
        let on = compute_settings_layout(MAIN_CLIENT_W, MAIN_CLIENT_H, 96, true, "balanced");
        assert!(r_eq(
            off.settings_btn(BTN_STARTUP).unwrap(),
            on.settings_btn(BTN_STARTUP).unwrap()
        ));
        assert_eq!(
            off.btns.iter().find(|b| b.id == BTN_STARTUP).unwrap().label,
            "[ ] Start with Windows"
        );
        assert_eq!(
            on.btns.iter().find(|b| b.id == BTN_STARTUP).unwrap().label,
            "[x] Start with Windows"
        );
        for id in [BTN_CLEAR_CACHE, BTN_CLEAR_INDEX, BTN_EXPORT, BTN_IMPORT] {
            let rc = l.settings_btn(id).unwrap();
            assert!(
                rc.top >= l.labels[0].rc.bottom && rc.top < l.labels[1].rc.top,
                "library {id} {rc:?} left the LIBRARY group"
            );
        }
        // The startup toggle spans the content width, one row under its label.
        let rc = l.settings_btn(BTN_STARTUP).unwrap();
        assert_eq!(rc.left, l.labels[1].rc.left);
        assert_eq!(rc.right, l.labels[1].rc.right);
        assert!(rc.top >= l.labels[1].rc.bottom && rc.top < l.labels[2].rc.top);
        // The footer row holds Back and Open log folder on the shared grid.
        let back = l.settings_btn(BTN_BACK).unwrap();
        let log = l.settings_btn(BTN_OPEN_LOG).unwrap();
        assert_eq!(back.top, log.top);
        assert_eq!(back.bottom, log.bottom);
        assert_eq!(back.right - back.left, log.right - log.left);
        assert_eq!(back.left, l.status.left);
        assert_eq!(log.right, l.status.right);
        assert!(back.bottom <= l.status.top);
    }

    #[test]
    fn layout_text_invariants() {
        for dpi in [96, 120, 144, 192] {
            let cw = scale_px(dpi, MAIN_CLIENT_W);
            let ch = scale_px(dpi, MAIN_CLIENT_H);
            for n in [0usize, 2, 20] {
                let l = compute_layout(&dirs(n), cw, ch, dpi);
                let rects = main_text_rects(cw, ch, dpi, &l, n as i32);
                for r in &rects {
                    assert!(
                        r.rc.right - r.rc.left > 0 && r.rc.bottom - r.rc.top > 0,
                        "{} non-positive",
                        r.id
                    );
                    assert!(
                        r.rc.left >= 0 && r.rc.top >= 0 && r.rc.right <= cw && r.rc.bottom <= ch,
                        "{} escapes",
                        r.id
                    );
                }
                for (i, a) in rects.iter().enumerate() {
                    for b in rects.iter().skip(i + 1) {
                        assert!(
                            !rects_intersect(a.rc, b.rc),
                            "dpi {dpi} dirs {n}: {} and {} intersect",
                            a.id,
                            b.id
                        );
                    }
                }
            }
            let sl = compute_settings_layout(cw, ch, dpi, false, "balanced");
            let srects = settings_text_rects(cw, ch, dpi, &sl);
            for r in &srects {
                assert!(
                    r.rc.right - r.rc.left > 0 && r.rc.bottom - r.rc.top > 0,
                    "dpi {dpi}: {} non-positive",
                    r.id
                );
                assert!(
                    r.rc.left >= 0 && r.rc.top >= 0 && r.rc.right <= cw && r.rc.bottom <= ch,
                    "dpi {dpi}: {} escapes",
                    r.id
                );
            }
            for (i, a) in srects.iter().enumerate() {
                for b in srects.iter().skip(i + 1) {
                    assert!(
                        !rects_intersect(a.rc, b.rc),
                        "dpi {dpi}: {} and {} intersect",
                        a.id,
                        b.id
                    );
                }
            }
        }
    }

    #[test]
    fn settings_esc_returns_to_main() {
        let mut st = UiState::default();
        st.cur_view = View::Settings;
        assert!(st.on_settings_key_down(NULL, VK_ESCAPE));
        assert_eq!(st.cur_view, View::Main);
        st.cur_view = View::Main;
        assert!(!st.on_settings_key_down(NULL, VK_ESCAPE));
    }

    #[test]
    fn focus_order_covers_rect_table() {
        for (order, want_len) in [
            (main_focus_order(), 8usize),
            (settings_focus_order(), 12),
            (card_focus_order(), 11),
        ] {
            assert_eq!(order.len(), want_len);
            let mut seen = std::collections::HashSet::new();
            for id in &order {
                assert!(seen.insert(*id), "duplicate {id}");
            }
        }
        assert_eq!(focus_order_for_view(View::Main).len(), 8);
        assert_eq!(focus_order_for_view(View::Settings).len(), 12);
        assert_eq!(focus_order_for_view(View::Card).len(), 11);
        for id in [
            BTN_ADD,
            BTN_REMOVE,
            BTN_SCAN,
            BTN_REFRESH,
            BTN_REPORT,
            BTN_CLOSE,
            BTN_SETTINGS,
            BTN_CARD,
        ] {
            assert!(main_focus_order().contains(&id), "missing {id}");
        }
        // The focus order IS the rect table: every control in every view's table
        // has exactly one focus stop, and every focus stop has a rect.
        for (order, ids) in [
            (
                main_focus_order(),
                compute_layout(&dirs(2), MAIN_CLIENT_W, MAIN_CLIENT_H, 96)
                    .btns
                    .iter()
                    .map(|b| b.id)
                    .collect::<Vec<i32>>(),
            ),
            (
                settings_focus_order(),
                compute_settings_layout(MAIN_CLIENT_W, MAIN_CLIENT_H, 96, false, "turbo")
                    .btns
                    .iter()
                    .map(|b| b.id)
                    .collect::<Vec<i32>>(),
            ),
            (
                card_focus_order(),
                compute_card_layout(MAIN_CLIENT_W, MAIN_CLIENT_H, 96, &CardSpec::default())
                    .btns
                    .iter()
                    .map(|b| b.id)
                    .collect::<Vec<i32>>(),
            ),
        ] {
            let mut a = order.clone();
            let mut b = ids;
            a.sort_unstable();
            b.sort_unstable();
            assert_eq!(a, b, "focus order and rect table disagree");
        }
    }

    #[test]
    fn move_focus_wraps() {
        let order = vec![BTN_ADD, BTN_REMOVE, BTN_SCAN];
        assert_eq!(move_focus(0, 1, &order), BTN_ADD);
        assert_eq!(move_focus(0, -1, &order), BTN_SCAN);
        assert_eq!(move_focus(BTN_SCAN, 1, &order), BTN_ADD);
        assert_eq!(move_focus(BTN_ADD, -1, &order), BTN_SCAN);
        assert_eq!(move_focus(BTN_ADD, 1, &order), BTN_REMOVE);
        assert_eq!(move_focus(0, 1, &[]), 0);
    }

    /// `Rect` has no `PartialEq` (it is a byte-exact ABI mirror); compare fields.
    fn rect_same(a: Rect, b: Rect) -> bool {
        a.left == b.left && a.top == b.top && a.right == b.right && a.bottom == b.bottom
    }

    /// sRGB channel (0-255) to linear light (WCAG 2.1 relative luminance).
    fn lin(c: u32) -> f64 {
        let s = c as f64 / 255.0;
        if s <= 0.04045 {
            s / 12.92
        } else {
            ((s + 0.055) / 1.055).powf(2.4)
        }
    }

    /// WCAG 2.1 contrast ratio of two `rgb()` COLORREFs, darkest-first.
    fn contrast(a: u32, b: u32) -> f64 {
        let lum = |c: u32| {
            0.2126 * lin(c & 0xFF) + 0.7152 * lin((c >> 8) & 0xFF) + 0.0722 * lin((c >> 16) & 0xFF)
        };
        let (lo, hi) = if lum(a) < lum(b) {
            (lum(a), lum(b))
        } else {
            (lum(b), lum(a))
        };
        (hi + 0.05) / (lo + 0.05)
    }

    /// Every text-on-background pair in the palette, computed from the actual
    /// `rgb()` constants (WCAG 2.1; AA body text needs 4.5:1, large/disabled
    /// 3:1). Prints the ratios so the proof carries the numbers, not just the
    /// verdict.
    #[test]
    fn palette_contrast() {
        let pairs = [
            ("COL_TEXT/COL_BG", COL_TEXT, COL_BG, 7.0),
            ("COL_TEXT/COL_ROW", COL_TEXT, COL_ROW, 7.0),
            ("COL_MUTED/COL_BG", COL_MUTED, COL_BG, 4.5),
            ("COL_MUTED/COL_CARD", COL_MUTED, COL_CARD, 4.5),
            ("COL_MUTED_DIM/COL_BG", COL_MUTED_DIM, COL_BG, 3.0),
            ("COL_ON_ACCENT/COL_ACCENT", COL_ON_ACCENT, COL_ACCENT, 4.5),
            ("COL_TEXT/COL_ACCENT_HOV", COL_TEXT, COL_ACCENT_HOV, 3.0),
            ("COL_DANGER/COL_BG", COL_DANGER, COL_BG, 3.0),
        ];
        for (name, fg, bg, floor) in pairs {
            let r = contrast(fg, bg);
            println!("contrast {name} = {r:.2}:1 (floor {floor})");
            assert!(r >= floor, "{name} = {r:.2}:1, below {floor}:1");
        }
        // The contrast fix the palette comment records: pure white beats
        // `COL_TEXT` on the brand red.
        assert!(contrast(COL_ON_ACCENT, COL_ACCENT) > contrast(COL_TEXT, COL_ACCENT));
    }

    /// The scroll clamp is exact: `list_top` never exceeds the last full
    /// viewport, the card keeps full height while scrolled, and the thumb is
    /// empty exactly when the list fits.
    #[test]
    fn scroll_clamp_and_thumb() {
        for dpi in [96, 144] {
            // Fits: no scrollbar, no offset.
            let l = compute_layout_total(2, MAIN_CLIENT_W, MAIN_CLIENT_H, dpi, 0);
            assert!(!l.scrolled(), "dpi {dpi}: 2 rows must not scroll");
            assert_eq!(l.list_top, 0);
            assert!(rect_same(l.thumb, Rect::default()));
            // Overflow: clamped to the last viewport, thumb present.
            let full = compute_layout_total(40, MAIN_CLIENT_W, MAIN_CLIENT_H, dpi, 0);
            assert!(full.scrolled(), "dpi {dpi}: 40 rows must scroll");
            assert!(full.list_capacity >= 1);
            let max_top = 40 - full.list_capacity;
            let over = compute_layout_total(40, MAIN_CLIENT_W, MAIN_CLIENT_H, dpi, max_top + 9);
            assert_eq!(over.list_top, max_top, "dpi {dpi}: clamp to last viewport");
            assert!(rect_same(over.card, full.card), "dpi {dpi}: scrolled card keeps full height");
            assert!(over.thumb.bottom > over.thumb.top, "dpi {dpi}: thumb visible");
            let thumb = scrollbar_thumb(over.scrollbar, 40, over.list_capacity, 0, 24);
            assert!(
                thumb.top <= over.thumb.top,
                "dpi {dpi}: thumb travels down with the offset"
            );
        }
    }

    /// `scroll_list` is a no-op outside the Main list and on a null window;
    /// hover clears when the viewport moves (needs a live window, so only the
    /// no-op halves are pinned here -- movement is proved by the captures).
    #[test]
    fn scroll_list_noops() {
        let mut st = UiState::default();
        st.cur_view = View::Settings;
        assert!(!st.scroll_list(NULL, 3));
        st.cur_view = View::Main;
        assert!(!st.scroll_list(NULL, 3));
        assert_eq!(st.scroll_top, 0);
    }

    #[test]
    fn focus_tab_cycle() {
        let mut st = UiState::default();
        st.cur_view = View::Main;
        st.focus_id = 0;
        assert!(st.on_settings_key_down_ex(NULL, VK_TAB, false));
        assert_eq!(st.focus_id, BTN_CARD);
        assert!(st.on_settings_key_down_ex(NULL, VK_TAB, false));
        assert_eq!(st.focus_id, BTN_SETTINGS);
        assert!(st.on_settings_key_down_ex(NULL, VK_TAB, true));
        assert_eq!(st.focus_id, BTN_CARD);
        assert!(st.on_settings_key_down_ex(NULL, VK_DOWN, false));
        assert_eq!(st.focus_id, BTN_SETTINGS);
        assert!(st.on_settings_key_down_ex(NULL, VK_UP, false));
        assert_eq!(st.focus_id, BTN_CARD);
        assert!(!st.on_settings_key_down_ex(NULL, VK_ESCAPE, false));
    }

    #[test]
    fn focus_activate_honours_disabled() {
        let mut st = UiState::default();
        st.cur_view = View::Main;
        st.scan_active = true;
        st.focus_id = BTN_REFRESH;
        assert!(st.on_settings_key_down_ex(NULL, VK_RETURN, false));
        assert_eq!(st.status_text, SCAN_BUSY_TEXT);
        assert!(st.scan_active);
    }

    /// Recorded from INSIDE the substituted blocking call.
    static BLOCKING_PROBE_RAN: AtomicBool = AtomicBool::new(false);
    static BLOCKING_PROBE_UI_UNLOCKED: AtomicBool = AtomicBool::new(false);
    static BLOCKING_PROBE_ACTION: Mutex<Option<BlockingUi>> = Mutex::new(None);

    /// Stands in for whatever real modal / Win32 blocking call an action would
    /// make (`SHBrowseForFolderW`, `MessageBoxW`, `GetOpen/SaveFileNameW`,
    /// `ShellExecuteW`, `ShowWindow`). Runs at the exact point the real call
    /// would pump the queue and re-enter `main_wnd_proc`: if a `UI` guard is
    /// held there, `try_lock` fails (non-blocking, so no hang).
    fn blocking_probe(action: BlockingUi) {
        BLOCKING_PROBE_UI_UNLOCKED.store(UI.try_lock().is_ok(), Ordering::SeqCst);
        *BLOCKING_PROBE_ACTION
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = Some(action);
        BLOCKING_PROBE_RAN.store(true, Ordering::SeqCst);
    }

    /// `show_main_window`'s substituted `center_window`/`ShowWindow` step.
    static SHOW_PROBE_RAN: AtomicBool = AtomicBool::new(false);
    static SHOW_PROBE_UI_UNLOCKED: AtomicBool = AtomicBool::new(false);

    fn show_probe() {
        SHOW_PROBE_UI_UNLOCKED.store(UI.try_lock().is_ok(), Ordering::SeqCst);
        SHOW_PROBE_RAN.store(true, Ordering::SeqCst);
    }

    /// How a route reaches its action through the REAL dispatcher.
    #[derive(Clone, Copy)]
    enum Drive {
        /// `WM_KEYDOWN`/`VK_RETURN` with `BTN_ADD` focused.
        Key { vk: u32 },
        /// A mouse click: `WM_L_BUTTON_DOWN`+`WM_L_BUTTON_UP` on a button.
        Btn { view: View, btn: i32 },
    }

    /// Every fixed blocking action, with the real route that reaches it.
    const ROUTES: &[(&str, Drive, BlockingUi)] = &[
        ("AddDir (VK_RETURN keyboard)", Drive::Key { vk: VK_RETURN }, BlockingUi::AddDir),
        ("AddDir (button)", Drive::Btn { view: View::Main, btn: BTN_ADD }, BlockingUi::AddDir),
        ("HideWindow ('Close to tray' button)", Drive::Btn { view: View::Main, btn: BTN_CLOSE }, BlockingUi::HideWindow),
        ("ClearMetadata ('Clear metadata' button)", Drive::Btn { view: View::Settings, btn: BTN_CLEAR_CACHE }, BlockingUi::ClearMetadata),
        ("ClearIndex ('Clear index' button)", Drive::Btn { view: View::Settings, btn: BTN_CLEAR_INDEX }, BlockingUi::ClearIndex),
        ("ExportBundle ('Export...' button)", Drive::Btn { view: View::Settings, btn: BTN_EXPORT }, BlockingUi::ExportBundle),
        ("ImportBundle ('Import...' button)", Drive::Btn { view: View::Settings, btn: BTN_IMPORT }, BlockingUi::ImportBundle),
        ("OpenLogFolder ('Open log folder' button)", Drive::Btn { view: View::Settings, btn: BTN_OPEN_LOG }, BlockingUi::OpenLogFolder),
    ];

    /// One route's observation, recorded from inside its blocking call.
    struct Obs {
        ran: bool,
        unlocked: bool,
        action: Option<BlockingUi>,
    }

    /// Pack a client `(x, y)` pair as a mouse `lParam`.
    fn mouse_lparam(x: i32, y: i32) -> isize {
        (((y as u32) & 0xFFFF) << 16 | ((x as u32) & 0xFFFF)) as isize
    }

    #[test]
    fn blocking_actions_run_outside_ui_guard_all_routes() {
        // FALSIFIABLE regression test for the re-entrant `UI` deadlock. It
        // drives the REAL dispatcher end to end for EVERY fixed blocking
        // action: it creates the actual main window so the real `main_wnd_proc`
        // is installed, then routes each action through `SendMessageW` into that
        // wndproc - mouse `WM_L_BUTTON_UP` -> `release_at` ->
        // `do_button_action`/`do_settings_action` -> `run_blocking_ui`, and the
        // keyboard route via `WM_KEYDOWN`/`VK_RETURN`. `show_main_window` (the
        // one action with no dispatcher route) is driven directly. Every modal
        // / Win32 blocking call is substituted by `TEST_BLOCKING_PROBE` (or
        // `TEST_SHOW_PROBE`), whose callback records whether `UI.try_lock()`
        // SUCCEEDS *from inside the blocking call*. Re-introducing the bug
        // (holding the guard across `run_blocking_ui`) makes `try_lock` fail ->
        // this test fails. A per-route watchdog (bounded `recv_timeout`) turns a
        // pre-fix HANG into a failure.
        let _ui_serial = UI_GLOBAL_SERIAL
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let (tx, rx) = std::sync::mpsc::channel::<Result<Obs, String>>();
        *TEST_SHOW_PROBE.lock().unwrap() = Some(show_probe);
        let tx_panic = tx.clone();
        std::thread::spawn(move || {
            *TEST_BLOCKING_PROBE.lock().unwrap() = Some(blocking_probe);
            let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                if let Err(e) = create_main_window() {
                    let _ = tx.send(Err(e));
                    return;
                }
                let hwnd = ui().main_hwnd as HWND;
                for &(_, drive, _) in ROUTES {
                    BLOCKING_PROBE_RAN.store(false, Ordering::SeqCst);
                    BLOCKING_PROBE_UI_UNLOCKED.store(false, Ordering::SeqCst);
                    *BLOCKING_PROBE_ACTION.lock().unwrap() = None;
                    {
                        let mut st = ui();
                        st.scan_active = false;
                        st.focus_id = 0;
                        st.pressed_btn = 0;
                        st.pressed_x = -1;
                        match drive {
                            Drive::Key { .. } => {
                                st.cur_view = View::Main;
                                st.focus_id = BTN_ADD;
                            }
                            Drive::Btn { view, .. } => st.cur_view = view,
                        }
                    }
                    let sent = match drive {
                        Drive::Key { vk } => {
                            // SAFETY: owned by this thread -> synchronous WNDPROC.
                            unsafe {
                                crate::win32::SendMessageW(hwnd, WM_KEYDOWN, vk as usize, 0);
                            }
                            Ok(())
                        }
                        Drive::Btn { view, btn } => {
                            let btns = {
                                let st = ui();
                                match view {
                                    View::Settings => st.layout_settings(hwnd).map(|l| l.btns),
                                    _ => st.layout(hwnd).map(|l| l.btns),
                                }
                            };
                            match btns.and_then(|b| b.into_iter().find(|b| b.id == btn)) {
                                Some(b) => {
                                    let lp = mouse_lparam(
                                        (b.rc.left + b.rc.right) / 2,
                                        (b.rc.top + b.rc.bottom) / 2,
                                    );
                                    // SAFETY: same thread -> synchronous WNDPROC.
                                    unsafe {
                                        crate::win32::SendMessageW(
                                            hwnd,
                                            crate::win32::WM_L_BUTTON_DOWN,
                                            0,
                                            lp,
                                        );
                                        crate::win32::SendMessageW(
                                            hwnd,
                                            crate::win32::WM_L_BUTTON_UP,
                                            0,
                                            lp,
                                        );
                                    }
                                    Ok(())
                                }
                                None => Err(format!("button {btn} missing from {view:?} layout")),
                            }
                        }
                    };
                    let obs = sent.map(|()| Obs {
                        ran: BLOCKING_PROBE_RAN.load(Ordering::SeqCst),
                        unlocked: BLOCKING_PROBE_UI_UNLOCKED.load(Ordering::SeqCst),
                        action: *BLOCKING_PROBE_ACTION.lock().unwrap(),
                    });
                    let _ = tx.send(obs);
                }
                // show/center: no dispatcher route, driven directly.
                SHOW_PROBE_RAN.store(false, Ordering::SeqCst);
                SHOW_PROBE_UI_UNLOCKED.store(false, Ordering::SeqCst);
                show_main_window();
                let _ = tx.send(Ok(Obs {
                    ran: SHOW_PROBE_RAN.load(Ordering::SeqCst),
                    unlocked: SHOW_PROBE_UI_UNLOCKED.load(Ordering::SeqCst),
                    action: None,
                }));
                // SAFETY: same thread created the window.
                unsafe {
                    crate::win32::DestroyWindow(hwnd);
                }
            }));
            if result.is_err() {
                let _ = tx_panic.send(Err("dispatch panicked".into()));
            }
        });

        for &(label, _, expect) in ROUTES {
            let got = rx.recv_timeout(Duration::from_secs(15)).unwrap_or_else(|_| {
                panic!("deadlock: route `{label}` did not complete within 15s (a UI guard was held across the blocking call)")
            });
            let obs = got.unwrap_or_else(|e| panic!("route `{label}` failed: {e}"));
            assert!(
                obs.ran,
                "route `{label}`: blocking path never exercised - test proves nothing"
            );
            assert_eq!(
                obs.action,
                Some(expect),
                "route `{label}`: dispatcher requested {:?}, expected {:?}",
                obs.action,
                expect
            );
            assert!(
                obs.unlocked,
                "route `{label}`: UI guard was HELD during the blocking call: the re-entrant deadlock is back"
            );
        }
        let show = rx
            .recv_timeout(Duration::from_secs(15))
            .expect("deadlock: show/center did not complete within 15s")
            .unwrap_or_else(|e| panic!("show/center failed: {e}"));
        assert!(
            show.ran,
            "show/center: blocking path never exercised - test proves nothing"
        );
        assert!(
            show.unlocked,
            "show/center: UI guard was HELD across center_window/ShowWindow: the re-entrant deadlock is back"
        );
        *TEST_BLOCKING_PROBE.lock().unwrap() = None;
        *TEST_SHOW_PROBE.lock().unwrap() = None;
    }

    #[test]
    fn remove_without_selection_feedback() {
        let mut st = UiState::default();
        st.ui_dirs = vec![r"C:\Media\Movies".into()];
        st.sel_row = -1;
        st.hover_row = -1;
        st.remove_dir_at(-1);
        assert_eq!(st.status_text, "Select a folder first.");
        assert_eq!(st.ui_dirs.len(), 1);
    }

    #[test]
    fn scan_blocks_edits() {
        for id in [BTN_ADD, BTN_REMOVE, BTN_REFRESH] {
            assert_eq!(scan_blocked_reason(true, id), SCAN_BUSY_TEXT);
        }
        for id in [BTN_SCAN, BTN_CLOSE, BTN_SETTINGS, BTN_CARD] {
            assert_eq!(scan_blocked_reason(true, id), "");
        }
        for id in [BTN_ADD, BTN_REMOVE, BTN_REFRESH, BTN_SCAN, BTN_CLOSE] {
            assert_eq!(scan_blocked_reason(false, id), "");
        }
    }

    #[test]
    fn remove_dim_needs_target() {
        assert!(!remove_has_target(-1, -1, 1));
        assert!(remove_has_target(0, -1, 1));
        assert!(remove_has_target(-1, 0, 1));
        assert!(!remove_has_target(0, 0, 0));
    }

    #[test]
    fn audit_single_line_verdict_classes() {
        assert_eq!(audit_single_line_verdict(100, 15, 420, 20, true), "");
        assert_eq!(audit_single_line_verdict(100, 15, 420, 20, false), "");
        assert_eq!(audit_single_line_verdict(449, 15, 420, 20, true), "trimmed");
        assert_eq!(
            audit_single_line_verdict(449, 15, 420, 20, false),
            "overflow"
        );
        assert_eq!(audit_single_line_verdict(100, 30, 420, 20, true), "trimmed");
        assert_eq!(
            audit_single_line_verdict(100, 30, 420, 20, false),
            "overflow"
        );
    }

    #[test]
    fn scan_bar_model_walk_resolve() {
        assert_eq!(scan_bar_model(false, 0, 0), (false, false, 0.0));
        let (show, det, _) = scan_bar_model(true, 0, 0);
        assert!(show && !det);
        assert_eq!(scan_bar_model(true, 12, 24), (true, true, 0.5));
        assert_eq!(scan_bar_model(true, 24, 24), (true, true, 1.0));
        assert_eq!(scan_bar_model(true, 30, 24), (true, true, 1.0));
    }

    #[test]
    fn scan_key_fraction_cases() {
        let cases = [
            (0, 10, Some(0.0)),
            (5, 10, Some(0.5)),
            (10, 10, Some(1.0)),
            (15, 10, Some(1.0)),
            (-2, 10, Some(0.0)),
            (5, 0, None),
            (5, -3, None),
        ];
        for (done, total, want) in cases {
            assert_eq!(scan_key_fraction(done, total), want, "({done},{total})");
        }
    }

    #[test]
    fn scan_lifecycle_throttle() {
        let t0 = Instant::now();
        let (st, fx) = scan_lifecycle_next(ScanLifecycle::default(), ScanLifecycleEvent::Start, t0);
        assert!(fx.arm && st.last_paint == t0);
        let mut last = st;
        let mut admitted = 0;
        for i in 1..=100 {
            let now = t0 + Duration::from_millis(i * 10);
            let (next, fx) = scan_lifecycle_next(last, ScanLifecycleEvent::Progress, now);
            if fx.paint {
                admitted += 1;
                last = next;
            }
        }
        assert!(admitted <= 10, "admitted {admitted}/s");
        assert!(admitted > 0);
    }

    #[test]
    fn scan_lifecycle_arm_and_reset() {
        let now = Instant::now();
        let old = now - Duration::from_secs(3600);
        let (st, fx) = scan_lifecycle_next(
            ScanLifecycle {
                active: false,
                last_paint: old,
            },
            ScanLifecycleEvent::Start,
            now,
        );
        assert!(st.active);
        assert!(fx.arm);
        assert_eq!(st.last_paint, now);
        assert!(fx.paint);
        assert!(!fx.kill);
    }

    #[test]
    fn scan_lifecycle_one_scan_at_a_time() {
        let now = Instant::now();
        let running = ScanLifecycle {
            active: true,
            last_paint: now - Duration::from_secs(1),
        };
        let (next, fx) = scan_lifecycle_next(running, ScanLifecycleEvent::Start, now);
        assert_eq!(fx, ScanFx::default());
        assert_eq!(next, running);
    }

    #[test]
    fn scan_lifecycle_tick_gate() {
        let t0 = Instant::now();
        let (st, _) = scan_lifecycle_next(ScanLifecycle::default(), ScanLifecycleEvent::Start, t0);
        let (next, fx) = scan_lifecycle_next(st, ScanLifecycleEvent::Tick, t0);
        assert!(!fx.paint && next.last_paint == t0);
        let later = t0 + SCAN_PAINT_INTERVAL;
        let (next, fx) = scan_lifecycle_next(st, ScanLifecycleEvent::Progress, later);
        assert!(fx.paint && next.last_paint == later);
        let (_, fx) =
            scan_lifecycle_next(ScanLifecycle::default(), ScanLifecycleEvent::Tick, later);
        assert_eq!(fx, ScanFx::default());
        assert!(scan_paint_gate(t0, t0 - Duration::from_secs(1)));
    }

    #[test]
    fn scan_lifecycle_terminal_paint() {
        let now = Instant::now();
        let (st, _) = scan_lifecycle_next(ScanLifecycle::default(), ScanLifecycleEvent::Start, now);
        let (next, fx) = scan_lifecycle_next(st, ScanLifecycleEvent::Done, now);
        assert!(fx.kill && fx.terminal && !fx.paint && !next.active);
        let (back, fx_back) = scan_lifecycle_next(
            st,
            ScanLifecycleEvent::Done,
            now - Duration::from_secs(3600),
        );
        assert!(fx_back.kill && fx_back.terminal && !back.active);
        let (_, fx) = scan_lifecycle_next(ScanLifecycle::default(), ScanLifecycleEvent::Done, now);
        assert!(fx.kill && !fx.terminal);
    }

    #[test]
    fn scan_start_clears_stale_progress() {
        let mut st = UiState::default();
        st.arm_timer = Arc::new(|_| {});
        st.scan_progress = Progress {
            keys_done: 24,
            keys_total: 24,
            keys_fetched: 4,
            keys_cached: 20,
            folders_scanned: 6,
            ..Progress::default()
        };
        st.scan_resolve_start = Some(Instant::now() - Duration::from_secs(60));
        st.scan_resolve_base = 20;
        let now = Instant::now();
        let started = st.begin_scan_run(NULL, false, now);
        assert!(started.is_some());
        assert_eq!(st.scan_progress, Progress::default());
        assert!(st.scan_resolve_start.is_none());
        assert_eq!(st.scan_resolve_base, 0);
        if let Some(c) = st.scan_cancel.take() {
            c.cancel();
        }
    }

    #[test]
    fn scan_start_applies_effects() {
        let arms = Arc::new(AtomicU32::new(0));
        let a = arms.clone();
        let mut st = UiState::default();
        st.arm_timer = Arc::new(move |_| {
            a.fetch_add(1, Ordering::SeqCst);
        });
        st.scan_last_paint = Instant::now() - Duration::from_secs(3600);
        let now = Instant::now();
        assert!(st.begin_scan_run(NULL, false, now).is_some());
        assert!(st.scan_active);
        assert_eq!(arms.load(Ordering::SeqCst), 1);
        assert_eq!(st.scan_last_paint, now);
        assert_eq!(st.status_text, "Scan starting...");
        assert!(!st.scan_force);

        assert!(
            st.begin_scan_run(NULL, true, now + Duration::from_secs(1))
                .is_none()
        );
        assert_eq!(arms.load(Ordering::SeqCst), 1);
        assert_eq!(st.scan_last_paint, now);
        assert_eq!(st.status_text, "Scan starting...");
        if let Some(c) = st.scan_cancel.take() {
            c.cancel();
        }

        let mut st2 = UiState::default();
        let arms2 = Arc::new(AtomicU32::new(0));
        let a2 = arms2.clone();
        st2.arm_timer = Arc::new(move |_| {
            a2.fetch_add(1, Ordering::SeqCst);
        });
        assert!(st2.begin_scan_run(NULL, true, now).is_some());
        assert_eq!(st2.status_text, "Refresh starting...");
        assert!(st2.scan_force);
        if let Some(c) = st2.scan_cancel.take() {
            c.cancel();
        }
    }

    #[test]
    fn refresh_scan_live_text_applies_effects() {
        let mut st = UiState::default();
        let now = Instant::now();
        st.scan_active = true;
        st.scan_cancel = Some(library::Cancel::background());
        st.scan_start = now - Duration::from_secs(30);
        st.scan_last_paint = now - Duration::from_secs(3600);
        st.scan_resolve_start = None;
        st.scan_resolve_base = 0;
        let snap = Progress {
            keys_done: 120,
            keys_total: 1000,
            keys_fetched: 90,
            keys_cached: 20,
            keys_failed: 10,
            folders_scanned: 9,
            folders_skipped: 1,
            ..Progress::default()
        };
        st.scan_progress = snap.clone();
        st.status_text = "SENTINEL".into();

        st.refresh_scan_live_text(NULL, ScanLifecycleEvent::Progress, now);
        assert_eq!(st.scan_last_paint, now);
        assert_eq!(st.scan_resolve_start, Some(now));
        assert_eq!(st.scan_resolve_base, scan_observed_done(&snap));
        let want = scan_live_status_text(
            &snap,
            now.saturating_duration_since(st.scan_start),
            scan_resolve_elapsed(Some(now), now),
            scan_observed_done(&snap),
            window_dpi(NULL),
            live_status_avail_px(NULL),
        );
        assert_eq!(st.status_text, want);
        assert!(st.status_text.starts_with("Scan "));

        st.status_text = "SENTINEL2".into();
        st.scan_progress = Progress {
            keys_done: 121,
            keys_total: 1000,
            keys_fetched: 91,
            keys_cached: 20,
            keys_failed: 10,
            folders_scanned: 9,
            folders_skipped: 1,
            ..Progress::default()
        };
        st.refresh_scan_live_text(NULL, ScanLifecycleEvent::Tick, now);
        assert_eq!(st.status_text, "SENTINEL2");
        assert_eq!(st.scan_last_paint, now);

        st.scan_cancel = None;
        st.status_text = "SENTINEL3".into();
        st.refresh_scan_live_text(
            NULL,
            ScanLifecycleEvent::Progress,
            now + Duration::from_secs(10),
        );
        assert_eq!(st.status_text, "SENTINEL3");
    }

    #[test]
    fn scan_terminal_arg_parity() {
        let kills = Arc::new(AtomicU32::new(0));
        let k = kills.clone();
        let mut st = UiState::default();
        st.kill_timer = Arc::new(move |_| {
            k.fetch_add(1, Ordering::SeqCst);
        });
        let land = |st: &mut UiState, force: bool, stats: Stats, err: Option<UiScanError>| {
            st.scan_stats = stats;
            st.scan_err = err;
            st.scan_force = force;
            st.scan_active = true;
            st.scan_cancel = Some(library::Cancel::background());
            st.on_scan_done(NULL);
        };
        let ok = Stats {
            keys: 24,
            keys_done: 24,
            keys_cached: 5,
            keys_fetched: 2,
            keys_not_found: 17,
            files: 10,
            folders_scanned: 4,
            folders_skipped: 2,
            ..Stats::default()
        };
        land(&mut st, false, ok, None);
        assert_eq!(st.status_text, scan_done_text("Scan", &ok));
        land(&mut st, true, ok, None);
        assert_eq!(st.status_text, scan_done_text("Refresh", &ok));
        assert_eq!(kills.load(Ordering::SeqCst), 2);
        assert!(!st.scan_active);
        assert!(st.scan_cancel.is_none());

        let cancelled = Stats {
            keys: 24,
            keys_done: 12,
            cancelled: true,
            ..Stats::default()
        };
        land(&mut st, false, cancelled, Some(UiScanError::other("boom")));
        assert_eq!(st.status_text, scan_cancelled_text(12, 24));
        let bare = Stats {
            keys: 10,
            keys_done: 4,
            ..Stats::default()
        };
        land(&mut st, true, bare, Some(UiScanError::cancelled()));
        assert_eq!(st.status_text, scan_cancelled_text(4, 10));
        land(&mut st, false, ok, Some(UiScanError::other("disk gone")));
        assert!(st.status_text.contains("Scan failed:"));
        land(&mut st, true, ok, Some(UiScanError::other("disk gone")));
        assert!(st.status_text.contains("Refresh failed:"));

        kills.store(0, Ordering::SeqCst);
        st.status_text = "keep me".into();
        st.scan_active = false;
        st.on_scan_done(NULL);
        assert_eq!(kills.load(Ordering::SeqCst), 1);
        assert_eq!(st.status_text, "keep me");
    }

    #[test]
    fn scan_eta_defensible() {
        let suppressed = [
            (Progress::default(), Duration::from_secs(30)),
            (
                Progress {
                    keys_total: 1000,
                    ..Progress::default()
                },
                Duration::from_secs(30),
            ),
            (
                Progress {
                    keys_done: 3,
                    keys_total: 1000,
                    keys_fetched: 3,
                    ..Progress::default()
                },
                Duration::from_secs(30),
            ),
            (
                Progress {
                    keys_done: 990,
                    keys_total: 1000,
                    keys_cached: 990,
                    ..Progress::default()
                },
                Duration::from_secs(30),
            ),
            (
                Progress {
                    keys_done: 995,
                    keys_total: 1000,
                    keys_cached: 990,
                    keys_fetched: 5,
                    ..Progress::default()
                },
                Duration::from_secs(1),
            ),
            (
                Progress {
                    keys_done: 100,
                    keys_total: 1000,
                    keys_fetched: 100,
                    ..Progress::default()
                },
                Duration::from_secs(1),
            ),
            (
                Progress {
                    keys_done: 1000,
                    keys_total: 1000,
                    keys_fetched: 900,
                    keys_cached: 100,
                    ..Progress::default()
                },
                Duration::from_secs(30),
            ),
        ];
        for (p, resolve) in suppressed {
            assert!(scan_eta(&p, 0, resolve).is_none());
        }
        let pre = Progress {
            keys_done: 100,
            keys_total: 1000,
            keys_fetched: 100,
            ..Progress::default()
        };
        assert!(scan_eta(&pre, 100, Duration::from_secs(20)).is_none());
        let steady = Progress {
            keys_done: 100,
            keys_total: 1000,
            keys_fetched: 100,
            ..Progress::default()
        };
        assert_eq!(
            scan_eta(&steady, 0, Duration::from_secs(20)),
            Some(Duration::from_secs(180))
        );
        let delta = Progress {
            keys_done: 200,
            keys_total: 1000,
            keys_fetched: 200,
            ..Progress::default()
        };
        assert_eq!(
            scan_eta(&delta, 100, Duration::from_secs(20)),
            Some(Duration::from_secs(160))
        );
        let mixed = Progress {
            keys_done: 600,
            keys_total: 1000,
            keys_fetched: 100,
            keys_cached: 500,
            ..Progress::default()
        };
        assert_eq!(
            scan_eta(&mixed, 0, Duration::from_secs(20)),
            Some(Duration::from_secs(80))
        );
        let half = Progress {
            keys_done: 500,
            keys_total: 1000,
            keys_fetched: 500,
            ..Progress::default()
        };
        assert_eq!(
            scan_eta(&half, 0, Duration::from_secs(10)),
            Some(Duration::from_secs(10))
        );
    }

    #[test]
    fn format_scan_duration_cases() {
        let cases = [
            (Duration::ZERO, "0s"),
            (Duration::from_secs(5), "5s"),
            (Duration::from_secs(65), "1m05s"),
            (Duration::from_secs(3720), "1h02m"),
        ];
        for (d, want) in cases {
            assert_eq!(format_scan_duration(d), want);
        }
    }

    #[test]
    fn scan_live_status_text_fit() {
        let p = Progress {
            keys_done: 12,
            keys_total: 24,
            keys_fetched: 7,
            keys_cached: 5,
            folders_scanned: 4,
            folders_skipped: 2,
            ..Progress::default()
        };
        let avail = scan_status_avail_px(96);
        let (_, _, early_eta) = scan_live_status_parts(
            &p,
            Duration::from_millis(500),
            Duration::from_millis(500),
            0,
        );
        assert_eq!(early_eta, "");
        let early = scan_live_status_text(
            &p,
            Duration::from_millis(500),
            Duration::from_millis(500),
            0,
            96,
            avail,
        );
        assert!(early.contains(" | 0s"), "{early}");
        assert!(!early.contains("eta"));
        let (_, _, steady_eta) =
            scan_live_status_parts(&p, Duration::from_secs(20), Duration::from_secs(20), 0);
        assert_ne!(steady_eta, "");
        let base = scan_progress_text(&p);
        let steady = scan_live_status_text(
            &p,
            Duration::from_secs(20),
            Duration::from_secs(20),
            0,
            96,
            avail,
        );
        assert!(steady.contains(&base), "{steady}");
        let walk = scan_live_status_text(
            &Progress::default(),
            Duration::from_secs(30),
            Duration::ZERO,
            0,
            96,
            avail,
        );
        assert!(!walk.contains("eta"));
        let burst = Progress {
            keys_done: 990,
            keys_total: 1000,
            keys_cached: 990,
            ..Progress::default()
        };
        assert!(
            !scan_live_status_text(
                &burst,
                Duration::from_secs(65),
                Duration::from_secs(1),
                0,
                96,
                avail
            )
            .contains("eta")
        );
        assert!(
            !scan_live_status_text(
                &burst,
                Duration::from_secs(90),
                Duration::from_secs(30),
                0,
                96,
                avail
            )
            .contains("eta")
        );
        let big = Progress {
            keys_done: 45000,
            keys_total: 90000,
            keys_fetched: 15000,
            keys_cached: 15000,
            keys_failed: 15000,
            folders_scanned: 450,
            folders_skipped: 32,
            ..Progress::default()
        };
        let got = scan_live_status_text(
            &big,
            Duration::from_secs(3600),
            Duration::from_secs(3600),
            15000,
            96,
            avail,
        );
        assert!(got.len() <= MAX_SCAN_STATUS_LEN, "len {}", got.len());
        assert!(got.contains("folders"), "{got}");
    }

    #[test]
    fn compose_scan_live_status_priority() {
        let p = Progress {
            keys_done: 12000,
            keys_total: 30000,
            keys_fetched: 8000,
            keys_cached: 3000,
            keys_failed: 1000,
            folders_scanned: 200,
            folders_skipped: 15,
            ..Progress::default()
        };
        let (elapsed, resolve) = (Duration::from_secs(20), Duration::from_secs(20));
        let measure = |s: &str| s.chars().count() as i32;
        let cands = scan_status_candidates(&p, elapsed, resolve, 0);
        let full = cands[0].clone();
        assert!(full.contains("eta"));
        assert_eq!(
            compose_scan_live_status(
                &p,
                elapsed,
                resolve,
                0,
                full.chars().count() as i32,
                measure
            ),
            full
        );
        let (counters, elapsed_sfx, _) = scan_live_status_parts(&p, elapsed, resolve, 0);
        let ce = format!("{counters}{elapsed_sfx}");
        assert_eq!(
            compose_scan_live_status(&p, elapsed, resolve, 0, ce.chars().count() as i32, measure),
            ce
        );
        let short = scan_progress_short(&p);
        let se = format!("{short}{elapsed_sfx}");
        assert!(se.chars().count() < ce.chars().count());
        assert_eq!(
            compose_scan_live_status(&p, elapsed, resolve, 0, se.chars().count() as i32, measure),
            se
        );
        let minimal = scan_progress_minimal(&p);
        let floor = scan_progress_floor(&p);
        assert_eq!(minimal, format!("{floor}, 1k failed"));
        assert!(minimal.chars().count() < short.chars().count());
        assert_eq!(
            compose_scan_live_status(
                &p,
                elapsed,
                resolve,
                0,
                minimal.chars().count() as i32,
                measure
            ),
            minimal
        );
        assert_eq!(
            compose_scan_live_status(&p, elapsed, resolve, 0, 1, measure),
            floor
        );
    }

    #[test]
    fn scan_live_status_fits_at_all_dpi() {
        let catching = Progress {
            done: 23402,
            total: 23402,
            resolved: 1118,
            keys_total: 2309,
            keys_done: 1118,
            keys_cached: 418,
            keys_fetched: 700,
            folders_scanned: 3,
            ..Progress::default()
        };
        let cases = [
            (
                Progress {
                    folders_scanned: 3,
                    ..Progress::default()
                },
                Duration::from_secs(30),
                Duration::ZERO,
                0,
            ),
            (
                Progress {
                    keys_done: 12,
                    keys_total: 24,
                    keys_fetched: 7,
                    keys_cached: 5,
                    folders_scanned: 4,
                    folders_skipped: 2,
                    ..Progress::default()
                },
                Duration::from_secs(20),
                Duration::from_secs(20),
                0,
            ),
            (
                Progress {
                    keys_done: 100,
                    keys_total: 1000,
                    keys_fetched: 100,
                    folders_scanned: 9,
                    folders_skipped: 1,
                    ..Progress::default()
                },
                Duration::from_secs(20),
                Duration::from_secs(20),
                0,
            ),
            (
                Progress {
                    keys_done: 12000,
                    keys_total: 30000,
                    keys_fetched: 8000,
                    keys_cached: 3000,
                    keys_failed: 1000,
                    folders_scanned: 200,
                    folders_skipped: 15,
                    ..Progress::default()
                },
                Duration::from_secs(600),
                Duration::from_secs(600),
                0,
            ),
            (
                Progress {
                    keys_done: 45000,
                    keys_total: 90000,
                    keys_fetched: 15000,
                    keys_cached: 15000,
                    keys_failed: 15000,
                    folders_scanned: 450,
                    folders_skipped: 32,
                    ..Progress::default()
                },
                Duration::from_secs(3600),
                Duration::from_secs(3600),
                15000,
            ),
            (
                catching,
                Duration::from_secs(360),
                Duration::from_secs(300),
                418,
            ),
            (
                Progress {
                    keys_done: 999_800,
                    keys_total: 999_900,
                    keys_fetched: 50000,
                    keys_cached: 50000,
                    keys_failed: 899_800,
                    folders_scanned: 999,
                    folders_skipped: 99,
                    ..Progress::default()
                },
                Duration::from_secs(3600),
                Duration::from_secs(3600),
                50000,
            ),
        ];
        for dpi in [96, 120, 144, 192] {
            let cw = scale_px(dpi, MAIN_CLIENT_W);
            let ch = scale_px(dpi, MAIN_CLIENT_H);
            let l = compute_layout(&dirs(0), cw, ch, dpi);
            let avail = l.status.right - l.status.left;
            assert_eq!(avail, scan_status_avail_px(dpi));
            assert_eq!(avail, status_avail_px(cw, dpi));
            let mut widths = vec![avail];
            if dpi > 96 {
                let narrow = status_avail_px(MAIN_CLIENT_W, dpi);
                if narrow < avail {
                    widths.push(narrow);
                }
            }
            for budget in widths {
                for (p, elapsed, resolve, base) in &cases {
                    let text = scan_live_status_text(p, *elapsed, *resolve, *base, dpi, budget);
                    let Some(need) = measure_scan_status_width(dpi, &text) else {
                        return; // no GDI DC: skip (the one native exception)
                    };
                    assert!(
                        need <= budget,
                        "dpi {dpi} avail {budget} need {need}: {text:?}"
                    );
                }
            }
        }
        let big = &cases[6];
        let (counters, elapsed_sfx, eta_sfx) = scan_live_status_parts(&big.0, big.1, big.2, big.3);
        assert_ne!(eta_sfx, "");
        let Some(need) =
            measure_scan_status_width(96, &format!("{counters}{elapsed_sfx}{eta_sfx}"))
        else {
            return;
        };
        assert!(
            need > scan_status_avail_px(96),
            "control is vacuous: {need}"
        );
        let narrow192 = status_avail_px(MAIN_CLIENT_W, 192);
        let Some(min_need) = measure_scan_status_width(192, &scan_progress_minimal(&big.0)) else {
            return;
        };
        assert!(
            min_need > narrow192,
            "minimal must overflow the narrowest budget"
        );
        let floor = scan_progress_floor(&big.0);
        let Some(floor_need) = measure_scan_status_width(192, &floor) else {
            return;
        };
        assert!(floor_need <= narrow192, "floor must fit");
        let fallback = format!("{floor} | {}", format_scan_duration(big.1));
        let Some(fb) = measure_scan_status_width(192, &fallback) else {
            return;
        };
        assert!(fb <= narrow192, "fallback must fit");
        assert_eq!(
            scan_live_status_text(&big.0, big.1, big.2, big.3, 192, narrow192),
            floor
        );
    }

    #[test]
    fn scan_bar_geometry() {
        for dpi in [96, 120, 144, 192] {
            let cw = scale_px(dpi, MAIN_CLIENT_W);
            let ch = scale_px(dpi, MAIN_CLIENT_H);
            let l = compute_layout(&dirs(0), cw, ch, dpi);
            if dpi == 96 {
                assert_eq!(l.status.right - l.status.left, 420);
            }
            let bar = scan_bar_rect(l.status, dpi);
            assert!(bar.right > bar.left && bar.bottom > bar.top);
            assert!(bar.left >= 0 && bar.top >= 0 && bar.right <= cw && bar.bottom <= ch);
            assert!(
                !rects_intersect(bar, l.status),
                "dpi {dpi}: bar intersects status"
            );
        }
    }

    #[test]
    fn scan_indeterminate_pulse() {
        assert_eq!(scan_indeterminate_pos(Duration::ZERO), 0.0);
        assert_eq!(scan_indeterminate_pos(Duration::from_millis(600)), 0.5);
        assert_eq!(scan_indeterminate_pos(Duration::from_millis(1200)), 0.0);
        for d in [
            Duration::from_secs(1),
            Duration::from_secs(5),
            Duration::from_secs(3600),
        ] {
            let v = scan_indeterminate_pos(d);
            assert!((0.0..1.0).contains(&v));
        }
    }

    #[test]
    fn scan_catching_up_text() {
        let p = Progress {
            done: 23402,
            total: 23402,
            resolved: 1118,
            keys_total: 2309,
            keys_done: 1118,
            keys_cached: 418,
            keys_fetched: 700,
            folders_scanned: 3,
            ..Progress::default()
        };
        assert!(scan_catching_up(&p));
        let seg = scan_index_meta_text(&p);
        assert_eq!(
            seg,
            "indexed 23402 of 23402 files | metadata 1118 of 2309 - catching up"
        );
        assert!(scan_progress_text(&p).contains(&seg));
        let measure = |s: &str| s.chars().count() as i32;
        assert_eq!(
            compose_scan_live_status(
                &p,
                Duration::from_secs(20),
                Duration::from_secs(20),
                0,
                seg.chars().count() as i32,
                measure
            ),
            seg
        );
        let (show, det, frac) = scan_bar_progress(&p, true);
        assert!(show && det && frac < 1.0);
    }

    #[test]
    fn scan_finished_text_not_catching_up() {
        let mut p = Progress {
            done: 23402,
            total: 23402,
            resolved: 1118,
            keys_total: 2309,
            keys_done: 1118,
            keys_cached: 418,
            keys_fetched: 700,
            folders_scanned: 3,
            ..Progress::default()
        };
        p.resolved = p.keys_total;
        p.keys_done = p.keys_total;
        assert!(!scan_catching_up(&p));
        let seg = scan_index_meta_text(&p);
        assert_eq!(seg, "indexed 23402 of 23402 files | metadata 2309 of 2309");
        assert!(!seg.contains("catching up"));
        let measure = |s: &str| s.chars().count() as i32;
        assert_eq!(
            compose_scan_live_status(
                &p,
                Duration::from_secs(20),
                Duration::from_secs(20),
                0,
                seg.chars().count() as i32,
                measure
            ),
            seg
        );
        let (_, det, frac) = scan_bar_progress(&p, true);
        assert!(det && frac == 1.0);
        let walk = Progress {
            done: 5,
            total: 10,
            folders_scanned: 1,
            ..Progress::default()
        };
        assert!(!scan_catching_up(&walk));
        assert_eq!(scan_index_meta_text(&walk), "indexed 5 of 10 files");
        let (show, det, _) = scan_bar_progress(&walk, true);
        assert!(show && !det);
        assert_eq!(scan_index_meta_text(&Progress::default()), "");
    }

    #[test]
    fn report_decision_branches() {
        let scratch = Scratch::new("report-decision");
        let missing = scratch.join("missing.json");
        let mut st = UiState::default();
        st.status_text = String::new();
        st.on_report_at(NULL, &missing);
        assert_eq!(st.status_text, REPORT_NOTHING_TEXT);
        assert!(st.status_text.to_lowercase().contains("nothing to report"));

        let dir = scratch.join("dir");
        std::fs::create_dir_all(&dir).unwrap();
        repair::report::write_report(&dir, &repair::report::Report::default()).unwrap();
        st.status_text = String::new();
        st.on_report_at(NULL, &dir.join(repair::report::REPORT_JSON_NAME));
        assert_eq!(st.status_text, REPORT_NOTHING_TEXT);

        let bad = scratch.write("bad.json", "{not json");
        st.status_text = String::new();
        st.on_report_at(NULL, &bad);
        assert_eq!(st.status_text, REPORT_NOTHING_TEXT);

        let one = repair::report::Report {
            entries: vec![repair::report::Entry {
                name: "a.mkv".into(),
                ..repair::report::Entry::default()
            }],
            ..repair::report::Report::default()
        };
        assert_eq!(report_decision(&one, false), (true, ""));
        assert_eq!(
            report_decision(&repair::report::Report::default(), false),
            (false, REPORT_NOTHING_TEXT)
        );
        assert_eq!(report_decision(&one, true), (false, REPORT_NOTHING_TEXT));
    }

    #[test]
    fn report_off_thread() {
        let scratch = Scratch::new("report-thread");
        let dir = scratch.join("dir");
        std::fs::create_dir_all(&dir).unwrap();
        repair::report::write_report(
            &dir,
            &repair::report::Report {
                entries: vec![repair::report::Entry {
                    name: "a.mkv".into(),
                    ..repair::report::Entry::default()
                }],
                ..repair::report::Report::default()
            },
        )
        .unwrap();
        const WANT: &str = "https://example.invalid/potflix/issues/1";
        let mut st = UiState::default();
        st.create_issue = Arc::new(|_| {
            std::thread::sleep(Duration::from_millis(300));
            (WANT.to_string(), None)
        });
        st.open_url = Arc::new(|_| false);
        st.status_text = String::new();
        let start = Instant::now();
        st.on_report_at(NULL, &dir.join(repair::report::REPORT_JSON_NAME));
        assert!(
            start.elapsed() < Duration::from_millis(150),
            "on_report_at blocked"
        );
        assert_eq!(st.status_text, REPORT_PENDING_TEXT);
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            if !st.report_done_text.lock().unwrap().is_empty() {
                break;
            }
            assert!(Instant::now() < deadline, "worker never published");
            std::thread::sleep(Duration::from_millis(5));
        }
        st.on_report_done();
        assert_eq!(st.status_text, report_launch_text(WANT));
        assert!(st.status_text.contains(WANT));
    }

    #[test]
    fn clear_actions_are_distinct() {
        assert_ne!(
            crate::settings::CLEAR_METADATA_STATUS,
            crate::settings::clear_index_status(3)
        );
        let scratch = Scratch::new("clear-distinct");
        let base = scratch.path().to_path_buf();
        let meta = base.join("meta");
        std::fs::create_dir_all(&meta).unwrap();
        let meta_s = meta.to_string_lossy().into_owned();
        let key = "movie|alpha|2001";
        metadata::cache::cache_put_raw(
            &meta_s,
            key,
            br#"{"key":"movie|alpha|2001","schema":3,"media":{"title":"Alpha"}}"#,
        )
        .unwrap();
        let idx = base.join("scanindex.json");
        let idx_s = idx.to_string_lossy().into_owned();
        let seed_index = || {
            let ix = library::scanindex::load_scan_index(&idx_s);
            ix.record(
                r"C:\Media",
                "fp-1",
                metadata::cache::CACHE_SCHEMA,
                &[key.to_string()],
            );
            ix.save(&idx_s).unwrap();
        };
        seed_index();

        let (records, err) = crate::settings::clear_scan_index(&idx_s);
        assert!(err.is_ok());
        assert_eq!(records, 1);
        assert!(!std::path::Path::new(&library::index::index_store_path(&idx_s)).exists());
        assert!(metadata::cache::cache_has(&meta_s, key));

        seed_index();
        crate::settings::clear_metadata_cache(&meta_s).unwrap();
        assert!(!metadata::cache::cache_has(&meta_s, key));
        assert_eq!(library::scanindex::load_scan_index(&idx_s).len(), 1);
    }

    #[test]
    fn report_button_layout() {
        let l = compute_layout(&dirs(1), MAIN_CLIENT_W, MAIN_CLIENT_H, 96);
        let by_id = |id: i32| l.btns.iter().find(|b| b.id == id).cloned().unwrap();
        let rep = by_id(BTN_REPORT);
        let scan = by_id(BTN_SCAN);
        let refresh = by_id(BTN_REFRESH);
        assert_eq!(rep.label, REPORT_BUTTON_LABEL);
        assert_eq!(rep.style, refresh.style);
        assert_eq!(rep.rc.left, scan.rc.left);
        assert_eq!(rep.rc.right, scan.rc.right);
        assert!(rep.rc.top > refresh.rc.bottom);
        assert!(rep.rc.bottom <= l.status.top && rep.rc.right <= MAIN_CLIENT_W);
        assert_eq!(
            button_at_point(
                &l.btns,
                (rep.rc.left + rep.rc.right) / 2,
                (rep.rc.top + rep.rc.bottom) / 2
            ),
            BTN_REPORT
        );
        assert!(main_focus_order().contains(&BTN_REPORT));
        assert_eq!(scan_blocked_reason(true, BTN_REPORT), "");
    }

    #[test]
    fn open_url_texts() {
        assert!(report_opened_text(false).contains("opened"));
        assert!(report_opened_text(true).contains("prefilled"));
        let url = "https://github.com/nathwn12/Potflix/issues/new?title=x";
        assert!(report_launch_text(url).contains(url));
        let path = repair_report_path();
        assert!(
            path.to_string_lossy()
                .ends_with(repair::report::REPORT_JSON_NAME)
        );
        assert!(path.to_string_lossy().to_lowercase().contains("potflix"));
    }

    // -- cardview_test.go ---------------------------------------------------

    #[test]
    fn card_view_switch() {
        let mut st = UiState::default();
        st.cur_view = View::Main;
        st.open_card();
        assert_eq!(st.cur_view, View::Card);
        st.close_card();
        assert_eq!(st.cur_view, View::Main);
    }

    #[test]
    fn card_esc() {
        let mut st = UiState::default();
        st.cur_view = View::Card;
        assert!(st.on_settings_key_down(NULL, VK_ESCAPE));
        assert_eq!(st.cur_view, View::Main);
        st.cur_view = View::Card;
        assert!(!st.on_settings_key_down(NULL, 0x41));
        assert_eq!(st.cur_view, View::Card);
    }

    #[test]
    fn card_hit_test() {
        for spec in [
            CardSpec::default(),
            CardSpec {
                details: "title".into(),
                state: "synopsis".into(),
            },
        ] {
            let l = compute_card_layout(MAIN_CLIENT_W, MAIN_CLIENT_H, 96, &spec);
            assert_eq!(l.btns.len(), 11);
            for id in [
                BTN_CARD_BACK,
                card_details_button(0),
                card_details_button(1),
                card_details_button(2),
                card_details_button(3),
                card_state_button(0),
                card_state_button(1),
                card_state_button(2),
                card_state_button(3),
            ] {
                let rc = l.card_btn(id).unwrap_or_else(|| panic!("id {id} missing"));
                assert!(
                    rc.right <= MAIN_CLIENT_W
                        && rc.bottom <= MAIN_CLIENT_H
                        && rc.left >= 0
                        && rc.top >= 0
                );
                assert_eq!(
                    button_at_point(&l.btns, (rc.left + rc.right) / 2, (rc.top + rc.bottom) / 2),
                    id
                );
            }
            for (i, a) in l.btns.iter().enumerate() {
                for b in l.btns.iter().skip(i + 1) {
                    assert!(!rects_intersect(a.rc, b.rc));
                }
            }
            assert_eq!(l.rows.len(), 5);
            for r in &l.rows {
                assert!(
                    r.left >= l.preview.left
                        && r.right <= l.preview.right
                        && r.top >= l.preview.top
                        && r.bottom <= l.preview.bottom
                );
            }
        }
        let a = compute_card_layout(MAIN_CLIENT_W, MAIN_CLIENT_H, 96, &CardSpec::default());
        let b = compute_card_layout(
            MAIN_CLIENT_W,
            MAIN_CLIENT_H,
            96,
            &CardSpec {
                details: "series".into(),
                state: "none".into(),
            },
        );
        for (x, y) in a.btns.iter().zip(b.btns.iter()) {
            assert!(r_eq(x.rc, y.rc), "preset {} moved", x.id);
        }
    }

    #[test]
    fn card_text_invariants() {
        for dpi in [96, 120, 144, 192] {
            let cw = scale_px(dpi, MAIN_CLIENT_W);
            let ch = scale_px(dpi, MAIN_CLIENT_H);
            for spec in [
                CardSpec::default(),
                CardSpec {
                    details: "title".into(),
                    state: "tag".into(),
                },
            ] {
                let l = compute_card_layout(cw, ch, dpi, &spec);
                let rects = card_text_rects(cw, ch, dpi, &l);
                for r in &rects {
                    assert!(
                        r.rc.right - r.rc.left > 0 && r.rc.bottom - r.rc.top > 0,
                        "dpi {dpi}: {} non-positive",
                        r.id
                    );
                    assert!(
                        r.rc.left >= 0 && r.rc.top >= 0 && r.rc.right <= cw && r.rc.bottom <= ch,
                        "dpi {dpi}: {} escapes",
                        r.id
                    );
                }
                for (i, a) in rects.iter().enumerate() {
                    for b in rects.iter().skip(i + 1) {
                        assert!(
                            !rects_intersect(a.rc, b.rc),
                            "dpi {dpi}: {} and {} intersect",
                            a.id,
                            b.id
                        );
                    }
                }
            }
        }
    }

    #[test]
    fn main_card_affordance() {
        let l = compute_layout(&dirs(2), MAIN_CLIENT_W, MAIN_CLIENT_H, 96);
        let s = l.card_link;
        assert!(s.right <= MAIN_CLIENT_W && s.bottom <= MAIN_CLIENT_H && s.left >= 0 && s.top >= 0);
        assert!(s.right > s.left && s.bottom > s.top);
        assert!(s.top >= TAB_TOP_L && s.bottom <= TAB_TOP_L + TAB_H_L);
        assert_eq!(s.right + scale_px(96, SP_2), l.settings.left);
        let rects = main_text_rects(MAIN_CLIENT_W, MAIN_CLIENT_H, 96, &l, 2);
        for (i, a) in rects.iter().enumerate() {
            for b in rects.iter().skip(i + 1) {
                assert!(
                    !rects_intersect(a.rc, b.rc),
                    "{} and {} intersect",
                    a.id,
                    b.id
                );
            }
        }
    }

    #[test]
    fn card_preset_mapping() {
        let (dets, states) = presence::cardspec::card_presets();
        for (i, p) in dets.iter().enumerate() {
            assert_eq!(
                card_preset_for_button(card_details_button(i as i32)),
                Some((0, p.id.clone()))
            );
        }
        for (i, p) in states.iter().enumerate() {
            assert_eq!(
                card_preset_for_button(card_state_button(i as i32)),
                Some((1, p.id.clone()))
            );
        }
        for id in [0, 1, 2, 7, 10, 29, 34, 39, 44, 99] {
            assert!(card_preset_for_button(id).is_none(), "id {id} mapped");
        }
        let spec = CardSpec {
            details: "episode".into(),
            state: "none".into(),
        };
        for p in &dets {
            let want = if p.id == "episode" {
                BTN_TAB_SEL
            } else {
                BTN_TAB
            };
            assert_eq!(card_preset_style(0, &p.id, &spec), want);
        }
        for p in &states {
            let want = if p.id == "none" {
                BTN_TAB_SEL
            } else {
                BTN_TAB
            };
            assert_eq!(card_preset_style(1, &p.id, &spec), want);
        }
    }

    #[test]
    fn preview_rows_quotes_payload() {
        for (i, r) in preview_rows(&Activity::default(), false).iter().enumerate() {
            assert!(!r.is_empty(), "placeholder {i} empty");
        }
        let act = Activity {
            name: "Show Title".into(),
            details: "The Episode".into(),
            state: "S1 E2".into(),
            large_image: "https://example.com/poster.jpg".into(),
            small_image: "play".into(),
            buttons: vec![
                presence::Button {
                    label: "Trailer".into(),
                    url: "https://example.com/trailer".into(),
                },
                presence::Button {
                    label: "IMDb".into(),
                    url: "https://example.com/imdb".into(),
                },
            ],
            ..Activity::default()
        };
        let got = preview_rows(&act, true);
        assert_eq!(
            got,
            [
                "Header: Show Title",
                "Details: The Episode",
                "State: S1 E2",
                "Images: https://example.com/poster.jpg + play",
                "Buttons: Trailer -> https://example.com/trailer | IMDb -> https://example.com/imdb",
            ]
        );
        let idle = Activity {
            details: "Browsing".into(),
            state: "Idle".into(),
            large_image: "logo".into(),
            small_image: "stop".into(),
            ..Activity::default()
        };
        let igot = preview_rows(&idle, true);
        assert_eq!(igot[0], "Header: (Discord app name)");
        assert_eq!(igot[2], "State: Idle");
        assert_eq!(igot[3], "Images: logo + stop");
        assert_eq!(igot[4], "Buttons: none");
        let empty = Activity {
            details: "X".into(),
            large_image: "logo".into(),
            small_image: "stop".into(),
            ..Activity::default()
        };
        assert_eq!(preview_rows(&empty, true)[2], "State: ");
    }

    #[test]
    fn preview_record_round_trip() {
        let mut st = UiState::default();
        let want = Activity {
            name: "T".into(),
            details: "D".into(),
            state: "S".into(),
            large_image: "logo".into(),
            small_image: "play".into(),
            ..Activity::default()
        };
        st.record_preview(want.clone());
        let (got, have) = st.preview_activity();
        assert!(have);
        assert_eq!(got, want);
    }

    #[test]
    fn dead_cache_label_text() {
        assert_eq!(
            crate::settings::dead_cache_label(0),
            "0 titles marked dead - Clear metadata retries them"
        );
        assert_eq!(
            crate::settings::dead_cache_label(5),
            "5 titles marked dead - Clear metadata retries them"
        );
    }

    #[test]
    fn count_negatives_empty_dir() {
        let scratch = Scratch::new("count-neg");
        let dir = scratch.path().to_string_lossy().into_owned();
        assert_eq!(metadata::cache::count_negatives(&dir), 0);
        for name in ["notes.txt", ".tmp-abc.json", "partial"] {
            std::fs::write(scratch.join(name), "{}").unwrap();
        }
        std::fs::write(scratch.join("corrupt.json"), "{not json").unwrap();
        assert_eq!(metadata::cache::count_negatives(&dir), 0);
        for i in 0..2 {
            let body = format!(
                r#"{{"key":"k{i}","schema":3,"not_found":true,"checked_at":"2026-01-01T00:00:00Z"}}"#
            );
            metadata::cache::cache_put_raw(&dir, &format!("k{i}"), body.as_bytes()).unwrap();
        }
        let pos = r#"{"key":"p","schema":3,"media":{"source":"tvmaze","sourceId":"1","kind":"series","title":"S","fetchedAt":"2026-01-01T00:00:00Z"}}"#;
        metadata::cache::cache_put_raw(&dir, "p", pos.as_bytes()).unwrap();
        assert_eq!(metadata::cache::count_negatives(&dir), 2);
        assert_eq!(
            metadata::cache::count_negatives(&scratch.join("missing").to_string_lossy()),
            0
        );
    }

    // -- scan_status_test.go ------------------------------------------------

    #[test]
    fn scan_progress_text_folders() {
        let p = Progress {
            keys_done: 12,
            keys_total: 24,
            keys_fetched: 3,
            keys_cached: 5,
            folders_scanned: 4,
            folders_skipped: 2,
            ..Progress::default()
        };
        assert_eq!(
            scan_progress_text(&p),
            "Scan 12/24 keys | 3 fetched, 5 cached | 4/2 folders"
        );
    }

    #[test]
    fn scan_progress_text_all_cached() {
        let p = Progress {
            keys_done: 24,
            keys_total: 24,
            keys_fetched: 0,
            keys_cached: 24,
            folders_scanned: 6,
            folders_skipped: 1,
            ..Progress::default()
        };
        assert_eq!(
            scan_progress_text(&p),
            "Scan 24/24 keys | 0 fetched, 24 cached | 6/1 folders"
        );
    }

    #[test]
    fn scan_progress_text_four_bucket() {
        let p = Progress {
            keys_done: 10,
            keys_total: 20,
            keys_fetched: 2,
            keys_cached: 7,
            keys_failed: 1,
            folders_scanned: 3,
            folders_skipped: 1,
            ..Progress::default()
        };
        assert_eq!(
            scan_progress_text(&p),
            "Scan 10/20 keys | 2 fetched, 7 cached, 1 failed | 3/1 folders"
        );
    }

    #[test]
    fn scan_progress_text_huge_elides() {
        let p = Progress {
            keys_done: 12_345_678,
            keys_total: 98_765_432,
            keys_fetched: 123_456,
            keys_cached: 12_222_222,
            folders_scanned: 1_234_567,
            folders_skipped: 7_654_321,
            ..Progress::default()
        };
        let got = scan_progress_text(&p);
        assert!(
            !got.contains("12345678") && !got.contains("98765432"),
            "{got}"
        );
        assert!(got.contains("folders") && got.contains('/'));
        assert!(got.len() <= MAX_SCAN_STATUS_LEN, "len {}", got.len());
        // Extreme int64-scale counts stay bounded and keep the suffix.
        let huge = Progress {
            keys_done: 1 << 60,
            keys_total: 1 << 60,
            keys_fetched: 1 << 60,
            keys_cached: 0,
            folders_scanned: 1 << 40,
            folders_skipped: 1 << 40,
            ..Progress::default()
        };
        let got_huge = scan_progress_text(&huge);
        assert!(!got_huge.contains(&(1_i64 << 60).to_string()), "{got_huge}");
        assert!(got_huge.contains("folders"));
        assert!(got_huge.len() <= MAX_SCAN_STATUS_LEN);
    }

    #[test]
    fn scan_done_text_folders() {
        let st = Stats {
            keys: 24,
            keys_cached: 5,
            keys_fetched: 2,
            keys_not_found: 1,
            files: 10,
            folders_scanned: 4,
            folders_skipped: 2,
            ..Stats::default()
        };
        assert_eq!(
            scan_done_text("Scan", &st),
            "Scan done: 24 keys - 5 cached, 2 fetched - 4/2 folders"
        );
        let r = scan_done_text("Refresh", &st);
        assert!(r.starts_with("Refresh done: ") && r.contains("4/2 folders"));
    }

    #[test]
    fn scan_done_text_all_cached() {
        let st = Stats {
            keys: 5,
            keys_cached: 5,
            keys_fetched: 0,
            keys_not_found: 0,
            files: 5,
            folders_scanned: 3,
            folders_skipped: 0,
            ..Stats::default()
        };
        assert_eq!(
            scan_done_text("Scan", &st),
            "Scan done: 5 keys - 5 cached, 0 fetched - 3/0 folders"
        );
    }

    #[test]
    fn scan_done_text_nothing_to_fetch() {
        let st = Stats {
            keys: 7,
            keys_cached: 7,
            keys_fetched: 0,
            keys_not_found: 0,
            files: 9,
            folders_scanned: 4,
            folders_skipped: 3,
            ..Stats::default()
        };
        assert_eq!(
            scan_done_text("Scan", &st),
            "Scan done: nothing to fetch (7 cached) - 3 folders skipped"
        );
        assert_eq!(
            scan_done_text("Refresh", &st),
            "Refresh done: nothing to fetch (7 cached) - 3 folders skipped"
        );
    }

    #[test]
    fn scan_done_text_huge_elides() {
        let st = Stats {
            keys: 12_345_678,
            keys_cached: 12_222_222,
            keys_fetched: 123_456,
            keys_not_found: 999_999,
            files: 2_345_678,
            folders_scanned: 1_234_567,
            folders_skipped: 7_654_321,
            ..Stats::default()
        };
        let got = scan_done_text("Scan", &st);
        assert!(!got.contains("12345678"), "{got}");
        assert!(got.contains("folders"));
        assert!(got.len() <= MAX_SCAN_STATUS_LEN);
    }

    #[test]
    fn scan_status_keeps_folders_suffix() {
        let long = format!("{} 4/2 folders", "k".repeat(MAX_SCAN_STATUS_LEN));
        let got = fit_scan_status(&long);
        assert!(got.len() <= MAX_SCAN_STATUS_LEN, "len {}", got.len());
        assert!(got.contains("4/2 folders"), "{got}");
        let st = Stats {
            keys: 24,
            keys_cached: 5,
            keys_fetched: 2,
            keys_not_found: 1,
            files: 10,
            folders_scanned: 4,
            folders_skipped: 2,
            ..Stats::default()
        };
        let got_done = scan_done_text(&"R".repeat(MAX_SCAN_STATUS_LEN), &st);
        assert!(got_done.len() <= MAX_SCAN_STATUS_LEN);
        assert!(got_done.contains("4/2 folders"));
    }

    #[test]
    fn build_scan_options_contract() {
        let d = vec![r"C:\Media\Movies".to_string(), r"D:\Shows".to_string()];
        let scan = build_scan_options(d.clone(), false, None);
        assert!(scan.use_index);
        assert_eq!(scan.index_path, crate::settings::scan_index_path());
        assert_eq!(scan.cache_dir, crate::settings::metadata_dir());
        assert!(!scan.force);
        assert_eq!(scan.dirs, d);
        let refresh = build_scan_options(d.clone(), true, None);
        assert!(
            refresh.use_index
                && refresh.index_path == crate::settings::scan_index_path()
                && refresh.cache_dir == crate::settings::metadata_dir()
        );
        assert!(refresh.force);
    }

    #[test]
    fn scan_cancelled_text_cases() {
        assert_eq!(
            scan_cancelled_text(12, 24),
            "Scan cancelled - 12/24 keys done, progress kept"
        );
        let huge = scan_cancelled_text(12_345_678, 98_765_432);
        assert!(!huge.contains("12345678") && !huge.contains("98765432"));
        assert!(huge.starts_with("Scan cancelled - ") && huge.ends_with("progress kept"));
        assert!(huge.len() <= MAX_SCAN_STATUS_LEN);
    }

    #[test]
    fn scan_result_cancelled_never_fails() {
        let cancelled = Stats {
            keys: 24,
            keys_done: 12,
            cancelled: true,
            ..Stats::default()
        };
        let got = scan_result_text("Scan", &cancelled, None);
        assert_eq!(got, scan_cancelled_text(12, 24));
        assert!(!got.contains("failed"));
        assert_eq!(
            scan_result_text("Scan", &cancelled, Some(&UiScanError::other("boom"))),
            scan_cancelled_text(12, 24)
        );
        let bare = Stats {
            keys: 10,
            keys_done: 4,
            ..Stats::default()
        };
        assert_eq!(
            scan_result_text("Scan", &bare, Some(&UiScanError::cancelled())),
            scan_cancelled_text(4, 10)
        );
        let ok = Stats {
            keys: 24,
            keys_cached: 5,
            keys_fetched: 2,
            keys_not_found: 1,
            files: 10,
            folders_scanned: 4,
            folders_skipped: 2,
            ..Stats::default()
        };
        assert_eq!(
            scan_result_text("Scan", &ok, None),
            scan_done_text("Scan", &ok)
        );
        assert!(
            scan_result_text("Scan", &ok, Some(&UiScanError::other("disk gone")))
                .contains("failed")
        );
    }

    #[test]
    fn scan_failed_suffix() {
        let clean = Progress {
            keys_done: 12,
            keys_total: 24,
            keys_fetched: 3,
            keys_cached: 5,
            folders_scanned: 4,
            folders_skipped: 2,
            ..Progress::default()
        };
        assert_eq!(
            scan_progress_text(&clean),
            "Scan 12/24 keys | 3 fetched, 5 cached | 4/2 folders"
        );
        let bad = Progress {
            keys_failed: 2,
            ..clean.clone()
        };
        assert_eq!(
            scan_progress_text(&bad),
            "Scan 12/24 keys | 3 fetched, 5 cached, 2 failed | 4/2 folders"
        );
        let st = Stats {
            keys: 7,
            keys_cached: 7,
            keys_fetched: 0,
            keys_not_found: 0,
            files: 9,
            folders_scanned: 4,
            folders_skipped: 3,
            ..Stats::default()
        };
        assert_eq!(
            scan_done_text("Scan", &st),
            "Scan done: nothing to fetch (7 cached) - 3 folders skipped"
        );
        let st_bad = Stats {
            keys_failed: 1,
            ..st
        };
        let got = scan_done_text("Scan", &st_bad);
        assert!(!got.contains("nothing to fetch"), "{got}");
        assert!(got.contains("1 failed"), "{got}");
    }

    #[test]
    fn scan_control_state() {
        assert_eq!(scan_button_label(false), "Scan");
        assert!(scan_refresh_enabled(false));
        assert_eq!(scan_button_label(true), "Cancel");
        assert!(!scan_refresh_enabled(true));
        assert_eq!(scan_button_label(false), "Scan");
        assert!(scan_refresh_enabled(false));
    }

    // -- settings_scan_test.go ----------------------------------------------

    #[test]
    fn settings_scan_hit_test() {
        let l = compute_settings_layout(MAIN_CLIENT_W, MAIN_CLIENT_H, 96, false, "balanced");
        assert_eq!(l.btns.len(), 12);
        for id in [BTN_SCAN_GENTLE, BTN_SCAN_BALANCED, BTN_SCAN_TURBO] {
            let rc = l
                .settings_btn(id)
                .unwrap_or_else(|| panic!("scan id {id} missing"));
            assert!(
                rc.right <= MAIN_CLIENT_W
                    && rc.bottom <= MAIN_CLIENT_H
                    && rc.left >= 0
                    && rc.top >= 0
            );
            assert_eq!(
                button_at_point(&l.btns, (rc.left + rc.right) / 2, (rc.top + rc.bottom) / 2),
                id
            );
        }
        let g = l.settings_btn(BTN_SCAN_GENTLE).unwrap();
        let b = l.settings_btn(BTN_SCAN_BALANCED).unwrap();
        let u = l.settings_btn(BTN_SCAN_TURBO).unwrap();
        assert!(g.top == b.top && b.top == u.top && g.bottom == b.bottom && b.bottom == u.bottom);
        assert!(g.right <= b.left && b.right <= u.left);
        for (i, a) in l.btns.iter().enumerate() {
            for c in l.btns.iter().skip(i + 1) {
                assert!(!rects_intersect(a.rc, c.rc));
            }
        }
        for id in [BTN_SCAN_GENTLE, BTN_SCAN_BALANCED, BTN_SCAN_TURBO] {
            let rc = l.settings_btn(id).unwrap();
            assert!(
                rc.top >= l.labels[2].rc.bottom && rc.bottom <= l.hint.top,
                "scan {id} {rc:?} left the SCAN SPEED group"
            );
        }
    }

    #[test]
    fn scan_segment_model() {
        assert_eq!(crate::settings::scan_segment_label("gentle"), "Gentle");
        assert_eq!(crate::settings::scan_segment_label("balanced"), "Balanced");
        assert_eq!(crate::settings::scan_segment_label("turbo"), "Turbo");
        assert_eq!(crate::settings::scan_segment_label("bogus"), "Turbo");
        for (profile, id) in [
            ("gentle", BTN_SCAN_GENTLE),
            ("balanced", BTN_SCAN_BALANCED),
            ("turbo", BTN_SCAN_TURBO),
        ] {
            assert_eq!(crate::settings::scan_button_for_profile(profile), id);
            assert_eq!(crate::settings::scan_profile_for_button(id), profile);
        }
        assert_eq!(crate::settings::scan_profile_for_button(BTN_BACK), "");
        for cur in ["gentle", "balanced", "turbo"] {
            for seg in ["gentle", "balanced", "turbo"] {
                let want = if seg == cur { BTN_TAB_SEL } else { BTN_TAB };
                assert_eq!(crate::settings::scan_segment_style(seg, cur), want);
            }
        }
        for cur in ["", "   ", "bogus"] {
            assert_eq!(
                crate::settings::scan_button_for_profile(cur),
                BTN_SCAN_TURBO
            );
            assert_eq!(
                crate::settings::scan_segment_style("turbo", cur),
                BTN_TAB_SEL
            );
            assert_eq!(
                crate::settings::scan_segment_style("balanced", cur),
                BTN_TAB
            );
        }
        assert!(
            crate::settings::SCAN_HINT_TEXT.contains("Turbo")
                && crate::settings::SCAN_HINT_TEXT.contains("(default)")
        );
    }

    #[test]
    fn mouse_xy_splits_lparam() {
        assert_eq!(mouse_xy(0x00C8_0064), (0x64, 0x00C8));
    }

    /// The Scan/Refresh worker is real (`go\window.go:2661-2674`): with a fake
    /// runner (no network) over a temp dir, `start_library_scan` invokes the
    /// runner with options built from the configured dirs, the progress
    /// callback publishes into the shared snapshot, and the UI-thread landing
    /// clears `scan_active`/`scan_cancel` with a terminal line and honest
    /// counters. Reverting the worker body to the stub leaves the runner
    /// uninvoked and the snapshot empty, so this test goes red.
    #[test]
    fn library_scan_worker_runs_runner_and_lands_done() {
        let tmp = Scratch::new("libscan");
        tmp.write("Show.S01E02.Pilot.1080p.mkv", "x");
        let dir = tmp.path().to_string_lossy().into_owned();

        let mut st = UiState::default();
        st.arm_timer = Arc::new(|_| {});
        st.kill_timer = Arc::new(|_| {});
        let d = dir.clone();
        st.scan_dirs = Arc::new(move || vec![d.clone()]);

        let ran = Arc::new(AtomicBool::new(false));
        let seen_dirs = Arc::new(Mutex::new(Vec::new()));
        let seen_force = Arc::new(AtomicBool::new(false));
        let r = ran.clone();
        let sd = seen_dirs.clone();
        let sf = seen_force.clone();
        st.scan_run = Arc::new(move |_cancel: &library::Cancel, opts: library::Options| {
            r.store(true, Ordering::SeqCst);
            *sd.lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner) = opts.dirs.clone();
            sf.store(opts.force, Ordering::SeqCst);
            if let Some(progress) = &opts.progress {
                progress(Progress {
                    keys_total: 4,
                    keys_done: 1,
                    keys_cached: 1,
                    folders_scanned: 1,
                    ..Progress::default()
                });
            }
            Ok(Stats {
                keys: 4,
                keys_done: 4,
                keys_cached: 4,
                files: 1,
                unique: 1,
                resolved: 1,
                cached: 1,
                folders_scanned: 1,
                ..Stats::default()
            })
        });

        st.start_library_scan(NULL, false);
        assert!(st.scan_active, "run must latch active until done lands");
        assert!(st.scan_cancel.is_some());

        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            let landed = st
                .scan_shared
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner)
                .stats
                .keys
                == 4;
            if landed || Instant::now() > deadline {
                break;
            }
            std::thread::sleep(Duration::from_millis(5));
        }
        assert!(ran.load(Ordering::SeqCst), "worker must invoke the runner");
        assert_eq!(
            *seen_dirs
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner),
            vec![dir],
            "options must be built from the configured dirs"
        );
        assert!(!seen_force.load(Ordering::SeqCst));

        // The real progress callback published into the shared snapshot.
        assert_eq!(
            st.scan_shared
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner)
                .progress
                .keys_done,
            1
        );

        // The UI-thread landing, exactly as the wndproc arms run it.
        st.publish_scan_progress();
        assert_eq!(st.scan_progress.keys_done, 1);
        assert_eq!(st.scan_progress.keys_total, 4);
        st.publish_scan_done();
        assert_eq!(st.scan_stats.keys, 4);
        st.on_scan_done(NULL);
        assert!(!st.scan_active, "done must unlatch the run");
        assert!(st.scan_cancel.is_none());
        assert!(st.scan_stats.keys > 0, "counters must be honest");
        assert_eq!(st.status_text, scan_done_text("Scan", &st.scan_stats));
    }

    /// The `WM_APP_SCAN_PROGRESS` / `WM_APP_SCAN_DONE` arms publish the shared
    /// snapshot before doing their existing work, driven through the REAL
    /// `main_wnd_proc` against the process-wide `UI` global. This is the
    /// real-path proof the headless `--selftest` cannot carry: a live scan
    /// ends in `start_catch_up`, whose background thread performs live
    /// provider lookups and writes the owner's real index/cache, so neither
    /// `--selftest` nor `cargo test` may run the default runner. Assertions
    /// cover the full terminal state, never the status line alone.
    #[test]
    fn scan_messages_land_through_real_wndproc() {
        let _ui_serial = UI_GLOBAL_SERIAL
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let now = Instant::now();
        {
            let mut st = ui();
            st.arm_timer = Arc::new(|_| {});
            st.kill_timer = Arc::new(|_| {});
            st.scan_active = true;
            st.scan_cancel = Some(library::Cancel::background());
            st.scan_force = false;
            st.scan_start = now - Duration::from_secs(30);
            st.scan_last_paint = now - Duration::from_secs(3600);
            st.scan_resolve_start = None;
            st.scan_resolve_base = 0;
            st.scan_progress = Progress::default();
            st.scan_stats = Stats::default();
            st.scan_err = None;
            *st.scan_shared
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner) = ScanSnapshot {
                progress: Progress {
                    keys_total: 4,
                    keys_done: 1,
                    keys_cached: 1,
                    folders_scanned: 1,
                    ..Progress::default()
                },
                stats: Stats::default(),
                err: None,
            };
            st.status_text = "Scan starting...".into();
        }
        // SAFETY: scan arms touch no controls; NULL posts are no-ops.
        unsafe {
            main_wnd_proc(NULL, WM_APP_SCAN_PROGRESS, 0, 0);
        }
        {
            let st = ui();
            assert_eq!(st.scan_progress.keys_done, 1);
            assert_eq!(st.scan_progress.keys_total, 4);
            assert!(
                st.status_text.starts_with("Scan "),
                "live line, got {:?}",
                st.status_text
            );
            assert!(st.scan_active);
        }
        let terminal = Stats {
            keys: 4,
            keys_done: 4,
            keys_cached: 4,
            files: 1,
            unique: 1,
            resolved: 1,
            cached: 1,
            folders_scanned: 1,
            ..Stats::default()
        };
        {
            let st = ui();
            *st.scan_shared
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner) = ScanSnapshot {
                progress: Progress {
                    keys_total: 4,
                    keys_done: 4,
                    keys_cached: 4,
                    folders_scanned: 1,
                    ..Progress::default()
                },
                stats: terminal,
                err: None,
            };
        }
        // SAFETY: scan arms touch no controls; NULL posts are no-ops.
        unsafe {
            main_wnd_proc(NULL, WM_APP_SCAN_DONE, 0, 0);
        }
        {
            let st = ui();
            assert!(!st.scan_active, "done must unlatch the run");
            assert!(st.scan_cancel.is_none());
            assert_eq!(st.scan_stats.keys, 4);
            assert!(st.scan_stats.keys_done > 0, "counters must be honest");
            assert_eq!(st.status_text, scan_done_text("Scan", &terminal));
        }
        // Leave the global pristine for the next serial holder.
        {
            let mut st = ui();
            st.arm_timer = Arc::new(default_arm_timer);
            st.kill_timer = Arc::new(default_kill_timer);
            st.scan_run = Arc::new(default_scan_run);
            st.scan_dirs = Arc::new(default_scan_dirs);
            st.scan_shared = Arc::new(Mutex::new(ScanSnapshot::default()));
            st.scan_active = false;
            if let Some(c) = st.scan_cancel.take() {
                c.cancel();
            }
            st.scan_force = false;
            st.scan_progress = Progress::default();
            st.scan_stats = Stats::default();
            st.scan_err = None;
            st.scan_resolve_start = None;
            st.scan_resolve_base = 0;
            st.scan_start = Instant::now();
            st.scan_last_paint = Instant::now();
            st.status_text = "Ready".into();
        }
    }
}
