//go:build linux

package linuxsubreaper

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"syscall"
	"time"
)

const (
	quietInventoryCount              = 2
	gracefulTerminationBudgetDivisor = 3
)

func retireOwnedTree(
	authority *inventoryAuthority,
	wait <-chan terminalResult,
	rootTerminal *terminalResult,
	cleanupDeadline time.Time,
) (*terminalResult, bool, error) {
	grace := time.Until(cleanupDeadline)
	if grace <= 0 {
		return rootTerminal, false, errors.New("termination cleanup deadline must be in the future")
	}
	started := time.Now()
	gracefulDeadline := started.Add(grace / gracefulTerminationBudgetDivisor)
	var cleanupFailure error
	if err := authority.signalTracked(unix.SIGTERM); err != nil {
		cleanupFailure = errors.Join(cleanupFailure, fmt.Errorf("signal tracked owned processes with SIGTERM: %w", err))
	}
	quiet, rootTerminal, err := waitForQuiet(
		authority, wait, rootTerminal, gracefulDeadline, unix.SIGTERM,
	)
	cleanupFailure = errors.Join(cleanupFailure, err)
	if quiet {
		return rootTerminal, true, cleanupFailure
	}
	if err := authority.signalTracked(unix.SIGKILL); err != nil {
		cleanupFailure = errors.Join(cleanupFailure, fmt.Errorf("signal tracked owned processes with SIGKILL: %w", err))
	}
	quiet, rootTerminal, err = waitForQuiet(
		authority, wait, rootTerminal, cleanupDeadline, unix.SIGKILL,
	)
	cleanupFailure = errors.Join(cleanupFailure, err)
	if !quiet {
		cleanupFailure = errors.Join(cleanupFailure, errors.New("owned process tree remained nonempty after bounded SIGKILL cleanup"))
	}
	return rootTerminal, quiet, cleanupFailure
}

func waitForQuiet(
	authority *inventoryAuthority,
	wait <-chan terminalResult,
	terminal *terminalResult,
	deadline time.Time,
	signal unix.Signal,
) (bool, *terminalResult, error) {
	quiet := 0
	rootTerminal := terminal
	var evidenceFailure error
	for time.Now().Before(deadline) {
		if rootTerminal == nil {
			select {
			case result := <-wait:
				rootTerminal = &result
			default:
			}
		}
		noChildren := false
		if rootTerminal != nil {
			var reapErr error
			noChildren, reapErr = reapAdoptedChildren()
			if reapErr != nil {
				evidenceFailure = errors.Join(evidenceFailure, reapErr)
			}
		}
		inventory, err := authority.refresh()
		switch {
		case err != nil:
			evidenceFailure = errors.Join(evidenceFailure, err)
			quiet = 0
			_ = authority.signalTracked(signal)
		case len(inventory) == 0 && rootTerminal != nil && noChildren:
			quiet++
			if quiet >= quietInventoryCount {
				return true, rootTerminal, evidenceFailure
			}
		default:
			quiet = 0
			if err := authority.signalInventory(inventory, signal); err != nil {
				evidenceFailure = errors.Join(evidenceFailure, err)
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		time.Sleep(min(inventoryPollInterval, remaining))
	}
	return false, rootTerminal, evidenceFailure
}

func reapAdoptedChildren() (bool, error) {
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.ECHILD) {
			return true, nil
		}
		if pid == 0 {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("reap adopted child: %w", err)
		}
	}
}
