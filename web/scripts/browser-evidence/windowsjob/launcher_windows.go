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

func runLauncherPlatform(
	request startRequest,
	eventHandleValue uintptr,
	stdinHandleValue uintptr,
	acknowledgementReader io.Reader,
) error {
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
	command := exec.Command(request.Executable, request.Arguments...)
	command.Dir = request.CWD
	command.Env = environmentStrings(request.Environment)
	var targetInputReader *os.File
	var targetInputWriter *os.File
	if stdin != nil {
		targetInputReader, targetInputWriter, err = os.Pipe()
		if err != nil {
			return fmt.Errorf("create exact target stdin pipe: %w", err)
		}
		defer targetInputReader.Close()
		defer targetInputWriter.Close()
		command.Stdin = targetInputReader
	}
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
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
	if targetInputReader != nil {
		_ = targetInputReader.Close()
		if err := writeAll(targetInputWriter, stdin); err != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			return fmt.Errorf("deliver exact target stdin: %w", err)
		}
		if err := targetInputWriter.Close(); err != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			return fmt.Errorf("close exact target stdin: %w", err)
		}
		for index := range stdin {
			stdin[index] = 0
		}
	}
	var transferHandle windows.Handle
	var duplicateErr error
	if err := command.Process.WithHandle(func(handle uintptr) {
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
		_ = command.Process.Kill()
		_ = command.Wait()
		return fmt.Errorf("read launcher-local root handle: %w", err)
	}
	if duplicateErr != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return fmt.Errorf("retain launcher-local root handle: %w", duplicateErr)
	}
	if transferHandle == 0 || transferHandle == windows.InvalidHandle {
		_ = command.Process.Kill()
		_ = command.Wait()
		return errors.New("retained launcher-local root handle is invalid")
	}
	defer func() {
		if transferHandle != 0 {
			_ = windows.CloseHandle(transferHandle)
		}
	}()
	if err := writeCanonicalFrame(eventWriter, launcherEvent{
		SchemaVersion: protocolSchemaVersion,
		Type:          launcherEventRootStarted,
		PID:           uint32(command.Process.Pid),
		ProcessHandle: uint64(uintptr(transferHandle)),
		SpawnFailure:  nil,
	}); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return fmt.Errorf("report root handle: %w", err)
	}
	if err := eventWriter.Close(); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return fmt.Errorf("close launcher event channel: %w", err)
	}
	acknowledgement := []byte{0}
	if _, err := io.ReadFull(acknowledgementReader, acknowledgement); err != nil || acknowledgement[0] != launcherRootAcknowledgement {
		_ = command.Process.Kill()
		_ = command.Wait()
		if err != nil {
			return fmt.Errorf("receive root-handle acknowledgement: %w", err)
		}
		return errors.New("root-handle acknowledgement is invalid")
	}
	if err := windows.CloseHandle(transferHandle); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return fmt.Errorf("release launcher-local transfer handle: %w", err)
	}
	transferHandle = 0
	// The supervisor now owns the durable root handle. Releasing the launcher's
	// local process reference lets the trusted launcher leave the Job before its
	// accounting is interpreted as target-tree liveness.
	if err := command.Process.Release(); err != nil {
		return fmt.Errorf("release launcher-local root process reference: %w", err)
	}
	return nil
}

func startAssignedLauncher(
	job managedJob,
	request startRequest,
	rawInput *os.File,
) (*assignedLauncher, error) {
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
		_ = launcherInput.Close()
		return nil, fmt.Errorf("start trusted launcher: %w", err)
	}
	_ = windows.SetHandleInformation(windows.Handle(eventWriter.Fd()), windows.HANDLE_FLAG_INHERIT, 0)
	_ = eventWriter.Close()

	var assignmentErr error
	var membershipHandle windows.Handle
	var duplicateErr error
	withHandleErr := command.Process.WithHandle(func(handle uintptr) {
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
		_ = launcherInput.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
		if membershipHandle != 0 {
			_ = windows.CloseHandle(membershipHandle)
		}
		if assignmentErr != nil {
			return nil, fmt.Errorf("assign trusted launcher to Job Object: %w", assignmentErr)
		}
		if duplicateErr != nil {
			return nil, fmt.Errorf("retain trusted launcher identity: %w", duplicateErr)
		}
		return nil, fmt.Errorf("access trusted launcher process handle: %w", withHandleErr)
	}
	if membershipHandle == 0 || membershipHandle == windows.InvalidHandle {
		_ = launcherInput.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, errors.New("retained trusted launcher handle is invalid")
	}
	membershipTransferred := false
	defer func() {
		if !membershipTransferred {
			_ = windows.CloseHandle(membershipHandle)
		}
	}()
	active, err := job.activeProcessCount()
	if err != nil || active != 1 {
		_ = launcherInput.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("fresh Job Object active process count is %d after launcher assignment, expected 1", active)
	}
	// No target-bearing byte crosses this pipe until assignment and accounting
	// both prove that the trusted launcher is contained.
	if err := writeCanonicalFrame(launcherInput, request); err != nil {
		_ = launcherInput.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, fmt.Errorf("send request to assigned launcher: %w", err)
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
