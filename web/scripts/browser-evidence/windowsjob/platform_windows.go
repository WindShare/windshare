//go:build windows

package main

import (
	"errors"
	"io"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

func runSupervisorPlatform(
	request startRequest,
	statusPath string,
	controlPath string,
	rawInput io.Reader,
) error {
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
	var rawInputFile *os.File
	if request.Stdin == nil {
		if err := requireExactRawEOF(rawInput); err != nil {
			return err
		}
	} else {
		var ok bool
		rawInputFile, ok = rawInput.(*os.File)
		if !ok {
			return errors.New("raw stdin authority must be an inherited anonymous file handle")
		}
	}
	launcher, err := startAssignedLauncher(job, request, rawInputFile)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(launcher.membershipHandle)
	defer launcher.eventReader.Close()
	defer launcher.input.Close()
	launcherEventChannel := make(chan launcherEventResult, 1)
	go func() {
		event, eventErr := readLauncherEvent(launcher.eventReader)
		launcherEventChannel <- launcherEventResult{event: event, err: eventErr}
	}()

	for {
		select {
		case eventResult := <-launcherEventChannel:
			if eventResult.err != nil {
				return terminateAfterAuthorityFailure(job, request, eventResult.err)
			}
			return superviseLaunchedTree(job, request, statusPath, eventResult.event, deadline, controls, launcher)
		case control := <-controls:
			if control.err != nil {
				return terminateAfterAuthorityFailure(job, request, control.err)
			}
			select {
			case eventResult := <-launcherEventChannel:
				if eventResult.err != nil {
					return terminateAfterAuthorityFailure(job, request, eventResult.err)
				}
				replayedControl := make(chan controlResult, 1)
				replayedControl <- control
				return superviseLaunchedTree(job, request, statusPath, eventResult.event, deadline, replayedControl, launcher)
			default:
			}
			return terminatePendingLaunch(job, request, statusPath, false, launcherEventChannel, launcher.wait)
		case <-deadline.C:
			select {
			case eventResult := <-launcherEventChannel:
				if eventResult.err != nil {
					return terminateAfterAuthorityFailure(job, request, eventResult.err)
				}
				immediateDeadline := time.NewTimer(0)
				defer immediateDeadline.Stop()
				return superviseLaunchedTree(job, request, statusPath, eventResult.event, immediateDeadline, controls, launcher)
			default:
			}
			return terminatePendingLaunch(job, request, statusPath, true, launcherEventChannel, launcher.wait)
		}
	}
}
