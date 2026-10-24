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

type windowsLinkRenameInformation struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

func windowsLinkRenameBuffer(flags uint32, root windows.Handle, name string) ([]byte, error) {
	encoded, err := windows.UTF16FromString(name)
	if err != nil {
		return nil, err
	}
	encoded = encoded[:len(encoded)-1]
	var layout windowsLinkRenameInformation
	headerSize := int(unsafe.Offsetof(layout.FileName))
	buffer := make([]byte, headerSize+len(encoded)*2)
	information := (*windowsLinkRenameInformation)(unsafe.Pointer(&buffer[0]))
	information.Flags = flags
	information.RootDirectory = root
	information.FileNameLength = uint32(len(encoded) * 2)
	copy(unsafe.Slice(&information.FileName[0], len(encoded)), encoded)
	return buffer, nil
}

func (directory *windowsDirectory) RemoveRegularLink(relative string, expected *windowsFile) error {
	return directory.removeRegularLink(relative, expected, directory.private)
}

func (directory *windowsDirectory) RemoveOrdinaryProfileLink(relative string, expected *windowsFile) error {
	return directory.removeRegularLink(relative, expected, false)
}

func (directory *windowsDirectory) removeRegularLink(
	relative string,
	expected *windowsFile,
	private bool,
) error {
	current, err := directory.openFileForDelete(relative, private)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	same, compareErr := sameWindowsOpenedObject(current, expected)
	if compareErr != nil || !same {
		return errors.Join(windowsFailure("remove output link", relative, errWindowsOutputUnsafe,
			errors.New("current directory entry differs from the expected open object")), compareErr, current.Close())
	}
	removeErr := windowsRemoveHandle(current.handle())
	return errors.Join(removeErr, current.Close())
}

func (directory *windowsDirectory) RemoveDirectory(relative string, expected *windowsDirectory) error {
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
	same, compareErr := sameWindowsOpenedDirectory(current, expected)
	if compareErr != nil || !same {
		return errors.Join(windowsFailure("remove output directory", relative, errWindowsOutputUnsafe,
			errors.New("current directory entry differs from the expected open directory")), compareErr, current.Close())
	}
	removeErr := windowsRemoveHandle(current.handle())
	return errors.Join(removeErr, current.Close())
}

type windowsDispositionInformation struct{ Flags uint32 }

func windowsRemoveHandle(handle windows.Handle) error {
	information := windowsDispositionInformation{Flags: windows.FILE_DISPOSITION_DELETE |
		windows.FILE_DISPOSITION_POSIX_SEMANTICS | windows.FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE}
	err := windows.SetFileInformationByHandle(
		handle, windows.FileDispositionInfoEx, (*byte)(unsafe.Pointer(&information)), uint32(unsafe.Sizeof(information)),
	)
	if err != nil {
		return windowsNativeOperationFailure("remove opened output object", "", err)
	}
	return nil
}

type windowsStableLock struct {
	mu         sync.Mutex
	file       *windowsFile
	overlapped windows.Overlapped
	closed     bool
}

func (directory *windowsDirectory) AcquireStableLock(relative string) (*windowsStableLock, bool, error) {
	file, created, err := directory.openOrCreatePrivateFile(relative)
	if err != nil {
		return nil, false, err
	}
	lock, err := windowsLockStableFile(file, relative)
	return lock, created, err
}

func (directory *windowsDirectory) AcquireExistingStableLock(relative string) (*windowsStableLock, error) {
	file, err := directory.OpenPrivateFile(relative)
	if err != nil {
		return nil, err
	}
	lock, err := windowsLockStableFile(file, relative)
	return lock, err
}

func windowsLockStableFile(file *windowsFile, relative string) (*windowsStableLock, error) {
	lock := &windowsStableLock{file: file}
	err := windows.LockFileEx(
		file.handle(), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &lock.overlapped,
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return nil, errors.Join(windowsFailure(
				"lock stable output authority", relative, errWindowsOutputLockBusy, err,
			), file.Close())
		}
		return nil, errors.Join(windowsNativeOperationFailure(
			"lock stable output authority", relative, err,
		), file.Close())
	}
	return lock, nil
}

func (lock *windowsStableLock) Close() error {
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

func windowsOpenNative(
	root windows.Handle,
	name string,
	access uint32,
	disposition uint32,
	typeOption uint32,
	attributes uint32,
	descriptor *windows.SECURITY_DESCRIPTOR,
) (windows.Handle, uintptr, error) {
	return windowsOpenNativeWithOptions(
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

func windowsOpenNativeWithOptions(
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
	return handle, status.Information, normalizeWindowsNTError(err)
}

func windowsNTPath(path string) string {
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

func normalizeWindowsNTError(err error) error {
	if err == nil {
		return nil
	}
	if status, ok := errors.AsType[windows.NTStatus](err); ok {
		return status.Errno()
	}
	return err
}

func windowsDirectoryAccess() uint32 {
	return windows.FILE_LIST_DIRECTORY | windows.FILE_TRAVERSE | windowsDirectoryAddFile |
		windowsDirectoryAddSubdirectory | windowsDirectoryDeleteChild | windows.FILE_READ_ATTRIBUTES |
		windows.FILE_WRITE_ATTRIBUTES | windows.READ_CONTROL | windows.DELETE | windows.SYNCHRONIZE
}

func windowsPublicDirectoryAccess() uint32 {
	// Public containers keep their ordinary inherited ACL. Admission asks only
	// for the rights used by public traversal and no-replace publication, so
	// unrelated metadata or delete policy cannot disable safe output.
	return windows.FILE_LIST_DIRECTORY | windows.FILE_TRAVERSE | windowsDirectoryAddFile |
		windowsDirectoryAddSubdirectory | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE
}

func windowsOpenedDirectoryAccess(placementGuard bool) uint32 {
	if placementGuard {
		return windowsPublicDirectoryAccess()
	}
	return windowsDirectoryAccess()
}

func windowsDirectoryShareMode(placementGuard bool) uint32 {
	share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE)
	if !placementGuard {
		share |= windows.FILE_SHARE_DELETE
	}
	return share
}

func windowsRootDirectoryAccess() uint32 {
	return windowsPublicDirectoryAccess()
}

func windowsPrivatePublicationRootAccess() uint32 {
	// The retained root must mutate children but never itself. Omitting DELETE
	// avoids rejecting ordinary readers that request delete sharing while the
	// no-delete-share handle still pins this exact private placement.
	return windowsDirectoryAccess() &^ windows.DELETE
}

func windowsPrivateRootParentAccess() uint32 {
	// The child starts delete-on-close and carries its own DELETE authority, so
	// rollback never needs ambient FILE_DELETE_CHILD. Excluding FILE_ADD_FILE as
	// well keeps this capability unable to create any non-directory entry.
	return windowsPublicDirectoryAccess() &^ windowsDirectoryAddFile
}

func windowsPrivateFileAccess() uint32 {
	return windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE | windows.FILE_READ_ATTRIBUTES |
		windows.FILE_WRITE_ATTRIBUTES | windows.READ_CONTROL | windows.DELETE | windows.SYNCHRONIZE
}

func windowsReadFileAccess() uint32 {
	return windows.FILE_GENERIC_READ | windows.READ_CONTROL | windows.SYNCHRONIZE
}

func windowsRecoveryDurabilityFileAccess() uint32 {
	// FlushFileBuffers requires one write-class right. Append authority is the
	// narrowest NTFS right that permits the flush while withholding arbitrary
	// offset writes, truncation, deletion, and metadata mutation.
	return windows.FILE_APPEND_DATA | windows.FILE_READ_ATTRIBUTES |
		windows.READ_CONTROL | windows.SYNCHRONIZE
}

func windowsDeleteFileAccess() uint32 {
	return windows.DELETE | windows.FILE_READ_ATTRIBUTES | windows.READ_CONTROL | windows.SYNCHRONIZE
}

func windowsCreationStatus(disposition uint32, status uintptr) (bool, error) {
	switch disposition {
	case windows.FILE_CREATE:
		if status != windowsFileCreated {
			return false, fmt.Errorf("exclusive create returned status %d", status)
		}
		return true, nil
	case windows.FILE_OPEN:
		if status != windowsFileOpened {
			return false, fmt.Errorf("open returned status %d", status)
		}
		return false, nil
	case windows.FILE_OPEN_IF:
		switch status {
		case windowsFileCreated:
			return true, nil
		case windowsFileOpened:
			return false, nil
		default:
			return false, fmt.Errorf("open-or-create returned status %d", status)
		}
	default:
		return false, fmt.Errorf("unsupported create disposition %d", disposition)
	}
}

func windowsRelativePath(path string, leafOnly bool) (string, error) {
	if path == "" || strings.ContainsRune(path, 0) || strings.Contains(path, ":") {
		return "", errors.New("empty, NUL, and alternate-stream paths are forbidden")
	}
	native := filepath.FromSlash(path)
	if !filepath.IsLocal(native) || filepath.IsAbs(native) || native == "." || filepath.Clean(native) != native {
		return "", errors.New("path is not a canonical root-relative name")
	}
	if len(utf16.Encode([]rune(native))) > windowsMaximumNTNameUTF16Units {
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
		if len(utf16.Encode([]rune(component))) > windowsMaximumComponentUTF16Units {
			return "", errors.New("path component exceeds the certified NTFS UTF-16 limit")
		}
		if windowsReservedComponent(component) {
			return "", errors.New("path component is reserved or not representable through Win32")
		}
	}
	return native, nil
}

func windowsReservedComponent(component string) bool {
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

func windowsIsCollision(err error) bool {
	return errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS)
}

func windowsIsUnsupportedNative(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) ||
		errors.Is(err, windows.ERROR_CALL_NOT_IMPLEMENTED)
}

var _ io.ReaderAt = (*windowsFile)(nil)
var _ io.WriterAt = (*windowsFile)(nil)
