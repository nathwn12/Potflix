//go:build !windows

package store

import "os"

// openStoreFile opens path read-write, creating it when it is absent. A POSIX
// unlink does not block on an open handle, so the portable open already meets
// the removal contract the package doc states; it keeps the package liftable
// off Windows.
func openStoreFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
}
