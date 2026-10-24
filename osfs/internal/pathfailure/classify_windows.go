package pathfailure

import (
	"errors"
	"syscall"
)

// ERROR_FILENAME_EXCED_RANGE is returned by Win32 APIs that reject a path or
// component before Go can translate the failure to syscall.ENAMETOOLONG.
const windowsErrorFilenameExceedsRange syscall.Errno = 206

func IsTooLong(err error) bool {
	return errors.Is(err, syscall.ENAMETOOLONG) || errors.Is(err, windowsErrorFilenameExceedsRange)
}
