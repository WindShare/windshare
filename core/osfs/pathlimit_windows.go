package osfs

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"github.com/windshare/windshare/core/transfer"
	"golang.org/x/sys/windows"
)

// windowsMaxPath is Win32 MAX_PATH measured in UTF-16 code units including the
// trailing NUL. Extended-path prefixes are intentionally excluded because an
// output ordinary desktop tools cannot reopen is not a usable successful result.
const windowsMaxPath = 260

// ERROR_FILENAME_EXCED_RANGE is the Win32 error returned by APIs that reject a
// path or component before Go can translate it to syscall.ENAMETOOLONG.
const windowsErrorFilenameExceedsRange syscall.Errno = 206

const windowsErrorDirectoryNotEmpty syscall.Errno = 145

const windowsErrorLockViolation syscall.Errno = 33

const windowsPersistentFileIdentityBytes = 24

const windowsVolumeNameGUID = 1

const windowsFileSupportsPOSIXUnlinkRename = 0x00000400

var (
	outputLockFileExW   = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
	outputUnlockFileExW = syscall.NewLazyDLL("kernel32.dll").NewProc("UnlockFileEx")
)

func isReparsePoint(information fs.FileInfo) bool {
	if information.Mode()&fs.ModeSymlink != 0 {
		return true
	}
	// Junctions, mount points, and cloud placeholders do not consistently map
	// to ModeSymlink, so the native attribute is the authoritative no-follow
	// boundary on Windows.
	native, ok := information.Sys().(*syscall.Win32FileAttributeData)
	return ok && native.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

const (
	outputLockExclusive     = 0x2
	outputLockFailImmediate = 0x1
)

func exceedsPathLimit(abs string) bool {
	// Go strings count UTF-8 bytes, while Win32 counts UTF-16 code units; the
	// additional unit accounts for the trailing NUL.
	return len(utf16.Encode([]rune(abs)))+1 > windowsMaxPath
}

func isPathTooLongError(err error) bool {
	return errors.Is(err, syscall.ENAMETOOLONG) || errors.Is(err, windowsErrorFilenameExceedsRange)
}

func isDirectoryNotEmptyError(err error) bool {
	return errors.Is(err, windowsErrorDirectoryNotEmpty)
}

func outputPlatformCapabilities() (transfer.DurabilityLevel, bool) {
	// Root-relative namespace changes are atomic across process restarts, but
	// Windows exposes no unprivileged retained-directory flush with which to
	// prove the directory entry survived sudden power loss.
	return transfer.DurabilityProcessRestart, true
}

func lockOutputFile(file *os.File) error {
	var overlapped syscall.Overlapped
	result, _, callErr := outputLockFileExW.Call(
		file.Fd(), outputLockExclusive|outputLockFailImmediate, 0, 1, 0,
		uintptr(unsafe.Pointer(&overlapped)),
	)
	runtime.KeepAlive(file)
	if result != 0 {
		return nil
	}
	if errors.Is(callErr, windowsErrorLockViolation) {
		return errors.Join(ErrOutputSessionActive, callErr)
	}
	if errors.Is(callErr, syscall.Errno(0)) {
		callErr = syscall.EINVAL
	}
	return callErr
}

func unlockOutputFile(file *os.File) error {
	var overlapped syscall.Overlapped
	result, _, callErr := outputUnlockFileExW.Call(file.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	runtime.KeepAlive(file)
	if result != 0 {
		return nil
	}
	if errors.Is(callErr, syscall.Errno(0)) {
		callErr = syscall.EINVAL
	}
	return callErr
}

func outputObjectIdentity(file *os.File) (transfer.OutputObjectIdentity, error) {
	if file == nil {
		return transfer.OutputObjectIdentity{}, ErrOwnedFileMissing
	}
	raw, err := inspectWindowsPersistentFileIdentity(windows.Handle(file.Fd()))
	if err != nil {
		return transfer.OutputObjectIdentity{}, err
	}
	return windowsOutputObjectIdentity(raw)
}

func windowsOutputObjectIdentity(raw [windowsPersistentFileIdentityBytes]byte) (transfer.OutputObjectIdentity, error) {
	digest := sha256.Sum256(append([]byte("windshare/output-object/windows/v1\x00"), raw[:]...))
	return transfer.OutputObjectIdentityFromBytes(digest[:])
}

type windowsFileIDInfo struct {
	VolumeSerialNumber uint64
	FileID             [16]byte
}

func inspectWindowsPersistentFileIdentity(handle windows.Handle) ([windowsPersistentFileIdentityBytes]byte, error) {
	var information windowsFileIDInfo
	if err := windows.GetFileInformationByHandleEx(
		handle, windows.FileIdInfo, (*byte)(unsafe.Pointer(&information)), uint32(unsafe.Sizeof(information)),
	); err != nil {
		return [windowsPersistentFileIdentityBytes]byte{}, err
	}
	var identity [windowsPersistentFileIdentityBytes]byte
	binary.BigEndian.PutUint64(identity[0:8], information.VolumeSerialNumber)
	copy(identity[8:], information.FileID[:])
	return identity, nil
}

func outputRootLocator(_ string, directory *os.File) (string, error) {
	if directory == nil {
		return "", transfer.ErrInvalidOutputBinding
	}
	path, err := finalWindowsPathWithFlags(windows.Handle(directory.Fd()), windowsVolumeNameGUID)
	if err != nil {
		return "", err
	}
	// The persistent object identity distinguishes case-sensitive directories;
	// folding only prevents ordinary Win32 spelling aliases from orphaning a
	// valid session journal after restart.
	return strings.ToLower(filepath.Clean(path)), nil
}

func validateOutputRootPlatform(directory *os.File) error {
	if directory == nil {
		return transfer.ErrInvalidOutputBinding
	}
	volume, err := inspectWindowsVolume(windows.Handle(directory.Fd()))
	if err != nil {
		return errors.Join(ErrUnsupportedOutputVolume, err)
	}
	return validateWindowsOutputVolume(volume)
}

func validateWindowsOutputVolume(volume windowsVolume) error {
	if err := validateWindowsLocalPersistentVolume(volume); err != nil {
		return errors.Join(ErrUnsupportedOutputVolume, err)
	}
	if volume.flags&windows.FILE_SUPPORTS_HARD_LINKS == 0 {
		return fmt.Errorf("%w: filesystem %q does not support hard links", ErrUnsupportedOutputVolume, volume.filesystem)
	}
	if volume.flags&windowsFileSupportsPOSIXUnlinkRename == 0 {
		return fmt.Errorf("%w: filesystem %q does not support handle-bound unlink", ErrUnsupportedOutputVolume, volume.filesystem)
	}
	return nil
}

type windowsVolume struct {
	filesystem string
	path       string
	driveType  uint32
	flags      uint32
}

func inspectWindowsVolume(handle windows.Handle) (windowsVolume, error) {
	var filesystem [32]uint16
	var flags uint32
	if err := windows.GetVolumeInformationByHandle(handle, nil, 0, nil, nil, &flags, &filesystem[0], uint32(len(filesystem))); err != nil {
		return windowsVolume{}, err
	}
	name := windows.UTF16ToString(filesystem[:])
	path, err := finalWindowsPath(handle)
	if err != nil {
		return windowsVolume{}, err
	}
	volume := filepath.VolumeName(path)
	if volume == "" {
		return windowsVolume{}, errors.New("windows volume path has no volume name")
	}
	rootPath := volume + `\`
	root, err := windows.UTF16PtrFromString(rootPath)
	if err != nil {
		return windowsVolume{}, err
	}
	return windowsVolume{filesystem: name, path: path, driveType: windows.GetDriveType(root), flags: flags}, nil
}

func validateWindowsLocalPersistentVolume(volume windowsVolume) error {
	if !strings.EqualFold(volume.filesystem, "NTFS") && !strings.EqualFold(volume.filesystem, "ReFS") {
		return fmt.Errorf("windows filesystem %q is outside the persistent-identity support matrix", volume.filesystem)
	}
	if strings.HasPrefix(strings.TrimPrefix(volume.path, `\\?\`), `UNC\`) {
		return errors.New("remote Windows filesystem is outside the support matrix")
	}
	if volume.driveType != windows.DRIVE_FIXED && volume.driveType != windows.DRIVE_REMOVABLE {
		return fmt.Errorf("windows drive type %d is outside the support matrix", volume.driveType)
	}
	return nil
}

func finalWindowsPath(handle windows.Handle) (string, error) {
	return finalWindowsPathWithFlags(handle, 0)
}

func finalWindowsPathWithFlags(handle windows.Handle, flags uint32) (string, error) {
	buffer := make([]uint16, 512)
	for {
		length, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), flags)
		if err != nil {
			return "", err
		}
		if length < uint32(len(buffer)) {
			return windows.UTF16ToString(buffer[:length]), nil
		}
		buffer = make([]uint16, length+1)
	}
}

type windowsFileDispositionInfoEx struct{ Flags uint32 }

func openOutputRemovalFile(root *os.Root, path string) (*os.File, error) {
	if root == nil || !filepath.IsLocal(path) || filepath.IsAbs(path) {
		return nil, ErrPathEscape
	}
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	name, err := windows.NewNTUnicodeString(path)
	if err != nil {
		return nil, errors.Join(err, directory.Close())
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: windows.Handle(directory.Fd()), ObjectName: name,
		Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&handle, windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		attributes, &status, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0, 0,
	)
	closeErr := directory.Close()
	if err != nil || closeErr != nil {
		if err == nil {
			_ = windows.CloseHandle(handle)
		}
		return nil, errors.Join(normalizeWindowsNTError(err), closeErr)
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("wrap Windows output removal handle")
	}
	return file, nil
}

func removeOpenedOutputFile(_ *os.Root, _ string, file *os.File) error {
	if file == nil {
		return ErrOwnedFileMissing
	}
	disposition := windowsFileDispositionInfoEx{Flags: windows.FILE_DISPOSITION_DELETE |
		windows.FILE_DISPOSITION_POSIX_SEMANTICS | windows.FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE}
	return windows.SetFileInformationByHandle(
		windows.Handle(file.Fd()), windows.FileDispositionInfoEx,
		(*byte)(unsafe.Pointer(&disposition)), uint32(unsafe.Sizeof(disposition)),
	)
}

func installOutputJournal(root *os.Root, tempName, targetName string) error {
	return root.Rename(tempName, targetName)
}

func publishOutputFile(root *os.Root, stage, target string) error {
	// Link is the root-confined, atomic no-replace primitive on Windows. If a
	// crash cuts between link and stage removal, recovery verifies their shared
	// object identity and completes publication.
	if err := root.Link(stage, target); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return categorizedPathFailure("publish output", target, ErrAlreadyExists, err)
		}
		return err
	}
	return root.Remove(stage)
}

func removeOutputJournal(root *os.Root, name string) error {
	if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
