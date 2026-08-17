// Package checkpointstore owns the native FileCheckpointV2 records and the
// ordinary-operation registry that bounds resume, list, and discard work.
//
// The package deliberately exposes only live capability handles to the output
// runtime. Session headers and terminal receipt/history models never enter this
// module, so they cannot become accidental prerequisites for recovery.
package checkpointstore

import (
	"bytes"
	"errors"
	"io/fs"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

const (
	ControlDirectory     = ".windshare-output"
	CheckpointsDirectory = "checkpoints"
	RecordsDirectory     = "records"
	AnchorsDirectory     = "anchors"
	StagesDirectory      = "stages"
	OwnershipFile        = "marker"
)

var checkpointEntries = map[string]outputcap.EntryKind{
	RecordsDirectory: outputcap.EntryDirectory,
	AnchorsDirectory: outputcap.EntryDirectory,
	StagesDirectory:  outputcap.EntryDirectory,
}

type OwnershipStatus uint8

const (
	OwnershipMatched OwnershipStatus = iota + 1
	OwnershipAbsent
	OwnershipMismatch
	OwnershipRecoverable
)

func openOrCreateDirectory(parent outputcap.Directory, name string) (outputcap.Directory, error) {
	if parent == nil || name == "" {
		return nil, transfer.ErrInvalidOutputBinding
	}
	kind, exact, err := parent.ClassifyExactEntry(name)
	if err != nil {
		return nil, err
	}
	if kind != outputcap.EntryAbsent {
		if !exact || kind != outputcap.EntryDirectory {
			return nil, outputcap.ErrUnsafeNamespace
		}
		opened, err := parent.OpenDirectory(name, true)
		if err != nil {
			return nil, errors.Join(err, closeDirectory(opened))
		}
		if opened == nil {
			return nil, outputcap.ErrUnsafeNamespace
		}
		return opened, nil
	}
	created, createErr := parent.CreateDirectory(name, true)
	if createErr == nil {
		if created == nil {
			return nil, outputcap.ErrUnsafeNamespace
		}
		if syncErr := errors.Join(created.Sync(), parent.Sync()); syncErr != nil {
			return nil, errors.Join(syncErr, created.Close())
		}
		return created, nil
	}
	closeErr := closeDirectory(created)
	if errors.Is(createErr, outputcap.ErrNamespaceCollision) {
		if closeErr != nil {
			return nil, closeErr
		}
		return openExistingDirectory(parent, name)
	}
	return nil, errors.Join(createErr, closeErr)
}

func inspectOwnership(
	directory outputcap.Directory,
	expected checkpointmodel.Ownership,
) (OwnershipStatus, error) {
	kind, err := directory.ObserveEntry(OwnershipFile)
	if err != nil {
		if candidateContention(err) {
			return 0, errors.Join(outputcap.ErrNamespaceLockBusy, err)
		}
		return 0, err
	}
	if kind == outputcap.EntryAbsent {
		return inspectAbsentOwnership(directory, expected)
	}
	if kind != outputcap.EntryRegularFile {
		return OwnershipMismatch, nil
	}
	actual, readErr := ReadFile(directory, OwnershipFile)
	if readErr != nil {
		if candidateContention(readErr) {
			return 0, errors.Join(outputcap.ErrNamespaceLockBusy, readErr)
		}
		return 0, readErr
	}
	decoded, decodeErr := checkpointmodel.DecodeOwnership(actual)
	if decodeErr != nil || !bytes.Equal(decoded.CanonicalBytes(), expected.CanonicalBytes()) {
		return OwnershipMismatch, nil
	}
	return OwnershipMatched, nil
}

func inspectAbsentOwnership(
	directory outputcap.Directory,
	expected checkpointmodel.Ownership,
) (OwnershipStatus, error) {
	encoded, err := checkpointmodel.EncodeOwnership(expected)
	if err != nil {
		return 0, err
	}
	recoverable, candidateErr := exactOwnershipCandidate(directory, encoded)
	// Absence and candidate enumeration are separate handle-relative
	// observations. Another initializer may publish the exact marker between
	// them; re-reading the fixed target prevents that successful race from being
	// misclassified as foreign ownership.
	matched, markerErr := ownershipImageMatches(directory, expected)
	switch {
	case markerErr != nil:
		return 0, markerErr
	case matched:
		return OwnershipMatched, nil
	case candidateErr != nil:
		return OwnershipMismatch, candidateErr
	case recoverable:
		return OwnershipRecoverable, nil
	default:
		return OwnershipAbsent, nil
	}
}

func ownershipImageMatches(
	directory outputcap.Directory,
	expected checkpointmodel.Ownership,
) (bool, error) {
	actual, err := ReadFile(directory, OwnershipFile)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		if candidateContention(err) {
			return false, errors.Join(outputcap.ErrNamespaceLockBusy, err)
		}
		return false, err
	}
	decoded, err := checkpointmodel.DecodeOwnership(actual)
	if err != nil {
		return false, nil
	}
	return bytes.Equal(decoded.CanonicalBytes(), expected.CanonicalBytes()), nil
}

func ensureOwnership(
	directory outputcap.Directory,
	expected checkpointmodel.Ownership,
	allowCreate bool,
) error {
	encoded, err := checkpointmodel.EncodeOwnership(expected)
	if err != nil {
		return err
	}
	status, err := inspectOwnership(directory, expected)
	if err != nil {
		return err
	}
	if status == OwnershipMatched {
		return nil
	}
	if (status != OwnershipAbsent && status != OwnershipRecoverable) || !allowCreate {
		return ownershipMismatch(nil)
	}
	return installOwnedFile(directory, OwnershipFile, encoded)
}

func installOwnedFile(directory outputcap.Directory, name string, encoded []byte) error {
	err := InstallCreate(directory, name, encoded)
	if errors.Is(err, checkpointmodel.ErrInvalidRecord) ||
		errors.Is(err, checkpointmodel.ErrRecordBinding) {
		return ownershipMismatch(err)
	}
	return err
}

func exactOwnershipCandidate(directory outputcap.Directory, encoded []byte) (bool, error) {
	names, err := directory.Names(installationAttempts + 1)
	if err != nil {
		if candidateContention(err) {
			return false, errors.Join(outputcap.ErrNamespaceLockBusy, err)
		}
		return false, err
	}
	if len(names) == 0 {
		return false, nil
	}
	if len(names) > installationAttempts {
		return false, ownershipMismatch(nil)
	}
	for _, name := range names {
		if !MatchesTemporaryName(name, OwnershipFile, encoded) {
			// Marker candidates prove only their own interrupted installation. They
			// never manufacture ownership for unrelated checkpoint-root siblings.
			return false, ownershipMismatch(nil)
		}
		kind, err := directory.ObserveEntry(name)
		if err != nil {
			if candidateContention(err) {
				return false, errors.Join(outputcap.ErrNamespaceLockBusy, err)
			}
			return false, err
		}
		if kind != outputcap.EntryRegularFile {
			return false, ownershipMismatch(nil)
		}
		file, err := openExactTemporary(directory, name, encoded)
		if errors.Is(err, fs.ErrNotExist) {
			// The name disappeared after enumeration only when another bootstrap
			// crossed this one. Let the caller converge through the target recheck.
			return false, errors.Join(outputcap.ErrNamespaceLockBusy, err)
		}
		if candidateContention(err) {
			return false, errors.Join(outputcap.ErrNamespaceLockBusy, err)
		}
		if err != nil {
			return false, ownershipMismatch(err)
		}
		if err := file.Close(); err != nil {
			return false, err
		}
	}
	return true, nil
}

func ownershipMismatch(cause error) error {
	return errors.Join(checkpointmodel.ErrInvalidOwnership, cause)
}

func closeDirectory(directory outputcap.Directory) error {
	if directory == nil {
		return nil
	}
	return directory.Close()
}

func closeFile(file outputcap.FileIdentity) error {
	if file == nil {
		return nil
	}
	return file.Close()
}

func closeLock(lock outputcap.Lock) error {
	if lock == nil {
		return nil
	}
	return lock.Close()
}

func openExistingDirectory(parent outputcap.Directory, name string) (outputcap.Directory, error) {
	if parent == nil || name == "" {
		return nil, transfer.ErrInvalidOutputBinding
	}
	kind, exact, err := parent.ClassifyExactEntry(name)
	if err != nil {
		return nil, err
	}
	if kind == outputcap.EntryAbsent {
		return nil, fs.ErrNotExist
	}
	if !exact || kind != outputcap.EntryDirectory {
		return nil, outputcap.ErrUnsafeNamespace
	}
	opened, err := parent.OpenDirectory(name, true)
	if err != nil {
		return nil, errors.Join(err, closeDirectory(opened))
	}
	if opened == nil {
		return nil, outputcap.ErrUnsafeNamespace
	}
	return opened, nil
}

func validateAllowedEntries(directory outputcap.Directory, allowed map[string]outputcap.EntryKind) error {
	if directory == nil {
		return transfer.ErrInvalidOutputBinding
	}
	names, err := directory.Names(len(allowed) + 1)
	if err != nil {
		return err
	}
	if len(names) > len(allowed) {
		return outputcap.ErrUnsafeNamespace
	}
	for _, name := range names {
		expected, known := allowed[name]
		if !known {
			return outputcap.ErrUnsafeNamespace
		}
		kind, exact, err := directory.ClassifyExactEntry(name)
		if err != nil {
			return err
		}
		if !exact || kind != expected {
			return outputcap.ErrUnsafeNamespace
		}
	}
	return nil
}
