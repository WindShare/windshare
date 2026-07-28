//go:build linux

package outputlinux

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	linuxExt4SuperMagic         = 0xef53
	linuxMountInfoPath          = "/proc/self/mountinfo"
	linuxProcessStatusPath      = "/proc/self/status"
	linuxMaximumMountInfoBytes  = 4 << 20
	linuxMaximumStatusBytes     = 1 << 20
	linuxOutputDirectoryMode    = 0o700
	linuxOutputStateFileMode    = 0o600
	dirPerm                     = 0o755
	filePerm                    = 0o644
	linuxOutputPermissionMask   = 0o7777
	linuxOutputUmaskMask        = 0o777
	linuxOutputNameMaximumBytes = 255
	linuxFSImmutableFlag        = uint32(0x00000010)
	linuxFSAppendFlag           = uint32(0x00000020)
	linuxFSEncryptFlag          = uint32(0x00000800)
	linuxFSProjectInheritFlag   = uint32(0x20000000)
	linuxFSCasefoldFlag         = uint32(0x40000000)
)

var (
	errLinuxOutputUnsupported = errors.New("osfs: Linux recoverable output is unsupported")
	errLinuxOutputUnsafe      = errors.New("osfs: Linux output namespace is unsafe")
	errLinuxOutputCollision   = errors.New("osfs: Linux output entry already exists")
)

type linuxOutputUnsupportedError struct {
	operation string
	reason    string
	cause     error
}

func (failure *linuxOutputUnsupportedError) Error() string {
	return fmt.Sprintf("osfs: %s: Linux recoverable output is unsupported: %s", failure.operation, failure.reason)
}

func (failure *linuxOutputUnsupportedError) Unwrap() error { return failure.cause }

func (failure *linuxOutputUnsupportedError) Is(target error) bool {
	return target == errLinuxOutputUnsupported
}

type linuxOutputUnsafeError struct {
	operation string
	reason    string
	cause     error
}

func (failure *linuxOutputUnsafeError) Error() string {
	return fmt.Sprintf("osfs: %s: Linux output namespace is unsafe: %s", failure.operation, failure.reason)
}

func (failure *linuxOutputUnsafeError) Unwrap() error { return failure.cause }

func (failure *linuxOutputUnsafeError) Is(target error) bool { return target == errLinuxOutputUnsafe }

type linuxOutputCollisionError struct {
	operation string
	name      string
	cause     error
}

func (failure *linuxOutputCollisionError) Error() string {
	return fmt.Sprintf("osfs: %s %q: Linux output entry already exists", failure.operation, failure.name)
}

func (failure *linuxOutputCollisionError) Unwrap() error { return failure.cause }

func (failure *linuxOutputCollisionError) Is(target error) bool {
	return target == errLinuxOutputCollision
}

func linuxUnsupported(operation, reason string, cause error) error {
	return &linuxOutputUnsupportedError{operation: operation, reason: reason, cause: cause}
}

func linuxUnsafe(operation, reason string, cause error) error {
	return &linuxOutputUnsafeError{operation: operation, reason: reason, cause: cause}
}

type linuxOutputSystem struct {
	openat2           func(int, string, *unix.OpenHow) (int, error)
	close             func(int) error
	statx             func(int, string, int, int, *unix.Statx_t) error
	fstatfs           func(int, *unix.Statfs_t) error
	mkdirat           func(int, string, uint32) error
	linkat            func(int, string, int, string, int) error
	renameat2         func(int, string, int, string, uint) error
	unlinkat          func(int, string, int) error
	fsync             func(int) error
	fchmod            func(int, uint32) error
	ftruncate         func(int, int64) error
	pread             func(int, []byte, int64) (int, error)
	pwrite            func(int, []byte, int64) (int, error)
	utimensat         func(int, string, []unix.Timespec, int) error
	faccessat2        func(int, string, uint32, int) error
	fgetxattr         func(int, string, []byte) (int, error)
	geteuid           func() int
	readDirent        func(int, []byte) (int, error)
	flock             func(int, int) error
	getVersion        func(int) (uint32, error)
	getFlags          func(int) (uint32, error)
	getFilesystemUUID func(int) ([linuxFilesystemUUIDBytes]byte, error)
	restartIdentity   linuxDirectoryRestartIdentityProvider
	readMountInfo     func() ([]byte, error)
	readProcessStatus func() ([]byte, error)
}

var linuxHostOutputSystem = linuxOutputSystem{
	openat2:           unix.Openat2,
	close:             unix.Close,
	statx:             unix.Statx,
	fstatfs:           unix.Fstatfs,
	mkdirat:           unix.Mkdirat,
	linkat:            unix.Linkat,
	renameat2:         unix.Renameat2,
	unlinkat:          unix.Unlinkat,
	fsync:             unix.Fsync,
	fchmod:            unix.Fchmod,
	ftruncate:         unix.Ftruncate,
	pread:             unix.Pread,
	pwrite:            unix.Pwrite,
	utimensat:         unix.UtimesNanoAt,
	faccessat2:        unix.Faccessat2,
	fgetxattr:         unix.Fgetxattr,
	geteuid:           unix.Geteuid,
	readDirent:        unix.ReadDirent,
	flock:             unix.Flock,
	getVersion:        linuxGetInodeGeneration,
	getFlags:          linuxGetInodeFlags,
	getFilesystemUUID: linuxGetFilesystemUUID,
	restartIdentity:   linuxStatxBirthTimeRestartIdentityProvider{},
	readMountInfo:     linuxReadMountInfo,
	readProcessStatus: linuxReadProcessStatus,
}

type linuxOutputDurability uint8

const linuxOutputProcessRestartDurability linuxOutputDurability = iota + 1

type linuxMountIdentity struct {
	uniqueMountID       uint64
	deviceMajor         uint32
	deviceMinor         uint32
	runtimeFilesystemID [2]int32
	filesystemUUID      [linuxFilesystemUUIDBytes]byte
}

type linuxOutputCertificate struct {
	mount               linuxMountIdentity
	rootObject          linuxOpenHandleIdentity
	rootRestartIdentity linuxDirectoryRestartIdentity
	durability          linuxOutputDurability
}

type linuxOutputDirectory struct {
	system                  *linuxOutputSystem
	fd                      int
	certificate             linuxOutputCertificate
	object                  linuxOpenHandleIdentity
	absolutePath            string
	exactPermissions        uint32
	requireExactPermissions bool
}

func linuxOpenExt4OutputRoot(path string, system *linuxOutputSystem) (*linuxOutputDirectory, error) {
	const operation = "open output root"
	if system == nil {
		return nil, linuxUnsupported(operation, "native syscall provider is absent", nil)
	}
	if !filepath.IsAbs(path) {
		return nil, linuxUnsafe(operation, "output root must be absolute so authority does not depend on process cwd", nil)
	}
	cleanPath := filepath.Clean(path)
	how := unix.OpenHow{
		Flags:   uint64(unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW),
		Resolve: uint64(unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS),
	}
	fd, err := system.openat2(unix.AT_FDCWD, cleanPath, &how)
	if err != nil {
		return nil, linuxClassifyOpenError(operation, err)
	}
	certificate, err := linuxCertifyExt4OutputFD(system, fd)
	if err != nil {
		return nil, errors.Join(err, system.close(fd))
	}
	if _, err := linuxCertifyAbsoluteOutputPlacement(cleanPath, system, certificate); err != nil {
		return nil, errors.Join(err, system.close(fd))
	}
	root := &linuxOutputDirectory{
		system:       system,
		fd:           fd,
		certificate:  certificate,
		object:       certificate.rootObject,
		absolutePath: cleanPath,
	}
	if err := root.validateExclusiveChildMutationAuthority(); err != nil {
		return nil, errors.Join(err, root.close())
	}
	return root, nil
}

func linuxCertifyExt4OutputFD(system *linuxOutputSystem, fd int) (linuxOutputCertificate, error) {
	const operation = "certify output filesystem"
	legacy, err := linuxReadOpenHandleFacts(system, fd, unix.STATX_MNT_ID)
	if err != nil {
		return linuxOutputCertificate{}, err
	}
	if legacy.identity.kind != unix.S_IFDIR {
		return linuxOutputCertificate{}, linuxUnsafe(operation, "output root handle is not a directory", nil)
	}
	unique, err := linuxReadOpenHandleFacts(system, fd, unix.STATX_MNT_ID_UNIQUE)
	if err != nil {
		return linuxOutputCertificate{}, err
	}
	// The legacy and unique statx queries intentionally return different mount
	// ID domains. The inode comparison detects an object swap between them
	// without treating that domain difference as a mount change.
	if !legacy.identity.sameInodeObject(unique.identity) {
		return linuxOutputCertificate{}, linuxUnsafe(operation, "mount or root object changed during certification", nil)
	}
	if err := linuxVerifyOpenDirectoryFlags(system, fd, operation); err != nil {
		return linuxOutputCertificate{}, err
	}
	var filesystem unix.Statfs_t
	if err := system.fstatfs(fd, &filesystem); err != nil {
		return linuxOutputCertificate{}, fmt.Errorf("%s: inspect filesystem: %w", operation, err)
	}
	// Statfs_t.Type follows the native Linux ABI and is narrower on 32-bit
	// architectures, so the explicit widening is required outside amd64.
	//nolint:unconvert
	filesystemType := int64(filesystem.Type)
	if filesystemType != linuxExt4SuperMagic {
		return linuxOutputCertificate{}, linuxUnsupported(operation, "filesystem superblock is not ext4", nil)
	}
	mountInfo, err := system.readMountInfo()
	if err != nil {
		return linuxOutputCertificate{}, linuxUnsupported(operation, "mount table cannot be inspected", err)
	}
	mount, err := linuxFindMountInfo(mountInfo, legacy.identity.mountID)
	if err != nil {
		return linuxOutputCertificate{}, linuxUnsafe(operation, "mount table is malformed or does not contain the open root", err)
	}
	if mount.filesystemType != "ext4" {
		// ext2, ext3, and ext4 share a statfs magic number. The kernel mount
		// table is the additional discriminator that keeps this allowlist exact.
		return linuxOutputCertificate{}, linuxUnsupported(operation, "mounted filesystem type is not ext4", nil)
	}
	if mount.deviceMajor != legacy.identity.deviceMajor || mount.deviceMinor != legacy.identity.deviceMinor {
		return linuxOutputCertificate{}, linuxUnsafe(operation, "mount table device does not match the open root", nil)
	}
	if system.readProcessStatus == nil {
		return linuxOutputCertificate{}, linuxUnsupported(operation,
			"process umask provider is unavailable", nil)
	}
	processStatus, err := system.readProcessStatus()
	if err != nil {
		return linuxOutputCertificate{}, linuxUnsupported(operation,
			"process umask cannot be inspected before private creation", err)
	}
	processUmask, err := linuxParseProcessUmask(processStatus)
	if err != nil {
		return linuxOutputCertificate{}, linuxUnsupported(operation,
			"process umask is unavailable or malformed", err)
	}
	// Private directories and files request only owner bits. If the process
	// umask preserves those bits, their 0700/0600 modes are exact at the create
	// syscall itself, so termination before the defensive fchmod remains a
	// recoverable cut rather than an inaccessible reserved namespace.
	if processUmask&linuxOutputDirectoryMode != 0 {
		return linuxOutputCertificate{}, linuxUnsupported(operation,
			"process umask masks required private owner permissions", nil)
	}
	if system.getFilesystemUUID == nil {
		return linuxOutputCertificate{}, linuxUnsupported(operation,
			"ext4 filesystem UUID provider is unavailable", nil)
	}
	filesystemUUID, err := system.getFilesystemUUID(fd)
	if err != nil {
		return linuxOutputCertificate{}, linuxUnsupported(operation,
			"ext4 filesystem UUID is unavailable", err)
	}
	if filesystemUUID == [linuxFilesystemUUIDBytes]byte{} {
		return linuxOutputCertificate{}, linuxUnsupported(operation,
			"ext4 filesystem UUID is all zero", nil)
	}
	mountIdentity := linuxMountIdentity{
		uniqueMountID:       unique.identity.mountID,
		deviceMajor:         unique.identity.deviceMajor,
		deviceMinor:         unique.identity.deviceMinor,
		runtimeFilesystemID: filesystem.Fsid.Val,
		filesystemUUID:      filesystemUUID,
	}
	if system.restartIdentity == nil {
		return linuxOutputCertificate{}, linuxUnsupported(operation,
			"directory restart-identity provider is unavailable", nil)
	}
	restartIdentity, err := system.restartIdentity.Read(system, fd, mountIdentity)
	if err != nil {
		return linuxOutputCertificate{}, err
	}
	if !restartIdentity.matchesHandle(unique.identity) {
		return linuxOutputCertificate{}, linuxUnsafe(operation,
			"restart identity differs from the certified root handle", nil)
	}
	return linuxOutputCertificate{
		mount:               mountIdentity,
		rootObject:          unique.identity,
		rootRestartIdentity: restartIdentity,
		durability:          linuxOutputProcessRestartDurability,
	}, nil
}

func linuxReadOpenHandleFacts(system *linuxOutputSystem, fd int, mountMask int) (linuxOpenHandleFacts, error) {
	const operation = "inspect open output object"
	if system == nil || system.statx == nil {
		return linuxOpenHandleFacts{}, linuxUnsupported(operation, "statx provider is unavailable", nil)
	}
	requested := unix.STATX_TYPE | unix.STATX_MODE | unix.STATX_INO | unix.STATX_SIZE | unix.STATX_UID | mountMask
	var stat unix.Statx_t
	err := system.statx(fd, "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW, requested, &stat)
	if err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
			return linuxOpenHandleFacts{}, linuxUnsupported(operation, "required statx identity is unavailable", err)
		}
		return linuxOpenHandleFacts{}, fmt.Errorf("%s: %w", operation, err)
	}
	requiredMask := uint32(
		unix.STATX_TYPE | unix.STATX_MODE | unix.STATX_INO | unix.STATX_SIZE | unix.STATX_UID | mountMask,
	)
	if stat.Mask&requiredMask != requiredMask {
		return linuxOpenHandleFacts{}, linuxUnsupported(operation, "kernel did not return the requested non-reused mount identity", nil)
	}
	return linuxOpenHandleFacts{
		identity: linuxOpenHandleIdentity{
			mountID: stat.Mnt_id, deviceMajor: stat.Dev_major, deviceMinor: stat.Dev_minor,
			inode: stat.Ino, kind: linuxFileType(stat.Mode),
		},
		mode: stat.Mode, size: stat.Size, ownerUID: stat.Uid,
	}, nil
}

func linuxVerifyOpenDirectoryFlags(system *linuxOutputSystem, fd int, operation string) error {
	if system.getFlags == nil {
		return linuxUnsupported(operation, "ext4 inode flag provider is unavailable", nil)
	}
	flags, err := system.getFlags(fd)
	if err != nil {
		return linuxUnsupported(operation, "ext4 directory flags are unavailable", err)
	}
	if flags&linuxFSCasefoldFlag != 0 {
		return linuxUnsupported(operation,
			"casefold directories are outside the certified byte-exact namespace", nil)
	}
	if flags&linuxFSEncryptFlag != 0 {
		return linuxUnsupported(operation,
			"fscrypt directories cannot preserve cross-policy publication and restart authority", nil)
	}
	if flags&linuxFSProjectInheritFlag != 0 {
		return linuxUnsupported(operation,
			"project-inheriting directories can reject publication across project identities", nil)
	}
	if flags&(linuxFSImmutableFlag|linuxFSAppendFlag) != 0 {
		return linuxUnsupported(operation,
			"immutable or append-only directories cannot satisfy recoverable mutation cuts", nil)
	}
	return nil
}

func linuxGetInodeGeneration(fd int) (uint32, error) {
	request := linuxReadLongIOCTL('v', 1)
	return unix.IoctlGetUint32(fd, request)
}

func linuxGetInodeFlags(fd int) (uint32, error) {
	return unix.IoctlGetUint32(fd, linuxReadLongIOCTL('f', 1))
}

type linuxFilesystemUUIDResponse struct {
	length uint8
	uuid   [linuxFilesystemUUIDBytes]byte
}

func linuxGetFilesystemUUID(fd int) ([linuxFilesystemUUIDBytes]byte, error) {
	response := linuxFilesystemUUIDResponse{}
	request := linuxReadSizedIOCTL(0x15, 0, linuxFilesystemUUIDResponseBytes)
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL, uintptr(fd), uintptr(request), uintptr(unsafe.Pointer(&response)),
	)
	if errno != 0 {
		return [linuxFilesystemUUIDBytes]byte{}, errno
	}
	if response.length != linuxFilesystemUUIDBytes {
		return [linuxFilesystemUUIDBytes]byte{}, fmt.Errorf(
			"filesystem UUID length is %d, want %d", response.length, linuxFilesystemUUIDBytes,
		)
	}
	if response.uuid == [linuxFilesystemUUIDBytes]byte{} {
		return [linuxFilesystemUUIDBytes]byte{}, errors.New("filesystem UUID is all zero")
	}
	return response.uuid, nil
}

func linuxReadLongIOCTL(kind byte, number uint) uint {
	return linuxReadSizedIOCTL(uint(kind), number, uint(unsafe.Sizeof(uintptr(0))))
}

func linuxReadSizedIOCTL(kind, number, size uint) uint {
	const (
		linuxIOCDirectionMask = uint(0xe0000000)
		linuxIOCSizeShift     = uint(16)
		linuxIOCTypeShift     = uint(8)
	)
	// FS_IOC_GETFLAGS supplies the architecture's native _IOR direction bits;
	// mips, powerpc, and sparc do not use the generic Linux direction encoding.
	readDirection := uint(unix.FS_IOC_GETFLAGS) & linuxIOCDirectionMask
	return readDirection | size<<linuxIOCSizeShift | kind<<linuxIOCTypeShift | number
}

func linuxVerifyOpenObject(
	system *linuxOutputSystem,
	fd int,
	certificate linuxOutputCertificate,
) (linuxOpenHandleFacts, error) {
	const operation = "verify open output object"
	identity, err := linuxReadOpenHandleFacts(system, fd, unix.STATX_MNT_ID_UNIQUE)
	if err != nil {
		return linuxOpenHandleFacts{}, err
	}
	var filesystem unix.Statfs_t
	if err := system.fstatfs(fd, &filesystem); err != nil {
		return linuxOpenHandleFacts{}, fmt.Errorf("%s: inspect filesystem: %w", operation, err)
	}
	//nolint:unconvert // Statfs_t.Type is int32 on supported 32-bit Linux ABIs.
	filesystemType := int64(filesystem.Type)
	mount := certificate.mount
	if identity.identity.mountID != mount.uniqueMountID ||
		identity.identity.deviceMajor != mount.deviceMajor || identity.identity.deviceMinor != mount.deviceMinor ||
		filesystem.Fsid.Val != mount.runtimeFilesystemID || filesystemType != linuxExt4SuperMagic {
		return linuxOpenHandleFacts{}, linuxUnsafe(operation, "object crossed or changed the certified ext4 mount", nil)
	}
	if identity.identity.kind == unix.S_IFDIR {
		if err := linuxVerifyOpenDirectoryFlags(system, fd, operation); err != nil {
			return linuxOpenHandleFacts{}, err
		}
	}
	return identity, nil
}

type linuxMountInfoRecord struct {
	mountID        uint64
	deviceMajor    uint32
	deviceMinor    uint32
	filesystemType string
}

func linuxFindMountInfo(data []byte, targetID uint64) (linuxMountInfoRecord, error) {
	var match linuxMountInfoRecord
	found := false
	for rawLine := range bytes.SplitSeq(data, []byte{'\n'}) {
		if len(rawLine) == 0 {
			continue
		}
		record, err := linuxParseMountInfoLine(string(rawLine))
		if err != nil {
			return linuxMountInfoRecord{}, err
		}
		if record.mountID != targetID {
			continue
		}
		if found {
			return linuxMountInfoRecord{}, errors.New("duplicate mount ID")
		}
		match = record
		found = true
	}
	if !found {
		return linuxMountInfoRecord{}, errors.New("mount ID not found")
	}
	return match, nil
}

func linuxParseMountInfoLine(line string) (linuxMountInfoRecord, error) {
	fields := strings.Fields(line)
	separator := -1
	for index, field := range fields {
		if field == "-" {
			separator = index
			break
		}
	}
	if len(fields) < 10 || separator < 6 || separator+3 >= len(fields) {
		return linuxMountInfoRecord{}, errors.New("invalid mountinfo field layout")
	}
	mountID, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return linuxMountInfoRecord{}, fmt.Errorf("parse mount ID: %w", err)
	}
	deviceParts := strings.Split(fields[2], ":")
	if len(deviceParts) != 2 {
		return linuxMountInfoRecord{}, errors.New("invalid mountinfo device")
	}
	deviceMajor, err := strconv.ParseUint(deviceParts[0], 10, 32)
	if err != nil {
		return linuxMountInfoRecord{}, fmt.Errorf("parse mount device major: %w", err)
	}
	deviceMinor, err := strconv.ParseUint(deviceParts[1], 10, 32)
	if err != nil {
		return linuxMountInfoRecord{}, fmt.Errorf("parse mount device minor: %w", err)
	}
	return linuxMountInfoRecord{
		mountID:        mountID,
		deviceMajor:    uint32(deviceMajor),
		deviceMinor:    uint32(deviceMinor),
		filesystemType: fields[separator+1],
	}, nil
}

func linuxReadMountInfo() ([]byte, error) {
	return linuxReadBoundedProcFile(linuxMountInfoPath, linuxMaximumMountInfoBytes)
}

func linuxReadProcessStatus() ([]byte, error) {
	return linuxReadBoundedProcFile(linuxProcessStatusPath, linuxMaximumStatusBytes)
}

func linuxReadBoundedProcFile(path string, maximumBytes int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maximumBytes+1))
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, err
	}
	if int64(len(data)) > maximumBytes {
		return nil, errors.New("proc metadata exceeds safety limit")
	}
	return data, nil
}

func linuxParseProcessUmask(data []byte) (uint32, error) {
	var result uint32
	found := false
	for rawLine := range bytes.SplitSeq(data, []byte{'\n'}) {
		fields := strings.Fields(string(rawLine))
		if len(fields) == 0 || fields[0] != "Umask:" {
			continue
		}
		if found || len(fields) != 2 {
			return 0, errors.New("invalid process Umask field")
		}
		parsed, err := strconv.ParseUint(fields[1], 8, 32)
		if err != nil || parsed > linuxOutputUmaskMask {
			return 0, errors.Join(errors.New("invalid process Umask value"), err)
		}
		result = uint32(parsed)
		found = true
	}
	if !found {
		return 0, errors.New("process Umask field is absent")
	}
	return result, nil
}

func linuxFileType(mode uint16) uint16 { return mode & uint16(unix.S_IFMT) }

func linuxClassifyOpenError(operation string, err error) error {
	switch {
	case errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EINVAL), errors.Is(err, unix.EOPNOTSUPP):
		return linuxUnsupported(operation, "openat2 safety constraints are unavailable", err)
	case errors.Is(err, unix.EXDEV):
		return linuxUnsafe(operation, "path crosses a mount boundary", err)
	case errors.Is(err, unix.ELOOP):
		return linuxUnsafe(operation, "path contains a symbolic or magic link", err)
	default:
		return fmt.Errorf("%s: %w", operation, err)
	}
}
