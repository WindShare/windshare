//go:build linux

package outputlinux

import (
	"errors"
	"fmt"
	"math"

	"github.com/windshare/windshare/core/catalog"
	"golang.org/x/sys/unix"
)

type linuxOutputMetadata struct {
	size        uint64
	seconds     int64
	nanoseconds uint32
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
	if timespec.Sec != seconds || timespec.Nsec != nanoseconds {
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
	if stat.Btime.Nsec >= 1_000_000_000 {
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
	if stat.Mtime.Nsec >= 1_000_000_000 {
		return linuxOutputMetadata{}, linuxUnsafe(operation, "filesystem returned an invalid modified time", nil)
	}
	return linuxOutputMetadata{size: stat.Size, seconds: stat.Mtime.Sec, nanoseconds: stat.Mtime.Nsec}, nil
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
