package checkpointstore

import (
	"errors"
	"io/fs"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type OperationRegistryLease struct {
	registry  *OperationRegistry
	operation receivecontract.OperationID
	record    checkpointmodel.OrdinaryOperationRecord
	lock      outputcap.Lock
	files     *fileExecutionAuthority
	deleted   bool
}

func (lease *OperationRegistryLease) Record() checkpointmodel.OrdinaryOperationRecord {
	if lease == nil {
		return checkpointmodel.OrdinaryOperationRecord{}
	}
	return lease.record
}

func (lease *OperationRegistryLease) Deleted() bool {
	return lease != nil && lease.deleted
}

// OpenFileState retains file checkpoints below the exact leased operation.
// A fresh operation may create the namespace without enumerating it; only a
// matched resume asks the file store to reconcile its bounded contents.
func (lease *OperationRegistryLease) OpenFileState(
	create bool,
) (outputcap.Directory, error) {
	if lease == nil || lease.registry == nil || lease.lock == nil ||
		lease.operation.IsZero() || !lease.record.Valid() {
		return nil, transfer.ErrInvalidOutputBinding
	}
	operation, err := openExistingDirectory(
		lease.registry.operations, operationNamespaceName(lease.operation),
	)
	if err != nil {
		return nil, repositoryError("open ordinary operation file state", err)
	}
	open := openExistingDirectory
	if create {
		open = openOrCreateDirectory
	}
	files, openErr := open(operation, ordinaryFileStateDirectory)
	closeErr := operation.Close()
	if openErr != nil || closeErr != nil {
		return nil, repositoryError(
			"open ordinary operation file state",
			errors.Join(openErr, closeErr, closeDirectory(files)),
		)
	}
	return files, nil
}

// AcquireOperationLease is reserved for explicit resume/list/discard flows.
// Ordinary first-download admission uses ActiveAdmission and never inventories.
func (registry *OperationRegistry) AcquireOperationLease(
	operation receivecontract.OperationID,
) (*OperationRegistryLease, error) {
	if !registry.valid() || operation.IsZero() {
		return nil, transfer.ErrInvalidOutputBinding
	}
	lease, err := registry.acquireOperationLease(operation)
	if err != nil {
		return nil, err
	}
	record, _, err := registry.readOperation(operation)
	if err != nil {
		return nil, errors.Join(err, lease.Close())
	}
	held := record
	if record.Lease() == checkpointmodel.OrdinaryLeaseReleased {
		held, err = checkpointmodel.NextOrdinaryOperationRecord(
			record,
			checkpointmodel.NextOrdinaryOperationRecordSpec{
				Lifecycle: record.Lifecycle(), Lease: checkpointmodel.OrdinaryLeaseHeld,
				ClosedReason: record.ClosedReason(),
			},
		)
		if err == nil {
			err = registry.replaceOperation(record, held)
		}
		if err != nil {
			return nil, errors.Join(err, lease.Close())
		}
	}
	lease.record = held
	return lease, nil
}

func (registry *OperationRegistry) acquireOperationLease(
	operation receivecontract.OperationID,
) (*OperationRegistryLease, error) {
	if !registry.valid() || operation.IsZero() {
		return nil, transfer.ErrInvalidOutputBinding
	}
	lock, created, err := registry.leases.AcquireLock(operationLeaseNameV1(operation), false)
	if err != nil {
		return nil, repositoryError("acquire exact ordinary operation lease", errors.Join(err, closeLock(lock)))
	}
	if lock == nil {
		return nil, codedError(ErrorUnsafeInstall, "acquire exact ordinary operation lease", outputcap.ErrUnsafeNamespace)
	}
	if created {
		if err := registry.leases.Sync(); err != nil {
			return nil, repositoryError("sync exact ordinary operation lease", errors.Join(err, lock.Close()))
		}
	}
	return &OperationRegistryLease{
		registry: registry, operation: operation, lock: lock,
		files: newFileExecutionAuthority(),
	}, nil
}

func (lease *OperationRegistryLease) Replace(
	previous checkpointmodel.OrdinaryOperationRecord,
	next checkpointmodel.OrdinaryOperationRecord,
) error {
	if lease == nil || lease.registry == nil || lease.lock == nil || !previous.Valid() || !next.Valid() ||
		previous.OperationID() != lease.operation || next.OperationID() != lease.operation ||
		!sameOrdinaryRecord(previous, lease.record) || !checkpointmodel.SameOrdinaryOperation(previous, next) ||
		next.LifecycleGeneration() != previous.LifecycleGeneration()+1 {
		return transfer.ErrInvalidOutputBinding
	}
	if err := validateOrdinaryRecordTransition(previous, next); err != nil {
		return err
	}
	if err := lease.registry.replaceOperation(previous, next); err != nil {
		return err
	}
	if previous.Lifecycle().ParticipatesInActiveLookup() {
		var indexErr error
		if !next.Lifecycle().ParticipatesInActiveLookup() {
			indexErr = lease.registry.removeActiveIndex(previous)
		}
		if indexErr != nil {
			lease.record = next
			return indexErr
		}
	}
	if !next.Lifecycle().ParticipatesInActiveLookup() {
		// Active publication deliberately precedes candidate retirement. A crash at
		// that cut can leave both exact images; terminal de-indexing must retire the
		// candidate too or it would later masquerade as a new needs-attention match.
		if err := lease.registry.removeOperationCandidate(next); err != nil {
			lease.record = next
			return err
		}
	}
	lease.record = next
	return nil
}

// CleanupEmptyFileState is restartable after every directory-removal cut. It
// removes only authenticated, empty ordinary checkpoint namespaces; any
// remaining or unknown entry keeps terminal cleanup pending.
func (lease *OperationRegistryLease) CleanupEmptyFileState() (resultErr error) {
	if lease == nil || lease.registry == nil || lease.lock == nil || lease.deleted ||
		!lease.record.Valid() || lease.record.Lease() != checkpointmodel.OrdinaryLeaseHeld ||
		lease.record.Lifecycle() != checkpointmodel.OrdinaryOperationCompleted &&
			lease.record.Lifecycle() != checkpointmodel.OrdinaryOperationDiscarded &&
			lease.record.Lifecycle() != checkpointmodel.OrdinaryOperationCleanupPending {
		return transfer.ErrInvalidOutputBinding
	}
	operation, err := openExistingDirectory(
		lease.registry.operations, operationNamespaceName(lease.operation),
	)
	if err != nil {
		return repositoryError("open terminal ordinary file state", err)
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()

	files, absent, err := openTerminalOrdinaryFileState(operation)
	if err != nil || absent {
		return err
	}
	filesOwned := true
	defer func() {
		if filesOwned {
			resultErr = errors.Join(resultErr, files.Close())
		}
	}()
	if err := cleanupTerminalOrdinaryCheckpoints(files); err != nil {
		return err
	}
	filesOwned = false
	if err := removeEmptyDirectory(operation, ordinaryFileStateDirectory, files); err != nil {
		return repositoryError("cleanup terminal ordinary file state", err)
	}
	return nil
}

func openTerminalOrdinaryFileState(
	operation outputcap.Directory,
) (outputcap.Directory, bool, error) {
	kind, exact, err := operation.ClassifyExactEntry(ordinaryFileStateDirectory)
	if err != nil || !exact {
		return nil, false, repositoryError(
			"authenticate terminal ordinary file state", errors.Join(err, outputcap.ErrUnsafeNamespace),
		)
	}
	if kind == outputcap.EntryAbsent {
		return nil, true, nil
	}
	if kind != outputcap.EntryDirectory {
		return nil, false, repositoryError(
			"authenticate terminal ordinary file state", outputcap.ErrUnsafeNamespace,
		)
	}
	files, err := openExistingDirectory(operation, ordinaryFileStateDirectory)
	if err != nil {
		return nil, false, repositoryError("open terminal ordinary file state", err)
	}
	if err := validateAllowedEntries(files, map[string]outputcap.EntryKind{
		CheckpointsDirectory: outputcap.EntryDirectory,
	}); err != nil {
		return nil, false, repositoryError(
			"authenticate terminal ordinary file state", errors.Join(err, files.Close()),
		)
	}
	return files, false, nil
}

func cleanupTerminalOrdinaryCheckpoints(files outputcap.Directory) (resultErr error) {
	kind, exact, err := files.ClassifyExactEntry(CheckpointsDirectory)
	if err != nil || !exact {
		return repositoryError(
			"authenticate terminal ordinary checkpoints", errors.Join(err, outputcap.ErrUnsafeNamespace),
		)
	}
	if kind == outputcap.EntryAbsent {
		return nil
	}
	if kind != outputcap.EntryDirectory {
		return repositoryError("authenticate terminal ordinary checkpoints", outputcap.ErrUnsafeNamespace)
	}
	checkpoints, err := openExistingDirectory(files, CheckpointsDirectory)
	if err != nil {
		return repositoryError("open terminal ordinary checkpoints", err)
	}
	checkpointsOwned := true
	defer func() {
		if checkpointsOwned {
			resultErr = errors.Join(resultErr, checkpoints.Close())
		}
	}()
	if err := validateAllowedEntries(checkpoints, checkpointEntries); err != nil {
		return repositoryError("authenticate terminal ordinary checkpoints", err)
	}
	if err := cleanupTerminalCheckpointNamespaces(checkpoints); err != nil {
		return err
	}
	checkpointsOwned = false
	if err := removeEmptyDirectory(files, CheckpointsDirectory, checkpoints); err != nil {
		return repositoryError("cleanup terminal ordinary checkpoints", err)
	}
	return nil
}

func cleanupTerminalCheckpointNamespaces(checkpoints outputcap.Directory) error {
	for _, name := range []string{RecordsDirectory, AnchorsDirectory, StagesDirectory} {
		entryKind, exact, err := checkpoints.ClassifyExactEntry(name)
		if err != nil || !exact {
			return repositoryError(
				"authenticate terminal ordinary checkpoint namespace", errors.Join(err, outputcap.ErrUnsafeNamespace),
			)
		}
		if entryKind == outputcap.EntryAbsent {
			continue
		}
		if entryKind != outputcap.EntryDirectory {
			return repositoryError(
				"authenticate terminal ordinary checkpoint namespace", outputcap.ErrUnsafeNamespace,
			)
		}
		root, err := openExistingDirectory(checkpoints, name)
		if err != nil {
			return repositoryError("open terminal ordinary checkpoint namespace", err)
		}
		if err := removeEmptyShards(root); err != nil {
			return repositoryError(
				"cleanup terminal ordinary checkpoint shards", errors.Join(err, root.Close()),
			)
		}
		if err := removeEmptyDirectory(checkpoints, name, root); err != nil {
			return repositoryError("cleanup terminal ordinary checkpoint namespace", err)
		}
	}
	return nil
}

// DeleteTerminal removes crash-only metadata after exact owned cleanup. It
// never touches the public final or result root; the reservation claim is merely
// released so future operations can choose names from current public reality.
func (lease *OperationRegistryLease) DeleteTerminal() error {
	if lease == nil || lease.registry == nil || lease.lock == nil || lease.deleted ||
		!lease.record.Valid() || lease.record.Lease() != checkpointmodel.OrdinaryLeaseHeld ||
		lease.record.Lifecycle() != checkpointmodel.OrdinaryOperationCompleted &&
			lease.record.Lifecycle() != checkpointmodel.OrdinaryOperationDiscarded &&
			lease.record.Lifecycle() != checkpointmodel.OrdinaryOperationCleanupPending {
		return transfer.ErrInvalidOutputBinding
	}
	if err := lease.CleanupEmptyFileState(); err != nil {
		return err
	}
	name := operationNamespaceName(lease.operation)
	directory, err := openExistingDirectory(lease.registry.operations, name)
	if err != nil {
		return repositoryError("open terminal ordinary operation", err)
	}
	if err := lease.registry.releaseTerminalReservation(lease.record); err != nil {
		return errors.Join(err, directory.Close())
	}
	if err := lease.registry.removeEmptyCandidateNamespace(lease.record.ActiveOperationKey()); err != nil {
		return errors.Join(err, directory.Close())
	}
	encoded, encodeErr := checkpointmodel.EncodeOrdinaryOperationRecord(lease.record)
	removeErr, closeFileErr := RemoveExact(directory, ordinaryOperationRecordFile, encoded)
	if encodeErr != nil || removeErr != nil || closeFileErr != nil {
		return repositoryError("remove terminal ordinary operation record", errors.Join(
			encodeErr, removeErr, closeFileErr, directory.Close(),
		))
	}
	if entries, namesErr := directory.Names(1); namesErr != nil || len(entries) != 0 {
		return repositoryError("authenticate empty terminal ordinary operation", errors.Join(
			namesErr, outputcap.ErrUnsafeNamespace, directory.Close(),
		))
	}
	removeDirectoryErr := lease.registry.operations.RemoveDirectory(name, directory)
	if removeDirectoryErr == nil {
		// Once namespace unlink succeeds this lease can no longer update a row.
		// Mark that cut before parent sync so callers never try to manufacture a
		// cleanup-pending record through an already-removed capability.
		lease.deleted = true
		lease.record = checkpointmodel.OrdinaryOperationRecord{}
		removeDirectoryErr = lease.registry.operations.Sync()
	}
	closeErr := directory.Close()
	if removeDirectoryErr != nil || closeErr != nil {
		return repositoryError("remove terminal ordinary operation", errors.Join(removeDirectoryErr, closeErr))
	}
	return nil
}

// RecoveryProof authenticates the immutable row against its exact reservation
// claim. Callers must already hold the operation lease before using the proof to
// reopen public authority.
func (registry *OperationRegistry) RecoveryProof(
	record checkpointmodel.OrdinaryOperationRecord,
) (ReservationRecoveryProof, error) {
	if !registry.valid() || !record.Valid() {
		return ReservationRecoveryProof{}, transfer.ErrInvalidOutputBinding
	}
	return registry.recoveryProof(record)
}

func (registry *OperationRegistry) releaseTerminalReservation(
	record checkpointmodel.OrdinaryOperationRecord,
) error {
	proof, err := registry.recoveryProof(record)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return registry.ReleaseReservation(proof.Claim(), record.OperationID())
}

func (registry *OperationRegistry) removeEmptyCandidateNamespace(key checkpointmodel.ActiveOperationKey) error {
	directory, err := openExistingDirectory(registry.candidates, activeKeyName(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return removeEmptyDirectory(registry.candidates, activeKeyName(key), directory)
}

func (lease *OperationRegistryLease) Close() error {
	if lease == nil {
		return nil
	}
	var releaseErr error
	if lease.registry != nil && lease.lock != nil && lease.record.Valid() &&
		lease.record.Lease() == checkpointmodel.OrdinaryLeaseHeld {
		released, err := checkpointmodel.NextOrdinaryOperationRecord(lease.record, checkpointmodel.NextOrdinaryOperationRecordSpec{
			Lifecycle: lease.record.Lifecycle(), Lease: checkpointmodel.OrdinaryLeaseReleased,
			ClosedReason: lease.record.ClosedReason(),
		})
		if err == nil {
			err = lease.registry.replaceOperation(lease.record, released)
		}
		releaseErr = err
		if err == nil {
			lease.record = released
		}
	}
	lockErr := closeLock(lease.lock)
	*lease = OperationRegistryLease{}
	return repositoryError("release exact ordinary operation lease", errors.Join(releaseErr, lockErr))
}
