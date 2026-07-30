//go:build windows

package main

import (
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

const launcherRootAcknowledgement byte = 0xa5

type assignedLauncher struct {
	eventReader      *os.File
	input            io.WriteCloser
	process          *os.Process
	membershipHandle windows.Handle
	wait             <-chan error
}

type launcherTargetInput struct {
	reader *os.File
	writer *os.File
}

func runLauncherPlatform(
	request startRequest,
	eventHandleValue uintptr,
	stdinHandleValue uintptr,
	acknowledgementReader io.Reader,
) (resultErr error) {
	eventHandle := windows.Handle(eventHandleValue)
	if err := windows.SetHandleInformation(eventHandle, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		return fmt.Errorf("make launcher event handle private: %w", err)
	}
	eventWriter := os.NewFile(eventHandleValue, "windowsjob-launcher-event")
	if eventWriter == nil {
		return errors.New("launcher event handle is invalid")
	}
	defer eventWriter.Close()

	stdin, err := readExactRawStdin(stdinHandleValue, request.Stdin)
	if err != nil {
		return fmt.Errorf("read exact raw target stdin: %w", err)
	}
	defer func() {
		for index := range stdin {
			stdin[index] = 0
		}
	}()
	executableLock, err := openAuthenticatedExecutable(request.Executable, request.ExecutableSHA256)
	if err != nil {
		return fmt.Errorf("authenticate target executable: %w", err)
	}
	if executableLock != nil {
		defer executableLock.Close()
	}
	command, targetInput, err := prepareLauncherTarget(request, stdin)
	if err != nil {
		return err
	}
	if targetInput.reader != nil {
		defer targetInput.reader.Close()
		defer targetInput.writer.Close()
	}
	if err := command.Start(); err != nil {
		failure := boundedDiagnostic(err)
		return writeCanonicalFrame(eventWriter, launcherEvent{
			SchemaVersion: protocolSchemaVersion,
			Type:          launcherEventSpawnFailed,
			PID:           0,
			ProcessHandle: 0,
			SpawnFailure:  &failure,
		})
	}
	targetOwned := true
	defer func() {
		if targetOwned {
			resultErr = errors.Join(resultErr, settleStartedProcess(command, "launcher-owned target"))
		}
	}()
	if err := deliverLauncherTargetInput(targetInput, stdin); err != nil {
		return err
	}
	transferHandle, err := retainLauncherRootHandle(command.Process)
	if err != nil {
		return err
	}
	defer func() {
		if transferHandle != 0 {
			resultErr = errors.Join(resultErr, closeOwnedProcessHandle(
				transferHandle,
				"release launcher-local transfer handle",
			))
		}
	}()
	if err := transferLauncherRoot(
		eventWriter,
		acknowledgementReader,
		uint32(command.Process.Pid),
		transferHandle,
	); err != nil {
		return err
	}
	transferHandle = 0
	// The supervisor now owns the durable root handle. Releasing the launcher's
	// local process reference lets the trusted launcher leave the Job before its
	// accounting is interpreted as target-tree liveness.
	targetOwned = false
	if err := command.Process.Release(); err != nil {
		return fmt.Errorf("release launcher-local root process reference: %w", err)
	}
	return nil
}

func prepareLauncherTarget(request startRequest, stdin []byte) (*exec.Cmd, launcherTargetInput, error) {
	command := exec.Command(request.Executable, request.Arguments...)
	command.Dir = request.CWD
	command.Env = environmentStrings(request.Environment)
	var input launcherTargetInput
	if stdin != nil {
		var err error
		input.reader, input.writer, err = os.Pipe()
		if err != nil {
			return nil, launcherTargetInput{}, fmt.Errorf("create exact target stdin pipe: %w", err)
		}
		command.Stdin = input.reader
	}
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return command, input, nil
}

func deliverLauncherTargetInput(input launcherTargetInput, stdin []byte) error {
	if input.reader == nil {
		return nil
	}
	_ = input.reader.Close()
	if err := writeAll(input.writer, stdin); err != nil {
		return fmt.Errorf("deliver exact target stdin: %w", err)
	}
	if err := input.writer.Close(); err != nil {
		return fmt.Errorf("close exact target stdin: %w", err)
	}
	for index := range stdin {
		stdin[index] = 0
	}
	return nil
}

func retainLauncherRootHandle(process *os.Process) (windows.Handle, error) {
	var transferHandle windows.Handle
	var duplicateErr error
	if err := process.WithHandle(func(handle uintptr) {
		duplicateErr = windows.DuplicateHandle(
			windows.CurrentProcess(),
			windows.Handle(handle),
			windows.CurrentProcess(),
			&transferHandle,
			0,
			false,
			windows.DUPLICATE_SAME_ACCESS,
		)
	}); err != nil {
		return 0, errors.Join(
			fmt.Errorf("read launcher-local root handle: %w", err),
			closeOwnedProcessHandle(transferHandle, "release partial launcher-local root handle"),
		)
	}
	if duplicateErr != nil {
		return 0, errors.Join(
			fmt.Errorf("retain launcher-local root handle: %w", duplicateErr),
			closeOwnedProcessHandle(transferHandle, "release partial launcher-local root handle"),
		)
	}
	if transferHandle == 0 || transferHandle == windows.InvalidHandle {
		return 0, errors.New("retained launcher-local root handle is invalid")
	}
	return transferHandle, nil
}

func transferLauncherRoot(
	eventWriter *os.File,
	acknowledgementReader io.Reader,
	processID uint32,
	transferHandle windows.Handle,
) error {
	if err := writeCanonicalFrame(eventWriter, launcherEvent{
		SchemaVersion: protocolSchemaVersion,
		Type:          launcherEventRootStarted,
		PID:           processID,
		ProcessHandle: uint64(uintptr(transferHandle)),
		SpawnFailure:  nil,
	}); err != nil {
		return fmt.Errorf("report root handle: %w", err)
	}
	if err := eventWriter.Close(); err != nil {
		return fmt.Errorf("close launcher event channel: %w", err)
	}
	acknowledgement := []byte{0}
	if _, err := io.ReadFull(acknowledgementReader, acknowledgement); err != nil {
		return fmt.Errorf("receive root-handle acknowledgement: %w", err)
	}
	if acknowledgement[0] != launcherRootAcknowledgement {
		return errors.New("root-handle acknowledgement is invalid")
	}
	return closeOwnedProcessHandle(transferHandle, "release launcher-local transfer handle")
}

func settleStartedProcess(command *exec.Cmd, owner string) error {
	killErr := command.Process.Kill()
	if errors.Is(killErr, os.ErrProcessDone) {
		killErr = nil
	}
	waitErr := command.Wait()
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) || errors.Is(waitErr, os.ErrProcessDone) {
		waitErr = nil
	}
	if killErr != nil {
		killErr = fmt.Errorf("terminate %s: %w", owner, killErr)
	}
	if waitErr != nil {
		waitErr = fmt.Errorf("reap %s: %w", owner, waitErr)
	}
	return errors.Join(killErr, waitErr)
}

func startAssignedLauncher(
	job managedJob,
	request startRequest,
	rawInput *os.File,
) (launcher *assignedLauncher, resultErr error) {
	eventReader, eventWriter, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create launcher event pipe: %w", err)
	}
	cleanupPipe := true
	defer func() {
		if cleanupPipe {
			_ = eventReader.Close()
			_ = eventWriter.Close()
		}
	}()
	if err := windows.SetHandleInformation(windows.Handle(eventWriter.Fd()), windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
		return nil, fmt.Errorf("make launcher event handle inheritable: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve supervisor executable: %w", err)
	}
	launcherArguments := []string{
		commandLauncher,
		"--event-handle", strconv.FormatUint(uint64(eventWriter.Fd()), 10),
	}
	inheritedHandles := []syscall.Handle{syscall.Handle(eventWriter.Fd())}
	if rawInput != nil {
		rawHandle := windows.Handle(rawInput.Fd())
		if err := windows.SetHandleInformation(
			rawHandle,
			windows.HANDLE_FLAG_INHERIT,
			windows.HANDLE_FLAG_INHERIT,
		); err != nil {
			return nil, fmt.Errorf("make raw stdin handle inheritable: %w", err)
		}
		defer func() {
			_ = windows.SetHandleInformation(rawHandle, windows.HANDLE_FLAG_INHERIT, 0)
			_ = rawInput.Close()
		}()
		launcherArguments = append(
			launcherArguments,
			"--stdin-handle", strconv.FormatUint(uint64(rawInput.Fd()), 10),
		)
		inheritedHandles = append(inheritedHandles, syscall.Handle(rawInput.Fd()))
	}
	command := exec.Command(executable, launcherArguments...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:                 true,
		AdditionalInheritedHandles: inheritedHandles,
	}
	launcherInput, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("create launcher input pipe: %w", err)
	}
	if err := command.Start(); err != nil {
		return nil, errors.Join(
			fmt.Errorf("start trusted launcher: %w", err),
			closeLauncherInput(launcherInput, "close unstarted launcher input"),
		)
	}
	_ = windows.SetHandleInformation(windows.Handle(eventWriter.Fd()), windows.HANDLE_FLAG_INHERIT, 0)
	_ = eventWriter.Close()

	membershipHandle, err := assignTrustedLauncherIdentity(job, command.Process)
	if err != nil {
		return nil, abandonStartedLauncher(command, launcherInput, err)
	}
	membershipTransferred := false
	defer func() {
		if !membershipTransferred {
			resultErr = errors.Join(resultErr, closeOwnedProcessHandle(
				membershipHandle,
				"close retained trusted launcher identity after startup failure",
			))
		}
	}()
	if err := authorizeAssignedLauncher(job, launcherInput, request); err != nil {
		return nil, abandonStartedLauncher(command, launcherInput, err)
	}
	waitChannel := make(chan error, 1)
	go func() { waitChannel <- command.Wait() }()
	cleanupPipe = false
	membershipTransferred = true
	return &assignedLauncher{
		eventReader:      eventReader,
		input:            launcherInput,
		process:          command.Process,
		membershipHandle: membershipHandle,
		wait:             waitChannel,
	}, nil
}

func assignTrustedLauncherIdentity(job managedJob, process *os.Process) (windows.Handle, error) {
	var assignmentErr error
	var membershipHandle windows.Handle
	var duplicateErr error
	withHandleErr := process.WithHandle(func(handle uintptr) {
		assignmentErr = windows.AssignProcessToJobObject(job.handle, windows.Handle(handle))
		if assignmentErr != nil {
			return
		}
		duplicateErr = windows.DuplicateHandle(
			windows.CurrentProcess(),
			windows.Handle(handle),
			windows.CurrentProcess(),
			&membershipHandle,
			0,
			false,
			windows.DUPLICATE_SAME_ACCESS,
		)
	})
	if withHandleErr != nil || assignmentErr != nil || duplicateErr != nil {
		var authorityErr error
		switch {
		case assignmentErr != nil:
			authorityErr = fmt.Errorf("assign trusted launcher to Job Object: %w", assignmentErr)
		case duplicateErr != nil:
			authorityErr = fmt.Errorf("retain trusted launcher identity: %w", duplicateErr)
		default:
			authorityErr = fmt.Errorf("access trusted launcher process handle: %w", withHandleErr)
		}
		return 0, errors.Join(
			authorityErr,
			closeOwnedProcessHandle(membershipHandle, "close partial trusted launcher identity"),
		)
	}
	if membershipHandle == 0 || membershipHandle == windows.InvalidHandle {
		return 0, errors.New("retained trusted launcher handle is invalid")
	}
	return membershipHandle, nil
}

func authorizeAssignedLauncher(job managedJob, input io.Writer, request startRequest) error {
	active, err := job.activeProcessCount()
	if err != nil {
		return err
	}
	if active != 1 {
		return fmt.Errorf("fresh Job Object active process count is %d after launcher assignment, expected 1", active)
	}
	// No target-bearing byte crosses this pipe until assignment and accounting
	// both prove that the trusted launcher is contained.
	if err := writeCanonicalFrame(input, request); err != nil {
		return fmt.Errorf("send request to assigned launcher: %w", err)
	}
	return nil
}

func abandonStartedLauncher(command *exec.Cmd, input io.Closer, cause error) error {
	return errors.Join(
		cause,
		closeLauncherInput(input, "close rejected launcher input"),
		settleStartedProcess(command, "rejected trusted launcher"),
	)
}

func closeLauncherInput(input io.Closer, operation string) error {
	if input == nil {
		return nil
	}
	if err := input.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return nil
}
