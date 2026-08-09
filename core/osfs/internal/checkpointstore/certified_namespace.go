package checkpointstore

import (
	"encoding/hex"
	"errors"
	"io/fs"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const (
	ControlDirectory    = ".windshare-output"
	CheckpointDirectory = "checkpoints-v2"
	OwnershipDirectory  = "ownership"
	LeasesDirectory     = "leases"
	LookupDirectory     = "lookup"
	OperationsDirectory = "operations"

	OperationFile        = "operation"
	ReservationFile      = "reservation"
	CheckpointsDirectory = "checkpoints"
	AnchorsDirectory     = "anchors"
	StagesDirectory      = "stages"
	ManifestsDirectory   = "manifests"
	ReceiptsDirectory    = "receipts"

	operationLockSuffix = ".lock"
)

var checkpointRootEntries = map[string]outputcap.EntryKind{
	OwnershipDirectory:  outputcap.EntryDirectory,
	LeasesDirectory:     outputcap.EntryDirectory,
	LookupDirectory:     outputcap.EntryDirectory,
	OperationsDirectory: outputcap.EntryDirectory,
}

var operationEntries = map[string]outputcap.EntryKind{
	OperationFile:        outputcap.EntryRegularFile,
	ReservationFile:      outputcap.EntryRegularFile,
	CheckpointsDirectory: outputcap.EntryDirectory,
	ManifestsDirectory:   outputcap.EntryDirectory,
	ReceiptsDirectory:    outputcap.EntryDirectory,
}

var checkpointEntries = map[string]outputcap.EntryKind{
	RecordsDirectory: outputcap.EntryDirectory,
	AnchorsDirectory: outputcap.EntryDirectory,
	StagesDirectory:  outputcap.EntryDirectory,
}

// CertifiedConfig carries an already-proved native root. Paths are never used
// as a substitute for the opaque authority carried by Ownership.
type CertifiedConfig struct {
	Root      outputcap.Directory
	Ownership checkpointmodel.Ownership
}

type Namespace struct {
	checkpointRoot outputcap.Directory
	operations     outputcap.Directory
	lookup         outputcap.Directory
	leases         outputcap.Directory
	ownership      checkpointmodel.Ownership
}

func AdoptPinnedNamespace(
	checkpointRoot outputcap.Directory,
	operations outputcap.Directory,
	lookup outputcap.Directory,
	leases outputcap.Directory,
	ownership checkpointmodel.Ownership,
) (Namespace, error) {
	if checkpointRoot == nil || operations == nil || lookup == nil || leases == nil || !ownership.Valid() {
		return Namespace{}, transfer.ErrInvalidOutputBinding
	}
	return Namespace{
		checkpointRoot: checkpointRoot, operations: operations, lookup: lookup,
		leases: leases, ownership: ownership,
	}, nil
}

func CheckpointRootEntryKind(name string) (outputcap.EntryKind, bool) {
	kind, known := checkpointRootEntries[name]
	return kind, known
}

func CheckpointRootEntryLimit() int { return len(checkpointRootEntries) }

func OperationEntryKind(name string) (outputcap.EntryKind, bool) {
	kind, known := operationEntries[name]
	return kind, known
}

func OperationEntryLimit() int { return len(operationEntries) }

func Initialize(config CertifiedConfig) (result Namespace, resultErr error) {
	if err := validateCertifiedConfig(config); err != nil {
		return Namespace{}, err
	}
	control, err := openOrCreateDirectory(config.Root, ControlDirectory)
	if err != nil {
		return Namespace{}, repositoryError("initialize control directory", err)
	}
	checkpointRoot, err := openOrCreateDirectory(control, CheckpointDirectory)
	if err != nil {
		return Namespace{}, repositoryError("initialize checkpoint directory", errors.Join(err, control.Close()))
	}
	if err := control.Close(); err != nil {
		return Namespace{}, repositoryError("close initialized control directory", errors.Join(err, checkpointRoot.Close()))
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, checkpointRoot.Close())
		}
	}()
	if err := validateAllowedEntries(checkpointRoot, checkpointRootEntries); err != nil {
		return Namespace{}, repositoryError("validate checkpoint layout", err)
	}
	ownershipDirectory, err := openOrCreateDirectory(checkpointRoot, OwnershipDirectory)
	if err != nil {
		return Namespace{}, repositoryError("initialize checkpoint ownership directory", err)
	}
	if err := ensureOwnership(ownershipDirectory, config.Ownership, true); err != nil {
		return Namespace{}, repositoryError("certify checkpoint ownership", errors.Join(err, ownershipDirectory.Close()))
	}
	encoded, err := checkpointmodel.EncodeOwnership(config.Ownership)
	if err != nil {
		return Namespace{}, codedError(ErrorOwnershipMismatch, "encode checkpoint ownership", errors.Join(err, ownershipDirectory.Close()))
	}
	if err := reconcileExactCandidates(ownershipDirectory, OwnershipFile, encoded); err != nil {
		return Namespace{}, repositoryError("reconcile checkpoint ownership", errors.Join(err, ownershipDirectory.Close()))
	}
	if err := ownershipDirectory.Close(); err != nil {
		return Namespace{}, repositoryError("close checkpoint ownership directory", err)
	}
	leases, err := openOrCreateDirectory(checkpointRoot, LeasesDirectory)
	if err != nil {
		return Namespace{}, repositoryError("initialize checkpoint leases", err)
	}
	lookup, err := openOrCreateDirectory(checkpointRoot, LookupDirectory)
	if err != nil {
		return Namespace{}, repositoryError("initialize checkpoint lookup", errors.Join(err, leases.Close()))
	}
	operations, err := openOrCreateDirectory(checkpointRoot, OperationsDirectory)
	if err != nil {
		return Namespace{}, repositoryError("initialize checkpoint operations", errors.Join(err, lookup.Close(), leases.Close()))
	}
	return Namespace{
		checkpointRoot: checkpointRoot, operations: operations, lookup: lookup,
		leases: leases, ownership: config.Ownership,
	}, nil
}

// OpenNamespace is observation-only: an incomplete or foreign scaffold is not
// repaired during recovery because repair would turn uncertainty into authority.
func OpenNamespace(config CertifiedConfig) (result Namespace, resultErr error) {
	if err := validateCertifiedConfig(config); err != nil {
		return Namespace{}, err
	}
	control, err := openExistingDirectory(config.Root, ControlDirectory)
	if err != nil {
		return Namespace{}, repositoryError("open control directory", err)
	}
	checkpointRoot, err := openExistingDirectory(control, CheckpointDirectory)
	if err != nil {
		return Namespace{}, repositoryError("open checkpoint directory", errors.Join(err, control.Close()))
	}
	if err := control.Close(); err != nil {
		return Namespace{}, repositoryError("close control directory", errors.Join(err, checkpointRoot.Close()))
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, checkpointRoot.Close())
		}
	}()
	if err := validateAllowedEntries(checkpointRoot, checkpointRootEntries); err != nil {
		return Namespace{}, repositoryError("validate checkpoint layout", err)
	}
	ownershipDirectory, err := openExistingDirectory(checkpointRoot, OwnershipDirectory)
	if err != nil {
		return Namespace{}, repositoryError("open checkpoint ownership directory", err)
	}
	status, err := inspectOwnership(ownershipDirectory, config.Ownership)
	closeOwnershipErr := ownershipDirectory.Close()
	if err != nil || closeOwnershipErr != nil {
		return Namespace{}, repositoryError("verify checkpoint ownership", errors.Join(err, closeOwnershipErr))
	}
	if status != OwnershipMatched {
		return Namespace{}, codedError(ErrorOwnershipMismatch, "verify checkpoint ownership", checkpointmodel.ErrInvalidOwnership)
	}
	leases, err := openExistingDirectory(checkpointRoot, LeasesDirectory)
	if err != nil {
		return Namespace{}, repositoryError("open checkpoint leases", err)
	}
	lookup, err := openExistingDirectory(checkpointRoot, LookupDirectory)
	if err != nil {
		return Namespace{}, repositoryError("open checkpoint lookup", errors.Join(err, leases.Close()))
	}
	operations, err := openExistingDirectory(checkpointRoot, OperationsDirectory)
	if err != nil {
		return Namespace{}, repositoryError("open checkpoint operations", errors.Join(err, lookup.Close(), leases.Close()))
	}
	return Namespace{
		checkpointRoot: checkpointRoot, operations: operations, lookup: lookup,
		leases: leases, ownership: config.Ownership,
	}, nil
}

func (namespace *Namespace) Close() error {
	if namespace == nil {
		return nil
	}
	err := errors.Join(
		closeDirectory(namespace.operations), closeDirectory(namespace.lookup),
		closeDirectory(namespace.leases), closeDirectory(namespace.checkpointRoot),
	)
	*namespace = Namespace{}
	return repositoryError("close checkpoint namespace", err)
}

// OperationLease is the single mutation authority for one durable operation.
// The operations handle is duplicated so closing Namespace cannot shorten it.
type OperationLease struct {
	operation  receivecontract.OperationID
	binding    checkpointmodel.Binding
	operations outputcap.Directory
	lookup     outputcap.Directory
	lock       outputcap.Lock
}

func (lease OperationLease) Binding() checkpointmodel.Binding { return lease.binding }

func (namespace *Namespace) AcquireOperation(
	operation receivecontract.OperationID,
	intent transfer.ReceiveIntentDigest,
	materialization receivecontract.BindingDigest,
) (OperationLease, error) {
	if namespace == nil || namespace.leases == nil || namespace.operations == nil || namespace.lookup == nil ||
		operation.IsZero() || intent.IsZero() || materialization.IsZero() {
		return OperationLease{}, transfer.ErrInvalidOutputBinding
	}
	binding, err := checkpointmodel.NewBinding(namespace.ownership, operation, intent, materialization)
	if err != nil {
		return OperationLease{}, transfer.ErrInvalidOutputBinding
	}
	lock, created, err := namespace.leases.AcquireLock(operationLeaseName(operation), false)
	if err != nil {
		return OperationLease{}, repositoryError("acquire operation lease", errors.Join(err, closeLock(lock)))
	}
	if lock == nil {
		return OperationLease{}, codedError(ErrorUnsafeInstall, "acquire operation lease", outputcap.ErrUnsafeNamespace)
	}
	if created {
		if err := namespace.leases.Sync(); err != nil {
			return OperationLease{}, repositoryError("sync operation lease", errors.Join(err, lock.Close()))
		}
	}
	operations, err := namespace.operations.Duplicate()
	if err != nil {
		return OperationLease{}, repositoryError("retain operation namespace", errors.Join(err, lock.Close()))
	}
	if operations == nil {
		return OperationLease{}, codedError(
			ErrorUnsafeInstall, "retain operation namespace", errors.Join(outputcap.ErrUnsafeNamespace, lock.Close()),
		)
	}
	lookup, err := namespace.lookup.Duplicate()
	if err != nil {
		return OperationLease{}, repositoryError(
			"retain operation lookup", errors.Join(err, operations.Close(), lock.Close()),
		)
	}
	if lookup == nil {
		return OperationLease{}, codedError(
			ErrorUnsafeInstall, "retain operation lookup",
			errors.Join(outputcap.ErrUnsafeNamespace, operations.Close(), lock.Close()),
		)
	}
	return OperationLease{
		operation: operation, binding: binding, operations: operations, lookup: lookup, lock: lock,
	}, nil
}

func (lease *OperationLease) Close() error {
	if lease == nil {
		return nil
	}
	// Releasing the lock last prevents another owner from observing half-closed
	// capability handles from this owner.
	err := errors.Join(closeDirectory(lease.operations), closeDirectory(lease.lookup), closeLock(lease.lock))
	*lease = OperationLease{}
	return repositoryError("release operation lease", err)
}

func (lease *OperationLease) OpenOrCreateRepository() (Repository, error) {
	return lease.openRepository(true)
}

func (lease *OperationLease) OpenExistingRepository() (Repository, error) {
	return lease.openRepository(false)
}

func (lease *OperationLease) openRepository(create bool) (result Repository, resultErr error) {
	if lease == nil || lease.operations == nil || lease.lock == nil || lease.operation.IsZero() {
		return Repository{}, transfer.ErrInvalidOutputBinding
	}
	open := openExistingDirectory
	if create {
		open = openOrCreateDirectory
	}
	operation, err := open(lease.operations, operationNamespaceName(lease.operation))
	if err != nil {
		return Repository{}, repositoryError("open leased operation", err)
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, operation.Close())
		}
	}()
	if err := validateAllowedEntries(operation, operationEntries); err != nil {
		return Repository{}, repositoryError("validate operation layout", err)
	}
	checkpoints, err := open(operation, CheckpointsDirectory)
	if err != nil {
		return Repository{}, repositoryError("open operation checkpoints", err)
	}
	if err := validateAllowedEntries(checkpoints, checkpointEntries); err != nil {
		return Repository{}, repositoryError("validate checkpoint layout", errors.Join(err, checkpoints.Close()))
	}
	records, err := open(checkpoints, RecordsDirectory)
	if err != nil {
		return Repository{}, repositoryError("open checkpoint records", errors.Join(err, checkpoints.Close()))
	}
	anchors, err := open(checkpoints, AnchorsDirectory)
	if err != nil {
		return Repository{}, repositoryError("open checkpoint anchors", errors.Join(err, records.Close(), checkpoints.Close()))
	}
	stages, err := open(checkpoints, StagesDirectory)
	if err != nil {
		return Repository{}, repositoryError("open checkpoint stages", errors.Join(err, anchors.Close(), records.Close(), checkpoints.Close()))
	}
	manifests, err := open(operation, ManifestsDirectory)
	if err != nil {
		return Repository{}, repositoryError("open operation manifests", errors.Join(err, stages.Close(), anchors.Close(), records.Close(), checkpoints.Close()))
	}
	receipts, err := open(operation, ReceiptsDirectory)
	if err != nil {
		return Repository{}, repositoryError("open operation receipts", errors.Join(err, manifests.Close(), stages.Close(), anchors.Close(), records.Close(), checkpoints.Close()))
	}
	return Repository{
		operation: operation, checkpoints: checkpoints, records: records, anchors: anchors,
		stages: stages, manifests: manifests, receipts: receipts, binding: lease.binding,
	}, nil
}

func validateCertifiedConfig(config CertifiedConfig) error {
	if config.Root == nil || !config.Ownership.Valid() {
		return transfer.ErrInvalidOutputBinding
	}
	return nil
}

func operationNamespaceName(operation receivecontract.OperationID) string {
	return hex.EncodeToString(operation.Bytes())
}

func operationLeaseName(operation receivecontract.OperationID) string {
	return operationNamespaceName(operation) + operationLockSuffix
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
