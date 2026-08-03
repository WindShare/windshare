//go:build linux

package testtrace

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

const (
	descendantEventFileDescriptor = 3
	ownerEventFileDescriptor      = 7
)

type linuxEventOperations struct {
	lookupEnvironment func(string) (string, bool)
	unsetEnvironment  func(string) error
	fstat             func(int, *unix.Stat_t) error
	fcntlInt          func(uintptr, int, int) (int, error)
	closeDescriptor   func(int) error
	newFile           func(uintptr, string) *os.File
}

func openEventFile() (*os.File, error) {
	return openLinuxEventFile(linuxEventOperations{
		lookupEnvironment: os.LookupEnv,
		unsetEnvironment:  os.Unsetenv,
		fstat:             unix.Fstat,
		fcntlInt:          unix.FcntlInt,
		closeDescriptor:   unix.Close,
		newFile:           os.NewFile,
	})
}

func openLinuxEventFile(operations linuxEventOperations) (_ *os.File, resultErr error) {
	if err := operations.validate(); err != nil {
		return nil, err
	}
	value, present := operations.lookupEnvironment(EventFDEnvironment)
	descriptor, valid := parseLinuxEventDescriptor(value)
	if !present || !valid {
		unavailable := errors.New("private Linux test-event descriptor is unavailable")
		if !present {
			return nil, unavailable
		}
		if err := operations.unsetEnvironment(EventFDEnvironment); err != nil {
			return nil, errors.Join(
				unavailable,
				fmt.Errorf("clear invalid private Linux test-event descriptor: %w", err),
			)
		}
		return nil, unavailable
	}

	descriptorOwned := true
	defer func() {
		if descriptorOwned {
			if err := operations.closeDescriptor(descriptor); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("close inherited Linux test-event descriptor: %w", err))
			}
		}
	}()
	if err := operations.unsetEnvironment(EventFDEnvironment); err != nil {
		return nil, fmt.Errorf("clear private Linux test-event descriptor: %w", err)
	}
	var metadata unix.Stat_t
	if err := operations.fstat(descriptor, &metadata); err != nil {
		return nil, fmt.Errorf("inspect private Linux test-event descriptor: %w", err)
	}
	if metadata.Mode&unix.S_IFMT != unix.S_IFIFO {
		return nil, errors.New("private Linux test-event descriptor must be a pipe")
	}
	statusFlags, err := operations.fcntlInt(uintptr(descriptor), unix.F_GETFL, 0)
	if err != nil {
		return nil, fmt.Errorf("inspect private Linux test-event access: %w", err)
	}
	if statusFlags&unix.O_ACCMODE != unix.O_WRONLY {
		return nil, errors.New("private Linux test-event descriptor must be write-only")
	}
	descriptorFlags, err := operations.fcntlInt(uintptr(descriptor), unix.F_GETFD, 0)
	if err != nil {
		return nil, fmt.Errorf("inspect private Linux test-event inheritance: %w", err)
	}
	if _, err := operations.fcntlInt(
		uintptr(descriptor), unix.F_SETFD, descriptorFlags|unix.FD_CLOEXEC,
	); err != nil {
		return nil, fmt.Errorf("make private Linux test-event descriptor close-on-exec: %w", err)
	}
	file := operations.newFile(uintptr(descriptor), "windshare-test-event")
	if file == nil {
		return nil, errors.New("private Linux test-event descriptor is invalid")
	}
	descriptorOwned = false
	return file, nil
}

func parseLinuxEventDescriptor(value string) (int, bool) {
	for _, descriptor := range [...]int{descendantEventFileDescriptor, ownerEventFileDescriptor} {
		if value == strconv.Itoa(descriptor) {
			return descriptor, true
		}
	}
	return 0, false
}

func (operations linuxEventOperations) validate() error {
	if operations.lookupEnvironment == nil || operations.unsetEnvironment == nil ||
		operations.fstat == nil || operations.fcntlInt == nil ||
		operations.closeDescriptor == nil || operations.newFile == nil {
		return errors.New("linux test-event operations are incomplete")
	}
	return nil
}
