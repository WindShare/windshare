//go:build windows

package windowsjob

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
	"golang.org/x/sys/windows"
)

func TestAuthorityFailureTerminationProvesEmptyJob(t *testing.T) {
	job, err := createManagedJob()
	if err != nil {
		t.Fatal(err)
	}
	defer job.close()
	request := windowsIntegrationRequest(t, "echo", 5_000)
	job.terminationCodes = mustTerminationExitCodes(t, request.Identity)
	cause := errors.New("control authority lost")
	got := terminateAfterAuthorityFailure(job, request, cause)
	if !errors.Is(got, cause) {
		t.Fatalf("authority failure = %v, want %v", got, cause)
	}
	var evidence *authorityTerminationError
	if !errors.As(got, &evidence) || !evidence.treeProvenEmpty() {
		t.Fatalf("authority termination discarded empty-tree proof: %#v", evidence)
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

func TestStartDecisionBoundaryPrioritizesPreReleaseTermination(t *testing.T) {
	accepted := ownerprotocol.StartDecision{Outcome: ownerprotocol.StartDecisionAccepted}
	for range 100 {
		decisions := make(chan startDecisionResult, 1)
		controls := make(chan controlResult, 1)
		decisions <- startDecisionResult{decision: accepted}
		controls <- controlResult{reason: ownerprotocol.TerminationStop}

		decision, trigger, err := awaitStartDecisionBeforeRelease(
			decisions,
			controls,
			make(chan time.Time),
			lifecycleTrigger{},
			time.Second,
		)
		if err != nil {
			t.Fatal(err)
		}
		if decision.Outcome != ownerprotocol.StartDecisionAccepted ||
			trigger.kind != lifecycleTriggerControl || trigger.control.reason != ownerprotocol.TerminationStop {
			t.Fatalf("simultaneous decision/control = decision %#v, trigger %#v", decision, trigger)
		}
	}
}

func TestStartDecisionBoundaryConsumesDecisionAfterPreReleaseStop(t *testing.T) {
	decisions := make(chan startDecisionResult)
	controls := make(chan controlResult, 1)
	controls <- controlResult{reason: ownerprotocol.TerminationStop}
	result := make(chan struct {
		decision ownerprotocol.StartDecision
		trigger  lifecycleTrigger
		err      error
	}, 1)
	go func() {
		decision, trigger, err := awaitStartDecisionBeforeRelease(
			decisions,
			controls,
			make(chan time.Time),
			lifecycleTrigger{},
			time.Second,
		)
		result <- struct {
			decision ownerprotocol.StartDecision
			trigger  lifecycleTrigger
			err      error
		}{decision: decision, trigger: trigger, err: err}
	}()

	select {
	case early := <-result:
		t.Fatalf("pre-release stop bypassed decision authentication: %#v", early)
	default:
	}
	decisions <- startDecisionResult{decision: ownerprotocol.StartDecision{Outcome: ownerprotocol.StartDecisionAccepted}}
	got := <-result
	if got.err != nil || got.decision.Outcome != ownerprotocol.StartDecisionAccepted ||
		got.trigger.kind != lifecycleTriggerControl || got.trigger.control.reason != ownerprotocol.TerminationStop {
		t.Fatalf("decision after stop = %#v", got)
	}
}

func TestStartDecisionBoundaryPreservesLegitimateRejection(t *testing.T) {
	rejected := ownerprotocol.StartDecision{
		Outcome: ownerprotocol.StartDecisionRejected, FailureCode: "AUTHORITY_REJECTED", FailureMessage: "identity mismatch",
	}
	decisions := make(chan startDecisionResult, 1)
	decisions <- startDecisionResult{decision: rejected}
	decision, trigger, err := awaitStartDecisionBeforeRelease(
		decisions,
		make(chan controlResult),
		make(chan time.Time),
		lifecycleTrigger{},
		time.Second,
	)
	if err != nil || decision != rejected || trigger.kind != lifecycleTriggerNone {
		t.Fatalf("rejected decision = %#v, trigger %#v, error %v", decision, trigger, err)
	}
}

func TestStartDecisionBoundaryRejectsUnauthenticatedDecision(t *testing.T) {
	cause := errors.New("decision does not bind exact start evidence")
	decisions := make(chan startDecisionResult, 1)
	decisions <- startDecisionResult{err: cause}
	_, _, err := awaitStartDecisionBeforeRelease(
		decisions,
		make(chan controlResult),
		make(chan time.Time),
		lifecycleTrigger{},
		time.Second,
	)
	if !errors.Is(err, cause) {
		t.Fatalf("decision authentication error = %v, want %v", err, cause)
	}
}

func TestManagedJobSnapshotPrimitivesRemainBoundedAndExact(t *testing.T) {
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
	membershipExpiration := make(chan struct{})
	membershipDeadlineTimer := time.AfterFunc(time.Millisecond, func() { close(membershipExpiration) })
	membershipDeadline := &launcherHandoffDeadline{
		expiration: membershipExpiration,
		stopTimer:  membershipDeadlineTimer.Stop,
	}
	defer membershipDeadline.close()
	if err := waitForProcessMembershipRelease(job, root.pid, 1, membershipDeadline); err == nil || !strings.Contains(err.Error(), "remained") {
		t.Fatalf("active membership fence error = %v", err)
	}
	codes := mustTerminationExitCodes(t, testIdentity)
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
	request.TerminationGraceMilliseconds = 1
	job.terminationCodes = mustTerminationExitCodes(t, request.Identity)
	launcher := &assignedLauncher{wait: make(chan error)}
	handoffDeadline := newLauncherHandoffDeadline(request)
	defer handoffDeadline.close()
	if err := finishTrustedLauncherHandoff(job, request, launcher, handoffDeadline); err == nil || !strings.Contains(err.Error(), "did not complete") {
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
	job.terminationCodes = mustTerminationExitCodes(t, request.Identity)
	deadline := time.NewTimer(time.Hour)
	defer deadline.Stop()
	cause := errors.New("invalid parent authority")
	controls := make(chan controlResult, 1)
	controls <- controlResult{err: cause}
	launcher := &assignedLauncher{wait: make(chan error)}
	handoffDeadline := newLauncherHandoffDeadline(request)
	defer handoffDeadline.close()
	if _, err := awaitTrustedLauncherHandoff(
		job,
		request,
		deadline,
		controls,
		launcher,
		handoffDeadline,
		lifecycleTrigger{},
	); !errors.Is(err, cause) {
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
	request.TerminationGraceMilliseconds = 1
	job.terminationCodes = mustTerminationExitCodes(t, request.Identity)
	deadline := time.NewTimer(time.Nanosecond)
	defer deadline.Stop()
	time.Sleep(time.Millisecond)
	launcher := &assignedLauncher{wait: make(chan error)}
	handoffDeadline := newLauncherHandoffDeadline(request)
	defer handoffDeadline.close()
	if _, err := awaitTrustedLauncherHandoff(
		job,
		request,
		deadline,
		nil,
		launcher,
		handoffDeadline,
		lifecycleTrigger{},
	); err == nil || !strings.Contains(err.Error(), "did not complete") {
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
	job.terminationCodes = mustTerminationExitCodes(t, request.Identity)
	cause := errors.New("launcher crashed")
	wait := make(chan error, 1)
	wait <- cause
	handoffDeadline := newLauncherHandoffDeadline(request)
	defer handoffDeadline.close()
	if err := finishTrustedLauncherHandoff(job, request, &assignedLauncher{wait: wait}, handoffDeadline); !errors.Is(err, cause) {
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
	job.terminationCodes = mustTerminationExitCodes(t, request.Identity)
	settlements, output := newTestSettlementSink(t, request)
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
	if err := superviseRootTree(
		job,
		fixedRootAuthority(1),
		request,
		settlements,
		deadline,
		nil,
		rootExit,
		nil,
		lifecycleTrigger{},
	); err != nil {
		t.Fatalf("supervise empty job: %v", err)
	}
	result := windowsSupervisorResult{status: decodeTestSettlement(t, output)}
	assertTreeStatus(t, result.status, request, ownerprotocol.TerminationNatural, false, rootNaturalExitCode)
}

func TestThirtyMillisecondDeadlineRechecksNaturallyEmptyTree(t *testing.T) {
	const regressionRuns = 32
	for run := range regressionRuns {
		job := newStaleActiveJob(mustTerminationExitCodes(t, testIdentity))
		request := windowsIntegrationRequest(t, "echo", 5_000)
		settlements, output := newTestSettlementSink(t, request)
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
			settlements,
			deadline,
			nil,
			rootExit,
			time.Second,
			nil,
			lifecycleTrigger{},
		)
		deadline.Stop()
		if err != nil {
			t.Fatalf("run %d: supervise naturally empty tree: %v", run, err)
		}
		if len(job.terminationCodes) != 0 {
			t.Fatalf("run %d: naturally empty tree received termination codes %#v", run, job.terminationCodes)
		}
		result := windowsSupervisorResult{status: decodeTestSettlement(t, output)}
		assertTreeStatus(t, result.status, request, ownerprotocol.TerminationNatural, false, 0)
	}
}

func TestFatalControlRechecksNaturallyEmptyTreeBeforeClaimingCausality(t *testing.T) {
	codes := mustTerminationExitCodes(t, testIdentity)
	job := newStaleActiveJob(codes)
	request := windowsIntegrationRequest(t, "echo", 5_000)
	settlements, output := newTestSettlementSink(t, request)
	deadline := time.NewTimer(time.Hour)
	defer deadline.Stop()
	controls := make(chan controlResult, 1)
	controls <- controlResult{err: errors.New("control framing failed")}
	rootExit := make(chan rootExitResult, 1)
	go func() {
		<-job.deadlineRechecked
		rootExit <- rootExitResult{status: rootStatus{PID: 1, ExitCode: 0}}
	}()

	if err := superviseRootTreeWithPollInterval(
		job,
		fixedRootAuthority(1),
		request,
		settlements,
		deadline,
		controls,
		rootExit,
		time.Second,
		nil,
		lifecycleTrigger{},
	); err != nil {
		t.Fatalf("supervise naturally empty fatal-control race: %v", err)
	}
	if len(job.terminationCodes) != 0 {
		t.Fatalf("naturally empty tree received authority termination codes %#v", job.terminationCodes)
	}
	status := decodeTestSettlement(t, output)
	assertTreeStatus(t, status, request, ownerprotocol.TerminationNatural, false, 0)
}

func TestFatalControlRequiresMatchingPrivateExitEvidence(t *testing.T) {
	codes := mustTerminationExitCodes(t, testIdentity)
	job := newRootExitRaceJob(codes)
	job.memberExitCode = codes.authority
	request := windowsIntegrationRequest(t, "echo", 5_000)
	settlements, output := newTestSettlementSink(t, request)
	deadline := time.NewTimer(time.Hour)
	defer deadline.Stop()
	cause := errors.New("control framing failed")
	controls := make(chan controlResult, 1)
	controls <- controlResult{err: cause}
	rootExit := make(chan rootExitResult, 1)
	go func() {
		<-job.terminationRequested
		rootExit <- rootExitResult{status: rootStatus{PID: 1, ExitCode: codes.authority}}
	}()

	if err := superviseRootTreeWithPollInterval(
		job,
		fixedRootAuthority(1),
		request,
		settlements,
		deadline,
		controls,
		rootExit,
		time.Second,
		nil,
		lifecycleTrigger{},
	); err != nil {
		t.Fatalf("supervise causal fatal control: %v", err)
	}
	status := decodeTestSettlement(t, output)
	assertTreeStatus(t, status, request, ownerprotocol.TerminationOwnerFailure, false, codes.authority)
	if status.OwnerFailure == nil || !strings.Contains(status.OwnerFailure.Message, cause.Error()) {
		t.Fatalf("owner failure evidence = %#v", status.OwnerFailure)
	}
}

func TestThirtyMillisecondRootExitRaceNeverAuthenticatesTimeout(t *testing.T) {
	const regressionRuns = 100
	for run := range regressionRuns {
		codes := mustTerminationExitCodes(t, testIdentity)
		job := newRootExitRaceJob(codes)
		request := windowsIntegrationRequest(t, "echo", 5_000)
		settlements, output := newTestSettlementSink(t, request)
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
			settlements,
			deadline,
			nil,
			rootExit,
			time.Second,
			nil,
			lifecycleTrigger{},
		)
		deadline.Stop()
		if err != nil {
			t.Fatalf("run %d: reconcile root exit race: %v", run, err)
		}
		if len(job.terminationCodes) != 1 || job.terminationCodes[0] != codes.deadline {
			t.Fatalf("run %d: termination requests = %#v", run, job.terminationCodes)
		}
		result := windowsSupervisorResult{status: decodeTestSettlement(t, output)}
		assertTreeStatus(t, result.status, request, ownerprotocol.TerminationNatural, false, 0)
	}
}

func TestReconcileTerminationRejectsConcurrentProcessGeneration(t *testing.T) {
	codes := mustTerminationExitCodes(t, testIdentity)
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
		reason:   ownerprotocol.TerminationDeadline,
		timedOut: true,
	}
	defer intervention.snapshot.close()

	_, _, err := reconcileTerminationIntervention(job, intervention, time.Second)
	if err == nil || !strings.Contains(err.Error(), "concurrent target process creation") {
		t.Fatalf("generation mismatch error = %v", err)
	}
}

func TestReconcileTerminationAuthenticatesMatchingPrivateExitCode(t *testing.T) {
	codes := mustTerminationExitCodes(t, testIdentity)
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
		reason:   ownerprotocol.TerminationDeadline,
		timedOut: true,
	}
	defer intervention.snapshot.close()

	reason, timedOut, err := reconcileTerminationIntervention(job, intervention, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if reason != ownerprotocol.TerminationDeadline || !timedOut {
		t.Fatalf("causal intervention = (%q, %t)", reason, timedOut)
	}
}

func TestReconcileTerminationRejectsUnexpectedPrivateExitCode(t *testing.T) {
	codes := mustTerminationExitCodes(t, testIdentity)
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
		reason:   ownerprotocol.TerminationDeadline,
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
	memberExitCode       uint32
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
			&fixedProcessExitAuthority{pid: root.processID(), exitCode: job.memberExitCode},
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
