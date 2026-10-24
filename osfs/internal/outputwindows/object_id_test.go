//go:build windows

package outputwindows

import (
	"errors"
	"sync/atomic"

	"golang.org/x/sys/windows"
)

type windowsObjectIDMutationTrap struct {
	calls atomic.Int64
}

func (trap *windowsObjectIDMutationTrap) CreateOrGet(
	windows.Handle,
) (windowsPersistentObjectID, error) {
	trap.calls.Add(1)
	return windowsPersistentObjectID{}, errors.New("unexpected object ID mutation")
}
