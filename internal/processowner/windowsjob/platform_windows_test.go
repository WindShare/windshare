//go:build windows

package windowsjob

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
	"golang.org/x/sys/windows"
)

func TestWindowsSupervisorPreservesCommandAndStreams(t *testing.T) {
	request := windowsIntegrationRequest(t, "echo", 5_000)
	request.Arguments = []string{"plain", "", `quote"slash\`, "<>&\u2028"}
	result := runWindowsSupervisorProcess(t, request, nil)
	if result.err != nil {
		t.Fatalf("supervisor: %v", result.err)
	}
	wantStdout := "stdout\x00fake-status-frame\n" + strings.Join(request.Arguments, "\x1f")
	if result.stdout != wantStdout {
		t.Fatalf("stdout = %q, want %q", result.stdout, wantStdout)
	}
	if result.stderr != "stderr<>&\u2028" {
		t.Fatalf("stderr = %q", result.stderr)
	}
	assertTreeStatus(t, result.status, request, ownerprotocol.TerminationNatural, false, rootNaturalExitCode)
}

func TestWindowsSupervisorDeliversExactRawStdinAfterContainment(t *testing.T) {
	rawInput := []byte{0, 1, 2, 0xff, '\n'}
	request := windowsIntegrationRequest(t, "stdin", 5_000)
	request.Stdin = &ownerprotocol.Stdin{ByteLength: int64(len(rawInput))}
	result := runWindowsSupervisorProcess(t, request, rawInput)
	if result.err != nil {
		t.Fatalf("supervisor: %v", result.err)
	}
	if !bytes.Equal([]byte(result.stdout), rawInput) || result.stderr != "" {
		t.Fatalf("raw stdin delivery = stdout %v, stderr %q", []byte(result.stdout), result.stderr)
	}
	assertTreeStatus(t, result.status, request, ownerprotocol.TerminationNatural, false, rootNaturalExitCode)
}

func TestWindowsSupervisorReportsExactStillActiveExitCode(t *testing.T) {
	request := windowsIntegrationRequest(t, "exit-259", 5_000)
	result := runWindowsSupervisor(t, request, nil)
	if result.err != nil {
		t.Fatalf("supervisor: %v", result.err)
	}
	assertTreeStatus(t, result.status, request, ownerprotocol.TerminationNatural, false, 259)
}

func TestWindowsSupervisorKeepsRejectedTargetSuspended(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "unexpected-release")
	request := windowsIntegrationRequest(t, "unexpected-release", 5_000)
	request.Environment = targetEnvironment("unexpected-release", marker, request.WorkingDirectory)
	result := runWindowsSupervisorWithStartDecision(
		t,
		request,
		nil,
		ownerprotocol.StartDecisionRejected,
		"TEST_REJECTED",
		"test start authority rejected the target",
	)
	if result.err != nil {
		t.Fatalf("supervisor: %v", result.err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected target executed before settlement: %v", err)
	}
	if result.status.TerminationReason != ownerprotocol.TerminationStartRejected ||
		result.status.Target.Outcome != ownerprotocol.TargetNotStarted ||
		result.status.Target.FailureCode != "START_REJECTED" ||
		!strings.Contains(result.status.Target.FailureMessage, "TEST_REJECTED") ||
		result.status.TreeState != ownerprotocol.TreeProvenEmpty ||
		result.status.Cleanup.Outcome != ownerprotocol.CleanupCompleted {
		t.Fatalf("rejected target settlement = %#v", result.status)
	}
}

func TestWindowsSupervisorDeadlineBeforeReleaseDrainsInputWithoutRunningTarget(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "unexpected-release")
	rawInput := []byte("pre-release-input")
	request := windowsIntegrationRequest(t, "unexpected-release", preReleaseDeadlineMS)
	request.Environment = targetEnvironment("unexpected-release", marker, request.WorkingDirectory)
	request.Stdin = &ownerprotocol.Stdin{ByteLength: int64(len(rawInput))}
	result := runWindowsSupervisorWithDelayedStartDecision(
		t,
		request,
		rawInput,
		ownerprotocol.StartDecisionAccepted,
		"",
		"",
		preReleaseDecisionDelay,
	)
	if result.err != nil {
		t.Fatalf("supervisor: %v", result.err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deadline-expired target executed before settlement: %v", err)
	}
	if result.status.TerminationReason != ownerprotocol.TerminationDeadline ||
		result.status.Target.Outcome != ownerprotocol.TargetNotStarted ||
		result.status.Input.Outcome != ownerprotocol.InputNotStarted ||
		result.status.TreeState != ownerprotocol.TreeProvenEmpty ||
		result.status.Cleanup.Outcome != ownerprotocol.CleanupCompleted {
		t.Fatalf("pre-release deadline settlement = %#v", result.status)
	}
}

func TestWindowsSupervisorPreloadedStopWinsDelayedStartDecision(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "unexpected-release")
	request := windowsIntegrationRequest(t, "unexpected-release", 5_000)
	request.Environment = targetEnvironment("unexpected-release", marker, request.WorkingDirectory)
	result := runWindowsSupervisorWithPreloadedStop(t, request, preReleaseDecisionDelay)
	if result.err != nil {
		t.Fatalf("supervisor: %v", result.err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stopped target executed before settlement: %v", err)
	}
	if result.status.TerminationReason != ownerprotocol.TerminationStop ||
		result.status.Target.Outcome != ownerprotocol.TargetNotStarted ||
		result.status.TreeState != ownerprotocol.TreeProvenEmpty ||
		result.status.Cleanup.Outcome != ownerprotocol.CleanupCompleted {
		t.Fatalf("pre-release stop settlement = %#v", result.status)
	}
}

func TestWindowsSupervisorPreloadedParentLossWinsDelayedStartDecision(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "unexpected-release")
	request := windowsIntegrationRequest(t, "unexpected-release", 5_000)
	request.Environment = targetEnvironment("unexpected-release", marker, request.WorkingDirectory)
	result := runWindowsSupervisorWithPreloadedParentLoss(t, request, preReleaseDecisionDelay)
	if result.err != nil {
		t.Fatalf("supervisor: %v", result.err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("parent-orphaned target executed before settlement: %v", err)
	}
	if result.status.TerminationReason != ownerprotocol.TerminationParentLost ||
		result.status.Target.Outcome != ownerprotocol.TargetNotStarted ||
		result.status.TreeState != ownerprotocol.TreeProvenEmpty ||
		result.status.Cleanup.Outcome != ownerprotocol.CleanupCompleted {
		t.Fatalf("pre-release parent-loss settlement = %#v", result.status)
	}
}

func TestWindowsSupervisorWaitsForNaturalDescendantDrain(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "natural-marker")
	request := windowsIntegrationRequest(t, "natural-descendant", 5_000)
	request.Environment = targetEnvironment("natural-descendant", marker, request.WorkingDirectory)
	result := runWindowsSupervisor(t, request, nil)
	if result.err != nil {
		t.Fatalf("supervisor: %v", result.err)
	}
	if encoded, err := os.ReadFile(marker); err != nil || string(encoded) != "natural-descendant" {
		t.Fatalf("natural descendant marker = %q, err = %v", encoded, err)
	}
	assertTreeStatus(t, result.status, request, ownerprotocol.TerminationNatural, false, rootBeforeDescendantExitCode)
}

func TestWindowsSupervisorKillsLingeringDescendantAtDeadline(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "late-marker")
	ready := filepath.Join(t.TempDir(), "descendant-ready")
	request := windowsIntegrationRequest(t, "deadline-descendant", deadlineTestMS)
	request.Environment = targetEnvironment("deadline-descendant", marker, request.WorkingDirectory)
	request.Environment = upsertEnvironmentEntry(request.Environment, testReadyEnvironment, ready)
	result := runWindowsSupervisor(t, request, nil)
	if result.err != nil {
		t.Fatalf("supervisor: %v", result.err)
	}
	if encoded, err := os.ReadFile(ready); err != nil || string(encoded) != "descendant-started" {
		t.Fatalf("deadline descendant readiness = %q, err = %v", encoded, err)
	}
	assertTreeStatus(t, result.status, request, ownerprotocol.TerminationDeadline, true, rootBeforeDescendantExitCode)
	time.Sleep(postDeadlineObservation)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("terminated descendant wrote after authoritative status: %v", err)
	}
}

func TestWindowsSupervisorRejectsBreakawayDescendant(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "breakaway-marker")
	request := windowsIntegrationRequest(t, "breakaway-attempt", 5_000)
	request.Environment = targetEnvironment("breakaway-attempt", marker, request.WorkingDirectory)
	result := runWindowsSupervisor(t, request, nil)
	if result.err != nil {
		t.Fatalf("supervisor: %v", result.err)
	}
	assertTreeStatus(t, result.status, request, ownerprotocol.TerminationNatural, false, 0)
	time.Sleep(postBreakawayObservation)
	encoded, err := os.ReadFile(marker)
	if err != nil || string(encoded) != "breakaway-blocked" {
		t.Fatalf("breakaway marker = %q, err = %v", encoded, err)
	}
}

func TestWindowsSupervisorCrashClosesTheJobLease(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "crash-late-marker")
	ready := filepath.Join(t.TempDir(), "crash-ready")
	request := windowsIntegrationRequest(t, "crash-tree", 15_000)
	request.Environment = targetEnvironment("crash-tree", marker, request.WorkingDirectory)
	request.Environment = upsertEnvironmentEntry(request.Environment, testReadyEnvironment, ready)
	request, coverageDirectory := prepareChildProcessEnvironment(t, request)

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	statusReader, statusWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer statusReader.Close()
	controlReader, controlWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer controlWriter.Close()
	parentReader, parentWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer parentWriter.Close()
	startPipes := newAcceptedStartGatePipes(t)
	startResult := startPipes.consume(request.Protocol)
	childEndpoints := []*os.File{statusWriter, controlReader, parentReader}
	childEndpoints = append(childEndpoints, startPipes.childFiles()...)
	for _, endpoint := range childEndpoints {
		if err := windows.SetHandleInformation(
			windows.Handle(endpoint.Fd()), windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT,
		); err != nil {
			t.Fatal(err)
		}
	}
	var framedRequest bytes.Buffer
	if err := ownerprotocol.WriteFrame(&framedRequest, request.Protocol); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(
		executable,
		commandSupervise,
		"--status-handle", strconv.FormatUint(uint64(statusWriter.Fd()), 10),
		"--control-handle", strconv.FormatUint(uint64(controlReader.Fd()), 10),
		"--ready-stdout",
		"--parent-handle", strconv.FormatUint(uint64(parentReader.Fd()), 10),
	)
	command.Args = append(command.Args, startPipes.arguments()...)
	replacements := map[string]string{testHelperEnvironment: "1"}
	if coverageDirectory != "" {
		replacements["GOCOVERDIR"] = coverageDirectory
	}
	command.Env = replaceEnvironment(os.Environ(), replacements)
	inheritedHandles := make([]syscall.Handle, 0, len(childEndpoints))
	for _, endpoint := range childEndpoints {
		inheritedHandles = append(inheritedHandles, syscall.Handle(endpoint.Fd()))
	}
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:                 true,
		AdditionalInheritedHandles: inheritedHandles,
	}
	command.Stdin = bytes.NewReader(framedRequest.Bytes())
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range childEndpoints {
		if err := endpoint.Close(); err != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			t.Fatal(err)
		}
	}
	select {
	case startErr := <-startResult:
		if startErr != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			t.Fatal(startErr)
		}
	case <-time.After(testMarkerWaitLimit):
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal("start-evidence handshake did not complete")
	}
	ownerWait := make(chan error, 1)
	ownerDone := make(chan struct{})
	go func() {
		ownerWait <- command.Wait()
		close(ownerDone)
	}()
	if err := waitForMarker(ready, ownerDone); err != nil {
		_ = command.Process.Kill()
		waitErr := <-ownerWait
		statusBytes, statusErr := io.ReadAll(statusReader)
		t.Fatalf(
			"%v; owner wait=%v stdout=%q stderr=%q status=%q status_err=%v",
			err,
			waitErr,
			stdout.String(),
			stderr.String(),
			statusBytes,
			statusErr,
		)
	}
	if encoded, err := os.ReadFile(ready); err != nil || string(encoded) != "tree-ready" {
		_ = command.Process.Kill()
		waitErr := <-ownerWait
		t.Fatalf("crash tree readiness = %q, err = %v, owner wait=%v", encoded, err, waitErr)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if waitErr := <-ownerWait; waitErr == nil {
		t.Fatal("abruptly killed supervisor reported success")
	}
	time.Sleep(postCrashObservation)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("descendant survived supervisor Job Object lease closure: %v", err)
	}
	if settlement, err := ownerprotocol.ReadLineDocument[ownerprotocol.Settlement](statusReader); err == nil {
		t.Fatalf("crashed supervisor published authority status: %#v", settlement)
	}
	if stdout.String() != string([]byte{ownerReadyByte}) || stderr.Len() != 0 {
		t.Fatalf("crash fixture wrote target streams: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestWindowsSupervisorPublishesSpawnFailureAfterEmptyJob(t *testing.T) {
	request := windowsIntegrationRequest(t, "echo", 5_000)
	request.Executable = filepath.Join(t.TempDir(), "does-not-exist.exe")
	result := runWindowsSupervisor(t, request, nil)
	if result.err != nil {
		t.Fatalf("supervisor: %v", result.err)
	}
	if result.status.Target.Outcome != ownerprotocol.TargetSpawnFailed {
		t.Fatalf("spawn failure status = %#v", result.status)
	}
	if len(result.status.Target.FailureMessage) == 0 || len(result.status.Target.FailureMessage) > maximumDiagnosticBytes {
		t.Fatalf("spawn failure diagnostic length = %d", len(result.status.Target.FailureMessage))
	}
	if result.status.TreeState != ownerprotocol.TreeProvenEmpty || result.status.TerminationReason != ownerprotocol.TerminationInitializationFailed {
		t.Fatalf("spawn failure authority = %#v", result.status)
	}
}

func TestWindowsSupervisorDrainsLargeDeclaredInputAfterSpawnFailure(t *testing.T) {
	rawInput := bytes.Repeat([]byte{0x7d}, ownerprotocol.MaximumStdinBytes)
	request := windowsIntegrationRequest(t, "echo", 5_000)
	request.Executable = filepath.Join(t.TempDir(), "does-not-exist.exe")
	request.Stdin = &ownerprotocol.Stdin{ByteLength: int64(len(rawInput))}
	result := runWindowsSupervisorProcess(t, request, rawInput)
	if result.err != nil {
		t.Fatalf("supervisor: %v", result.err)
	}
	if result.status.Target.Outcome != ownerprotocol.TargetSpawnFailed ||
		result.status.Input.Outcome != ownerprotocol.InputNotStarted ||
		result.status.TreeState != ownerprotocol.TreeProvenEmpty {
		t.Fatalf("spawn failure with drained input = %#v", result.status)
	}
}

func TestRunSuperviseOwnsAuthenticatedEndpointsInProcess(t *testing.T) {
	t.Run("exact input", func(t *testing.T) {
		rawInput := []byte("direct-supervise-input")
		request := windowsIntegrationRequest(t, "stdin-silent", 5_000)
		request.Stdin = &ownerprotocol.Stdin{ByteLength: int64(len(rawInput))}
		result := runWindowsSuperviseCommandInProcess(t, request, rawInput)
		if result.err != nil {
			t.Fatal(result.err)
		}
		assertTreeStatus(t, result.status, request, ownerprotocol.TerminationNatural, false, rootNaturalExitCode)
	})

	t.Run("spawn failure drains input", func(t *testing.T) {
		rawInput := bytes.Repeat([]byte{0x51}, ownerprotocol.MaximumStdinBytes)
		request := windowsIntegrationRequest(t, "echo", 5_000)
		request.Executable = filepath.Join(t.TempDir(), "missing.exe")
		request.Stdin = &ownerprotocol.Stdin{ByteLength: int64(len(rawInput))}
		result := runWindowsSuperviseCommandInProcess(t, request, rawInput)
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.status.Target.Outcome != ownerprotocol.TargetSpawnFailed ||
			result.status.Input.Outcome != ownerprotocol.InputNotStarted ||
			result.status.TreeState != ownerprotocol.TreeProvenEmpty {
			t.Fatalf("spawn-failure settlement = %#v", result.status)
		}
	})
}

func TestWindowsSupervisorParentRequestTerminatesRoot(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "root-ready")
	request := windowsIntegrationRequest(t, "long-root", 5_000)
	request.Environment = targetEnvironment("long-root", ready, request.WorkingDirectory)
	result := runWindowsSupervisorThroughExpiringParent(t, request)
	if result.err != nil {
		t.Fatalf("supervisor: %v", result.err)
	}
	assertPrivateTreeStatus(
		t,
		result.status,
		request,
		ownerprotocol.TerminationParentLost,
		false,
	)
}

func TestWindowsSupervisorAuthenticatesCreateNewControlRequest(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "root-ready")
	request := windowsIntegrationRequest(t, "long-root", 5_000)
	request.Environment = targetEnvironment("long-root", ready, request.WorkingDirectory)
	result := runWindowsSupervisorWithControl(t, request)
	if result.err != nil {
		t.Fatalf("supervisor: %v", result.err)
	}
	assertPrivateTreeStatus(
		t,
		result.status,
		request,
		ownerprotocol.TerminationStop,
		false,
	)
}

func TestTerminationExitCodesAreSecretDerivedAndDistinct(t *testing.T) {
	secret := bytes.Repeat([]byte{0x5a}, terminationExitCodeKeyBytes)
	first, err := generateTerminationExitCodesFrom(bytes.NewReader(secret))
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := generateTerminationExitCodesFrom(bytes.NewReader(secret))
	if err != nil {
		t.Fatal(err)
	}
	if repeated != first {
		t.Fatalf("same private secret produced different termination codes: %#v then %#v", first, repeated)
	}
	alternate, err := generateTerminationExitCodesFrom(bytes.NewReader(bytes.Repeat([]byte{0xa5}, terminationExitCodeKeyBytes)))
	if err != nil {
		t.Fatal(err)
	}
	if alternate == first {
		t.Fatalf("different private secrets produced identical termination code sets: %#v", first)
	}
	seen := make(map[uint32]struct{}, 3)
	for reason, code := range map[string]uint32{
		"deadline":  first.deadline,
		"parent":    first.parent,
		"authority": first.authority,
	} {
		if code == 0 || code == windowsStillActiveExitCode {
			t.Fatalf("%s termination code is ambiguous: %d", reason, code)
		}
		if code&uint32(windows.APPLICATION_ERROR) == 0 {
			t.Fatalf("%s termination code lacks APPLICATION_ERROR class: %#x", reason, code)
		}
		if _, duplicate := seen[code]; duplicate {
			t.Fatalf("termination code is reused across reasons: %#x", code)
		}
		seen[code] = struct{}{}
	}
}

func TestTargetVisibleIdentityCannotSpoofTerminationIntervention(t *testing.T) {
	actual, err := generateTerminationExitCodesFrom(bytes.NewReader(bytes.Repeat([]byte{0xc3}, terminationExitCodeKeyBytes)))
	if err != nil {
		t.Fatal(err)
	}
	identityKey := sha256.Sum256([]byte(testIdentity.RunID + "\x00" + testIdentity.OperationID + "\x00" + testIdentity.Scenario))
	spoof := derivePrivateExitCode(identityKey[:], deadlineExitCodeDomain, make(map[uint32]struct{}))
	if spoof == actual.deadline {
		t.Fatal("deterministic test material unexpectedly collided")
	}
	job := newRootExitRaceJob(actual)
	job.terminated = true
	intervention := terminationIntervention{
		exitCode: actual.deadline,
		snapshot: targetMemberSnapshot{totalProcessesBefore: 1, members: []processExitAuthority{
			&fixedProcessExitAuthority{pid: 1, exitCode: spoof},
		}},
		reason:   ownerprotocol.TerminationDeadline,
		timedOut: true,
	}
	defer intervention.snapshot.close()
	reason, timedOut, err := reconcileTerminationIntervention(job, intervention, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if reason != ownerprotocol.TerminationNatural || timedOut {
		t.Fatalf("target-visible spoof authenticated as (%q, %t)", reason, timedOut)
	}
}

func TestWindowsLifecycleEvidenceHelpersRejectAmbiguity(t *testing.T) {
	if err := Run(nil, bytes.NewReader(nil)); err == nil {
		t.Fatal("empty Windows owner command was accepted")
	}
	if !validNamedPipePath(`\\.\pipe\private-owner`) ||
		validNamedPipePath(`C:\private-owner`) || validNamedPipePath(`\\.\pipe\..\escape`) {
		t.Fatal("named-pipe path authority was classified incorrectly")
	}
	if evidence := completedInputEvidence(nil); evidence.Outcome != ownerprotocol.InputDelivered {
		t.Fatalf("completed input evidence = %#v", evidence)
	}
	inputFailure := errors.New("input failed")
	if evidence := completedInputEvidence(inputFailure); evidence.Outcome != ownerprotocol.InputFailed {
		t.Fatalf("failed input evidence = %#v", evidence)
	}
	if evidence := lostInputEvidence(inputFailure); evidence.Outcome != ownerprotocol.InputEvidenceLost {
		t.Fatalf("lost input evidence = %#v", evidence)
	}
	if !strings.Contains(jobProcessEvidenceLimitError(7).Error(), "7") {
		t.Fatal("process evidence limit omitted its bound")
	}
	query := jobProcessIDQuery{assigned: 5, capacity: 2, callErr: windows.ERROR_MORE_DATA}
	if capacity, err := query.retryCapacity(10); err != nil || capacity != 5 {
		t.Fatalf("retry capacity = %d, %v", capacity, err)
	}
}

func mustTerminationExitCodes(t *testing.T, identity ownerprotocol.Identity) terminationExitCodes {
	t.Helper()
	_ = identity
	codes, err := generateTerminationExitCodes()
	if err != nil {
		t.Fatal(err)
	}
	return codes
}

func TestWindowsEnvironmentUsesOrdinalIgnoreCaseUniqueness(t *testing.T) {
	equal, err := windowsEnvironmentNamesEqual("Ä_NAME", "ä_name")
	if err != nil {
		t.Fatal(err)
	}
	if !equal {
		t.Fatal("CompareStringOrdinal did not identify non-ASCII case pair")
	}
	if err := validateWindowsEnvironment([]ownerprotocol.EnvironmentEntry{{Name: "Ä_NAME"}, {Name: "ä_name"}}); err == nil {
		t.Fatal("Windows-case-insensitive duplicate environment was accepted")
	}
}
