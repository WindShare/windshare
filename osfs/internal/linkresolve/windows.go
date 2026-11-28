//go:build windows

package linkresolve

import (
	"errors"
	"strings"

	"golang.org/x/sys/windows"
)

const (
	WindowsExtendedPrefix        = `\\?\`
	WindowsExtendedUNCPrefix     = `\\?\UNC\`
	WindowsNTNamespacePrefix     = `\??\`
	WindowsDeviceNamespacePrefix = `\\.\`

	WindowsVolumeNameDOS  uint32 = 0x0
	WindowsVolumeNameGUID uint32 = 0x1

	initialFinalPathUnits = 512
)

// HasWindowsExtendedOrDevicePrefix reports whether path already carries an NT
// (\??\), extended-length (\\?\), or device (\\.\) namespace prefix.
func HasWindowsExtendedOrDevicePrefix(path string) bool {
	if len(path) < 4 {
		return false
	}
	if strings.HasPrefix(path, WindowsNTNamespacePrefix) {
		return true
	}
	isSep := func(c byte) bool { return c == '\\' || c == '/' }
	return isSep(path[0]) && isSep(path[1]) && (path[2] == '?' || path[2] == '.') && isSep(path[3])
}

// QueryWindowsFinalPath calls GetFinalPathNameByHandle with dynamic buffer
// growth for paths exceeding MAX_PATH.
func QueryWindowsFinalPath(handle windows.Handle, flags uint32) (string, error) {
	buffer := make([]uint16, initialFinalPathUnits)
	for {
		length, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), flags)
		if err != nil {
			return "", err
		}
		if length < uint32(len(buffer)) {
			return windows.UTF16ToString(buffer[:length]), nil
		}
		buffer = make([]uint16, length+1)
	}
}

// WindowsFinalPathByHandle resolves the canonical final path of an already-open
// handle, falling back to its stable volume GUID path when the volume is
// mounted without a drive letter.
func WindowsFinalPathByHandle(handle windows.Handle) (string, error) {
	return WindowsFinalPathByHandleWith(func(flags uint32) (string, error) {
		return QueryWindowsFinalPath(handle, flags)
	})
}

// WindowsFinalPathByHandleWith runs the DOS-to-GUID fallback and prefix
// normalization against an injected query function for unit testing.
func WindowsFinalPathByHandleWith(query func(flags uint32) (string, error)) (string, error) {
	resolved, err := query(WindowsVolumeNameDOS)
	if errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		// Volumes mounted at a junction without an assigned drive letter have no
		// DOS path; fall back to their stable volume GUID path.
		resolved, err = query(WindowsVolumeNameGUID)
	}
	if err != nil {
		return "", err
	}
	return NormalizeWindowsFinalPath(resolved), nil
}

// NormalizeWindowsFinalPath strips \\?\ and \\?\UNC\ prefixes from standard
// drive and UNC paths while preserving volume GUID paths (\\?\Volume{...}\).
func NormalizeWindowsFinalPath(path string) string {
	if rest, ok := strings.CutPrefix(path, WindowsExtendedUNCPrefix); ok {
		return `\\` + rest
	}
	if rest, ok := strings.CutPrefix(path, WindowsExtendedPrefix); ok &&
		len(rest) >= 3 && rest[1] == ':' && rest[2] == '\\' {
		return rest
	}
	return path
}

// IsWindowsUnresolvableLinkError reports Windows errors indicating a symlink /
// reparse cycle, corrupted reparse data, or an unhandled reparse target.
func IsWindowsUnresolvableLinkError(err error) bool {
	return errors.Is(err, windows.ERROR_CANT_RESOLVE_FILENAME) ||
		errors.Is(err, windows.ERROR_INVALID_REPARSE_DATA) ||
		errors.Is(err, windows.ERROR_REPARSE_TAG_INVALID) ||
		errors.Is(err, windows.ERROR_REPARSE_TAG_MISMATCH) ||
		errors.Is(err, windows.ERROR_CANT_ACCESS_FILE)
}
