//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/windshare/windshare/internal/testnetwork"
)

func TestTerminatePendingLaunchCanAuthenticateSpawnFailure(t *testing.T) {
	job, err := createManagedJob()
	if err != nil {
		t.Fatal(err)
	}
	defer job.close()
	request := windowsIntegrationRequest(t, "echo", 5_000)
	job.terminationCodes = mustTerminationExitCodes(t, request.Nonce)
	failure := "target unavailable"
	events := make(chan launcherEventResult, 1)
	events <- launcherEventResult{event: launcherEvent{
		SchemaVersion: protocolSchemaVersion,
		Type:          launcherEventSpawnFailed,
		SpawnFailure:  &failure,
	}}
	launcherWait := make(chan error, 1)
	launcherWait <- nil
	statusPath := filepath.Join(t.TempDir(), "status.json")
	if err := terminatePendingLaunch(
		job,
		request,
		statusPath,
		terminateReasonParentRequest,
		false,
		events,
		launcherWait,
	); err != nil {
		t.Fatalf("terminate pending spawn failure: %v", err)
	}
	result := decodeWindowsSupervisorResult(t, statusPath, windowsSupervisorResult{})
	if result.status.SupervisionOutcome != statusOutcomeSpawnFailed || result.status.SpawnFailure == nil || *result.status.SpawnFailure != failure {
		t.Fatalf("spawn-failed status = %#v", result.status)
	}
}

func TestTerminatePendingLaunchRejectsIncompleteRootTransfer(t *testing.T) {
	job, err := createManagedJob()
	if err != nil {
		t.Fatal(err)
	}
	defer job.close()
	request := windowsIntegrationRequest(t, "echo", 5_000)
	job.terminationCodes = mustTerminationExitCodes(t, request.Nonce)
	events := make(chan launcherEventResult, 1)
	events <- launcherEventResult{event: launcherEvent{
		SchemaVersion: protocolSchemaVersion,
		Type:          launcherEventRootStarted,
		PID:           1,
		ProcessHandle: 1,
	}}
	launcherWait := make(chan error, 1)
	launcherWait <- nil
	statusPath := filepath.Join(t.TempDir(), "status.json")
	err = terminatePendingLaunch(
		job,
		request,
		statusPath,
		terminationReasonDeadline,
		true,
		events,
		launcherWait,
	)
	if err == nil || !strings.Contains(err.Error(), "root handle transfer did not complete") {
		t.Fatalf("incomplete transfer error = %v", err)
	}
	if _, statErr := os.Stat(statusPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("incomplete transfer published authority status: %v", statErr)
	}
}

func TestAuthorityFailureTerminationProvesEmptyJob(t *testing.T) {
	job, err := createManagedJob()
	if err != nil {
		t.Fatal(err)
	}
	defer job.close()
	request := windowsIntegrationRequest(t, "echo", 5_000)
	job.terminationCodes = mustTerminationExitCodes(t, request.Nonce)
	cause := errors.New("control authority lost")
	if got := terminateAfterAuthorityFailure(job, request, cause); !errors.Is(got, cause) {
		t.Fatalf("authority failure = %v, want %v", got, cause)
	}
	if err := waitForJobEmpty(job, time.Second); err != nil {
		t.Fatalf("empty proof: %v", err)
	}
	if duration := positiveDurationUntil(time.Now().Add(-time.Second)); duration <= 0 {
		t.Fatalf("expired duration = %v", duration)
	}
	if duration := positiveDurationUntil(time.Now().Add(time.Second)); duration <= 0 || duration > time.Second {
		t.Fatalf("future duration = %v", duration)
	}
}

func TestManagedJobSnapshotPrimitivesRemainBoundedAndExact(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	job, err := createManagedJob()
	if err != nil {
		t.Fatal(err)
	}
	defer job.close()
	if _, err := job.activeProcessIDs(0); err == nil {
		t.Fatal("zero process-evidence limit was accepted")
	}
	processIDs, err := job.activeProcessIDs(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(processIDs) != 0 {
		t.Fatalf("fresh Job process IDs = %v", processIDs)
	}
	snapshot, err := job.captureTerminationSnapshot(fixedRootAuthority(1), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.close()
	if len(snapshot.members) != 0 || snapshot.totalProcessesBefore != 0 {
		t.Fatalf("fresh Job snapshot = %#v", snapshot)
	}
	cause := errors.New("process disappeared")
	retry, err := job.classifyBenignSnapshotLoss(
		42,
		map[uint32]struct{}{42: {}},
		1,
		0,
		cause,
	)
	if err != nil || !retry {
		t.Fatalf("empty Job retry = %t, error = %v", retry, err)
	}

	authority, err := openProcessExitAuthority(uint32(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	defer authority.close()
	if authority.processID() != uint32(os.Getpid()) {
		t.Fatalf("retained current PID = %d", authority.processID())
	}
	if err := authority.verifyJobMembership(job.handle); err == nil {
		t.Fatal("current test process was reported as a member of a fresh Job")
	}
	if _, err := authority.exactExitCode(time.Millisecond); err == nil {
		t.Fatal("running test process exposed a terminal exit code")
	}
	if _, err := openProcessExitAuthority(0); err == nil {
		t.Fatal("PID zero produced process-exit authority")
	}
	if _, err := (managedRoot{handle: 0, pid: 1}).retainExitAuthority(); err == nil {
		t.Fatal("invalid root handle produced process-exit authority")
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable)
	command.Env = replaceEnvironment(os.Environ(), map[string]string{
		testTargetEnvironment: "long-root",
		testMarkerEnvironment: filepath.Join(t.TempDir(), "snapshot-ready"),
	})
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	processWaited := false
	defer func() {
		if !processWaited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	var rootHandle windows.Handle
	var assignmentErr error
	var duplicateErr error
	if err := command.Process.WithHandle(func(handle uintptr) {
		assignmentErr = windows.AssignProcessToJobObject(job.handle, windows.Handle(handle))
		if assignmentErr == nil {
			rootHandle, duplicateErr = duplicateLocalProcessHandle(windows.Handle(handle))
		}
	}); err != nil {
		t.Fatal(err)
	}
	if assignmentErr != nil || duplicateErr != nil {
		t.Fatalf("prepare managed snapshot root: assign=%v duplicate=%v", assignmentErr, duplicateErr)
	}
	defer windows.CloseHandle(rootHandle)
	root := managedRoot{handle: rootHandle, pid: uint32(command.Process.Pid)}
	activeSnapshot, err := job.captureTerminationSnapshot(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer activeSnapshot.close()
	if len(activeSnapshot.members) != 1 {
		t.Fatalf("active Job snapshot members = %d", len(activeSnapshot.members))
	}
	if err := waitForProcessMembershipRelease(job, root.pid, 1, time.Millisecond); err == nil || !strings.Contains(err.Error(), "remained") {
		t.Fatalf("active membership fence error = %v", err)
	}
	codes := mustTerminationExitCodes(t, testNonce)
	job.terminationCodes = codes
	if err := job.terminate(codes.deadline); err != nil {
		t.Fatal(err)
	}
	if err := waitForJobEmpty(job, time.Second); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	processWaited = true
	exitCode, err := activeSnapshot.members[0].exactExitCode(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if exitCode != codes.deadline {
		t.Fatalf("retained target exit code = %#x, want %#x", exitCode, codes.deadline)
	}
}

func TestTrustedLauncherHandoffTimeoutFailsClosed(t *testing.T) {
	job, err := createManagedJob()
	if err != nil {
		t.Fatal(err)
	}
	defer job.close()
	request := windowsIntegrationRequest(t, "echo", 5_000)
	request.TerminationGraceMS = 1
	job.terminationCodes = mustTerminationExitCodes(t, request.Nonce)
	launcher := &assignedLauncher{wait: make(chan error)}
	if err := finishTrustedLauncherHandoff(job, request, launcher); err == nil || !strings.Contains(err.Error(), "did not complete") {
		t.Fatalf("handoff timeout error = %v", err)
	}
	if err := waitForJobEmpty(job, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestTrustedLauncherHandoffRejectsInvalidControlBeforeReap(t *testing.T) {
	job, err := createManagedJob()
	if err != nil {
		t.Fatal(err)
	}
	defer job.close()
	request := windowsIntegrationRequest(t, "echo", 5_000)
	job.terminationCodes = mustTerminationExitCodes(t, request.Nonce)
	deadline := time.NewTimer(time.Hour)
	defer deadline.Stop()
	cause := errors.New("invalid parent authority")
	controls := make(chan controlResult, 1)
	controls <- controlResult{err: cause}
	launcher := &assignedLauncher{wait: make(chan error)}
	if _, err := awaitTrustedLauncherHandoff(job, request, deadline, controls, launcher); !errors.Is(err, cause) {
		t.Fatalf("invalid control error = %v, want %v", err, cause)
	}
}

func TestTrustedLauncherHandoffReplaysDeadlineOnlyAfterReapFence(t *testing.T) {
	job, err := createManagedJob()
	if err != nil {
		t.Fatal(err)
	}
	defer job.close()
	request := windowsIntegrationRequest(t, "echo", 5_000)
	request.TerminationGraceMS = 1
	job.terminationCodes = mustTerminationExitCodes(t, request.Nonce)
	deadline := time.NewTimer(time.Nanosecond)
	defer deadline.Stop()
	time.Sleep(time.Millisecond)
	launcher := &assignedLauncher{wait: make(chan error)}
	if _, err := awaitTrustedLauncherHandoff(job, request, deadline, nil, launcher); err == nil || !strings.Contains(err.Error(), "did not complete") {
		t.Fatalf("deadline handoff error = %v", err)
	}
}

func TestTrustedLauncherHandoffRejectsLauncherFailure(t *testing.T) {
	job, err := createManagedJob()
	if err != nil {
		t.Fatal(err)
	}
	defer job.close()
	request := windowsIntegrationRequest(t, "echo", 5_000)
	job.terminationCodes = mustTerminationExitCodes(t, request.Nonce)
	cause := errors.New("launcher crashed")
	wait := make(chan error, 1)
	wait <- cause
	if err := finishTrustedLauncherHandoff(job, request, &assignedLauncher{wait: wait}); !errors.Is(err, cause) {
		t.Fatalf("launcher failure = %v, want %v", err, cause)
	}
}

func TestEmptyJobWinsAnExpiredDeadlineWithoutIntervention(t *testing.T) {
	job, err := createManagedJob()
	if err != nil {
		t.Fatal(err)
	}
	defer job.close()
	request := windowsIntegrationRequest(t, "echo", 5_000)
	job.terminationCodes = mustTerminationExitCodes(t, request.Nonce)
	statusPath := filepath.Join(t.TempDir(), "status.json")
	deadline := time.NewTimer(time.Nanosecond)
	defer deadline.Stop()
	// Make the deadline ready before the delayed launcher reap. The authoritative
	// empty-job observation must still win because no termination remains to cause.
	time.Sleep(time.Millisecond)
	rootExit := make(chan rootExitResult, 1)
	go func() {
		time.Sleep(2 * jobPollInterval)
		rootExit <- rootExitResult{status: rootStatus{PID: 1, ExitCode: rootNaturalExitCode}}
	}()
	if err := superviseRootTree(job, fixedRootAuthority(1), request, statusPath, deadline, nil, rootExit); err != nil {
		t.Fatalf("supervise empty job: %v", err)
	}
	result := decodeWindowsSupervisorResult(t, statusPath, windowsSupervisorResult{})
	assertTreeStatus(t, result.status, request, terminationReasonNatural, false, rootNaturalExitCode)
}

func TestThirtyMillisecondDeadlineRechecksNaturallyEmptyTree(t *testing.T) {
	const regressionRuns = 32
	for run := 0; run < regressionRuns; run++ {
		job := newStaleActiveJob(mustTerminationExitCodes(t, testNonce))
		request := windowsIntegrationRequest(t, "echo", 5_000)
		statusPath := filepath.Join(t.TempDir(), "status.json")
		deadline := time.NewTimer(30 * time.Millisecond)
		rootExit := make(chan rootExitResult, 1)
		go func() {
			<-job.deadlineRechecked
			rootExit <- rootExitResult{status: rootStatus{PID: 1, ExitCode: 0}}
		}()

		err := superviseRootTreeWithPollInterval(
			job,
			fixedRootAuthority(1),
			request,
			statusPath,
			deadline,
			nil,
			rootExit,
			time.Second,
		)
		deadline.Stop()
		if err != nil {
			t.Fatalf("run %d: supervise naturally empty tree: %v", run, err)
		}
		if len(job.terminationCodes) != 0 {
			t.Fatalf("run %d: naturally empty tree received termination codes %#v", run, job.terminationCodes)
		}
		result := decodeWindowsSupervisorResult(t, statusPath, windowsSupervisorResult{})
		assertTreeStatus(t, result.status, request, terminationReasonNatural, false, 0)
	}
}

func TestThirtyMillisecondRootExitRaceNeverAuthenticatesTimeout(t *testing.T) {
	const regressionRuns = 100
	for run := 0; run < regressionRuns; run++ {
		codes := mustTerminationExitCodes(t, testNonce)
		job := newRootExitRaceJob(codes)
		request := windowsIntegrationRequest(t, "echo", 5_000)
		statusPath := filepath.Join(t.TempDir(), "status.json")
		deadline := time.NewTimer(30 * time.Millisecond)
		rootExit := make(chan rootExitResult, 1)
		go func() {
			<-job.terminationRequested
			rootExit <- rootExitResult{status: rootStatus{PID: 1, ExitCode: 0}}
		}()

		err := superviseRootTreeWithPollInterval(
			job,
			fixedRootAuthority(1),
			request,
			statusPath,
			deadline,
			nil,
			rootExit,
			time.Second,
		)
		deadline.Stop()
		if err != nil {
			t.Fatalf("run %d: reconcile root exit race: %v", run, err)
		}
		if len(job.terminationCodes) != 1 || job.terminationCodes[0] != codes.deadline {
			t.Fatalf("run %d: termination requests = %#v", run, job.terminationCodes)
		}
		result := decodeWindowsSupervisorResult(t, statusPath, windowsSupervisorResult{})
		assertTreeStatus(t, result.status, request, terminationReasonNatural, false, 0)
	}
}

func TestReconcileTerminationRejectsConcurrentProcessGeneration(t *testing.T) {
	codes := mustTerminationExitCodes(t, testNonce)
	job := newRootExitRaceJob(codes)
	job.terminated = true
	job.totalProcesses = 2
	intervention := terminationIntervention{
		exitCode: codes.deadline,
		snapshot: targetMemberSnapshot{
			totalProcessesBefore: 1,
			members: []processExitAuthority{
				&fixedProcessExitAuthority{pid: 1, exitCode: codes.deadline},
			},
		},
		reason:   terminationReasonDeadline,
		timedOut: true,
	}
	defer intervention.snapshot.close()

	_, _, err := reconcileTerminationIntervention(job, intervention, time.Second)
	if err == nil || !strings.Contains(err.Error(), "concurrent target process creation") {
		t.Fatalf("generation mismatch error = %v", err)
	}
}

func TestReconcileTerminationAuthenticatesMatchingPrivateExitCode(t *testing.T) {
	codes := mustTerminationExitCodes(t, testNonce)
	job := newRootExitRaceJob(codes)
	job.terminated = true
	job.totalProcesses = 3
	intervention := terminationIntervention{
		exitCode: codes.deadline,
		snapshot: targetMemberSnapshot{
			totalProcessesBefore: 3,
			members: []processExitAuthority{
				&fixedProcessExitAuthority{pid: 1, exitCode: 0},
				&fixedProcessExitAuthority{pid: 2, exitCode: codes.deadline},
			},
		},
		reason:   terminationReasonDeadline,
		timedOut: true,
	}
	defer intervention.snapshot.close()

	reason, timedOut, err := reconcileTerminationIntervention(job, intervention, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if reason != terminationReasonDeadline || !timedOut {
		t.Fatalf("causal intervention = (%q, %t)", reason, timedOut)
	}
}

func TestReconcileTerminationRejectsUnexpectedPrivateExitCode(t *testing.T) {
	codes := mustTerminationExitCodes(t, testNonce)
	job := newRootExitRaceJob(codes)
	job.terminated = true
	intervention := terminationIntervention{
		exitCode: codes.deadline,
		snapshot: targetMemberSnapshot{
			totalProcessesBefore: 1,
			members: []processExitAuthority{
				&fixedProcessExitAuthority{pid: 1, exitCode: codes.parent},
			},
		},
		reason:   terminationReasonDeadline,
		timedOut: true,
	}
	defer intervention.snapshot.close()

	_, _, err := reconcileTerminationIntervention(job, intervention, time.Second)
	if err == nil || !strings.Contains(err.Error(), "unexpected private termination code") {
		t.Fatalf("unexpected private-code error = %v", err)
	}
}

func TestSnapshotLossRetryRequiresPureNaturalExitChurn(t *testing.T) {
	cause := errors.New("retain failed")
	initialMembers := map[uint32]struct{}{1: {}, 2: {}}
	tests := []struct {
		name         string
		currentTotal uint32
		currentIDs   []uint32
		wantRetry    bool
		wantError    string
	}{
		{name: "lost member removed", currentTotal: 3, currentIDs: []uint32{1}, wantRetry: true},
		{name: "lost member remains", currentTotal: 3, currentIDs: []uint32{1, 2}, wantError: cause.Error()},
		{name: "process generation changed", currentTotal: 4, currentIDs: []uint32{1}, wantError: "process generation changed"},
		{name: "new member appeared", currentTotal: 3, currentIDs: []uint32{1, 3}, wantError: "process membership changed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			retry, err := classifyBenignSnapshotLossEvidence(
				2,
				initialMembers,
				3,
				test.currentTotal,
				test.currentIDs,
				cause,
			)
			if retry != test.wantRetry {
				t.Fatalf("retry = %t, want %t", retry, test.wantRetry)
			}
			if test.wantError == "" && err != nil {
				t.Fatalf("error = %v", err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("error = %v, want text %q", err, test.wantError)
			}
		})
	}
}

type staleActiveJob struct {
	activeQueries     int
	deadlineRechecked chan struct{}
	terminationCodes  []uint32
	privateCodes      terminationExitCodes
}

func newStaleActiveJob(codes terminationExitCodes) *staleActiveJob {
	return &staleActiveJob{deadlineRechecked: make(chan struct{}), privateCodes: codes}
}

func (job *staleActiveJob) activeProcessCount() (uint32, error) {
	accounting, err := job.processAccounting()
	return accounting.active, err
}

func (job *staleActiveJob) processAccounting() (jobProcessAccounting, error) {
	job.activeQueries++
	if job.activeQueries == 1 {
		// The target exits naturally immediately after the loop's observation;
		// exact root evidence is intentionally delayed until the deadline branch
		// proves it refreshed Job liveness instead of trusting that stale count.
		return jobProcessAccounting{total: 1, active: 1}, nil
	}
	if job.activeQueries == 2 {
		close(job.deadlineRechecked)
	}
	return jobProcessAccounting{total: 1, active: 0}, nil
}

func (job *staleActiveJob) captureTerminationSnapshot(
	rootLifecycleAuthority,
	int,
) (targetMemberSnapshot, error) {
	accounting, err := job.processAccounting()
	return targetMemberSnapshot{totalProcessesBefore: accounting.total}, err
}

func (job *staleActiveJob) terminate(exitCode uint32) error {
	job.terminationCodes = append(job.terminationCodes, exitCode)
	return nil
}

func (job *staleActiveJob) exitCodes() terminationExitCodes {
	return job.privateCodes
}

type fixedRootAuthority uint32

func (authority fixedRootAuthority) processID() uint32 {
	return uint32(authority)
}

func (fixedRootAuthority) retainExitAuthority() (processExitAuthority, error) {
	return nil, errors.New("fixed root authority does not retain native process handles")
}

type rootExitRaceJob struct {
	terminated           bool
	totalProcesses       uint32
	terminationRequested chan struct{}
	terminationCodes     []uint32
	privateCodes         terminationExitCodes
}

func newRootExitRaceJob(codes terminationExitCodes) *rootExitRaceJob {
	return &rootExitRaceJob{totalProcesses: 1, terminationRequested: make(chan struct{}), privateCodes: codes}
}

func (job *rootExitRaceJob) activeProcessCount() (uint32, error) {
	accounting, err := job.processAccounting()
	return accounting.active, err
}

func (job *rootExitRaceJob) processAccounting() (jobProcessAccounting, error) {
	if job.terminated {
		return jobProcessAccounting{total: job.totalProcesses, active: 0}, nil
	}
	return jobProcessAccounting{total: job.totalProcesses, active: 1}, nil
}

func (job *rootExitRaceJob) captureTerminationSnapshot(
	root rootLifecycleAuthority,
	_ int,
) (targetMemberSnapshot, error) {
	accounting, err := job.processAccounting()
	if err != nil {
		return targetMemberSnapshot{}, err
	}
	return targetMemberSnapshot{
		totalProcessesBefore: accounting.total,
		members: []processExitAuthority{
			&fixedProcessExitAuthority{pid: root.processID(), exitCode: 0},
		},
	}, nil
}

func (job *rootExitRaceJob) terminate(exitCode uint32) error {
	job.terminationCodes = append(job.terminationCodes, exitCode)
	job.terminated = true
	close(job.terminationRequested)
	return nil
}

func (job *rootExitRaceJob) exitCodes() terminationExitCodes {
	return job.privateCodes
}

type fixedProcessExitAuthority struct {
	pid      uint32
	exitCode uint32
}

func (authority *fixedProcessExitAuthority) processID() uint32 {
	return authority.pid
}

func (*fixedProcessExitAuthority) verifyJobMembership(windows.Handle) error {
	return nil
}

func (authority *fixedProcessExitAuthority) exactExitCode(time.Duration) (uint32, error) {
	return authority.exitCode, nil
}

func (*fixedProcessExitAuthority) close() {}
