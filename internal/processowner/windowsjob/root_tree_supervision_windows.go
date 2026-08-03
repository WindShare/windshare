//go:build windows

package windowsjob

import (
	"errors"
	"time"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
)

type rootExitResult struct {
	status rootStatus
	err    error
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
