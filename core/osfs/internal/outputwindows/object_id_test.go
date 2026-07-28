//go:build windows

package outputwindows

import (
	"errors"
	"sync/atomic"

	"golang.org/x/sys/windows"
)

type windowsV3ObjectIDMutationTrap struct {
	calls atomic.Int64
}

func (trap *windowsV3ObjectIDMutationTrap) CreateOrGet(
	windows.Handle,
) (windowsV3PersistentObjectID, error) {
	trap.calls.Add(1)
	return windowsV3PersistentObjectID{}, errors.New("unexpected object ID mutation")
}
