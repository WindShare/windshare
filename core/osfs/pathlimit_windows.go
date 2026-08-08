package osfs

import (
	"io/fs"
	"syscall"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputwindows"
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

func openNativeOutputPlatform(path string, create bool) (outputcap.Platform, error) {
	return outputwindows.Open(path, create)
}
