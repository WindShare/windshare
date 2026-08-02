//go:build windows

package windowsjob

import (
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"os"
	"time"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
)

type rootExitResult struct {
	status rootStatus
	err    error
}

func superviseLaunchedTree(
	job managedJob,
	request supervisionRequest,
	settlements *settlementSink,
	event launcherEvent,
	deadline *time.Timer,
	controls <-chan controlResult,
	launcher *assignedLauncher,
	rawInput *os.File,
	starts *startGate,
	executable *windowsExecutableAuthority,
	handoffDeadline *launcherHandoffDeadline,
	preownedTrigger lifecycleTrigger,
) (resultErr error) {
	if event.Type == launcherEventSpawnFailed {
		_ = launcher.input.Close()
		inputDrain, err := startUnstartedInputDrain(rawInput, request.Stdin)
		if err != nil {
			return terminateAfterUnstartedAuthorityFailure(job, request, fmt.Errorf("start spawn-failure input drain: %w", err))
		}
		defer func() {
			resultErr = errors.Join(resultErr, inputDrain.stopAndJoin(maximumTargetInputAbortWait))
		}()
		return superviseSpawnFailure(
			job,
			request,
			settlements,
			event,
			deadline,
			controls,
			launcher.wait,
			inputDrain,
			preownedTrigger,
		)
	}
	rootHandle, err := rootHandleFromEvent(job, event, launcher.process)
	if err != nil {
		return terminateAfterStartedAuthorityFailure(job, request, err)
	}
	inputWriter, err := adoptLauncherInputWriter(event, launcher.process, request.Stdin)
	if err != nil {
		_ = windows.CloseHandle(rootHandle)
		return terminateAfterStartedAuthorityFailure(job, request, fmt.Errorf("adopt target stdin ownership: %w", err))
	}
	inputWriterOwned := inputWriter != nil
	defer func() {
		if inputWriterOwned {
			resultErr = errors.Join(resultErr, closeOptionalFile(inputWriter))
		}
	}()
	rootProbeHandle, err := duplicateLocalProcessHandle(rootHandle)
	if err != nil {
		_ = windows.CloseHandle(rootHandle)
		return terminateAfterStartedAuthorityFailure(job, request, fmt.Errorf("retain root liveness probe: %w", err))
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
	startEvidence, err := executable.startEvidence(request.Identity, event.PID, rootHandle)
	if err != nil {
		_ = windows.CloseHandle(rootHandle)
		return terminateAfterStartedAuthorityFailure(job, request, err)
	}
	decisions, err := starts.publish(startEvidence)
	if err != nil {
		_ = windows.CloseHandle(rootHandle)
		return terminateAfterStartedAuthorityFailure(job, request, err)
	}
	decision, trigger, err := awaitStartDecisionBeforeRelease(
		decisions,
		controls,
		deadline.C,
		preownedTrigger,
		time.Duration(request.TerminationGraceMilliseconds)*time.Millisecond,
	)
	if err != nil {
		_ = windows.CloseHandle(rootHandle)
		return terminateAfterUnstartedAuthorityFailure(
			job,
			request,
			fmt.Errorf("authenticate pre-release authorities: %w", err),
		)
	}
	if trigger.kind != lifecycleTriggerNone {
		inputCloseErr := closeOptionalFile(inputWriter)
		inputWriterOwned = false
		if inputCloseErr != nil {
			_ = windows.CloseHandle(rootHandle)
			return terminateAfterUnstartedAuthorityFailure(job, request, inputCloseErr)
		}
		return settleUnreleasedTarget(
			job,
			request,
			settlements,
			rootHandle,
			event.PID,
			rawInput,
			triggerReason(trigger),
			"TARGET_NOT_RELEASED",
			"target was contained but not released before termination",
		)
	}
	if decision.Outcome == ownerprotocol.StartDecisionRejected {
		inputCloseErr := closeOptionalFile(inputWriter)
		inputWriterOwned = false
		if inputCloseErr != nil {
			_ = windows.CloseHandle(rootHandle)
			return terminateAfterUnstartedAuthorityFailure(job, request, inputCloseErr)
		}
		return settleUnreleasedTarget(
			job,
			request,
			settlements,
			rootHandle,
			event.PID,
			rawInput,
			ownerprotocol.TerminationStartRejected,
			"START_REJECTED",
			fmt.Sprintf("%s: %s", decision.FailureCode, decision.FailureMessage),
		)
	}
	if err := writeAll(launcher.input, []byte{launcherRootAcknowledgement}); err != nil {
		_ = windows.CloseHandle(rootHandle)
		return terminateAfterStartedAuthorityFailure(job, request, fmt.Errorf("acknowledge root handle ownership: %w", err))
	}
	if err := launcher.input.Close(); err != nil {
		_ = windows.CloseHandle(rootHandle)
		return terminateAfterStartedAuthorityFailure(job, request, fmt.Errorf("close launcher acknowledgement channel: %w", err))
	}
	initialTrigger, err := awaitTrustedLauncherHandoff(
		job,
		request,
		deadline,
		controls,
		launcher,
		handoffDeadline,
		preownedTrigger,
	)
	if err != nil {
		return errors.Join(err, windows.CloseHandle(rootHandle))
	}
	if initialTrigger.kind == lifecycleTriggerNone {
		select {
		case control := <-controls:
			if control.err != nil {
				return errors.Join(
					terminateAfterUnstartedAuthorityFailure(job, request, control.err),
					windows.CloseHandle(rootHandle),
				)
			}
			initialTrigger = controlLifecycleTrigger(control)
		default:
			select {
			case <-deadline.C:
				initialTrigger = deadlineLifecycleTrigger()
			default:
			}
		}
	}
	if initialTrigger.kind != lifecycleTriggerNone {
		inputCloseErr := closeOptionalFile(inputWriter)
		inputWriterOwned = false
		if inputCloseErr != nil {
			return errors.Join(
				terminateAfterUnstartedAuthorityFailure(job, request, inputCloseErr),
				windows.CloseHandle(rootHandle),
			)
		}
		return settleUnreleasedTarget(
			job,
			request,
			settlements,
			rootHandle,
			event.PID,
			rawInput,
			triggerReason(initialTrigger),
			"TARGET_NOT_RELEASED",
			"target was contained but not released before termination",
		)
	}
	if err := resumeContainedTarget(rootProbeHandle); err != nil {
		inputCloseErr := closeOptionalFile(inputWriter)
		inputWriterOwned = false
		return errors.Join(
			terminateAfterUnstartedAuthorityFailure(job, request, err),
			inputCloseErr,
			windows.CloseHandle(rootHandle),
		)
	}
	rootExit := make(chan rootExitResult, 1)
	go func() {
		status, waitErr := waitRootAndClose(rootHandle, event.PID)
		rootExit <- rootExitResult{status: status, err: waitErr}
	}()
	inputDelivery, err := startTargetInputDelivery(rawInput, inputWriter, request.Stdin)
	inputWriterOwned = false
	if err != nil {
		return terminateAfterStartedAuthorityFailure(job, request, fmt.Errorf("start target stdin delivery: %w", err))
	}
	runErr := superviseRootTree(
		job,
		managedRoot{handle: rootProbeHandle, pid: event.PID},
		request,
		settlements,
		deadline,
		controls,
		rootExit,
		inputDelivery,
		lifecycleTrigger{},
	)
	if runErr != nil {
		if settlements.publicationAttempted() {
			// Settlement construction already consumed input evidence and the single
			// publication authority. Re-entering either phase would fabricate a
			// second lifecycle observation after an irrevocable stream attempt.
			return runErr
		}
		_, inputErr := settleTargetInput(request.Stdin, inputDelivery)
		var terminated *authorityTerminationError
		if !errors.As(runErr, &terminated) {
			runErr = terminateAfterStartedAuthorityFailure(job, request, runErr)
		}
		return errors.Join(runErr, inputErr)
	}
	return nil
}

func awaitStartDecisionBeforeRelease(
	decisions <-chan startDecisionResult,
	controls <-chan controlResult,
	deadline <-chan time.Time,
	preowned lifecycleTrigger,
	grace time.Duration,
) (ownerprotocol.StartDecision, lifecycleTrigger, error) {
	if err := preowned.validate(); err != nil {
		return ownerprotocol.StartDecision{}, lifecycleTrigger{}, err
	}
	trigger := preowned
	var triggerErr error
	var timer *time.Timer
	var decisionTimeout <-chan time.Time
	beginDecisionTimeout := func() {
		if timer == nil {
			timer = time.NewTimer(grace)
			decisionTimeout = timer.C
		}
		controls = nil
		deadline = nil
	}
	if trigger.kind != lifecycleTriggerNone {
		beginDecisionTimeout()
	}
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	observeControl := func(control controlResult) {
		if control.err != nil {
			triggerErr = control.err
			beginDecisionTimeout()
			return
		}
		trigger = controlLifecycleTrigger(control)
		beginDecisionTimeout()
	}
	observeDeadline := func() {
		trigger = deadlineLifecycleTrigger()
		beginDecisionTimeout()
	}

	for {
		select {
		case result := <-decisions:
			if result.err != nil {
				return ownerprotocol.StartDecision{}, trigger, result.err
			}
			// A control already readable at the decision boundary was authoritative
			// while the target remained suspended, so it wins over release.
			if trigger.kind == lifecycleTriggerNone && triggerErr == nil {
				select {
				case control := <-controls:
					observeControl(control)
				default:
					select {
					case <-deadline:
						observeDeadline()
					default:
					}
				}
			}
			if triggerErr != nil {
				return result.decision, trigger, triggerErr
			}
			return result.decision, trigger, nil
		case control := <-controls:
			observeControl(control)
		case <-deadline:
			observeDeadline()
		case <-decisionTimeout:
			return ownerprotocol.StartDecision{}, trigger, errors.New(
				"start decision did not arrive within termination grace after a pre-release trigger",
			)
		}
	}
}

func triggerReason(trigger lifecycleTrigger) string {
	switch trigger.kind {
	case lifecycleTriggerControl:
		return trigger.control.reason
	case lifecycleTriggerDeadline:
		return ownerprotocol.TerminationDeadline
	default:
		return ""
	}
}

func settleUnreleasedTarget(
	job managedJob,
	request supervisionRequest,
	settlements *settlementSink,
	rootHandle windows.Handle,
	processID uint32,
	rawInput *os.File,
	reason string,
	failureCode string,
	failureMessage string,
) (resultErr error) {
	switch reason {
	case ownerprotocol.TerminationStop,
		ownerprotocol.TerminationParentLost,
		ownerprotocol.TerminationDeadline,
		ownerprotocol.TerminationStartRejected:
	default:
		_ = windows.CloseHandle(rootHandle)
		return terminateAfterUnstartedAuthorityFailure(
			job,
			request,
			fmt.Errorf("unreleased target has invalid termination reason %q", reason),
		)
	}
	inputDrain, err := startUnstartedInputDrain(rawInput, request.Stdin)
	if err != nil {
		_ = windows.CloseHandle(rootHandle)
		return terminateAfterUnstartedAuthorityFailure(
			job,
			request,
			fmt.Errorf("start unreleased-target input drain: %w", err),
		)
	}
	if inputDrain != nil {
		defer func() {
			resultErr = errors.Join(
				resultErr,
				inputDrain.stopAndJoin(maximumTargetInputAbortWait),
			)
		}()
	}

	exitCode := job.exitCodes().parent
	if reason == ownerprotocol.TerminationDeadline {
		exitCode = job.exitCodes().deadline
	}
	if reason == ownerprotocol.TerminationStartRejected {
		exitCode = job.exitCodes().authority
	}
	terminationErr := job.terminate(exitCode)
	root, rootErr := waitRootAndClose(rootHandle, processID)
	inputErr := inputDrain.await(
		time.Duration(request.TerminationGraceMilliseconds) * time.Millisecond,
	)
	cleanupErr := waitForJobEmpty(
		job,
		time.Duration(request.TerminationGraceMilliseconds)*time.Millisecond,
	)
	if err := errors.Join(terminationErr, rootErr, inputErr, cleanupErr); err != nil {
		return &authorityTerminationError{
			cause: err, cleanupErr: cleanupErr, start: targetKnownUnstarted,
		}
	}
	active := uint32(0)
	return settlements.publish(ownerprotocol.Settlement{
		SchemaVersion:     ownerprotocol.SettlementSchemaVersion,
		Identity:          request.Identity,
		TerminationReason: reason,
		Target: ownerprotocol.TargetEvidence{
			Outcome:        ownerprotocol.TargetNotStarted,
			FailureCode:    failureCode,
			FailureMessage: boundedDiagnostic(errors.New(failureMessage)),
		},
		Input:     ownerprotocol.InputEvidence{Outcome: unstartedInputOutcome(request)},
		TreeState: ownerprotocol.TreeProvenEmpty,
		Cleanup:   ownerprotocol.CleanupEvidence{Outcome: ownerprotocol.CleanupCompleted},
		Platform: ownerprotocol.PlatformEvidence{
			Kind:     ownerprotocol.PlatformWindowsJob,
			OwnerPID: os.Getpid(),
			Root: &ownerprotocol.RootEvidence{
				PID:      int(root.PID),
				State:    ownerprotocol.RootExited,
				ExitCode: func() *int64 { value := int64(root.ExitCode); return &value }(),
			},
			ActiveProcessCount: &active,
		},
	})
}

func superviseRootTree(
	job jobLifecycleAuthority,
	rootAuthority rootLifecycleAuthority,
	request supervisionRequest,
	settlements *settlementSink,
	deadline *time.Timer,
	controls <-chan controlResult,
	rootExit <-chan rootExitResult,
	inputDelivery *targetInputDelivery,
	initialTrigger lifecycleTrigger,
) error {
	return superviseRootTreeWithPollInterval(
		job,
		rootAuthority,
		request,
		settlements,
		deadline,
		controls,
		rootExit,
		jobPollInterval,
		inputDelivery,
		initialTrigger,
	)
}

func superviseRootTreeWithPollInterval(
	job jobLifecycleAuthority,
	rootAuthority rootLifecycleAuthority,
	request supervisionRequest,
	settlements *settlementSink,
	deadline *time.Timer,
	controls <-chan controlResult,
	rootExit <-chan rootExitResult,
	pollInterval time.Duration,
	inputDelivery *targetInputDelivery,
	initialTrigger lifecycleTrigger,
) error {
	if err := initialTrigger.validate(); err != nil {
		return err
	}
	poll := time.NewTicker(pollInterval)
	defer poll.Stop()
	monitor := rootTreeMonitor{
		rootExit:          rootExit,
		controls:          controls,
		deadline:          deadline.C,
		terminationReason: ownerprotocol.TerminationNatural,
		inputDelivery:     inputDelivery,
		initialTrigger:    initialTrigger,
	}
	defer monitor.close()
	for {
		active, err := job.activeProcessCount()
		if err != nil {
			return err
		}
		if active == 0 {
			return monitor.settleEmptyTree(job, request, settlements)
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
	inputDelivery       *targetInputDelivery
	initialTrigger      lifecycleTrigger
}

func (monitor *rootTreeMonitor) close() {
	if monitor.pendingIntervention != nil {
		monitor.pendingIntervention.snapshot.close()
	}
}

func (monitor *rootTreeMonitor) awaitEvent(
	job jobLifecycleAuthority,
	rootAuthority rootLifecycleAuthority,
	request supervisionRequest,
	poll <-chan time.Time,
) error {
	switch monitor.initialTrigger.kind {
	case lifecycleTriggerControl:
		control := monitor.initialTrigger.control
		monitor.initialTrigger = lifecycleTrigger{}
		return monitor.observeControl(job, rootAuthority, request, control)
	case lifecycleTriggerDeadline:
		monitor.initialTrigger = lifecycleTrigger{}
		return monitor.observeDeadline(job, rootAuthority, request)
	case lifecycleTriggerNone:
	default:
		return errors.New("root supervision received an invalid lifecycle trigger")
	}
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
	request supervisionRequest,
	result rootExitResult,
) error {
	if result.err != nil {
		return terminateAfterStartedAuthorityFailure(job, request, result.err)
	}
	monitor.root = &result.status
	monitor.rootExit = nil
	return nil
}

func (monitor *rootTreeMonitor) observeControl(
	job jobLifecycleAuthority,
	rootAuthority rootLifecycleAuthority,
	request supervisionRequest,
	control controlResult,
) error {
	monitor.controls = nil
	if control.err != nil {
		if monitor.terminating {
			return nil
		}
		intervention, err := terminateObservedNonemptyJob(
			job,
			rootAuthority,
			job.exitCodes().authority,
			ownerprotocol.TerminationOwnerFailure,
			false,
		)
		if err != nil {
			return err
		}
		if !intervention.applied {
			return nil
		}
		// A malformed control stream is provisional until an exact retained
		// member reports the private authority exit code. This prevents delayed
		// root-exit publication from overwriting an already-natural tree.
		monitor.fatalControl = control.err
		monitor.pendingIntervention = &intervention
		monitor.beginTerminationGrace(request)
		return nil
	}
	if monitor.terminating {
		return nil
	}
	exitCode := job.exitCodes().parent
	timedOut := false
	if control.reason == ownerprotocol.TerminationDeadline {
		exitCode = job.exitCodes().deadline
		timedOut = true
	}
	return monitor.intervene(
		job,
		rootAuthority,
		request,
		exitCode,
		control.reason,
		timedOut,
	)
}

func (monitor *rootTreeMonitor) observeDeadline(
	job jobLifecycleAuthority,
	rootAuthority rootLifecycleAuthority,
	request supervisionRequest,
) error {
	if monitor.terminating {
		return nil
	}
	return monitor.intervene(
		job,
		rootAuthority,
		request,
		job.exitCodes().deadline,
		ownerprotocol.TerminationDeadline,
		true,
	)
}

func (monitor *rootTreeMonitor) intervene(
	job jobLifecycleAuthority,
	rootAuthority rootLifecycleAuthority,
	request supervisionRequest,
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

func (monitor *rootTreeMonitor) beginTerminationGrace(request supervisionRequest) {
	monitor.terminating = true
	monitor.terminationLimit = time.Now().Add(time.Duration(request.TerminationGraceMilliseconds) * time.Millisecond)
	monitor.terminationDeadline = time.After(positiveDurationUntil(monitor.terminationLimit))
}

func (monitor *rootTreeMonitor) settleEmptyTree(
	job jobLifecycleAuthority,
	request supervisionRequest,
	settlements *settlementSink,
) error {
	if err := monitor.awaitRootAfterEmpty(request); err != nil {
		return err
	}
	if err := monitor.reconcileIntervention(job); err != nil {
		return err
	}
	inputEvidence, inputAuthorityErr := settleTargetInput(request.Stdin, monitor.inputDelivery)
	settlement := completedSettlement(
		request,
		*monitor.root,
		monitor.terminationReason,
		inputEvidence,
		inputAuthorityErr,
	)
	if monitor.terminationReason == ownerprotocol.TerminationOwnerFailure {
		if monitor.fatalControl == nil {
			return errors.New("owner-failure termination lacks its causal control evidence")
		}
		settlement.OwnerFailure = &ownerprotocol.FailureEvidence{
			Code:    "CONTROL_AUTHORITY_FAILED",
			Message: boundedDiagnostic(errors.Join(monitor.fatalControl, inputAuthorityErr)),
		}
	}
	return settlements.publish(settlement)
}

func (monitor *rootTreeMonitor) awaitRootAfterEmpty(request supervisionRequest) error {
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
	case <-time.After(time.Duration(request.TerminationGraceMilliseconds) * time.Millisecond):
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
	request supervisionRequest,
	settlements *settlementSink,
	event launcherEvent,
	deadline *time.Timer,
	controls <-chan controlResult,
	launcherWait <-chan error,
	inputDrain *unstartedInputDrain,
	initialTrigger lifecycleTrigger,
) error {
	if err := initialTrigger.validate(); err != nil {
		return terminateAfterUnstartedAuthorityFailure(job, request, err)
	}
	poll := time.NewTicker(jobPollInterval)
	defer poll.Stop()
	launcherReaped := false
	inputDrained := inputDrain == nil
	inputCompletion := inputDrain.completed()
	for {
		active, err := job.activeProcessCount()
		if err != nil {
			return err
		}
		if active == 0 && launcherReaped && inputDrained {
			if inputErr := inputDrain.resultValue(); inputErr != nil {
				return terminateAfterUnstartedAuthorityFailure(job, request, fmt.Errorf("drain input after target spawn failure: %w", inputErr))
			}
			return settlements.publish(spawnFailedSettlement(request, errors.New(*event.SpawnFailure)))
		}
		switch initialTrigger.kind {
		case lifecycleTriggerControl:
			initialTrigger = lifecycleTrigger{}
			return terminateAfterUnstartedAuthorityFailure(job, request, errors.New("parent termination raced target spawn failure"))
		case lifecycleTriggerDeadline:
			initialTrigger = lifecycleTrigger{}
			return terminateAfterUnstartedAuthorityFailure(job, request, errors.New("trusted launcher did not drain after target spawn failure"))
		case lifecycleTriggerNone:
		default:
			return terminateAfterUnstartedAuthorityFailure(job, request, errors.New("spawn-failure supervision received an invalid lifecycle trigger"))
		}
		select {
		case <-inputCompletion:
			inputDrained = true
			inputCompletion = nil
		case waitErr := <-launcherWait:
			if waitErr != nil {
				return terminateAfterUnstartedAuthorityFailure(job, request, fmt.Errorf("trusted launcher failed after spawn failure: %w", waitErr))
			}
			launcherReaped = true
			launcherWait = nil
		case control := <-controls:
			if control.err != nil {
				return terminateAfterUnstartedAuthorityFailure(job, request, control.err)
			}
			return terminateAfterUnstartedAuthorityFailure(job, request, errors.New("parent termination raced target spawn failure"))
		case <-deadline.C:
			return terminateAfterUnstartedAuthorityFailure(job, request, errors.New("trusted launcher did not drain after target spawn failure"))
		case <-poll.C:
		}
	}
}
