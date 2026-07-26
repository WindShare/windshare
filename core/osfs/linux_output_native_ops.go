//go:build linux

package osfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"golang.org/x/sys/unix"
)

const (
	linuxOutputDirectoryReadBufferBytes = 32 << 10
)

var errLinuxOutputLockBusy = errors.New("osfs: Linux output lock is already held")

type linuxOutputMetadata struct {
	size        uint64
	seconds     int64
	nanoseconds uint32
}

func (directory *linuxOutputDirectory) Duplicate() (*linuxOutputDirectory, error) {
	const operation = "duplicate output directory authority"
	if err := directory.verifyHandle(); err != nil {
		return nil, err
	}
	fd, err := directory.openRelative(".", unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	identity, err := linuxVerifyOpenObject(directory.system, fd, directory.certificate)
	if err != nil || identity.identity.kind != unix.S_IFDIR || !identity.matches(directory.object) {
		if err == nil {
			err = linuxUnsafe(operation, "duplicated handle does not identify the fixed directory", nil)
		}
		return nil, errors.Join(err, directory.system.close(fd))
	}
	return &linuxOutputDirectory{
		system: directory.system, fd: fd, certificate: directory.certificate, object: identity.identity,
		absolutePath: directory.absolutePath, exactPermissions: directory.exactPermissions,
		requireExactPermissions: directory.requireExactPermissions,
	}, nil
}

func (directory *linuxOutputDirectory) SameDirectory(other *linuxOutputDirectory) (bool, error) {
	return linuxSameOpenDirectory(directory, other)
}

func linuxOutputLocatorKey(path string) (string, error) {
	canonical, err := catalog.CanonicalPath(path)
	if err != nil || canonical != path || !filepath.IsLocal(path) || filepath.Clean(path) != path {
		return "", errors.Join(linuxUnsafe("validate output locator", "locator is not a canonical relative path", nil), err)
	}
	for _, component := range strings.Split(path, "/") {
		if err := linuxValidateComponent("validate output locator", component); err != nil {
			return "", err
		}
	}
	return path, nil
}

func (directory *linuxOutputDirectory) names(limit int) ([]string, error) {
	return directory.namesMatching(limit, func(string) bool { return true })
}

func (directory *linuxOutputDirectory) namesWithPrefix(prefix string, limit int) ([]string, error) {
	return directory.namesMatching(limit, func(name string) bool {
		return len(name) >= len(prefix) && name[:len(prefix)] == prefix
	})
}

func (directory *linuxOutputDirectory) namesMatching(
	limit int,
	include func(string) bool,
) (_ []string, resultErr error) {
	const operation = "enumerate output directory"
	if limit < 0 || include == nil {
		return nil, linuxUnsafe(operation, "enumeration bound or filter is invalid", nil)
	}
	if err := directory.verifyHandle(); err != nil {
		return nil, err
	}
	// Reopening "." from the fixed handle creates an independent directory
	// cursor. dup(2) would share the cursor and make repeated recovery scans
	// depend on earlier enumeration.
	fd, err := directory.openRelative(".", unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.system.close(fd)) }()
	identity, err := linuxVerifyOpenObject(directory.system, fd, directory.certificate)
	if err != nil {
		return nil, err
	}
	if identity.identity.kind != unix.S_IFDIR || !identity.matches(directory.object) {
		return nil, linuxUnsafe(operation, "enumeration cursor does not identify the fixed directory", nil)
	}

	buffer := make([]byte, linuxOutputDirectoryReadBufferBytes)
	names := make([]string, 0, min(limit, 16))
	for {
		count, readErr := directory.system.readDirent(fd, buffer)
		if readErr != nil {
			if errors.Is(readErr, unix.ENOSYS) || errors.Is(readErr, unix.EOPNOTSUPP) {
				return nil, linuxUnsupported(operation, "handle-relative directory enumeration is unavailable", readErr)
			}
			return nil, fmt.Errorf("%s: %w", operation, readErr)
		}
		if count == 0 {
			break
		}
		consumed, _, batch := unix.ParseDirent(buffer[:count], -1, nil)
		if consumed != count {
			return nil, linuxUnsafe(operation, "kernel returned a malformed directory entry stream", nil)
		}
		for _, name := range batch {
			if !include(name) {
				continue
			}
			if len(names) == limit {
				return nil, linuxUnsafe(operation, "directory exceeds its declared entry bound", nil)
			}
			names = append(names, name)
		}
	}
	if err := directory.verifyHandle(); err != nil {
		return nil, err
	}
	sort.Strings(names)
	for index := 1; index < len(names); index++ {
		if names[index-1] == names[index] {
			return nil, linuxUnsafe(operation, "directory enumeration returned a duplicate name", nil)
		}
	}
	return names, nil
}

func (directory *linuxOutputDirectory) observeEntry(name string) (outputV3EntryKind, error) {
	const operation = "observe output entry"
	if err := directory.verifyHandle(); err != nil {
		return outputV3EntryAbsent, err
	}
	if err := linuxValidateComponent(operation, name); err != nil {
		return outputV3EntryAbsent, err
	}
	var stat unix.Statx_t
	err := directory.system.statx(directory.fd, name, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_TYPE, &stat)
	if errors.Is(err, fs.ErrNotExist) {
		return outputV3EntryAbsent, nil
	}
	if err != nil {
		return outputV3EntryAbsent, fmt.Errorf("%s %q: %w", operation, name, err)
	}
	if stat.Mask&uint32(unix.STATX_TYPE) == 0 {
		return outputV3EntryAbsent, linuxUnsupported(operation, "filesystem omitted the entry type", nil)
	}
	switch linuxFileType(stat.Mode) {
	case unix.S_IFREG:
		return outputV3EntryRegularFile, nil
	case unix.S_IFDIR:
		return outputV3EntryDirectory, nil
	default:
		return outputV3EntryOther, nil
	}
}

func (directory *linuxOutputDirectory) classifyExactEntry(
	name string,
) (outputV3EntryKind, bool, error) {
	kind, err := directory.observeEntry(name)
	if err != nil {
		return outputV3EntryAbsent, false, err
	}
	// ext4 lookup is byte-exact under the certified backend, so a successful
	// no-follow observation cannot silently resolve a differently spelled leaf.
	return kind, true, nil
}

func (directory *linuxOutputDirectory) prepareIdentityClaim() ([]byte, error) {
	return directory.directoryIdentityClaim(true)
}

func (directory *linuxOutputDirectory) identityClaim() ([]byte, error) {
	return directory.directoryIdentityClaim(false)
}

func (directory *linuxOutputDirectory) directoryIdentityClaim(prepare bool) ([]byte, error) {
	const operation = "claim output directory identity"
	if err := directory.validateExclusiveChildMutationAuthority(); err != nil {
		return nil, errors.Join(errOutputAncestryAuthorityDenied, err)
	}
	identity, err := linuxVerifyOpenObject(directory.system, directory.fd, directory.certificate)
	if err != nil {
		return nil, err
	}
	if identity.identity.kind != unix.S_IFDIR || !identity.matches(directory.object) {
		return nil, linuxUnsafe(operation, "directory changed while its identity was claimed", nil)
	}
	provider := directory.system.restartIdentity
	if provider == nil {
		return nil, linuxUnsupported(operation, "directory restart-identity provider is unavailable", nil)
	}
	var restartIdentity linuxDirectoryRestartIdentity
	if prepare {
		restartIdentity, err = provider.Prepare(directory.system, directory.fd, directory.certificate.mount)
	} else {
		restartIdentity, err = provider.Read(directory.system, directory.fd, directory.certificate.mount)
	}
	if err != nil {
		return nil, err
	}
	if !restartIdentity.matchesHandle(identity.identity) {
		return nil, linuxUnsafe(operation, "restart identity differs from the open directory", nil)
	}
	if directory.object.sameObject(directory.certificate.rootObject) &&
		!restartIdentity.sameDirectory(directory.certificate.rootRestartIdentity) {
		return nil, linuxUnsafe(operation, "certified output-root restart identity changed", nil)
	}
	objectClaim, err := linuxEncodeDirectoryRestartIdentity(restartIdentity)
	if err != nil {
		return nil, err
	}
	if directory.absolutePath == "" {
		return objectClaim, nil
	}
	placementClaim, err := linuxCertifyAbsoluteOutputPlacement(
		directory.absolutePath, directory.system, directory.certificate,
	)
	if err != nil {
		return nil, err
	}
	return linuxEncodeAnchoredDirectoryClaim(placementClaim, objectClaim)
}

func linuxOutputEntryKind(mode uint16) outputV3EntryKind {
	switch linuxFileType(mode) {
	case unix.S_IFREG:
		return outputV3EntryRegularFile
	case unix.S_IFDIR:
		return outputV3EntryDirectory
	default:
		return outputV3EntryOther
	}
}

func (directory *linuxOutputDirectory) namedEntrySnapshotNoFollow(name string) (linuxNamedEntrySnapshot, error) {
	const operation = "inspect opaque output entry"
	requested := unix.STATX_TYPE | unix.STATX_INO | unix.STATX_MNT_ID_UNIQUE
	var stat unix.Statx_t
	if err := directory.system.statx(directory.fd, name, unix.AT_SYMLINK_NOFOLLOW, requested, &stat); err != nil {
		return linuxNamedEntrySnapshot{}, fmt.Errorf("%s %q: %w", operation, name, err)
	}
	if stat.Mask&uint32(requested) != uint32(requested) {
		return linuxNamedEntrySnapshot{}, linuxUnsupported(operation, "filesystem omitted current named identity", nil)
	}
	snapshot := linuxNamedEntrySnapshot{identity: linuxOpenHandleIdentity{
		mountID: stat.Mnt_id, deviceMajor: stat.Dev_major, deviceMinor: stat.Dev_minor,
		inode: stat.Ino, kind: linuxFileType(stat.Mode),
	}}
	mount := directory.certificate.mount
	if snapshot.identity.mountID != mount.uniqueMountID ||
		snapshot.identity.deviceMajor != mount.deviceMajor || snapshot.identity.deviceMinor != mount.deviceMinor {
		return linuxNamedEntrySnapshot{}, linuxUnsafe(operation, "entry crossed the certified ext4 mount", nil)
	}
	return snapshot, nil
}

func (file *linuxOutputRegularFile) ReadAt(destination []byte, offset int64) (int, error) {
	const operation = "read output file"
	if offset < 0 {
		return 0, linuxUnsafe(operation, "offset cannot be negative", nil)
	}
	if err := file.verifyHandle(); err != nil {
		return 0, err
	}
	for {
		count, err := file.system.pread(file.fd, destination, offset)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return count, fmt.Errorf("%s: %w", operation, err)
		}
		if count != len(destination) {
			return count, io.EOF
		}
		return count, nil
	}
}

func (file *linuxOutputRegularFile) WriteAt(source []byte, offset int64) (int, error) {
	const operation = "write output file"
	if offset < 0 {
		return 0, linuxUnsafe(operation, "offset cannot be negative", nil)
	}
	if err := file.requireWritable(operation); err != nil {
		return 0, err
	}
	for {
		count, err := file.system.pwrite(file.fd, source, offset)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return count, fmt.Errorf("%s: %w", operation, err)
		}
		if count != len(source) {
			return count, io.ErrShortWrite
		}
		return count, nil
	}
}

func (file *linuxOutputRegularFile) Size() (uint64, error) {
	identity, err := file.currentIdentity()
	if err != nil {
		return 0, err
	}
	return identity.size, nil
}

func (file *linuxOutputRegularFile) allocatedSize() (uint64, error) {
	const operation = "inspect output file allocation"
	current, err := file.currentIdentity()
	if err != nil {
		return 0, err
	}
	if !file.object.sameObject(current.identity) {
		return 0, linuxUnsafe(operation, "open file no longer matches its fixed incarnation", nil)
	}
	requested := unix.STATX_TYPE | unix.STATX_MODE | unix.STATX_INO | unix.STATX_BLOCKS | unix.STATX_MNT_ID_UNIQUE
	var stat unix.Statx_t
	if err := file.system.statx(
		file.fd, "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW, requested, &stat,
	); err != nil {
		return 0, fmt.Errorf("%s: %w", operation, err)
	}
	if stat.Mask&uint32(requested) != uint32(requested) {
		return 0, linuxUnsupported(operation, "filesystem omitted allocation or current-object identity", nil)
	}
	identity := linuxOpenHandleIdentity{
		mountID: stat.Mnt_id, deviceMajor: stat.Dev_major, deviceMinor: stat.Dev_minor,
		inode: stat.Ino, kind: linuxFileType(stat.Mode),
	}
	// STATX_BLOCKS is obtained separately from the generation-bearing identity.
	// The full handle remains live across both observations, so raw inode equality
	// cannot be satisfied by reuse while this comparison is in progress.
	if identity.kind != unix.S_IFREG || !identity.sameObject(current.identity) {
		return 0, linuxUnsafe(operation, "allocation metadata is outside the fixed file authority", nil)
	}
	const statBlockBytes = uint64(512)
	if stat.Blocks > math.MaxUint64/statBlockBytes {
		return 0, linuxUnsafe(operation, "allocated byte count overflows", nil)
	}
	return stat.Blocks * statBlockBytes, nil
}

func linuxSameOpenDirectory(left, right *linuxOutputDirectory) (bool, error) {
	const operation = "compare open output directories"
	if err := linuxVerifyDirectoryPair(left, right); err != nil {
		return false, err
	}
	return left.object.sameObject(right.object), nil
}

func linuxValidateModifiedTime(modified catalog.ModifiedTime) error {
	if !modified.Present() {
		return nil
	}
	if modified.Nanoseconds() >= 1_000_000_000 || modified.Precision() < catalog.TimePrecisionSeconds ||
		modified.Precision() > catalog.TimePrecisionNanoseconds {
		return linuxUnsafe("validate output modified time", "catalog modified time is invalid", nil)
	}
	_, err := linuxModifiedTimespec(modified)
	return err
}

func linuxModifiedTimeRequiresExtendedInodeFields(modified catalog.ModifiedTime) bool {
	if !modified.Present() {
		return false
	}
	return modified.Nanoseconds() != 0 ||
		modified.Seconds() < math.MinInt32 || modified.Seconds() > math.MaxInt32
}

func linuxModifiedTimespec(modified catalog.ModifiedTime) (unix.Timespec, error) {
	const nanosecondsPerSecond = int64(1_000_000_000)
	seconds := modified.Seconds()
	nanoseconds := int64(modified.Nanoseconds())
	if seconds > math.MaxInt64/nanosecondsPerSecond || seconds < math.MinInt64/nanosecondsPerSecond {
		return unix.Timespec{}, linuxUnsupported("validate output modified time",
			"timestamp exceeds the certified Linux timespec range", nil)
	}
	total := seconds * nanosecondsPerSecond
	if total > math.MaxInt64-nanoseconds {
		return unix.Timespec{}, linuxUnsupported("validate output modified time",
			"timestamp exceeds the certified Linux timespec range", nil)
	}
	timespec := unix.NsecToTimespec(total + nanoseconds)
	if int64(timespec.Sec) != seconds || int64(timespec.Nsec) != nanoseconds {
		return unix.Timespec{}, linuxUnsupported("validate output modified time",
			"timestamp is not exactly representable by the native Linux ABI", nil)
	}
	return timespec, nil
}

func (file *linuxOutputRegularFile) setModifiedTime(modified catalog.ModifiedTime) error {
	const operation = "set output file modified time"
	if err := file.requireWritable(operation); err != nil {
		return err
	}
	if linuxModifiedTimeRequiresExtendedInodeFields(modified) {
		if err := linuxRequireExtendedTimestampLayout(
			file.system, file.fd, file.certificate, unix.S_IFREG, operation,
		); err != nil {
			return err
		}
	}
	return linuxSetHandleModifiedTime(file.system, file.fd, modified, operation)
}

func (directory *linuxOutputDirectory) setModifiedTime(modified catalog.ModifiedTime) error {
	const operation = "set output directory modified time"
	if err := directory.verifyHandle(); err != nil {
		return err
	}
	if linuxModifiedTimeRequiresExtendedInodeFields(modified) {
		if err := linuxRequireExtendedTimestampLayout(
			directory.system, directory.fd, directory.certificate, unix.S_IFDIR, operation,
		); err != nil {
			return err
		}
	}
	return linuxSetHandleModifiedTime(directory.system, directory.fd, modified, operation)
}

func linuxSetHandleModifiedTime(
	system *linuxOutputSystem,
	fd int,
	modified catalog.ModifiedTime,
	operation string,
) error {
	if err := linuxValidateModifiedTime(modified); err != nil {
		return err
	}
	if !modified.Present() {
		return nil
	}
	mtime, err := linuxModifiedTimespec(modified)
	if err != nil {
		return err
	}
	times := []unix.Timespec{
		{Nsec: unix.UTIME_OMIT},
		mtime,
	}
	if err := system.utimensat(fd, "", times, unix.AT_EMPTY_PATH); err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
			return linuxUnsupported(operation, "handle-bound nanosecond timestamps are unavailable", err)
		}
		return fmt.Errorf("%s: %w", operation, err)
	}
	return nil
}

func (file *linuxOutputRegularFile) metadataMatches(
	exactSize uint64,
	modified catalog.ModifiedTime,
) (bool, error) {
	if linuxModifiedTimeRequiresExtendedInodeFields(modified) {
		if err := linuxRequireExtendedTimestampLayout(
			file.system, file.fd, file.certificate, unix.S_IFREG, "inspect output file metadata",
		); err != nil {
			return false, err
		}
	}
	metadata, err := linuxReadHandleMetadata(file.system, file.fd, file.certificate, unix.S_IFREG)
	if err != nil {
		return false, err
	}
	return metadata.size == exactSize && linuxModifiedTimeMatches(metadata, modified), nil
}

func (directory *linuxOutputDirectory) metadataMatches(
	_ uint64,
	modified catalog.ModifiedTime,
) (bool, error) {
	if linuxModifiedTimeRequiresExtendedInodeFields(modified) {
		if err := linuxRequireExtendedTimestampLayout(
			directory.system, directory.fd, directory.certificate, unix.S_IFDIR,
			"inspect output directory metadata",
		); err != nil {
			return false, err
		}
	}
	metadata, err := linuxReadHandleMetadata(directory.system, directory.fd, directory.certificate, unix.S_IFDIR)
	if err != nil {
		return false, err
	}
	return linuxModifiedTimeMatches(metadata, modified), nil
}

func linuxRequireExtendedTimestampLayout(
	system *linuxOutputSystem,
	fd int,
	certificate linuxOutputCertificate,
	expectedType uint16,
	operation string,
) error {
	if system == nil || system.statx == nil {
		return linuxUnsupported(operation, "extended ext4 timestamp-layout provider is unavailable", nil)
	}
	current, err := linuxVerifyOpenObject(system, fd, certificate)
	if err != nil {
		return err
	}
	if linuxFileType(current.mode) != expectedType {
		return linuxUnsafe(operation, "extended timestamp witness has the wrong object type", nil)
	}
	if system.geteuid == nil {
		return linuxUnsupported(operation, "extended timestamp owner provider is unavailable", nil)
	}
	if current.ownerUID != uint32(system.geteuid()) {
		return linuxUnsafe(operation, "extended timestamp witness is not owned by the effective receiver user", nil)
	}

	const identityMask = unix.STATX_TYPE | unix.STATX_MODE | unix.STATX_INO |
		unix.STATX_UID | unix.STATX_MNT_ID_UNIQUE
	requested := identityMask | unix.STATX_BTIME
	var stat unix.Statx_t
	if err := system.statx(
		fd, "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW, requested, &stat,
	); err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
			return linuxUnsupported(operation, "handle-bound extended timestamp layout is unavailable", err)
		}
		return fmt.Errorf("%s: inspect extended timestamp layout: %w", operation, err)
	}
	if stat.Mask&uint32(identityMask) != uint32(identityMask) {
		return linuxUnsupported(operation, "filesystem omitted extended timestamp witness identity", nil)
	}
	if stat.Mask&uint32(unix.STATX_BTIME) == 0 {
		// On ext4, i_crtime follows i_mtime_extra in the on-disk inode. A
		// returned BTIME therefore proves this exact inode can persist the earlier
		// nanosecond/epoch extension rather than only caching it in memory.
		return linuxUnsupported(operation, "ext4 inode lacks persistent extended timestamp fields", nil)
	}
	observed := linuxOpenHandleIdentity{
		mountID: stat.Mnt_id, deviceMajor: stat.Dev_major, deviceMinor: stat.Dev_minor,
		inode: stat.Ino, kind: linuxFileType(stat.Mode),
	}
	if !current.identity.sameObject(observed) || stat.Uid != current.ownerUID {
		return linuxUnsafe(operation, "extended timestamp proof escaped or changed the fixed object authority", nil)
	}
	if stat.Btime.Nsec < 0 || stat.Btime.Nsec >= 1_000_000_000 {
		return linuxUnsafe(operation, "filesystem returned an invalid birth time", nil)
	}
	return nil
}

func linuxReadHandleMetadata(
	system *linuxOutputSystem,
	fd int,
	certificate linuxOutputCertificate,
	expectedType uint16,
) (linuxOutputMetadata, error) {
	const operation = "inspect output metadata"
	requested := unix.STATX_TYPE | unix.STATX_INO | unix.STATX_SIZE | unix.STATX_MTIME | unix.STATX_MNT_ID_UNIQUE
	var stat unix.Statx_t
	if err := system.statx(fd, "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW, requested, &stat); err != nil {
		return linuxOutputMetadata{}, fmt.Errorf("%s: %w", operation, err)
	}
	if stat.Mask&uint32(requested) != uint32(requested) {
		return linuxOutputMetadata{}, linuxUnsupported(operation, "filesystem omitted required size, time, or identity fields", nil)
	}
	identity := linuxOpenHandleIdentity{
		mountID: stat.Mnt_id, deviceMajor: stat.Dev_major, deviceMinor: stat.Dev_minor,
		inode: stat.Ino, kind: linuxFileType(stat.Mode),
	}
	if identity.mountID != certificate.mount.uniqueMountID ||
		identity.deviceMajor != certificate.mount.deviceMajor || identity.deviceMinor != certificate.mount.deviceMinor {
		return linuxOutputMetadata{}, linuxUnsafe(operation, "metadata handle crossed the certified mount", nil)
	}
	if identity.kind != expectedType {
		return linuxOutputMetadata{}, linuxUnsafe(operation, "metadata handle has the wrong object type", nil)
	}
	if stat.Mtime.Nsec < 0 || stat.Mtime.Nsec >= 1_000_000_000 {
		return linuxOutputMetadata{}, linuxUnsafe(operation, "filesystem returned an invalid modified time", nil)
	}
	return linuxOutputMetadata{size: stat.Size, seconds: stat.Mtime.Sec, nanoseconds: uint32(stat.Mtime.Nsec)}, nil
}

func linuxModifiedTimeMatches(actual linuxOutputMetadata, expected catalog.ModifiedTime) bool {
	if !expected.Present() {
		return true
	}
	if actual.seconds != expected.Seconds() {
		return false
	}
	switch expected.Precision() {
	case catalog.TimePrecisionSeconds:
		return true
	case catalog.TimePrecisionMilliseconds:
		return actual.nanoseconds/1_000_000 == expected.Nanoseconds()/1_000_000
	case catalog.TimePrecisionNanoseconds:
		return actual.nanoseconds == expected.Nanoseconds()
	default:
		return false
	}
}

type linuxOutputStableLock struct {
	mu     sync.Mutex
	file   *linuxOutputRegularFile
	closed bool
}

func (directory *linuxOutputDirectory) acquireExistingStableLock(name string) (*linuxOutputStableLock, error) {
	file, err := directory.openRegularFileExact(name, true, linuxOutputStateFileMode)
	if err != nil {
		return nil, err
	}
	lock, err := linuxLockStableFile(file)
	if err != nil {
		return nil, errors.Join(err, file.close())
	}
	matches, err := directory.regularEntryMatches(name, file)
	if err != nil || !matches {
		return nil, errors.Join(
			linuxUnsafe("lock stable output authority", "lock name does not identify the locked object", nil),
			err,
			lock.Close(),
		)
	}
	return lock, nil
}

func linuxLockStableFile(file *linuxOutputRegularFile) (*linuxOutputStableLock, error) {
	const operation = "lock stable output authority"
	if err := file.requireWritable(operation); err != nil {
		return nil, err
	}
	if err := file.system.flock(file.fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, errors.Join(errLinuxOutputLockBusy, err)
		}
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) {
			return nil, linuxUnsupported(operation, "kernel-backed stable locking is unavailable", err)
		}
		return nil, fmt.Errorf("%s: %w", operation, err)
	}
	return &linuxOutputStableLock{file: file}, nil
}

func (lock *linuxOutputStableLock) Close() error {
	if lock == nil {
		return nil
	}
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.closed {
		return nil
	}
	lock.closed = true
	if lock.file == nil || lock.file.fd < 0 {
		return nil
	}
	unlockErr := lock.file.system.flock(lock.file.fd, unix.LOCK_UN)
	closeErr := lock.file.close()
	lock.file = nil
	return errors.Join(unlockErr, closeErr)
}

var _ io.ReaderAt = (*linuxOutputRegularFile)(nil)
var _ io.WriterAt = (*linuxOutputRegularFile)(nil)
