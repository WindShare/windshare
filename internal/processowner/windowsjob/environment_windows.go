//go:build windows

package windowsjob

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
	"golang.org/x/sys/windows"
)

const compareStringEqual uintptr = 2

func validateWindowsEnvironment(environment []ownerprotocol.EnvironmentEntry) error {
	for right := 1; right < len(environment); right++ {
		for left := 0; left < right; left++ {
			equal, err := windowsEnvironmentNamesEqual(environment[left].Name, environment[right].Name)
			if err != nil {
				return err
			}
			if equal {
				return fmt.Errorf("environment contains Windows-case-insensitive duplicate %q", environment[right].Name)
			}
		}
	}
	return nil
}

func windowsEnvironmentNamesEqual(left, right string) (bool, error) {
	leftUTF16, err := windows.UTF16FromString(left)
	if err != nil {
		return false, fmt.Errorf("encode environment name %q: %w", left, err)
	}
	rightUTF16, err := windows.UTF16FromString(right)
	if err != nil {
		return false, fmt.Errorf("encode environment name %q: %w", right, err)
	}
	result, _, callErr := compareStringOrdinalProcedure.Call(
		uintptr(unsafe.Pointer(&leftUTF16[0])),
		uintptr(len(leftUTF16)-1),
		uintptr(unsafe.Pointer(&rightUTF16[0])),
		uintptr(len(rightUTF16)-1),
		1,
	)
	runtime.KeepAlive(leftUTF16)
	runtime.KeepAlive(rightUTF16)
	if result == 0 {
		if callErr == nil || errors.Is(callErr, windows.ERROR_SUCCESS) {
			callErr = syscall.EINVAL
		}
		return false, fmt.Errorf("compare Windows environment names: %w", callErr)
	}
	return result == compareStringEqual, nil
}
