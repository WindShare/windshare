//go:build linux

package testtrace

import (
	"errors"
	"os"
	"slices"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOpenLinuxEventFileOwnsGrantedDescriptorBeforeEnvironmentCleanup(t *testing.T) {
	want := errors.New("unset failed")
	operations := validLinuxEventOperations()
	operations.unsetEnvironment = func(string) error { return want }
	var closed []int
	operations.closeDescriptor = func(descriptor int) error {
		closed = append(closed, descriptor)
		return nil
	}

	file, err := openLinuxEventFile(operations)
	if file != nil || !errors.Is(err, want) {
		t.Fatalf("open result file=%v err=%v", file, err)
	}
	if len(closed) != 1 || closed[0] != ownerEventFileDescriptor {
		t.Fatalf("closed descriptors = %v, want inherited descriptor", closed)
	}
}

func TestOpenLinuxEventFileRetiresDescriptorWhenInheritanceClearingFails(t *testing.T) {
	want := errors.New("close-on-exec update failed")
	operations := validLinuxEventOperations()
	operations.fcntlInt = func(_ uintptr, command int, _ int) (int, error) {
		switch command {
		case unix.F_GETFL:
			return unix.O_WRONLY, nil
		case unix.F_GETFD:
			return 0, nil
		case unix.F_SETFD:
			return 0, want
		default:
			return 0, errors.New("unexpected fcntl command")
		}
	}
	var closed []int
	operations.closeDescriptor = func(descriptor int) error {
		closed = append(closed, descriptor)
		return nil
	}

	file, err := openLinuxEventFile(operations)
	if file != nil || !errors.Is(err, want) {
		t.Fatalf("open result file=%v err=%v", file, err)
	}
	if len(closed) != 1 || closed[0] != ownerEventFileDescriptor {
		t.Fatalf("closed descriptors = %v, want inherited descriptor", closed)
	}
}

func TestOpenLinuxEventFileDoesNotCloseDescriptorFromInvalidToken(t *testing.T) {
	operations := validLinuxEventOperations()
	operations.lookupEnvironment = func(string) (string, bool) { return "07", true }
	unsetCalls := 0
	operations.unsetEnvironment = func(string) error {
		unsetCalls++
		return nil
	}
	closeCalls := 0
	operations.closeDescriptor = func(int) error {
		closeCalls++
		return nil
	}

	if file, err := openLinuxEventFile(operations); err == nil || file != nil {
		t.Fatalf("invalid token opened file=%v err=%v", file, err)
	}
	if unsetCalls != 1 || closeCalls != 0 {
		t.Fatalf("invalid token unset calls=%d close calls=%d", unsetCalls, closeCalls)
	}
}

func TestOpenLinuxEventFileTransfersPrivatizedDescriptorToFile(t *testing.T) {
	operations := validLinuxEventOperations()
	file, err := os.CreateTemp(t.TempDir(), "linux-event-endpoint-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	operations.newFile = func(uintptr, string) *os.File { return file }
	closeCalls := 0
	operations.closeDescriptor = func(int) error {
		closeCalls++
		return nil
	}
	closeOnExecCalls := 0
	operations.fcntlInt = func(_ uintptr, command int, _ int) (int, error) {
		switch command {
		case unix.F_GETFL:
			return unix.O_WRONLY, nil
		case unix.F_GETFD:
			return 0, nil
		case unix.F_SETFD:
			closeOnExecCalls++
			return 0, nil
		default:
			return 0, errors.New("unexpected fcntl command")
		}
	}

	opened, err := openLinuxEventFile(operations)
	if err != nil || opened != file {
		t.Fatalf("open result file=%v err=%v", opened, err)
	}
	if closeCalls != 0 || closeOnExecCalls != 1 {
		t.Fatalf("descriptor close calls=%d close-on-exec calls=%d", closeCalls, closeOnExecCalls)
	}
}

func TestOpenLinuxEventFileAcceptsDescendantDescriptorAndMakesItCloseOnExec(t *testing.T) {
	operations := validLinuxEventOperations()
	operations.lookupEnvironment = func(string) (string, bool) { return "3", true }
	var inspected []int
	operations.fstat = func(descriptor int, metadata *unix.Stat_t) error {
		inspected = append(inspected, descriptor)
		metadata.Mode = unix.S_IFIFO
		return nil
	}
	closeOnExecCalls := 0
	operations.fcntlInt = func(descriptor uintptr, command int, _ int) (int, error) {
		if int(descriptor) != descendantEventFileDescriptor {
			t.Fatalf("descriptor = %d, want %d", descriptor, descendantEventFileDescriptor)
		}
		switch command {
		case unix.F_GETFL:
			return unix.O_WRONLY, nil
		case unix.F_GETFD:
			return 0, nil
		case unix.F_SETFD:
			closeOnExecCalls++
			return 0, nil
		default:
			return 0, errors.New("unexpected fcntl command")
		}
	}
	file, err := os.CreateTemp(t.TempDir(), "linux-descendant-event-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	operations.newFile = func(uintptr, string) *os.File { return file }

	opened, err := openLinuxEventFile(operations)
	if err != nil || opened != file {
		t.Fatalf("open result file=%v err=%v", opened, err)
	}
	if !slices.Equal(inspected, []int{descendantEventFileDescriptor}) || closeOnExecCalls != 1 {
		t.Fatalf("inspected=%v close-on-exec=%d", inspected, closeOnExecCalls)
	}
}

func validLinuxEventOperations() linuxEventOperations {
	return linuxEventOperations{
		lookupEnvironment: func(string) (string, bool) { return "7", true },
		unsetEnvironment:  func(string) error { return nil },
		fstat: func(_ int, metadata *unix.Stat_t) error {
			metadata.Mode = unix.S_IFIFO
			return nil
		},
		fcntlInt: func(_ uintptr, command int, _ int) (int, error) {
			switch command {
			case unix.F_GETFL:
				return unix.O_WRONLY, nil
			case unix.F_GETFD, unix.F_SETFD:
				return 0, nil
			default:
				return 0, errors.New("unexpected fcntl command")
			}
		},
		closeDescriptor: func(int) error { return nil },
		newFile:         func(uintptr, string) *os.File { return nil },
	}
}
