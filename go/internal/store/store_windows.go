//go:build windows

package store

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// openStoreFile opens path read-write, creating it when it is absent.
//
// It exists for the share mode. os.OpenFile asks Windows for sharing of
// FILE_SHARE_READ|FILE_SHARE_WRITE only, so the file cannot be deleted for as
// long as the DB holds it: a test's t.TempDir cleanup, the app's own clear
// actions over library.idx, or any other party's delete fails with "being used
// by another process". FILE_SHARE_DELETE is the missing half - the other
// party's delete succeeds at once, Windows marks the file for deletion and
// unlinks it, and the handle left here points at the orphaned data. Holding
// the store open therefore never blocks removal.
//
// The cost is that a write into that orphaned file still reports success, so
// the durability claims check the path and report ErrRemoved; see the package
// doc's "Removal while open".
//
// Everything else matches os.OpenFile with os.O_RDWR|os.O_CREATE: the file is
// created when absent, never truncated, opened synchronously, with no file
// attributes set.
func openStoreFile(path string) (*os.File, error) {
	namep, err := syscall.UTF16PtrFromString(longPath(path))
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := syscall.CreateFile(
		namep,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// longPath applies the \\?\ prefix os.OpenFile applies to a long absolute
// path, because syscall.CreateFile is handed the name verbatim.
func longPath(path string) string {
	if len(path) < 248 || strings.HasPrefix(path, `\\?\`) || filepath.VolumeName(path) == "" {
		return path
	}
	if strings.HasPrefix(path, `\\`) {
		return `\\?\UNC\` + path[2:]
	}
	return `\\?\` + path
}
