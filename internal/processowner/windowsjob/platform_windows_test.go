//go:build windows

package windowsjob

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
	"golang.org/x/sys/windows"
)

const (
	testHelperEnvironment        = "WINDSHARE_WINDOWSJOB_TEST_HELPER"
	testTargetEnvironment        = "WINDSHARE_WINDOWSJOB_TEST_TARGET"
	testMarkerEnvironment        = "WINDSHARE_WINDOWSJOB_TEST_MARKER"
	testReadyEnvironment         = "WINDSHARE_WINDOWSJOB_TEST_READY"
	testCWDEnvironment           = "WINDSHARE_WINDOWSJOB_TEST_CWD"
	rootNaturalExitCode          = 37
	rootBeforeDescendantExitCode = 7
	launcherReleaseRootExitCode  = 46
)

const (
	testMarkerPollInterval         = 20 * time.Millisecond
	testMarkerWaitLimit            = 10 * time.Second
	rootReadyDelay                 = 300 * time.Millisecond
	launcherReleaseRootDelay       = 750 * time.Millisecond
	naturalDescendantDelay         = 250 * time.Millisecond
	deadlineTestMS           int64 = 3_000
	preReleaseDeadlineMS     int64 = 50
	preReleaseDecisionDelay        = 200 * time.Millisecond
	deadlineDescendantDelay        = 5 * time.Second
	postDeadlineObservation        = 3 * time.Second
	breakawayWriterDelay           = 1 * time.Second
	postBreakawayObservation       = 1_500 * time.Millisecond
	crashDescendantDelay           = 2 * time.Second
	postCrashObservation           = 3 * time.Second
)

func TestMain(testMain *testing.M) {
	if target := os.Getenv(testTargetEnvironment); target != "" {
		os.Exit(runWindowsTargetFixture(target))
	}
	if os.Getenv(testHelperEnvironment) == "1" {
		if err := runCommand(os.Args[1:], os.Stdin); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(testMain.Run())
}

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

type windowsSupervisorResult struct {
	status ownerprotocol.Settlement
	stdout string
	stderr string
	err    error
}

func runSupervisorPlatformAccepted(
	request supervisionRequest,
	settlements *settlementSink,
	control *os.File,
	rawInput *os.File,
	ready io.Writer,
) error {
	return runSupervisorPlatformWithDecision(
		request,
		settlements,
		control,
		rawInput,
		ready,
		ownerprotocol.StartDecisionAccepted,
		"",
		"",
		0,
	)
}

func runSupervisorPlatformWithDecision(
	request supervisionRequest,
	settlements *settlementSink,
	control *os.File,
	rawInput *os.File,
	ready io.Writer,
	outcome string,
	failureCode string,
	failureMessage string,
	decisionDelay time.Duration,
) error {
	evidenceReader, evidenceWriter, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create test start-evidence pipe: %w", err)
	}
	decisionReader, decisionWriter, err := os.Pipe()
	if err != nil {
		return errors.Join(
			fmt.Errorf("create test start-decision pipe: %w", err),
			evidenceReader.Close(),
			evidenceWriter.Close(),
		)
	}
	consumerResult := make(chan error, 1)
	go func() {
		defer evidenceReader.Close()
		defer decisionWriter.Close()
		evidence, readErr := ownerprotocol.ReadFrame[ownerprotocol.StartEvidence](evidenceReader)
		if errors.Is(readErr, io.EOF) {
			consumerResult <- nil
			return
		}
		if readErr != nil {
			consumerResult <- readErr
			return
		}
		trailing, trailingErr := io.ReadAll(evidenceReader)
		if trailingErr != nil || len(trailing) != 0 {
			consumerResult <- errors.Join(
				trailingErr,
				errors.New("test start-evidence stream contains trailing bytes"),
			)
			return
		}
		if validationErr := ownerprotocol.ValidateStartEvidenceForRequest(evidence, request.Protocol); validationErr != nil {
			consumerResult <- validationErr
			return
		}
		if decisionDelay > 0 {
			time.Sleep(decisionDelay)
		}
		consumerResult <- ownerprotocol.WriteFrame(
			decisionWriter,
			ownerprotocol.NewStartDecision(evidence, outcome, failureCode, failureMessage),
		)
	}()
	runErr := runSupervisorPlatform(
		request,
		settlements,
		control,
		rawInput,
		newStartGate(evidenceWriter, decisionReader, request.Protocol),
		ready,
	)
	ownerCloseErr := errors.Join(closeOptionalFile(evidenceWriter), closeOptionalFile(decisionReader))
	return errors.Join(runErr, ownerCloseErr, <-consumerResult)
}

func runWindowsSupervisor(t *testing.T, request supervisionRequest, rawInput []byte) windowsSupervisorResult {
	return runWindowsSupervisorWithStartDecision(
		t,
		request,
		rawInput,
		ownerprotocol.StartDecisionAccepted,
		"",
		"",
	)
}

func runWindowsSupervisorWithStartDecision(
	t *testing.T,
	request supervisionRequest,
	rawInput []byte,
	outcome string,
	failureCode string,
	failureMessage string,
) windowsSupervisorResult {
	return runWindowsSupervisorWithDelayedStartDecision(
		t,
		request,
		rawInput,
		outcome,
		failureCode,
		failureMessage,
		0,
	)
}

func runWindowsSupervisorWithDelayedStartDecision(
	t *testing.T,
	request supervisionRequest,
	rawInput []byte,
	outcome string,
	failureCode string,
	failureMessage string,
	decisionDelay time.Duration,
) windowsSupervisorResult {
	return runWindowsSupervisorWithStartBoundary(
		t,
		request,
		rawInput,
		outcome,
		failureCode,
		failureMessage,
		decisionDelay,
		false,
		false,
	)
}

func runWindowsSupervisorWithPreloadedStop(
	t *testing.T,
	request supervisionRequest,
	decisionDelay time.Duration,
) windowsSupervisorResult {
	return runWindowsSupervisorWithStartBoundary(
		t,
		request,
		nil,
		ownerprotocol.StartDecisionAccepted,
		"",
		"",
		decisionDelay,
		true,
		false,
	)
}

func runWindowsSupervisorWithPreloadedParentLoss(
	t *testing.T,
	request supervisionRequest,
	decisionDelay time.Duration,
) windowsSupervisorResult {
	return runWindowsSupervisorWithStartBoundary(
		t,
		request,
		nil,
		ownerprotocol.StartDecisionAccepted,
		"",
		"",
		decisionDelay,
		false,
		true,
	)
}

func runWindowsSupervisorWithStartBoundary(
	t *testing.T,
	request supervisionRequest,
	rawInput []byte,
	outcome string,
	failureCode string,
	failureMessage string,
	decisionDelay time.Duration,
	preloadStop bool,
	preloadParentLoss bool,
) windowsSupervisorResult {
	t.Helper()
	request, coverageDirectory := prepareChildProcessEnvironment(t, request)
	parentReader, parentWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer parentReader.Close()
	defer parentWriter.Close()
	request.ParentHandle = parentReader.Fd()
	if preloadParentLoss {
		if err := parentWriter.Close(); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(testHelperEnvironment, "1")
	if coverageDirectory != "" {
		t.Setenv("GOCOVERDIR", coverageDirectory)
	}
	settlements, output := newTestSettlementSink(t, request)
	controlReader, controlWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer controlReader.Close()
	defer controlWriter.Close()
	if preloadStop {
		control := ownerprotocol.Control{
			SchemaVersion: ownerprotocol.ControlSchemaVersion,
			Identity:      request.Identity,
			Reason:        ownerprotocol.ControlReasonStop,
		}
		if err := errors.Join(ownerprotocol.WriteFrame(controlWriter, control), controlWriter.Close()); err != nil {
			t.Fatal(err)
		}
	}
	rawReader, rawWrite := exactRawInputReader(t, request, rawInput)
	runErr := runSupervisorPlatformWithDecision(
		request,
		settlements,
		controlReader,
		rawReader,
		io.Discard,
		outcome,
		failureCode,
		failureMessage,
		decisionDelay,
	)
	if closeErr := closeOptionalFile(rawReader); runErr == nil && closeErr != nil {
		runErr = closeErr
	}
	if rawWrite != nil {
		if writeErr := <-rawWrite; runErr == nil && writeErr != nil {
			runErr = writeErr
		}
	}
	result := windowsSupervisorResult{err: runErr}
	if runErr == nil {
		result.status = decodeTestSettlement(t, output)
	}
	return result
}

type acceptedStartGatePipes struct {
	evidenceReader *os.File
	evidenceWriter *os.File
	decisionReader *os.File
	decisionWriter *os.File
}

func newAcceptedStartGatePipes(t *testing.T) *acceptedStartGatePipes {
	t.Helper()
	evidenceReader, evidenceWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	decisionReader, decisionWriter, err := os.Pipe()
	if err != nil {
		_ = evidenceReader.Close()
		_ = evidenceWriter.Close()
		t.Fatal(err)
	}
	return &acceptedStartGatePipes{
		evidenceReader: evidenceReader,
		evidenceWriter: evidenceWriter,
		decisionReader: decisionReader,
		decisionWriter: decisionWriter,
	}
}

func (pipes *acceptedStartGatePipes) childFiles() []*os.File {
	return []*os.File{pipes.evidenceWriter, pipes.decisionReader}
}

func (pipes *acceptedStartGatePipes) arguments() []string {
	return []string{
		"--start-evidence-handle", strconv.FormatUint(uint64(pipes.evidenceWriter.Fd()), 10),
		"--start-decision-handle", strconv.FormatUint(uint64(pipes.decisionReader.Fd()), 10),
	}
}

func (pipes *acceptedStartGatePipes) consume(request ownerprotocol.Request) <-chan error {
	result := make(chan error, 1)
	go func() {
		defer pipes.evidenceReader.Close()
		defer pipes.decisionWriter.Close()
		evidence, err := ownerprotocol.ReadFrame[ownerprotocol.StartEvidence](pipes.evidenceReader)
		if errors.Is(err, io.EOF) {
			result <- nil
			return
		}
		if err != nil {
			result <- err
			return
		}
		trailing, trailingErr := io.ReadAll(pipes.evidenceReader)
		if trailingErr != nil || len(trailing) != 0 {
			result <- errors.Join(trailingErr, errors.New("test start-evidence stream contains trailing bytes"))
			return
		}
		if err := ownerprotocol.ValidateStartEvidenceForRequest(evidence, request); err != nil {
			result <- err
			return
		}
		result <- ownerprotocol.WriteFrame(
			pipes.decisionWriter,
			ownerprotocol.NewStartDecision(evidence, ownerprotocol.StartDecisionAccepted, "", ""),
		)
	}()
	return result
}

func runWindowsSuperviseCommandInProcess(
	t *testing.T,
	request supervisionRequest,
	rawInput []byte,
) windowsSupervisorResult {
	t.Helper()
	request, coverageDirectory := prepareChildProcessEnvironment(t, request)
	t.Setenv(testHelperEnvironment, "1")
	if coverageDirectory != "" {
		t.Setenv("GOCOVERDIR", coverageDirectory)
	}
	statusReader, statusWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	controlReader, controlWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	parentReader, parentWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	startPipes := newAcceptedStartGatePipes(t)
	startResult := startPipes.consume(request.Protocol)
	var rawReader, rawWriter *os.File
	if request.Stdin != nil {
		rawReader, rawWriter, err = os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []*os.File{
		statusReader, statusWriter, controlReader, controlWriter, parentReader, parentWriter,
		rawReader, rawWriter,
		startPipes.evidenceReader, startPipes.evidenceWriter,
		startPipes.decisionReader, startPipes.decisionWriter,
	} {
		if file != nil {
			defer file.Close()
		}
	}
	statusHandle := duplicateSuperviseEndpoint(t, statusWriter)
	controlHandle := duplicateSuperviseEndpoint(t, controlReader)
	parentHandle := duplicateSuperviseEndpoint(t, parentReader)
	startEvidenceHandle := duplicateSuperviseEndpoint(t, startPipes.evidenceWriter)
	startDecisionHandle := duplicateSuperviseEndpoint(t, startPipes.decisionReader)
	if err := errors.Join(startPipes.evidenceWriter.Close(), startPipes.decisionReader.Close()); err != nil {
		t.Fatal(err)
	}
	arguments := []string{
		commandSupervise,
		"--status-handle", strconv.FormatUint(uint64(statusHandle), 10),
		"--control-handle", strconv.FormatUint(uint64(controlHandle), 10),
		"--parent-handle", strconv.FormatUint(uint64(parentHandle), 10),
		"--start-evidence-handle", strconv.FormatUint(uint64(startEvidenceHandle), 10),
		"--start-decision-handle", strconv.FormatUint(uint64(startDecisionHandle), 10),
		"--ready-stdout",
	}
	var rawWriteResult chan error
	if rawWriter != nil {
		rawHandle := duplicateSuperviseEndpoint(t, rawReader)
		arguments = append(arguments, "--input-handle", strconv.FormatUint(uint64(rawHandle), 10))
		rawWriteResult = make(chan error, 1)
		payload := bytes.Clone(rawInput)
		go func() {
			writeErr := writeAll(rawWriter, payload)
			for index := range payload {
				payload[index] = 0
			}
			rawWriteResult <- errors.Join(writeErr, rawWriter.Close())
		}()
	}
	var encodedRequest bytes.Buffer
	if err := ownerprotocol.WriteFrame(&encodedRequest, request.Protocol); err != nil {
		t.Fatal(err)
	}
	var ready bytes.Buffer
	runErr := runCommandWithReady(arguments, &encodedRequest, &ready)
	runErr = errors.Join(runErr, <-startResult)
	if ready.String() != string([]byte{ownerReadyByte}) {
		return windowsSupervisorResult{err: errors.Join(runErr, fmt.Errorf("readiness = %v", ready.Bytes()))}
	}
	if rawWriteResult != nil {
		runErr = errors.Join(runErr, <-rawWriteResult)
	}
	_ = statusWriter.Close()
	_ = controlWriter.Close()
	_ = parentWriter.Close()
	result := windowsSupervisorResult{err: runErr}
	settlement, statusErr := ownerprotocol.ReadLineDocument[ownerprotocol.Settlement](statusReader)
	if statusErr == nil {
		statusErr = ownerprotocol.ValidateSettlementForRequest(settlement, request.Protocol)
	}
	result.err = errors.Join(result.err, statusErr)
	if statusErr == nil {
		result.status = settlement
	}
	return result
}

func duplicateSuperviseEndpoint(t *testing.T, file *os.File) windows.Handle {
	t.Helper()
	var duplicate windows.Handle
	if err := windows.DuplicateHandle(
		windows.CurrentProcess(), windows.Handle(file.Fd()), windows.CurrentProcess(), &duplicate,
		0, true, windows.DUPLICATE_SAME_ACCESS,
	); err != nil {
		t.Fatal(err)
	}
	return duplicate
}

func runWindowsSupervisorProcess(t *testing.T, request supervisionRequest, rawInput []byte) windowsSupervisorResult {
	t.Helper()
	return runWindowsSupervisorProcessAt(t, request, rawInput)
}

func runWindowsSupervisorProcessAt(t *testing.T, request supervisionRequest, rawInput []byte) windowsSupervisorResult {
	t.Helper()
	request, coverageDirectory := prepareChildProcessEnvironment(t, request)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	statusReader, statusWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	controlReader, controlWriter, err := os.Pipe()
	if err != nil {
		_ = statusReader.Close()
		_ = statusWriter.Close()
		t.Fatal(err)
	}
	parentReader, parentWriter, err := os.Pipe()
	if err != nil {
		_ = statusReader.Close()
		_ = statusWriter.Close()
		_ = controlReader.Close()
		_ = controlWriter.Close()
		t.Fatal(err)
	}
	rawReader, rawWrite := exactRawInputReader(t, request, rawInput)
	startPipes := newAcceptedStartGatePipes(t)
	startResult := startPipes.consume(request.Protocol)
	childEndpoints := []*os.File{statusWriter, controlReader, parentReader}
	childEndpoints = append(childEndpoints, startPipes.childFiles()...)
	if rawReader != nil {
		childEndpoints = append(childEndpoints, rawReader)
	}
	for _, endpoint := range childEndpoints {
		if err := windows.SetHandleInformation(
			windows.Handle(endpoint.Fd()), windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT,
		); err != nil {
			t.Fatal(err)
		}
	}
	var requestBytes bytes.Buffer
	if err := ownerprotocol.WriteFrame(&requestBytes, request.Protocol); err != nil {
		t.Fatal(err)
	}
	defer parentWriter.Close()
	defer controlWriter.Close()
	defer statusReader.Close()
	arguments := []string{
		commandSupervise,
		"--status-handle", strconv.FormatUint(uint64(statusWriter.Fd()), 10),
		"--control-handle", strconv.FormatUint(uint64(controlReader.Fd()), 10),
		"--parent-handle", strconv.FormatUint(uint64(parentReader.Fd()), 10),
		"--ready-stdout",
	}
	arguments = append(arguments, startPipes.arguments()...)
	if rawReader != nil {
		arguments = append(arguments, "--input-handle", strconv.FormatUint(uint64(rawReader.Fd()), 10))
	}
	command := exec.Command(
		executable,
		arguments...,
	)
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
		HideWindow: true, AdditionalInheritedHandles: inheritedHandles,
	}
	command.Stdin = bytes.NewReader(requestBytes.Bytes())
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		for _, endpoint := range childEndpoints {
			_ = endpoint.Close()
		}
		t.Fatal(err)
	}
	for _, endpoint := range childEndpoints {
		if err := endpoint.Close(); err != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			t.Fatal(err)
		}
	}
	waitResult := make(chan error, 1)
	go func() { waitResult <- command.Wait() }()
	var waitErr error
	select {
	case waitErr = <-waitResult:
	case <-time.After(testMarkerWaitLimit):
		_ = command.Process.Kill()
		waitErr = <-waitResult
		t.Fatalf("owner process exceeded its bounded test lease: %v", waitErr)
	}
	if rawWrite != nil {
		if writeErr := <-rawWrite; waitErr == nil && writeErr != nil {
			waitErr = writeErr
		}
	}
	waitErr = errors.Join(waitErr, <-startResult)
	_ = controlWriter.Close()
	_ = parentWriter.Close()
	output := stdout.Bytes()
	if len(output) == 0 || output[0] != ownerReadyByte {
		t.Fatalf("owner readiness output = %v", output)
	}
	result := windowsSupervisorResult{stdout: string(output[1:]), stderr: stderr.String(), err: waitErr}
	settlement, statusErr := ownerprotocol.ReadLineDocument[ownerprotocol.Settlement](statusReader)
	if statusErr == nil {
		statusErr = ownerprotocol.ValidateSettlementForRequest(settlement, request.Protocol)
	}
	if result.err == nil && statusErr != nil {
		result.err = statusErr
	}
	if statusErr == nil {
		result.status = settlement
	}
	return result
}

func exactRawInputReader(
	t *testing.T,
	request supervisionRequest,
	rawInput []byte,
) (*os.File, <-chan error) {
	t.Helper()
	if request.Stdin == nil {
		if len(rawInput) != 0 {
			t.Fatal("test supplied undeclared raw stdin")
		}
		return nil, nil
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(rawInput)) != request.Stdin.ByteLength {
		_ = reader.Close()
		_ = writer.Close()
		t.Fatalf("test raw stdin length = %d, declared %d", len(rawInput), request.Stdin.ByteLength)
	}
	payload := bytes.Clone(rawInput)
	result := make(chan error, 1)
	go func() {
		defer func() {
			for index := range payload {
				payload[index] = 0
			}
		}()
		result <- errors.Join(writeAll(writer, payload), writer.Close())
	}()
	return reader, result
}

func runWindowsSupervisorThroughExpiringParent(
	t *testing.T,
	request supervisionRequest,
) windowsSupervisorResult {
	t.Helper()
	request, coverageDirectory := prepareChildProcessEnvironment(t, request)
	ready := environmentValue(request.Environment, testMarkerEnvironment)
	if ready == "" {
		t.Fatal("parent-loss request has no readiness marker")
	}
	parentReader, parentWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer parentReader.Close()
	request.ParentHandle = parentReader.Fd()
	controlReader, controlWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer controlReader.Close()
	defer controlWriter.Close()
	settlements, output := newTestSettlementSink(t, request)
	t.Setenv(testHelperEnvironment, "1")
	if coverageDirectory != "" {
		t.Setenv("GOCOVERDIR", coverageDirectory)
	}
	finished := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		finished <- runSupervisorPlatformAccepted(request, settlements, controlReader, nil, io.Discard)
		close(done)
	}()
	if err := waitForMarker(ready, done); err != nil {
		return windowsSupervisorResult{err: err}
	}
	if err := parentWriter.Close(); err != nil {
		return windowsSupervisorResult{err: err}
	}
	runErr := <-finished
	result := windowsSupervisorResult{err: runErr}
	if runErr == nil {
		result.status = decodeTestSettlement(t, output)
	}
	return result
}

func runWindowsSupervisorWithControl(t *testing.T, request supervisionRequest) windowsSupervisorResult {
	t.Helper()
	request, coverageDirectory := prepareChildProcessEnvironment(t, request)
	parentReader, parentWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer parentReader.Close()
	defer parentWriter.Close()
	request.ParentHandle = parentReader.Fd()
	t.Setenv(testHelperEnvironment, "1")
	if coverageDirectory != "" {
		t.Setenv("GOCOVERDIR", coverageDirectory)
	}
	settlements, output := newTestSettlementSink(t, request)
	controlReader, controlWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer controlReader.Close()
	defer controlWriter.Close()
	ready := environmentValue(request.Environment, testMarkerEnvironment)
	if ready == "" {
		t.Fatal("control request has no readiness marker")
	}
	controlResult := make(chan error, 1)
	go func() {
		if err := waitForMarker(ready, nil); err != nil {
			controlResult <- err
			return
		}
		control := ownerprotocol.Control{
			SchemaVersion: ownerprotocol.ControlSchemaVersion,
			Identity:      request.Identity,
			Reason:        ownerprotocol.ControlReasonStop,
		}
		controlResult <- errors.Join(
			ownerprotocol.WriteFrame(controlWriter, control),
			controlWriter.Close(),
		)
	}()
	rawReader, _ := exactRawInputReader(t, request, nil)
	runErr := runSupervisorPlatformAccepted(request, settlements, controlReader, rawReader, io.Discard)
	_ = closeOptionalFile(rawReader)
	if controlErr := <-controlResult; runErr == nil && controlErr != nil {
		runErr = controlErr
	}
	result := windowsSupervisorResult{err: runErr}
	if runErr == nil {
		result.status = decodeTestSettlement(t, output)
	}
	return result
}

func prepareChildProcessEnvironment(t *testing.T, request supervisionRequest) (supervisionRequest, string) {
	t.Helper()
	coverageDirectory := ""
	if testing.CoverMode() != "" {
		coverageDirectory = t.TempDir()
		request.Environment = upsertEnvironmentEntry(request.Environment, "GOCOVERDIR", coverageDirectory)
	}
	request.Protocol = ownerprotocol.NewRequest(request.Identity, ownerprotocol.Command{
		Executable: request.Executable, Arguments: request.Arguments,
		WorkingDirectory: request.WorkingDirectory, Environment: request.Environment, Stdin: request.Stdin,
	}, request.DeadlineMilliseconds, request.TerminationGraceMilliseconds)
	return request, coverageDirectory
}

func waitForMarker(path string, finished <-chan struct{}) error {
	deadline := time.NewTimer(testMarkerWaitLimit)
	defer deadline.Stop()
	poll := time.NewTicker(testMarkerPollInterval)
	defer poll.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect readiness marker: %w", err)
		}
		select {
		case <-finished:
			return errors.New("supervisor finished before target readiness marker")
		case <-deadline.C:
			return errors.New("target readiness marker was not published in time")
		case <-poll.C:
		}
	}
}

func windowsIntegrationRequest(t *testing.T, target string, deadlineMS int64) supervisionRequest {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Clean(t.TempDir())
	identity := ownerprotocol.Identity{
		RunID: "windows-owner-tests", OperationID: "windows-integration-" + target, Scenario: target,
	}
	protocolRequest := ownerprotocol.NewRequest(identity, ownerprotocol.Command{
		Executable: filepath.Clean(executable), Arguments: []string{}, WorkingDirectory: cwd,
		Environment: targetEnvironment(target, "", cwd),
	}, deadlineMS, 3_000)
	return newSupervisionRequest(protocolRequest, 0)
}

func targetEnvironment(target, marker, cwd string) []ownerprotocol.EnvironmentEntry {
	values := map[string]string{
		testTargetEnvironment: target,
		testCWDEnvironment:    cwd,
	}
	if marker != "" {
		values[testMarkerEnvironment] = marker
	}
	if systemRoot := os.Getenv("SystemRoot"); systemRoot != "" {
		values["SystemRoot"] = systemRoot
	}
	environment := make([]ownerprotocol.EnvironmentEntry, 0, len(values))
	for name, value := range values {
		environment = append(environment, ownerprotocol.EnvironmentEntry{Name: name, Value: value})
	}
	sort.Slice(environment, func(left, right int) bool {
		return strings.ToLower(environment[left].Name) < strings.ToLower(environment[right].Name)
	})
	return environment
}

func upsertEnvironmentEntry(environment []ownerprotocol.EnvironmentEntry, name, value string) []ownerprotocol.EnvironmentEntry {
	result := make([]ownerprotocol.EnvironmentEntry, 0, len(environment)+1)
	replaced := false
	for _, entry := range environment {
		if strings.EqualFold(entry.Name, name) {
			if !replaced {
				result = append(result, ownerprotocol.EnvironmentEntry{Name: name, Value: value})
				replaced = true
			}
			continue
		}
		result = append(result, entry)
	}
	if !replaced {
		result = append(result, ownerprotocol.EnvironmentEntry{Name: name, Value: value})
	}
	sort.Slice(result, func(left, right int) bool {
		return strings.ToLower(result[left].Name) < strings.ToLower(result[right].Name)
	})
	return result
}

func environmentValue(environment []ownerprotocol.EnvironmentEntry, name string) string {
	for _, entry := range environment {
		if strings.EqualFold(entry.Name, name) {
			return entry.Value
		}
	}
	return ""
}

func settledInputOutcome(request supervisionRequest) string {
	if request.Stdin == nil {
		return ownerprotocol.InputNotRequested
	}
	return ownerprotocol.InputDelivered
}

func assertTreeStatus(t *testing.T, status ownerprotocol.Settlement, request supervisionRequest, reason string, timedOut bool, exitCode uint32) {
	t.Helper()
	if status.SchemaVersion != ownerprotocol.SettlementSchemaVersion || status.Identity != request.Identity {
		t.Fatalf("status identity = %#v", status)
	}
	if status.TreeState != ownerprotocol.TreeProvenEmpty || status.TerminationReason != reason ||
		(reason == ownerprotocol.TerminationDeadline) != timedOut {
		t.Fatalf("status outcome = %#v", status)
	}
	if status.Platform.ActiveProcessCount == nil || *status.Platform.ActiveProcessCount != 0 ||
		status.Platform.Root == nil || status.Platform.Root.ExitCode == nil ||
		uint32(*status.Platform.Root.ExitCode) != exitCode {
		t.Fatalf("status authority = %#v", status)
	}
	if status.Input.Outcome != settledInputOutcome(request) {
		t.Fatalf("status input outcome = %q, want %q", status.Input.Outcome, settledInputOutcome(request))
	}
}

func assertPrivateTreeStatus(
	t *testing.T,
	status ownerprotocol.Settlement,
	request supervisionRequest,
	reason string,
	timedOut bool,
) {
	t.Helper()
	if status.Platform.Root == nil || status.Platform.Root.ExitCode == nil {
		t.Fatalf("status omits exact root termination evidence: %#v", status)
	}
	exitCode := uint32(*status.Platform.Root.ExitCode)
	assertTreeStatus(t, status, request, reason, timedOut, exitCode)
	if exitCode == 0 || exitCode == windowsStillActiveExitCode || exitCode&uint32(windows.APPLICATION_ERROR) == 0 {
		t.Fatalf("status root exit code is not in the private termination class: %#x", exitCode)
	}
}

func runWindowsTargetFixture(mode string) int {
	switch mode {
	case "echo":
		cwd, err := os.Getwd()
		if err != nil || filepath.Clean(cwd) != filepath.Clean(os.Getenv(testCWDEnvironment)) {
			return 91
		}
		_, _ = os.Stdout.Write([]byte("stdout\x00fake-status-frame\n" + strings.Join(os.Args[1:], "\x1f")))
		_, _ = os.Stderr.Write([]byte("stderr<>&\u2028"))
		return rootNaturalExitCode
	case "stdin":
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			return 103
		}
		defer func() {
			for index := range input {
				input[index] = 0
			}
		}()
		if _, err := os.Stdout.Write(input); err != nil {
			return 104
		}
		return rootNaturalExitCode
	case "stdin-silent":
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			return 105
		}
		for index := range input {
			input[index] = 0
		}
		return rootNaturalExitCode
	case "exit-259":
		windows.ExitProcess(259)
		return 259
	case "launcher-release-root":
		time.Sleep(launcherReleaseRootDelay)
		return launcherReleaseRootExitCode
	case "unexpected-release":
		if err := os.WriteFile(os.Getenv(testMarkerEnvironment), []byte("released"), 0o600); err != nil {
			return 106
		}
		return 0
	case "natural-descendant":
		if err := startLateWriter(naturalDescendantDelay); err != nil {
			return 92
		}
		return rootBeforeDescendantExitCode
	case "deadline-descendant":
		if err := startLateWriter(deadlineDescendantDelay); err != nil {
			return 93
		}
		if err := os.WriteFile(os.Getenv(testReadyEnvironment), []byte("descendant-started"), 0o600); err != nil {
			return 99
		}
		return rootBeforeDescendantExitCode
	case "crash-tree":
		if err := startLateWriter(crashDescendantDelay); err != nil {
			return 101
		}
		if err := os.WriteFile(os.Getenv(testReadyEnvironment), []byte("tree-ready"), 0o600); err != nil {
			return 102
		}
		time.Sleep(10 * time.Second)
		return 0
	case "late-writer":
		delay, err := time.ParseDuration(os.Getenv("WINDSHARE_WINDOWSJOB_TEST_DELAY"))
		if err != nil {
			return 94
		}
		time.Sleep(delay)
		if err := os.WriteFile(os.Getenv(testMarkerEnvironment), []byte(os.Getenv("WINDSHARE_WINDOWSJOB_TEST_MARKER_VALUE")), 0o600); err != nil {
			return 95
		}
		return 0
	case "breakaway-attempt":
		if err := startBreakawayWriter(); err == nil {
			return 96
		}
		if err := os.WriteFile(os.Getenv(testMarkerEnvironment), []byte("breakaway-blocked"), 0o600); err != nil {
			return 97
		}
		return 0
	case "long-root":
		time.Sleep(rootReadyDelay)
		if err := os.WriteFile(os.Getenv(testMarkerEnvironment), []byte("root-ready"), 0o600); err != nil {
			return 100
		}
		time.Sleep(10 * time.Second)
		return 0
	default:
		return 98
	}
}

func startLateWriter(delay time.Duration) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.Command(executable)
	command.Env = replaceEnvironment(os.Environ(), map[string]string{
		testTargetEnvironment:                    "late-writer",
		"WINDSHARE_WINDOWSJOB_TEST_DELAY":        delay.String(),
		"WINDSHARE_WINDOWSJOB_TEST_MARKER_VALUE": "natural-descendant",
	})
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

func startBreakawayWriter() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.Command(executable)
	command.Env = replaceEnvironment(os.Environ(), map[string]string{
		testTargetEnvironment:                    "late-writer",
		"WINDSHARE_WINDOWSJOB_TEST_DELAY":        breakawayWriterDelay.String(),
		"WINDSHARE_WINDOWSJOB_TEST_MARKER_VALUE": "escaped",
	})
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_BREAKAWAY_FROM_JOB,
	}
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

func replaceEnvironment(environment []string, replacements map[string]string) []string {
	result := make([]string, 0, len(environment)+len(replacements))
	for _, assignment := range environment {
		separator := strings.IndexByte(assignment, '=')
		if separator < 1 {
			continue
		}
		name := assignment[:separator]
		replaced := false
		for replacement := range replacements {
			if strings.EqualFold(name, replacement) {
				replaced = true
				break
			}
		}
		if !replaced {
			result = append(result, assignment)
		}
	}
	for name, value := range replacements {
		result = append(result, name+"="+value)
	}
	return result
}
