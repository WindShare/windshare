package osfs

import (
	"io/fs"
	"syscall"
)

func isReparsePoint(information fs.FileInfo) bool {
	if information.Mode()&fs.ModeSymlink != 0 {
		return true
	}
	// Junctions, mount points, and cloud placeholders do not consistently map
	// to ModeSymlink, so the native attribute is the authoritative no-follow
	// boundary on Windows.
	native, ok := information.Sys().(*syscall.Win32FileAttributeData)
	return ok && native.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}
