//go:build windows

package outputwindows

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"

	"golang.org/x/sys/windows"
)

func (directory *windowsDirectory) createExactFile(relative string, private bool, size int64) (*windowsFile, error) {
	var descriptor *windows.SECURITY_DESCRIPTOR
	if private {
		var err error
		descriptor, err = directory.policy.descriptor(false)
		if err != nil {
			return nil, windowsFailure("prepare private file ACL", relative, errWindowsOutputUnsafe, err)
		}
	}
	return directory.createFile(relative, windowsPrivateFileAccess(), descriptor, private, size)
}

func (directory *windowsDirectory) createFile(
	relative string,
	access uint32,
	descriptor *windows.SECURITY_DESCRIPTOR,
	private bool,
	size int64,
) (_ *windowsFile, resultErr error) {
	const operation = "create output file"
	if err := directory.usable(); err != nil {
		return nil, err
	}
	if size < 0 {
		return nil, windowsFailure(operation, relative, errWindowsOutputUnsafe, errors.New("file size cannot be negative"))
	}
	native, err := windowsRelativePath(relative, private)
	if err != nil {
		return nil, windowsFailure(operation, relative, errWindowsOutputUnsafe, err)
	}
	// Until initialization commits, the kernel owns rollback through this exact
	// creation handle. Verification failure must not force name-based cleanup or
	// leave an untracked file when the caller receives no usable capability.
	handle, status, err := windowsOpenNative(
		directory.handle(), native, access, windows.FILE_CREATE,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_DELETE_ON_CLOSE,
		windows.FILE_ATTRIBUTE_NORMAL, descriptor,
	)
	runtime.KeepAlive(descriptor)
	if err != nil {
		return nil, windowsNativeNoReplaceFailure(operation, relative, err)
	}
	wrapped := os.NewFile(uintptr(handle), relative)
	if wrapped == nil {
		return nil, errors.Join(
			windowsFailure(operation, relative, errWindowsOutputUnsafe, errors.New("wrap file handle")),
			windows.CloseHandle(handle), directory.Sync(),
		)
	}
	file := &windowsFile{
		file: wrapped, path: filepath.Join(directory.path, relative), volume: directory.volume,
		inspector: directory.inspector, policy: directory.policy,
	}
	committed := false
	defer func() {
		if !committed {
			resultErr = errors.Join(resultErr, file.Close(), directory.Sync())
		}
	}()
	if _, err := windowsCreationStatus(windows.FILE_CREATE, status); err != nil {
		return nil, windowsFailure(operation, relative, errWindowsOutputUnsafe, err)
	}
	if err := file.verify(private); err != nil {
		return nil, err
	}
	if err := windowsVerifyOpenedLeafAuthority(file.handle(), native, private); err != nil {
		return nil, err
	}
	if err := file.Truncate(size); err != nil {
		return nil, err
	}
	actual, err := file.Size()
	if err != nil || actual != uint64(size) {
		return nil, windowsFailure(operation, relative, errWindowsOutputUnsafe,
			errors.Join(errors.New("file creation did not install the exact size"), err))
	}
	if err := windowsCommitDeleteOnClose(file.handle()); err != nil {
		return nil, windowsNativeOperationFailure("commit output file creation", relative, err)
	}
	committed = true
	return file, nil
}
