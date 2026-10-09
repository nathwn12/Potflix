//go:build windows

package bundle

import "syscall"

// fileAttributeReparsePoint is FILE_ATTRIBUTE_REPARSE_POINT (0x400): set on
// symlinks, junctions, and other reparse points.
const fileAttributeReparsePoint = 0x400

// dirHasReparseAttribute reports whether path carries the Windows reparse
// attribute, without following it. It catches junctions, which Lstat alone
// may not flag. An unstatable path reports false.
func dirHasReparseAttribute(path string) bool {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	attrs, err := syscall.GetFileAttributes(p)
	if err != nil {
		return false
	}
	return attrs&fileAttributeReparsePoint != 0
}
