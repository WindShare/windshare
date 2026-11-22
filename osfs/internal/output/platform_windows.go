//go:build windows

package output

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/windshare/windshare/osfs/internal/pathfailure"
	"golang.org/x/sys/windows"
)

const (
	windowsExtendedPrefix      = `\\?\`
	windowsExtendedUNCPrefix   = `\\?\UNC\`
	windowsShortFinalPathLimit = 240
	initialResolvedPathUnits   = 512
)

type fileAttributeTagInfo struct {
	FileAttributes uint32
	ReparseTag     uint32
}

// A caller-named container whose final component is a Windows directory symlink
// (mklink /D) or directory junction / mount point (mklink /J) is resolved once
// through the kernel, while other reparse points such as cloud placeholders
// stay rejected. Every entry inside the container stays no-follow.
func resolveContainerTarget(
	path string,
	info os.FileInfo,
	follow func(string) (os.FileInfo, error),
) (string, os.FileInfo, error) {
	link, err := isWindowsContainerLink(path, info)
	if err != nil {
		return "", nil, errors.Join(ErrUnsafeNamespace, err)
	}
	if !link {
		if !info.IsDir() || isWindowsReparsePoint(info) {
			return "", nil, ErrUnsafeNamespace
		}
		return path, info, nil
	}
	followed, err := follow(path)
	if err != nil || !followed.IsDir() {
		return "", nil, errors.Join(ErrUnsafeNamespace, err)
	}
	resolved, err := windowsResolvedContainerPath(path)
	if err != nil {
		return "", nil, errors.Join(ErrUnsafeNamespace, err)
	}
	target, err := os.Lstat(resolved)
	if err != nil || !target.IsDir() || isWindowsReparsePoint(target) || !os.SameFile(followed, target) {
		return "", nil, errors.Join(ErrUnsafeNamespace, err)
	}
	return resolved, followed, nil
}

func isWindowsReparsePoint(info os.FileInfo) bool {
	if info.Mode()&os.ModeSymlink != 0 {
		return true
	}
	native, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && native.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

func isWindowsContainerLink(path string, info os.FileInfo) (bool, error) {
	if info.Mode()&os.ModeSymlink != 0 {
		return true, nil
	}
	if !isWindowsReparsePoint(info) {
		return false, nil
	}
	tag, err := windowsReparseTag(path)
	if err != nil {
		return false, err
	}
	return tag == windows.IO_REPARSE_TAG_SYMLINK || tag == windows.IO_REPARSE_TAG_MOUNT_POINT, nil
}

func windowsReparseTag(path string) (uint32, error) {
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	handle, err := windows.CreateFile(
		encoded,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(handle)
	var tagInfo fileAttributeTagInfo
	err = windows.GetFileInformationByHandleEx(
		handle,
		windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&tagInfo)),
		uint32(unsafe.Sizeof(tagInfo)),
	)
	if err != nil {
		return 0, err
	}
	if tagInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		return 0, nil
	}
	return tagInfo.ReparseTag, nil
}

func windowsResolvedContainerPath(path string) (string, error) {
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	handle, err := windows.CreateFile(
		encoded,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(handle)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return "", err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 ||
		info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return "", ErrUnsafeNamespace
	}
	buffer := make([]uint16, initialResolvedPathUnits)
	for {
		length, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
		if err != nil {
			return "", err
		}
		if length < uint32(len(buffer)) {
			return normalizeWindowsFinalPath(windows.UTF16ToString(buffer[:length])), nil
		}
		buffer = make([]uint16, length+1)
	}
}

func normalizeWindowsFinalPath(path string) string {
	if rest, ok := strings.CutPrefix(path, windowsExtendedUNCPrefix); ok {
		stripped := `\\` + rest
		if len(stripped) < windowsShortFinalPathLimit {
			return stripped
		}
		return path
	}
	if rest, ok := strings.CutPrefix(path, windowsExtendedPrefix); ok &&
		len(rest) >= 3 && rest[1] == ':' && rest[2] == '\\' && len(rest) < windowsShortFinalPathLimit {
		return rest
	}
	return path
}

func openDirHandle(path string) (*Dir, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || isWindowsReparsePoint(info) {
		return nil, ErrUnsafeNamespace
	}
	return openVerifiedDirHandle(path, info)
}

func openVerifiedDirHandle(path string, expected os.FileInfo) (*Dir, error) {
	handle, err := openWindowsPath(path, os.O_RDONLY, true)
	if err != nil {
		return nil, err
	}
	current, err := handle.Stat()
	if err != nil || !current.IsDir() || !os.SameFile(expected, current) {
		return nil, errors.Join(ErrUnsafeNamespace, err, handle.Close())
	}
	return &Dir{path: path, handle: handle, info: current}, nil
}

func openChildDir(parent *Dir, name string) (*Dir, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	return openDirHandle(filepath.Join(parent.path, name))
}

func openChildFile(parent *Dir, name string, flags int, _ fs.FileMode) (*os.File, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	f, err := openWindowsPath(filepath.Join(parent.path, name), flags, false)
	if err != nil {
		if isNativeNameRefusal(err) {
			return nil, errors.Join(ErrNameRejected, err)
		}
		return nil, err
	}
	return f, nil
}

func openWindowsPath(path string, flags int, directory bool) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	access := uint32(windows.GENERIC_READ)
	if directory {
		access = windows.FILE_LIST_DIRECTORY | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE
		if flags&os.O_RDWR != 0 {
			access |= windows.DELETE
		}
	} else {
		if flags&os.O_APPEND != 0 {
			access |= windows.FILE_APPEND_DATA | windows.SYNCHRONIZE
		} else if flags&(os.O_RDWR|os.O_WRONLY) != 0 {
			access |= windows.GENERIC_WRITE | windows.DELETE
		}
	}
	disposition := uint32(windows.OPEN_EXISTING)
	switch {
	case flags&os.O_CREATE != 0 && flags&os.O_EXCL != 0:
		disposition = windows.CREATE_NEW
	case flags&os.O_CREATE != 0 && flags&os.O_TRUNC != 0:
		disposition = windows.CREATE_ALWAYS
	case flags&os.O_CREATE != 0:
		disposition = windows.OPEN_ALWAYS
	case flags&os.O_TRUNC != 0:
		disposition = windows.TRUNCATE_EXISTING
	}
	attributes := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT | windows.FILE_FLAG_BACKUP_SEMANTICS)
	h, err := windows.CreateFile(
		name,
		access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		disposition,
		attributes,
		0,
	)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil ||
		info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(h)
		return nil, errors.Join(ErrUnsafeNamespace, err)
	}
	isDir := info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if directory != isDir {
		_ = windows.CloseHandle(h)
		return nil, ErrUnsafeNamespace
	}
	return os.NewFile(uintptr(h), path), nil
}

func lstatChild(parent *Dir, name string) (EntryKind, uint64, error) {
	if err := validName(name); err != nil {
		return 0, 0, err
	}
	info, err := os.Lstat(filepath.Join(parent.path, name))
	if errors.Is(err, fs.ErrNotExist) {
		return EntryAbsent, 0, nil
	}
	if err != nil {
		if isNativeNameRefusal(err) {
			return 0, 0, errors.Join(ErrNameRejected, err)
		}
		return 0, 0, err
	}
	if isWindowsReparsePoint(info) {
		return EntryOther, 0, nil
	}
	if info.IsDir() {
		return EntryDirectory, 0, nil
	}
	if info.Mode().IsRegular() {
		return EntryRegularFile, uint64(max(info.Size(), 0)), nil
	}
	return EntryOther, 0, nil
}

func mkdirChild(parent *Dir, name string, mode fs.FileMode) error {
	if err := validName(name); err != nil {
		return err
	}
	err := os.Mkdir(filepath.Join(parent.path, name), mode)
	if err != nil {
		if isNativeNameRefusal(err) {
			return errors.Join(ErrNameRejected, err)
		}
		return mapCollision(err)
	}
	return nil
}

func removeChild(parent *Dir, name string, _ bool) error {
	if err := ValidComponent(name); err != nil {
		return err
	}
	err := os.Remove(filepath.Join(parent.path, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func readDirNames(dir *Dir) ([]string, error) {
	entries, err := os.ReadDir(dir.path)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
	}
	return names, nil
}

func validateNativeName(name string) error {
	if !filepath.IsLocal(name) || strings.ContainsAny(name, `<>"|?*`) ||
		strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return ErrNameRejected
	}
	return nil
}

func isNativeNameRefusal(err error) bool {
	return pathfailure.IsTooLong(err) || errors.Is(err, windows.ERROR_INVALID_NAME)
}

func entryMatchesSpelling(parent *Dir, name string) (exact bool, resultErr error) {
	encoded, err := windows.UTF16PtrFromString(filepath.Join(parent.path, name))
	if err != nil {
		return false, err
	}
	handle, err := windows.CreateFile(
		encoded,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(handle)) }()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return false, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return false, ErrUnsafeNamespace
	}
	buffer := make([]uint16, initialResolvedPathUnits)
	for {
		length, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
		if err != nil {
			return false, err
		}
		if length < uint32(len(buffer)) {
			return name == filepath.Base(windows.UTF16ToString(buffer[:length])), nil
		}
		buffer = make([]uint16, length+1)
	}
}

// Windows does not promise a directory fsync barrier; flushed files and atomic
// rename bound interrupted updates.
func syncDir(*os.File) error { return nil }

func setFileModifiedTime(handle *os.File, t time.Time) error {
	stamp := windows.NsecToFiletime(t.UnixNano())
	err := windows.SetFileTime(windows.Handle(handle.Fd()), nil, nil, &stamp)
	runtime.KeepAlive(handle)
	return err
}

func setDirModifiedTime(d *Dir, t time.Time) error {
	encoded, err := windows.UTF16PtrFromString(d.path)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(
		encoded,
		windows.FILE_WRITE_ATTRIBUTES|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(h), d.path)
	defer f.Close()
	cur, err := f.Stat()
	if err != nil || !cur.IsDir() || !os.SameFile(d.info, cur) {
		return errors.Join(ErrUnsafeNamespace, err)
	}
	return setFileModifiedTime(f, t)
}

func lockFile(f *os.File, blocking bool) (func() error, error) {
	overlap := new(windows.Overlapped)
	h := windows.Handle(f.Fd())
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK)
	if !blocking {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	err := windows.LockFileEx(h, flags, 0, 1, 0, overlap)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return nil, errors.Join(ErrNamespaceLockBusy, err)
	}
	if err != nil {
		return nil, err
	}
	return func() error {
		unlockErr := windows.UnlockFileEx(h, 0, 1, 0, overlap)
		runtime.KeepAlive(f)
		return unlockErr
	}, nil
}

type renameInformation struct {
	Replace uint32
	Root    windows.Handle
	Length  uint32
	Name    [1]uint16
}

func renameHandle(handle windows.Handle, parent *Dir, name string, replace bool) error {
	encoded, err := windows.UTF16FromString(name)
	if err != nil {
		return err
	}
	encoded = encoded[:len(encoded)-1]
	var layout renameInformation
	buffer := make([]byte, int(unsafe.Offsetof(layout.Name))+2*len(encoded))
	value := (*renameInformation)(unsafe.Pointer(&buffer[0]))
	value.Root = windows.Handle(parent.handle.Fd())
	if replace {
		value.Replace = 1
	}
	value.Length = uint32(2 * len(encoded))
	copy(unsafe.Slice(&value.Name[0], len(encoded)), encoded)
	var status windows.IO_STATUS_BLOCK
	const fileRenameInformation = 10
	err = windows.NtSetInformationFile(handle, &status, &buffer[0], uint32(len(buffer)), fileRenameInformation)
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
	5 * time.Millisecond,
	10 * time.Millisecond,
	20 * time.Millisecond,
	40 * time.Millisecond,
	80 * time.Millisecond,
	160 * time.Millisecond,
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
		opened, err := openWindowsPath(filepath.Join(srcDir.path, srcName), os.O_RDWR, false)
		if err != nil {
			return err
		}
		defer opened.Close()
		return renameHandle(windows.Handle(opened.Fd()), dstDir, dstName, replace)
	}, time.Sleep)
}

func classifyPlatformError(err error) (NativeErrorClass, bool) {
	switch {
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		return NativeErrorAccessDenied, true
	case errors.Is(err, windows.ERROR_SHARING_VIOLATION), errors.Is(err, windows.ERROR_LOCK_VIOLATION):
		return NativeErrorSharingViolation, true
	case errors.Is(err, windows.ERROR_FILE_NOT_FOUND), errors.Is(err, windows.ERROR_PATH_NOT_FOUND):
		return NativeErrorNotFound, true
	case errors.Is(err, windows.ERROR_INVALID_HANDLE):
		return NativeErrorInvalidHandle, true
	case errors.Is(err, windows.ERROR_NOT_SUPPORTED), errors.Is(err, windows.ERROR_INVALID_FUNCTION),
		errors.Is(err, windows.ERROR_CALL_NOT_IMPLEMENTED):
		return NativeErrorUnsupported, true
	case errors.Is(err, windows.ERROR_IO_DEVICE), errors.Is(err, windows.ERROR_CRC):
		return NativeErrorIO, true
	default:
		return 0, false
	}
}
