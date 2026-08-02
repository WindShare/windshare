//go:build windows

package windowsjob

import (
	"errors"
	"fmt"
	"slices"
	"time"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
	"golang.org/x/sys/windows"
)

const (
	launcherEventHandoffTimeout = "trusted launcher did not publish its lifecycle event within termination grace"
	launcherExitHandoffTimeout  = "trusted launcher handoff did not complete within termination grace"
)

// launcherHandoffDeadline is one absolute budget for publishing the launcher
// event, reaping the trusted launcher, and fencing its Job membership. Sharing
// one timer prevents each mechanical phase from silently resetting the grace.
type launcherHandoffDeadline struct {
	expiration <-chan struct{}
	stopTimer  func() bool
}

type pendingLauncherTransition struct {
	eventResult launcherEventResult
	trigger     lifecycleTrigger
}

type lifecycleTriggerKind uint8

const (
	lifecycleTriggerNone lifecycleTriggerKind = iota
	lifecycleTriggerControl
	lifecycleTriggerDeadline
)

type lifecycleTrigger struct {
	kind    lifecycleTriggerKind
	control controlResult
}

type launcherExitObservation struct {
	waitErr error
	err     error
}

type launcherMembershipAuthority interface {
	activeProcessIDs(maximumProcesses int) ([]uint32, error)
}

func controlLifecycleTrigger(control controlResult) lifecycleTrigger {
	return lifecycleTrigger{kind: lifecycleTriggerControl, control: control}
}

func deadlineLifecycleTrigger() lifecycleTrigger {
	return lifecycleTrigger{kind: lifecycleTriggerDeadline}
}

func (trigger lifecycleTrigger) validate() error {
	hasControlPayload := trigger.control.reason != "" || trigger.control.err != nil
	switch trigger.kind {
	case lifecycleTriggerNone:
		if hasControlPayload {
			return errors.New("empty lifecycle trigger carries a control payload")
		}
	case lifecycleTriggerControl:
		if !validLifecycleControlReason(trigger.control.reason) || trigger.control.err != nil {
			return errors.New("control lifecycle trigger requires one successful termination reason")
		}
	case lifecycleTriggerDeadline:
		if hasControlPayload {
			return errors.New("deadline lifecycle trigger carries a control payload")
		}
	default:
		return errors.New("lifecycle trigger kind is invalid")
	}
	return nil
}

func validLifecycleControlReason(reason string) bool {
	switch reason {
	case ownerprotocol.TerminationStop,
		ownerprotocol.TerminationParentLost,
		ownerprotocol.TerminationDeadline:
		return true
	default:
		return false
	}
}

func newLauncherHandoffDeadline(request supervisionRequest) *launcherHandoffDeadline {
	expiresAt := time.Now().Add(time.Duration(request.TerminationGraceMilliseconds) * time.Millisecond)
	expiration := make(chan struct{})
	timer := time.AfterFunc(positiveDurationUntil(expiresAt), func() {
		// Expiry is a lifecycle fact consumed by several ordered phases. Closing a
		// broadcast keeps that fact observable after an event wins at the boundary.
		close(expiration)
	})
	return &launcherHandoffDeadline{
		expiration: expiration,
		stopTimer:  timer.Stop,
	}
}

func (deadline *launcherHandoffDeadline) close() {
	if deadline != nil && deadline.stopTimer != nil {
		deadline.stopTimer()
	}
}

func awaitPendingLauncherTransition(
	events <-chan launcherEventResult,
	deadline *launcherHandoffDeadline,
	trigger lifecycleTrigger,
) (pendingLauncherTransition, error) {
	if err := trigger.validate(); err != nil {
		return pendingLauncherTransition{}, err
	}
	if trigger.kind == lifecycleTriggerNone {
		return pendingLauncherTransition{}, errors.New("pending launcher transition requires a lifecycle trigger")
	}
	eventResult, err := awaitPendingLauncherEvent(events, deadline)
	if err != nil {
		return pendingLauncherTransition{}, err
	}
	return pendingLauncherTransition{
		eventResult: eventResult,
		trigger:     trigger,
	}, nil
}

func awaitPendingLauncherEvent(
	events <-chan launcherEventResult,
	deadline *launcherHandoffDeadline,
) (launcherEventResult, error) {
	if events == nil {
		return launcherEventResult{}, errors.New("trusted launcher event authority is unavailable")
	}
	if deadline == nil || deadline.expiration == nil {
		return launcherEventResult{}, errors.New("trusted launcher handoff deadline is unavailable")
	}
	// An event already published at the boundary is authoritative even when the
	// deadline signal is simultaneously observable.
	select {
	case result, ok := <-events:
		return validateLauncherEventObservation(result, ok)
	default:
	}
	select {
	case result, ok := <-events:
		return validateLauncherEventObservation(result, ok)
	case <-deadline.expiration:
		select {
		case result, ok := <-events:
			return validateLauncherEventObservation(result, ok)
		default:
			return launcherEventResult{}, errors.New(launcherEventHandoffTimeout)
		}
	}
}

func validateLauncherEventObservation(
	result launcherEventResult,
	ok bool,
) (launcherEventResult, error) {
	if !ok {
		return launcherEventResult{}, errors.New("trusted launcher event authority closed without an observation")
	}
	return result, nil
}

func awaitTrustedLauncherHandoff(
	job managedJob,
	request supervisionRequest,
	deadline *time.Timer,
	controls <-chan controlResult,
	launcher *assignedLauncher,
	handoffDeadline *launcherHandoffDeadline,
	preownedTrigger lifecycleTrigger,
) (lifecycleTrigger, error) {
	if err := preownedTrigger.validate(); err != nil {
		return lifecycleTrigger{}, terminateAfterStartedAuthorityFailure(job, request, err)
	}
	if preownedTrigger.kind != lifecycleTriggerNone {
		if observation, ready := pollTrustedLauncherExit(launcher.wait); ready {
			if observation.err != nil {
				return lifecycleTrigger{}, terminateAfterStartedAuthorityFailure(job, request, observation.err)
			}
			if err := completeTrustedLauncherHandoff(job, request, launcher, observation.waitErr, handoffDeadline); err != nil {
				return lifecycleTrigger{}, err
			}
		} else if err := finishTrustedLauncherHandoff(job, request, launcher, handoffDeadline); err != nil {
			return lifecycleTrigger{}, err
		}
		return preownedTrigger, nil
	}
	if observation, ready := pollTrustedLauncherExit(launcher.wait); ready {
		if observation.err != nil {
			return lifecycleTrigger{}, terminateAfterStartedAuthorityFailure(job, request, observation.err)
		}
		if err := completeTrustedLauncherHandoff(job, request, launcher, observation.waitErr, handoffDeadline); err != nil {
			return lifecycleTrigger{}, err
		}
		return lifecycleTrigger{}, nil
	}
	select {
	case waitErr, ok := <-launcher.wait:
		observation := validateLauncherExitObservation(waitErr, ok)
		if observation.err != nil {
			return lifecycleTrigger{}, terminateAfterStartedAuthorityFailure(job, request, observation.err)
		}
		if err := completeTrustedLauncherHandoff(job, request, launcher, observation.waitErr, handoffDeadline); err != nil {
			return lifecycleTrigger{}, err
		}
		return lifecycleTrigger{}, nil
	case control := <-controls:
		if control.err != nil {
			return lifecycleTrigger{}, terminateAfterStartedAuthorityFailure(job, request, control.err)
		}
		if err := finishTrustedLauncherHandoff(job, request, launcher, handoffDeadline); err != nil {
			return lifecycleTrigger{}, err
		}
		return controlLifecycleTrigger(control), nil
	case <-deadline.C:
		if err := finishTrustedLauncherHandoff(job, request, launcher, handoffDeadline); err != nil {
			return lifecycleTrigger{}, err
		}
		return deadlineLifecycleTrigger(), nil
	case <-handoffDeadline.expiration:
		if observation, ready := pollTrustedLauncherExit(launcher.wait); ready {
			if observation.err != nil {
				return lifecycleTrigger{}, terminateAfterStartedAuthorityFailure(job, request, observation.err)
			}
			if err := completeTrustedLauncherHandoff(job, request, launcher, observation.waitErr, handoffDeadline); err != nil {
				return lifecycleTrigger{}, err
			}
			return lifecycleTrigger{}, nil
		}
		return lifecycleTrigger{}, terminateAfterStartedAuthorityFailure(
			job,
			request,
			errors.New(launcherExitHandoffTimeout),
		)
	}
}

func finishTrustedLauncherHandoff(
	job managedJob,
	request supervisionRequest,
	launcher *assignedLauncher,
	deadline *launcherHandoffDeadline,
) error {
	observation := awaitTrustedLauncherExit(launcher.wait, deadline)
	if observation.err != nil {
		return terminateAfterStartedAuthorityFailure(job, request, observation.err)
	}
	return completeTrustedLauncherHandoff(job, request, launcher, observation.waitErr, deadline)
}

func awaitTrustedLauncherExit(
	wait <-chan error,
	deadline *launcherHandoffDeadline,
) launcherExitObservation {
	if wait == nil {
		return launcherExitObservation{err: errors.New("trusted launcher exit authority is unavailable")}
	}
	if deadline == nil || deadline.expiration == nil {
		return launcherExitObservation{err: errors.New("trusted launcher handoff deadline is unavailable")}
	}
	if observation, ready := pollTrustedLauncherExit(wait); ready {
		return observation
	}
	select {
	case waitErr, ok := <-wait:
		return validateLauncherExitObservation(waitErr, ok)
	case <-deadline.expiration:
		if observation, ready := pollTrustedLauncherExit(wait); ready {
			return observation
		}
		return launcherExitObservation{err: errors.New(launcherExitHandoffTimeout)}
	}
}

func pollTrustedLauncherExit(wait <-chan error) (launcherExitObservation, bool) {
	select {
	case waitErr, ok := <-wait:
		return validateLauncherExitObservation(waitErr, ok), true
	default:
		return launcherExitObservation{}, false
	}
}

func validateLauncherExitObservation(waitErr error, ok bool) launcherExitObservation {
	if !ok {
		return launcherExitObservation{err: errors.New("trusted launcher exit authority closed without an observation")}
	}
	return launcherExitObservation{waitErr: waitErr}
}

func completeTrustedLauncherHandoff(
	job managedJob,
	request supervisionRequest,
	launcher *assignedLauncher,
	waitErr error,
	deadline *launcherHandoffDeadline,
) error {
	if waitErr != nil {
		return terminateAfterStartedAuthorityFailure(job, request, fmt.Errorf("trusted launcher failed during root handoff: %w", waitErr))
	}
	launcherPID := launcher.process.Pid
	if launcherPID <= 0 || uint64(launcherPID) > maxWindowsProcessID {
		return terminateAfterStartedAuthorityFailure(job, request, errors.New("trusted launcher PID is invalid"))
	}
	retainedPID, err := windows.GetProcessId(launcher.membershipHandle)
	if err != nil || retainedPID != uint32(launcherPID) {
		if err == nil {
			err = errors.New("retained trusted launcher identity changed")
		}
		return terminateAfterStartedAuthorityFailure(job, request, fmt.Errorf("verify trusted launcher identity: %w", err))
	}
	if err := waitForProcessMembershipRelease(
		job,
		retainedPID,
		maxTerminationSnapshotProcesses,
		deadline,
	); err != nil {
		return terminateAfterStartedAuthorityFailure(job, request, fmt.Errorf("fence trusted launcher membership: %w", err))
	}
	return nil
}

func waitForProcessMembershipRelease(
	job launcherMembershipAuthority,
	processID uint32,
	maximumProcesses int,
	deadline *launcherHandoffDeadline,
) error {
	if deadline == nil || deadline.expiration == nil {
		return errors.New("trusted launcher handoff deadline is unavailable")
	}
	poll := time.NewTicker(jobPollInterval)
	defer poll.Stop()
	for {
		processIDs, err := job.activeProcessIDs(maximumProcesses)
		if err != nil {
			return err
		}
		if !slices.Contains(processIDs, processID) {
			return nil
		}
		select {
		case <-deadline.expiration:
			// Membership released at the exact boundary is valid handoff evidence;
			// rechecking avoids turning scheduler order into an owner failure.
			processIDs, err := job.activeProcessIDs(maximumProcesses)
			if err != nil {
				return err
			}
			if !slices.Contains(processIDs, processID) {
				return nil
			}
			return errors.New("trusted launcher remained in the Job process list beyond termination grace")
		case <-poll.C:
		}
	}
}
