//go:build linux

package outputlinux

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

const (
	linuxMountIdentityClaimDomain            = "linux/ext4/mount-instance/v2"
	linuxDirectoryRestartIdentityClaimDomain = "linux/ext4/directory-restart/statx-btime/v1"
	linuxFilesystemUUIDBytes                 = 16
	linuxFilesystemUUIDResponseBytes         = 1 + linuxFilesystemUUIDBytes
)

// linuxOpenHandleIdentity is meaningful only while a corresponding handle is
// live. The open handle pins the inode, which is why current-object comparisons
// neither need nor consume a restart incarnation heuristic.
type linuxOpenHandleIdentity struct {
	mountID     uint64
	deviceMajor uint32
	deviceMinor uint32
	inode       uint64
	kind        uint16
}

func (identity linuxOpenHandleIdentity) sameObject(other linuxOpenHandleIdentity) bool {
	return identity == other
}

func (identity linuxOpenHandleIdentity) sameInodeObject(other linuxOpenHandleIdentity) bool {
	return identity.deviceMajor == other.deviceMajor &&
		identity.deviceMinor == other.deviceMinor &&
		identity.inode == other.inode &&
		identity.kind == other.kind
}

type linuxOpenHandleFacts struct {
	identity linuxOpenHandleIdentity
	mode     uint16
	size     uint64
	ownerUID uint32
}

func (facts linuxOpenHandleFacts) matches(identity linuxOpenHandleIdentity) bool {
	return facts.identity.sameObject(identity)
}

// linuxNamedEntrySnapshot is deliberately not an authority. It exists only
// around a before/open/after sequence in which the opened handle pins the inode.
type linuxNamedEntrySnapshot struct {
	identity linuxOpenHandleIdentity
}

func (snapshot linuxNamedEntrySnapshot) matches(opened linuxOpenHandleIdentity) bool {
	return snapshot.identity.sameObject(opened)
}

type linuxDirectoryRestartIdentity struct {
	mount              linuxMountIdentity
	inode              uint64
	kind               uint16
	birthSeconds       int64
	birthNanoseconds   uint32
	generation         uint32
	hasGenerationProof bool
}

func (identity linuxDirectoryRestartIdentity) sameDirectory(other linuxDirectoryRestartIdentity) bool {
	return identity == other
}

func (identity linuxDirectoryRestartIdentity) matchesHandle(handle linuxOpenHandleIdentity) bool {
	return identity.mount.uniqueMountID == handle.mountID &&
		identity.mount.deviceMajor == handle.deviceMajor &&
		identity.mount.deviceMinor == handle.deviceMinor &&
		identity.inode == handle.inode && identity.kind == handle.kind
}

// linuxDirectoryRestartIdentityProvider separates enrollment-capable identity
// preparation from the strictly read-only observation used on recovery. The
// statx-btime candidate is read-only in both cases; a future xattr candidate may
// implement preparation without weakening recovery reads.
type linuxDirectoryRestartIdentityProvider interface {
	Prepare(
		system *linuxOutputSystem,
		fd int,
		mount linuxMountIdentity,
	) (linuxDirectoryRestartIdentity, error)
	Read(
		system *linuxOutputSystem,
		fd int,
		mount linuxMountIdentity,
	) (linuxDirectoryRestartIdentity, error)
}

type linuxStatxBirthTimeRestartIdentityProvider struct{}

func (linuxStatxBirthTimeRestartIdentityProvider) Prepare(
	system *linuxOutputSystem,
	fd int,
	mount linuxMountIdentity,
) (linuxDirectoryRestartIdentity, error) {
	return linuxReadStatxBirthTimeRestartIdentity(system, fd, mount)
}

func (linuxStatxBirthTimeRestartIdentityProvider) Read(
	system *linuxOutputSystem,
	fd int,
	mount linuxMountIdentity,
) (linuxDirectoryRestartIdentity, error) {
	return linuxReadStatxBirthTimeRestartIdentity(system, fd, mount)
}

func linuxReadStatxBirthTimeRestartIdentity(
	system *linuxOutputSystem,
	fd int,
	mount linuxMountIdentity,
) (linuxDirectoryRestartIdentity, error) {
	const operation = "inspect restart-stable ext4 directory identity"
	before, err := linuxReadOpenHandleFacts(system, fd, unix.STATX_MNT_ID_UNIQUE)
	if err != nil {
		return linuxDirectoryRestartIdentity{}, err
	}
	if before.identity.kind != unix.S_IFDIR {
		return linuxDirectoryRestartIdentity{}, linuxUnsafe(operation, "open object is not a directory", nil)
	}
	if before.identity.mountID != mount.uniqueMountID ||
		before.identity.deviceMajor != mount.deviceMajor ||
		before.identity.deviceMinor != mount.deviceMinor {
		return linuxDirectoryRestartIdentity{}, linuxUnsafe(operation,
			"directory escaped the certified mount", nil)
	}

	requested := unix.STATX_TYPE | unix.STATX_INO | unix.STATX_BTIME | unix.STATX_MNT_ID_UNIQUE
	var stat unix.Statx_t
	if err := system.statx(
		fd, "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW, requested, &stat,
	); err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
			return linuxDirectoryRestartIdentity{}, linuxUnsupported(operation,
				"handle-bound birth-time identity is unavailable", err)
		}
		return linuxDirectoryRestartIdentity{}, fmt.Errorf("%s: %w", operation, err)
	}
	if stat.Mask&uint32(requested) != uint32(requested) {
		return linuxDirectoryRestartIdentity{}, linuxUnsupported(operation,
			"filesystem omitted birth time or non-reused mount identity", nil)
	}
	observed := linuxOpenHandleIdentity{
		mountID: stat.Mnt_id, deviceMajor: stat.Dev_major, deviceMinor: stat.Dev_minor,
		inode: stat.Ino, kind: linuxFileType(stat.Mode),
	}
	if !observed.sameObject(before.identity) {
		return linuxDirectoryRestartIdentity{}, linuxUnsafe(operation,
			"birth-time observation differs from the open directory", nil)
	}
	if stat.Btime.Nsec >= 1_000_000_000 {
		return linuxDirectoryRestartIdentity{}, linuxUnsafe(operation,
			"filesystem returned an invalid directory birth time", nil)
	}

	generation, hasGenerationProof, err := linuxReadOptionalInodeGeneration(system, fd, operation)
	if err != nil {
		return linuxDirectoryRestartIdentity{}, err
	}
	after, err := linuxReadOpenHandleFacts(system, fd, unix.STATX_MNT_ID_UNIQUE)
	if err != nil {
		return linuxDirectoryRestartIdentity{}, err
	}
	if !after.identity.sameObject(before.identity) {
		return linuxDirectoryRestartIdentity{}, linuxUnsafe(operation,
			"directory changed while restart identity was inspected", nil)
	}
	return linuxDirectoryRestartIdentity{
		mount:              mount,
		inode:              before.identity.inode,
		kind:               before.identity.kind,
		birthSeconds:       stat.Btime.Sec,
		birthNanoseconds:   stat.Btime.Nsec,
		generation:         generation,
		hasGenerationProof: hasGenerationProof,
	}, nil
}

func linuxReadOptionalInodeGeneration(
	system *linuxOutputSystem,
	fd int,
	operation string,
) (uint32, bool, error) {
	if system.getVersion == nil {
		return 0, false, nil
	}
	generation, err := system.getVersion(fd)
	if errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, linuxUnsupported(operation,
			"ext4 inode generation could not be inspected as additive evidence", err)
	}
	if generation == 0 {
		return 0, false, nil
	}
	return generation, true, nil
}

func linuxEncodeMountIdentity(mount linuxMountIdentity) ([]byte, error) {
	const operation = "encode ext4 mount identity"
	if mount.uniqueMountID == 0 || mount.filesystemUUID == [linuxFilesystemUUIDBytes]byte{} {
		return nil, linuxUnsupported(operation, "mount identity is incomplete", nil)
	}
	encoded := make([]byte, 0,
		linuxClaimUint16Bytes+len(linuxMountIdentityClaimDomain)+
			linuxClaimUint64Bytes+4*linuxClaimUint32Bytes+linuxFilesystemUUIDBytes)
	var err error
	encoded, err = linuxAppendLengthPrefixed16(encoded, []byte(linuxMountIdentityClaimDomain))
	if err != nil {
		return nil, linuxUnsupported(operation, "mount identity domain is too large", err)
	}
	encoded = linuxAppendUint64(encoded, mount.uniqueMountID)
	encoded = linuxAppendUint32(encoded, mount.deviceMajor)
	encoded = linuxAppendUint32(encoded, mount.deviceMinor)
	encoded = linuxAppendUint32(encoded, uint32(mount.runtimeFilesystemID[0]))
	encoded = linuxAppendUint32(encoded, uint32(mount.runtimeFilesystemID[1]))
	return append(encoded, mount.filesystemUUID[:]...), nil
}

func linuxEncodeDirectoryRestartIdentity(identity linuxDirectoryRestartIdentity) ([]byte, error) {
	const operation = "encode ext4 directory restart identity"
	if identity.kind != unix.S_IFDIR || identity.inode == 0 ||
		identity.birthNanoseconds >= 1_000_000_000 ||
		(identity.hasGenerationProof && identity.generation == 0) ||
		(!identity.hasGenerationProof && identity.generation != 0) {
		return nil, linuxUnsupported(operation, "directory identity is incomplete or malformed", nil)
	}
	mount, err := linuxEncodeMountIdentity(identity.mount)
	if err != nil {
		return nil, err
	}
	encoded := make([]byte, 0,
		linuxClaimUint16Bytes+len(linuxDirectoryRestartIdentityClaimDomain)+
			linuxClaimUint32Bytes+len(mount)+2*linuxClaimUint64Bytes+
			2*linuxClaimUint32Bytes+1+linuxClaimUint16Bytes)
	encoded, err = linuxAppendLengthPrefixed16(encoded, []byte(linuxDirectoryRestartIdentityClaimDomain))
	if err != nil {
		return nil, linuxUnsupported(operation, "directory identity domain is too large", err)
	}
	encoded = linuxAppendUint32(encoded, uint32(len(mount)))
	encoded = append(encoded, mount...)
	encoded = linuxAppendUint64(encoded, identity.inode)
	encoded = linuxAppendUint64(encoded, uint64(identity.birthSeconds))
	encoded = linuxAppendUint32(encoded, identity.birthNanoseconds)
	if identity.hasGenerationProof {
		encoded = append(encoded, 1)
	} else {
		encoded = append(encoded, 0)
	}
	encoded = linuxAppendUint32(encoded, identity.generation)
	encoded = linuxAppendUint16(encoded, identity.kind)
	return encoded, nil
}
