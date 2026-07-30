//go:build linux

package main

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"syscall"
	"time"
)

const quietInventoryCount = 2

func retireOwnedTree(
	authority *inventoryAuthority,
	wait <-chan terminalResult,
	rootTerminal *terminalResult,
	rootProcess *os.Process,
	rootPID int,
	grace time.Duration,
) (*terminalResult, error) {
	if grace <= 0 {
		return rootTerminal, errors.New("termination grace must be positive")
	}
	var cleanupFailure error
	if err := authority.signalTracked(unix.SIGTERM); err != nil {
		cleanupFailure = errors.Join(cleanupFailure, fmt.Errorf("signal tracked descendants with SIGTERM: %w", err))
	}
	signalRootFallback(rootProcess, rootPID, rootTerminal, unix.SIGTERM)
	quiet, rootTerminal, err := waitForQuiet(
		authority, wait, rootTerminal, rootProcess, rootPID, grace, unix.SIGTERM,
	)
	cleanupFailure = errors.Join(cleanupFailure, err)
	if quiet && cleanupFailure == nil {
		return rootTerminal, nil
	}
	for !quiet {
		if err := authority.signalTracked(unix.SIGKILL); err != nil {
			cleanupFailure = errors.Join(cleanupFailure, fmt.Errorf("signal tracked descendants with SIGKILL: %w", err))
		}
		signalRootFallback(rootProcess, rootPID, rootTerminal, unix.SIGKILL)
		quiet, rootTerminal, err = waitForQuiet(
			authority, wait, rootTerminal, rootProcess, rootPID, grace, unix.SIGKILL,
		)
		cleanupFailure = errors.Join(cleanupFailure, err)
	}
	return rootTerminal, cleanupFailure
}

func waitForQuiet(
	authority *inventoryAuthority,
	wait <-chan terminalResult,
	terminal *terminalResult,
	rootProcess *os.Process,
	rootPID int,
	maximumWait time.Duration,
	signal unix.Signal,
) (bool, *terminalResult, error) {
	deadline := time.Now().Add(maximumWait)
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
			signalRootFallback(rootProcess, rootPID, rootTerminal, signal)
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
			signalRootFallback(rootProcess, rootPID, rootTerminal, signal)
		}
		time.Sleep(inventoryPollInterval)
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

func bestEffortKill(process *os.Process) {
	if process != nil {
		_ = process.Kill()
	}
}

func signalRootFallback(
	process *os.Process,
	rootPID int,
	terminal *terminalResult,
	signal unix.Signal,
) {
	// The PID fallback is cleanup-only and can never contribute to treeEmpty.
	// Before Wait returns, the child PID cannot have been recycled.
	if terminal == nil && process != nil {
		_ = process.Signal(signal)
		// PGID equals the unreaped wrapper PID until Wait returns, so reuse is
		// impossible in this branch. Post-Wait cleanup uses pidfds only.
		_ = unix.Kill(-rootPID, signal)
	}
}
