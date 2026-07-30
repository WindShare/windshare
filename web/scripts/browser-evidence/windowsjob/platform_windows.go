//go:build windows

package main

import (
	"errors"
	"io"
	"os"
	"time"
)

type pendingLauncherSupervision struct {
	job        managedJob
	request    startRequest
	statusPath string
	deadline   *time.Timer
	controls   <-chan controlResult
	launcher   *assignedLauncher
}

func runSupervisorPlatform(
	request startRequest,
	statusPath string,
	controlPath string,
	rawInput io.Reader,
) (resultErr error) {
	if err := validateWindowsEnvironment(request.Environment); err != nil {
		return err
	}
	terminationCodes, err := deriveTerminationExitCodes(request.Nonce)
	if err != nil {
		return err
	}
	if err := ensureFreshStatusDestination(statusPath); err != nil {
		return err
	}
	if err := ensureFreshControlDestination(controlPath); err != nil {
		return err
	}
	job, err := createManagedJob()
	if err != nil {
		return err
	}
	job.terminationCodes = terminationCodes
	defer job.close()

	deadline := time.NewTimer(time.Duration(request.DeadlineMS) * time.Millisecond)
	defer deadline.Stop()
	parentControls, closeParentAuthority, err := watchParentProcess(request)
	if err != nil {
		return err
	}
	defer closeParentAuthority()
	fileControls, closeFileAuthority := watchTerminationControl(controlPath, request)
	defer closeFileAuthority()
	controls, closeControlMerge := mergeControlAuthorities(parentControls, fileControls)
	defer closeControlMerge()
	rawInputFile, err := rawInputAuthority(request, rawInput)
	if err != nil {
		return err
	}
	launcher, err := startAssignedLauncher(job, request, rawInputFile)
	if err != nil {
		return err
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
		job: job, request: request, statusPath: statusPath,
		deadline: deadline, controls: controls, launcher: launcher,
	}).awaitLauncherEvent(launcherEventChannel)
}

func rawInputAuthority(request startRequest, rawInput io.Reader) (*os.File, error) {
	if request.Stdin == nil {
		return nil, requireExactRawEOF(rawInput)
	}
	rawInputFile, ok := rawInput.(*os.File)
	if !ok {
		return nil, errors.New("raw stdin authority must be an inherited anonymous file handle")
	}
	return rawInputFile, nil
}

func (supervision pendingLauncherSupervision) awaitLauncherEvent(
	launcherEvents <-chan launcherEventResult,
) error {
	select {
	case eventResult := <-launcherEvents:
		return supervision.acceptLauncherEvent(eventResult, supervision.deadline, supervision.controls)
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
) error {
	if result.err != nil {
		return terminateAfterAuthorityFailure(supervision.job, supervision.request, result.err)
	}
	return superviseLaunchedTree(
		supervision.job,
		supervision.request,
		supervision.statusPath,
		result.event,
		deadline,
		controls,
		supervision.launcher,
	)
}

func (supervision pendingLauncherSupervision) acceptPrelaunchControl(
	control controlResult,
	launcherEvents <-chan launcherEventResult,
) error {
	if control.err != nil {
		return terminateAfterAuthorityFailure(supervision.job, supervision.request, control.err)
	}
	select {
	case eventResult := <-launcherEvents:
		replayedControl := make(chan controlResult, 1)
		replayedControl <- control
		return supervision.acceptLauncherEvent(eventResult, supervision.deadline, replayedControl)
	default:
		return terminatePendingLaunch(
			supervision.job,
			supervision.request,
			supervision.statusPath,
			false,
			launcherEvents,
			supervision.launcher.wait,
		)
	}
}

func (supervision pendingLauncherSupervision) acceptPrelaunchDeadline(
	launcherEvents <-chan launcherEventResult,
) error {
	select {
	case eventResult := <-launcherEvents:
		immediateDeadline := time.NewTimer(0)
		defer immediateDeadline.Stop()
		return supervision.acceptLauncherEvent(eventResult, immediateDeadline, supervision.controls)
	default:
		return terminatePendingLaunch(
			supervision.job,
			supervision.request,
			supervision.statusPath,
			true,
			launcherEvents,
			supervision.launcher.wait,
		)
	}
}
