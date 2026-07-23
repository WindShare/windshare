//go:build !windows && !plan9

package osfs

import (
	"errors"
	"syscall"
)

func isDirectoryNotEmptyError(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}
