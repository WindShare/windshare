//go:build linux

package outputlinux

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// validatePublicCreateAuthority asks the kernel whether this process can create
// entries through the retained directory handle. Public containers are user
// space, not WindShare ownership boundaries, so inherited group/default/named
// ACLs and setgid are deliberately left to normal Linux permission semantics.
func (directory *linuxOutputDirectory) validatePublicCreateAuthority() error {
	const operation = "validate public output create authority"
	if err := directory.verifyHandle(); err != nil {
		return err
	}
	if directory.system.faccessat2 == nil {
		return linuxUnsupported(operation, "handle-bound effective access provider is unavailable", nil)
	}
	if err := directory.system.faccessat2(
		directory.fd, "", uint32(unix.W_OK|unix.X_OK), unix.AT_EMPTY_PATH|unix.AT_EACCESS,
	); err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
			return linuxUnsupported(operation, "handle-bound effective access checks are unavailable", err)
		}
		return linuxUnsafe(operation, "directory lacks effective create/search authority", err)
	}
	return directory.verifyHandle()
}

// validatePrivateCreateAuthority protects the cut before creation of owned
// control state. Ordinary public entries retain kernel umask/default-ACL
// inheritance, while the parent of owned state must already be an authenticated
// private namespace so no other principal can race or replace an owned entry.
func (directory *linuxOutputDirectory) validatePrivateCreateAuthority() error {
	const operation = "validate private output create authority"
	if err := directory.validatePublicCreateAuthority(); err != nil {
		return err
	}
	// The first owner-only control directory is installed beneath the public
	// container. Once inside that boundary, every deeper private mutation also
	// re-proves the parent's exact owner-only authority.
	if !directory.requireExactPermissions {
		return nil
	}
	return directory.validatePrivateAuthority(operation)
}

func (directory *linuxOutputDirectory) validatePrivateAuthority(operation string) error {
	if err := directory.verifyHandle(); err != nil {
		return err
	}
	identity, err := linuxVerifyOpenObject(directory.system, directory.fd, directory.certificate)
	if err != nil {
		return err
	}
	if identity.identity.kind != unix.S_IFDIR || !identity.matches(directory.object) {
		return linuxUnsafe(operation, "private directory handle no longer identifies its fixed object", nil)
	}
	if directory.system.geteuid == nil {
		return linuxUnsupported(operation, "effective identity provider is unavailable", nil)
	}
	if identity.ownerUID != uint32(directory.system.geteuid()) {
		return linuxUnsafe(operation, "private directory is not owned by the receiver identity", nil)
	}
	if linuxPermissions(identity.mode) != linuxOutputDirectoryMode {
		return linuxUnsafe(operation, "private directory permissions are not exactly owner-only", nil)
	}
	return directory.verifyHandle()
}

// validateCreateAuthority remains the existing consumer-side hook, but its
// semantics are now public actual access. Private callers invoke the stricter
// validator explicitly at their ownership boundary.
func (directory *linuxOutputDirectory) validateCreateAuthority() error {
	return directory.validatePublicCreateAuthority()
}

func (directory *linuxOutputDirectory) validateMetadataAuthority() error {
	const operation = "validate output directory metadata authority"
	if err := directory.verifyHandle(); err != nil {
		return err
	}
	if directory.system.geteuid == nil {
		return linuxUnsupported(operation, "effective identity provider is unavailable", nil)
	}
	owner, err := directory.ownerUID()
	if err != nil {
		return err
	}
	if owner != uint32(directory.system.geteuid()) {
		return linuxUnsafe(operation,
			"selected directory is not owned by the effective user", nil)
	}
	return directory.verifyHandle()
}

func (directory *linuxOutputDirectory) ownerUID() (uint32, error) {
	const operation = "inspect output directory owner"
	requested := unix.STATX_TYPE | unix.STATX_INO | unix.STATX_UID | unix.STATX_MNT_ID_UNIQUE
	var stat unix.Statx_t
	if err := directory.system.statx(
		directory.fd, "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW, requested, &stat,
	); err != nil {
		return 0, fmt.Errorf("%s: %w", operation, err)
	}
	if stat.Mask&uint32(requested) != uint32(requested) {
		return 0, linuxUnsupported(operation, "filesystem omitted required owner or identity fields", nil)
	}
	if stat.Mnt_id != directory.certificate.mount.uniqueMountID ||
		stat.Dev_major != directory.object.deviceMajor || stat.Dev_minor != directory.object.deviceMinor ||
		stat.Ino != directory.object.inode || linuxFileType(stat.Mode) != unix.S_IFDIR {
		return 0, linuxUnsafe(operation, "owner metadata is outside the fixed directory authority", nil)
	}
	return stat.Uid, nil
}
