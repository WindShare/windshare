//go:build windows

package windowsjob

import (
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"os"
	"time"
)

const (
	maxWindowsProcessID        uint64 = 1<<32 - 1
	windowsStillActiveExitCode uint32 = 259
)

type rootLifecycleAuthority interface {
	processID() uint32
	retainExitAuthority() (processExitAuthority, error)
}

type managedRoot struct {
	handle windows.Handle
	pid    uint32
}

type processExitAuthority interface {
	processID() uint32
	verifyJobMembership(windows.Handle) error
	exactExitCode(time.Duration) (uint32, error)
	close()
}

type managedProcessExitAuthority struct {
	handle windows.Handle
	pid    uint32
}

func openProcessExitAuthority(processID uint32) (processExitAuthority, error) {
	handle, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE,
		false,
		processID,
	)
	if err != nil {
		return nil, err
	}
	authority := &managedProcessExitAuthority{handle: handle, pid: processID}
	actualProcessID, err := windows.GetProcessId(handle)
	if err != nil {
		authority.close()
		return nil, err
	}
	if actualProcessID != processID {
		authority.close()
		return nil, errors.New("opened process identity does not match the requested PID")
	}
	return authority, nil
}

func (authority *managedProcessExitAuthority) processID() uint32 {
	return authority.pid
}

func (authority *managedProcessExitAuthority) verifyJobMembership(job windows.Handle) error {
	member, err := isProcessInJob(authority.handle, job)
	if err != nil {
		return err
	}
	if !member {
		return errors.New("retained process is no longer a member of the managed Job Object")
	}
	return nil
}

func (authority *managedProcessExitAuthority) exactExitCode(maximumWait time.Duration) (uint32, error) {
	var waitMilliseconds uint64
	if maximumWait > 0 {
		waitMilliseconds = uint64(maximumWait / time.Millisecond)
		if maximumWait%time.Millisecond != 0 {
			waitMilliseconds++
		}
	}
	if waitMilliseconds >= uint64(windows.INFINITE) {
		waitMilliseconds = uint64(windows.INFINITE) - 1
	}
	result, err := windows.WaitForSingleObject(authority.handle, uint32(waitMilliseconds))
	if err != nil {
		return 0, fmt.Errorf("wait for retained process %d: %w", authority.pid, err)
	}
	if result != windows.WAIT_OBJECT_0 {
		return 0, fmt.Errorf("retained process %d was not signaled after Job completion", authority.pid)
	}
	var exitCode uint32
	if err := windows.GetExitCodeProcess(authority.handle, &exitCode); err != nil {
		return 0, fmt.Errorf("read retained process %d exit code: %w", authority.pid, err)
	}
	// GetExitCodeProcess overloads 259 as STILL_ACTIVE, but the signaled retained
	// handle above is stronger lifecycle evidence and makes a real exit code 259
	// unambiguous. Rejecting it would discard exact evidence for a valid process.
	return exitCode, nil
}

func (authority *managedProcessExitAuthority) close() {
	if authority.handle != 0 && authority.handle != windows.InvalidHandle {
		_ = windows.CloseHandle(authority.handle)
		authority.handle = 0
	}
}

func closeOwnedProcessHandle(handle windows.Handle, operation string) error {
	if handle == 0 || handle == windows.InvalidHandle {
		return nil
	}
	if err := windows.CloseHandle(handle); err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return nil
}

func rootHandleFromEvent(job managedJob, event launcherEvent, launcher *os.Process) (windows.Handle, error) {
	if uint64(uintptr(event.ProcessHandle)) != event.ProcessHandle {
		return 0, errors.New("launcher-local root process handle does not fit this architecture")
	}
	sourceHandle := windows.Handle(uintptr(event.ProcessHandle))
	if sourceHandle == 0 || sourceHandle == windows.InvalidHandle {
		return 0, errors.New("launcher-local root process handle is invalid")
	}
	var handle windows.Handle
	var duplicateErr error
	withHandleErr := launcher.WithHandle(func(launcherHandle uintptr) {
		duplicateErr = windows.DuplicateHandle(
			windows.Handle(launcherHandle),
			sourceHandle,
			windows.CurrentProcess(),
			&handle,
			0,
			false,
			windows.DUPLICATE_SAME_ACCESS,
		)
	})
	if withHandleErr != nil {
		return 0, fmt.Errorf("access launcher for root handle transfer: %w", withHandleErr)
	}
	if duplicateErr != nil {
		return 0, fmt.Errorf("transfer root handle from launcher: %w", duplicateErr)
	}
	if handle == 0 || handle == windows.InvalidHandle {
		return 0, errors.New("transferred root process handle is invalid")
	}
	pid, err := windows.GetProcessId(handle)
	if err != nil {
		_ = windows.CloseHandle(handle)
		return 0, fmt.Errorf("read duplicated root PID: %w", err)
	}
	if pid != event.PID {
		_ = windows.CloseHandle(handle)
		return 0, errors.New("duplicated root handle PID does not match launcher event")
	}
	member, err := isProcessInJob(handle, job.handle)
	if err != nil {
		_ = windows.CloseHandle(handle)
		return 0, err
	}
	if !member {
		_ = windows.CloseHandle(handle)
		return 0, errors.New("root process did not inherit the managed Job Object")
	}
	return handle, nil
}

func duplicateLocalProcessHandle(source windows.Handle) (windows.Handle, error) {
	var duplicate windows.Handle
	if err := windows.DuplicateHandle(
		windows.CurrentProcess(),
		source,
		windows.CurrentProcess(),
		&duplicate,
		0,
		false,
		windows.DUPLICATE_SAME_ACCESS,
	); err != nil {
		return 0, err
	}
	if duplicate == 0 || duplicate == windows.InvalidHandle {
		return 0, errors.New("duplicated process handle is invalid")
	}
	return duplicate, nil
}

func (root managedRoot) processID() uint32 {
	return root.pid
}

func (root managedRoot) retainExitAuthority() (processExitAuthority, error) {
	handle, err := duplicateLocalProcessHandle(root.handle)
	if err != nil {
		return nil, err
	}
	authority := &managedProcessExitAuthority{handle: handle, pid: root.pid}
	processID, err := windows.GetProcessId(handle)
	if err != nil {
		authority.close()
		return nil, err
	}
	if processID != root.pid {
		authority.close()
		return nil, errors.New("retained root process identity changed")
	}
	return authority, nil
}

func waitRootAndClose(handle windows.Handle, pid uint32) (status rootStatus, resultErr error) {
	defer func() {
		// A successful wait is not settled while its durable identity handle leaks.
		// Preserve the wait failure as the primary cause and append cleanup evidence.
		resultErr = errors.Join(resultErr, closeOwnedProcessHandle(handle, "close exact root process handle"))
	}()
	result, err := windows.WaitForSingleObject(handle, windows.INFINITE)
	if err != nil {
		return rootStatus{}, fmt.Errorf("wait for root process: %w", err)
	}
	if result != windows.WAIT_OBJECT_0 {
		return rootStatus{}, fmt.Errorf("wait for root process returned %#x", result)
	}
	var exitCode uint32
	if err := windows.GetExitCodeProcess(handle, &exitCode); err != nil {
		return rootStatus{}, fmt.Errorf("read exact root exit code: %w", err)
	}
	return rootStatus{PID: pid, ExitCode: exitCode}, nil
}
