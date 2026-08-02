//go:build linux

package linuxsubreaper

import (
	"bufio"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const inventoryPollInterval = 25 * time.Millisecond

type trackedProcess struct {
	identity processIdentity
	pidfd    int
}

type terminationSignalWitness struct {
	accepted int
	terminal int
}

func (witness terminationSignalWitness) applied() bool {
	return witness.accepted > 0
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
	candidates, err := directChildInventory(authority.ownerPID)
	if err != nil {
		return nil, err
	}
	authority.scans++
	inventory := make([]processIdentity, 0, len(candidates))
	for _, candidate := range candidates {
		identity, retained, err := authority.trackLocked(candidate)
		if err != nil {
			return nil, err
		}
		if retained {
			inventory = append(inventory, identity)
		}
	}
	if len(inventory) > authority.maximumDescendants {
		authority.maximumDescendants = len(inventory)
	}
	return inventory, nil
}

func (authority *inventoryAuthority) track(identity processIdentity) error {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	_, retained, err := authority.trackLocked(identity)
	if err != nil {
		return err
	}
	if !retained {
		return errors.New("owned process vanished before its pidfd authority was retained")
	}
	return nil
}

func (authority *inventoryAuthority) trackLocked(candidate processIdentity) (processIdentity, bool, error) {
	key := identityKey(candidate)
	if tracked, exists := authority.tracked[key]; exists {
		pid, err := pidfdProcessID(tracked.pidfd)
		if err != nil {
			return processIdentity{}, false, fmt.Errorf("authenticate retained pidfd for %s: %w", key, err)
		}
		if pid == candidate.PID {
			return tracked.identity, true, nil
		}
		_ = unix.Close(tracked.pidfd)
		delete(authority.tracked, key)
	}
	pidfd, err := unix.PidfdOpen(candidate.PID, 0)
	if err != nil {
		if errors.Is(err, unix.ESRCH) || errors.Is(err, unix.ENOENT) {
			return processIdentity{}, false, nil
		}
		return processIdentity{}, false, fmt.Errorf("open pidfd for %s: %w", key, err)
	}
	current, err := readStableProcessIdentity(candidate.PID)
	if errors.Is(err, os.ErrNotExist) {
		_ = unix.Close(pidfd)
		return processIdentity{}, false, nil
	}
	if err != nil {
		_ = unix.Close(pidfd)
		return processIdentity{}, false, fmt.Errorf("revalidate pidfd identity %s: %w", key, err)
	}
	pidfdPID, pidfdErr := pidfdProcessID(pidfd)
	if pidfdErr != nil || !pidfdAuthenticatesDirectChild(pidfdPID, current, authority.ownerPID) {
		var identityFailure error
		switch {
		case pidfdErr != nil:
			identityFailure = fmt.Errorf("authenticate pidfd identity %s: %w", key, pidfdErr)
		case pidfdPID < 0:
			_ = unix.Close(pidfd)
			return processIdentity{}, false, nil
		case pidfdPID != current.PID:
			identityFailure = fmt.Errorf("pid %d was reused while acquiring pidfd", candidate.PID)
		default:
			// A candidate that was reparented or recycled before authentication is
			// outside this snapshot; a later scan may discover the current child.
			_ = unix.Close(pidfd)
			return processIdentity{}, false, nil
		}
		closeErr := unix.Close(pidfd)
		if closeErr != nil {
			closeErr = fmt.Errorf("close unauthenticated pidfd for %s: %w", key, closeErr)
		}
		// Identity authentication is authoritative; descriptor cleanup can add
		// evidence but must never replace the reason authority was refused.
		return processIdentity{}, false, errors.Join(identityFailure, closeErr)
	}
	key = identityKey(current)
	authority.tracked[key] = &trackedProcess{identity: current, pidfd: pidfd}
	return current, true, nil
}

func pidfdAuthenticatesDirectChild(pidfdPID int, current processIdentity, ownerPID int) bool {
	return pidfdPID == current.PID && current.PPID == ownerPID
}

func pidfdProcessID(pidfd int) (int, error) {
	file, err := os.Open(filepath.Join("/proc/self/fdinfo", strconv.Itoa(pidfd)))
	if err != nil {
		return 0, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		name, value, found := strings.Cut(scanner.Text(), ":")
		if !found || name != "Pid" {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return 0, fmt.Errorf("parse pidfd process id: %w", err)
		}
		return pid, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New("pidfd metadata omitted its process id")
}

func (authority *inventoryAuthority) signalAll(signal unix.Signal) error {
	inventory, err := authority.refresh()
	if err != nil {
		return err
	}
	return authority.signalInventory(inventory, signal)
}

func (authority *inventoryAuthority) signalTracked(signal unix.Signal) error {
	_, err := authority.signalTrackedWithWitness(signal)
	return err
}

func (authority *inventoryAuthority) signalTrackedWithWitness(
	signal unix.Signal,
) (terminationSignalWitness, error) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	witness := terminationSignalWitness{}
	var failures error
	for key, tracked := range authority.tracked {
		err := unix.PidfdSendSignal(tracked.pidfd, signal, nil, 0)
		switch {
		case err == nil:
			witness.accepted++
		case errors.Is(err, unix.ESRCH):
			witness.terminal++
		default:
			failures = errors.Join(failures, fmt.Errorf("signal tracked owned process %s: %w", key, err))
		}
	}
	return witness, failures
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
