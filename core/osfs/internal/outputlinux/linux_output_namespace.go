//go:build linux

package outputlinux

import (
	"errors"
	"fmt"
	"io/fs"

	"golang.org/x/sys/unix"
)

const linuxRelativeOpenResolution = unix.RESOLVE_BENEATH |
	unix.RESOLVE_NO_MAGICLINKS |
	unix.RESOLVE_NO_SYMLINKS |
	unix.RESOLVE_NO_XDEV

type linuxOutputRegularFile struct {
	system                  *linuxOutputSystem
	fd                      int
	certificate             linuxOutputCertificate
	object                  linuxOpenHandleIdentity
	exactPermissions        uint32
	requireExactPermissions bool
	access                  linuxOutputFileAccess
}

type linuxOutputFileAccess uint8

const (
	linuxOutputFileObserved linuxOutputFileAccess = iota + 1
	linuxOutputFileRecoveryDurability
	linuxOutputFileMutable
)

type linuxRenameDisposition uint8

const (
	linuxRenameReplace linuxRenameDisposition = iota + 1
	linuxRenameNoReplace
)

func (directory *linuxOutputDirectory) durability() linuxOutputDurability {
	if directory == nil {
		return 0
	}
	return directory.certificate.durability
}

func (directory *linuxOutputDirectory) openDirectory(name string) (*linuxOutputDirectory, error) {
	return directory.openDirectoryWithMode(name, 0, false)
}

func (directory *linuxOutputDirectory) openDirectoryExact(
	name string,
	permissions uint32,
) (*linuxOutputDirectory, error) {
	return directory.openDirectoryWithMode(name, permissions, true)
}

func (directory *linuxOutputDirectory) openDirectoryWithMode(
	name string,
	permissions uint32,
	requireExactMode bool,
) (*linuxOutputDirectory, error) {
	const operation = "open output directory"
	if err := directory.verifyHandle(); err != nil {
		return nil, err
	}
	if err := linuxValidateComponent(operation, name); err != nil {
		return nil, err
	}
	if requireExactMode {
		if err := linuxValidatePermissions(operation, permissions); err != nil {
			return nil, err
		}
	}
	fd, err := directory.openRelative(name, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	identity, err := linuxVerifyOpenObject(directory.system, fd, directory.certificate)
	if err != nil {
		return nil, errors.Join(err, directory.system.close(fd))
	}
	if linuxFileType(identity.mode) != unix.S_IFDIR {
		return nil, errors.Join(
			linuxUnsafe(operation, "entry is not a directory", nil),
			directory.system.close(fd),
		)
	}
	if requireExactMode && linuxPermissions(identity.mode) != permissions {
		return nil, errors.Join(
			linuxUnsafe(operation, "private directory permissions do not match the required mode", nil),
			directory.system.close(fd),
		)
	}
	if requireExactMode {
		if err := linuxValidateExactOwner(directory.system, identity, operation); err != nil {
			return nil, errors.Join(err, directory.system.close(fd))
		}
	}
	return &linuxOutputDirectory{
		system:                  directory.system,
		fd:                      fd,
		certificate:             directory.certificate,
		object:                  identity.identity,
		exactPermissions:        permissions,
		requireExactPermissions: requireExactMode,
	}, nil
}

func (directory *linuxOutputDirectory) createDirectoryExact(
	name string,
	permissions uint32,
) (*linuxOutputDirectory, error) {
	return directory.createDirectoryExactWithRollback(name, permissions, false)
}

func (directory *linuxOutputDirectory) createPrivateDirectoryExact(
	name string,
	permissions uint32,
) (*linuxOutputDirectory, error) {
	return directory.createDirectoryExactWithRollback(name, permissions, true)
}

func (directory *linuxOutputDirectory) createDirectoryExactWithRollback(
	name string,
	permissions uint32,
	private bool,
) (_ *linuxOutputDirectory, resultErr error) {
	const operation = "create output directory"
	if err := directory.verifyHandle(); err != nil {
		return nil, err
	}
	if err := linuxValidateComponent(operation, name); err != nil {
		return nil, err
	}
	if err := linuxValidatePermissions(operation, permissions); err != nil {
		return nil, err
	}
	// Public parents use their native ACL semantics. Private parents are already
	// the ownership boundary and require exact owner-only authority at the cut.
	if private {
		if err := directory.validatePrivateCreateAuthority(); err != nil {
			return nil, err
		}
	} else if err := directory.validatePublicCreateAuthority(); err != nil {
		return nil, err
	}
	if err := directory.system.mkdirat(directory.fd, name, permissions); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, &linuxOutputCollisionError{operation: operation, name: name, cause: err}
		}
		return nil, fmt.Errorf("%s %q: %w", operation, name, err)
	}
	created, err := directory.openDirectory(name)
	if err != nil {
		// Without a verified handle the name is no longer authority. Retaining the
		// exclusive-created directory lets recovery classify it without risking
		// deletion of a replacement installed after mkdirat.
		return nil, err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		var rollbackErr error
		if private {
			rollbackErr = directory.unlinkDirectory(name, created)
		}
		resultErr = errors.Join(resultErr, rollbackErr, created.close())
	}()
	if private {
		if err := created.setExactMode(permissions); err != nil {
			return nil, err
		}
	}
	if err := created.sync(); err != nil {
		return nil, err
	}
	if err := directory.sync(); err != nil {
		return nil, err
	}
	committed = true
	return created, nil
}

func (directory *linuxOutputDirectory) openRegularFile(
	name string,
	access linuxOutputFileAccess,
) (*linuxOutputRegularFile, error) {
	return directory.openRegularFileWithMode(name, access, 0, false)
}

func (directory *linuxOutputDirectory) openRegularFileExact(
	name string,
	access linuxOutputFileAccess,
	permissions uint32,
) (*linuxOutputRegularFile, error) {
	return directory.openRegularFileWithMode(name, access, permissions, true)
}

func (directory *linuxOutputDirectory) openRegularFileWithMode(
	name string,
	access linuxOutputFileAccess,
	permissions uint32,
	requireExactMode bool,
) (*linuxOutputRegularFile, error) {
	const operation = "open output regular file"
	if err := directory.verifyHandle(); err != nil {
		return nil, err
	}
	if err := linuxValidateComponent(operation, name); err != nil {
		return nil, err
	}
	if requireExactMode {
		if err := linuxValidatePermissions(operation, permissions); err != nil {
			return nil, err
		}
	}
	openMode := unix.O_RDONLY
	switch access {
	case linuxOutputFileObserved:
	case linuxOutputFileRecoveryDurability:
		openMode = unix.O_WRONLY
	case linuxOutputFileMutable:
		openMode = unix.O_RDWR
	default:
		return nil, linuxUnsafe(operation, "file access purpose is invalid", nil)
	}
	fd, err := directory.openRelative(name, openMode, 0)
	if err != nil {
		return nil, err
	}
	identity, err := linuxVerifyOpenObject(directory.system, fd, directory.certificate)
	if err != nil {
		return nil, errors.Join(err, directory.system.close(fd))
	}
	if linuxFileType(identity.mode) != unix.S_IFREG {
		return nil, errors.Join(
			linuxUnsafe(operation, "entry is not a regular file", nil),
			directory.system.close(fd),
		)
	}
	if requireExactMode && linuxPermissions(identity.mode) != permissions {
		return nil, errors.Join(
			linuxUnsafe(operation, "file permissions do not match the required mode", nil),
			directory.system.close(fd),
		)
	}
	if requireExactMode {
		if err := linuxValidateExactOwner(directory.system, identity, operation); err != nil {
			return nil, errors.Join(err, directory.system.close(fd))
		}
	}
	return &linuxOutputRegularFile{
		system:                  directory.system,
		fd:                      fd,
		certificate:             directory.certificate,
		object:                  identity.identity,
		exactPermissions:        permissions,
		requireExactPermissions: requireExactMode,
		access:                  access,
	}, nil
}

func (directory *linuxOutputDirectory) createPrivateRegularFileExact(
	name string,
	permissions uint32,
	size int64,
) (*linuxOutputRegularFile, error) {
	return directory.createRegularFileExactWithAuthority(name, permissions, size, true)
}

func (directory *linuxOutputDirectory) createRegularFileExactWithAuthority(
	name string,
	permissions uint32,
	size int64,
	private bool,
) (_ *linuxOutputRegularFile, resultErr error) {
	const operation = "create output regular file"
	if err := directory.verifyHandle(); err != nil {
		return nil, err
	}
	if err := linuxValidateComponent(operation, name); err != nil {
		return nil, err
	}
	if err := linuxValidatePermissions(operation, permissions); err != nil {
		return nil, err
	}
	if size < 0 {
		return nil, linuxUnsafe(operation, "file size cannot be negative", nil)
	}
	if private {
		if err := directory.validatePrivateCreateAuthority(); err != nil {
			return nil, err
		}
	} else if err := directory.validatePublicCreateAuthority(); err != nil {
		return nil, err
	}
	fd, err := directory.openRelative(name, unix.O_CREAT|unix.O_EXCL|unix.O_RDWR, permissions)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, &linuxOutputCollisionError{operation: operation, name: name, cause: err}
		}
		return nil, err
	}
	created := &linuxOutputRegularFile{
		system:      directory.system,
		fd:          fd,
		certificate: directory.certificate,
		access:      linuxOutputFileMutable,
	}
	committed := false
	authorityFixed := false
	defer func() {
		if committed {
			return
		}
		var rollbackErr error
		if authorityFixed {
			// The created handle remains live while the parent reopens the name and
			// compares identities, so inode reuse or a concurrent replacement can
			// only make rollback fail closed.
			rollbackErr = directory.unlinkRegularFile(name, created)
		}
		resultErr = errors.Join(resultErr, rollbackErr, created.close())
	}()
	identity, err := linuxVerifyOpenObject(directory.system, fd, directory.certificate)
	if err != nil {
		return nil, err
	}
	if linuxFileType(identity.mode) != unix.S_IFREG {
		return nil, linuxUnsafe(operation, "exclusive create did not produce a regular file", nil)
	}
	created.object = identity.identity
	authorityFixed = true
	if private {
		if err := created.setExactMode(permissions); err != nil {
			return nil, err
		}
	}
	if err := created.truncate(size); err != nil {
		return nil, err
	}
	if err := created.sync(); err != nil {
		return nil, err
	}
	if err := directory.sync(); err != nil {
		return nil, err
	}
	committed = true
	return created, nil
}
