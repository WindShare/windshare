//go:build windows

package testtrace

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"testing"

	"golang.org/x/sys/windows"
)

const (
	testOriginalEventHandle   windows.Handle = 41
	testDuplicateEventHandle  windows.Handle = 43
	testDescriptorEventHandle windows.Handle = 45
)

func TestOpenWindowsEventFileOwnsGrantedHandleBeforeEnvironmentCleanup(t *testing.T) {
	want := errors.New("unset failed")
	operations := validWindowsEventOperations()
	operations.unsetEnvironment = func(string) error { return want }
	var closed []windows.Handle
	operations.closeHandle = func(handle windows.Handle) error {
		closed = append(closed, handle)
		return nil
	}

	file, err := openWindowsEventFile(operations)
	if file != nil || !errors.Is(err, want) {
		t.Fatalf("open result file=%v err=%v", file, err)
	}
	if len(closed) != 1 || closed[0] != testOriginalEventHandle {
		t.Fatalf("closed handles = %v, want inherited handle", closed)
	}
}

func TestOpenWindowsEventFileRetiresHandleWhenInheritanceClearingFails(t *testing.T) {
	want := errors.New("inheritance update failed")
	operations := validWindowsEventOperations()
	operations.setHandleInformation = func(windows.Handle, uint32, uint32) error { return want }
	var closed []windows.Handle
	operations.closeHandle = func(handle windows.Handle) error {
		closed = append(closed, handle)
		return nil
	}

	file, err := openWindowsEventFile(operations)
	if file != nil || !errors.Is(err, want) {
		t.Fatalf("open result file=%v err=%v", file, err)
	}
	if len(closed) != 1 || closed[0] != testOriginalEventHandle {
		t.Fatalf("closed handles = %v, want inherited handle", closed)
	}
}

func TestOpenWindowsEventFileRetriesOriginalRetirementAfterCloseFailure(t *testing.T) {
	want := errors.New("first original close failed")
	operations := validWindowsEventOperations()
	closeCounts := make(map[windows.Handle]int)
	operations.closeHandle = func(handle windows.Handle) error {
		closeCounts[handle]++
		if handle == testOriginalEventHandle && closeCounts[handle] == 1 {
			return want
		}
		return nil
	}

	file, err := openWindowsEventFile(operations)
	if file != nil || !errors.Is(err, want) {
		t.Fatalf("open result file=%v err=%v", file, err)
	}
	if closeCounts[testOriginalEventHandle] != 2 || closeCounts[testDuplicateEventHandle] != 1 {
		t.Fatalf("close counts = %v, want original retry and duplicate retirement", closeCounts)
	}
}

func TestOpenWindowsEventFileDoesNotCloseHandleFromInvalidToken(t *testing.T) {
	operations := validWindowsEventOperations()
	operations.lookupEnvironment = func(name string) (string, bool) {
		if name == EventHandleEnvironment {
			return "0" + strconv.FormatUint(uint64(testOriginalEventHandle), 10), true
		}
		return "", false
	}
	unsetCalls := 0
	operations.unsetEnvironment = func(string) error {
		unsetCalls++
		return nil
	}
	closeCalls := 0
	operations.closeHandle = func(windows.Handle) error {
		closeCalls++
		return nil
	}

	if file, err := openWindowsEventFile(operations); err == nil || file != nil {
		t.Fatalf("invalid token opened file=%v err=%v", file, err)
	}
	if unsetCalls != 1 || closeCalls != 0 {
		t.Fatalf("invalid token unset calls=%d close calls=%d", unsetCalls, closeCalls)
	}
}

func TestOpenWindowsDescriptorEventFileDuplicatesBeforeClosingCRTDescriptor(t *testing.T) {
	operations := validWindowsDescriptorEventOperations()
	file, err := os.CreateTemp(t.TempDir(), "windows-descriptor-event-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	operations.newFile = func(uintptr, string) *os.File { return file }
	var steps []string
	operations.setHandleInformation = func(handle windows.Handle, _, _ uint32) error {
		steps = append(steps, fmt.Sprintf("private-%d", handle))
		return nil
	}
	operations.duplicateHandle = func(
		_ windows.Handle,
		source windows.Handle,
		_ windows.Handle,
		target *windows.Handle,
		_ uint32,
		inherit bool,
		_ uint32,
	) error {
		if source != testDescriptorEventHandle || inherit {
			t.Fatalf("duplicate source=%d inherit=%t", source, inherit)
		}
		steps = append(steps, "duplicate")
		*target = testDuplicateEventHandle
		return nil
	}
	operations.closeCRTDescriptor = func(descriptor int) error {
		steps = append(steps, fmt.Sprintf("close-crt-%d", descriptor))
		return nil
	}
	operations.unsetEnvironment = func(name string) error {
		steps = append(steps, "unset-"+name)
		return nil
	}

	opened, err := openWindowsEventFile(operations)
	if err != nil || opened != file {
		t.Fatalf("open result file=%v err=%v", opened, err)
	}
	want := []string{
		"private-45", "duplicate", "close-crt-3",
		"unset-" + EventFDEnvironment, "private-43",
	}
	if !slices.Equal(steps, want) {
		t.Fatalf("descriptor ownership steps = %v, want %v", steps, want)
	}
}

func TestOpenWindowsDescriptorEventFileRejectsWrongDescriptorWithoutClosingUnknownHandle(t *testing.T) {
	operations := validWindowsDescriptorEventOperations()
	operations.lookupEnvironment = func(name string) (string, bool) {
		if name == EventFDEnvironment {
			return "4", true
		}
		return "", false
	}
	closeCRTCalls := 0
	operations.closeCRTDescriptor = func(int) error {
		closeCRTCalls++
		return nil
	}

	if file, err := openWindowsEventFile(operations); err == nil || file != nil {
		t.Fatalf("wrong descriptor opened file=%v err=%v", file, err)
	}
	if closeCRTCalls != 0 {
		t.Fatalf("wrong descriptor closed %d CRT descriptors", closeCRTCalls)
	}
}

func TestOpenWindowsEventFileRejectsAmbiguousHandleAndDescriptor(t *testing.T) {
	operations := validWindowsDescriptorEventOperations()
	operations.lookupEnvironment = func(name string) (string, bool) {
		if name == EventHandleEnvironment {
			return strconv.FormatUint(uint64(testOriginalEventHandle), 10), true
		}
		if name == EventFDEnvironment {
			return "3", true
		}
		return "", false
	}
	var unset []string
	operations.unsetEnvironment = func(name string) error {
		unset = append(unset, name)
		return nil
	}

	if file, err := openWindowsEventFile(operations); err == nil || file != nil {
		t.Fatalf("ambiguous endpoint opened file=%v err=%v", file, err)
	}
	if !slices.Equal(unset, []string{EventHandleEnvironment, EventFDEnvironment}) {
		t.Fatalf("ambiguous endpoint unset = %v", unset)
	}
}

func TestOpenWindowsDescriptorEventFileRejectsNonPipeAfterRetiringCRTDescriptor(t *testing.T) {
	operations := validWindowsDescriptorEventOperations()
	operations.getFileType = func(windows.Handle) (uint32, error) { return windows.FILE_TYPE_DISK, nil }
	closeCRTCalls := 0
	operations.closeCRTDescriptor = func(int) error {
		closeCRTCalls++
		return nil
	}
	var closed []windows.Handle
	operations.closeHandle = func(handle windows.Handle) error {
		closed = append(closed, handle)
		return nil
	}

	if file, err := openWindowsEventFile(operations); err == nil || file != nil {
		t.Fatalf("non-pipe descriptor opened file=%v err=%v", file, err)
	}
	if closeCRTCalls != 1 || !slices.Equal(closed, []windows.Handle{testDuplicateEventHandle}) {
		t.Fatalf("non-pipe retirement closeCRT=%d handles=%v", closeCRTCalls, closed)
	}
}

func TestOpenWindowsEventFileTransfersOnlyRestrictedDuplicateToFile(t *testing.T) {
	operations := validWindowsEventOperations()
	file, err := os.CreateTemp(t.TempDir(), "windows-event-endpoint-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	operations.newFile = func(uintptr, string) *os.File { return file }
	var closed []windows.Handle
	operations.closeHandle = func(handle windows.Handle) error {
		closed = append(closed, handle)
		return nil
	}
	var inheritanceCleared []windows.Handle
	operations.setHandleInformation = func(handle windows.Handle, _, _ uint32) error {
		inheritanceCleared = append(inheritanceCleared, handle)
		return nil
	}

	opened, err := openWindowsEventFile(operations)
	if err != nil || opened != file {
		t.Fatalf("open result file=%v err=%v", opened, err)
	}
	if len(closed) != 1 || closed[0] != testOriginalEventHandle {
		t.Fatalf("closed handles = %v, want only inherited handle", closed)
	}
	if len(inheritanceCleared) != 2 || inheritanceCleared[0] != testOriginalEventHandle ||
		inheritanceCleared[1] != testDuplicateEventHandle {
		t.Fatalf("inheritance-cleared handles = %v", inheritanceCleared)
	}
}

func validWindowsEventOperations() windowsEventOperations {
	return windowsEventOperations{
		lookupEnvironment: func(name string) (string, bool) {
			if name == EventHandleEnvironment {
				return strconv.FormatUint(uint64(testOriginalEventHandle), 10), true
			}
			return "", false
		},
		unsetEnvironment:     func(string) error { return nil },
		getOSFileHandle:      func(int) (windows.Handle, error) { return 0, errors.New("unexpected CRT descriptor") },
		closeCRTDescriptor:   func(int) error { return errors.New("unexpected CRT descriptor close") },
		setHandleInformation: func(windows.Handle, uint32, uint32) error { return nil },
		getFileType:          func(windows.Handle) (uint32, error) { return windows.FILE_TYPE_PIPE, nil },
		duplicateHandle: func(
			_ windows.Handle,
			_ windows.Handle,
			_ windows.Handle,
			target *windows.Handle,
			_ uint32,
			_ bool,
			_ uint32,
		) error {
			*target = testDuplicateEventHandle
			return nil
		},
		currentProcess: func() windows.Handle { return 1 },
		closeHandle:    func(windows.Handle) error { return nil },
		newFile:        func(uintptr, string) *os.File { return nil },
	}
}

func validWindowsDescriptorEventOperations() windowsEventOperations {
	operations := validWindowsEventOperations()
	operations.lookupEnvironment = func(name string) (string, bool) {
		if name == EventFDEnvironment {
			return "3", true
		}
		return "", false
	}
	operations.getOSFileHandle = func(descriptor int) (windows.Handle, error) {
		if descriptor != 3 {
			return 0, fmt.Errorf("unexpected descriptor %d", descriptor)
		}
		return testDescriptorEventHandle, nil
	}
	operations.closeCRTDescriptor = func(int) error { return nil }
	return operations
}
