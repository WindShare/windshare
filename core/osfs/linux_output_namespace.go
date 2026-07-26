//go:build linux

package osfs

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
	writable                bool
}

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
	rollbackPrivate bool,
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
	// Revalidate immediately before the irreversible name creation. A setgid bit
	// or default ACL introduced after admission could otherwise change the mode at
	// the mkdir cut and leave a process-restart recovery name permanently strict.
	if err := directory.validateCreateAuthority(); err != nil {
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
		if rollbackPrivate {
			rollbackErr = directory.unlinkDirectory(name, created)
		}
		resultErr = errors.Join(resultErr, rollbackErr, created.close())
	}()
	if err := created.setExactMode(permissions); err != nil {
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

func (directory *linuxOutputDirectory) openRegularFile(
	name string,
	writable bool,
) (*linuxOutputRegularFile, error) {
	return directory.openRegularFileWithMode(name, writable, 0, false)
}

func (directory *linuxOutputDirectory) openRegularFileExact(
	name string,
	writable bool,
	permissions uint32,
) (*linuxOutputRegularFile, error) {
	return directory.openRegularFileWithMode(name, writable, permissions, true)
}

func (directory *linuxOutputDirectory) openRegularFileWithMode(
	name string,
	writable bool,
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
	access := unix.O_RDONLY
	if writable {
		access = unix.O_RDWR
	}
	fd, err := directory.openRelative(name, access, 0)
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
		writable:                writable,
	}, nil
}

func (directory *linuxOutputDirectory) createRegularFileExact(
	name string,
	permissions uint32,
	size int64,
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
	if err := directory.validateCreateAuthority(); err != nil {
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
		writable:    true,
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
	if err := created.setExactMode(permissions); err != nil {
		return nil, err
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

func (targetDirectory *linuxOutputDirectory) linkRegularFileNoReplace(
	sourceDirectory *linuxOutputDirectory,
	sourceName string,
	expected *linuxOutputRegularFile,
	targetName string,
) error {
	const operation = "link output regular file"
	if err := targetDirectory.verifyHandle(); err != nil {
		return err
	}
	if err := linuxValidateComponent(operation, targetName); err != nil {
		return err
	}
	if err := linuxValidateComponent(operation, sourceName); err != nil {
		return err
	}
	if err := sourceDirectory.verifyHandle(); err != nil {
		return err
	}
	if expected == nil {
		return linuxUnsafe(operation, "source handle is absent", nil)
	}
	if err := expected.verifyHandle(); err != nil {
		return err
	}
	if expected.certificate.mount != targetDirectory.certificate.mount ||
		sourceDirectory.certificate.mount != targetDirectory.certificate.mount {
		return linuxUnsafe(operation, "source handle belongs to a different certified mount", nil)
	}
	matches, err := sourceDirectory.regularEntryMatches(sourceName, expected)
	if err != nil || !matches {
		// The no-replace primitive has not run yet, so this is deterministic
		// source-witness invalidation rather than ambiguous publication history.
		return errors.Join(
			errOutputV3LinkSourceChanged,
			linuxUnsafe(operation, "fixed source entry does not identify the expected open file", nil),
			err,
		)
	}
	// Linux requires CAP_DAC_READ_SEARCH for linkat(AT_EMPTY_PATH), so the
	// certified backend instead links the exact name beneath a pinned private
	// directory. Immediate pre/post identity checks preserve the ownership
	// witness while remaining usable by an ordinary unprivileged receiver.
	if err := targetDirectory.system.linkat(sourceDirectory.fd, sourceName, targetDirectory.fd, targetName, 0); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return &linuxOutputCollisionError{operation: operation, name: targetName, cause: err}
		}
		return fmt.Errorf("%s %q: %w", operation, targetName, err)
	}
	if err := targetDirectory.sync(); err != nil {
		return err
	}
	matches, err = targetDirectory.regularEntryMatches(targetName, expected)
	if err != nil {
		return err
	}
	if !matches {
		return linuxUnsafe(operation, "new hard link does not identify the expected open file", nil)
	}
	return nil
}

func (directory *linuxOutputDirectory) renameRegularFile(
	sourceName string,
	expected *linuxOutputRegularFile,
	targetDirectory *linuxOutputDirectory,
	targetName string,
	disposition linuxRenameDisposition,
) error {
	return linuxRenameVerifiedEntry(
		directory,
		sourceName,
		targetDirectory,
		targetName,
		disposition,
		"rename output regular file",
		"regular file",
		func(parent *linuxOutputDirectory, name string) (bool, error) {
			return parent.regularEntryMatches(name, expected)
		},
	)
}

func (directory *linuxOutputDirectory) unlinkRegularFile(
	name string,
	expected *linuxOutputRegularFile,
) error {
	const operation = "unlink output regular file"
	if err := directory.verifyHandle(); err != nil {
		return err
	}
	if err := linuxValidateComponent(operation, name); err != nil {
		return err
	}
	matches, err := directory.regularEntryMatches(name, expected)
	if err != nil {
		return err
	}
	if !matches {
		return linuxUnsafe(operation, "name no longer identifies the expected open file", nil)
	}
	if err := directory.system.unlinkat(directory.fd, name, 0); err != nil {
		return fmt.Errorf("%s %q: %w", operation, name, err)
	}
	return directory.sync()
}

func (directory *linuxOutputDirectory) unlinkDirectory(
	name string,
	expected *linuxOutputDirectory,
) error {
	const operation = "unlink output directory"
	if err := linuxVerifyDirectoryPair(directory, expected); err != nil {
		return err
	}
	if err := linuxValidateComponent(operation, name); err != nil {
		return err
	}
	matches, closeErr := directory.directoryEntryMatches(name, expected)
	if !matches {
		return errors.Join(
			linuxUnsafe(operation, "name no longer identifies the expected open directory", nil),
			closeErr,
		)
	}
	if closeErr != nil {
		return closeErr
	}
	if err := directory.system.unlinkat(directory.fd, name, unix.AT_REMOVEDIR); err != nil {
		return fmt.Errorf("%s %q: %w", operation, name, err)
	}
	return directory.sync()
}

func (directory *linuxOutputDirectory) renameDirectory(
	sourceName string,
	expected *linuxOutputDirectory,
	targetDirectory *linuxOutputDirectory,
	targetName string,
	disposition linuxRenameDisposition,
) error {
	return linuxRenameVerifiedEntry(
		directory,
		sourceName,
		targetDirectory,
		targetName,
		disposition,
		"rename output directory",
		"directory",
		func(parent *linuxOutputDirectory, name string) (bool, error) {
			return parent.directoryEntryMatches(name, expected)
		},
	)
}

func linuxRenameVerifiedEntry(
	sourceDirectory *linuxOutputDirectory,
	sourceName string,
	targetDirectory *linuxOutputDirectory,
	targetName string,
	disposition linuxRenameDisposition,
	operation string,
	entryKind string,
	matchesExpected func(*linuxOutputDirectory, string) (bool, error),
) error {
	if err := linuxVerifyDirectoryPair(sourceDirectory, targetDirectory); err != nil {
		return err
	}
	if err := linuxValidateComponent(operation, sourceName); err != nil {
		return err
	}
	if err := linuxValidateComponent(operation, targetName); err != nil {
		return err
	}
	matches, err := matchesExpected(sourceDirectory, sourceName)
	if err != nil {
		return err
	}
	if !matches {
		return linuxUnsafe(operation, "source name no longer identifies the expected "+entryKind, nil)
	}
	flags, err := linuxRenameFlags(operation, disposition)
	if err != nil {
		return err
	}
	if err := sourceDirectory.system.renameat2(
		sourceDirectory.fd,
		sourceName,
		targetDirectory.fd,
		targetName,
		flags,
	); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return &linuxOutputCollisionError{operation: operation, name: targetName, cause: err}
		}
		return fmt.Errorf("%s %q: %w", operation, targetName, err)
	}
	if err := linuxSyncRenamedParents(sourceDirectory, targetDirectory); err != nil {
		return err
	}
	matches, err = matchesExpected(targetDirectory, targetName)
	if err != nil {
		return err
	}
	if !matches {
		return linuxUnsafe(operation, "rename target does not identify the expected "+entryKind, nil)
	}
	return nil
}

func (directory *linuxOutputDirectory) directoryEntryMatches(
	name string,
	expected *linuxOutputDirectory,
) (bool, error) {
	if expected == nil {
		return false, linuxUnsafe("compare output directory entry", "expected directory handle is absent", nil)
	}
	if err := expected.verifyHandle(); err != nil {
		return false, err
	}
	opened, err := directory.openDirectory(name)
	if err != nil {
		return false, err
	}
	same := opened.object.sameObject(expected.object)
	return same, opened.close()
}
