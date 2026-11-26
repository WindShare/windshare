//go:build windows

package output

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
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
	windowsExtendedPrefix       = `\\?\`
	windowsExtendedUNCPrefix    = `\\?\UNC\`
	windowsNTNamespacePrefix    = `\??\`
	windowsMaxShortDirPath      = 248
	initialResolvedPathUnits    = 512
	windowsDirLockPrefix        = `Local\windshare-output-dir-`
	windowsDirLockPathHashBytes = 16

	windowsShareAll      = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE
	windowsDirReadAccess = windows.FILE_LIST_DIRECTORY | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE
	windowsSyncDirOpts   = windows.FILE_DIRECTORY_FILE | windows.FILE_SYNCHRONOUS_IO_NONALERT
	windowsSyncFileOpts  = windows.FILE_NON_DIRECTORY_FILE | windows.FILE_SYNCHRONOUS_IO_NONALERT

	windowsTicksPerSecond           = int64(10_000_000)
	windowsNanosPerTick             = int64(100)
	windowsFiletimeEpochUnixSeconds = int64(-11_644_473_600) // 1601-01-01T00:00:00Z
	windowsFiletimeMaxUnixSeconds   = math.MaxInt64/windowsTicksPerSecond + windowsFiletimeEpochUnixSeconds
	windowsFiletimeMaxSubsecTicks   = math.MaxInt64 % windowsTicksPerSecond
)

type fileAttributeTagInfo struct {
	FileAttributes, ReparseTag uint32
}

type fileDispositionInfoEx struct{ Flags uint32 }
type fileDispositionInfo struct{ DeleteFile byte }

type fileBasicInfo struct {
	CreationTime, LastAccessTime, LastWriteTime, ChangedTime int64
	FileAttributes, _                                        uint32
}

// A caller-named container whose final component is a Windows directory symlink
// (mklink /D) or directory junction / mount point (mklink /J) is resolved once
// through the kernel, while other reparse points such as cloud placeholders
// stay rejected. Every entry inside the container stays no-follow.
func resolveContainerTarget(path string, info os.FileInfo, follow func(string) (os.FileInfo, error)) (string, os.FileInfo, error) {
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

func hasWindowsExtendedOrDevicePrefix(path string) bool {
	if len(path) < 4 {
		return false
	}
	if strings.HasPrefix(path, windowsNTNamespacePrefix) {
		return true
	}
	isSep := func(c byte) bool { return c == '\\' || c == '/' }
	return isSep(path[0]) && isSep(path[1]) && (path[2] == '?' || path[2] == '.') && isSep(path[3])
}

// windowsCreateFilePath mirrors Go's os.fixLongPath before raw windows.CreateFile
// calls so OpenContainer matches os.Lstat and os.MkdirAll on standard Windows
// (LongPathsEnabled == 0): short paths (< 248 bytes) stay unmodified to preserve
// native Win32 normalization, while long paths are canonicalized via FullPath
// before prepending \\?\ or \\?\UNC\.
func windowsCreateFilePath(path string) (string, error) {
	if path == "" || hasWindowsExtendedOrDevicePrefix(path) {
		return path, nil
	}
	pathLen := len(path)
	if !filepath.IsAbs(path) {
		if wd, err := os.Getwd(); err == nil {
			pathLen += len(wd) + 1
		}
	}
	if pathLen < windowsMaxShortDirPath {
		return path, nil
	}
	full, err := windows.FullPath(path)
	if err != nil {
		return "", err
	}
	if hasWindowsExtendedOrDevicePrefix(full) {
		return full, nil
	}
	if rest, ok := strings.CutPrefix(full, `\\`); ok {
		return windowsExtendedUNCPrefix + rest, nil
	}
	if len(full) >= 3 && full[1] == ':' && full[2] == '\\' {
		return windowsExtendedPrefix + full, nil
	}
	return full, nil
}

func windowsCreateFileUTF16(path string) (*uint16, error) {
	openPath, err := windowsCreateFilePath(path)
	if err != nil {
		return nil, err
	}
	return windows.UTF16PtrFromString(openPath)
}

func windowsReparseTag(path string) (uint32, error) {
	encoded, err := windowsCreateFileUTF16(path)
	if err != nil {
		return 0, err
	}
	handle, err := windows.CreateFile(encoded, 0, windowsShareAll, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(handle)
	var tagInfo fileAttributeTagInfo
	err = windows.GetFileInformationByHandleEx(handle, windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&tagInfo)), uint32(unsafe.Sizeof(tagInfo)))
	if err != nil {
		return 0, err
	}
	if tagInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		return 0, nil
	}
	return tagInfo.ReparseTag, nil
}

func windowsResolvedContainerPath(path string) (string, error) {
	encoded, err := windowsCreateFileUTF16(path)
	if err != nil {
		return "", err
	}
	handle, err := windows.CreateFile(encoded, windows.FILE_READ_ATTRIBUTES, windowsShareAll, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
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
	resolved, err := finalPathByHandle(handle)
	if err != nil {
		return "", err
	}
	return normalizeWindowsFinalPath(resolved), nil
}

func finalPathByHandle(handle windows.Handle) (string, error) {
	buffer := make([]uint16, initialResolvedPathUnits)
	for {
		length, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
		if err != nil {
			return "", err
		}
		if length < uint32(len(buffer)) {
			return windows.UTF16ToString(buffer[:length]), nil
		}
		buffer = make([]uint16, length+1)
	}
}

func normalizeWindowsFinalPath(path string) string {
	if rest, ok := strings.CutPrefix(path, windowsExtendedUNCPrefix); ok {
		return `\\` + rest
	}
	if rest, ok := strings.CutPrefix(path, windowsExtendedPrefix); ok &&
		len(rest) >= 3 && rest[1] == ':' && rest[2] == '\\' {
		return rest
	}
	return path
}

func openDirHandle(path string) (*Dir, error) {
	encoded, err := windowsCreateFileUTF16(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(encoded, windowsDirReadAccess, windowsShareAll, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	if _, err := verifyOpenHandle(h, true); err != nil {
		return nil, err
	}
	handle := os.NewFile(uintptr(h), path)
	info, err := handle.Stat()
	if err != nil || !info.IsDir() {
		return nil, errors.Join(ErrUnsafeNamespace, err, handle.Close())
	}
	return &Dir{path: path, handle: handle, info: info}, nil
}

func openVerifiedDirHandle(path string, expected os.FileInfo) (*Dir, error) {
	dir, err := openDirHandle(path)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(expected, dir.info) {
		return nil, errors.Join(ErrUnsafeNamespace, dir.Close())
	}
	return dir, nil
}

// ntCreateAt opens or creates a single component relative to an open directory
// handle so child operations stay anchored across directory renames and bypass
// intermediate path re-resolution and Win32 MAX_PATH limits.
func ntCreateAt(dir *Dir, name string, access, disposition, options uint32) (windows.Handle, error) {
	if dir == nil || dir.handle == nil {
		return windows.InvalidHandle, fs.ErrClosed
	}
	h, err := ntCreateRelative(windows.Handle(dir.handle.Fd()), name, access, disposition, options)
	runtime.KeepAlive(dir.handle)
	return h, err
}

func ntCreateRelative(root windows.Handle, name string, access, disposition, options uint32) (windows.Handle, error) {
	objName := &windows.NTUnicodeString{}
	if name != "" {
		var err error
		objName, err = windows.NewNTUnicodeString(name)
		if err != nil {
			return windows.InvalidHandle, err
		}
	}
	attrs := windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: root,
		ObjectName:    objName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	var handle windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	err := windows.NtCreateFile(&handle, access, &attrs, &iosb, nil, windows.FILE_ATTRIBUTE_NORMAL,
		windowsShareAll, disposition, options|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_OPEN_REPARSE_POINT, 0, 0)
	if err != nil {
		return windows.InvalidHandle, ntStatusToError(err, options)
	}
	return handle, nil
}

func ntStatusToError(err error, options uint32) error {
	nt, ok := errors.AsType[windows.NTStatus](err)
	if !ok {
		return err
	}
	errno := nt.Errno()
	switch nt {
	case windows.STATUS_REPARSE_POINT_ENCOUNTERED:
		return errors.Join(ErrUnsafeNamespace, errno)
	case windows.STATUS_DELETE_PENDING:
		// RtlNtStatusToDosError maps STATUS_DELETE_PENDING to ERROR_ACCESS_DENIED;
		// preserve ERROR_DELETE_PENDING so transient open races stay distinguishable
		// from permanent ACL denials.
		return errors.Join(windows.ERROR_DELETE_PENDING, errno)
	case windows.STATUS_FILE_IS_A_DIRECTORY:
		if options&windows.FILE_NON_DIRECTORY_FILE != 0 {
			return errors.Join(ErrUnsafeNamespace, errno)
		}
	case windows.STATUS_NOT_A_DIRECTORY:
		if options&windows.FILE_DIRECTORY_FILE != 0 {
			return errors.Join(ErrUnsafeNamespace, errno)
		}
	}
	return errno
}

func verifyOpenHandle(h windows.Handle, wantDir bool) (windows.ByHandleFileInformation, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil ||
		info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(h)
		return windows.ByHandleFileInformation{}, errors.Join(ErrUnsafeNamespace, err)
	}
	isDir := info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if wantDir != isDir {
		_ = windows.CloseHandle(h)
		return windows.ByHandleFileInformation{}, ErrUnsafeNamespace
	}
	if !wantDir {
		if fileType, err := windows.GetFileType(h); err != nil || fileType != windows.FILE_TYPE_DISK {
			_ = windows.CloseHandle(h)
			return windows.ByHandleFileInformation{}, errors.Join(ErrUnsafeNamespace, err)
		}
	}
	return info, nil
}

func wrapAtError(op string, parent *Dir, name string, err error, mapNameRefusal bool) error {
	if mapNameRefusal && isNativeNameRefusal(err) {
		return errors.Join(ErrNameRejected, err)
	}
	if errors.Is(err, ErrUnsafeNamespace) {
		return err
	}
	path := parent.Path()
	if name != "" {
		path = filepath.Join(path, name)
	}
	return &os.PathError{Op: op, Path: path, Err: err}
}

func ntFileAccessAndDisposition(flags int) (access, disposition uint32) {
	access = windows.FILE_GENERIC_READ | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE
	if flags&os.O_WRONLY != 0 {
		access = windows.FILE_GENERIC_WRITE | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE
	} else if flags&(os.O_RDWR|os.O_CREATE) != 0 {
		access |= windows.FILE_GENERIC_WRITE
	}
	if flags&os.O_APPEND != 0 {
		access |= windows.FILE_APPEND_DATA
		if flags&os.O_TRUNC == 0 {
			access &^= windows.FILE_WRITE_DATA
		}
	}

	// Never pass FILE_OVERWRITE/FILE_OVERWRITE_IF directly so an existing
	// reparse point cannot be clobbered before verifyOpenHandle rejects it.
	switch {
	case flags&os.O_CREATE != 0 && flags&os.O_EXCL != 0:
		disposition = windows.FILE_CREATE
	case flags&os.O_CREATE != 0:
		disposition = windows.FILE_OPEN_IF
	default:
		disposition = windows.FILE_OPEN
	}
	return access, disposition
}

func openChildDir(parent *Dir, name string) (*Dir, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	h, err := ntCreateAt(parent, name, windowsDirReadAccess, windows.FILE_OPEN, windowsSyncDirOpts)
	if err != nil {
		return nil, wrapAtError("openat", parent, name, err, true)
	}
	if _, err := verifyOpenHandle(h, true); err != nil {
		return nil, err
	}
	childPath := filepath.Join(parent.Path(), name)
	handle := os.NewFile(uintptr(h), childPath)
	info, err := handle.Stat()
	if err != nil || !info.IsDir() {
		return nil, errors.Join(ErrUnsafeNamespace, err, handle.Close())
	}
	return &Dir{path: childPath, handle: handle, info: info}, nil
}

func openChildFile(parent *Dir, name string, flags int, _ fs.FileMode) (*os.File, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	access, disposition := ntFileAccessAndDisposition(flags)
	h, err := ntCreateAt(parent, name, access, disposition, windowsSyncFileOpts)
	if err != nil {
		return nil, wrapAtError("openat", parent, name, err, true)
	}
	if _, err := verifyOpenHandle(h, false); err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(h), filepath.Join(parent.Path(), name))
	if flags&os.O_TRUNC != 0 {
		if err := f.Truncate(0); err != nil {
			return nil, errors.Join(err, f.Close())
		}
	}
	return f, nil
}

func lstatChild(parent *Dir, name string) (EntryKind, uint64, error) {
	if err := validName(name); err != nil {
		return 0, 0, err
	}
	h, err := ntCreateAt(parent, name, windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE, windows.FILE_OPEN, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return EntryAbsent, 0, nil
	}
	if err != nil {
		return 0, 0, wrapAtError("fstatat", parent, name, err, true)
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return 0, 0, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return EntryOther, 0, nil
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return EntryDirectory, 0, nil
	}
	if fileType, err := windows.GetFileType(h); err != nil || fileType != windows.FILE_TYPE_DISK {
		return EntryOther, 0, nil
	}
	return EntryRegularFile, uint64(info.FileSizeHigh)<<32 | uint64(info.FileSizeLow), nil
}

func mkdirChild(parent *Dir, name string, _ fs.FileMode) error {
	if err := validName(name); err != nil {
		return err
	}
	h, err := ntCreateAt(parent, name, windowsDirReadAccess, windows.FILE_CREATE, windows.FILE_DIRECTORY_FILE)
	if err != nil {
		if isNativeNameRefusal(err) {
			return errors.Join(ErrNameRejected, err)
		}
		if errors.Is(err, fs.ErrExist) {
			return errors.Join(ErrNamespaceCollision, err)
		}
		return wrapAtError("mkdirat", parent, name, err, false)
	}
	return windows.CloseHandle(h)
}

func removeChild(parent *Dir, name string, isDir bool) error {
	if err := ValidComponent(name); err != nil {
		return err
	}
	options := uint32(windows.FILE_NON_DIRECTORY_FILE)
	if isDir {
		options = windows.FILE_DIRECTORY_FILE
	}
	h, err := ntCreateAt(parent, name, windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE, windows.FILE_OPEN, options)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		h, err = ntCreateAt(parent, name, windows.DELETE|windows.SYNCHRONIZE, windows.FILE_OPEN, options)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return wrapAtError("unlinkat", parent, name, err, false)
	}
	defer windows.CloseHandle(h)
	if err := deleteOpenHandle(h); err != nil {
		return wrapAtError("unlinkat", parent, name, err, false)
	}
	return nil
}

func deleteOpenHandle(h windows.Handle) error {
	var iosb windows.IO_STATUS_BLOCK
	ex := fileDispositionInfoEx{
		Flags: windows.FILE_DISPOSITION_DELETE | windows.FILE_DISPOSITION_POSIX_SEMANTICS | windows.FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE,
	}
	err := windows.NtSetInformationFile(h, &iosb, (*byte)(unsafe.Pointer(&ex)),
		uint32(unsafe.Sizeof(ex)), windows.FileDispositionInformationEx)
	if err == nil {
		return nil
	}
	if nt, ok := errors.AsType[windows.NTStatus](err); ok {
		if isUnsupportedFileInfoExStatus(nt) {
			return deleteOpenHandleFallback(h)
		}
		return nt.Errno()
	}
	return err
}

func deleteOpenHandleFallback(h windows.Handle) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err == nil &&
		info.FileAttributes&windows.FILE_ATTRIBUTE_READONLY != 0 {
		wh, err := ntCreateRelative(h, "", windows.FILE_WRITE_ATTRIBUTES|windows.SYNCHRONIZE, windows.FILE_OPEN, 0)
		if err != nil {
			return err
		}
		attrs := info.FileAttributes &^ windows.FILE_ATTRIBUTE_READONLY
		if attrs == 0 {
			attrs = windows.FILE_ATTRIBUTE_NORMAL
		}
		basic := fileBasicInfo{FileAttributes: attrs}
		setErr := windows.SetFileInformationByHandle(wh, windows.FileBasicInfo,
			(*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic)))
		_ = windows.CloseHandle(wh)
		if setErr != nil {
			return setErr
		}
	}
	disp := fileDispositionInfo{DeleteFile: 1}
	return windows.SetFileInformationByHandle(h, windows.FileDispositionInfo,
		(*byte)(unsafe.Pointer(&disp)), uint32(unsafe.Sizeof(disp)))
}

func readDirNames(dir *Dir) ([]string, error) {
	h, err := ntCreateAt(dir, "", windowsDirReadAccess, windows.FILE_OPEN, windowsSyncDirOpts)
	if err != nil {
		return nil, wrapAtError("openat", dir, "", err, false)
	}
	f := os.NewFile(uintptr(h), dir.Path())
	defer f.Close()
	return f.Readdirnames(-1)
}

func validateNativeName(name string) error {
	if !filepath.IsLocal(name) || strings.ContainsAny(name, `<>:"|?*`) ||
		strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return ErrNameRejected
	}
	return nil
}

func isNativeNameRefusal(err error) bool {
	return pathfailure.IsTooLong(err) || errors.Is(err, windows.ERROR_INVALID_NAME)
}

func entryMatchesSpelling(parent *Dir, name string) (exact bool, resultErr error) {
	if err := validName(name); err != nil {
		return false, err
	}
	handle, err := ntCreateAt(parent, name, windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE, windows.FILE_OPEN, 0)
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
	actual, err := finalPathByHandle(handle)
	if err != nil {
		return false, err
	}
	return name == filepath.Base(actual), nil
}

// Windows does not promise a directory fsync barrier; flushed files and atomic
// rename bound interrupted updates.
func syncDir(*os.File) error { return nil }

// timeToWindowsFiletime converts t to a strictly positive signed FILETIME
// without using time.Time.UnixNano (which silently wraps outside 1678-2262).
// Zero is rejected because SetFileTime treats it as "leave unchanged", and
// values above math.MaxInt64 are rejected because NT treats negative file times
// as control sentinels or invalid parameters.
func timeToWindowsFiletime(t time.Time) (windows.Filetime, error) {
	sec := t.Unix()
	subsecTicks := int64(t.Nanosecond()) / windowsNanosPerTick
	if sec < windowsFiletimeEpochUnixSeconds || sec > windowsFiletimeMaxUnixSeconds {
		return windows.Filetime{}, windows.ERROR_INVALID_PARAMETER
	}
	if sec == windowsFiletimeMaxUnixSeconds && subsecTicks > windowsFiletimeMaxSubsecTicks {
		return windows.Filetime{}, windows.ERROR_INVALID_PARAMETER
	}
	ticks := (sec-windowsFiletimeEpochUnixSeconds)*windowsTicksPerSecond + subsecTicks
	if ticks <= 0 {
		return windows.Filetime{}, windows.ERROR_INVALID_PARAMETER
	}
	return windows.Filetime{
		LowDateTime:  uint32(ticks),
		HighDateTime: uint32(ticks >> 32),
	}, nil
}

func setFileModifiedTime(handle *os.File, t time.Time) error {
	if handle == nil {
		return fs.ErrClosed
	}
	stamp, err := timeToWindowsFiletime(t)
	if err != nil {
		return err
	}
	err = windows.SetFileTime(windows.Handle(handle.Fd()), nil, nil, &stamp)
	runtime.KeepAlive(handle)
	return err
}

func setDirModifiedTime(d *Dir, t time.Time) error {
	stamp, err := timeToWindowsFiletime(t)
	if err != nil {
		return err
	}
	h, err := ntCreateAt(d, "", windows.FILE_WRITE_ATTRIBUTES|windows.SYNCHRONIZE, windows.FILE_OPEN, windowsSyncDirOpts)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.SetFileTime(h, nil, nil, &stamp)
}

func windowsDirLockName(root *Dir) (string, error) {
	var info windows.ByHandleFileInformation
	err := windows.GetFileInformationByHandle(windows.Handle(root.handle.Fd()), &info)
	runtime.KeepAlive(root.handle)
	if err != nil {
		return "", err
	}
	if info.VolumeSerialNumber != 0 || info.FileIndexHigh != 0 || info.FileIndexLow != 0 {
		return fmt.Sprintf(
			"%s%08x-%08x%08x",
			windowsDirLockPrefix,
			info.VolumeSerialNumber,
			info.FileIndexHigh,
			info.FileIndexLow,
		), nil
	}
	// Fallback for virtual filesystems that report zero volume and file indices.
	finalPath, err := finalPathByHandle(windows.Handle(root.handle.Fd()))
	runtime.KeepAlive(root.handle)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(strings.ToUpper(finalPath)))
	return windowsDirLockPrefix + "p-" + hex.EncodeToString(sum[:windowsDirLockPathHashBytes]), nil
}

func acquireDirLock(root *Dir) (*DirLock, error) {
	if root == nil || root.handle == nil {
		return nil, fs.ErrClosed
	}
	name, err := windowsDirLockName(root)
	if err != nil {
		return nil, err
	}
	encoded, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	// Windows cannot LockFileEx a directory handle; a kernel named mutex keyed
	// by container identity keeps .windshare-tmp free of lock files and is
	// automatically destroyed when the last handle closes. Pin the goroutine to
	// its OS thread because Windows mutexes have OS-thread ownership semantics.
	runtime.LockOSThread()
	handle, err := windows.CreateMutex(nil, false, encoded)
	if handle == 0 || (err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS)) {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		runtime.UnlockOSThread()
		return nil, err
	}
	waitStatus, err := windows.WaitForSingleObject(handle, windows.INFINITE)
	if err != nil {
		_ = windows.CloseHandle(handle)
		runtime.UnlockOSThread()
		return nil, err
	}
	// WAIT_ABANDONED means the previous owner crashed without releasing the
	// mutex; Windows still transfers ownership to this caller.
	if waitStatus != windows.WAIT_OBJECT_0 && waitStatus != windows.WAIT_ABANDONED {
		_ = windows.CloseHandle(handle)
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("wait for directory lock: unexpected status 0x%x", waitStatus)
	}
	return &DirLock{
		release: func() error {
			releaseErr := windows.ReleaseMutex(handle)
			closeErr := windows.CloseHandle(handle)
			runtime.UnlockOSThread()
			return errors.Join(releaseErr, closeErr)
		},
	}, nil
}

func lockFile(f *os.File) (func() error, error) {
	overlap := new(windows.Overlapped)
	h := windows.Handle(f.Fd())
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
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
