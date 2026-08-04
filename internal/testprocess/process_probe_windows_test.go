//go:build windows

package testprocess

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func requireProcessGone(processID int) error {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(processID))
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(handle)
	state, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		return err
	}
	if state == uint32(windows.WAIT_TIMEOUT) {
		return fmt.Errorf("descendant process %d is still running", processID)
	}
	return nil
}
