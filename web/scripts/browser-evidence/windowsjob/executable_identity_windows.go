//go:build windows

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"io"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	maximumAuthenticatedExecutableBytes         = 128 << 20
	compareStringEqual                  uintptr = 2
)

func openAuthenticatedExecutable(path, expectedSHA256 string) (*os.File, error) {
	if expectedSHA256 == "" {
		return nil, nil
	}
	encodedPath, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		encodedPath,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("wrap authenticated executable handle")
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if information.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return nil, errors.Join(errors.New("authenticated executable is not a regular file"), file.Close())
	}
	digest := sha256.New()
	written, err := io.Copy(digest, io.LimitReader(file, maximumAuthenticatedExecutableBytes+1))
	if err != nil || written < 1 || written > maximumAuthenticatedExecutableBytes {
		return nil, errors.Join(errors.New("authenticated executable size is outside authority"), err, file.Close())
	}
	if hex.EncodeToString(digest.Sum(nil)) != expectedSHA256 {
		return nil, errors.Join(errors.New("authenticated executable digest differs"), file.Close())
	}
	return file, nil
}

func validateWindowsEnvironment(environment []environmentEntry) error {
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
