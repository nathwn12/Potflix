package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// tray.go - hidden window + Shell_NotifyIconW + GetMessageW loop.
//
// Contract (section 1): tray built before config/loop; tooltip "Potflix";
// one menu item "Exit Potflix"; add failure is non-fatal. Java used a
// SystemTray; here the icon lives on a hidden top-level window whose message
// loop pumps the tray callbacks.

const trayClassName = "PotflixTrayWnd"
const trayUID = 1

// appLog is the process logger, read from the window proc / menu callbacks.
var appLog *logger

// clearActivity is the M1 stub for the clear-on-exit deviation (fleet decision
// 6): the Java tray Exit did a bare System.exit(0), leaving a stale Discord
// card. M3 replaces this body with a real IPC clear.
var clearActivity = func() {
	appLog.logf("cleared (exit)")
}

// createTrayWindow registers the window class, creates the hidden window, loads
// the embedded icon, and adds the notify icon. It must be called on the same
// OS thread that later pumps messages.
func createTrayWindow() (hwnd uintptr, icon uintptr, added bool, err error) {
	hInst, _, _ := procGetModuleHandleW.Call(0)
	className, _ := syscall.UTF16PtrFromString(trayClassName)

	wc := wndClassEx{
		cbSize:        uint32(unsafe.Sizeof(wndClassEx{})),
		lpfnWndProc:   syscall.NewCallback(wndProc),
		hInstance:     hInst,
		lpszClassName: className,
	}
	if atom, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		return 0, 0, false, callErr
	}

	// style 0 = hidden, never shown; zero size.
	hwnd, _, callErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		0, 0,
		0, 0, 0, 0,
		0, 0, hInst, 0,
	)
	if hwnd == 0 {
		return 0, 0, false, callErr
	}

	icon = loadEmbeddedIcon()
	added = shellNotifyIcon(nimAdd, makeNotifyIconData(hwnd, icon))
	return hwnd, icon, added, nil
}

// loadEmbeddedIcon builds an HICON straight from the embedded .ico bytes.
// The ICO holds one BMP/DIB frame, which is exactly the "resource bits" shape
// CreateIconFromResourceEx consumes, so no temp file is needed.
func loadEmbeddedIcon() uintptr {
	h, _ := loadEmbeddedIconDiag()
	return h
}

func loadEmbeddedIconDiag() (uintptr, error) {
	const entrySize = 16
	if len(iconICO) < 6+entrySize {
		return 0, fmt.Errorf("icon bytes too short: %d", len(iconICO))
	}
	count := int(binary.LittleEndian.Uint16(iconICO[4:6]))
	if count < 1 {
		return 0, fmt.Errorf("ico frame count = %d", count)
	}
	size := int(binary.LittleEndian.Uint32(iconICO[6+8 : 6+12])) // dwBytesInRes
	off := int(binary.LittleEndian.Uint32(iconICO[6+12 : 6+16])) // dwImageOffset
	if off < 6+entrySize || size <= 0 || off+size > len(iconICO) {
		return 0, fmt.Errorf("bad ico entry: off=%d size=%d total=%d", off, size, len(iconICO))
	}
	data := iconICO[off : off+size] // subslice of a global: stays alive/put
	h, _, callErr := procCreateIconFromResourceEx.Call(
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(size),
		1, // fIcon
		iconResourceVersion,
		0, 0, 0,
	)
	if h == 0 {
		return 0, fmt.Errorf("CreateIconFromResourceEx(off=%d size=%d) failed: %v", off, size, callErr)
	}
	return h, nil
}

func makeNotifyIconData(hwnd, icon uintptr) notifyIconData {
	var nid notifyIconData
	nid.cbSize = uint32(unsafe.Sizeof(notifyIconData{}))
	nid.hWnd = hwnd
	nid.uID = trayUID
	nid.uFlags = nifMessage | nifIcon | nifTip
	nid.uCallbackMessage = wmAppTray
	nid.hIcon = icon
	copy(nid.szTip[:], syscall.StringToUTF16("Potflix"))
	return nid
}

func shellNotifyIcon(verb uintptr, nid notifyIconData) bool {
	r, _, _ := procShellNotifyIconW.Call(verb, uintptr(unsafe.Pointer(&nid)))
	return r != 0
}

func trayDelete(hwnd uintptr) {
	shellNotifyIcon(nimDelete, makeNotifyIconData(hwnd, 0))
}

// messageLoop runs GetMessageW/TranslateMessage/DispatchMessageW until WM_QUIT.
func messageLoop() {
	var msg msgT
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) == -1 {
			return // GetMessage error
		}
		if r == 0 {
			return // WM_QUIT
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

// trayMouseMessage extracts the mouse/context message from a Shell_NotifyIcon
// callback. Both shell conventions carry it in LOWORD(lParam): legacy (uVersion
// 0, which is what this app gets - it never calls NIM_SETVERSION) sends
// wParam = icon id and lParam = the mouse message; NOTIFYICON_VERSION_4 sends
// LOWORD(lParam) = the event (a mouse message or NIN_*), HIWORD(lParam) = icon
// id, and wParam = anchor coordinates (NOTIFYICONDATAW docs). The owner's real
// right-click delivered wParam=1, lParam=WM_RBUTTONUP - the legacy shape - so
// reading LOWORD(lParam) is the honest fix; reading wParam here was the bug.
// LOWORD(wParam) is checked only as a defensive fallback, so a future version
// or loader change that moved the message cannot silently kill the menu again.
// Returns wmNull when neither word is a menu-opening message.
func trayMouseMessage(wparam, lparam uintptr) uint16 {
	if m := uint16(lparam & 0xFFFF); opensTrayMenu(m) {
		return m
	}
	if m := uint16(wparam & 0xFFFF); opensTrayMenu(m) {
		return m
	}
	return wmNull
}

// opensTrayMenu reports whether m is a message that should open the tray menu.
func opensTrayMenu(m uint16) bool {
	return m == wmRButtonUp || m == wmContextMenu
}

func wndProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	switch msg {
	case wmAppTray:
		switch trayMouseMessage(wparam, lparam) {
		case wmRButtonUp, wmContextMenu:
			showTrayMenu(hwnd)
		}
		return 0
	case wmAppQuit:
		procPostQuitMessage.Call(0)
		return 0
	case wmClose:
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return r
}

func showTrayMenu(hwnd uintptr) {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)

	openLabel, _ := syscall.UTF16PtrFromString("Open Potflix")
	procAppendMenuW.Call(menu, mfString, 2, uintptr(unsafe.Pointer(openLabel)))

	label, _ := syscall.UTF16PtrFromString("Exit Potflix")
	procAppendMenuW.Call(menu, mfString, 1, uintptr(unsafe.Pointer(label)))

	var p point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	procSetForegroundWindow.Call(hwnd)
	sel, _, _ := procTrackPopupMenu.Call(
		menu,
		tpmRightButton|tpmBottomAlign|tpmReturnCmd,
		uintptr(p.x), uintptr(p.y), 0, hwnd, 0,
	)
	procPostMessageW.Call(hwnd, wmNull, 0, 0)
	if int(sel) == 2 {
		showMainWindow()
	}
	if int(sel) == 1 {
		onExit()
	}
}

func onExit() {
	appLog.logf("exit requested (tray menu)")
	clearActivity()
	os.Exit(0)
}
