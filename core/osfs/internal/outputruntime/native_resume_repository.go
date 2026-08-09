package outputruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/directoryauthority"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const (
	nativeResumeCleanupEvidenceDomain = "windshare/native-resume-cleanup/v1"
	nativeResumeExpiryEvidenceDomain  = "windshare/native-resume-expiry/v1"
	nativeResumeDirectoryPrefix       = "directory-"
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
		snapshot, err := listNativeResumeSnapshot(ctx, &namespace, operations, candidate, operationBytes)
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
	store, records, directories, err := lease.reconciledRecordsLocked(ctx, lifecycle)
	if err != nil {
		if nativeResumeUncertain(err) {
			return NativeResumeDiscardEvidence{State: NativeResumeCleanupUnknown}, nil
		}
		return NativeResumeDiscardEvidence{}, nativeResumeError(err)
	}
	proven, err := lease.observeRecordsLocked(ctx, store, records)
	if err != nil || !proven {
		if err == nil || nativeResumeUncertain(err) {
			return NativeResumeDiscardEvidence{State: NativeResumeCleanupUnknown}, nil
		}
		return NativeResumeDiscardEvidence{}, nativeResumeError(err)
	}
	proven, err = lease.observeDirectoriesLocked(ctx, directories)
	if err != nil || !proven {
		if err == nil || nativeResumeUncertain(err) {
			return NativeResumeDiscardEvidence{State: NativeResumeCleanupUnknown}, nil
		}
		return NativeResumeDiscardEvidence{}, nativeResumeError(err)
	}
	objects := nativeResumeObjects(records)
	for _, object := range objects {
		for _, step := range []fileexecution.RetirementStep{
			fileexecution.RetirementRemoveStage,
			fileexecution.RetirementSyncStageNamespace,
			fileexecution.RetirementRemoveAnchor,
			fileexecution.RetirementSyncAnchorNamespace,
		} {
			if err := ctx.Err(); err != nil {
				return NativeResumeDiscardEvidence{}, err
			}
			observation, stepErr := store.ApplyRetirement(ctx, object, step)
			if stepErr != nil {
				if nativeResumeUncertain(stepErr) {
					return NativeResumeDiscardEvidence{State: NativeResumeCleanupUnknown}, nil
				}
				return NativeResumeDiscardEvidence{}, nativeResumeError(stepErr)
			}
			if observation.ObjectID() != object {
				return NativeResumeDiscardEvidence{State: NativeResumeCleanupUnknown}, nil
			}
			if step == fileexecution.RetirementSyncAnchorNamespace &&
				observation.Condition() != fileexecution.OwnedAbsent {
				return NativeResumeDiscardEvidence{State: NativeResumeCleanupUnknown}, nil
			}
		}
	}
	removedDirectories, err := lease.cleanupDirectoriesLocked(ctx, directories)
	if err != nil {
		if nativeResumeUncertain(err) {
			return NativeResumeDiscardEvidence{State: NativeResumeCleanupUnknown}, nil
		}
		return NativeResumeDiscardEvidence{}, nativeResumeError(err)
	}
	receiptObjects := append(slices.Clone(objects), removedDirectories...)
	receipt, err := nativeResumeCleanupReceipt(operation, lifecycle, records, receiptObjects)
	if err != nil {
		return NativeResumeDiscardEvidence{}, err
	}
	return NativeResumeDiscardEvidence{
		State:   NativeResumeCleanupComplete,
		Receipt: receipt.CanonicalBytes(),
	}, nil
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

func listNativeResumeSnapshot(
	ctx context.Context,
	namespace *checkpointstore.Namespace,
	operations outputcap.Directory,
	candidate checkpointmodel.ReceiveOperation,
	operationBytes []byte,
) (result NativeResumeSnapshot, resultErr error) {
	lease, err := namespace.AcquireOperation(
		candidate.OperationID(), candidate.ReceiveIntentDigest(), candidate.BindingDigest(),
	)
	if err != nil {
		return NativeResumeSnapshot{}, nativeResumeError(err)
	}
	defer func() { resultErr = errors.Join(resultErr, nativeResumeError(lease.Close())) }()
	repository, err := lease.OpenExistingRepository()
	if err != nil {
		if nativeResumeUncertain(err) {
			return uncertainNativeResumeSnapshot(operationBytes), nil
		}
		return NativeResumeSnapshot{}, nativeResumeError(err)
	}
	defer func() { resultErr = errors.Join(resultErr, nativeResumeError(repository.Close())) }()
	stored, operationErr := repository.ReadOperation()
	intent, intentErr := candidate.VerifyIntent(transfer.DecodeReceiveIntent)
	verificationErr := verifyStoredOperation(&repository, intent)
	if operationErr != nil || intentErr != nil || verificationErr != nil ||
		!sameNativeResumeOperation(stored, candidate) {
		joined := errors.Join(operationErr, intentErr, verificationErr, checkpointmodel.ErrRecordBinding)
		if nativeResumeUncertain(joined) {
			return uncertainNativeResumeSnapshot(operationBytes), nil
		}
		return NativeResumeSnapshot{}, nativeResumeError(joined)
	}
	lifecycle, lifecycleErr := repository.ReadLifecycleState()
	if lifecycleErr != nil {
		if !nativeResumeUncertain(lifecycleErr) {
			return NativeResumeSnapshot{}, nativeResumeError(lifecycleErr)
		}
		lifecycleBytes, readErr := readNativeResumeLifecycle(operations, candidate.OperationID())
		if readErr != nil {
			lifecycleBytes = []byte{0}
		}
		return NativeResumeSnapshot{
			OperationRecord: slices.Clone(operationBytes),
			LifecycleRecord: lifecycleBytes,
		}, nil
	}
	encodedLifecycle, err := checkpointmodel.EncodeReceiveLifecycleState(lifecycle)
	if err != nil {
		return NativeResumeSnapshot{}, err
	}
	return NativeResumeSnapshot{
		OperationRecord: slices.Clone(operationBytes),
		LifecycleRecord: encodedLifecycle,
	}, nil
}

func uncertainNativeResumeSnapshot(operation []byte) NativeResumeSnapshot {
	return NativeResumeSnapshot{OperationRecord: slices.Clone(operation), LifecycleRecord: []byte{0}}
}

func sameNativeResumeOperation(left, right checkpointmodel.ReceiveOperation) bool {
	leftBytes, leftErr := checkpointmodel.EncodeReceiveOperation(left)
	rightBytes, rightErr := checkpointmodel.EncodeReceiveOperation(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftBytes, rightBytes)
}

func nativeResumeOperationNames(operations outputcap.Directory) ([]string, error) {
	if operations == nil {
		return nil, transfer.ErrInvalidOutputBinding
	}
	names, err := operations.Names(checkpointstore.EntryLimit)
	if err != nil {
		return nil, err
	}
	if len(names) >= checkpointstore.EntryLimit {
		return nil, outputcap.ErrUnsafeNamespace
	}
	slices.Sort(names)
	for _, name := range names {
		if _, err := parseNativeResumeOperationName(name); err != nil {
			return nil, errors.Join(ErrNativeResumeOwnershipUnknown, err)
		}
	}
	return names, nil
}

func readNativeResumeOperation(
	operations outputcap.Directory,
	name string,
) (checkpointmodel.ReceiveOperation, []byte, error) {
	operationID, err := parseNativeResumeOperationName(name)
	if err != nil {
		return checkpointmodel.ReceiveOperation{}, nil, err
	}
	directory, err := openNativeResumeDirectory(operations, name, true)
	if err != nil {
		return checkpointmodel.ReceiveOperation{}, nil, err
	}
	encoded, readErr := checkpointstore.ReadFile(directory, checkpointstore.OperationFile)
	closeErr := directory.Close()
	if readErr != nil || closeErr != nil {
		return checkpointmodel.ReceiveOperation{}, nil, errors.Join(readErr, closeErr)
	}
	record, err := checkpointmodel.DecodeReceiveOperation(encoded)
	if err != nil || record.OperationID() != operationID {
		return checkpointmodel.ReceiveOperation{}, nil, errors.Join(err, checkpointmodel.ErrRecordBinding)
	}
	canonical, err := checkpointmodel.EncodeReceiveOperation(record)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return checkpointmodel.ReceiveOperation{}, nil, errors.Join(err, checkpointmodel.ErrRecordNonCanonical)
	}
	return record, canonical, nil
}

func readNativeResumeLifecycle(
	operations outputcap.Directory,
	operation receivecontract.OperationID,
) ([]byte, error) {
	directory, err := openNativeResumeDirectory(operations, hex.EncodeToString(operation.Bytes()), true)
	if err != nil {
		return nil, err
	}
	receipts, err := openNativeResumeDirectory(directory, checkpointstore.ReceiptsDirectory, true)
	if err != nil {
		return nil, errors.Join(err, directory.Close())
	}
	encoded, readErr := checkpointstore.ReadFile(receipts, "lifecycle")
	return encoded, errors.Join(readErr, receipts.Close(), directory.Close())
}

func parseNativeResumeOperationName(name string) (receivecontract.OperationID, error) {
	if len(name) != receivecontract.StableIdentityBytes*2 || name != strings.ToLower(name) {
		return receivecontract.OperationID{}, checkpointmodel.ErrRecordBinding
	}
	raw, err := hex.DecodeString(name)
	if err != nil {
		return receivecontract.OperationID{}, checkpointmodel.ErrRecordBinding
	}
	return receivecontract.OperationIDFromBytes(raw)
}

func openNativeResumeOperations(root outputcap.Directory) (outputcap.Directory, error) {
	control, err := openNativeResumeDirectory(root, checkpointstore.ControlDirectory, true)
	if err != nil {
		return nil, err
	}
	checkpoints, err := openNativeResumeDirectory(control, checkpointstore.CheckpointDirectory, true)
	if err != nil {
		return nil, errors.Join(err, control.Close())
	}
	operations, err := openNativeResumeDirectory(checkpoints, checkpointstore.OperationsDirectory, true)
	closeErr := errors.Join(checkpoints.Close(), control.Close())
	if err != nil || closeErr != nil {
		return nil, errors.Join(err, closeErr, closeNativeResumeDirectory(operations))
	}
	return operations, nil
}

func openNativeResumeDirectory(
	parent outputcap.Directory,
	name string,
	private bool,
) (outputcap.Directory, error) {
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
	directory, err := parent.OpenDirectory(name, private)
	if err != nil || directory == nil {
		return nil, errors.Join(err, outputcap.ErrUnsafeNamespace, closeNativeResumeDirectory(directory))
	}
	return directory, nil
}

func (lease *NativeResumeLease) reconciledRecordsLocked(
	ctx context.Context,
	lifecycle checkpointmodel.ReceiveLifecycleState,
) (*checkpointstore.FileExecutionStore, []checkpointmodel.Record, []checkpointmodel.AdmittedDirectory, error) {
	if err := lease.validateLocked(ctx); err != nil {
		return nil, nil, nil, err
	}
	store, err := checkpointstore.NewFileExecutionStore(lease.repository)
	if err != nil {
		return nil, nil, nil, err
	}
	snapshot, err := lease.repository.Reconcile(func(checkpointmodel.Record) (bool, error) {
		// NewFileExecutionStore already resolved every recoverable candidate under
		// this operation lease. A second candidate is therefore a changed cut.
		return false, nil
	})
	if err != nil || len(snapshot.Attention()) != 0 {
		return nil, nil, nil, errors.Join(err, checkpointmodel.ErrRecordRecovery)
	}
	records := snapshot.Records()
	indexed := make(map[checkpointmodel.RecordID]checkpointmodel.Record, len(records))
	objects := make(map[checkpointmodel.ObjectID]struct{}, len(records))
	for _, record := range records {
		if record.OperationID() != lease.operation.OperationID() ||
			record.ReceiveIntentDigest() != lease.operation.ReceiveIntentDigest() ||
			record.MaterializationBindingDigest() != lease.operation.BindingDigest() ||
			record.AuthorityRef() != lease.authorityRef ||
			record.MaterializerKind() != checkpointmodel.MaterializerNativeTree {
			return nil, nil, nil, checkpointmodel.ErrRecordBinding
		}
		if _, duplicate := objects[record.OwnedObjectID()]; duplicate {
			return nil, nil, nil, checkpointmodel.ErrRecordObjectConflict
		} else {
			objects[record.OwnedObjectID()] = struct{}{}
		}
		indexed[record.RecordID()] = record
	}
	for _, reference := range lifecycle.CheckpointReferences() {
		record, found := indexed[reference.RecordID()]
		if !found || record.CheckpointGeneration() != reference.CheckpointGeneration() ||
			(record.CommitState() != checkpointmodel.CommitVerified &&
				record.CommitState() != checkpointmodel.CommitPublished) {
			return nil, nil, nil, checkpointmodel.ErrRecordBinding
		}
	}
	artifacts, err := scanNativeResumeRecoveryArtifacts(lease.operationDirectory)
	if err != nil {
		return nil, nil, nil, err
	}
	for object := range artifacts {
		if _, owned := objects[object]; !owned {
			return nil, nil, nil, outputcap.ErrUnsafeNamespace
		}
	}
	directories, err := lease.admittedDirectoriesLocked()
	if err != nil {
		return nil, nil, nil, err
	}
	return store, records, directories, nil
}

func scanNativeResumeRecoveryArtifacts(
	operation outputcap.Directory,
) (map[checkpointmodel.ObjectID]struct{}, error) {
	checkpoints, err := openNativeResumeDirectory(operation, checkpointstore.CheckpointsDirectory, true)
	if err != nil {
		return nil, err
	}
	defer checkpoints.Close()
	result := make(map[checkpointmodel.ObjectID]struct{})
	for _, target := range []struct {
		name string
		kind checkpointstore.RecoveryArtifactKind
	}{
		{name: checkpointstore.StagesDirectory, kind: checkpointstore.RecoveryStage},
		{name: checkpointstore.AnchorsDirectory, kind: checkpointstore.RecoveryAnchor},
	} {
		directory, err := openNativeResumeDirectory(checkpoints, target.name, true)
		if err != nil {
			return nil, err
		}
		shards, err := directory.Names(checkpointstore.ShardLimit)
		if err != nil || len(shards) >= checkpointstore.ShardLimit {
			return nil, errors.Join(err, outputcap.ErrUnsafeNamespace, directory.Close())
		}
		for _, shardName := range shards {
			shard, err := checkpointstore.OpenShard(directory, shardName, false)
			if err != nil {
				return nil, errors.Join(err, directory.Close())
			}
			names, err := shard.Names(checkpointstore.EntryLimit)
			if err != nil || len(names) >= checkpointstore.EntryLimit {
				return nil, errors.Join(err, outputcap.ErrUnsafeNamespace, shard.Close(), directory.Close())
			}
			for _, name := range names {
				object, parseErr := checkpointstore.ParseRecoveryArtifactLocation(shardName, name, target.kind)
				kind, exact, classifyErr := shard.ClassifyExactEntry(name)
				if parseErr != nil || classifyErr != nil || !exact || kind != outputcap.EntryRegularFile {
					return nil, errors.Join(
						parseErr, classifyErr, outputcap.ErrUnsafeNamespace, shard.Close(), directory.Close(),
					)
				}
				result[object] = struct{}{}
			}
			if err := shard.Close(); err != nil {
				return nil, errors.Join(err, directory.Close())
			}
		}
		if err := directory.Close(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (lease *NativeResumeLease) admittedDirectoriesLocked() ([]checkpointmodel.AdmittedDirectory, error) {
	manifests, err := openNativeResumeDirectory(
		lease.operationDirectory, checkpointstore.ManifestsDirectory, true,
	)
	if err != nil {
		return nil, err
	}
	defer manifests.Close()
	names, err := manifests.Names(checkpointstore.EntryLimit)
	if err != nil || len(names) >= checkpointstore.EntryLimit {
		return nil, errors.Join(err, outputcap.ErrUnsafeNamespace)
	}
	slices.Sort(names)
	result := make([]checkpointmodel.AdmittedDirectory, 0, len(names))
	admissions := make(map[checkpointmodel.AggregateDigest]struct{}, len(names))
	paths := make(map[string]struct{}, len(names))
	objects := make(map[transfer.OwnedObjectID]struct{}, len(names))
	for _, name := range names {
		if !strings.HasPrefix(name, nativeResumeDirectoryPrefix) {
			return nil, outputcap.ErrUnsafeNamespace
		}
		encodedID := strings.TrimPrefix(name, nativeResumeDirectoryPrefix)
		if len(encodedID) != catalog.IdentityBytes*2 || encodedID != strings.ToLower(encodedID) {
			return nil, outputcap.ErrUnsafeNamespace
		}
		rawID, decodeErr := hex.DecodeString(encodedID)
		directoryID, identityErr := catalog.DirectoryIDFromBytes(rawID)
		record, readErr := lease.repository.ReadAdmittedDirectory(directoryID)
		if decodeErr != nil || identityErr != nil || readErr != nil ||
			record.OperationID() != lease.operation.OperationID() ||
			record.ReceiveIntentDigest() != lease.operation.ReceiveIntentDigest() {
			return nil, errors.Join(
				decodeErr, identityErr, readErr, checkpointmodel.ErrInvalidAdmittedDirectory,
			)
		}
		if _, duplicate := admissions[record.AdmissionDigest()]; duplicate {
			return nil, checkpointmodel.ErrInvalidAdmittedDirectory
		}
		if _, duplicate := paths[record.CanonicalPath()]; duplicate {
			return nil, checkpointmodel.ErrInvalidAdmittedDirectory
		}
		if _, duplicate := objects[record.OwnedObjectID()]; duplicate {
			return nil, checkpointmodel.ErrInvalidAdmittedDirectory
		}
		admissions[record.AdmissionDigest()] = struct{}{}
		paths[record.CanonicalPath()] = struct{}{}
		objects[record.OwnedObjectID()] = struct{}{}
		result = append(result, record)
	}
	for _, record := range result {
		if record.CanonicalPath() == "" {
			if !record.ParentAdmissionDigest().IsZero() {
				return nil, checkpointmodel.ErrInvalidAdmittedDirectory
			}
			continue
		}
		if _, found := admissions[record.ParentAdmissionDigest()]; !found {
			return nil, checkpointmodel.ErrInvalidAdmittedDirectory
		}
	}
	slices.SortFunc(result, func(left, right checkpointmodel.AdmittedDirectory) int {
		leftDepth := nativeResumePathDepth(left.CanonicalPath())
		rightDepth := nativeResumePathDepth(right.CanonicalPath())
		if leftDepth != rightDepth {
			return leftDepth - rightDepth
		}
		return strings.Compare(left.CanonicalPath(), right.CanonicalPath())
	})
	return result, nil
}

func nativeResumePathDepth(path string) int {
	if path == "" {
		return 0
	}
	return strings.Count(path, "/") + 1
}

func (lease *NativeResumeLease) observeDirectoriesLocked(
	ctx context.Context,
	directories []checkpointmodel.AdmittedDirectory,
) (bool, error) {
	for _, record := range directories {
		pin, err := pinNativeResumeDirectory(ctx, lease.platform, record.CanonicalPath())
		if err != nil {
			return false, err
		}
		if pin.absent {
			stable, revalidateErr := pin.Revalidate()
			closeErr := pin.Close()
			if revalidateErr != nil || closeErr != nil {
				return false, errors.Join(revalidateErr, closeErr)
			}
			if !stable {
				return false, nil
			}
			continue
		}
		owned, identityErr := directoryauthority.PersistentOwnedDirectoryID(pin.directory)
		stable, revalidateErr := pin.Revalidate()
		closeErr := pin.Close()
		if identityErr != nil || revalidateErr != nil || closeErr != nil {
			return false, errors.Join(identityErr, revalidateErr, closeErr)
		}
		if !stable || owned != record.OwnedObjectID() {
			return false, nil
		}
	}
	return true, nil
}

func (lease *NativeResumeLease) cleanupDirectoriesLocked(
	ctx context.Context,
	directories []checkpointmodel.AdmittedDirectory,
) ([]checkpointmodel.ObjectID, error) {
	ordered := slices.Clone(directories)
	slices.Reverse(ordered)
	removed := make([]checkpointmodel.ObjectID, 0, len(ordered))
	for _, record := range ordered {
		if record.CanonicalPath() == "" {
			continue
		}
		pin, err := pinNativeResumeDirectory(ctx, lease.platform, record.CanonicalPath())
		if err != nil {
			return nil, err
		}
		object, objectErr := checkpointmodel.ObjectIDFromBytes(record.OwnedObjectID().Bytes())
		if objectErr != nil {
			return nil, errors.Join(objectErr, pin.Close())
		}
		if pin.absent {
			stable, revalidateErr := pin.Revalidate()
			closeErr := pin.Close()
			if revalidateErr != nil || closeErr != nil || !stable {
				return nil, errors.Join(
					revalidateErr, closeErr, ErrNativeResumeOwnershipUnknown,
				)
			}
			removed = append(removed, object)
			continue
		}
		owned, identityErr := directoryauthority.PersistentOwnedDirectoryID(pin.directory)
		if identityErr != nil || owned != record.OwnedObjectID() {
			return nil, errors.Join(identityErr, ErrNativeResumeOwnershipUnknown, pin.Close())
		}
		names, namesErr := pin.directory.Names(1)
		stable, revalidateErr := pin.Revalidate()
		if namesErr != nil || revalidateErr != nil || !stable {
			return nil, errors.Join(
				namesErr, revalidateErr, ErrNativeResumeOwnershipUnknown, pin.Close(),
			)
		}
		if len(names) != 0 {
			// A retained finalized entry or a caller-created entry makes the
			// directory ineligible for removal, not ownership-uncertain. Discard
			// cleans only WindShare-owned unfinished objects and preserves the
			// non-empty directory without inspecting or mutating its children.
			if closeErr := pin.Close(); closeErr != nil {
				return nil, errors.Join(closeErr, ErrNativeResumeOwnershipUnknown)
			}
			continue
		}
		// Public child handles intentionally deny delete sharing on Windows. Once
		// ownership and emptiness are proven, retain the entry witness and lineage
		// but release the child capability before unlinking through that witness.
		targetCloseErr := pin.directory.Close()
		pin.directory = nil
		if targetCloseErr != nil {
			return nil, errors.Join(targetCloseErr, ErrNativeResumeOwnershipUnknown, pin.Close())
		}
		removeErr := pin.parent.RemoveEntry(pin.leaf, pin.entry)
		kind, exact, classifyErr := pin.parent.ClassifyExactEntry(pin.leaf)
		lineageStable, lineageErr := pin.RevalidateLineage()
		closeErr := pin.Close()
		if removeErr != nil || classifyErr != nil || lineageErr != nil || !lineageStable ||
			!exact || kind != outputcap.EntryAbsent || closeErr != nil {
			return nil, errors.Join(
				removeErr, classifyErr, lineageErr, closeErr, ErrNativeResumeOwnershipUnknown,
			)
		}
		removed = append(removed, object)
	}
	slices.SortFunc(removed, func(left, right checkpointmodel.ObjectID) int {
		return bytes.Compare(left.Bytes(), right.Bytes())
	})
	return removed, nil
}

type nativeResumeDirectoryPin struct {
	guard outputcap.PublicOperationGuard

	directory outputcap.Directory
	parent    outputcap.Directory
	leaf      string
	entry     outputcap.CurrentEntryReference
	lineage   []nativeResumeLineagePin
	opened    []outputcap.Directory

	absent       bool
	absentParent outputcap.Directory
	absentName   string
	closed       bool
}

func pinNativeResumeDirectory(
	ctx context.Context,
	platform outputcap.Platform,
	path string,
) (result *nativeResumeDirectoryPin, resultErr error) {
	if ctx == nil || platform == nil || platform.Root() == nil {
		return nil, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if path != "" {
		canonical, err := catalog.CanonicalPath(path)
		if err != nil || canonical != path {
			return nil, errors.Join(err, checkpointmodel.ErrInvalidAdmittedDirectory)
		}
		if key, err := platform.CanonicalLocatorKey(path); err != nil || key == "" {
			return nil, errors.Join(err, outputcap.ErrUnsafeNamespace)
		}
	}
	guard, err := platform.AcquirePublicOperationGuard()
	if err != nil {
		return nil, nativeResumeError(err)
	}
	pin := &nativeResumeDirectoryPin{guard: guard}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, pin.Close())
		}
	}()
	if guard == nil || guard.Root() == nil {
		return nil, outputcap.ErrUnsafeNamespace
	}
	sameRoot, err := platform.Root().SameDirectory(guard.Root())
	if err != nil || !sameRoot {
		return nil, errors.Join(err, outputcap.ErrUnsafeNamespace)
	}
	if path == "" {
		pin.directory = guard.Root()
		return pin, nil
	}
	components := strings.Split(path, "/")
	current := guard.Root()
	for index, component := range components {
		kind, exact, classifyErr := current.ClassifyExactEntry(component)
		if classifyErr != nil {
			return nil, classifyErr
		}
		if kind == outputcap.EntryAbsent && exact {
			pin.absent = true
			pin.absentParent = current
			pin.absentName = component
			return pin, nil
		}
		if !exact || kind != outputcap.EntryDirectory {
			return nil, ErrNativeResumeOwnershipUnknown
		}
		entry, err := current.OpenEntry(component)
		if err != nil || entry == nil || entry.Kind() != outputcap.EntryDirectory {
			return nil, errors.Join(err, outputcap.ErrUnsafeNamespace, closeNativeResumeEntry(entry))
		}
		child, err := current.OpenPinnedDirectory(entry, false)
		if err != nil || child == nil {
			return nil, errors.Join(
				err, outputcap.ErrUnsafeNamespace,
				closeNativeResumeEntry(entry), closeNativeResumeDirectory(child),
			)
		}
		if index == len(components)-1 {
			pin.parent = current
			pin.leaf = component
			pin.entry = entry
			pin.directory = child
			pin.opened = append(pin.opened, child)
			return pin, nil
		}
		pin.lineage = append(pin.lineage, nativeResumeLineagePin{
			parent: current, name: component, entry: entry,
		})
		pin.opened = append(pin.opened, child)
		current = child
	}
	return nil, outputcap.ErrUnsafeNamespace
}

func (pin *nativeResumeDirectoryPin) Revalidate() (bool, error) {
	if pin == nil || pin.closed || pin.guard == nil {
		return false, transfer.ErrInvalidOutputBinding
	}
	if pin.absent {
		kind, exact, err := pin.absentParent.ClassifyExactEntry(pin.absentName)
		if err != nil || !exact || kind != outputcap.EntryAbsent {
			return false, err
		}
	} else if pin.entry != nil {
		unchanged, err := pin.parent.EntryMatches(pin.leaf, pin.entry)
		if err != nil || !unchanged {
			return false, err
		}
	}
	return pin.RevalidateLineage()
}

func (pin *nativeResumeDirectoryPin) RevalidateLineage() (bool, error) {
	if pin == nil || pin.closed || pin.guard == nil {
		return false, transfer.ErrInvalidOutputBinding
	}
	for index := len(pin.lineage) - 1; index >= 0; index-- {
		lineage := pin.lineage[index]
		unchanged, err := lineage.parent.EntryMatches(lineage.name, lineage.entry)
		if err != nil || !unchanged {
			return false, err
		}
	}
	return true, nil
}

func (pin *nativeResumeDirectoryPin) Close() error {
	if pin == nil || pin.closed {
		return nil
	}
	pin.closed = true
	var result error
	result = errors.Join(result, closeNativeResumeEntry(pin.entry))
	for index := len(pin.lineage) - 1; index >= 0; index-- {
		result = errors.Join(result, closeNativeResumeEntry(pin.lineage[index].entry))
	}
	for index := len(pin.opened) - 1; index >= 0; index-- {
		result = errors.Join(result, closeNativeResumeDirectory(pin.opened[index]))
	}
	result = errors.Join(result, closeNativeResumeGuard(pin.guard))
	return result
}

func (lease *NativeResumeLease) observeRecordsLocked(
	ctx context.Context,
	store *checkpointstore.FileExecutionStore,
	records []checkpointmodel.Record,
) (bool, error) {
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if record.Phase() == checkpointmodel.PhaseQuarantined ||
			record.CommitState() == checkpointmodel.CommitQuarantined {
			return false, nil
		}
		if record.Phase() == checkpointmodel.PhasePublished ||
			record.Phase() == checkpointmodel.PhaseRetired &&
				record.RetirementReason() == checkpointmodel.RetirementPublished {
			proven, err := observeNativeResumePublication(ctx, lease.platform, store, record)
			if err != nil || !proven {
				return proven, err
			}
			continue
		}
		file, observation, err := store.OpenOwnedFile(
			ctx, record.OwnedObjectID(), record.ExactSize(), false,
		)
		closeErr := closeNativeResumeOwnedFile(file)
		if err != nil || closeErr != nil || observation.ObjectID() != record.OwnedObjectID() {
			return false, errors.Join(err, closeErr, outputcap.ErrUnsafeNamespace)
		}
		switch record.Phase() {
		case checkpointmodel.PhaseRetired:
			switch observation.Condition() {
			case fileexecution.OwnedAbsent, fileexecution.OwnedReady,
				fileexecution.OwnedAnchorMissing, fileexecution.OwnedStageMissing:
				continue
			default:
				return false, nil
			}
		case checkpointmodel.PhaseReserved, checkpointmodel.PhaseActive,
			checkpointmodel.PhasePaused, checkpointmodel.PhasePublishing:
			if observation.Condition() != fileexecution.OwnedReady {
				return false, nil
			}
		default:
			return false, nil
		}
	}
	return true, nil
}

type nativeResumeLineagePin struct {
	parent outputcap.Directory
	name   string
	entry  outputcap.CurrentEntryReference
}

func observeNativeResumePublication(
	ctx context.Context,
	platform outputcap.Platform,
	store *checkpointstore.FileExecutionStore,
	record checkpointmodel.Record,
) (proven bool, resultErr error) {
	if ctx == nil || platform == nil || platform.Root() == nil || store == nil || !record.Valid() {
		return false, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	canonical, err := catalog.CanonicalPath(record.CanonicalPath())
	if err != nil || canonical != record.CanonicalPath() || canonical == "" {
		return false, errors.Join(err, checkpointmodel.ErrRecordBinding)
	}
	if key, err := platform.CanonicalLocatorKey(canonical); err != nil || key == "" {
		return false, errors.Join(err, outputcap.ErrUnsafeNamespace)
	}
	guard, err := platform.AcquirePublicOperationGuard()
	if err != nil {
		return false, nativeResumeError(err)
	}
	if guard == nil || guard.Root() == nil {
		return false, errors.Join(outputcap.ErrUnsafeNamespace, closeNativeResumeGuard(guard))
	}
	defer func() { resultErr = errors.Join(resultErr, closeNativeResumeGuard(guard)) }()
	sameRoot, err := platform.Root().SameDirectory(guard.Root())
	if err != nil || !sameRoot {
		return false, errors.Join(err, outputcap.ErrUnsafeNamespace)
	}

	components := strings.Split(canonical, "/")
	current := guard.Root()
	lineage := make([]nativeResumeLineagePin, 0, len(components)-1)
	opened := make([]outputcap.Directory, 0, len(components)-1)
	defer func() {
		for index := len(lineage) - 1; index >= 0; index-- {
			resultErr = errors.Join(resultErr, closeNativeResumeEntry(lineage[index].entry))
		}
		for index := len(opened) - 1; index >= 0; index-- {
			resultErr = errors.Join(resultErr, closeNativeResumeDirectory(opened[index]))
		}
	}()
	for _, component := range components[:len(components)-1] {
		kind, exact, classifyErr := current.ClassifyExactEntry(component)
		if classifyErr != nil {
			return false, classifyErr
		}
		if !exact || kind != outputcap.EntryDirectory {
			return false, nil
		}
		entry, err := current.OpenEntry(component)
		if err != nil || entry == nil || entry.Kind() != outputcap.EntryDirectory {
			return false, errors.Join(err, outputcap.ErrUnsafeNamespace, closeNativeResumeEntry(entry))
		}
		child, err := current.OpenPinnedDirectory(entry, false)
		if err != nil || child == nil {
			return false, errors.Join(err, outputcap.ErrUnsafeNamespace, closeNativeResumeEntry(entry), closeNativeResumeDirectory(child))
		}
		lineage = append(lineage, nativeResumeLineagePin{parent: current, name: component, entry: entry})
		opened = append(opened, child)
		current = child
	}
	leaf := components[len(components)-1]
	kind, exact, err := current.ClassifyExactEntry(leaf)
	if err != nil {
		return false, err
	}
	if !exact || kind != outputcap.EntryRegularFile {
		return false, nil
	}
	entry, err := current.OpenEntry(leaf)
	if err != nil || entry == nil || entry.Kind() != outputcap.EntryRegularFile {
		return false, errors.Join(err, outputcap.ErrUnsafeNamespace, closeNativeResumeEntry(entry))
	}
	defer func() { resultErr = errors.Join(resultErr, closeNativeResumeEntry(entry)) }()
	final, err := current.OpenFile(leaf, false, false)
	if err != nil || final == nil {
		return false, errors.Join(err, outputcap.ErrUnsafeNamespace, closeNativeResumeFile(final))
	}
	defer func() { resultErr = errors.Join(resultErr, closeNativeResumeFile(final)) }()
	unchanged, err := current.EntryMatches(leaf, entry)
	if err != nil || !unchanged {
		return false, errors.Join(err, outputcap.ErrUnsafeNamespace)
	}
	matches, err := store.FinalMatchesOwned(ctx, record.OwnedObjectID(), record.ExactSize(), final)
	if err != nil || !matches {
		return false, err
	}
	unchanged, err = current.EntryMatches(leaf, entry)
	if err != nil || !unchanged {
		return false, errors.Join(err, outputcap.ErrUnsafeNamespace)
	}
	for index := len(lineage) - 1; index >= 0; index-- {
		pin := lineage[index]
		unchanged, err := pin.parent.EntryMatches(pin.name, pin.entry)
		if err != nil || !unchanged {
			return false, errors.Join(err, outputcap.ErrUnsafeNamespace)
		}
	}
	return true, nil
}

func (lease *NativeResumeLease) terminalReceiptLocked() ([]byte, error) {
	receipts, err := openNativeResumeDirectory(
		lease.operationDirectory, checkpointstore.ReceiptsDirectory, true,
	)
	if err != nil {
		return nil, err
	}
	defer receipts.Close()
	names, err := receipts.Names(checkpointstore.EntryLimit)
	if err != nil || len(names) >= checkpointstore.EntryLimit {
		return nil, errors.Join(err, outputcap.ErrUnsafeNamespace)
	}
	slices.Sort(names)
	var terminal checkpointmodel.DirectTreeReceipt
	for _, name := range names {
		if name == "lifecycle" {
			continue
		}
		if len(name) != sha256.Size*2 || name != strings.ToLower(name) {
			return nil, outputcap.ErrUnsafeNamespace
		}
		rawDigest, decodeNameErr := hex.DecodeString(name)
		digest, digestErr := checkpointmodel.AggregateDigestFromBytes(rawDigest)
		encoded, readErr := checkpointstore.ReadFile(receipts, name)
		decoded, decodeErr := checkpointmodel.DecodeDirectTreeReceipt(encoded)
		stored, storedErr := lease.repository.ReadReceipt(digest)
		if decodeNameErr != nil || digestErr != nil || readErr != nil || decodeErr != nil || storedErr != nil ||
			decoded.Digest() != digest || !bytes.Equal(decoded.CanonicalBytes(), stored.CanonicalBytes()) {
			return nil, errors.Join(
				decodeNameErr, digestErr, readErr, decodeErr, storedErr, checkpointmodel.ErrInvalidReceipt,
			)
		}
		if decoded.Kind() != checkpointmodel.ReceiptTreeCompletion &&
			decoded.Kind() != checkpointmodel.ReceiptPartialDirectory {
			continue
		}
		if terminal.Valid() {
			return nil, ErrNativeResumeOwnershipUnknown
		}
		terminal = decoded
	}
	return terminal.CanonicalBytes(), nil
}

func nativeResumeExpiryReceipt(
	operation checkpointmodel.ReceiveOperation,
	lifecycle checkpointmodel.ReceiveLifecycleState,
) ([]byte, error) {
	if lifecycle.Phase() != checkpointmodel.LifecycleResumableReceive {
		return nil, nil
	}
	if lifecycle.StateGeneration() == ^uint64(0) {
		return nil, checkpointmodel.ErrInvalidLifecycleState
	}
	evidence, err := nativeResumeEvidenceDigest(
		nativeResumeExpiryEvidenceDomain,
		operation,
		lifecycle,
		lifecycle.CheckpointReferences(),
		nil,
	)
	if err != nil {
		return nil, err
	}
	receipt, err := checkpointmodel.NewDirectTreeReceipt(checkpointmodel.DirectTreeReceiptSpec{
		Kind:        checkpointmodel.ReceiptExpiry,
		OperationID: operation.OperationID(), ReceiveIntent: operation.ReceiveIntentDigest(),
		ReservationDigest: operation.BindingDigest(), CheckpointRefs: lifecycle.CheckpointReferences(),
		EvidenceDigest: evidence, SuccessCount: lifecycle.SuccessCount(), FailureCount: lifecycle.FailureCount(),
		CleanupGeneration: lifecycle.StateGeneration() + 1,
	})
	if err != nil {
		return nil, err
	}
	return receipt.CanonicalBytes(), nil
}

func nativeResumeCleanupReceipt(
	operation checkpointmodel.ReceiveOperation,
	lifecycle checkpointmodel.ReceiveLifecycleState,
	records []checkpointmodel.Record,
	objects []checkpointmodel.ObjectID,
) (checkpointmodel.DirectTreeReceipt, error) {
	if lifecycle.StateGeneration() == ^uint64(0) {
		return checkpointmodel.DirectTreeReceipt{}, checkpointmodel.ErrInvalidLifecycleState
	}
	references := make([]checkpointmodel.FileCheckpointReference, 0, len(records))
	for _, record := range records {
		reference, err := checkpointmodel.NewFileCheckpointReference(record)
		if err != nil {
			return checkpointmodel.DirectTreeReceipt{}, err
		}
		references = append(references, reference)
	}
	evidence, err := nativeResumeEvidenceDigest(
		nativeResumeCleanupEvidenceDomain, operation, lifecycle, references, objects,
	)
	if err != nil {
		return checkpointmodel.DirectTreeReceipt{}, err
	}
	return checkpointmodel.NewDirectTreeReceipt(checkpointmodel.DirectTreeReceiptSpec{
		Kind:        checkpointmodel.ReceiptCleanup,
		OperationID: operation.OperationID(), ReceiveIntent: operation.ReceiveIntentDigest(),
		ReservationDigest: operation.BindingDigest(), EvidenceDigest: evidence,
		CleanupGeneration:  lifecycle.StateGeneration() + 1,
		RemovedObjectCount: uint64(len(objects)), RemovedRecordCount: 0,
	})
}

func nativeResumeEvidenceDigest(
	domain string,
	operation checkpointmodel.ReceiveOperation,
	lifecycle checkpointmodel.ReceiveLifecycleState,
	references []checkpointmodel.FileCheckpointReference,
	objects []checkpointmodel.ObjectID,
) (checkpointmodel.AggregateDigest, error) {
	if domain == "" || !operation.Valid() || !lifecycle.Valid() {
		return checkpointmodel.AggregateDigest{}, transfer.ErrInvalidOutputBinding
	}
	hash := sha256.New()
	writeNativeResumeEvidenceField(hash, []byte(domain))
	writeNativeResumeEvidenceField(hash, operation.OperationID().Bytes())
	writeNativeResumeEvidenceField(hash, operation.ReceiveIntentDigest().Bytes())
	writeNativeResumeEvidenceField(hash, operation.BindingDigest().Bytes())
	var generation [8]byte
	binary.BigEndian.PutUint64(generation[:], lifecycle.StateGeneration())
	_, _ = hash.Write(generation[:])
	canonicalReferences := slices.Clone(references)
	slices.SortFunc(canonicalReferences, func(left, right checkpointmodel.FileCheckpointReference) int {
		return bytes.Compare(left.RecordID().Bytes(), right.RecordID().Bytes())
	})
	for _, reference := range canonicalReferences {
		writeNativeResumeEvidenceField(hash, reference.RecordID().Bytes())
		binary.BigEndian.PutUint64(generation[:], reference.CheckpointGeneration())
		_, _ = hash.Write(generation[:])
	}
	canonicalObjects := slices.Clone(objects)
	slices.SortFunc(canonicalObjects, func(left, right checkpointmodel.ObjectID) int {
		return bytes.Compare(left.Bytes(), right.Bytes())
	})
	for _, object := range canonicalObjects {
		writeNativeResumeEvidenceField(hash, object.Bytes())
	}
	return checkpointmodel.AggregateDigestFromBytes(hash.Sum(nil))
}

func writeNativeResumeEvidenceField(hash interface{ Write([]byte) (int, error) }, value []byte) {
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(value)))
	_, _ = hash.Write(size[:])
	_, _ = hash.Write(value)
}

func nativeResumeObjects(records []checkpointmodel.Record) []checkpointmodel.ObjectID {
	seen := make(map[checkpointmodel.ObjectID]struct{}, len(records))
	result := make([]checkpointmodel.ObjectID, 0, len(records))
	for _, record := range records {
		if _, exists := seen[record.OwnedObjectID()]; exists {
			continue
		}
		seen[record.OwnedObjectID()] = struct{}{}
		result = append(result, record.OwnedObjectID())
	}
	slices.SortFunc(result, func(left, right checkpointmodel.ObjectID) int {
		return bytes.Compare(left.Bytes(), right.Bytes())
	})
	return result
}

func unknownNativeResumeEvidence(
	lifecycle checkpointmodel.ReceiveLifecycleState,
) NativeResumeRecoveryEvidence {
	cleanup := NativeResumeCleanupUnknown
	if lifecycle.Valid() && lifecycle.CleanupState() == checkpointmodel.OwnedCleanupClean {
		cleanup = NativeResumeCleanupComplete
	}
	return NativeResumeRecoveryEvidence{
		TargetOwnership: NativeResumeEvidenceUnknown,
		Checkpoints:     NativeResumeEvidenceUnknown,
		Cleanup:         cleanup,
	}
}

func nativeResumeError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrNativeResumeBusy) {
		return err
	}
	var checkpointErr *checkpointstore.Error
	if errors.Is(err, outputcap.ErrNamespaceLockBusy) ||
		errors.As(err, &checkpointErr) && checkpointErr != nil &&
			checkpointErr.Code() == checkpointstore.ErrorBusy {
		return errors.Join(ErrNativeResumeBusy, err)
	}
	return err
}

func nativeResumeUncertain(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrNativeResumeBusy) {
		return false
	}
	if errors.Is(err, ErrNativeResumeOwnershipUnknown) || errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, outputcap.ErrUnsafeNamespace) || errors.Is(err, outputcap.ErrNamespaceCollision) ||
		errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) ||
		errors.Is(err, directoryauthority.ErrRetainedAuthorityChanged) ||
		errors.Is(err, checkpointmodel.ErrInvalidOwnership) ||
		errors.Is(err, checkpointmodel.ErrOwnershipChecksum) ||
		errors.Is(err, checkpointmodel.ErrOwnershipNonCanonical) ||
		errors.Is(err, checkpointmodel.ErrInvalidAdmittedDirectory) ||
		errors.Is(err, checkpointmodel.ErrInvalidRecord) ||
		errors.Is(err, checkpointmodel.ErrRecordChecksum) ||
		errors.Is(err, checkpointmodel.ErrRecordNonCanonical) ||
		errors.Is(err, checkpointmodel.ErrRecordBinding) ||
		errors.Is(err, checkpointmodel.ErrRecordGeneration) ||
		errors.Is(err, checkpointmodel.ErrRecordObjectConflict) ||
		errors.Is(err, checkpointmodel.ErrRecordRecovery) ||
		errors.Is(err, checkpointmodel.ErrRecordCrashBoundary) ||
		errors.Is(err, checkpointmodel.ErrInvalidReceipt) ||
		errors.Is(err, checkpointmodel.ErrInvalidLifecycleState) {
		return true
	}
	var checkpointErr *checkpointstore.Error
	if !errors.As(err, &checkpointErr) || checkpointErr == nil {
		return false
	}
	switch checkpointErr.Code() {
	case checkpointstore.ErrorCorruptRecord,
		checkpointstore.ErrorUnsafeInstall,
		checkpointstore.ErrorOwnershipMismatch:
		return true
	default:
		return false
	}
}

func closeNativeResumeRepository(repository *checkpointstore.Repository) error {
	if repository == nil {
		return nil
	}
	return repository.Close()
}

func closeNativeResumeOperationLease(lease *checkpointstore.OperationLease) error {
	if lease == nil {
		return nil
	}
	return lease.Close()
}

func closeNativeResumeNamespace(namespace *checkpointstore.Namespace) error {
	if namespace == nil {
		return nil
	}
	return namespace.Close()
}

func closeNativeResumePlatform(platform outputcap.Platform) error {
	if platform == nil {
		return nil
	}
	return platform.Close()
}

func closeNativeResumeDirectory(directory outputcap.Directory) error {
	if directory == nil {
		return nil
	}
	return directory.Close()
}

func closeNativeResumeFile(file outputcap.File) error {
	if file == nil {
		return nil
	}
	return file.Close()
}

func closeNativeResumeOwnedFile(file fileexecution.OwnedFile) error {
	if file == nil {
		return nil
	}
	return file.Close()
}

func closeNativeResumeEntry(entry outputcap.CurrentEntryReference) error {
	if entry == nil {
		return nil
	}
	return entry.Close()
}

func closeNativeResumeGuard(guard outputcap.PublicOperationGuard) error {
	if guard == nil {
		return nil
	}
	return guard.Close()
}
