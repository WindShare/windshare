//go:build windows

package outputwindows

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsV3LinkRenameInformation struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

func windowsV3LinkRenameBuffer(flags uint32, root windows.Handle, name string) ([]byte, error) {
	encoded, err := windows.UTF16FromString(name)
	if err != nil {
		return nil, err
	}
	encoded = encoded[:len(encoded)-1]
	var layout windowsV3LinkRenameInformation
	headerSize := int(unsafe.Offsetof(layout.FileName))
	buffer := make([]byte, headerSize+len(encoded)*2)
	information := (*windowsV3LinkRenameInformation)(unsafe.Pointer(&buffer[0]))
	information.Flags = flags
	information.RootDirectory = root
	information.FileNameLength = uint32(len(encoded) * 2)
	copy(unsafe.Slice(&information.FileName[0], len(encoded)), encoded)
	return buffer, nil
}

func (directory *windowsV3Directory) RemoveRegularLink(relative string, expected *windowsV3File) error {
	return directory.removeRegularLink(relative, expected, directory.private)
}

func (directory *windowsV3Directory) RemoveOrdinaryProfileLink(relative string, expected *windowsV3File) error {
	return directory.removeRegularLink(relative, expected, false)
}

func (directory *windowsV3Directory) removeRegularLink(
	relative string,
	expected *windowsV3File,
	private bool,
) error {
	current, err := directory.openFileForDelete(relative, private)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	same, compareErr := sameWindowsV3OpenedObject(current, expected)
	if compareErr != nil || !same {
		return errors.Join(windowsV3Failure("remove output link", relative, errWindowsV3OutputUnsafe,
			errors.New("current directory entry differs from the expected open object")), compareErr, current.Close())
	}
	removeErr := windowsV3RemoveHandle(current.handle())
	return errors.Join(removeErr, current.Close())
}

func (directory *windowsV3Directory) RemoveDirectory(relative string, expected *windowsV3Directory) error {
	// The child's authority decides its security and placement policy. A private
	// probe directly beneath the public root must remain movable for cleanup,
	// while an arbitrary public child must never be silently downgraded.
	private := directory.private
	if expected != nil {
		private = expected.private
	}
	current, err := directory.openDirectory(relative, private, windows.FILE_OPEN)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	same, compareErr := sameWindowsV3OpenedDirectory(current, expected)
	if compareErr != nil || !same {
		return errors.Join(windowsV3Failure("remove output directory", relative, errWindowsV3OutputUnsafe,
			errors.New("current directory entry differs from the expected open directory")), compareErr, current.Close())
	}
	removeErr := windowsV3RemoveHandle(current.handle())
	return errors.Join(removeErr, current.Close())
}

type windowsV3DispositionInformation struct{ Flags uint32 }

func windowsV3RemoveHandle(handle windows.Handle) error {
	information := windowsV3DispositionInformation{Flags: windows.FILE_DISPOSITION_DELETE |
		windows.FILE_DISPOSITION_POSIX_SEMANTICS | windows.FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE}
	err := windows.SetFileInformationByHandle(
		handle, windows.FileDispositionInfoEx, (*byte)(unsafe.Pointer(&information)), uint32(unsafe.Sizeof(information)),
	)
	if err != nil {
		return windowsV3NativeOperationFailure("remove opened output object", "", err)
	}
	return nil
}

type windowsV3StableLock struct {
	mu         sync.Mutex
	file       *windowsV3File
	overlapped windows.Overlapped
	closed     bool
}

func (directory *windowsV3Directory) AcquireStableLock(relative string) (*windowsV3StableLock, bool, error) {
	file, created, err := directory.openOrCreatePrivateFile(relative)
	if err != nil {
		return nil, false, err
	}
	lock, err := windowsV3LockStableFile(file, relative)
	return lock, created, err
}

func (directory *windowsV3Directory) AcquireExistingStableLock(relative string) (*windowsV3StableLock, error) {
	file, err := directory.OpenPrivateFile(relative)
	if err != nil {
		return nil, err
	}
	lock, err := windowsV3LockStableFile(file, relative)
	return lock, err
}

func windowsV3LockStableFile(file *windowsV3File, relative string) (*windowsV3StableLock, error) {
	lock := &windowsV3StableLock{file: file}
	err := windows.LockFileEx(
		file.handle(), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &lock.overlapped,
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return nil, errors.Join(windowsV3Failure(
				"lock stable output authority", relative, errWindowsV3OutputLockBusy, err,
			), file.Close())
		}
		return nil, errors.Join(windowsV3NativeOperationFailure(
			"lock stable output authority", relative, err,
		), file.Close())
	}
	return lock, nil
}

func (lock *windowsV3StableLock) Close() error {
	if lock == nil {
		return nil
	}
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.closed {
		return nil
	}
	lock.closed = true
	if lock.file == nil || lock.file.file == nil {
		return nil
	}
	unlockErr := windows.UnlockFileEx(lock.file.handle(), 0, 1, 0, &lock.overlapped)
	closeErr := lock.file.Close()
	lock.file = nil
	return errors.Join(unlockErr, closeErr)
}

func windowsV3OpenNative(
	root windows.Handle,
	name string,
	access uint32,
	disposition uint32,
	typeOption uint32,
	attributes uint32,
	descriptor *windows.SECURITY_DESCRIPTOR,
) (windows.Handle, uintptr, error) {
	return windowsV3OpenNativeWithOptions(
		root,
		name,
		access,
		disposition,
		typeOption,
		attributes,
		descriptor,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.OBJ_CASE_INSENSITIVE|windows.OBJ_DONT_REPARSE,
	)
}

func windowsV3OpenNativeWithOptions(
	root windows.Handle,
	name string,
	access uint32,
	disposition uint32,
	typeOption uint32,
	attributes uint32,
	descriptor *windows.SECURITY_DESCRIPTOR,
	shareMode uint32,
	objectAttributes uint32,
) (windows.Handle, uintptr, error) {
	nativeName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windows.InvalidHandle, 0, err
	}
	object := &windows.OBJECT_ATTRIBUTES{
		Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: root,
		ObjectName: nativeName, Attributes: objectAttributes,
		SecurityDescriptor: descriptor,
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	createOptions := typeOption | windows.FILE_OPEN_REPARSE_POINT
	if access&windows.SYNCHRONIZE != 0 {
		createOptions |= windows.FILE_SYNCHRONOUS_IO_NONALERT
	}
	err = windows.NtCreateFile(
		&handle, access, object, &status, nil, attributes,
		shareMode,
		disposition, createOptions,
		0, 0,
	)
	runtime.KeepAlive(descriptor)
	return handle, status.Information, normalizeWindowsV3NTError(err)
}

func windowsV3NTPath(path string) string {
	clean := filepath.Clean(path)
	switch {
	case strings.HasPrefix(clean, `\\?\UNC\`):
		return `\??\UNC\` + strings.TrimPrefix(clean, `\\?\UNC\`)
	case strings.HasPrefix(clean, `\\?\`):
		return `\??\` + strings.TrimPrefix(clean, `\\?\`)
	case strings.HasPrefix(clean, `\\`):
		return `\??\UNC\` + strings.TrimPrefix(clean, `\\`)
	default:
		return `\??\` + clean
	}
}

func normalizeWindowsV3NTError(err error) error {
	if err == nil {
		return nil
	}
	if status, ok := errors.AsType[windows.NTStatus](err); ok {
		return status.Errno()
	}
	return err
}

func windowsV3DirectoryAccess() uint32 {
	return windows.FILE_LIST_DIRECTORY | windows.FILE_TRAVERSE | windowsV3DirectoryAddFile |
		windowsV3DirectoryAddSubdirectory | windowsV3DirectoryDeleteChild | windows.FILE_READ_ATTRIBUTES |
		windows.FILE_WRITE_ATTRIBUTES | windows.READ_CONTROL | windows.DELETE | windows.SYNCHRONIZE
}

func windowsV3PublicDirectoryAccess() uint32 {
	// Public containers keep their ordinary inherited ACL. Admission asks only
	// for the rights used by public traversal and no-replace publication, so
	// unrelated metadata or delete policy cannot disable safe output.
	return windows.FILE_LIST_DIRECTORY | windows.FILE_TRAVERSE | windowsV3DirectoryAddFile |
		windowsV3DirectoryAddSubdirectory | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE
}

func windowsV3OpenedDirectoryAccess(placementGuard bool) uint32 {
	if placementGuard {
		return windowsV3PublicDirectoryAccess()
	}
	return windowsV3DirectoryAccess()
}

func windowsV3DirectoryShareMode(placementGuard bool) uint32 {
	share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE)
	if !placementGuard {
		share |= windows.FILE_SHARE_DELETE
	}
	return share
}

func windowsV3RootDirectoryAccess() uint32 {
	return windowsV3PublicDirectoryAccess()
}

func windowsV3PrivatePublicationRootAccess() uint32 {
	// The retained root must mutate children but never itself. Omitting DELETE
	// avoids rejecting ordinary readers that request delete sharing while the
	// no-delete-share handle still pins this exact private placement.
	return windowsV3DirectoryAccess() &^ windows.DELETE
}

func windowsV3PrivateRootParentAccess() uint32 {
	// The child starts delete-on-close and carries its own DELETE authority, so
	// rollback never needs ambient FILE_DELETE_CHILD. Excluding FILE_ADD_FILE as
	// well keeps this capability unable to create any non-directory entry.
	return windowsV3PublicDirectoryAccess() &^ windowsV3DirectoryAddFile
}

func windowsV3PrivateFileAccess() uint32 {
	return windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE | windows.FILE_READ_ATTRIBUTES |
		windows.FILE_WRITE_ATTRIBUTES | windows.READ_CONTROL | windows.DELETE | windows.SYNCHRONIZE
}

func windowsV3ReadFileAccess() uint32 {
	return windows.FILE_GENERIC_READ | windows.READ_CONTROL | windows.SYNCHRONIZE
}

func windowsV3RecoveryDurabilityFileAccess() uint32 {
	// FlushFileBuffers requires one write-class right. Append authority is the
	// narrowest NTFS right that permits the flush while withholding arbitrary
	// offset writes, truncation, deletion, and metadata mutation.
	return windows.FILE_APPEND_DATA | windows.FILE_READ_ATTRIBUTES |
		windows.READ_CONTROL | windows.SYNCHRONIZE
}

func windowsV3DeleteFileAccess() uint32 {
	return windows.DELETE | windows.FILE_READ_ATTRIBUTES | windows.READ_CONTROL | windows.SYNCHRONIZE
}

func windowsV3CreationStatus(disposition uint32, status uintptr) (bool, error) {
	switch disposition {
	case windows.FILE_CREATE:
		if status != windowsV3FileCreated {
			return false, fmt.Errorf("exclusive create returned status %d", status)
		}
		return true, nil
	case windows.FILE_OPEN:
		if status != windowsV3FileOpened {
			return false, fmt.Errorf("open returned status %d", status)
		}
		return false, nil
	case windows.FILE_OPEN_IF:
		switch status {
		case windowsV3FileCreated:
			return true, nil
		case windowsV3FileOpened:
			return false, nil
		default:
			return false, fmt.Errorf("open-or-create returned status %d", status)
		}
	default:
		return false, fmt.Errorf("unsupported create disposition %d", disposition)
	}
}

func windowsV3RelativePath(path string, leafOnly bool) (string, error) {
	if path == "" || strings.ContainsRune(path, 0) || strings.Contains(path, ":") {
		return "", errors.New("empty, NUL, and alternate-stream paths are forbidden")
	}
	native := filepath.FromSlash(path)
	if !filepath.IsLocal(native) || filepath.IsAbs(native) || native == "." || filepath.Clean(native) != native {
		return "", errors.New("path is not a canonical root-relative name")
	}
	if len(utf16.Encode([]rune(native))) > windowsV3MaximumNTNameUTF16Units {
		return "", errors.New("path exceeds the NT object-name UTF-16 limit")
	}
	components := strings.Split(native, string(filepath.Separator))
	if leafOnly && len(components) != 1 {
		return "", errors.New("operation requires one directory-entry name")
	}
	for _, component := range components {
		if component == "" || component == "." || component == ".." || strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
			return "", errors.New("path component has an unsafe Win32 alias")
		}
		if len(utf16.Encode([]rune(component))) > windowsV3MaximumComponentUTF16Units {
			return "", errors.New("path component exceeds the certified NTFS UTF-16 limit")
		}
		if windowsV3ReservedComponent(component) {
			return "", errors.New("path component is reserved or not representable through Win32")
		}
	}
	return native, nil
}

func windowsV3ReservedComponent(component string) bool {
	for _, character := range component {
		if character < 0x20 || strings.ContainsRune(`<>"|?*`, character) {
			return true
		}
	}
	base := strings.ToUpper(strings.SplitN(component, ".", 2)[0])
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CLOCK$",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9",
		"COM¹", "COM²", "COM³", "LPT¹", "LPT²", "LPT³":
		return true
	default:
		return false
	}
}

func windowsV3IsCollision(err error) bool {
	return errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS)
}

func windowsV3IsUnsupportedNative(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) ||
		errors.Is(err, windows.ERROR_CALL_NOT_IMPLEMENTED)
}

var _ io.ReaderAt = (*windowsV3File)(nil)
var _ io.WriterAt = (*windowsV3File)(nil)
