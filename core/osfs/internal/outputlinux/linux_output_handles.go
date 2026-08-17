//go:build linux

package outputlinux

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

func (directory *linuxOutputDirectory) regularEntryMatches(
	name string,
	expected *linuxOutputRegularFile,
) (bool, error) {
	if expected == nil {
		return false, linuxUnsafe("compare output entry", "expected file handle is absent", nil)
	}
	if err := expected.verifyHandle(); err != nil {
		return false, err
	}
	opened, err := directory.openRegularFile(name, linuxOutputFileObserved)
	if err != nil {
		return false, err
	}
	same, compareErr := linuxSameOpenRegularFile(opened, expected)
	return same, errors.Join(compareErr, opened.close())
}

func linuxSameOpenRegularFile(left, right *linuxOutputRegularFile) (bool, error) {
	const operation = "compare open output files"
	if left == nil || right == nil {
		return false, linuxUnsafe(operation, "file handle is absent", nil)
	}
	if left.certificate.mount != right.certificate.mount {
		return false, linuxUnsafe(operation, "file handles belong to different certified mounts", nil)
	}
	leftIdentity, err := left.currentIdentity()
	if err != nil {
		return false, err
	}
	rightIdentity, err := right.currentIdentity()
	if err != nil {
		return false, err
	}
	return leftIdentity.identity.sameObject(rightIdentity.identity), nil
}

func (directory *linuxOutputDirectory) setExactMode(permissions uint32) error {
	const operation = "set output directory mode"
	if err := linuxValidatePermissions(operation, permissions); err != nil {
		return err
	}
	if err := directory.verifyHandle(); err != nil {
		return err
	}
	if err := directory.system.fchmod(directory.fd, permissions); err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	identity, err := linuxVerifyOpenObject(directory.system, directory.fd, directory.certificate)
	if err != nil {
		return err
	}
	if linuxPermissions(identity.mode) != permissions {
		return linuxUnsafe(operation, "filesystem did not install the exact directory mode", nil)
	}
	if err := linuxValidateExactOwner(directory.system, identity, operation); err != nil {
		return err
	}
	directory.exactPermissions = permissions
	directory.requireExactPermissions = true
	return nil
}

func (file *linuxOutputRegularFile) setExactMode(permissions uint32) error {
	const operation = "set output file mode"
	if err := linuxValidatePermissions(operation, permissions); err != nil {
		return err
	}
	if err := file.requireMutable(operation); err != nil {
		return err
	}
	if err := file.system.fchmod(file.fd, permissions); err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	identity, err := file.currentIdentity()
	if err != nil {
		return err
	}
	if linuxPermissions(identity.mode) != permissions {
		return linuxUnsafe(operation, "filesystem did not install the exact file mode", nil)
	}
	if err := linuxValidateExactOwner(file.system, identity, operation); err != nil {
		return err
	}
	file.exactPermissions = permissions
	file.requireExactPermissions = true
	return nil
}

func (file *linuxOutputRegularFile) truncate(size int64) error {
	const operation = "set output file size"
	if size < 0 {
		return linuxUnsafe(operation, "file size cannot be negative", nil)
	}
	if err := file.requireMutable(operation); err != nil {
		return err
	}
	if err := file.system.ftruncate(file.fd, size); err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	identity, err := file.currentIdentity()
	if err != nil {
		return err
	}
	if identity.size != uint64(size) {
		return linuxUnsafe(operation, "filesystem did not install the exact file size", nil)
	}
	return nil
}

func (directory *linuxOutputDirectory) sync() error {
	if err := directory.verifyHandle(); err != nil {
		return err
	}
	return linuxSyncOutputHandle(directory.system, directory.fd, "sync output directory")
}

func (file *linuxOutputRegularFile) sync() error {
	const operation = "sync output file"
	if err := file.requireSyncAuthority(operation); err != nil {
		return err
	}
	return linuxSyncOutputHandle(file.system, file.fd, operation)
}

func linuxSyncOutputHandle(system *linuxOutputSystem, fd int, operation string) error {
	if err := system.fsync(fd); err != nil {
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) {
			return linuxUnsupported(operation, "required file or directory sync is unavailable", err)
		}
		return fmt.Errorf("%s: %w", operation, err)
	}
	return nil
}

func (directory *linuxOutputDirectory) verifyHandle() error {
	const operation = "verify output directory handle"
	if directory == nil || directory.system == nil || directory.fd < 0 {
		return linuxUnsafe(operation, "directory handle is closed or absent", nil)
	}
	identity, err := linuxVerifyOpenObject(directory.system, directory.fd, directory.certificate)
	if err != nil {
		return err
	}
	if identity.identity.kind != unix.S_IFDIR || !identity.matches(directory.object) {
		return linuxUnsafe(operation, "open directory object no longer matches its authority", nil)
	}
	if directory.requireExactPermissions && linuxPermissions(identity.mode) != directory.exactPermissions {
		return linuxUnsafe(operation, "open directory permissions changed after authority was fixed", nil)
	}
	if directory.requireExactPermissions {
		if err := linuxValidateExactOwner(directory.system, identity, operation); err != nil {
			return err
		}
	}
	return nil
}

func (file *linuxOutputRegularFile) verifyHandle() error {
	const operation = "verify output file handle"
	if file == nil || file.system == nil || file.fd < 0 {
		return linuxUnsafe(operation, "file handle is closed or absent", nil)
	}
	identity, err := file.currentIdentity()
	if err != nil {
		return err
	}
	if !identity.matches(file.object) {
		return linuxUnsafe(operation, "open file object no longer matches its authority", nil)
	}
	if file.requireExactPermissions && linuxPermissions(identity.mode) != file.exactPermissions {
		return linuxUnsafe(operation, "open file permissions changed after authority was fixed", nil)
	}
	if file.requireExactPermissions {
		if err := linuxValidateExactOwner(file.system, identity, operation); err != nil {
			return err
		}
	}
	return nil
}

func linuxValidateExactOwner(
	system *linuxOutputSystem,
	identity linuxOpenHandleFacts,
	operation string,
) error {
	if system == nil || system.geteuid == nil {
		return linuxUnsupported(operation, "effective owner provider is unavailable", nil)
	}
	if identity.ownerUID != uint32(system.geteuid()) {
		return linuxUnsafe(operation,
			"exact-mode private object is not owned by the effective receiver user", nil)
	}
	return nil
}

func (file *linuxOutputRegularFile) currentIdentity() (linuxOpenHandleFacts, error) {
	const operation = "inspect output regular file"
	if file == nil || file.system == nil || file.fd < 0 {
		return linuxOpenHandleFacts{}, linuxUnsafe(operation, "file handle is closed or absent", nil)
	}
	identity, err := linuxVerifyOpenObject(file.system, file.fd, file.certificate)
	if err != nil {
		return linuxOpenHandleFacts{}, err
	}
	if identity.identity.kind != unix.S_IFREG {
		return linuxOpenHandleFacts{}, linuxUnsafe(operation, "open object is not a regular file", nil)
	}
	return identity, nil
}

func (file *linuxOutputRegularFile) requireMutable(operation string) error {
	if err := file.verifyHandle(); err != nil {
		return err
	}
	if file.access != linuxOutputFileMutable {
		return linuxUnsafe(operation, "file handle was not opened for mutation", nil)
	}
	return nil
}

func (file *linuxOutputRegularFile) requireSyncAuthority(operation string) error {
	if err := file.verifyHandle(); err != nil {
		return err
	}
	if file.access != linuxOutputFileRecoveryDurability && file.access != linuxOutputFileMutable {
		return linuxUnsafe(operation, "file handle was not opened for durability", nil)
	}
	return nil
}

func (directory *linuxOutputDirectory) openRelative(name string, flags int, mode uint32) (int, error) {
	how := unix.OpenHow{
		Flags:   uint64(flags | unix.O_CLOEXEC | unix.O_NOFOLLOW),
		Mode:    uint64(mode),
		Resolve: uint64(linuxRelativeOpenResolution),
	}
	fd, err := directory.system.openat2(directory.fd, name, &how)
	if err != nil {
		return -1, linuxClassifyOpenError("open output entry", err)
	}
	return fd, nil
}

func (directory *linuxOutputDirectory) close() error {
	if directory == nil || directory.fd < 0 {
		return nil
	}
	err := directory.system.close(directory.fd)
	directory.fd = -1
	return err
}

func (file *linuxOutputRegularFile) close() error {
	if file == nil || file.fd < 0 {
		return nil
	}
	err := file.system.close(file.fd)
	file.fd = -1
	return err
}

func linuxVerifyDirectoryPair(left, right *linuxOutputDirectory) error {
	const operation = "verify output directory pair"
	if err := left.verifyHandle(); err != nil {
		return err
	}
	if err := right.verifyHandle(); err != nil {
		return err
	}
	if left.certificate.mount != right.certificate.mount {
		return linuxUnsafe(operation, "directory handles belong to different certified mounts", nil)
	}
	return nil
}

func linuxRenameFlags(operation string, disposition linuxRenameDisposition) (uint, error) {
	switch disposition {
	case linuxRenameReplace:
		return 0, nil
	case linuxRenameNoReplace:
		return unix.RENAME_NOREPLACE, nil
	default:
		return 0, linuxUnsafe(operation, "rename disposition is invalid", nil)
	}
}

func linuxSyncRenamedParents(source, target *linuxOutputDirectory) error {
	if err := target.sync(); err != nil {
		return err
	}
	if source.object.sameObject(target.object) {
		return nil
	}
	return source.sync()
}

func linuxValidateComponent(operation, name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') || strings.ContainsRune(name, 0) {
		return linuxUnsafe(operation, "entry name is not one relative path component", nil)
	}
	if len(name) > linuxOutputNameMaximumBytes {
		return linuxUnsafe(operation, "entry name exceeds the ext4 component limit", nil)
	}
	return nil
}

func linuxValidatePermissions(operation string, permissions uint32) error {
	if permissions&^linuxOutputPermissionMask != 0 {
		return linuxUnsafe(operation, "mode contains file type or unsupported permission bits", nil)
	}
	return nil
}

func linuxPermissions(mode uint16) uint32 {
	return uint32(mode) & linuxOutputPermissionMask
}
