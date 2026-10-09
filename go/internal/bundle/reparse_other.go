//go:build !windows

package bundle

// dirHasReparseAttribute is the non-Windows stub: reparse points do not
// exist there, and the Lstat ModeSymlink check in bundle.go already covers
// symlinks on every platform.
func dirHasReparseAttribute(path string) bool { return false }
