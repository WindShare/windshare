//go:build linux

package outputlinux

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type linuxOutputFilesystem struct {
	magic int64
	name  string
}

func linuxOpenOutputRoot(path string, system *linuxOutputSystem) (*linuxOutputDirectory, error) {
	const operation = "open output root"
	if system == nil || system.openat2 == nil || system.close == nil {
		return nil, linuxUnsupported(operation, "native syscall provider is absent", nil)
	}
	if !filepath.IsAbs(path) {
		return nil, linuxUnsafe(operation, "output root must be absolute", nil)
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
	binding, err := linuxBindOutputFD(system, fd)
	if err != nil {
		return nil, errors.Join(err, system.close(fd))
	}
	certified, certificateErr := linuxEnrollExt4Restart(system, fd, binding)
	if certificateErr == nil {
		_, certificateErr = linuxCertifyAbsoluteOutputPlacement(cleanPath, system, certified)
	}
	if certificateErr == nil {
		binding = certified
	} else if !errors.Is(certificateErr, errLinuxOutputUnsupported) {
		return nil, errors.Join(certificateErr, system.close(fd))
	}
	root := &linuxOutputDirectory{
		system: system, fd: fd, binding: binding,
		object: binding.rootObject, absolutePath: cleanPath,
	}
	if err := root.validatePublicCreateAuthority(); err != nil {
		return nil, errors.Join(err, root.close())
	}
	return root, nil
}

// linuxBindOutputFD establishes only current-process authority. Durable UUID and
// incarnation evidence cannot strengthen an open handle's inode pin, and their
// absence must not prevent ordinary output through that handle.
func linuxBindOutputFD(system *linuxOutputSystem, fd int) (linuxOutputBinding, error) {
	const operation = "bind output filesystem"
	legacy, err := linuxReadOpenHandleFacts(system, fd, unix.STATX_MNT_ID)
	if err != nil {
		return linuxOutputBinding{}, err
	}
	if legacy.identity.kind != unix.S_IFDIR {
		return linuxOutputBinding{}, linuxUnsafe(operation, "output root handle is not a directory", nil)
	}
	unique, err := linuxReadOpenHandleFacts(system, fd, unix.STATX_MNT_ID_UNIQUE)
	if err != nil {
		return linuxOutputBinding{}, err
	}
	if !legacy.identity.sameInodeObject(unique.identity) {
		return linuxOutputBinding{}, linuxUnsafe(operation, "mount or root object changed during admission", nil)
	}
	if system.fstatfs == nil || system.readMountInfo == nil {
		return linuxOutputBinding{}, linuxUnsupported(operation, "filesystem inspection providers are absent", nil)
	}
	var filesystem unix.Statfs_t
	if err := system.fstatfs(fd, &filesystem); err != nil {
		return linuxOutputBinding{}, fmt.Errorf("%s: inspect filesystem: %w", operation, err)
	}
	mountInfo, err := system.readMountInfo()
	if err != nil {
		return linuxOutputBinding{}, linuxUnsupported(operation, "mount table cannot be inspected", err)
	}
	mount, err := linuxFindMountInfo(mountInfo, legacy.identity.mountID)
	if err != nil {
		return linuxOutputBinding{}, linuxUnsafe(operation, "mount table does not identify the open root", err)
	}
	if mount.deviceMajor != legacy.identity.deviceMajor || mount.deviceMinor != legacy.identity.deviceMinor {
		return linuxOutputBinding{}, linuxUnsafe(operation, "mount table device does not match the open root", nil)
	}
	//nolint:unconvert // Statfs_t.Type is int32 on supported 32-bit Linux ABIs.
	profile := linuxOutputFilesystem{magic: int64(filesystem.Type), name: mount.filesystemType}
	if err := linuxValidateOutputFilesystemSemantics(profile); err != nil {
		return linuxOutputBinding{}, err
	}
	if err := linuxVerifyOutputDirectorySemantics(system, fd, profile, operation); err != nil {
		return linuxOutputBinding{}, err
	}
	if err := linuxValidatePrivateCreationUmask(system); err != nil {
		return linuxOutputBinding{}, err
	}
	return linuxOutputBinding{
		mount: linuxMountIdentity{
			uniqueMountID: unique.identity.mountID,
			deviceMajor:   unique.identity.deviceMajor, deviceMinor: unique.identity.deviceMinor,
			runtimeFilesystemID: filesystem.Fsid.Val,
		},
		rootObject: unique.identity,
		filesystem: profile,
	}, nil
}

func linuxValidateOutputFilesystemSemantics(filesystem linuxOutputFilesystem) error {
	const operation = "admit Linux output namespace"
	// Server-controlled inode lifetimes, userspace emulation, and copy-up do not
	// provide the retained local-inode authority used by publication and deletion.
	// Successful syscall probes cannot establish those lifetime guarantees.
	switch filesystem.name {
	case "nfs", "nfs4", "cifs", "smb3", "9p", "ceph", "afs", "coda", "virtiofs", "overlay", "ecryptfs":
		return linuxUnsupported(operation, "filesystem does not provide a fixed local inode namespace", nil)
	}
	if filesystem.name == "fuse" || strings.HasPrefix(filesystem.name, "fuse.") ||
		filesystem.name == "fuseblk" {
		return linuxUnsupported(operation, "userspace filesystem inode authority is unsupported", nil)
	}
	return nil
}

func linuxVerifyOutputDirectorySemantics(
	system *linuxOutputSystem, fd int, filesystem linuxOutputFilesystem, operation string,
) error {
	err := linuxVerifyOpenDirectoryFlags(system, fd, operation)
	// tmpfs has byte-exact names and no fscrypt, casefold, or project policies.
	// Its absent ioctl is irrelevant to live inode authority, but says nothing
	// about persistence; tmpfs never receives an ext4 restart certificate.
	if filesystem.magic == unix.TMPFS_MAGIC && filesystem.name == "tmpfs" &&
		(errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.EOPNOTSUPP)) {
		return nil
	}
	return err
}

func linuxValidatePrivateCreationUmask(system *linuxOutputSystem) error {
	const operation = "admit private output creation"
	if system.readProcessStatus == nil {
		return linuxUnsupported(operation, "process umask provider is unavailable", nil)
	}
	status, err := system.readProcessStatus()
	if err != nil {
		return linuxUnsupported(operation, "process umask cannot be inspected", err)
	}
	mask, err := linuxParseProcessUmask(status)
	if err != nil {
		return linuxUnsupported(operation, "process umask is unavailable or malformed", err)
	}
	if mask&linuxOutputDirectoryMode != 0 {
		return linuxUnsupported(operation, "process umask masks required private owner permissions", nil)
	}
	return nil
}

func linuxEnrollExt4Restart(
	system *linuxOutputSystem, fd int, binding linuxOutputBinding,
) (linuxOutputBinding, error) {
	const operation = "certify ext4 output restart recovery"
	// ext2/ext3 share the superblock magic. Both observations are necessary for
	// the exact release evidence; ordinary capability probes cannot broaden it.
	if binding.filesystem.magic != linuxExt4SuperMagic || binding.filesystem.name != "ext4" {
		return linuxOutputBinding{}, linuxUnsupported(operation, "filesystem has no ext4 restart certificate", nil)
	}
	if system.getFilesystemUUID == nil {
		return linuxOutputBinding{}, linuxUnsupported(operation, "ext4 filesystem UUID provider is unavailable", nil)
	}
	uuid, err := system.getFilesystemUUID(fd)
	if err != nil || uuid == [linuxFilesystemUUIDBytes]byte{} {
		return linuxOutputBinding{}, linuxUnsupported(operation, "ext4 filesystem UUID is unavailable", err)
	}
	binding.mount.filesystemUUID = uuid
	if system.restartIdentity == nil {
		return linuxOutputBinding{}, linuxUnsupported(operation, "directory restart-identity provider is unavailable", nil)
	}
	identity, err := system.restartIdentity.Read(system, fd, binding.mount)
	if err != nil {
		return linuxOutputBinding{}, err
	}
	if !identity.matchesHandle(binding.rootObject) {
		return linuxOutputBinding{}, linuxUnsafe(operation, "restart identity differs from the pinned root", nil)
	}
	binding.restart = &linuxOutputRestartCertificate{
		rootIdentity: identity, durability: linuxOutputProcessRestartDurability,
	}
	return binding, nil
}
