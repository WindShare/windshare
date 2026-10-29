//go:build windows

package outputportable

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"
	"unsafe"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/windows"
)

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
