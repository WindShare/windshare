//go:build windows

package perfevidence

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// isReparsePointInfo treats junctions and cloud placeholders as links even
// when the Go mode bits do not expose ModeSymlink. Cleanup and source
// inventory must never descend through one of those objects.
func isReparsePointInfo(info os.FileInfo) bool {
	if info == nil || info.Mode()&os.ModeSymlink != 0 {
		return true
	}
	native, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && native.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}
func syncWindowsDirectoryContents(
	parent windows.Handle,
	relative string,
	transition func(string, string) error,
	meter *evidenceStoreMeter,
) error {
	entries, err := readWindowsDirectory(parent, relative)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		childRelative := filepath.ToSlash(filepath.Join(relative, entry.Name()))
		handle, err := openWindowsRelativeAccess(
			parent, entry.Name(), windows.FILE_OPEN,
			windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			windows.FILE_GENERIC_READ|windows.FILE_READ_ATTRIBUTES|windows.FILE_WRITE_ATTRIBUTES|windows.SYNCHRONIZE,
		)
		if err != nil {
			return err
		}
		var opened windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(handle, &opened); err != nil {
			return errors.Join(err, windows.CloseHandle(handle))
		}
		if opened.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return errors.Join(
				fmt.Errorf("refusing to sync reparse-point artifact %s", childRelative),
				windows.CloseHandle(handle),
			)
		}
		if opened.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
			if err := meter.observeDirectory(childRelative, evidenceRelativeDepth(childRelative)); err != nil {
				return errors.Join(err, windows.CloseHandle(handle))
			}
			var syncErr error
			if transition != nil {
				syncErr = transition(childRelative, "directory-opened")
			}
			if syncErr == nil {
				syncErr = syncWindowsDirectoryContents(handle, childRelative, transition, meter)
			}
			closeErr := windows.CloseHandle(handle)
			if err := errors.Join(syncErr, closeErr, verifyWindowsEntryIdentity(parent, entry.Name(), opened)); err != nil {
				return err
			}
			continue
		}
		var openedSize int64 = int64(opened.FileSizeHigh)<<32 | int64(opened.FileSizeLow)
		if err := meter.observeFile(
			childRelative, evidenceRelativeDepth(childRelative), openedSize, evidenceArtifactFile,
		); err != nil {
			return errors.Join(err, windows.CloseHandle(handle))
		}
		if transition != nil {
			if err := transition(childRelative, "file-opened"); err != nil {
				return errors.Join(err, windows.CloseHandle(handle))
			}
		}
		syncErr := syncWindowsRegularFile(parent, entry.Name(), handle, opened)
		closeErr := windows.CloseHandle(handle)
		if err := errors.Join(syncErr, closeErr, verifyWindowsEntryIdentity(parent, entry.Name(), opened)); err != nil {
			return err
		}
	}
	return nil
}

func syncWindowsRegularFile(
	parent windows.Handle,
	name string,
	attributeHandle windows.Handle,
	opened windows.ByHandleFileInformation,
) error {
	var original windowsFileBasicInfo
	if err := windows.GetFileInformationByHandleEx(
		attributeHandle, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&original)), uint32(unsafe.Sizeof(original)),
	); err != nil {
		return err
	}
	writable := original
	writable.attributes &^= windows.FILE_ATTRIBUTE_READONLY
	if writable.attributes == 0 {
		writable.attributes = windows.FILE_ATTRIBUTE_NORMAL
	}
	if err := setWindowsFileBasicInfo(attributeHandle, writable); err != nil {
		return err
	}
	writeHandle, openErr := openWindowsRelativeAccess(
		parent, name, windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.SYNCHRONIZE,
	)
	if openErr == nil {
		var writeInfo windows.ByHandleFileInformation
		openErr = windows.GetFileInformationByHandle(writeHandle, &writeInfo)
		if openErr == nil && windowsFileIdentity(writeInfo) != windowsFileIdentity(opened) {
			openErr = errors.New("artifact changed before handle-relative sync")
		}
	}
	var syncErr, closeErr error
	if openErr == nil {
		syncErr = windows.FlushFileBuffers(writeHandle)
	}
	if writeHandle != windows.InvalidHandle {
		closeErr = windows.CloseHandle(writeHandle)
	}
	restoreErr := setWindowsFileBasicInfo(attributeHandle, original)
	return errors.Join(openErr, syncErr, closeErr, restoreErr)
}

func setWindowsFileBasicInfo(handle windows.Handle, info windowsFileBasicInfo) error {
	return windows.SetFileInformationByHandle(
		handle, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)),
	)
}

func verifyWindowsEntryIdentity(
	parent windows.Handle,
	name string,
	expected windows.ByHandleFileInformation,
) error {
	handle, err := openWindowsRelativeAccess(
		parent, name, windows.FILE_OPEN,
		windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
	)
	if err != nil {
		return err
	}
	var observed windows.ByHandleFileInformation
	observeErr := windows.GetFileInformationByHandle(handle, &observed)
	if observeErr != nil {
		return errors.Join(observeErr, windows.CloseHandle(handle))
	}
	if observed.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		windowsFileIdentity(observed) != windowsFileIdentity(expected) {
		return errors.Join(
			errors.New("filesystem entry changed during handle-relative traversal"),
			windows.CloseHandle(handle),
		)
	}
	return windows.CloseHandle(handle)
}

func windowsFileIdentity(info windows.ByHandleFileInformation) directoryIdentity {
	return directoryIdentity{
		volume: uint64(info.VolumeSerialNumber),
		object: uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
	}
}

func (authority *outputRootAuthority) readDir() ([]os.DirEntry, error) {
	if err := authority.verifyPath(); err != nil {
		return nil, err
	}
	entries, err := readWindowsDirectory(authority.handle, authority.path)
	if err != nil {
		return nil, err
	}
	meter := defaultEvidenceStoreMeter()
	if err := meter.observeRootEntries(len(entries)); err != nil {
		return nil, err
	}
	return entries, nil
}

func (authority *outputRootAuthority) removeChild(name string, transition func(string) error) error {
	if authority == nil || authority.handle == windows.InvalidHandle {
		return errors.New("evidence output authority is closed")
	}
	return removeWindowsEntryAt(authority.handle, name, name, transition)
}

func openWindowsRelative(
	parent windows.Handle,
	name string,
	disposition uint32,
	options uint32,
) (windows.Handle, error) {
	return openWindowsRelativeShared(
		parent, name, disposition, options,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
	)
}

func openWindowsRelativeShared(
	parent windows.Handle,
	name string,
	disposition uint32,
	options uint32,
	share uint32,
) (windows.Handle, error) {
	return openWindowsRelativeAccess(
		parent, name, disposition, options, share,
		windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|
			windows.FILE_WRITE_ATTRIBUTES|windows.DELETE|windows.SYNCHRONIZE,
	)
}

func openWindowsRelativeAccess(
	parent windows.Handle,
	name string,
	disposition uint32,
	options uint32,
	share uint32,
	access uint32,
) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windows.InvalidHandle, err
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: parent, ObjectName: objectName,
	}
	var status windows.IO_STATUS_BLOCK
	allocationSize := int64(0)
	handle := windows.InvalidHandle
	err = windows.NtCreateFile(
		&handle,
		access,
		&attributes,
		&status,
		&allocationSize,
		windows.FILE_ATTRIBUTE_NORMAL,
		share,
		disposition,
		options,
		0,
		0,
	)
	return handle, err
}

func removeWindowsEntryAt(
	parent windows.Handle,
	name string,
	relative string,
	transition func(string) error,
) error {
	for range cleanupMutationLimit {
		handle, err := openWindowsRelative(
			parent, name, windows.FILE_OPEN,
			windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		)
		if windowsPathAbsent(err) {
			return nil
		}
		if err != nil {
			return err
		}
		var info windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
			return errors.Join(err, windows.CloseHandle(handle))
		}
		if transition != nil {
			if err := transition(relative); err != nil {
				return errors.Join(err, windows.CloseHandle(handle))
			}
		}
		realDirectory := info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 &&
			info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0
		if realDirectory {
			if err := emptyWindowsDirectory(handle, relative, transition); err != nil {
				return errors.Join(err, windows.CloseHandle(handle))
			}
		}
		deleteErr := markWindowsHandleForDeletion(handle)
		closeErr := windows.CloseHandle(handle)
		if windowsDirectoryNotEmpty(deleteErr) && closeErr == nil {
			continue
		}
		if err := errors.Join(deleteErr, closeErr); err != nil {
			return err
		}
	}
	return fmt.Errorf("directory entry %s kept changing during handle-relative cleanup", relative)
}

func emptyWindowsDirectory(
	handle windows.Handle,
	relative string,
	transition func(string) error,
) error {
	entries, err := readWindowsDirectory(handle, relative)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		child := filepath.ToSlash(filepath.Join(relative, entry.Name()))
		if err := removeWindowsEntryAt(handle, entry.Name(), child, transition); err != nil {
			return err
		}
	}
	return nil
}

func readWindowsDirectory(handle windows.Handle, name string) ([]os.DirEntry, error) {
	enumeration, err := reopenWindowsDirectory(handle)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(enumeration), name)
	maximumEntries := DefaultEvidenceStoreBudget().MaxObjects
	var entries []os.DirEntry
	var readErr error
	for {
		batch, err := file.ReadDir(evidenceStoreReadBatch)
		if len(batch) > maximumEntries-len(entries) {
			readErr = fmt.Errorf("evidence directory %s exceeds %d entries", name, maximumEntries)
			break
		}
		entries = append(entries, batch...)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			readErr = err
			break
		}
	}
	return entries, errors.Join(readErr, file.Close())
}

func reopenWindowsDirectory(handle windows.Handle) (windows.Handle, error) {
	const maximumNTPathCharacters = 32_768
	path := make([]uint16, maximumNTPathCharacters)
	length, err := windows.GetFinalPathNameByHandle(handle, &path[0], uint32(len(path)), 0)
	if err != nil {
		return windows.InvalidHandle, err
	}
	if length == 0 || length >= uint32(len(path)) {
		return windows.InvalidHandle, errors.New("directory authority path exceeded the NT path limit")
	}
	reopened, err := openWindowsDirectory(
		windows.UTF16ToString(path[:length]),
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
	)
	if err != nil {
		return windows.InvalidHandle, err
	}
	expected, expectedErr := windowsDirectoryIdentity(handle)
	observed, observedErr := windowsDirectoryIdentity(reopened)
	if err := errors.Join(expectedErr, observedErr); err != nil {
		return windows.InvalidHandle, errors.Join(err, windows.CloseHandle(reopened))
	}
	if expected != observed {
		return windows.InvalidHandle, errors.Join(
			errors.New("directory changed while reopening its enumeration handle"),
			windows.CloseHandle(reopened),
		)
	}
	return reopened, nil
}

func windowsDirectoryNotEmpty(err error) bool {
	if status, ok := errors.AsType[windows.NTStatus](err); ok {
		err = status.Errno()
	}
	return errors.Is(err, windows.ERROR_DIR_NOT_EMPTY)
}

func markWindowsHandleForDeletion(handle windows.Handle) error {
	flags := uint32(
		windows.FILE_DISPOSITION_DELETE |
			windows.FILE_DISPOSITION_POSIX_SEMANTICS |
			windows.FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE,
	)
	return windows.SetFileInformationByHandle(
		handle, windows.FileDispositionInfoEx, (*byte)(unsafe.Pointer(&flags)), uint32(unsafe.Sizeof(flags)),
	)
}

func windowsPathAbsent(err error) bool {
	if err == nil {
		return false
	}
	if status, ok := errors.AsType[windows.NTStatus](err); ok {
		err = status.Errno()
	}
	return errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}

func authorityChildAbsent(err error) bool {
	return windowsPathAbsent(err)
}

func platformPathKey(path string) string {
	// Preserve path spelling because Windows directories can opt into
	// case-sensitive lookup. Lower-casing would collapse distinct compiled
	// inputs; existing aliases are compared by file identity in samePath.
	clean := filepath.Clean(path)
	if len(clean) >= 2 && clean[1] == ':' {
		clean = strings.ToUpper(clean[:1]) + clean[1:]
	}
	return clean
}

func platformPathAlias(left, right string) bool {
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	if leftErr == nil && rightErr == nil {
		// SameFile is the authority when a case-sensitive directory contains
		// two spellings that differ only by case; EqualFold alone would merge
		// those distinct objects.
		return os.SameFile(leftInfo, rightInfo)
	}
	return false
}

type memoryStatusEx struct {
	length            uint32
	memoryLoad        uint32
	totalPhysical     uint64
	availablePhysical uint64
	totalPageFile     uint64
	availablePageFile uint64
	totalVirtual      uint64
	availableVirtual  uint64
	availableExtended uint64
}

var globalMemoryStatusEx = syscall.NewLazyDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

func physicalMemory() (uint64, string, error) {
	status := memoryStatusEx{length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	success, _, callErr := globalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&status)))
	if success == 0 {
		if callErr == nil || errors.Is(callErr, syscall.Errno(0)) {
			callErr = errors.New("GlobalMemoryStatusEx returned failure")
		}
		return 0, "GlobalMemoryStatusEx", callErr
	}
	if status.totalPhysical == 0 {
		return 0, "GlobalMemoryStatusEx", errors.New("physical memory was zero")
	}
	return status.totalPhysical, "GlobalMemoryStatusEx", nil
}

func cpuModel() (model string, resultErr error) {
	key, err := registry.OpenKey(
		registry.LOCAL_MACHINE,
		`HARDWARE\DESCRIPTION\System\CentralProcessor\0`,
		registry.QUERY_VALUE,
	)
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, key.Close()) }()
	model, _, err = key.GetStringValue("ProcessorNameString")
	if err != nil {
		return "", err
	}
	return model, nil
}

func osDescription() string {
	version := windows.RtlGetVersion()
	return fmt.Sprintf("Windows %d.%d build %d", version.MajorVersion, version.MinorVersion, version.BuildNumber)
}

func currentProcessToken() (string, error) {
	return windowsProcessToken(os.Getpid())
}

func processMatches(processID int, token string) (matches bool, resultErr error) {
	if processID <= 0 {
		return false, nil
	}
	handle, err := windows.OpenProcess(
		windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		uint32(processID),
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return false, nil
		}
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(handle)) }()
	result, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		return false, err
	}
	if result != uint32(windows.WAIT_TIMEOUT) {
		return false, nil
	}
	observed, err := windowsProcessTokenFromHandle(handle)
	if err != nil {
		return false, err
	}
	return observed == token, nil
}

func windowsProcessToken(processID int) (token string, resultErr error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(processID))
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(handle)) }()
	return windowsProcessTokenFromHandle(handle)
}

func windowsProcessTokenFromHandle(handle windows.Handle) (string, error) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return "", err
	}
	value := uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime)
	return strconv.FormatUint(value, 16), nil
}

func (authority *outputRootAuthority) sync() error {
	if authority == nil || authority.handle == windows.InvalidHandle {
		return errors.New("evidence output authority is closed")
	}
	// FlushFileBuffers on a write-authorized directory handle issues a
	// synchronous filesystem flush for its metadata. Returning the platform
	// error is essential: a filesystem without this primitive cannot claim a
	// durable content-addressed namespace publication.
	if err := windows.FlushFileBuffers(authority.handle); err != nil {
		return fmt.Errorf("flush evidence directory metadata: %w", err)
	}
	return nil
}

func (authority *outputRootAuthority) renameChildNoReplace(
	stage *stageDirectoryAuthority,
	destination string,
) error {
	if authority == nil || authority.handle == windows.InvalidHandle {
		return errors.New("evidence output authority is closed")
	}
	if err := requireDirectChildName(destination); err != nil {
		return err
	}
	if err := stage.verifyName(authority); err != nil {
		return err
	}
	if stage.transition != nil {
		if err := stage.transition("", "rename-source-verified"); err != nil {
			return err
		}
	}
	// Normal child processes need pathname access below the stage, so its
	// long-lived authority deliberately denies delete sharing. At the rename
	// boundary we release that lease, reopen the direct child with DELETE
	// authority, and compare its file ID before renaming the exact handle.
	// A name swap in the reopen window therefore fails instead of publishing
	// the replacement.
	expected := stage.identity
	if err := stage.close(); err != nil {
		return err
	}
	renameHandle, err := openWindowsRelativeAccess(
		authority.handle, stage.name, windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_READ_ATTRIBUTES|windows.DELETE|windows.SYNCHRONIZE,
	)
	if err != nil {
		return err
	}
	closeRenameHandle := func(operationErr error) error {
		return errors.Join(operationErr, windows.CloseHandle(renameHandle))
	}
	observed, err := windowsDirectoryIdentity(renameHandle)
	if err != nil {
		return closeRenameHandle(err)
	}
	if observed != expected {
		return closeRenameHandle(errors.New("stage name changed while acquiring rename authority"))
	}
	encoded, err := windows.UTF16FromString(destination)
	if err != nil {
		return closeRenameHandle(err)
	}
	nameBytes := (len(encoded) - 1) * 2
	var layout windowsFileRenameInformation
	buffer := make([]byte, int(unsafe.Offsetof(layout.fileName))+nameBytes)
	information := (*windowsFileRenameInformation)(unsafe.Pointer(&buffer[0]))
	information.rootDirectory = authority.handle
	information.fileNameLength = uint32(nameBytes)
	copy(
		(*[windows.MAX_LONG_PATH]uint16)(unsafe.Pointer(&information.fileName[0]))[:nameBytes/2:nameBytes/2],
		encoded,
	)
	var status windows.IO_STATUS_BLOCK
	renameErr := windows.NtSetInformationFile(
		renameHandle,
		&status,
		&buffer[0],
		uint32(len(buffer)),
		windows.FileRenameInformation,
	)
	return closeRenameHandle(renameErr)
}
