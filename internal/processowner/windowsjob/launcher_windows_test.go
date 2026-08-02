//go:build windows

package windowsjob

import (
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
	"golang.org/x/sys/windows"
)

func TestLauncherRejectsInvalidRequestAndReleasesEventEndpoint(t *testing.T) {
	tests := []struct {
		name  string
		write func(*testing.T, io.Writer)
	}{
		{
			name: "non-NFC executable",
			write: func(t *testing.T, writer io.Writer) {
				request := windowsIntegrationRequest(t, "echo", 5_000)
				request.Executable = filepath.Join(t.TempDir(), "missing-e\u0301.exe")
				request, _ = prepareChildProcessEnvironment(t, request)
				if err := ownerprotocol.WriteFrame(writer, request.Protocol); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "relative executable",
			write: func(t *testing.T, writer io.Writer) {
				request := windowsIntegrationRequest(t, "echo", 5_000)
				request.Executable = "fixture.exe"
				request, _ = prepareChildProcessEnvironment(t, request)
				if err := ownerprotocol.WriteFrame(writer, request.Protocol); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "oversized frame",
			write: func(t *testing.T, writer io.Writer) {
				header := make([]byte, 4)
				binary.BigEndian.PutUint32(header, ownerprotocol.MaximumDocumentBytes+1)
				if _, err := writer.Write(header); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertLauncherValidationReleasesEventEndpoint(t, test.write)
		})
	}
}

func assertLauncherValidationReleasesEventEndpoint(t *testing.T, write func(*testing.T, io.Writer)) {
	t.Helper()
	eventReader, eventWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer eventReader.Close()
	eventHandle := duplicateInheritableHandle(t, windows.Handle(eventWriter.Fd()))
	if err := eventWriter.Close(); err != nil {
		_ = windows.CloseHandle(eventHandle)
		t.Fatal(err)
	}
	inputReader, inputWriter := io.Pipe()
	runResult := make(chan error, 1)
	go func() {
		runResult <- runCommand([]string{
			commandLauncher,
			"--event-handle", strconv.FormatUint(uint64(uintptr(eventHandle)), 10),
		}, inputReader)
	}()
	write(t, inputWriter)
	if err := inputWriter.Close(); err != nil {
		t.Fatal(err)
	}
	readResult := make(chan error, 1)
	go func() {
		_, readErr := readLauncherEvent(eventReader)
		readResult <- readErr
	}()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case runErr := <-runResult:
		if runErr == nil {
			t.Fatal("invalid launcher request was accepted")
		}
	case <-timer.C:
		t.Fatal("launcher validation did not return within its bound")
	}
	select {
	case readErr := <-readResult:
		if readErr == nil {
			t.Fatal("invalid launcher request emitted a target event")
		}
	case <-timer.C:
		t.Fatal("launcher event endpoint remained open after validation failure")
	}
}

func TestLauncherCommandTransfersStableRootHandle(t *testing.T) {
	request := windowsIntegrationRequest(t, "exit-259", 5_000)
	acknowledgement := launcherRootAcknowledgement
	event, runErr := runLauncherCommand(t, request, &acknowledgement, func(event launcherEvent) {
		handle := windows.Handle(uintptr(event.ProcessHandle))
		pid, err := windows.GetProcessId(handle)
		if err != nil {
			t.Fatalf("use ACK-fenced transfer handle: %v", err)
		}
		if pid != event.PID {
			t.Fatalf("transfer handle PID = %d, event PID = %d", pid, event.PID)
		}
		if err := resumeContainedTarget(handle); err != nil {
			t.Fatalf("supervisor resume of transferred root: %v", err)
		}
	})
	if runErr != nil {
		t.Fatalf("launcher command: %v", runErr)
	}
	if event.Type != launcherEventRootStarted || event.PID == 0 || event.ProcessHandle == 0 || event.SpawnFailure != nil {
		t.Fatalf("root-started event = %#v", event)
	}
}

func TestLauncherReleasesSuspendedRootWithoutWaitingForItsExit(t *testing.T) {
	request := windowsIntegrationRequest(t, "launcher-release-root", 5_000)
	acknowledgement := launcherRootAcknowledgement
	var retainedRoot windows.Handle
	event, runErr := runLauncherCommand(t, request, &acknowledgement, func(event launcherEvent) {
		retainedRoot = duplicateInheritableHandle(t, windows.Handle(uintptr(event.ProcessHandle)))
	})
	if retainedRoot == 0 || retainedRoot == windows.InvalidHandle {
		t.Fatal("launcher did not expose a transferable root handle")
	}
	defer windows.CloseHandle(retainedRoot)
	if runErr != nil {
		t.Fatalf("launcher command: %v", runErr)
	}
	if event.Type != launcherEventRootStarted {
		t.Fatalf("launcher event = %#v", event)
	}
	waitResult, err := windows.WaitForSingleObject(retainedRoot, 0)
	if err != nil {
		t.Fatalf("probe released root process: %v", err)
	}
	if waitResult != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("launcher waited for root exit; zero-time wait returned %#x", waitResult)
	}
	if err := resumeContainedTarget(retainedRoot); err != nil {
		t.Fatalf("supervisor resume of transferred root: %v", err)
	}
	if waitResult, err = windows.WaitForSingleObject(retainedRoot, windows.INFINITE); err != nil || waitResult != windows.WAIT_OBJECT_0 {
		t.Fatalf("wait for released root: result=%#x err=%v", waitResult, err)
	}
	var exitCode uint32
	if err := windows.GetExitCodeProcess(retainedRoot, &exitCode); err != nil {
		t.Fatalf("read released root exit code: %v", err)
	}
	if exitCode != launcherReleaseRootExitCode {
		t.Fatalf("released root exit code = %d, want %d", exitCode, launcherReleaseRootExitCode)
	}
}

func TestResumeContainedTargetRejectsUnavailableOrWrongHandle(t *testing.T) {
	if err := resumeContainedTarget(0); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("unavailable target error = %v", err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	if err := resumeContainedTarget(windows.Handle(reader.Fd())); err == nil ||
		!strings.Contains(err.Error(), "resume contained target") {
		t.Fatalf("wrong-handle target error = %v", err)
	}
}

func TestLauncherCommandReportsBoundedSpawnFailure(t *testing.T) {
	request := windowsIntegrationRequest(t, "echo", 5_000)
	request.Executable = filepath.Join(t.TempDir(), "missing-é.exe")
	event, runErr := runLauncherCommand(t, request, nil, nil)
	if runErr != nil {
		t.Fatalf("launcher command: %v", runErr)
	}
	if event.Type != launcherEventSpawnFailed || event.SpawnFailure == nil {
		t.Fatalf("spawn-failed event = %#v", event)
	}
	if len(*event.SpawnFailure) > maximumDiagnosticBytes || strings.Contains(*event.SpawnFailure, "e\u0301") {
		t.Fatalf("spawn diagnostic is not bounded NFC text: %q", *event.SpawnFailure)
	}
}

func TestLauncherCommandFailsClosedOnInvalidAcknowledgement(t *testing.T) {
	request := windowsIntegrationRequest(t, "exit-259", 5_000)
	invalidAcknowledgement := byte(0)
	event, runErr := runLauncherCommand(t, request, &invalidAcknowledgement, nil)
	if runErr == nil || !strings.Contains(runErr.Error(), "acknowledgement is invalid") {
		t.Fatalf("invalid acknowledgement error = %v", runErr)
	}
	if event.Type != launcherEventRootStarted {
		t.Fatalf("launcher event = %#v", event)
	}
}

func runLauncherCommand(
	t *testing.T,
	request supervisionRequest,
	acknowledgement *byte,
	inspectBeforeAcknowledgement func(launcherEvent),
) (launcherEvent, error) {
	t.Helper()
	request, _ = prepareChildProcessEnvironment(t, request)
	eventReader, eventWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer eventReader.Close()
	eventHandle := duplicateInheritableHandle(t, windows.Handle(eventWriter.Fd()))
	if err := eventWriter.Close(); err != nil {
		_ = windows.CloseHandle(eventHandle)
		t.Fatal(err)
	}

	inputReader, inputWriter := io.Pipe()
	defer inputReader.Close()
	defer inputWriter.Close()
	runResult := make(chan error, 1)
	go func() {
		runResult <- runCommand([]string{
			commandLauncher,
			"--event-handle",
			strconv.FormatUint(uint64(uintptr(eventHandle)), 10),
		}, inputReader)
	}()
	if err := ownerprotocol.WriteFrame(inputWriter, request.Protocol); err != nil {
		_ = windows.CloseHandle(eventHandle)
		t.Fatal(err)
	}
	event, eventErr := readLauncherEvent(eventReader)
	if eventErr != nil {
		_ = inputWriter.CloseWithError(eventErr)
		<-runResult
		t.Fatalf("read launcher event: %v", eventErr)
	}
	if inspectBeforeAcknowledgement != nil {
		inspectBeforeAcknowledgement(event)
	}
	if acknowledgement != nil {
		if err := writeAll(inputWriter, []byte{*acknowledgement}); err != nil {
			t.Fatal(err)
		}
	}
	if err := inputWriter.Close(); err != nil {
		t.Fatal(err)
	}
	runErr := <-runResult
	return event, runErr
}

func duplicateInheritableHandle(t *testing.T, source windows.Handle) windows.Handle {
	t.Helper()
	var duplicate windows.Handle
	if err := windows.DuplicateHandle(
		windows.CurrentProcess(),
		source,
		windows.CurrentProcess(),
		&duplicate,
		0,
		true,
		windows.DUPLICATE_SAME_ACCESS,
	); err != nil {
		t.Fatal(err)
	}
	if duplicate == 0 || duplicate == windows.InvalidHandle {
		t.Fatal("DuplicateHandle returned an invalid event handle")
	}
	return duplicate
}
