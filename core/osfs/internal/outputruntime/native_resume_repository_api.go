package outputruntime

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"sync"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

var (
	ErrNativeResumeBusy             = errors.New("osfs: resume state is busy")
	ErrNativeResumeOwnershipUnknown = errors.New("osfs: resume state ownership is unknown")
)

type NativeResumeEvidenceState uint8

const (
	NativeResumeEvidenceAbsent NativeResumeEvidenceState = iota + 1
	NativeResumeEvidenceProven
	NativeResumeEvidenceUnknown
)

type NativeResumeCleanupState uint8

const (
	NativeResumeCleanupPending NativeResumeCleanupState = iota + 1
	NativeResumeCleanupComplete
	NativeResumeCleanupUnknown
)

type NativeResumeSnapshot struct {
	OperationRecord []byte
	LifecycleRecord []byte
}

type NativeResumeRecoveryEvidence struct {
	TargetOwnership NativeResumeEvidenceState
	Checkpoints     NativeResumeEvidenceState
	Cleanup         NativeResumeCleanupState
	TerminalReceipt []byte
	ExpiryReceipt   []byte
}

type NativeResumeDiscardEvidence struct {
	State   NativeResumeCleanupState
	Receipt []byte
}

// NativeResumeRepository reopens the certified root for every operation. A
// path selects a root to certify; it never becomes durable mutation authority.
type NativeResumeRepository struct {
	rootPath        string
	platformFactory PlatformFactory
}

func NewNativeResumeRepository(
	rootPath string,
	platformFactory PlatformFactory,
) (*NativeResumeRepository, error) {
	if rootPath == "" || !filepath.IsAbs(rootPath) || filepath.Clean(rootPath) != rootPath ||
		platformFactory == nil {
		return nil, transfer.ErrInvalidOutputBinding
	}
	return &NativeResumeRepository{rootPath: rootPath, platformFactory: platformFactory}, nil
}

func (repository *NativeResumeRepository) List(
	ctx context.Context,
) (result []NativeResumeSnapshot, resultErr error) {
	if repository == nil || ctx == nil || repository.platformFactory == nil {
		return nil, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	platform, authorityRef, err := repository.openPlatform(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, platform.Close()) }()

	namespace, _, namespaceErr := openNativeCheckpointNamespace(platform, authorityRef)
	if namespaceErr != nil {
		return repository.listUncertainNamespace(ctx, platform.Root(), namespaceErr)
	}
	defer func() { resultErr = errors.Join(resultErr, namespace.Close()) }()

	operations, err := openNativeResumeOperations(platform.Root())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, nativeResumeError(err)
	}
	defer func() { resultErr = errors.Join(resultErr, operations.Close()) }()
	names, err := nativeResumeOperationNames(operations)
	if err != nil {
		return nil, nativeResumeError(err)
	}
	result = make([]NativeResumeSnapshot, 0, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		candidate, operationBytes, err := readNativeResumeOperation(operations, name)
		if err != nil {
			return nil, nativeResumeError(err)
		}
		snapshot, err := listNativeResumeSnapshot(&namespace, operations, candidate, operationBytes)
		if err != nil {
			return nil, err
		}
		result = append(result, snapshot)
	}
	return result, nil
}

func (repository *NativeResumeRepository) Acquire(
	ctx context.Context,
	operation receivecontract.OperationID,
) (result *NativeResumeLease, resultErr error) {
	if repository == nil || ctx == nil || repository.platformFactory == nil || operation.IsZero() {
		return nil, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	platform, authorityRef, err := repository.openPlatform(ctx)
	if err != nil {
		return nil, err
	}
	resources := &NativeResumeLease{platform: platform, authorityRef: authorityRef}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, resources.Close())
		}
	}()

	namespace, _, err := openNativeCheckpointNamespace(platform, authorityRef)
	if err != nil {
		return nil, nativeResumeError(err)
	}
	resources.namespace = &namespace
	operations, err := openNativeResumeOperations(platform.Root())
	if err != nil {
		return nil, nativeResumeError(err)
	}
	name := hex.EncodeToString(operation.Bytes())
	candidate, _, candidateErr := readNativeResumeOperation(operations, name)
	operationDirectory, operationDirectoryErr := openNativeResumeDirectory(operations, name, true)
	resources.operationDirectory = operationDirectory
	operationsCloseErr := operations.Close()
	if candidateErr != nil || operationDirectoryErr != nil || operationsCloseErr != nil ||
		candidate.OperationID() != operation {
		return nil, nativeResumeError(errors.Join(
			candidateErr, operationDirectoryErr, operationsCloseErr, checkpointmodel.ErrRecordBinding,
		))
	}
	lease, err := namespace.AcquireOperation(
		operation, candidate.ReceiveIntentDigest(), candidate.BindingDigest(),
	)
	if err != nil {
		return nil, nativeResumeError(err)
	}
	resources.operationLease = &lease
	stored, err := lease.OpenExistingRepository()
	if err != nil {
		return nil, nativeResumeError(err)
	}
	resources.repository = &stored
	intent, intentErr := candidate.VerifyIntent(transfer.DecodeReceiveIntent)
	verificationErr := verifyStoredOperation(&stored, intent)
	if intentErr != nil || verificationErr != nil ||
		intent.MaterializationPlan().Kind() != receivecontract.PlanDirectTree ||
		candidate.OperationID() != operation || candidate.ReceiveIntentDigest() != intent.Digest() ||
		candidate.BindingDigest() != intent.BindingDigest() {
		return nil, nativeResumeError(errors.Join(
			intentErr, verificationErr, checkpointmodel.ErrRecordBinding,
		))
	}
	resources.operation = candidate
	return resources, nil
}

func (repository *NativeResumeRepository) openPlatform(
	ctx context.Context,
) (result outputcap.Platform, authorityRef receivecontract.AuthorityRef, resultErr error) {
	platform, err := repository.platformFactory(repository.rootPath, false)
	if err != nil {
		return nil, receivecontract.AuthorityRef{}, err
	}
	defer func() {
		if resultErr != nil && platform != nil {
			resultErr = errors.Join(resultErr, platform.Close())
		}
	}()
	if platform == nil || platform.Root() == nil ||
		filesystemOutputCertificationFromState(platform.Certification()) == "" {
		return nil, receivecontract.AuthorityRef{}, outputcap.ErrRecoverableOutputUnsupported
	}
	if err := ctx.Err(); err != nil {
		return nil, receivecontract.AuthorityRef{}, err
	}
	if err := validateOutputCreateAuthority(platform.Root()); err != nil {
		return nil, receivecontract.AuthorityRef{}, err
	}
	if err := platform.ProbeRecoverableFeatures(); err != nil {
		return nil, receivecontract.AuthorityRef{}, err
	}
	if platform.Durability() != transfer.DurabilityProcessRestart {
		return nil, receivecontract.AuthorityRef{}, outputcap.ErrRecoverableOutputUnsupported
	}
	rootBinding, err := platform.RootBinding()
	if err != nil || rootBinding.IsZero() {
		return nil, receivecontract.AuthorityRef{}, errors.Join(err, transfer.ErrInvalidOutputBinding)
	}
	authority, err := receivecontract.AuthorityRefFromBytes(rootBinding.Bytes())
	if err != nil {
		return nil, receivecontract.AuthorityRef{}, err
	}
	return platform, authority, nil
}

func (repository *NativeResumeRepository) listUncertainNamespace(
	ctx context.Context,
	root outputcap.Directory,
	namespaceErr error,
) ([]NativeResumeSnapshot, error) {
	if !nativeResumeUncertain(namespaceErr) && !errors.Is(namespaceErr, fs.ErrNotExist) {
		return nil, nativeResumeError(namespaceErr)
	}
	operations, err := openNativeResumeOperations(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, nativeResumeError(errors.Join(namespaceErr, err))
	}
	defer operations.Close()
	names, err := nativeResumeOperationNames(operations)
	if err != nil {
		return nil, nativeResumeError(errors.Join(namespaceErr, err))
	}
	result := make([]NativeResumeSnapshot, 0, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		_, encoded, readErr := readNativeResumeOperation(operations, name)
		if readErr != nil {
			return nil, errors.Join(ErrNativeResumeOwnershipUnknown, namespaceErr, readErr)
		}
		// A valid immutable operation identifies the item for the UI, but an
		// invalid lifecycle prevents this observation from becoming authority.
		result = append(result, NativeResumeSnapshot{
			OperationRecord: encoded,
			LifecycleRecord: []byte{0},
		})
	}
	return result, nil
}

type NativeResumeLease struct {
	mu     sync.Mutex
	closed bool

	platform           outputcap.Platform
	authorityRef       receivecontract.AuthorityRef
	namespace          *checkpointstore.Namespace
	operationLease     *checkpointstore.OperationLease
	repository         *checkpointstore.Repository
	operationDirectory outputcap.Directory
	operation          checkpointmodel.ReceiveOperation
}

func (lease *NativeResumeLease) Snapshot(ctx context.Context) (NativeResumeSnapshot, error) {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	operation, lifecycle, err := lease.snapshotLocked(ctx)
	if err != nil {
		return NativeResumeSnapshot{}, err
	}
	operationBytes, operationErr := checkpointmodel.EncodeReceiveOperation(operation)
	lifecycleBytes, lifecycleErr := checkpointmodel.EncodeReceiveLifecycleState(lifecycle)
	if operationErr != nil || lifecycleErr != nil {
		return NativeResumeSnapshot{}, errors.Join(operationErr, lifecycleErr)
	}
	return NativeResumeSnapshot{OperationRecord: operationBytes, LifecycleRecord: lifecycleBytes}, nil
}

func (lease *NativeResumeLease) ObserveRecovery(
	ctx context.Context,
) (NativeResumeRecoveryEvidence, error) {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	operation, lifecycle, err := lease.snapshotLocked(ctx)
	if err != nil {
		return NativeResumeRecoveryEvidence{}, err
	}
	store, records, directories, err := lease.reconciledRecordsLocked(ctx, lifecycle)
	if err != nil {
		if nativeResumeUncertain(err) {
			return unknownNativeResumeEvidence(lifecycle), nil
		}
		return NativeResumeRecoveryEvidence{}, nativeResumeError(err)
	}
	proven, err := lease.observeRecordsLocked(ctx, store, records)
	if err != nil {
		if nativeResumeUncertain(err) {
			return unknownNativeResumeEvidence(lifecycle), nil
		}
		return NativeResumeRecoveryEvidence{}, nativeResumeError(err)
	}
	if !proven {
		return unknownNativeResumeEvidence(lifecycle), nil
	}
	proven, err = lease.observeDirectoriesLocked(ctx, directories)
	if err != nil {
		if nativeResumeUncertain(err) {
			return unknownNativeResumeEvidence(lifecycle), nil
		}
		return NativeResumeRecoveryEvidence{}, nativeResumeError(err)
	}
	if !proven {
		return unknownNativeResumeEvidence(lifecycle), nil
	}
	terminal, err := lease.terminalReceiptLocked()
	if err != nil {
		if nativeResumeUncertain(err) {
			return unknownNativeResumeEvidence(lifecycle), nil
		}
		return NativeResumeRecoveryEvidence{}, nativeResumeError(err)
	}
	expiry, err := nativeResumeExpiryReceipt(operation, lifecycle)
	if err != nil {
		return NativeResumeRecoveryEvidence{}, err
	}
	cleanup := NativeResumeCleanupPending
	if lifecycle.CleanupState() == checkpointmodel.OwnedCleanupClean {
		cleanup = NativeResumeCleanupComplete
	}
	return NativeResumeRecoveryEvidence{
		TargetOwnership: NativeResumeEvidenceProven,
		Checkpoints:     NativeResumeEvidenceProven,
		Cleanup:         cleanup,
		TerminalReceipt: terminal,
		ExpiryReceipt:   expiry,
	}, nil
}

func (lease *NativeResumeLease) CleanupOwned(
	ctx context.Context,
) (NativeResumeDiscardEvidence, error) {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	operation, lifecycle, err := lease.snapshotLocked(ctx)
	if err != nil {
		return NativeResumeDiscardEvidence{}, err
	}
	authorization, proven, err := lease.authorizeCleanupLocked(ctx, operation, lifecycle)
	if err != nil || !proven {
		return nativeResumeUnprovenDiscard(err)
	}
	proven, err = retireNativeResumeObjects(ctx, authorization.store, authorization.objects)
	if err != nil || !proven {
		return nativeResumeUnprovenDiscard(err)
	}
	removedDirectories, err := lease.cleanupDirectoriesLocked(ctx, authorization.directories)
	if err != nil {
		return nativeResumeUnprovenDiscard(err)
	}
	receiptObjects := append(slices.Clone(authorization.objects), removedDirectories...)
	receipt, err := nativeResumeCleanupReceipt(
		authorization.operation,
		authorization.lifecycle,
		authorization.records,
		receiptObjects,
	)
	if err != nil {
		return NativeResumeDiscardEvidence{}, err
	}
	return NativeResumeDiscardEvidence{
		State:   NativeResumeCleanupComplete,
		Receipt: receipt.CanonicalBytes(),
	}, nil
}

type nativeResumeCleanupAuthorization struct {
	operation   checkpointmodel.ReceiveOperation
	lifecycle   checkpointmodel.ReceiveLifecycleState
	store       *checkpointstore.FileExecutionStore
	records     []checkpointmodel.Record
	directories []checkpointmodel.AdmittedDirectory
	objects     []checkpointmodel.ObjectID
}

func (lease *NativeResumeLease) authorizeCleanupLocked(
	ctx context.Context,
	operation checkpointmodel.ReceiveOperation,
	lifecycle checkpointmodel.ReceiveLifecycleState,
) (nativeResumeCleanupAuthorization, bool, error) {
	store, records, directories, err := lease.reconciledRecordsLocked(ctx, lifecycle)
	if err != nil {
		return nativeResumeCleanupAuthorization{}, false, err
	}
	proven, err := lease.observeRecordsLocked(ctx, store, records)
	if err != nil || !proven {
		return nativeResumeCleanupAuthorization{}, proven, err
	}
	proven, err = lease.observeDirectoriesLocked(ctx, directories)
	if err != nil || !proven {
		return nativeResumeCleanupAuthorization{}, proven, err
	}
	return nativeResumeCleanupAuthorization{
		operation: operation, lifecycle: lifecycle, store: store,
		records: records, directories: directories, objects: nativeResumeObjects(records),
	}, true, nil
}

func retireNativeResumeObjects(
	ctx context.Context,
	store *checkpointstore.FileExecutionStore,
	objects []checkpointmodel.ObjectID,
) (bool, error) {
	steps := [...]fileexecution.RetirementStep{
		fileexecution.RetirementRemoveStage,
		fileexecution.RetirementSyncStageNamespace,
		fileexecution.RetirementRemoveAnchor,
		fileexecution.RetirementSyncAnchorNamespace,
	}
	for _, object := range objects {
		for _, step := range steps {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			observation, err := store.ApplyRetirement(ctx, object, step)
			if err != nil {
				return false, err
			}
			if observation.ObjectID() != object {
				return false, nil
			}
			if step == fileexecution.RetirementSyncAnchorNamespace &&
				observation.Condition() != fileexecution.OwnedAbsent {
				return false, nil
			}
		}
	}
	return true, nil
}

func nativeResumeUnprovenDiscard(err error) (NativeResumeDiscardEvidence, error) {
	if err == nil || nativeResumeUncertain(err) {
		return NativeResumeDiscardEvidence{State: NativeResumeCleanupUnknown}, nil
	}
	return NativeResumeDiscardEvidence{}, nativeResumeError(err)
}

func (lease *NativeResumeLease) InstallReceipt(ctx context.Context, encoded []byte) error {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if err := lease.validateLocked(ctx); err != nil {
		return err
	}
	receipt, err := checkpointmodel.DecodeDirectTreeReceipt(encoded)
	if err != nil || receipt.OperationID() != lease.operation.OperationID() ||
		receipt.ReceiveIntentDigest() != lease.operation.ReceiveIntentDigest() ||
		receipt.ReservationDigest() != lease.operation.BindingDigest() {
		return errors.Join(checkpointmodel.ErrInvalidReceipt, err)
	}
	return nativeResumeError(lease.repository.InstallReceipt(receipt))
}

func (lease *NativeResumeLease) ReplaceLifecycle(
	ctx context.Context,
	previousBytes []byte,
	nextBytes []byte,
) error {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if err := lease.validateLocked(ctx); err != nil {
		return err
	}
	previous, previousErr := checkpointmodel.DecodeReceiveLifecycleState(previousBytes)
	next, nextErr := checkpointmodel.DecodeReceiveLifecycleState(nextBytes)
	current, currentErr := lease.repository.ReadLifecycleState()
	currentBytes, encodeErr := checkpointmodel.EncodeReceiveLifecycleState(current)
	if previousErr != nil || nextErr != nil || currentErr != nil || encodeErr != nil ||
		!bytes.Equal(currentBytes, previousBytes) ||
		previous.OperationID() != lease.operation.OperationID() ||
		next.OperationID() != lease.operation.OperationID() {
		return nativeResumeError(errors.Join(
			checkpointmodel.ErrInvalidLifecycleState,
			previousErr, nextErr, currentErr, encodeErr,
		))
	}
	return nativeResumeError(lease.repository.ReplaceLifecycleState(previous, next))
}

func (lease *NativeResumeLease) Close() error {
	if lease == nil {
		return nil
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.closed {
		return nil
	}
	lease.closed = true
	// The operation lease closes after every repository capability, preventing a
	// second process from observing a half-released mutation authority.
	err := errors.Join(
		closeNativeResumeRepository(lease.repository),
		closeNativeResumeDirectory(lease.operationDirectory),
		closeNativeResumeOperationLease(lease.operationLease),
		closeNativeResumeNamespace(lease.namespace),
		closeNativeResumePlatform(lease.platform),
	)
	lease.repository = nil
	lease.operationDirectory = nil
	lease.operationLease = nil
	lease.namespace = nil
	lease.platform = nil
	return nativeResumeError(err)
}

func (lease *NativeResumeLease) snapshotLocked(
	ctx context.Context,
) (checkpointmodel.ReceiveOperation, checkpointmodel.ReceiveLifecycleState, error) {
	if err := lease.validateLocked(ctx); err != nil {
		return checkpointmodel.ReceiveOperation{}, checkpointmodel.ReceiveLifecycleState{}, err
	}
	operation, err := lease.repository.ReadOperation()
	if err != nil {
		return checkpointmodel.ReceiveOperation{}, checkpointmodel.ReceiveLifecycleState{}, nativeResumeError(err)
	}
	intent, err := operation.VerifyIntent(transfer.DecodeReceiveIntent)
	if err != nil || verifyStoredOperation(lease.repository, intent) != nil ||
		operation.OperationID() != lease.operation.OperationID() ||
		operation.ReceiveIntentDigest() != lease.operation.ReceiveIntentDigest() ||
		operation.BindingDigest() != lease.operation.BindingDigest() {
		return checkpointmodel.ReceiveOperation{}, checkpointmodel.ReceiveLifecycleState{},
			nativeResumeError(errors.Join(err, checkpointmodel.ErrRecordBinding))
	}
	lifecycle, err := lease.repository.ReadLifecycleState()
	if err != nil {
		return checkpointmodel.ReceiveOperation{}, checkpointmodel.ReceiveLifecycleState{}, nativeResumeError(err)
	}
	return operation, lifecycle, nil
}

func (lease *NativeResumeLease) validateLocked(ctx context.Context) error {
	if lease == nil || lease.closed || lease.platform == nil || lease.namespace == nil ||
		lease.operationLease == nil || lease.repository == nil || lease.operationDirectory == nil ||
		lease.operation.OperationID().IsZero() || ctx == nil {
		return transfer.ErrInvalidOutputBinding
	}
	return ctx.Err()
}
