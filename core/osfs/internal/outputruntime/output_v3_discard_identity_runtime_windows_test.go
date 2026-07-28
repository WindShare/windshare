//go:build windows

package outputruntime

import (
	"errors"

	"golang.org/x/sys/windows"
)

func runtimeDiscardAncestorReplacementMustBeBlocked() bool { return true }

func runtimeDiscardIsBlockedAncestorReplacement(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
