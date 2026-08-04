//go:build windows

package outputruntime

import (
	"errors"

	"golang.org/x/sys/windows"
)

func runtimeDiscardAncestorReplacementMustBeBlocked() bool {
	// The portable capability model deliberately permits the namespace move so
	// short tests exercise the runtime's post-move identity revalidation. Native
	// Windows denial remains covered by the named long durability owner.
	return false
}

func runtimeDiscardIsBlockedAncestorReplacement(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
