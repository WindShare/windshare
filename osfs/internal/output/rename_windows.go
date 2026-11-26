//go:build windows

package output

import (
	"errors"
	"io/fs"
	"os"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// windowsFileRenameInformationEx is NT FILE_INFORMATION_CLASS value 65
// (Windows 10 RS1+). golang.org/x/sys/windows defines FileRenameInformation
// (10) and FileRenameInfoEx (22, for SetFileInformationByHandle) but omits
// the NtSetInformationFile class constant.
const windowsFileRenameInformationEx uint32 = 65

type renameInformation struct {
	Flags  uint32
	Root   windows.Handle
	Length uint32
	Name   [1]uint16
}

func isUnsupportedFileInfoExStatus(nt windows.NTStatus) bool {
	return nt == windows.STATUS_INVALID_INFO_CLASS ||
		nt == windows.STATUS_INVALID_PARAMETER ||
		nt == windows.STATUS_NOT_SUPPORTED ||
		nt == windows.STATUS_INVALID_DEVICE_REQUEST
}

func renameHandle(handle windows.Handle, parent *Dir, name string, replace bool) error {
	if parent == nil || parent.handle == nil {
		return fs.ErrClosed
	}
	encoded, err := windows.UTF16FromString(name)
	if err != nil {
		return err
	}
	encoded = encoded[:len(encoded)-1]
	var layout renameInformation
	buffer := make([]byte, int(unsafe.Offsetof(layout.Name))+2*len(encoded))
	value := (*renameInformation)(unsafe.Pointer(&buffer[0]))
	value.Root = windows.Handle(parent.handle.Fd())
	value.Length = uint32(2 * len(encoded))
	copy(unsafe.Slice(&value.Name[0], len(encoded)), encoded)

	err = applyRenameInformation(replace, func(flags, class uint32) error {
		value.Flags = flags
		var status windows.IO_STATUS_BLOCK
		return windows.NtSetInformationFile(handle, &status, &buffer[0], uint32(len(buffer)), class)
	})
	runtime.KeepAlive(parent.handle)
	if nt, ok := errors.AsType[windows.NTStatus](err); ok {
		err = nt.Errno()
	}
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
		return errors.Join(ErrNamespaceCollision, fs.ErrExist, err)
	}
	if !replace {
		return noReplaceError(err)
	}
	return err
}

// applyRenameInformation uses NTFS POSIX overwrite semantics when replace is
// requested so concurrent FILE_SHARE_DELETE readers (such as lock-free journal
// header scans) do not block journal compaction, while falling back to legacy
// FileRenameInformation on filesystems without FileRenameInformationEx.
// No-replace publication keeps legacy FileRenameInformation directly.
func applyRenameInformation(replace bool, setInfo func(flags, class uint32) error) error {
	if !replace {
		return setInfo(0, windows.FileRenameInformation)
	}
	const posixReplaceFlags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	err := setInfo(posixReplaceFlags, windowsFileRenameInformationEx)
	if err == nil {
		return nil
	}
	if nt, ok := errors.AsType[windows.NTStatus](err); ok && isUnsupportedFileInfoExStatus(nt) {
		return setInfo(windows.FILE_RENAME_REPLACE_IF_EXISTS, windows.FileRenameInformation)
	}
	return err
}

func noReplaceError(err error) error {
	if errors.Is(err, windows.ERROR_NOT_SUPPORTED) ||
		errors.Is(err, windows.ERROR_INVALID_FUNCTION) ||
		errors.Is(err, windows.ERROR_CALL_NOT_IMPLEMENTED) {
		return errors.Join(ErrUnsupportedOutput, err)
	}
	return err
}

// Windows antivirus and search-indexer minifilters routinely hold handles on
// freshly written files for 10-200ms, causing rename handle opens or
// NtSetInformationFile calls to return ACCESS_DENIED or SHARING_VIOLATION.
var windowsRenameBackoff = [...]time.Duration{
	5 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond,
	40 * time.Millisecond, 80 * time.Millisecond, 160 * time.Millisecond,
}

func retryWindowsRename(attempt func() error, wait func(time.Duration)) error {
	err := attempt()
	if !isTransientWindowsRenameError(err) {
		return err
	}
	for _, delay := range windowsRenameBackoff {
		wait(delay)
		err = attempt()
		if !isTransientWindowsRenameError(err) {
			return err
		}
	}
	return err
}

func isTransientWindowsRenameError(err error) bool {
	if err == nil || errors.Is(err, ErrUnsafeNamespace) {
		return false
	}
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

func renameFileNoReplace(srcDir *Dir, srcName string, dstDir *Dir, dstName string, replace bool) error {
	if err := validName(srcName); err != nil {
		return err
	}
	if err := validName(dstName); err != nil {
		return err
	}
	return retryWindowsRename(func() error {
		h, err := ntCreateAt(srcDir, srcName, windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
			windows.FILE_OPEN, windowsSyncFileOpts)
		if err != nil {
			return wrapAtError("openat", srcDir, srcName, err, false)
		}
		if _, err := verifyOpenHandle(h, false); err != nil {
			return err
		}
		defer windows.CloseHandle(h)
		return renameHandle(h, dstDir, dstName, replace)
	}, time.Sleep)
}

// Header scans run under dirLock without each task's task.lock; a short bounded
// retry absorbs transient sharing or delete-pending races (such as legacy
// non-NTFS fallback renames or antivirus scanners) without stalling dirLock on
// permanent ACL permission errors.
var windowsHeaderReadBackoff = [...]time.Duration{
	2 * time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond,
	20 * time.Millisecond, 40 * time.Millisecond,
}

func openJournalHeaderFile(opDir *Dir) (*os.File, error) {
	return retryWindowsJournalOpen(windowsHeaderReadBackoff[:], func() (*os.File, error) {
		return openChildFile(opDir, journalFileName, os.O_RDONLY, 0)
	}, time.Sleep)
}

// Reopening state.log right after compaction closes and renames it runs outside
// normal dirLock scans and faces the same 10-200ms AV/indexer scan window as
// rename, while keeping retries scoped to sharing/delete-pending races so
// permanent ACL denials still fail immediately.
func reopenJournalFile(opDir *Dir) (*os.File, error) {
	return retryWindowsJournalOpen(windowsRenameBackoff[:], func() (*os.File, error) {
		return openChildFile(opDir, journalFileName, os.O_RDWR, privateFileMode)
	}, time.Sleep)
}

func retryWindowsJournalOpen(
	backoff []time.Duration,
	attempt func() (*os.File, error),
	wait func(time.Duration),
) (*os.File, error) {
	f, err := attempt()
	if !isTransientWindowsJournalOpenError(err) {
		return f, err
	}
	for _, delay := range backoff {
		wait(delay)
		f, err = attempt()
		if !isTransientWindowsJournalOpenError(err) {
			return f, err
		}
	}
	return nil, err
}

func isTransientWindowsJournalOpenError(err error) bool {
	if err == nil || errors.Is(err, ErrUnsafeNamespace) {
		return false
	}
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_DELETE_PENDING)
}
