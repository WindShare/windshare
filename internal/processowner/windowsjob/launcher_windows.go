//go:build windows

package windowsjob

import (
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
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

func makeLauncherHandlePrivate(value uintptr, label string) error {
	if err := windows.SetHandleInformation(windows.Handle(value), windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		return fmt.Errorf("make launcher %s handle private: %w", label, err)
	}
	return nil
}

func closeUntransferredLauncherHandle(value uintptr) {
	if value != 0 {
		_ = windows.CloseHandle(windows.Handle(value))
	}
}

func runLauncherPlatform(
	request ownerprotocol.Request,
	eventHandleValue uintptr,
	targetEventHandle uintptr,
	acknowledgementReader io.Reader,
) (resultErr error) {
	eventWriter := os.NewFile(eventHandleValue, "windowsjob-launcher-event")
	if eventWriter == nil {
		return errors.New("launcher event handle is invalid")
	}
	defer eventWriter.Close()
	if targetEventHandle != 0 {
		defer windows.CloseHandle(windows.Handle(targetEventHandle))
	}

	command, targetInput, err := prepareLauncherTarget(request, targetEventHandle)
	if err != nil {
		return err
	}
	if targetInput.reader != nil {
		defer targetInput.reader.Close()
		defer targetInput.writer.Close()
	}
	if targetEventHandle != 0 {
		if err := windows.SetHandleInformation(
			windows.Handle(targetEventHandle), windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT,
		); err != nil {
			return fmt.Errorf("make target event handle inheritable for CreateProcess: %w", err)
		}
	}
	startErr := command.Start()
	var clearEventInheritErr error
	if targetEventHandle != 0 {
		clearEventInheritErr = windows.SetHandleInformation(
			windows.Handle(targetEventHandle), windows.HANDLE_FLAG_INHERIT, 0,
		)
	}
	if startErr != nil {
		failure := boundedDiagnostic(errors.Join(startErr, clearEventInheritErr))
		return ownerprotocol.WriteFrame(eventWriter, launcherEvent{
			SchemaVersion: launcherEventSchema,
			Type:          launcherEventSpawnFailed,
			PID:           0,
			ProcessHandle: 0,
			SpawnFailure:  &failure,
		})
	}
	if clearEventInheritErr != nil {
		return fmt.Errorf("restore private target event handle after CreateProcess: %w", clearEventInheritErr)
	}
	targetOwned := true
	defer func() {
		if targetOwned {
			resultErr = errors.Join(resultErr, settleStartedProcess(command, "launcher-owned target"))
		}
	}()
	if targetInput.reader != nil {
		if err := targetInput.reader.Close(); err != nil {
			return fmt.Errorf("release launcher-local target stdin reader: %w", err)
		}
		targetInput.reader = nil
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
	inputHandle := uintptr(0)
	if targetInput.writer != nil {
		inputHandle = targetInput.writer.Fd()
	}
	if err := transferLauncherCapabilities(
		eventWriter,
		acknowledgementReader,
		uint32(command.Process.Pid),
		transferHandle,
		inputHandle,
	); err != nil {
		return err
	}
	if err := closeOwnedProcessHandle(transferHandle, "release launcher-local transfer handle"); err != nil {
		return err
	}
	transferHandle = 0
	if targetInput.writer != nil {
		if err := targetInput.writer.Close(); err != nil {
			return fmt.Errorf("release launcher-local target stdin writer: %w", err)
		}
		targetInput.writer = nil
	}
	// The supervisor resumes the target only after this trusted launcher has left
	// the Job. Keeping the target suspended makes stop/deadline during handoff a
	// true pre-release decision instead of allowing work before containment settles.
	targetOwned = false
	if err := command.Process.Release(); err != nil {
		return fmt.Errorf("release launcher-local root process reference: %w", err)
	}
	return nil
}

func prepareLauncherTarget(request ownerprotocol.Request, targetEventHandle uintptr) (*exec.Cmd, launcherTargetInput, error) {
	command := exec.Command(request.Command.Executable, request.Command.Arguments...)
	command.Dir = request.Command.WorkingDirectory
	command.Env = environmentStrings(request.Command.Environment, targetEventHandle, request.Identity)
	var input launcherTargetInput
	if request.Command.Stdin != nil {
		var err error
		input.reader, input.writer, err = os.Pipe()
		if err != nil {
			return nil, launcherTargetInput{}, fmt.Errorf("create exact target stdin pipe: %w", err)
		}
		command.Stdin = input.reader
	}
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_SUSPENDED,
	}
	if targetEventHandle != 0 {
		command.SysProcAttr.AdditionalInheritedHandles = []syscall.Handle{syscall.Handle(targetEventHandle)}
	}
	return command, input, nil
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

func transferLauncherCapabilities(
	eventWriter *os.File,
	acknowledgementReader io.Reader,
	processID uint32,
	transferHandle windows.Handle,
	inputHandle uintptr,
) error {
	if err := ownerprotocol.WriteFrame(eventWriter, launcherEvent{
		SchemaVersion: launcherEventSchema,
		Type:          launcherEventRootStarted,
		PID:           processID,
		ProcessHandle: uint64(uintptr(transferHandle)),
		InputHandle:   uint64(inputHandle),
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
	return nil
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
	request supervisionRequest,
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
	if request.EventHandle != 0 {
		eventHandle := windows.Handle(request.EventHandle)
		if err := windows.SetHandleInformation(
			eventHandle,
			windows.HANDLE_FLAG_INHERIT,
			windows.HANDLE_FLAG_INHERIT,
		); err != nil {
			return nil, fmt.Errorf("make target event handle inheritable: %w", err)
		}
		launcherArguments = append(
			launcherArguments,
			"--target-event-handle", strconv.FormatUint(uint64(request.EventHandle), 10),
		)
		inheritedHandles = append(inheritedHandles, syscall.Handle(request.EventHandle))
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
	if request.EventHandle != 0 {
		if err := windows.SetHandleInformation(
			windows.Handle(request.EventHandle), windows.HANDLE_FLAG_INHERIT, 0,
		); err != nil {
			return nil, abandonStartedLauncher(
				command, launcherInput, fmt.Errorf("restore private target event authority after launcher CreateProcess: %w", err),
			)
		}
	}

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
	if err := authorizeAssignedLauncher(job, launcherInput, request.Protocol); err != nil {
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

func authorizeAssignedLauncher(job managedJob, input io.Writer, request ownerprotocol.Request) error {
	active, err := job.activeProcessCount()
	if err != nil {
		return err
	}
	if active != 1 {
		return fmt.Errorf("fresh Job Object active process count is %d after launcher assignment, expected 1", active)
	}
	// No target-bearing byte crosses this pipe until assignment and accounting
	// both prove that the trusted launcher is contained.
	if err := ownerprotocol.WriteFrame(input, request); err != nil {
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
