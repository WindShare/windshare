//go:build windows

package testtrace

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/windows"
)

type windowsEventOperations struct {
	lookupEnvironment    func(string) (string, bool)
	unsetEnvironment     func(string) error
	getOSFileHandle      func(int) (windows.Handle, error)
	closeCRTDescriptor   func(int) error
	setHandleInformation func(windows.Handle, uint32, uint32) error
	getFileType          func(windows.Handle) (uint32, error)
	duplicateHandle      func(windows.Handle, windows.Handle, windows.Handle, *windows.Handle, uint32, bool, uint32) error
	currentProcess       func() windows.Handle
	closeHandle          func(windows.Handle) error
	newFile              func(uintptr, string) *os.File
}

func openEventFile() (*os.File, error) {
	return openWindowsEventFile(windowsEventOperations{
		lookupEnvironment:    os.LookupEnv,
		unsetEnvironment:     os.Unsetenv,
		getOSFileHandle:      windowsEventHandleFromCRTDescriptor,
		closeCRTDescriptor:   closeWindowsCRTDescriptor,
		setHandleInformation: windows.SetHandleInformation,
		getFileType:          windows.GetFileType,
		duplicateHandle:      windows.DuplicateHandle,
		currentProcess:       windows.CurrentProcess,
		closeHandle:          windows.CloseHandle,
		newFile:              os.NewFile,
	})
}

func openWindowsEventFile(operations windowsEventOperations) (_ *os.File, resultErr error) {
	if err := operations.validate(); err != nil {
		return nil, err
	}
	handleValue, handlePresent := operations.lookupEnvironment(EventHandleEnvironment)
	descriptorValue, descriptorPresent := operations.lookupEnvironment(EventFDEnvironment)
	if handlePresent && descriptorPresent {
		return nil, errors.Join(
			errors.New("private Windows test-event endpoint is ambiguous"),
			operations.unsetEnvironment(EventHandleEnvironment),
			operations.unsetEnvironment(EventFDEnvironment),
		)
	}
	if descriptorPresent {
		return openWindowsDescriptorEventFile(descriptorValue, operations)
	}
	return openWindowsInheritedEventFile(handleValue, handlePresent, operations)
}

func openWindowsDescriptorEventFile(
	value string,
	operations windowsEventOperations,
) (_ *os.File, resultErr error) {
	const descriptor = 3
	if value != strconv.Itoa(descriptor) {
		return nil, errors.Join(
			errors.New("private Windows test-event descriptor is unavailable"),
			operations.unsetEnvironment(EventFDEnvironment),
		)
	}
	original, err := operations.getOSFileHandle(descriptor)
	if err != nil || original == 0 || original == windows.InvalidHandle {
		return nil, errors.Join(
			errors.New("private Windows test-event descriptor is unavailable"),
			err,
			operations.unsetEnvironment(EventFDEnvironment),
		)
	}
	if err := operations.setHandleInformation(original, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		return nil, errors.Join(
			fmt.Errorf("make private Windows test-event descriptor non-inheritable: %w", err),
			operations.closeCRTDescriptor(descriptor),
			operations.unsetEnvironment(EventFDEnvironment),
		)
	}
	var duplicate windows.Handle
	if err := operations.duplicateHandle(
		operations.currentProcess(), original,
		operations.currentProcess(), &duplicate,
		windows.GENERIC_WRITE|windows.SYNCHRONIZE, false, 0,
	); err != nil {
		return nil, errors.Join(
			fmt.Errorf("authenticate private Windows test-event descriptor access: %w", err),
			operations.closeCRTDescriptor(descriptor),
			operations.unsetEnvironment(EventFDEnvironment),
		)
	}
	duplicateOwned := true
	defer func() {
		if duplicateOwned {
			resultErr = errors.Join(resultErr, operations.closeHandle(duplicate))
		}
	}()
	if err := operations.closeCRTDescriptor(descriptor); err != nil {
		return nil, errors.Join(
			fmt.Errorf("retire inherited Windows test-event descriptor: %w", err),
			operations.unsetEnvironment(EventFDEnvironment),
		)
	}
	if err := operations.unsetEnvironment(EventFDEnvironment); err != nil {
		return nil, fmt.Errorf("clear private Windows test-event descriptor: %w", err)
	}
	if err := validateEventPipeType(duplicate, operations.getFileType); err != nil {
		return nil, err
	}
	if err := operations.setHandleInformation(duplicate, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		return nil, fmt.Errorf("make adopted Windows test-event descriptor non-inheritable: %w", err)
	}
	file := operations.newFile(uintptr(duplicate), "windshare-test-event")
	if file == nil {
		return nil, errors.New("private Windows test-event descriptor is invalid")
	}
	duplicateOwned = false
	return file, nil
}

func openWindowsInheritedEventFile(
	value string,
	present bool,
	operations windowsEventOperations,
) (_ *os.File, resultErr error) {
	parsed, err := strconv.ParseUint(value, 10, strconv.IntSize)
	if !present || err != nil || parsed == 0 || uintptr(parsed) == ^uintptr(0) ||
		strconv.FormatUint(parsed, 10) != value {
		unavailable := errors.New("private Windows test-event handle is unavailable")
		if !present {
			return nil, unavailable
		}
		if err := operations.unsetEnvironment(EventHandleEnvironment); err != nil {
			return nil, errors.Join(
				unavailable,
				fmt.Errorf("clear invalid private Windows test-event handle: %w", err),
			)
		}
		return nil, unavailable
	}
	original := windows.Handle(uintptr(parsed))
	originalOwned := true
	defer func() {
		if originalOwned {
			if err := operations.closeHandle(original); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("close inherited Windows test-event handle: %w", err))
			}
		}
	}()
	if err := operations.unsetEnvironment(EventHandleEnvironment); err != nil {
		return nil, fmt.Errorf("clear private Windows test-event handle: %w", err)
	}
	if err := operations.setHandleInformation(original, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		return nil, fmt.Errorf("make private Windows test-event handle non-inheritable: %w", err)
	}
	if err := validateEventPipeType(original, operations.getFileType); err != nil {
		return nil, err
	}
	var duplicate windows.Handle
	currentProcess := operations.currentProcess()
	if err := operations.duplicateHandle(
		currentProcess, original,
		currentProcess, &duplicate,
		windows.GENERIC_WRITE|windows.SYNCHRONIZE, false, 0,
	); err != nil {
		return nil, fmt.Errorf("authenticate private Windows test-event write access: %w", err)
	}
	duplicateOwned := true
	defer func() {
		if duplicateOwned {
			if err := operations.closeHandle(duplicate); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("close adopted Windows test-event handle: %w", err))
			}
		}
	}()
	if err := operations.closeHandle(original); err != nil {
		return nil, fmt.Errorf("retire inherited Windows test-event handle: %w", err)
	}
	originalOwned = false
	// Duplication with explicit GENERIC_WRITE is the access authentication.
	// Re-running GetNamedPipeInfo on the restricted duplicate can itself require
	// metadata rights that a deliberately write-only endpoint no longer has.
	if err := validateEventPipeType(duplicate, operations.getFileType); err != nil {
		return nil, err
	}
	if err := operations.setHandleInformation(duplicate, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		return nil, fmt.Errorf("make adopted Windows test-event handle non-inheritable: %w", err)
	}
	file := operations.newFile(uintptr(duplicate), "windshare-test-event")
	if file == nil {
		return nil, errors.New("private Windows test-event handle is invalid")
	}
	duplicateOwned = false
	return file, nil
}

func (operations windowsEventOperations) validate() error {
	if operations.lookupEnvironment == nil || operations.unsetEnvironment == nil ||
		operations.getOSFileHandle == nil || operations.closeCRTDescriptor == nil ||
		operations.setHandleInformation == nil || operations.getFileType == nil ||
		operations.duplicateHandle == nil || operations.currentProcess == nil ||
		operations.closeHandle == nil || operations.newFile == nil {
		return errors.New("Windows test-event operations are incomplete")
	}
	return nil
}

var (
	getOSFileHandleProcedure = windows.NewLazySystemDLL("msvcrt.dll").NewProc("_get_osfhandle")
	closeDescriptorProcedure = windows.NewLazySystemDLL("msvcrt.dll").NewProc("_close")
)

func windowsEventHandleFromCRTDescriptor(descriptor int) (windows.Handle, error) {
	value, _, _ := getOSFileHandleProcedure.Call(uintptr(descriptor))
	if value == ^uintptr(0) {
		return 0, errors.New("resolve inherited Windows test-event CRT descriptor")
	}
	return windows.Handle(value), nil
}

func closeWindowsCRTDescriptor(descriptor int) error {
	result, _, _ := closeDescriptorProcedure.Call(uintptr(descriptor))
	if int32(result) == -1 {
		return errors.New("close inherited Windows test-event CRT descriptor")
	}
	return nil
}

func validateEventPipeType(
	handle windows.Handle,
	getFileType func(windows.Handle) (uint32, error),
) error {
	fileType, err := getFileType(handle)
	if err != nil {
		return fmt.Errorf("inspect private Windows test-event handle type: %w", err)
	}
	if fileType != windows.FILE_TYPE_PIPE {
		return errors.New("private Windows test-event handle must be a pipe")
	}
	return nil
}
