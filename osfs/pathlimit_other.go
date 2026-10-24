//go:build !windows

package osfs

import (
	"io/fs"
)

func isReparsePoint(information fs.FileInfo) bool {
	// Lstat reports symbolic links directly on POSIX, so this is the complete
	// no-follow boundary corresponding to Windows reparse-point rejection.
	return information.Mode()&fs.ModeSymlink != 0
}
