//go:build !windows

package pathfailure

import (
	"errors"
	"syscall"
)

func IsTooLong(err error) bool {
	return errors.Is(err, syscall.ENAMETOOLONG)
}
