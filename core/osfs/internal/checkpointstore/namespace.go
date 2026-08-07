// Package checkpointstore owns the native FileCheckpointV1 namespace.
//
// The package deliberately exposes only live capability handles to the output
// runtime. Legacy session headers never enter this module, so they cannot become
// an accidental prerequisite for checkpoint recovery.
package checkpointstore

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io/fs"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

const (
	OwnershipFile      = "ownership.marker"
	IntentsDirectory   = "intents"
	RecordsDirectory   = "records"
	RuntimeLockFile    = "runtime.lock"
	markerMaximumBytes = resumestate.MaxSessionHeaderBytes
	installAttempts    = outputnamespace.AllocationAttempts
)

// Config binds the namespace to one certified output root and transfer intent.
type Config struct {
	Root         outputcap.Directory
	BackendID    transfer.OutputBackendID
	RootIdentity []byte
	Intent       transfer.TransferIntentDigest
}

// NamespaceConfig binds the checkpoint namespace to the persistent identity of
// the already certified native root. A pathname is deliberately absent: it can
// locate a different object after a mount, reparse, or directory replacement.
type NamespaceConfig struct {
	Root         outputcap.Directory
	BackendID    transfer.OutputBackendID
	RootIdentity []byte
}

type OwnershipStatus uint8

const (
	OwnershipMatched OwnershipStatus = iota + 1
	OwnershipAbsent
	OwnershipMismatch
	OwnershipRecoverable
)

// Claim is the complete live authority for one intent. Callers either close the
// claim or transfer every handle into one runtime session; partial ownership is
// intentionally not representable.
type Claim struct {
	Intent  outputcap.Directory
	Records outputcap.Directory
	Anchors outputcap.Directory
	Stages  outputcap.Directory
	Lock    outputcap.Lock
}

func (claim *Claim) Close() error {
	if claim == nil {
		return nil
	}
	err := errors.Join(
		closeLock(claim.Lock), closeDirectory(claim.Records),
		closeDirectory(claim.Anchors), closeDirectory(claim.Stages), closeDirectory(claim.Intent),
	)
	*claim = Claim{}
	return err
}

func Open(config Config) (result Claim, resultErr error) {
	if config.Root == nil || config.Intent.IsZero() || len(config.RootIdentity) != sha256.Size {
		return Claim{}, transfer.ErrInvalidOutputBinding
	}
	if _, err := transfer.NewOutputBackendID(string(config.BackendID)); err != nil {
		return Claim{}, transfer.ErrInvalidOutputBinding
	}
	checkpointRoot, err := OpenOwnedNamespace(NamespaceConfig{
		Root: config.Root, BackendID: config.BackendID, RootIdentity: config.RootIdentity,
	})
	if err != nil {
		return Claim{}, err
	}
	defer checkpointRoot.Close()
	intents, err := openOrCreateDirectory(checkpointRoot, IntentsDirectory)
	if err != nil {
		return Claim{}, err
	}
	defer intents.Close()
	intent, err := openOrCreateDirectory(intents, resumestate.IntentNamespaceName(config.Intent))
	if err != nil {
		return Claim{}, err
	}
	result.Intent = intent
	defer func() {
		if resultErr != nil {
			_ = result.Close()
		}
	}()
	if result.Records, err = openOrCreateDirectory(intent, RecordsDirectory); err != nil {
		return result, err
	}
	if result.Anchors, err = openOrCreateDirectory(intent, resumestate.AnchorsDirectoryName); err != nil {
		return result, err
	}
	if result.Stages, err = openOrCreateDirectory(intent, resumestate.StagesDirectoryName); err != nil {
		return result, err
	}
	lock, _, err := intent.AcquireLock(RuntimeLockFile, false)
	if err != nil {
		return result, err
	}
	result.Lock = lock
	return result, nil
}

func InspectOwnership(config NamespaceConfig) (OwnershipStatus, error) {
	if err := validateNamespaceConfig(config); err != nil {
		return 0, err
	}
	control, err := config.Root.OpenDirectory(resumestate.ControlDirectoryName, true)
	if errors.Is(err, fs.ErrNotExist) {
		return OwnershipAbsent, nil
	}
	if err != nil {
		return 0, err
	}
	defer control.Close()
	checkpointRoot, err := control.OpenDirectory(resumestate.CheckpointsDirectoryName, true)
	if errors.Is(err, fs.ErrNotExist) {
		return OwnershipAbsent, nil
	}
	if err != nil {
		return 0, err
	}
	defer checkpointRoot.Close()
	expected, err := namespaceOwnership(config)
	if err != nil {
		return 0, err
	}
	return inspectOwnership(checkpointRoot, expected)
}

// BootstrapOwnership installs the V1 marker only after the caller has proved
// that an absent marker is uncontested. The cleaner owns that proof while
// holding the certified-root guard and the retired coordinator lock domain.
func BootstrapOwnership(config NamespaceConfig) error {
	if err := validateNamespaceConfig(config); err != nil {
		return err
	}
	control, err := openOrCreateDirectory(config.Root, resumestate.ControlDirectoryName)
	if err != nil {
		return err
	}
	checkpointRoot, openErr := openOrCreateDirectory(control, resumestate.CheckpointsDirectoryName)
	if openErr != nil {
		return errors.Join(openErr, control.Close())
	}
	expected, ownershipErr := namespaceOwnership(config)
	if ownershipErr == nil {
		ownershipErr = ensureOwnership(checkpointRoot, expected, true)
	}
	return errors.Join(ownershipErr, checkpointRoot.Close(), control.Sync(), control.Close())
}

func OpenOwnedNamespace(config NamespaceConfig) (outputcap.Directory, error) {
	if err := validateNamespaceConfig(config); err != nil {
		return nil, err
	}
	control, err := config.Root.OpenDirectory(resumestate.ControlDirectoryName, true)
	if err != nil {
		return nil, err
	}
	checkpointRoot, openErr := control.OpenDirectory(resumestate.CheckpointsDirectoryName, true)
	if openErr != nil {
		return nil, errors.Join(openErr, control.Close())
	}
	expected, ownershipErr := namespaceOwnership(config)
	if ownershipErr == nil {
		var status OwnershipStatus
		status, ownershipErr = inspectOwnership(checkpointRoot, expected)
		if status != OwnershipMatched && ownershipErr == nil {
			ownershipErr = resumestate.ErrFileCheckpointOwnership
		}
	}
	if closeErr := control.Close(); closeErr != nil {
		ownershipErr = errors.Join(ownershipErr, closeErr)
	}
	if ownershipErr != nil {
		return nil, errors.Join(ownershipErr, checkpointRoot.Close())
	}
	return checkpointRoot, nil
}

func validateNamespaceConfig(config NamespaceConfig) error {
	if config.Root == nil || len(config.RootIdentity) != sha256.Size {
		return transfer.ErrInvalidOutputBinding
	}
	if _, err := transfer.NewOutputBackendID(string(config.BackendID)); err != nil {
		return transfer.ErrInvalidOutputBinding
	}
	return nil
}

func namespaceOwnership(config NamespaceConfig) (resumestate.FileCheckpointOwnership, error) {
	return resumestate.NewFileCheckpointOwnership(string(config.BackendID), config.RootIdentity)
}

func openOrCreateDirectory(parent outputcap.Directory, name string) (outputcap.Directory, error) {
	if parent == nil || name == "" {
		return nil, transfer.ErrInvalidOutputBinding
	}
	opened, err := parent.OpenDirectory(name, true)
	if err == nil {
		return opened, nil
	}
	if opened != nil {
		_ = opened.Close()
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	created, createErr := parent.CreateDirectory(name, true)
	if createErr == nil {
		if syncErr := parent.Sync(); syncErr != nil {
			return nil, errors.Join(syncErr, created.Close())
		}
		return created, nil
	}
	if created != nil {
		_ = created.Close()
	}
	if errors.Is(createErr, outputcap.ErrNamespaceCollision) {
		return parent.OpenDirectory(name, true)
	}
	return nil, createErr
}

func inspectOwnership(
	directory outputcap.Directory,
	expected resumestate.FileCheckpointOwnership,
) (OwnershipStatus, error) {
	kind, err := directory.ObserveEntry(OwnershipFile)
	if err != nil {
		if candidateContention(err) {
			return 0, errors.Join(outputcap.ErrNamespaceLockBusy, err)
		}
		return 0, err
	}
	if kind == outputcap.EntryAbsent {
		encoded, encodeErr := resumestate.EncodeFileCheckpointOwnership(expected)
		if encodeErr != nil {
			return 0, encodeErr
		}
		recoverable, candidateErr := exactOwnershipCandidate(directory, encoded)
		if candidateErr != nil {
			return OwnershipMismatch, candidateErr
		}
		if recoverable {
			return OwnershipRecoverable, nil
		}
		return OwnershipAbsent, nil
	}
	if kind != outputcap.EntryRegularFile {
		return OwnershipMismatch, nil
	}
	actual, readErr := outputnamespace.ReadRecord(directory, OwnershipFile, markerMaximumBytes)
	if readErr != nil {
		if candidateContention(readErr) {
			return 0, errors.Join(outputcap.ErrNamespaceLockBusy, readErr)
		}
		return 0, readErr
	}
	decoded, decodeErr := resumestate.DecodeFileCheckpointOwnership(actual)
	if decodeErr != nil || !bytes.Equal(decoded.CanonicalBytes(), expected.CanonicalBytes()) {
		return OwnershipMismatch, nil
	}
	return OwnershipMatched, nil
}

func ensureOwnership(
	directory outputcap.Directory,
	expected resumestate.FileCheckpointOwnership,
	allowCreate bool,
) error {
	encoded, err := resumestate.EncodeFileCheckpointOwnership(expected)
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
		return resumestate.ErrFileCheckpointOwnership
	}
	return installOwnedFile(directory, OwnershipFile, encoded)
}

func installOwnedFile(directory outputcap.Directory, name string, encoded []byte) error {
	err := InstallCreate(directory, name, encoded)
	if errors.Is(err, resumestate.ErrInvalidFileCheckpoint) ||
		errors.Is(err, resumestate.ErrFileCheckpointBinding) {
		return errors.Join(resumestate.ErrFileCheckpointOwnership, err)
	}
	return err
}

func exactOwnershipCandidate(directory outputcap.Directory, encoded []byte) (bool, error) {
	names, err := directory.Names(installAttempts + 1)
	if err != nil {
		if candidateContention(err) {
			return false, errors.Join(outputcap.ErrNamespaceLockBusy, err)
		}
		return false, err
	}
	if len(names) == 0 {
		return false, nil
	}
	if len(names) != 1 || !MatchesTemporaryName(names[0], OwnershipFile, encoded) {
		// A marker candidate proves only its own interrupted installation. It must
		// never manufacture ownership for unrelated checkpoint-root siblings.
		return false, resumestate.ErrFileCheckpointOwnership
	}
	name := names[0]
	kind, err := directory.ObserveEntry(name)
	if err != nil {
		if candidateContention(err) {
			return false, errors.Join(outputcap.ErrNamespaceLockBusy, err)
		}
		return false, err
	}
	if kind != outputcap.EntryRegularFile {
		return false, resumestate.ErrFileCheckpointOwnership
	}
	file, err := openExactTemporary(directory, name, encoded)
	if errors.Is(err, fs.ErrNotExist) {
		// The name disappeared after enumeration only when another bootstrap
		// crossed this one. Let the caller converge through the namespace lock.
		return false, errors.Join(outputcap.ErrNamespaceLockBusy, err)
	}
	if candidateContention(err) {
		return false, errors.Join(outputcap.ErrNamespaceLockBusy, err)
	}
	if err != nil {
		return false, errors.Join(resumestate.ErrFileCheckpointOwnership, err)
	}
	return true, file.Close()
}

func closeDirectory(directory outputcap.Directory) error {
	if directory == nil {
		return nil
	}
	return directory.Close()
}

func closeFile(file outputcap.File) error {
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
