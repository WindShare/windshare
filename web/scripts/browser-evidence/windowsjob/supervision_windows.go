//go:build windows

package main

import (
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"slices"
	"time"
)

type rootExitResult struct {
	status rootStatus
	err    error
}

type launcherHandoffResult struct {
	control         *controlResult
	deadlineArrived bool
}

func superviseLaunchedTree(
	job managedJob,
	request startRequest,
	statusPath string,
	event launcherEvent,
	deadline *time.Timer,
	controls <-chan controlResult,
	launcher *assignedLauncher,
) (resultErr error) {
	if event.Type == launcherEventSpawnFailed {
		_ = launcher.input.Close()
		return superviseSpawnFailure(job, request, statusPath, event, deadline, controls, launcher.wait)
	}
	rootHandle, err := rootHandleFromEvent(job, event, launcher.process)
	if err != nil {
		return terminateAfterAuthorityFailure(job, request, err)
	}
	rootProbeHandle, err := duplicateLocalProcessHandle(rootHandle)
	if err != nil {
		_ = windows.CloseHandle(rootHandle)
		return terminateAfterAuthorityFailure(job, request, fmt.Errorf("retain root liveness probe: %w", err))
	}
	defer func() {
		closeErr := windows.CloseHandle(rootProbeHandle)
		if closeErr != nil {
			closeErr = fmt.Errorf("close root liveness probe: %w", closeErr)
		}
		// Supervision is authoritative; releasing its probe may add cleanup
		// evidence but must never replace the preceding lifecycle failure.
		resultErr = errors.Join(resultErr, closeErr)
	}()
	if err := writeAll(launcher.input, []byte{launcherRootAcknowledgement}); err != nil {
		_ = windows.CloseHandle(rootHandle)
		return terminateAfterAuthorityFailure(job, request, fmt.Errorf("acknowledge root handle ownership: %w", err))
	}
	if err := launcher.input.Close(); err != nil {
		_ = windows.CloseHandle(rootHandle)
		return terminateAfterAuthorityFailure(job, request, fmt.Errorf("close launcher acknowledgement channel: %w", err))
	}
	rootExit := make(chan rootExitResult, 1)
	go func() {
		status, waitErr := waitRootAndClose(rootHandle, event.PID)
		rootExit <- rootExitResult{status: status, err: waitErr}
	}()
	handoff, err := awaitTrustedLauncherHandoff(job, request, deadline, controls, launcher)
	if err != nil {
		return err
	}
	if handoff.control != nil {
		replayedControl := make(chan controlResult, 1)
		replayedControl <- *handoff.control
		controls = replayedControl
	}
	if handoff.deadlineArrived {
		immediateDeadline := time.NewTimer(0)
		defer immediateDeadline.Stop()
		deadline = immediateDeadline
	}
	return superviseRootTree(
		job,
		managedRoot{handle: rootProbeHandle, pid: event.PID},
		request,
		statusPath,
		deadline,
		controls,
		rootExit,
	)
}

func awaitTrustedLauncherHandoff(
	job managedJob,
	request startRequest,
	deadline *time.Timer,
	controls <-chan controlResult,
	launcher *assignedLauncher,
) (launcherHandoffResult, error) {
	// Prefer an already completed handoff over a simultaneously ready deadline
	// or control frame. Once Wait succeeds, Job accounting excludes launcher
	// infrastructure and those events can be judged against the target tree.
	select {
	case waitErr := <-launcher.wait:
		if err := completeTrustedLauncherHandoff(job, request, launcher, waitErr); err != nil {
			return launcherHandoffResult{}, err
		}
		return launcherHandoffResult{}, nil
	default:
	}
	select {
	case waitErr := <-launcher.wait:
		if err := completeTrustedLauncherHandoff(job, request, launcher, waitErr); err != nil {
			return launcherHandoffResult{}, err
		}
		return launcherHandoffResult{}, nil
	case control := <-controls:
		if control.err != nil {
			return launcherHandoffResult{}, terminateAfterAuthorityFailure(job, request, control.err)
		}
		if err := finishTrustedLauncherHandoff(job, request, launcher); err != nil {
			return launcherHandoffResult{}, err
		}
		return launcherHandoffResult{control: &control}, nil
	case <-deadline.C:
		if err := finishTrustedLauncherHandoff(job, request, launcher); err != nil {
			return launcherHandoffResult{}, err
		}
		return launcherHandoffResult{deadlineArrived: true}, nil
	}
}

func finishTrustedLauncherHandoff(
	job managedJob,
	request startRequest,
	launcher *assignedLauncher,
) error {
	waitLimit := time.NewTimer(time.Duration(request.TerminationGraceMS) * time.Millisecond)
	defer waitLimit.Stop()
	select {
	case waitErr := <-launcher.wait:
		return completeTrustedLauncherHandoff(job, request, launcher, waitErr)
	case <-waitLimit.C:
		return terminateAfterAuthorityFailure(job, request, errors.New("trusted launcher handoff did not complete within termination grace"))
	}
}

func completeTrustedLauncherHandoff(
	job managedJob,
	request startRequest,
	launcher *assignedLauncher,
	waitErr error,
) error {
	if waitErr != nil {
		return terminateAfterAuthorityFailure(job, request, fmt.Errorf("trusted launcher failed during root handoff: %w", waitErr))
	}
	launcherPID := launcher.process.Pid
	if launcherPID <= 0 || uint64(launcherPID) > maxWindowsProcessID {
		return terminateAfterAuthorityFailure(job, request, errors.New("trusted launcher PID is invalid"))
	}
	retainedPID, err := windows.GetProcessId(launcher.membershipHandle)
	if err != nil || retainedPID != uint32(launcherPID) {
		if err == nil {
			err = errors.New("retained trusted launcher identity changed")
		}
		return terminateAfterAuthorityFailure(job, request, fmt.Errorf("verify trusted launcher identity: %w", err))
	}
	if err := waitForProcessMembershipRelease(
		job,
		retainedPID,
		maxTerminationSnapshotProcesses,
		time.Duration(request.TerminationGraceMS)*time.Millisecond,
	); err != nil {
		return terminateAfterAuthorityFailure(job, request, fmt.Errorf("fence trusted launcher membership: %w", err))
	}
	return nil
}

func waitForProcessMembershipRelease(
	job managedJob,
	processID uint32,
	maximumProcesses int,
	maximumWait time.Duration,
) error {
	deadline := time.NewTimer(maximumWait)
	defer deadline.Stop()
	poll := time.NewTicker(jobPollInterval)
	defer poll.Stop()
	for {
		processIDs, err := job.activeProcessIDs(maximumProcesses)
		if err != nil {
			return err
		}
		found := slices.Contains(processIDs, processID)
		if !found {
			return nil
		}
		select {
		case <-deadline.C:
			return errors.New("trusted launcher remained in the Job process list beyond termination grace")
		case <-poll.C:
		}
	}
}

func superviseRootTree(
	job jobLifecycleAuthority,
	rootAuthority rootLifecycleAuthority,
	request startRequest,
	statusPath string,
	deadline *time.Timer,
	controls <-chan controlResult,
	rootExit <-chan rootExitResult,
) error {
	return superviseRootTreeWithPollInterval(
		job,
		rootAuthority,
		request,
		statusPath,
		deadline,
		controls,
		rootExit,
		jobPollInterval,
	)
}

func superviseRootTreeWithPollInterval(
	job jobLifecycleAuthority,
	rootAuthority rootLifecycleAuthority,
	request startRequest,
	statusPath string,
	deadline *time.Timer,
	controls <-chan controlResult,
	rootExit <-chan rootExitResult,
	pollInterval time.Duration,
) error {
	poll := time.NewTicker(pollInterval)
	defer poll.Stop()
	monitor := rootTreeMonitor{
		rootExit:          rootExit,
		controls:          controls,
		deadline:          deadline.C,
		terminationReason: terminationReasonNatural,
	}
	defer monitor.close()
	for {
		active, err := job.activeProcessCount()
		if err != nil {
			return err
		}
		if active == 0 {
			return monitor.settleEmptyTree(job, request, statusPath)
		}
		if err := monitor.awaitEvent(job, rootAuthority, request, poll.C); err != nil {
			return err
		}
	}
}

type rootTreeMonitor struct {
	root                *rootStatus
	rootExit            <-chan rootExitResult
	controls            <-chan controlResult
	deadline            <-chan time.Time
	terminationDeadline <-chan time.Time
	terminationLimit    time.Time
	terminationReason   string
	timedOut            bool
	terminating         bool
	fatalControl        error
	pendingIntervention *terminationIntervention
}

func (monitor *rootTreeMonitor) close() {
	if monitor.pendingIntervention != nil {
		monitor.pendingIntervention.snapshot.close()
	}
}

func (monitor *rootTreeMonitor) awaitEvent(
	job jobLifecycleAuthority,
	rootAuthority rootLifecycleAuthority,
	request startRequest,
	poll <-chan time.Time,
) error {
	select {
	case result := <-monitor.rootExit:
		return monitor.observeRootExit(job, request, result)
	case control := <-monitor.controls:
		return monitor.observeControl(job, rootAuthority, request, control)
	case <-monitor.deadline:
		return monitor.observeDeadline(job, rootAuthority, request)
	case <-monitor.terminationDeadline:
		return errors.New("the Job Object did not become empty within termination grace")
	case <-poll:
		return nil
	}
}

func (monitor *rootTreeMonitor) observeRootExit(
	job jobLifecycleAuthority,
	request startRequest,
	result rootExitResult,
) error {
	if result.err != nil {
		return terminateAfterAuthorityFailure(job, request, result.err)
	}
	monitor.root = &result.status
	monitor.rootExit = nil
	return nil
}

func (monitor *rootTreeMonitor) observeControl(
	job jobLifecycleAuthority,
	rootAuthority rootLifecycleAuthority,
	request startRequest,
	control controlResult,
) error {
	monitor.controls = nil
	if control.err != nil {
		monitor.fatalControl = control.err
		if monitor.terminating {
			return nil
		}
		if err := job.terminate(job.exitCodes().authority); err != nil {
			return err
		}
		monitor.beginTerminationGrace(request)
		return nil
	}
	if monitor.terminating {
		return nil
	}
	return monitor.intervene(
		job,
		rootAuthority,
		request,
		job.exitCodes().parent,
		terminateReasonParentRequest,
		false,
	)
}

func (monitor *rootTreeMonitor) observeDeadline(
	job jobLifecycleAuthority,
	rootAuthority rootLifecycleAuthority,
	request startRequest,
) error {
	if monitor.terminating {
		return nil
	}
	return monitor.intervene(
		job,
		rootAuthority,
		request,
		job.exitCodes().deadline,
		terminationReasonDeadline,
		true,
	)
}

func (monitor *rootTreeMonitor) intervene(
	job jobLifecycleAuthority,
	rootAuthority rootLifecycleAuthority,
	request startRequest,
	exitCode uint32,
	reason string,
	timedOut bool,
) error {
	intervention, err := terminateObservedNonemptyJob(job, rootAuthority, exitCode, reason, timedOut)
	if err != nil {
		return err
	}
	if !intervention.applied {
		return nil
	}
	monitor.pendingIntervention = &intervention
	monitor.beginTerminationGrace(request)
	return nil
}

func (monitor *rootTreeMonitor) beginTerminationGrace(request startRequest) {
	monitor.terminating = true
	monitor.terminationLimit = time.Now().Add(time.Duration(request.TerminationGraceMS) * time.Millisecond)
	monitor.terminationDeadline = time.After(positiveDurationUntil(monitor.terminationLimit))
}

func (monitor *rootTreeMonitor) settleEmptyTree(
	job jobLifecycleAuthority,
	request startRequest,
	statusPath string,
) error {
	if monitor.fatalControl != nil {
		return monitor.fatalControl
	}
	if err := monitor.awaitRootAfterEmpty(request); err != nil {
		return err
	}
	if err := monitor.reconcileIntervention(job); err != nil {
		return err
	}
	return publishStatusNew(statusPath, supervisorStatus{
		SchemaVersion:      protocolSchemaVersion,
		OperationID:        request.OperationID,
		Nonce:              request.Nonce,
		SupervisionOutcome: statusOutcomeTreeEmpty,
		TerminationReason:  monitor.terminationReason,
		TimedOut:           monitor.timedOut,
		ActiveProcessCount: 0,
		InputOutcome:       settledInputOutcome(request),
		Root:               monitor.root,
		SpawnFailure:       nil,
	})
}

func (monitor *rootTreeMonitor) awaitRootAfterEmpty(request startRequest) error {
	if monitor.root != nil {
		return nil
	}
	select {
	case result := <-monitor.rootExit:
		if result.err != nil {
			return result.err
		}
		monitor.root = &result.status
		monitor.rootExit = nil
		return nil
	case <-time.After(time.Duration(request.TerminationGraceMS) * time.Millisecond):
		return errors.New("the Job Object became empty before exact root status was available")
	}
}

func (monitor *rootTreeMonitor) reconcileIntervention(job jobLifecycleAuthority) error {
	if monitor.pendingIntervention == nil {
		return nil
	}
	reason, timedOut, err := reconcileTerminationIntervention(
		job,
		*monitor.pendingIntervention,
		positiveDurationUntil(monitor.terminationLimit),
	)
	if err != nil {
		return err
	}
	monitor.terminationReason = reason
	monitor.timedOut = timedOut
	return nil
}

func superviseSpawnFailure(
	job managedJob,
	request startRequest,
	statusPath string,
	event launcherEvent,
	deadline *time.Timer,
	controls <-chan controlResult,
	launcherWait <-chan error,
) error {
	poll := time.NewTicker(jobPollInterval)
	defer poll.Stop()
	launcherReaped := false
	for {
		active, err := job.activeProcessCount()
		if err != nil {
			return err
		}
		if active == 0 && launcherReaped {
			return publishStatusNew(statusPath, supervisorStatus{
				SchemaVersion:      protocolSchemaVersion,
				OperationID:        request.OperationID,
				Nonce:              request.Nonce,
				SupervisionOutcome: statusOutcomeSpawnFailed,
				TerminationReason:  terminationReasonTargetSpawnFailed,
				TimedOut:           false,
				ActiveProcessCount: 0,
				InputOutcome:       inputOutcomeNotStarted,
				Root:               nil,
				SpawnFailure:       event.SpawnFailure,
			})
		}
		select {
		case waitErr := <-launcherWait:
			if waitErr != nil {
				return terminateAfterAuthorityFailure(job, request, fmt.Errorf("trusted launcher failed after spawn failure: %w", waitErr))
			}
			launcherReaped = true
			launcherWait = nil
		case control := <-controls:
			if control.err != nil {
				return terminateAfterAuthorityFailure(job, request, control.err)
			}
			return terminateAfterAuthorityFailure(job, request, errors.New("parent termination raced target spawn failure"))
		case <-deadline.C:
			return terminateAfterAuthorityFailure(job, request, errors.New("trusted launcher did not drain after target spawn failure"))
		case <-poll.C:
		}
	}
}

func terminatePendingLaunch(
	job managedJob,
	request startRequest,
	statusPath string,
	timedOut bool,
	events <-chan launcherEventResult,
	launcherWait <-chan error,
) error {
	exitCode := job.exitCodes().parent
	if timedOut {
		exitCode = job.exitCodes().deadline
	}
	if err := job.terminate(exitCode); err != nil {
		return err
	}
	authorityDeadline := time.Now().Add(time.Duration(request.TerminationGraceMS) * time.Millisecond)
	var event launcherEvent
	select {
	case eventResult := <-events:
		if eventResult.err != nil {
			_ = waitForJobEmpty(job, time.Until(authorityDeadline))
			return eventResult.err
		}
		event = eventResult.event
	case <-time.After(positiveDurationUntil(authorityDeadline)):
		return errors.New("launcher event was unavailable within termination grace")
	}
	select {
	case <-launcherWait:
	case <-time.After(positiveDurationUntil(authorityDeadline)):
		return errors.New("trusted launcher was not reaped within termination grace")
	}
	if err := waitForJobEmpty(job, positiveDurationUntil(authorityDeadline)); err != nil {
		return err
	}
	if event.Type == launcherEventSpawnFailed {
		return publishStatusNew(statusPath, supervisorStatus{
			SchemaVersion:      protocolSchemaVersion,
			OperationID:        request.OperationID,
			Nonce:              request.Nonce,
			SupervisionOutcome: statusOutcomeSpawnFailed,
			TerminationReason:  terminationReasonTargetSpawnFailed,
			TimedOut:           false,
			ActiveProcessCount: 0,
			InputOutcome:       inputOutcomeNotStarted,
			Root:               nil,
			SpawnFailure:       event.SpawnFailure,
		})
	}
	// A root-started event requires the launcher ACK transaction. If termination
	// won the race before that transaction, exact root status cannot be recovered.
	return errors.New("root handle transfer did not complete before termination")
}

func settledInputOutcome(request startRequest) string {
	if request.Stdin == nil {
		return inputOutcomeNotRequested
	}
	return inputOutcomeDelivered
}
