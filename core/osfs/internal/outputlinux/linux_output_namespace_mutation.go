//go:build linux

package outputlinux

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"golang.org/x/sys/unix"
)

func (directory *linuxOutputDirectory) reservePublicDirectoryNoReplace(
	name string,
	permissions uint32,
) (result *linuxOutputDirectory, outcome outputcap.PublishNoReplaceOutcome, resultErr error) {
	const operation = "reserve public output directory"
	if err := directory.verifyHandle(); err != nil {
		return nil, 0, err
	}
	if err := linuxValidateComponent(operation, name); err != nil {
		return nil, 0, err
	}
	if err := linuxValidatePermissions(operation, permissions); err != nil {
		return nil, 0, err
	}
	if err := directory.validatePublicCreateAuthority(); err != nil {
		return nil, 0, err
	}
	if err := directory.system.mkdirat(directory.fd, name, permissions); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, outputcap.PublishNoReplaceCollision, nil
		}
		return nil, 0, fmt.Errorf("%s %q: %w", operation, name, err)
	}
	outcome = outputcap.PublishNoReplaceIndeterminate
	created, err := directory.openDirectory(name)
	if err != nil {
		return nil, outcome, err
	}
	// Past mkdirat's mutation cut, transfer the exact handle even when a later
	// durability or identity proof fails. The caller needs that object witness to
	// reconcile an indeterminate name without reopening by path.
	result = created
	if err := created.sync(); err != nil {
		return result, outcome, err
	}
	if err := directory.sync(); err != nil {
		return result, outcome, err
	}
	matches, err := directory.directoryEntryMatches(name, created)
	if err != nil || !matches {
		return result, outcome, errors.Join(
			linuxUnsafe(operation, "reserved name does not identify the created directory", nil), err)
	}
	return result, outputcap.PublishNoReplaceCommitted, nil
}

func (publicDirectory *linuxOutputDirectory) createLiveCleanupStage(
	proofDirectory *linuxOutputDirectory,
	name string,
	size int64,
) (result *linuxOutputRegularFile, resultErr error) {
	const operation = "create Linux live-cleanup stage"
	if err := linuxVerifyDirectoryPair(publicDirectory, proofDirectory); err != nil {
		return nil, err
	}
	if err := publicDirectory.validatePublicCreateAuthority(); err != nil {
		return nil, err
	}
	if !proofDirectory.requireExactPermissions {
		return nil, linuxUnsafe(operation, "proof directory is not a private authority boundary", nil)
	}
	if err := proofDirectory.validatePrivateAuthority(operation); err != nil {
		return nil, err
	}
	if err := linuxValidateComponent(operation, name); err != nil {
		return nil, err
	}
	if size < 0 {
		return nil, linuxUnsafe(operation, "stage size cannot be negative", nil)
	}

	// O_TMPFILE applies the public final parent's umask/default ACL profile without
	// exposing a public name. Before linkat the kernel deletes the anonymous inode
	// on close; after linkat the protected proof name is the cleanup authority.
	fd, err := publicDirectory.openRelative(".", unix.O_TMPFILE|unix.O_RDWR, linuxPublicFileCreateMode)
	if err != nil {
		return nil, err
	}
	stage := &linuxOutputRegularFile{
		system: publicDirectory.system, fd: fd, certificate: publicDirectory.certificate,
		writable: true,
	}
	defer func() {
		if result == nil {
			resultErr = errors.Join(resultErr, stage.close())
		}
	}()
	identity, err := stage.currentIdentity()
	if err != nil {
		return nil, err
	}
	stage.object = identity.identity
	if err := stage.truncate(size); err != nil {
		return nil, err
	}
	if err := stage.sync(); err != nil {
		return nil, err
	}
	if err := publicDirectory.system.linkat(
		stage.fd, "", proofDirectory.fd, name, unix.AT_EMPTY_PATH,
	); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, &linuxOutputCollisionError{operation: operation, name: name, cause: err}
		}
		return nil, fmt.Errorf("%s %q: %w", operation, name, err)
	}
	// From this cut onward the stage may be durably named. Return errors without
	// unlinking; the journal reconciler must observe the exact proof name.
	if err := proofDirectory.sync(); err != nil {
		return nil, err
	}
	matches, err := proofDirectory.regularEntryMatches(name, stage)
	if err != nil || !matches {
		return nil, errors.Join(
			linuxUnsafe(operation, "proof name does not identify the anonymous stage", nil), err)
	}
	result = stage
	return result, nil
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
	if expected == nil {
		return linuxUnsafe(operation, "source handle is absent", nil)
	}
	if err := expected.verifyHandle(); err != nil {
		return err
	}
	if expected.certificate.mount != targetDirectory.certificate.mount {
		return linuxUnsafe(operation, "source handle belongs to a different certified mount", nil)
	}
	sourceFD := expected.fd
	sourcePath := ""
	linkFlags := unix.AT_EMPTY_PATH
	if sourceDirectory != nil {
		if err := linuxValidateComponent(operation, sourceName); err != nil {
			return err
		}
		if err := sourceDirectory.verifyHandle(); err != nil {
			return err
		}
		if sourceDirectory.certificate.mount != targetDirectory.certificate.mount {
			return linuxUnsafe(operation, "source name belongs to a different certified mount", nil)
		}
		matches, err := sourceDirectory.regularEntryMatches(sourceName, expected)
		if err != nil || !matches {
			// The no-replace primitive has not run yet, so this is deterministic
			// source-witness invalidation rather than ambiguous publication history.
			return errors.Join(
				outputcap.ErrFixedLinkSourceChanged,
				linuxUnsafe(operation, "fixed source entry does not identify the expected open file", nil),
				err,
			)
		}
		sourceFD = sourceDirectory.fd
		sourcePath = sourceName
		linkFlags = 0
	}
	// Anonymous O_TMPFILE handles use AT_EMPTY_PATH; named proof stages use the
	// retained protected parent and exact source name. Both retain the same inode.
	if err := targetDirectory.system.linkat(sourceFD, sourcePath, targetDirectory.fd, targetName, linkFlags); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return &linuxOutputCollisionError{operation: operation, name: targetName, cause: err}
		}
		return fmt.Errorf("%s %q: %w", operation, targetName, err)
	}
	// Once linkat succeeds the public final may be visible. Any later error is an
	// indeterminate publication cut and must preserve both names for reconciliation.
	if err := targetDirectory.sync(); err != nil {
		return errors.Join(errLinuxOutputPublishIndeterminate, err)
	}
	matches, err := targetDirectory.regularEntryMatches(targetName, expected)
	if err != nil {
		return errors.Join(errLinuxOutputPublishIndeterminate, err)
	}
	if !matches {
		return errors.Join(errLinuxOutputPublishIndeterminate,
			linuxUnsafe(operation, "new hard link does not identify the expected open file", nil))
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
		if disposition == linuxRenameNoReplace {
			// Linux documents RENAME_NOREPLACE as atomic, but an unexpected
			// syscall failure has no trusted post-state witness. Recovery must
			// reconcile the fixed target identity rather than infer no change.
			return errors.Join(errLinuxOutputPublishIndeterminate,
				fmt.Errorf("%s %q: %w", operation, targetName, err))
		}
		return fmt.Errorf("%s %q: %w", operation, targetName, err)
	}
	if err := linuxSyncRenamedParents(sourceDirectory, targetDirectory); err != nil {
		if disposition == linuxRenameNoReplace {
			return errors.Join(errLinuxOutputPublishIndeterminate, err)
		}
		return err
	}
	matches, err = matchesExpected(targetDirectory, targetName)
	if err != nil {
		if disposition == linuxRenameNoReplace {
			return errors.Join(errLinuxOutputPublishIndeterminate, err)
		}
		return err
	}
	if !matches {
		mismatch := linuxUnsafe(operation, "rename target does not identify the expected "+entryKind, nil)
		if disposition == linuxRenameNoReplace {
			return errors.Join(errLinuxOutputPublishIndeterminate, mismatch)
		}
		return mismatch
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
