//go:build windows

package osfs

import (
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"golang.org/x/sys/windows"
)

const (
	windowsRevisionIdentityBytes  = 24
	windowsRevisionCandidateBytes = windowsRevisionIdentityBytes + 24
	windowsFiletimeUnixOffset     = int64(116444736000000000)
)

type windowsRevisionFileIDInfo struct {
	VolumeSerialNumber uint64
	FileID             [16]byte
}

func inspectWindowsPersistentFileIdentity(handle windows.Handle) ([windowsRevisionIdentityBytes]byte, error) {
	var information windowsRevisionFileIDInfo
	if err := windows.GetFileInformationByHandleEx(
		handle, windows.FileIdInfo, (*byte)(unsafe.Pointer(&information)), uint32(unsafe.Sizeof(information)),
	); err != nil {
		return [windowsRevisionIdentityBytes]byte{}, err
	}
	var identity [windowsRevisionIdentityBytes]byte
	binary.BigEndian.PutUint64(identity[0:8], information.VolumeSerialNumber)
	copy(identity[8:], information.FileID[:])
	return identity, nil
}

type windowsRevisionVolume struct {
	filesystem string
	path       string
	driveType  uint32
}

func inspectWindowsRevisionVolume(handle windows.Handle) (windowsRevisionVolume, error) {
	var filesystem [32]uint16
	var flags uint32
	if err := windows.GetVolumeInformationByHandle(
		handle, nil, 0, nil, nil, &flags, &filesystem[0], uint32(len(filesystem)),
	); err != nil {
		return windowsRevisionVolume{}, err
	}
	path, err := finalWindowsHandlePath(handle)
	if err != nil {
		return windowsRevisionVolume{}, err
	}
	volume := filepath.VolumeName(path)
	if volume == "" {
		return windowsRevisionVolume{}, errors.New("windows revision volume path has no volume name")
	}
	root, err := windows.UTF16PtrFromString(volume + `\`)
	if err != nil {
		return windowsRevisionVolume{}, err
	}
	return windowsRevisionVolume{
		filesystem: windows.UTF16ToString(filesystem[:]),
		path:       path,
		driveType:  windows.GetDriveType(root),
	}, nil
}

func validateWindowsLocalRevisionVolume(volume windowsRevisionVolume) error {
	if !strings.EqualFold(volume.filesystem, "NTFS") && !strings.EqualFold(volume.filesystem, "ReFS") {
		return fmt.Errorf("windows filesystem %q is outside the revision-stability support matrix", volume.filesystem)
	}
	if strings.HasPrefix(strings.TrimPrefix(volume.path, `\\?\`), `UNC\`) {
		return errors.New("remote Windows filesystem is outside the revision-stability support matrix")
	}
	if volume.driveType != windows.DRIVE_FIXED && volume.driveType != windows.DRIVE_REMOVABLE {
		return fmt.Errorf("windows drive type %d is outside the revision-stability support matrix", volume.driveType)
	}
	return nil
}

func finalWindowsHandlePath(handle windows.Handle) (string, error) {
	buffer := make([]uint16, 512)
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

type windowsMutationToken struct {
	identity   [windowsRevisionIdentityBytes]byte
	size       uint64
	lastWrite  int64
	changeTime int64
}

func (t windowsMutationToken) sourceIdentityBytes() []byte {
	result := make([]byte, windowsRevisionIdentityBytes)
	copy(result, t.identity[:])
	return result
}

func (t windowsMutationToken) candidateBytes() []byte {
	result := make([]byte, windowsRevisionCandidateBytes)
	copy(result, t.identity[:])
	binary.BigEndian.PutUint64(result[windowsRevisionIdentityBytes:windowsRevisionIdentityBytes+8], t.size)
	binary.BigEndian.PutUint64(result[windowsRevisionIdentityBytes+8:windowsRevisionIdentityBytes+16], uint64(t.lastWrite))
	binary.BigEndian.PutUint64(result[windowsRevisionIdentityBytes+16:windowsRevisionIdentityBytes+24], uint64(t.changeTime))
	return result
}

func (t windowsMutationToken) matches(record catalog.NodeRecord) bool {
	return t.size == record.Entry().ExpectedSize() &&
		subtle.ConstantTimeCompare(record.SourceIdentity().Bytes(), t.sourceIdentityBytes()) == 1 &&
		subtle.ConstantTimeCompare(record.VersionCandidate().Bytes(), t.candidateBytes()) == 1
}

func (t windowsMutationToken) sameOpenedRevision(other windowsMutationToken) bool {
	// ChangeTime closes the catalog-to-stable-open race, but a later rename also
	// changes it even though the write-excluding handle still names the exact
	// original object. Once FILE_SHARE_WRITE is denied, object identity, size,
	// and last-write time are the content invariants that remain meaningful.
	return t.identity == other.identity && t.size == other.size && t.lastWrite == other.lastWrite
}

func (t windowsMutationToken) modifiedTime() (catalog.ModifiedTime, error) {
	unixTicks := t.lastWrite - windowsFiletimeUnixOffset
	seconds := unixTicks / 10_000_000
	remainder := unixTicks % 10_000_000
	if remainder < 0 {
		seconds--
		remainder += 10_000_000
	}
	return catalog.NewModifiedTime(seconds, uint32(remainder*100), catalog.TimePrecisionNanoseconds)
}

type windowsRevisionFile interface {
	Token() (windowsMutationToken, error)
	ReadAt([]byte, int64) (int, error)
	Close() error
}

type windowsRevisionRoot interface {
	OpenStable(string) (windowsRevisionFile, error)
	Identity() ([windowsRevisionIdentityBytes]byte, error)
	Close() error
}

// windowsRevisionPlatform is the syscall boundary. Tests inject it so share
// modes, root selection, mutation cuts, and handle ownership are proven without
// weakening the production native-open path.
type windowsRevisionPlatform interface {
	OpenRoot(string) (windowsRevisionRoot, error)
	Token(*os.File) (windowsMutationToken, error)
}

type nativeWindowsRevisionPlatform struct{}

func (nativeWindowsRevisionPlatform) OpenRoot(path string) (windowsRevisionRoot, error) {
	handle, err := openWindowsRootHandle(path)
	if err != nil {
		return nil, classifyWindowsRootOpenError(err)
	}
	if err := ensureSupportedWindowsRevisionVolume(handle); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	return &nativeWindowsRevisionRoot{handle: handle}, nil
}

func (nativeWindowsRevisionPlatform) Token(file *os.File) (windowsMutationToken, error) {
	token, err := inspectWindowsMutationToken(windows.Handle(file.Fd()))
	return token, classifyWindowsIdentityError(err)
}

type nativeWindowsRevisionRoot struct {
	mu     sync.Mutex
	handle windows.Handle
}

func (r *nativeWindowsRevisionRoot) Identity() ([windowsRevisionIdentityBytes]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handle == 0 || r.handle == windows.InvalidHandle {
		return [windowsRevisionIdentityBytes]byte{}, content.ErrRevisionStoreClosed
	}
	identity, err := inspectWindowsPersistentFileIdentity(r.handle)
	return identity, classifyWindowsIdentityError(err)
}

func (r *nativeWindowsRevisionRoot) OpenStable(relative string) (windowsRevisionFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handle == 0 || r.handle == windows.InvalidHandle {
		return nil, content.ErrRevisionStoreClosed
	}
	handle, err := openWindowsRelativeStableHandle(r.handle, relative)
	if err != nil {
		return nil, classifyWindowsStableOpenError(err)
	}
	file := os.NewFile(uintptr(handle), relative)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("wrap Windows stable revision handle")
	}
	return &nativeWindowsRevisionFile{file: file}, nil
}

func (r *nativeWindowsRevisionRoot) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handle == 0 || r.handle == windows.InvalidHandle {
		return nil
	}
	handle := r.handle
	r.handle = windows.InvalidHandle
	return windows.CloseHandle(handle)
}

type nativeWindowsRevisionFile struct{ file *os.File }

func (f *nativeWindowsRevisionFile) Token() (windowsMutationToken, error) {
	token, err := inspectWindowsMutationToken(windows.Handle(f.file.Fd()))
	return token, classifyWindowsIdentityError(err)
}

func (f *nativeWindowsRevisionFile) ReadAt(destination []byte, offset int64) (int, error) {
	return f.file.ReadAt(destination, offset)
}

func (f *nativeWindowsRevisionFile) Close() error { return f.file.Close() }

type windowsFileBasicInfo struct {
	CreationTime   int64
	LastAccessTime int64
	LastWriteTime  int64
	ChangeTime     int64
	FileAttributes uint32
	_              uint32
}

func inspectWindowsMutationToken(handle windows.Handle) (windowsMutationToken, error) {
	identity, err := inspectWindowsPersistentFileIdentity(handle)
	if err != nil {
		return windowsMutationToken{}, err
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return windowsMutationToken{}, err
	}
	if information.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return windowsMutationToken{}, content.ErrRevisionStale
	}
	var basic windowsFileBasicInfo
	if err := windows.GetFileInformationByHandleEx(
		handle, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic)),
	); err != nil {
		return windowsMutationToken{}, err
	}
	size := uint64(information.FileSizeHigh)<<32 | uint64(information.FileSizeLow)
	if size > catalog.MaxFileSize {
		return windowsMutationToken{}, content.ErrRevisionStale
	}
	return windowsMutationToken{
		identity: identity,
		size:     size, lastWrite: basic.LastWriteTime, changeTime: basic.ChangeTime,
	}, nil
}

func inspectWindowsCatalogToken(handle windows.Handle) (windowsMutationToken, error) {
	identity, err := inspectWindowsPersistentFileIdentity(handle)
	if err != nil {
		return windowsMutationToken{}, err
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return windowsMutationToken{}, err
	}
	if information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return windowsMutationToken{}, content.ErrRevisionStale
	}
	var basic windowsFileBasicInfo
	if err := windows.GetFileInformationByHandleEx(
		handle, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic)),
	); err != nil {
		return windowsMutationToken{}, err
	}
	var size uint64
	if information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		size = uint64(information.FileSizeHigh)<<32 | uint64(information.FileSizeLow)
		if size > catalog.MaxFileSize {
			return windowsMutationToken{}, content.ErrRevisionStale
		}
	}
	return windowsMutationToken{
		identity: identity, size: size, lastWrite: basic.LastWriteTime, changeTime: basic.ChangeTime,
	}, nil
}

func openWindowsRootHandle(path string) (windows.Handle, error) {
	name, err := windows.NewNTUnicodeString(windowsNTPath(path))
	if err != nil {
		return windows.InvalidHandle, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), ObjectName: name,
		Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&handle, windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		attributes, &status, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0, 0,
	)
	return handle, normalizeWindowsNTError(err)
}

func openWindowsRelativeStableHandle(root windows.Handle, relative string) (windows.Handle, error) {
	if !filepath.IsLocal(relative) || filepath.IsAbs(relative) {
		return windows.InvalidHandle, content.ErrRevisionStale
	}
	name, err := windows.NewNTUnicodeString(relative)
	if err != nil {
		return windows.InvalidHandle, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: root, ObjectName: name,
		Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&handle, windowsStableDesiredAccess(), attributes, &status, nil, 0,
		// Denying FILE_SHARE_WRITE is the Windows stability proof. Sharing
		// delete preserves ordinary rename semantics while volume/file ID keeps
		// the opened object authoritative after a path replacement.
		windowsStableShareMode(),
		windows.FILE_OPEN,
		windowsStableOpenOptions(),
		0, 0,
	)
	return handle, normalizeWindowsNTError(err)
}

func windowsStableShareMode() uint32 {
	return windows.FILE_SHARE_READ | windows.FILE_SHARE_DELETE
}

func windowsStableDesiredAccess() uint32 { return windows.FILE_GENERIC_READ }

func windowsStableOpenOptions() uint32 {
	return windows.FILE_NON_DIRECTORY_FILE | windows.FILE_OPEN_REPARSE_POINT |
		windows.FILE_RANDOM_ACCESS | windows.FILE_SYNCHRONOUS_IO_NONALERT
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

func classifyWindowsStableOpenError(err error) error {
	switch {
	case errors.Is(err, windows.ERROR_SHARING_VIOLATION):
		return errors.Join(content.ErrUnsupportedStability, err)
	case errors.Is(err, windows.ERROR_INVALID_PARAMETER), errors.Is(err, windows.ERROR_NOT_SUPPORTED),
		errors.Is(err, windows.ERROR_CALL_NOT_IMPLEMENTED):
		return errors.Join(content.ErrUnsupportedStability, err)
	case errors.Is(err, windows.ERROR_FILE_NOT_FOUND), errors.Is(err, windows.ERROR_PATH_NOT_FOUND),
		errors.Is(err, windows.ERROR_CANT_ACCESS_FILE), errors.Is(err, windows.ERROR_REPARSE),
		errors.Is(err, windows.ERROR_REPARSE_OBJECT), errors.Is(err, windows.ERROR_REPARSE_POINT_ENCOUNTERED):
		return errors.Join(content.ErrRevisionStale, err)
	default:
		return err
	}
}

func classifyWindowsIdentityError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) ||
		errors.Is(err, windows.ERROR_CALL_NOT_IMPLEMENTED) {
		return errors.Join(content.ErrUnsupportedStability, err)
	}
	return err
}

func classifyWindowsRootOpenError(err error) error {
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) ||
		errors.Is(err, windows.ERROR_CALL_NOT_IMPLEMENTED) {
		return errors.Join(content.ErrUnsupportedStability, err)
	}
	return err
}

func ensureSupportedWindowsRevisionVolume(handle windows.Handle) error {
	volume, err := inspectWindowsRevisionVolume(handle)
	if err == nil {
		err = validateWindowsLocalRevisionVolume(volume)
	}
	if err != nil {
		return errors.Join(content.ErrUnsupportedStability, err)
	}
	return nil
}
