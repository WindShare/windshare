//go:build linux

package main

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"sync"
	"time"
)

const inventoryPollInterval = 25 * time.Millisecond

type trackedProcess struct {
	identity processIdentity
	pidfd    int
}

type inventoryAuthority struct {
	ownerPID           int
	tracked            map[string]*trackedProcess
	scans              int
	maximumDescendants int
	mu                 sync.Mutex
}

func newInventoryAuthority(ownerPID int) *inventoryAuthority {
	return &inventoryAuthority{ownerPID: ownerPID, tracked: make(map[string]*trackedProcess)}
}

func (authority *inventoryAuthority) refresh() ([]processIdentity, error) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	inventory, err := descendantInventory(authority.ownerPID, authority.tracked)
	if err != nil {
		return nil, err
	}
	authority.scans++
	if len(inventory) > authority.maximumDescendants {
		authority.maximumDescendants = len(inventory)
	}
	for _, identity := range inventory {
		if err := authority.trackLocked(identity); err != nil {
			return nil, err
		}
	}
	return inventory, nil
}

func (authority *inventoryAuthority) track(identity processIdentity) error {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	return authority.trackLocked(identity)
}

func (authority *inventoryAuthority) trackLocked(identity processIdentity) error {
	key := identityKey(identity)
	if _, exists := authority.tracked[key]; exists {
		return nil
	}
	pidfd, err := unix.PidfdOpen(identity.PID, 0)
	if err != nil {
		return fmt.Errorf("open pidfd for %s: %w", key, err)
	}
	current, err := readStableProcessIdentity(identity.PID)
	if err != nil || current.StartTimeTicks != identity.StartTimeTicks {
		var identityFailure error
		if err != nil {
			identityFailure = fmt.Errorf("revalidate pidfd identity %s: %w", key, err)
		} else {
			identityFailure = fmt.Errorf("pid %d was reused while acquiring pidfd", identity.PID)
		}
		closeErr := unix.Close(pidfd)
		if closeErr != nil {
			closeErr = fmt.Errorf("close unauthenticated pidfd for %s: %w", key, closeErr)
		}
		// Identity authentication is authoritative; descriptor cleanup can add
		// evidence but must never replace the reason authority was refused.
		return errors.Join(identityFailure, closeErr)
	}
	authority.tracked[key] = &trackedProcess{identity: identity, pidfd: pidfd}
	return nil
}

func (authority *inventoryAuthority) signalAll(signal unix.Signal) error {
	inventory, err := authority.refresh()
	if err != nil {
		return err
	}
	return authority.signalInventory(inventory, signal)
}

func (authority *inventoryAuthority) signalTracked(signal unix.Signal) error {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	var failures error
	for key, tracked := range authority.tracked {
		if err := unix.PidfdSendSignal(tracked.pidfd, signal, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
			failures = errors.Join(failures, fmt.Errorf("signal tracked descendant %s: %w", key, err))
		}
	}
	return failures
}

func (authority *inventoryAuthority) signalInventory(
	inventory []processIdentity,
	signal unix.Signal,
) error {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	for _, identity := range inventory {
		tracked := authority.tracked[identityKey(identity)]
		if tracked == nil {
			return fmt.Errorf("descendant %s lacks an authenticated pidfd", identityKey(identity))
		}
		if err := unix.PidfdSendSignal(tracked.pidfd, signal, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
			return fmt.Errorf("signal descendant %s: %w", identityKey(identity), err)
		}
	}
	return nil
}

func (authority *inventoryAuthority) close() {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	for _, tracked := range authority.tracked {
		_ = unix.Close(tracked.pidfd)
	}
	authority.tracked = nil
}
