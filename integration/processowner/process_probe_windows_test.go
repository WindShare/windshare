//go:build windows

package processowner_test

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/windows"
)

type processProbe struct {
	handle windows.Handle
}

func retainProcessProbe(processID int) (*processProbe, error) {
	if processID < 1 || uint64(processID) > uint64(^uint32(0)) {
		return nil, errors.New("process probe PID is invalid")
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(processID))
	if err != nil {
		return nil, err
	}
	return &processProbe{handle: handle}, nil
}

func (probe *processProbe) waitRetired(timeout time.Duration) error {
	if probe == nil || probe.handle == 0 || timeout <= 0 || timeout > time.Duration(^uint32(0))*time.Millisecond {
		return errors.New("process probe wait authority is invalid")
	}
	result, err := windows.WaitForSingleObject(probe.handle, uint32(timeout.Milliseconds()))
	if err != nil {
		return err
	}
	switch result {
	case windows.WAIT_OBJECT_0:
		return nil
	case uint32(windows.WAIT_TIMEOUT):
		return errors.New("process remained active beyond the probe deadline")
	default:
		return fmt.Errorf("process probe returned unexpected wait result %#x", result)
	}
}

func (probe *processProbe) close() {
	if probe != nil && probe.handle != 0 {
		_ = windows.CloseHandle(probe.handle)
		probe.handle = 0
	}
}
