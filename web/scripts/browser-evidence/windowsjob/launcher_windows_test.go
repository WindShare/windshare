//go:build windows

package main

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/windshare/windshare/internal/testnetwork"
)

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
	})
	if runErr != nil {
		t.Fatalf("launcher command: %v", runErr)
	}
	if event.Type != launcherEventRootStarted || event.PID == 0 || event.ProcessHandle == 0 || event.SpawnFailure != nil {
		t.Fatalf("root-started event = %#v", event)
	}
}

func TestLauncherReleasesRootWithoutWaitingForItsExit(t *testing.T) {
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

func TestLauncherCommandReportsBoundedSpawnFailure(t *testing.T) {
	request := windowsIntegrationRequest(t, "echo", 5_000)
	request.Executable = filepath.Join(t.TempDir(), "missing-e\u0301.exe")
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
	request startRequest,
	acknowledgement *byte,
	inspectBeforeAcknowledgement func(launcherEvent),
) (launcherEvent, error) {
	t.Helper()
	testnetwork.RequireOSNetwork(t)
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
	if err := writeCanonicalFrame(inputWriter, request); err != nil {
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
