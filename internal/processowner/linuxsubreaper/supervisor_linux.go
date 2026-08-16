//go:build linux

// Package linuxsubreaper owns one test process tree behind a Linux subreaper.
package linuxsubreaper

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/windshare/windshare/internal/processowner"
	"github.com/windshare/windshare/internal/testtrace"
	"golang.org/x/sys/unix"
)

const (
	descendantPollInterval            = 10 * time.Millisecond
	descendantEmptyConfirmationPolls  = 5
	descendantEmptyConfirmationWindow = (descendantEmptyConfirmationPolls - 1) * descendantPollInterval
)

type waitResult struct {
	exitCode int64
	signal   string
	err      error
}

// Run launches the configured target in a private process group, adopts any
// orphaned descendants, and does not publish its terminal result until every
// descendant has been reaped.
func Run(
	config processowner.Config,
	status *os.File,
	control *os.File,
	events *os.File,
) (resultErr error) {
	if status == nil || control == nil || events == nil {
		return errors.New("linux process-owner endpoints are incomplete")
	}
	if err := processowner.ValidateConfig(config); err != nil {
		return err
	}
	unix.CloseOnExec(int(status.Fd()))
	unix.CloseOnExec(int(control.Fd()))
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("enable Linux child subreaper: %w", err)
	}

	command := exec.Command(config.Executable, config.Arguments...)
	command.Dir = config.WorkingDirectory
	command.Env = replaceEnvironment(
		config.Environment,
		map[string]string{
			testtrace.EventFDEnvironment:     "3",
			testtrace.EventHandleEnvironment: "",
		},
	)
	command.Stdin = nil
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.ExtraFiles = []*os.File{events}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if err := command.Start(); err != nil {
		_ = events.Close()
		result := processowner.Result{Reason: processowner.ReasonSpawnFailed, Error: diagnostic(err)}
		return processowner.WriteStatus(status, processowner.Status{
			State: processowner.StatusFinished, Result: &result,
		})
	}
	rootPID := command.Process.Pid
	defer func() { _ = command.Process.Release() }()
	if err := events.Close(); err != nil {
		_ = forceTree(rootPID)
		_, _ = waitCommand(command)
		return fmt.Errorf("close Linux owner event endpoint: %w", err)
	}
	if err := processowner.WriteStatus(status, processowner.Status{State: processowner.StatusStarted}); err != nil {
		_ = forceTree(rootPID)
		_, _ = waitCommand(command)
		return fmt.Errorf("publish Linux process readiness: %w", err)
	}

	observeRoot := func() (bool, error) { return rootExited(rootPID) }
	decision := awaitInitialOutcome(
		time.Now().Add(time.Duration(config.DeadlineMilliseconds)*time.Millisecond),
		observeRoot,
		func() (trigger, bool, error) { return observeControl(control) },
		time.Now,
		func(maximum time.Duration) error { return waitForControl(control, maximum) },
	)
	var outcome waitResult
	var retirementErr error
	if decision.rootSettled {
		var waitErr error
		outcome, waitErr = waitCommand(command)
		outcome.err = waitErr
	} else {
		outcome, retirementErr = interruptThenWait(
			command,
			rootPID,
			observeRoot,
			time.Duration(config.TerminationGraceMilliseconds)*time.Millisecond,
		)
	}
	cleanupErr := cleanupDescendants(
		rootPID,
		time.Duration(config.TerminationGraceMilliseconds)*time.Millisecond,
	)
	result := processowner.Result{
		ExitCode:     &outcome.exitCode,
		Signal:       outcome.signal,
		Reason:       decision.reason,
		Error:        diagnostic(errors.Join(outcome.err, decision.controlErr, retirementErr)),
		CleanupError: diagnostic(cleanupErr),
	}
	return processowner.WriteStatus(status, processowner.Status{
		State: processowner.StatusFinished, Result: &result,
	})
}

func waitCommand(command *exec.Cmd) (waitResult, error) {
	err := command.Wait()
	if command.ProcessState == nil {
		return waitResult{exitCode: -1}, fmt.Errorf("wait for Linux target produced no process state: %w", err)
	}
	result := waitResult{exitCode: int64(command.ProcessState.ExitCode())}
	if status, ok := command.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		result.signal = status.Signal().String()
	}
	var exitError *exec.ExitError
	if err != nil && !errors.As(err, &exitError) {
		return result, fmt.Errorf("wait for Linux target: %w", err)
	}
	return result, nil
}

func rootExited(rootPID int) (bool, error) {
	info := unix.Siginfo{}
	if err := unix.Waitid(
		unix.P_PID,
		rootPID,
		&info,
		unix.WEXITED|unix.WNOHANG|unix.WNOWAIT,
		nil,
	); err != nil {
		return false, fmt.Errorf("observe Linux target: pid=%d: %w", rootPID, err)
	}
	return info.Signo != 0, nil
}

func interruptThenWait(
	command *exec.Cmd,
	rootPID int,
	observeRoot rootObserver,
	grace time.Duration,
) (waitResult, error) {
	var retirementErr error
	if err := signalProcessGroup(rootPID, syscall.SIGINT); err != nil {
		retirementErr = errors.Join(retirementErr, fmt.Errorf(
			"interrupt Linux target: phase=interrupt pid=%d: %w",
			rootPID,
			err,
		))
	}
	settled, err := waitForRootExit(observeRoot, grace)
	if err != nil {
		retirementErr = errors.Join(retirementErr, fmt.Errorf(
			"observe Linux target: phase=interrupt_wait pid=%d: %w",
			rootPID,
			err,
		))
	}
	if settled {
		outcome, waitErr := waitCommand(command)
		outcome.err = waitErr
		return outcome, retirementErr
	}
	if err := forceTree(rootPID); err != nil {
		retirementErr = errors.Join(retirementErr, fmt.Errorf(
			"terminate Linux process tree: phase=force pid=%d: %w",
			rootPID,
			err,
		))
	}
	settled, err = waitForRootExit(observeRoot, grace)
	if err != nil {
		retirementErr = errors.Join(retirementErr, fmt.Errorf(
			"observe Linux target: phase=forced_wait pid=%d: %w",
			rootPID,
			err,
		))
	}
	if !settled {
		return waitResult{
			exitCode: -1,
			err: fmt.Errorf(
				"linux target did not exit after forced termination: phase=forced_wait pid=%d",
				rootPID,
			),
		}, retirementErr
	}
	outcome, waitErr := waitCommand(command)
	outcome.err = waitErr
	return outcome, retirementErr
}

func waitForRootExit(observeRoot rootObserver, maximum time.Duration) (bool, error) {
	deadline := time.Now().Add(maximum)
	for {
		settled, err := observeRoot()
		if err != nil || settled {
			return settled, err
		}
		now := time.Now()
		if !now.Before(deadline) {
			return false, nil
		}
		pause := min(deadline.Sub(now), lifecyclePollInterval)
		time.Sleep(pause)
	}
}

func observeControl(reader *os.File) (trigger, bool, error) {
	ready, err := controlReady(reader, 0)
	if err != nil || !ready {
		return trigger{}, false, err
	}
	return readControl(reader), true, nil
}

func waitForControl(reader *os.File, maximum time.Duration) error {
	_, err := controlReady(reader, maximum)
	return err
}

func controlReady(reader *os.File, maximum time.Duration) (bool, error) {
	poll := []unix.PollFd{{
		Fd:     int32(reader.Fd()),
		Events: unix.POLLIN | unix.POLLHUP | unix.POLLERR,
	}}
	timeout := unix.NsecToTimespec(maximum.Nanoseconds())
	observed, err := unix.Ppoll(poll, &timeout, nil)
	if errors.Is(err, syscall.EINTR) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("poll Linux process control: fd=%d: %w", reader.Fd(), err)
	}
	if poll[0].Revents&unix.POLLNVAL != 0 {
		return false, fmt.Errorf("poll Linux process control: fd=%d revents=%#x", reader.Fd(), poll[0].Revents)
	}
	return observed > 0 && poll[0].Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) != 0, nil
}

func readControl(reader io.Reader) trigger {
	var command [1]byte
	_, err := io.ReadFull(reader, command[:])
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return trigger{reason: processowner.ReasonParentLost}
		}
		return trigger{reason: processowner.ReasonStop, err: fmt.Errorf("read process control: %w", err)}
	}
	switch command[0] {
	case processowner.ControlInterrupt:
		return trigger{reason: processowner.ReasonInterrupt}
	case processowner.ControlStop:
		return trigger{reason: processowner.ReasonStop}
	default:
		return trigger{reason: processowner.ReasonStop, err: errors.New("process control command is invalid")}
	}
}

func forceTree(rootPID int) error {
	groupErr := signalProcessGroup(rootPID, syscall.SIGKILL)
	descendants, scanErr := descendantPIDs(os.Getpid())
	var signalErr error
	for _, processID := range descendants {
		if err := syscall.Kill(processID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			signalErr = errors.Join(signalErr, err)
		}
	}
	return errors.Join(groupErr, scanErr, signalErr)
}

func signalProcessGroup(rootPID int, signal syscall.Signal) error {
	err := syscall.Kill(-rootPID, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func cleanupDescendants(rootPID int, maximum time.Duration) error {
	deadline := time.Now().Add(maximum)
	stableEmpty := stableEmptyTracker{}
	var descendants []int
	var cleanupErr error
	for {
		if err := reapAdoptedChildren(); err != nil {
			return errors.Join(cleanupErr, err)
		}
		observedDescendants, err := descendantPIDs(os.Getpid())
		if err != nil {
			return errors.Join(cleanupErr, err)
		}
		descendants = observedDescendants
		now := time.Now()
		if len(descendants) == 0 {
			// Orphan reparenting and /proc visibility do not form one atomic
			// observation. An elapsed stable-empty window keeps terminal status
			// causally after subreaper adoption even when polling is delayed.
			if stableEmpty.observe(true, now, descendantEmptyConfirmationWindow) {
				return cleanupErr
			}
		} else {
			stableEmpty.observe(false, now, descendantEmptyConfirmationWindow)
			for _, processID := range descendants {
				if err := syscall.Kill(processID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
					cleanupErr = errors.Join(cleanupErr, fmt.Errorf(
						"terminate Linux descendant: phase=stable_empty pid=%d: %w",
						processID,
						err,
					))
				}
			}
		}
		if !now.Before(deadline) {
			return errors.Join(cleanupErr, fmt.Errorf(
				"linux descendants did not become stably empty: phase=stable_empty root_pid=%d active_descendants=%v stable_empty_for=%s required=%s",
				rootPID,
				descendants,
				stableEmpty.elapsed(now),
				descendantEmptyConfirmationWindow,
			))
		}
		pause := min(deadline.Sub(now), descendantPollInterval)
		time.Sleep(pause)
	}
}

func reapAdoptedChildren() error {
	for {
		var status syscall.WaitStatus
		processID, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if err != nil {
			if errors.Is(err, syscall.ECHILD) {
				return nil
			}
			return fmt.Errorf("reap Linux descendant: phase=stable_empty: %w", err)
		}
		if processID <= 0 {
			return nil
		}
	}
}

func descendantPIDs(ownerPID int) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read Linux process inventory: %w", err)
	}
	children := make(map[int][]int)
	for _, entry := range entries {
		processID, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil || processID == ownerPID {
			continue
		}
		encoded, readErr := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if readErr != nil {
			if ignorableProcessStatReadError(readErr) {
				continue
			}
			return nil, fmt.Errorf("read Linux process %d: %w", processID, readErr)
		}
		parentPID, parseErr := parseParentPID(string(encoded))
		if parseErr != nil {
			continue
		}
		children[parentPID] = append(children[parentPID], processID)
	}
	queue := append([]int(nil), children[ownerPID]...)
	result := make([]int, 0, len(queue))
	for len(queue) > 0 {
		processID := queue[0]
		queue = queue[1:]
		result = append(result, processID)
		queue = append(queue, children[processID]...)
	}
	return result, nil
}

func ignorableProcessStatReadError(err error) bool {
	// A process can exit after ReadDir exposes its PID but before procfs serves
	// stat. Linux reports that race as either ENOENT or ESRCH; stable-empty
	// confirmation performs later inventory cuts, so neither is cleanup failure.
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) ||
		errors.Is(err, os.ErrPermission)
}

func parseParentPID(stat string) (int, error) {
	closing := strings.LastIndexByte(stat, ')')
	if closing < 0 || closing+1 >= len(stat) {
		return 0, errors.New("linux process stat has no command terminator")
	}
	fields := strings.Fields(stat[closing+1:])
	if len(fields) < 2 {
		return 0, errors.New("linux process stat has no parent PID")
	}
	parentPID, err := strconv.Atoi(fields[1])
	if err != nil || parentPID < 0 {
		return 0, errors.New("linux process stat parent PID is invalid")
	}
	return parentPID, nil
}

func replaceEnvironment(base []string, replacements map[string]string) []string {
	environment := make([]string, 0, len(base)+len(replacements))
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		if _, replace := replacements[name]; !replace {
			environment = append(environment, entry)
		}
	}
	for name, value := range replacements {
		if value != "" {
			environment = append(environment, name+"="+value)
		}
	}
	return environment
}

func diagnostic(err error) string {
	if err == nil {
		return ""
	}
	const maximumDiagnosticBytes = 2048
	message := err.Error()
	if len(message) > maximumDiagnosticBytes {
		return message[:maximumDiagnosticBytes]
	}
	return message
}
