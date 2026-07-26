//go:build windows

package osfs

import (
	"errors"

	"golang.org/x/sys/windows"
)

func v3RecoveryAncestorReplacementMustBeBlocked() bool {
	return true
}

func v3RecoveryIsBlockedAncestorReplacement(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
