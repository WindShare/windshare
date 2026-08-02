//go:build windows

package perfevidence

import (
	"os"
	"syscall"
)

// isReparsePointInfo treats junctions and cloud placeholders as links even
// when the Go mode bits do not expose ModeSymlink. Cleanup and source
// inventory must never descend through one of those objects.
func isReparsePointInfo(info os.FileInfo) bool {
	if info == nil || info.Mode()&os.ModeSymlink != 0 {
		return true
	}
	native, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && native.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}
