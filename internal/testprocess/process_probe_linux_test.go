//go:build linux

package testprocess

import (
	"errors"
	"fmt"
	"syscall"
)

func requireProcessGone(processID int) error {
	err := syscall.Kill(processID, 0)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("descendant process %d is still running", processID)
}
