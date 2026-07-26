//go:build linux

package osfs

import (
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

const (
	linuxDefaultAccessACL        = "system.posix_acl_default"
	linuxAccessACL               = "system.posix_acl_access"
	linuxPOSIXACLVersion         = uint32(2)
	linuxPOSIXACLHeaderBytes     = 4
	linuxPOSIXACLEntryBytes      = 8
	linuxMaximumAccessACLBytes   = 64 << 10
	linuxPOSIXACLUserObject      = uint16(0x01)
	linuxPOSIXACLNamedUser       = uint16(0x02)
	linuxPOSIXACLGroupObject     = uint16(0x04)
	linuxPOSIXACLNamedGroup      = uint16(0x08)
	linuxPOSIXACLMask            = uint16(0x10)
	linuxPOSIXACLOther           = uint16(0x20)
	linuxPOSIXACLPermissionWrite = uint16(0x02)
	linuxPOSIXACLPermissionExec  = uint16(0x01)
	linuxPOSIXACLPermissions     = uint16(0x07)
	linuxPOSIXACLUndefinedID     = ^uint32(0)
)

func (directory *linuxOutputDirectory) validateCreateAuthority() error {
	const operation = "validate output directory create authority"
	if err := directory.verifyHandle(); err != nil {
		return err
	}
	if directory.system.faccessat2 == nil || directory.system.fgetxattr == nil {
		return linuxUnsupported(operation, "effective access or default-ACL provider is unavailable", nil)
	}
	if err := directory.system.faccessat2(
		directory.fd, "", uint32(unix.W_OK|unix.X_OK), unix.AT_EMPTY_PATH|unix.AT_EACCESS,
	); err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
			return linuxUnsupported(operation, "handle-bound effective access checks are unavailable", err)
		}
		return linuxUnsafe(operation, "directory lacks effective create/search authority", err)
	}
	identity, err := linuxVerifyOpenObject(directory.system, directory.fd, directory.certificate)
	if err != nil {
		return err
	}
	if identity.mode&unix.S_ISGID != 0 {
		return linuxUnsupported(operation,
			"setgid inheritance can change a private directory mode at the create cut", nil)
	}
	if _, err := directory.system.fgetxattr(directory.fd, linuxDefaultAccessACL, nil); err == nil {
		return linuxUnsupported(operation,
			"a default POSIX ACL can change a private entry mode at the create cut", nil)
	} else if !errors.Is(err, unix.ENODATA) {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) {
			return linuxUnsupported(operation, "default POSIX ACL state cannot be inspected", err)
		}
		return linuxUnsafe(operation, "default POSIX ACL state cannot be trusted", err)
	}
	if err := directory.validateExclusiveChildMutationAuthority(); err != nil {
		return err
	}
	return directory.verifyHandle()
}

func (directory *linuxOutputDirectory) validateExclusiveChildMutationAuthority() error {
	const operation = "validate exclusive output ancestry authority"
	if err := directory.verifyHandle(); err != nil {
		return err
	}
	if directory.system.fgetxattr == nil || directory.system.geteuid == nil {
		return linuxUnsupported(operation, "owner or access-ACL provider is unavailable", nil)
	}
	identity, err := linuxVerifyOpenObject(directory.system, directory.fd, directory.certificate)
	if err != nil {
		return err
	}
	if identity.identity.kind != unix.S_IFDIR || !identity.matches(directory.object) {
		return linuxUnsafe(operation, "ancestry handle no longer identifies its fixed directory", nil)
	}
	receiverUID := uint32(directory.system.geteuid())
	if identity.ownerUID != receiverUID {
		return linuxUnsafe(operation, "ancestry directory is not owned by the receiver identity", nil)
	}
	reason, err := linuxExternalChildMutationAuthority(
		directory.system, directory.fd, identity.mode, receiverUID, operation,
	)
	if err != nil {
		return err
	}
	if reason != "" {
		// Sticky directories are intentionally not a special case: an outside
		// writer can still allocate conflicting children, and WindShare cannot
		// make a general nested-output claim on that shared namespace.
		return linuxUnsafe(operation, reason, nil)
	}
	return directory.verifyHandle()
}

func linuxExternalChildMutationAuthority(
	system *linuxOutputSystem,
	fd int,
	mode uint16,
	receiverUID uint32,
	operation string,
) (string, error) {
	if system == nil || system.fgetxattr == nil {
		return "", linuxUnsupported(operation, "access POSIX ACL provider is unavailable", nil)
	}
	encoded, present, err := linuxReadAccessACL(system, fd, operation)
	if err != nil {
		return "", err
	}
	if !present {
		groupPermissions := uint16(linuxPermissions(mode)>>3) & linuxPOSIXACLPermissions
		otherPermissions := uint16(linuxPermissions(mode)) & linuxPOSIXACLPermissions
		if linuxPOSIXACLGrantsChildMutation(groupPermissions) ||
			linuxPOSIXACLGrantsChildMutation(otherPermissions) {
			return "group or other mode grants another principal rename/delete-child authority", nil
		}
		return "", nil
	}
	acl, err := linuxParseAccessACL(encoded)
	if err != nil {
		return "", linuxUnsafe(operation, "access POSIX ACL is malformed", err)
	}
	if err := acl.validateMode(mode); err != nil {
		return "", linuxUnsafe(operation, "access POSIX ACL disagrees with directory mode", err)
	}
	return acl.externalChildMutationAuthority(receiverUID), nil
}

func linuxReadAccessACL(
	system *linuxOutputSystem,
	fd int,
	operation string,
) ([]byte, bool, error) {
	size, err := system.fgetxattr(fd, linuxAccessACL, nil)
	if errors.Is(err, unix.ENODATA) {
		return nil, false, nil
	}
	if err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) {
			return nil, false, linuxUnsupported(operation, "access POSIX ACL state cannot be inspected", err)
		}
		return nil, false, linuxUnsafe(operation, "access POSIX ACL state cannot be trusted", err)
	}
	if size < linuxPOSIXACLHeaderBytes+3*linuxPOSIXACLEntryBytes ||
		size > linuxMaximumAccessACLBytes {
		return nil, false, linuxUnsafe(operation, "access POSIX ACL size is outside its certified bound", nil)
	}
	encoded := make([]byte, size)
	read, err := system.fgetxattr(fd, linuxAccessACL, encoded)
	if err != nil {
		return nil, false, linuxUnsafe(operation, "access POSIX ACL changed or became unreadable", err)
	}
	if read != size {
		return nil, false, linuxUnsafe(operation, "access POSIX ACL changed while it was inspected", nil)
	}
	return encoded, true, nil
}

type linuxPOSIXACLPermission struct {
	value   uint16
	present bool
}

type linuxPOSIXACL struct {
	owner       linuxPOSIXACLPermission
	group       linuxPOSIXACLPermission
	mask        linuxPOSIXACLPermission
	other       linuxPOSIXACLPermission
	namedUsers  map[uint32]uint16
	namedGroups map[uint32]uint16
}

func linuxParseAccessACL(encoded []byte) (linuxPOSIXACL, error) {
	if len(encoded) < linuxPOSIXACLHeaderBytes+3*linuxPOSIXACLEntryBytes ||
		len(encoded) > linuxMaximumAccessACLBytes ||
		(len(encoded)-linuxPOSIXACLHeaderBytes)%linuxPOSIXACLEntryBytes != 0 {
		return linuxPOSIXACL{}, errors.New("invalid ACL length")
	}
	if binary.LittleEndian.Uint32(encoded) != linuxPOSIXACLVersion {
		return linuxPOSIXACL{}, errors.New("invalid ACL version")
	}
	acl := linuxPOSIXACL{
		namedUsers:  make(map[uint32]uint16),
		namedGroups: make(map[uint32]uint16),
	}
	for offset := linuxPOSIXACLHeaderBytes; offset < len(encoded); offset += linuxPOSIXACLEntryBytes {
		tag := binary.LittleEndian.Uint16(encoded[offset:])
		permissions := binary.LittleEndian.Uint16(encoded[offset+2:])
		id := binary.LittleEndian.Uint32(encoded[offset+4:])
		if permissions&^linuxPOSIXACLPermissions != 0 {
			return linuxPOSIXACL{}, errors.New("ACL entry has invalid permissions")
		}
		switch tag {
		case linuxPOSIXACLUserObject:
			if id != linuxPOSIXACLUndefinedID || acl.owner.present {
				return linuxPOSIXACL{}, errors.New("invalid owner ACL entry")
			}
			acl.owner = linuxPOSIXACLPermission{value: permissions, present: true}
		case linuxPOSIXACLNamedUser:
			if id == linuxPOSIXACLUndefinedID {
				return linuxPOSIXACL{}, errors.New("named user ACL entry has no identity")
			}
			if _, duplicate := acl.namedUsers[id]; duplicate {
				return linuxPOSIXACL{}, errors.New("duplicate named user ACL entry")
			}
			acl.namedUsers[id] = permissions
		case linuxPOSIXACLGroupObject:
			if id != linuxPOSIXACLUndefinedID || acl.group.present {
				return linuxPOSIXACL{}, errors.New("invalid owning-group ACL entry")
			}
			acl.group = linuxPOSIXACLPermission{value: permissions, present: true}
		case linuxPOSIXACLNamedGroup:
			if id == linuxPOSIXACLUndefinedID {
				return linuxPOSIXACL{}, errors.New("named group ACL entry has no identity")
			}
			if _, duplicate := acl.namedGroups[id]; duplicate {
				return linuxPOSIXACL{}, errors.New("duplicate named group ACL entry")
			}
			acl.namedGroups[id] = permissions
		case linuxPOSIXACLMask:
			if id != linuxPOSIXACLUndefinedID || acl.mask.present {
				return linuxPOSIXACL{}, errors.New("invalid ACL mask entry")
			}
			acl.mask = linuxPOSIXACLPermission{value: permissions, present: true}
		case linuxPOSIXACLOther:
			if id != linuxPOSIXACLUndefinedID || acl.other.present {
				return linuxPOSIXACL{}, errors.New("invalid other ACL entry")
			}
			acl.other = linuxPOSIXACLPermission{value: permissions, present: true}
		default:
			return linuxPOSIXACL{}, errors.New("unknown ACL entry tag")
		}
	}
	if !acl.owner.present || !acl.group.present || !acl.other.present {
		return linuxPOSIXACL{}, errors.New("ACL omits a required object entry")
	}
	if (len(acl.namedUsers) != 0 || len(acl.namedGroups) != 0) && !acl.mask.present {
		return linuxPOSIXACL{}, errors.New("extended ACL omits its mask")
	}
	return acl, nil
}

func (acl linuxPOSIXACL) validateMode(mode uint16) error {
	owner := uint16(linuxPermissions(mode)>>6) & linuxPOSIXACLPermissions
	group := uint16(linuxPermissions(mode)>>3) & linuxPOSIXACLPermissions
	other := uint16(linuxPermissions(mode)) & linuxPOSIXACLPermissions
	if acl.owner.value != owner || acl.other.value != other {
		return errors.New("ACL owner or other permissions differ from mode")
	}
	groupMode := acl.group.value
	if acl.mask.present {
		groupMode = acl.mask.value
	}
	if groupMode != group {
		return errors.New("ACL group class differs from mode")
	}
	return nil
}

func (acl linuxPOSIXACL) externalChildMutationAuthority(receiverUID uint32) string {
	mask := linuxPOSIXACLPermissions
	if acl.mask.present {
		mask = acl.mask.value
	}
	if linuxPOSIXACLGrantsChildMutation(acl.group.value & mask) {
		return "owning-group ACL grants another principal rename/delete-child authority"
	}
	for uid, permissions := range acl.namedUsers {
		// The receiver owner entry always takes precedence over a named entry for
		// the same UID. UID 0 is outside the approved unprivileged threat model.
		if uid == receiverUID || uid == 0 {
			continue
		}
		if linuxPOSIXACLGrantsChildMutation(permissions & mask) {
			return "named-user ACL grants another principal rename/delete-child authority"
		}
	}
	for _, permissions := range acl.namedGroups {
		if linuxPOSIXACLGrantsChildMutation(permissions & mask) {
			return "named-group ACL grants another principal rename/delete-child authority"
		}
	}
	if linuxPOSIXACLGrantsChildMutation(acl.other.value) {
		return "other ACL grants another principal rename/delete-child authority"
	}
	return ""
}

func linuxPOSIXACLGrantsChildMutation(permissions uint16) bool {
	const required = linuxPOSIXACLPermissionWrite | linuxPOSIXACLPermissionExec
	return permissions&required == required
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
