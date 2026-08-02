//go:build linux

package linuxsubreaper

import (
	"errors"
	"os"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
	"golang.org/x/sys/unix"
)

const eventDescriptor = 6

func runSupervise(arguments []string) error {
	if len(arguments) != 1 || arguments[0] != commandSupervise {
		return errors.New("linux process owner requires supervise")
	}
	statusFile := os.NewFile(statusDescriptor, "process-owner-status")
	controlFile := os.NewFile(controlDescriptor, "process-owner-control")
	childInputFile := os.NewFile(childInputDescriptor, "process-owner-child-input")
	if statusFile == nil || controlFile == nil || childInputFile == nil {
		return errors.New("linux process owner requires status, control, and child-input pipes")
	}
	defer statusFile.Close()
	defer controlFile.Close()
	defer childInputFile.Close()
	if err := validateSupervisorDescriptors(); err != nil {
		return err
	}
	if err := errors.Join(
		setDescriptorInherited(statusDescriptor, false, "status"),
		setDescriptorInherited(controlDescriptor, false, "control"),
		setDescriptorInherited(childInputDescriptor, false, "child input"),
	); err != nil {
		return err
	}

	request, err := ownerprotocol.ReadDocument[ownerprotocol.Request](os.Stdin)
	if err != nil {
		return err
	}
	if err := ownerprotocol.ValidateRequest(request); err != nil {
		return err
	}
	eventFile, err := inheritedEventFile()
	if err != nil {
		return err
	}
	if eventFile == nil {
		return errors.New("linux process owner requires a test-event pipe")
	}
	defer eventFile.Close()
	starts, err := inheritedStartGate()
	if err != nil {
		return err
	}
	defer starts.close()
	settlement := supervise(request, controlFile, childInputFile, eventFile, starts)
	if err := validateSettlement(settlement, request); err != nil {
		return err
	}
	return ownerprotocol.WriteLineDocument(statusFile, settlement)
}

func inheritedEventFile() (*os.File, error) {
	file := os.NewFile(eventDescriptor, "process-owner-test-event")
	if file == nil {
		return nil, errors.New("linux test-event descriptor is invalid")
	}
	if err := validatePipeDescriptor(eventDescriptor, unix.O_WRONLY, "test event"); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := setDescriptorInherited(eventDescriptor, false, "test event"); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}
