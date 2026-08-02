//go:build linux

package linuxsubreaper

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func validatePipeDescriptor(descriptor, accessMode int, label string) error {
	var metadata unix.Stat_t
	if err := unix.Fstat(descriptor, &metadata); err != nil {
		return fmt.Errorf("inspect %s descriptor: %w", label, err)
	}
	if metadata.Mode&unix.S_IFMT != unix.S_IFIFO {
		return fmt.Errorf("%s descriptor is not an anonymous pipe", label)
	}
	flags, err := unix.FcntlInt(uintptr(descriptor), unix.F_GETFL, 0)
	if err != nil {
		return fmt.Errorf("inspect %s descriptor flags: %w", label, err)
	}
	if flags&unix.O_ACCMODE != accessMode {
		return fmt.Errorf("%s descriptor has the wrong access direction", label)
	}
	return nil
}

func validatePathDescriptor(descriptor int, label string) error {
	return validatePathDescriptorKind(descriptor, unix.S_IFREG, "regular file", label)
}

func validateDirectoryPathDescriptor(descriptor int, label string) error {
	return validatePathDescriptorKind(descriptor, unix.S_IFDIR, "directory", label)
}

func validatePathDescriptorKind(descriptor int, kind uint32, kindLabel, label string) error {
	var metadata unix.Stat_t
	if err := unix.Fstat(descriptor, &metadata); err != nil {
		return fmt.Errorf("inspect %s descriptor: %w", label, err)
	}
	if metadata.Mode&unix.S_IFMT != kind {
		return fmt.Errorf("%s descriptor is not a %s", label, kindLabel)
	}
	flags, err := unix.FcntlInt(uintptr(descriptor), unix.F_GETFL, 0)
	if err != nil {
		return fmt.Errorf("inspect %s descriptor flags: %w", label, err)
	}
	if flags&unix.O_PATH == 0 {
		return fmt.Errorf("%s descriptor does not carry path-only execution authority", label)
	}
	return nil
}

func validateNullDescriptor(descriptor int, label string) error {
	var actual unix.Stat_t
	if err := unix.Fstat(descriptor, &actual); err != nil {
		return fmt.Errorf("inspect %s descriptor: %w", label, err)
	}
	var expected unix.Stat_t
	if err := unix.Stat(os.DevNull, &expected); err != nil {
		return fmt.Errorf("inspect null device identity: %w", err)
	}
	if actual.Dev != expected.Dev || actual.Ino != expected.Ino || actual.Rdev != expected.Rdev {
		return fmt.Errorf("%s descriptor is not the null device", label)
	}
	flags, err := unix.FcntlInt(uintptr(descriptor), unix.F_GETFL, 0)
	if err != nil {
		return fmt.Errorf("inspect %s descriptor flags: %w", label, err)
	}
	if flags&unix.O_ACCMODE != unix.O_WRONLY {
		return fmt.Errorf("%s descriptor is not write-only", label)
	}
	return nil
}

func setDescriptorInherited(descriptor int, inherited bool, label string) error {
	flags, err := unix.FcntlInt(uintptr(descriptor), unix.F_GETFD, 0)
	if err != nil {
		return fmt.Errorf("inspect %s inheritance: %w", label, err)
	}
	if inherited {
		flags &^= unix.FD_CLOEXEC
	} else {
		flags |= unix.FD_CLOEXEC
	}
	if _, err := unix.FcntlInt(uintptr(descriptor), unix.F_SETFD, flags); err != nil {
		return fmt.Errorf("set %s inheritance: %w", label, err)
	}
	return nil
}

func validateSupervisorDescriptors() error {
	return errors.Join(
		validatePipeDescriptor(0, unix.O_RDONLY, "request"),
		validatePipeDescriptor(statusDescriptor, unix.O_WRONLY, "status"),
		validatePipeDescriptor(controlDescriptor, unix.O_RDONLY, "control"),
		validatePipeDescriptor(childInputDescriptor, unix.O_RDONLY, "child input"),
	)
}

func validateExecChildDescriptors() error {
	return errors.Join(
		validatePipeDescriptor(0, unix.O_RDONLY, "target input"),
		validatePipeDescriptor(3, unix.O_RDONLY, "exec-gate metadata"),
		validatePipeDescriptor(4, unix.O_WRONLY, "exec-gate readiness"),
		validatePipeDescriptor(5, unix.O_RDONLY, "exec-gate release"),
		validatePathDescriptor(targetExecutableDescriptor, "exec-gate target"),
		validatePipeDescriptor(execResultDescriptor, unix.O_WRONLY, "exec-gate result"),
		validateDirectoryPathDescriptor(targetDirectoryDescriptor, "exec-gate working directory"),
	)
}
