//go:build windows

package windowsjob

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

type pendingLauncherSupervision struct {
	job         managedJob
	request     supervisionRequest
	settlements *settlementSink
	deadline    *time.Timer
	controls    <-chan controlResult
	launcher    *assignedLauncher
	rawInput    *os.File
	starts      *startGate
	executable  *windowsExecutableAuthority
}

const ownerReadyByte byte = 0xa5

func writeAll(writer io.Writer, encoded []byte) error {
	for len(encoded) > 0 {
		written, err := writer.Write(encoded)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		encoded = encoded[written:]
	}
	return nil
}

func runSupervisorPlatform(
	request supervisionRequest,
	settlements *settlementSink,
	control *os.File,
	rawInput *os.File,
	starts *startGate,
	ready io.Writer,
) (resultErr error) {
	if err := validateWindowsEnvironment(request.Environment); err != nil {
		return err
	}
	terminationCodes, err := generateTerminationExitCodes()
	if err != nil {
		return err
	}
	job, err := createManagedJob()
	if err != nil {
		return err
	}
	job.terminationCodes = terminationCodes
	defer job.close()
	deadline := time.NewTimer(time.Duration(request.DeadlineMilliseconds) * time.Millisecond)
	defer deadline.Stop()
	parentControls, closeParentAuthority, err := watchParentProcess(request)
	if err != nil {
		return terminateAfterUnstartedAuthorityFailure(job, request, err)
	}
	defer func() { resultErr = errors.Join(resultErr, closeParentAuthority()) }()
	fileControls, closeFileAuthority := watchTerminationControl(control, request)
	defer func() { resultErr = errors.Join(resultErr, closeFileAuthority()) }()
	controls, closeControlMerge := mergeControlAuthorities(parentControls, fileControls)
	defer func() { resultErr = errors.Join(resultErr, closeControlMerge()) }()
	rawInputFile, err := rawInputAuthority(request, rawInput)
	if err != nil {
		return terminateAfterUnstartedAuthorityFailure(job, request, err)
	}
	// Start returns to the caller only after both create-new destinations and
	// control authorities are established, eliminating stop-vs-preflight races.
	if err := writeAll(ready, []byte{ownerReadyByte}); err != nil {
		return terminateAfterUnstartedAuthorityFailure(
			job,
			request,
			fmt.Errorf("publish process-owner readiness: %w", err),
		)
	}
	executable, err := holdWindowsExecutable(request.Executable)
	if err != nil {
		inputDrain, drainStartErr := startUnstartedInputDrain(rawInput, request.Stdin)
		if drainStartErr != nil {
			return terminateAfterUnstartedAuthorityFailure(
				job,
				request,
				errors.Join(err, fmt.Errorf("start preflight-failure input drain: %w", drainStartErr)),
			)
		}
		if inputDrain != nil {
			defer func() {
				resultErr = errors.Join(
					resultErr,
					inputDrain.stopAndJoin(maximumTargetInputAbortWait),
				)
			}()
			if drainErr := inputDrain.await(
				time.Duration(request.TerminationGraceMilliseconds) * time.Millisecond,
			); drainErr != nil {
				return terminateAfterUnstartedAuthorityFailure(
					job,
					request,
					errors.Join(err, fmt.Errorf("drain input after executable preflight failure: %w", drainErr)),
				)
			}
		}
		if cleanupErr := waitForJobEmpty(
			job,
			time.Duration(request.TerminationGraceMilliseconds)*time.Millisecond,
		); cleanupErr != nil {
			return &authorityTerminationError{
				cause: err, cleanupErr: cleanupErr, start: targetKnownUnstarted,
			}
		}
		return settlements.publish(spawnFailedSettlement(request, err))
	}
	defer func() { resultErr = errors.Join(resultErr, executable.close()) }()

	launcher, err := startAssignedLauncher(job, request)
	if err != nil {
		return terminateAfterAuthorityFailure(job, request, err)
	}
	defer func() {
		// The retained launcher identity is part of the supervision authority. Its
		// release failure cannot replace the runtime verdict, but must remain visible.
		resultErr = errors.Join(resultErr, closeOwnedProcessHandle(
			launcher.membershipHandle,
			"close retained trusted launcher identity",
		))
	}()
	defer launcher.eventReader.Close()
	defer launcher.input.Close()
	launcherEventChannel := make(chan launcherEventResult, 1)
	go func() {
		event, eventErr := readLauncherEvent(launcher.eventReader)
		launcherEventChannel <- launcherEventResult{event: event, err: eventErr}
	}()

	return (pendingLauncherSupervision{
		job: job, request: request, settlements: settlements,
		deadline: deadline, controls: controls, launcher: launcher, rawInput: rawInputFile,
		starts: starts, executable: executable,
	}).awaitLauncherEvent(launcherEventChannel)
}

func rawInputAuthority(request supervisionRequest, rawInput *os.File) (*os.File, error) {
	if request.Stdin == nil {
		if rawInput != nil {
			return nil, errors.New("raw stdin authority is present for an undeclared input")
		}
		return nil, nil
	}
	if rawInput == nil {
		return nil, errors.New("raw stdin authority must be an inherited anonymous file handle")
	}
	return rawInput, nil
}

func (supervision pendingLauncherSupervision) awaitLauncherEvent(
	launcherEvents <-chan launcherEventResult,
) error {
	select {
	case eventResult := <-launcherEvents:
		handoffDeadline := newLauncherHandoffDeadline(supervision.request)
		defer handoffDeadline.close()
		return supervision.acceptLauncherEvent(
			eventResult,
			supervision.deadline,
			supervision.controls,
			handoffDeadline,
			lifecycleTrigger{},
		)
	case control := <-supervision.controls:
		return supervision.acceptPrelaunchControl(control, launcherEvents)
	case <-supervision.deadline.C:
		return supervision.acceptPrelaunchDeadline(launcherEvents)
	}
}

func (supervision pendingLauncherSupervision) acceptLauncherEvent(
	result launcherEventResult,
	deadline *time.Timer,
	controls <-chan controlResult,
	handoffDeadline *launcherHandoffDeadline,
	preownedTrigger lifecycleTrigger,
) error {
	if result.err != nil {
		return terminateAfterAuthorityFailure(supervision.job, supervision.request, result.err)
	}
	return superviseLaunchedTree(
		supervision.job,
		supervision.request,
		supervision.settlements,
		result.event,
		deadline,
		controls,
		supervision.launcher,
		supervision.rawInput,
		supervision.starts,
		supervision.executable,
		handoffDeadline,
		preownedTrigger,
	)
}

func (supervision pendingLauncherSupervision) acceptPrelaunchControl(
	control controlResult,
	launcherEvents <-chan launcherEventResult,
) error {
	if control.err != nil {
		return terminateAfterAuthorityFailure(supervision.job, supervision.request, control.err)
	}
	handoffDeadline := newLauncherHandoffDeadline(supervision.request)
	defer handoffDeadline.close()
	transition, err := awaitPendingLauncherTransition(
		launcherEvents,
		handoffDeadline,
		controlLifecycleTrigger(control),
	)
	if err != nil {
		return terminateAfterAuthorityFailure(supervision.job, supervision.request, err)
	}
	return supervision.acceptLauncherEvent(
		transition.eventResult,
		supervision.deadline,
		supervision.controls,
		handoffDeadline,
		transition.trigger,
	)
}

func (supervision pendingLauncherSupervision) acceptPrelaunchDeadline(
	launcherEvents <-chan launcherEventResult,
) error {
	handoffDeadline := newLauncherHandoffDeadline(supervision.request)
	defer handoffDeadline.close()
	transition, err := awaitPendingLauncherTransition(
		launcherEvents,
		handoffDeadline,
		deadlineLifecycleTrigger(),
	)
	if err != nil {
		return terminateAfterAuthorityFailure(supervision.job, supervision.request, err)
	}
	return supervision.acceptLauncherEvent(
		transition.eventResult,
		supervision.deadline,
		supervision.controls,
		handoffDeadline,
		transition.trigger,
	)
}
