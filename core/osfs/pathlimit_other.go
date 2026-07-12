//go:build !windows

package osfs

import (
	"errors"
	"runtime"
	"syscall"
)

const (
	linuxMaxPath        = 4096
	portableUnixMaxPath = 1024
)

func exceedsPathLimit(abs string) bool {
	// Linux and the BSD-derived platforms expose different PATH_MAX values.
	// Applying the smaller value everywhere would reject valid Linux outputs,
	// while applying Linux's value everywhere would create paths ordinary macOS
	// APIs cannot reopen.
	maxPath := portableUnixMaxPath
	if runtime.GOOS == "linux" {
		maxPath = linuxMaxPath
	}
	return len(abs)+1 > maxPath
}

func isPathTooLongError(err error) bool {
	return errors.Is(err, syscall.ENAMETOOLONG)
}
