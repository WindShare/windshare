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
	descendantPollInterval           = 10 * time.Millisecond
	descendantEmptyConfirmationPolls = 5
)

type trigger struct {
	reason string
	err    error
}

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

	waited := make(chan waitResult, 1)
	go func() {
		outcome, err := waitCommand(command)
		outcome.err = err
		waited <- outcome
	}()
	controls := make(chan trigger, 1)
	go readControl(control, controls)
	deadline := time.NewTimer(time.Duration(config.DeadlineMilliseconds) * time.Millisecond)
	defer deadline.Stop()

	reason := processowner.ReasonNatural
	var controlErr error
	var outcome waitResult
	select {
	case outcome = <-waited:
	case requested := <-controls:
		reason, controlErr = requested.reason, requested.err
		outcome = interruptThenWait(
			rootPID,
			waited,
			time.Duration(config.TerminationGraceMilliseconds)*time.Millisecond,
		)
	case <-deadline.C:
		reason = processowner.ReasonDeadline
		outcome = interruptThenWait(
			rootPID,
			waited,
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
		Reason:       reason,
		Error:        diagnostic(errors.Join(outcome.err, controlErr)),
		CleanupError: diagnostic(cleanupErr),
	}
	return processowner.WriteStatus(status, processowner.Status{
		State: processowner.StatusFinished, Result: &result,
	})
}

func waitCommand(command *exec.Cmd) (waitResult, error) {
	err := command.Wait()
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

func interruptThenWait(rootPID int, waited <-chan waitResult, grace time.Duration) waitResult {
	_ = signalProcessGroup(rootPID, syscall.SIGINT)
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case result := <-waited:
		return result
	case <-timer.C:
		_ = forceTree(rootPID)
		return <-waited
	}
}

func readControl(reader io.Reader, result chan<- trigger) {
	var command [1]byte
	for {
		_, err := io.ReadFull(reader, command[:])
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				result <- trigger{reason: processowner.ReasonParentLost}
			} else {
				result <- trigger{reason: processowner.ReasonStop, err: fmt.Errorf("read process control: %w", err)}
			}
			return
		}
		switch command[0] {
		case processowner.ControlInterrupt:
			result <- trigger{reason: processowner.ReasonInterrupt}
		case processowner.ControlStop:
			result <- trigger{reason: processowner.ReasonStop}
		default:
			result <- trigger{reason: processowner.ReasonStop, err: errors.New("process control command is invalid")}
		}
		return
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
	forceErr := forceTree(rootPID)
	deadline := time.Now().Add(maximum)
	emptyPolls := 0
	for {
		reapAdoptedChildren()
		descendants, err := descendantPIDs(os.Getpid())
		if err != nil {
			return errors.Join(forceErr, err)
		}
		if len(descendants) == 0 {
			emptyPolls++
			// Orphan reparenting and /proc visibility do not form one atomic
			// observation. Requiring a stable-empty window keeps terminal status
			// causally after subreaper adoption.
			if emptyPolls >= descendantEmptyConfirmationPolls {
				return forceErr
			}
		} else {
			emptyPolls = 0
			for _, processID := range descendants {
				if err := syscall.Kill(processID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
					forceErr = errors.Join(forceErr, err)
				}
			}
		}
		if !time.Now().Before(deadline) {
			return errors.Join(forceErr, fmt.Errorf("Linux descendants did not exit: %v", descendants))
		}
		time.Sleep(descendantPollInterval)
	}
}

func reapAdoptedChildren() {
	for {
		var status syscall.WaitStatus
		processID, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if processID <= 0 || err != nil {
			return
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
			if errors.Is(readErr, os.ErrNotExist) || errors.Is(readErr, os.ErrPermission) {
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

func parseParentPID(stat string) (int, error) {
	closing := strings.LastIndexByte(stat, ')')
	if closing < 0 || closing+1 >= len(stat) {
		return 0, errors.New("Linux process stat has no command terminator")
	}
	fields := strings.Fields(stat[closing+1:])
	if len(fields) < 2 {
		return 0, errors.New("Linux process stat has no parent PID")
	}
	parentPID, err := strconv.Atoi(fields[1])
	if err != nil || parentPID < 0 {
		return 0, errors.New("Linux process stat parent PID is invalid")
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
