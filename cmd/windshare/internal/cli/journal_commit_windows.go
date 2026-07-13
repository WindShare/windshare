//go:build windows

package cli

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	moveFileReplaceExisting = 0x1
	moveFileWriteThrough    = 0x8
)

var moveFileExW = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

func installJournal(tempPath, targetPath string) error {
	temp, err := syscall.UTF16PtrFromString(tempPath)
	if err != nil {
		return fmt.Errorf("encode temporary path: %w", err)
	}
	target, err := syscall.UTF16PtrFromString(targetPath)
	if err != nil {
		return fmt.Errorf("encode target path: %w", err)
	}

	// Windows rejects FlushFileBuffers on directory handles. A write-through
	// rename provides the equivalent durability boundary without pretending
	// that an unsupported directory sync succeeded.
	result, _, callErr := moveFileExW.Call(
		uintptr(unsafe.Pointer(temp)),
		uintptr(unsafe.Pointer(target)),
		moveFileReplaceExisting|moveFileWriteThrough,
	)
	runtime.KeepAlive(temp)
	runtime.KeepAlive(target)
	if result == 0 {
		if errors.Is(callErr, syscall.Errno(0)) {
			callErr = syscall.EINVAL
		}
		return callErr
	}
	return nil
}
