//go:build linux

package outputlinux

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"golang.org/x/sys/unix"
)

const (
	linuxOutputDirectoryReadBufferBytes = 32 << 10
)

var errLinuxOutputLockBusy = errors.New("osfs: Linux output lock is already held")

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
	for component := range strings.SplitSeq(path, "/") {
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

func (directory *linuxOutputDirectory) observeEntry(name string) (outputcap.EntryKind, error) {
	const operation = "observe output entry"
	if err := directory.verifyHandle(); err != nil {
		return outputcap.EntryAbsent, err
	}
	if err := linuxValidateComponent(operation, name); err != nil {
		return outputcap.EntryAbsent, err
	}
	var stat unix.Statx_t
	err := directory.system.statx(directory.fd, name, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_TYPE, &stat)
	if errors.Is(err, fs.ErrNotExist) {
		return outputcap.EntryAbsent, nil
	}
	if err != nil {
		return outputcap.EntryAbsent, fmt.Errorf("%s %q: %w", operation, name, err)
	}
	if stat.Mask&uint32(unix.STATX_TYPE) == 0 {
		return outputcap.EntryAbsent, linuxUnsupported(operation, "filesystem omitted the entry type", nil)
	}
	switch linuxFileType(stat.Mode) {
	case unix.S_IFREG:
		return outputcap.EntryRegularFile, nil
	case unix.S_IFDIR:
		return outputcap.EntryDirectory, nil
	default:
		return outputcap.EntryOther, nil
	}
}

func (directory *linuxOutputDirectory) classifyExactEntry(
	name string,
) (outputcap.EntryKind, bool, error) {
	kind, err := directory.observeEntry(name)
	if err != nil {
		return outputcap.EntryAbsent, false, err
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
		return nil, errors.Join(outputfault.ErrAncestryAuthorityDenied, err)
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

func linuxOutputEntryKind(mode uint16) outputcap.EntryKind {
	switch linuxFileType(mode) {
	case unix.S_IFREG:
		return outputcap.EntryRegularFile
	case unix.S_IFDIR:
		return outputcap.EntryDirectory
	default:
		return outputcap.EntryOther
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

func linuxSameOpenDirectory(left, right *linuxOutputDirectory) (bool, error) {
	if err := linuxVerifyDirectoryPair(left, right); err != nil {
		return false, err
	}
	return left.object.sameObject(right.object), nil
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
