//go:build windows

// Package windowsjob owns one test process tree in a kill-on-close Job Object.
package windowsjob

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/windshare/windshare/internal/processowner"
	"github.com/windshare/windshare/internal/testtrace"
	"golang.org/x/sys/windows"
)

const (
	jobPollInterval           = 10 * time.Millisecond
	jobEmptyConfirmationPolls = 5
	forcedTerminationCode     = uint32(windows.STATUS_CONTROL_C_EXIT)
)

type trigger struct {
	reason string
	err    error
}

type rootProcess struct {
	handle windows.Handle
	id     uint32
}

type rootResult struct {
	exitCode int64
	err      error
}

type jobObjectBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// Run starts the target suspended, assigns it to the Job Object, and resumes it
// only after containment is established. Closing or terminating the job therefore
// covers descendants created immediately at process start.
func Run(
	config processowner.Config,
	status *os.File,
	control *os.File,
	events *os.File,
) error {
	if status == nil || control == nil || events == nil {
		return errors.New("windows process-owner endpoints are incomplete")
	}
	if err := processowner.ValidateConfig(config); err != nil {
		return err
	}
	for _, file := range []*os.File{status, control} {
		if err := windows.SetHandleInformation(
			windows.Handle(file.Fd()), windows.HANDLE_FLAG_INHERIT, 0,
		); err != nil {
			return fmt.Errorf("make Windows owner endpoint private: %w", err)
		}
	}
	job, err := createJob()
	if err != nil {
		return err
	}
	defer windows.CloseHandle(job)
	root, err := startTarget(config, job, events)
	if err != nil {
		_ = events.Close()
		result := processowner.Result{Reason: processowner.ReasonSpawnFailed, Error: diagnostic(err)}
		return processowner.WriteStatus(status, processowner.Status{
			State: processowner.StatusFinished, Result: &result,
		})
	}
	defer windows.CloseHandle(root.handle)
	if err := events.Close(); err != nil {
		_ = windows.TerminateJobObject(job, forcedTerminationCode)
		return fmt.Errorf("close Windows owner event endpoint: %w", err)
	}
	if err := processowner.WriteStatus(status, processowner.Status{State: processowner.StatusStarted}); err != nil {
		_ = windows.TerminateJobObject(job, forcedTerminationCode)
		return fmt.Errorf("publish Windows process readiness: %w", err)
	}

	waited := make(chan rootResult, 1)
	go func() { waited <- waitRoot(root) }()
	controls := make(chan trigger, 1)
	go readControl(control, controls)
	deadline := time.NewTimer(time.Duration(config.DeadlineMilliseconds) * time.Millisecond)
	defer deadline.Stop()

	reason := processowner.ReasonNatural
	var controlErr error
	var outcome rootResult
	select {
	case outcome = <-waited:
	case requested := <-controls:
		reason, controlErr = requested.reason, requested.err
		outcome, err = interruptThenWait(
			job,
			root,
			waited,
			time.Duration(config.TerminationGraceMilliseconds)*time.Millisecond,
		)
		controlErr = errors.Join(controlErr, err)
	case <-deadline.C:
		reason = processowner.ReasonDeadline
		outcome, controlErr = interruptThenWait(
			job,
			root,
			waited,
			time.Duration(config.TerminationGraceMilliseconds)*time.Millisecond,
		)
	}
	cleanupErr := retireJob(job, time.Duration(config.TerminationGraceMilliseconds)*time.Millisecond)
	result := processowner.Result{
		ExitCode:     &outcome.exitCode,
		Reason:       reason,
		Error:        diagnostic(errors.Join(outcome.err, controlErr)),
		CleanupError: diagnostic(cleanupErr),
	}
	return processowner.WriteStatus(status, processowner.Status{
		State: processowner.StatusFinished, Result: &result,
	})
}

func createJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("create Windows Job Object: %w", err)
	}
	configured := false
	defer func() {
		if !configured {
			_ = windows.CloseHandle(job)
		}
	}()
	if err := windows.SetHandleInformation(job, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		return 0, fmt.Errorf("make Windows Job Object private: %w", err)
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)),
		uint32(unsafe.Sizeof(limits)),
	); err != nil {
		return 0, fmt.Errorf("configure Windows Job Object: %w", err)
	}
	runtime.KeepAlive(&limits)
	configured = true
	return job, nil
}

func startTarget(config processowner.Config, job windows.Handle, events *os.File) (_ rootProcess, resultErr error) {
	nullInput, err := os.Open(os.DevNull)
	if err != nil {
		return rootProcess{}, fmt.Errorf("open target null input: %w", err)
	}
	defer nullInput.Close()
	handles := uniqueHandles([]windows.Handle{
		windows.Handle(nullInput.Fd()), windows.Handle(os.Stdout.Fd()),
		windows.Handle(os.Stderr.Fd()), windows.Handle(events.Fd()),
	})
	handlesPrivate := false
	defer func() {
		if !handlesPrivate {
			resultErr = errors.Join(resultErr, makeHandlesPrivate(handles))
		}
	}()
	for _, handle := range handles {
		if err := windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			return rootProcess{}, fmt.Errorf("make target handle inheritable: %w", err)
		}
	}
	attributes, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return rootProcess{}, fmt.Errorf("create target handle list: %w", err)
	}
	defer attributes.Delete()
	if err := attributes.Update(
		windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST,
		unsafe.Pointer(&handles[0]),
		uintptr(len(handles))*unsafe.Sizeof(handles[0]),
	); err != nil {
		return rootProcess{}, fmt.Errorf("bind target handle list: %w", err)
	}
	startup := windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb:       uint32(unsafe.Sizeof(windows.StartupInfoEx{})),
			Flags:    windows.STARTF_USESTDHANDLES,
			StdInput: windows.Handle(nullInput.Fd()), StdOutput: windows.Handle(os.Stdout.Fd()),
			StdErr: windows.Handle(os.Stderr.Fd()),
		},
		ProcThreadAttributeList: attributes.List(),
	}
	executable, err := windows.UTF16PtrFromString(config.Executable)
	if err != nil {
		return rootProcess{}, err
	}
	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(
		append([]string{config.Executable}, config.Arguments...),
	))
	if err != nil {
		return rootProcess{}, err
	}
	directory, err := windows.UTF16PtrFromString(config.WorkingDirectory)
	if err != nil {
		return rootProcess{}, err
	}
	environment := windowsEnvironment(config.Environment, windows.Handle(events.Fd()))
	information := windows.ProcessInformation{}
	err = windows.CreateProcess(
		executable,
		commandLine,
		nil,
		nil,
		true,
		windows.CREATE_SUSPENDED|windows.CREATE_NEW_PROCESS_GROUP|
			windows.CREATE_UNICODE_ENVIRONMENT|windows.EXTENDED_STARTUPINFO_PRESENT,
		&environment[0],
		directory,
		&startup.StartupInfo,
		&information,
	)
	runtime.KeepAlive(environment)
	if err != nil {
		return rootProcess{}, fmt.Errorf("create suspended Windows target: %w", err)
	}
	processOwned := true
	defer func() {
		if processOwned {
			_ = windows.TerminateProcess(information.Process, forcedTerminationCode)
			resultErr = errors.Join(resultErr, windows.CloseHandle(information.Process))
		}
	}()
	defer windows.CloseHandle(information.Thread)
	if err := windows.AssignProcessToJobObject(job, information.Process); err != nil {
		return rootProcess{}, fmt.Errorf("assign Windows target to Job Object: %w", err)
	}
	if _, err := windows.ResumeThread(information.Thread); err != nil {
		return rootProcess{}, fmt.Errorf("resume Windows target: %w", err)
	}
	if err := makeHandlesPrivate(handles); err != nil {
		return rootProcess{}, err
	}
	handlesPrivate = true
	processOwned = false
	return rootProcess{handle: information.Process, id: information.ProcessId}, nil
}

func makeHandlesPrivate(handles []windows.Handle) error {
	var result error
	for _, handle := range handles {
		if err := windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
			result = errors.Join(result, fmt.Errorf("make target handle private: %w", err))
		}
	}
	return result
}

func waitRoot(root rootProcess) rootResult {
	if event, err := windows.WaitForSingleObject(root.handle, windows.INFINITE); err != nil {
		return rootResult{exitCode: -1, err: fmt.Errorf("wait for Windows target: %w", err)}
	} else if event != windows.WAIT_OBJECT_0 {
		return rootResult{exitCode: -1, err: fmt.Errorf("wait for Windows target returned %#x", event)}
	}
	var exitCode uint32
	if err := windows.GetExitCodeProcess(root.handle, &exitCode); err != nil {
		return rootResult{exitCode: -1, err: fmt.Errorf("read Windows target exit code: %w", err)}
	}
	return rootResult{exitCode: int64(exitCode)}
}

func interruptThenWait(
	job windows.Handle,
	root rootProcess,
	waited <-chan rootResult,
	grace time.Duration,
) (rootResult, error) {
	interruptErr := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, root.id)
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case result := <-waited:
		return result, interruptErr
	case <-timer.C:
		terminateErr := windows.TerminateJobObject(job, forcedTerminationCode)
		return <-waited, errors.Join(interruptErr, terminateErr)
	}
}

func retireJob(job windows.Handle, maximum time.Duration) error {
	deadline := time.Now().Add(maximum)
	var terminateErr error
	terminationRequested := false
	emptyPolls := 0
	for {
		active, err := activeProcessCount(job)
		if err != nil {
			return errors.Join(terminateErr, err)
		}
		if active == 0 {
			emptyPolls++
			// Job accounting can briefly report zero while a newly inherited
			// descendant becomes visible. A short stable-empty window keeps the
			// terminal status causally after descendant adoption.
			if emptyPolls >= jobEmptyConfirmationPolls {
				return terminateErr
			}
		} else {
			emptyPolls = 0
			if !terminationRequested {
				terminateErr = windows.TerminateJobObject(job, forcedTerminationCode)
				terminationRequested = true
			}
		}
		if !time.Now().Before(deadline) {
			return errors.Join(terminateErr, errors.New("windows Job Object did not become empty"))
		}
		time.Sleep(jobPollInterval)
	}
}

func activeProcessCount(job windows.Handle) (uint32, error) {
	accounting := jobObjectBasicAccountingInformation{}
	if err := windows.QueryInformationJobObject(
		job,
		windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&accounting)),
		uint32(unsafe.Sizeof(accounting)),
		nil,
	); err != nil {
		return 0, fmt.Errorf("query Windows Job Object: %w", err)
	}
	runtime.KeepAlive(&accounting)
	return accounting.ActiveProcesses, nil
}

func readControl(reader io.Reader, result chan<- trigger) {
	var command [1]byte
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
}

func windowsEnvironment(base []string, eventHandle windows.Handle) []uint16 {
	environment := make([]string, 0, len(base)+1)
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(name, testtrace.EventFDEnvironment) ||
			strings.EqualFold(name, testtrace.EventHandleEnvironment) {
			continue
		}
		environment = append(environment, entry)
	}
	environment = append(environment, testtrace.EventHandleEnvironment+"="+strconv.FormatUint(uint64(eventHandle), 10))
	sort.Slice(environment, func(left, right int) bool {
		return strings.ToUpper(environment[left]) < strings.ToUpper(environment[right])
	})
	return utf16.Encode([]rune(strings.Join(environment, "\x00") + "\x00\x00"))
}

func uniqueHandles(handles []windows.Handle) []windows.Handle {
	unique := make([]windows.Handle, 0, len(handles))
	seen := make(map[windows.Handle]struct{}, len(handles))
	for _, handle := range handles {
		if _, exists := seen[handle]; exists {
			continue
		}
		seen[handle] = struct{}{}
		unique = append(unique, handle)
	}
	return unique
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
