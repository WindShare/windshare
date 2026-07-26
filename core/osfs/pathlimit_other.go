//go:build !windows

package osfs

import (
	"errors"
	"io/fs"
	"syscall"
)

// linuxMaxPath remains the native absolute-placement bound used by the v3
// ext4 certifier; the removed portable path heuristic had no production caller.
const linuxMaxPath = 4096

func isReparsePoint(information fs.FileInfo) bool {
	// Lstat reports symbolic links directly on POSIX, so this is the complete
	// no-follow boundary corresponding to Windows reparse-point rejection.
	return information.Mode()&fs.ModeSymlink != 0
}

func isPathTooLongError(err error) bool {
	return errors.Is(err, syscall.ENAMETOOLONG)
}
