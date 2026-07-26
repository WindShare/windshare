package osfs

import (
	"errors"
	"io/fs"
	"syscall"
)

// ERROR_FILENAME_EXCED_RANGE is the Win32 error returned by APIs that reject a
// path or component before Go can translate it to syscall.ENAMETOOLONG.
const windowsErrorFilenameExceedsRange syscall.Errno = 206

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

func isPathTooLongError(err error) bool {
	return errors.Is(err, syscall.ENAMETOOLONG) || errors.Is(err, windowsErrorFilenameExceedsRange)
}
