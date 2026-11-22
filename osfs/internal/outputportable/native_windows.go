//go:build windows

package outputportable

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

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/windows"
)

const (
	windowsExtendedPrefix    = `\\?\`
	windowsExtendedUNCPrefix = `\\?\UNC\`
	// Keep the extended-length prefix near Win32 MAX_PATH so child handle opens
	// continue to work without hitting legacy 260-character path limits.
	windowsShortFinalPathLimit = 240
)

type fileAttributeTagInfo struct {
	FileAttributes uint32
	ReparseTag     uint32
}

// A caller-named container whose final component is a Windows directory symlink
// (mklink /D) or directory junction / mount point (mklink /J) is resolved once
// through the kernel, while other reparse points such as cloud placeholders
// stay rejected. Non-link containers pass through unchanged, and every entry
// inside the container stays no-follow.
func resolveContainerTarget(
	path string,
	info os.FileInfo,
	follow func(string) (os.FileInfo, error),
) (string, os.FileInfo, error) {
	link, err := isWindowsContainerLink(path, info)
	if err != nil {
		return "", nil, errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	if !link {
		if !info.IsDir() {
			return "", nil, outputcap.ErrUnsafeNamespace
		}
		return path, info, nil
	}
	followed, err := follow(path)
	if err != nil || !followed.IsDir() {
		return "", nil, errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	resolved, err := windowsResolvedContainerPath(path)
	if err != nil {
		return "", nil, errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	target, err := os.Lstat(resolved)
	if err != nil || !target.IsDir() || isWindowsReparsePoint(target) || !os.SameFile(followed, target) {
		return "", nil, errors.Join(outputcap.ErrUnsafeNamespace, err)
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
	// Open without FILE_FLAG_OPEN_REPARSE_POINT so the kernel follows symlinks
	// and junctions in one step and returns the resolved target handle.
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
		return "", outputcap.ErrUnsafeNamespace
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

func setDirectoryTimes(d *directory, t time.Time) error { return d.root.Chtimes(".", t, t) }

func openDirectoryHandle(root *os.Root) (*os.File, error) {
	return openWindowsPath(root.Name(), os.O_RDONLY, true)
}
func openFileAt(d *directory, name string, flags int, _ fs.FileMode) (*os.File, error) {
	root, err := d.root.open()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return openWindowsPath(filepath.Join(d.path, name), flags, false)
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
	} else if flags&os.O_RDWR != 0 {
		access |= windows.GENERIC_WRITE | windows.DELETE
	}
	disposition := uint32(windows.OPEN_EXISTING)
	if flags&os.O_CREATE != 0 {
		disposition = windows.CREATE_NEW
	}
	attributes := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT | windows.FILE_FLAG_BACKUP_SEMANTICS)
	h, err := windows.CreateFile(name, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, disposition, attributes, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(h)
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	return os.NewFile(uintptr(h), path), nil
}

// Process-restart retention does not promise a Windows directory fsync barrier.
// Flushed candidate files and atomic replacement bound an interrupted update.
func syncDirectory(*os.File) error { return nil }
func setFileModifiedTime(handle *os.File, t time.Time) error {
	stamp := windows.NsecToFiletime(t.UnixNano())
	err := windows.SetFileTime(windows.Handle(handle.Fd()), nil, nil, &stamp)
	runtime.KeepAlive(handle)
	return err
}
func lockFile(f *os.File) (func() error, error) {
	overlap := new(windows.Overlapped)
	h := windows.Handle(f.Fd())
	err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlap)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return nil, errors.Join(outputcap.ErrNamespaceLockBusy, err)
	}
	if err != nil {
		return nil, err
	}
	return func() error { return windows.UnlockFileEx(h, 0, 1, 0, overlap) }, nil
}

type renameInformation struct {
	Replace uint32
	Root    windows.Handle
	Length  uint32
	Name    [1]uint16
}

func renameHandle(handle windows.Handle, parent *directory, name string, replace bool) error {
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
	if nt, ok := errors.AsType[windows.NTStatus](err); ok {
		err = nt.Errno()
	}
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
		return errors.Join(fs.ErrExist, err)
	}
	if !replace {
		return noReplaceError(err)
	}
	return err
}

func noReplaceError(err error) error {
	if errors.Is(err, windows.ERROR_NOT_SUPPORTED) || errors.Is(err, windows.ERROR_INVALID_FUNCTION) || errors.Is(err, windows.ERROR_CALL_NOT_IMPLEMENTED) {
		return errors.Join(outputcap.ErrOrdinaryOutputUnsupported, err)
	}
	return err
}

func renameFile(from *file, to *directory, name string, replace bool) error {
	opened, err := openFileAt(from.parent, from.name, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer opened.Close()
	expected, e1 := from.handle.Stat()
	actual, e2 := opened.Stat()
	if e1 != nil || e2 != nil || !os.SameFile(expected, actual) {
		return errors.Join(outputcap.ErrUnsafeNamespace, e1, e2)
	}
	return renameHandle(windows.Handle(opened.Fd()), to, name, replace)
}
func renameDirectory(from, to *directory, name string) error {
	f, err := openWindowsPath(from.path, os.O_RDWR, true)
	if err != nil {
		return err
	}
	defer f.Close()
	current, err := f.Stat()
	if err != nil || !os.SameFile(from.info, current) {
		return errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	return renameHandle(windows.Handle(f.Fd()), to, name, false)
}
func linkFile(from *file, to *directory, name string) error {
	return os.Link(filepath.Join(from.parent.path, from.name), filepath.Join(to.path, name))
}
