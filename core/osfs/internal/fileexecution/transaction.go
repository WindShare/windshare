package fileexecution

import (
	"context"
	"errors"
	"io"
	"math"
	"sync"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer"
)

type transactionState uint8

const (
	transactionOpen transactionState = iota + 1
	transactionClosed
)

type resumablePartialFileTransaction struct {
	engine          *Engine
	materialization transfer.MaterializationFile
	destination     FileDestination
	file            OwnedFile
	binding         transfer.MaterializedFileBinding

	mu       sync.Mutex
	record   checkpointmodel.Record
	pending  content.RangeSet
	state    transactionState
	resumed  bool
	warnings []MetadataWarning
}

type MetadataWarningKind uint8

const MetadataModifiedTimeWarning MetadataWarningKind = iota + 1

type MetadataWarning struct{ kind MetadataWarningKind }

func (warning MetadataWarning) Kind() MetadataWarningKind { return warning.kind }

func settlementMetadataWarnings(warnings []MetadataWarning) []transfer.FileMetadataWarning {
	result := make([]transfer.FileMetadataWarning, 0, len(warnings))
	for _, warning := range warnings {
		if warning.kind == MetadataModifiedTimeWarning {
			result = append(result, transfer.FileMetadataModifiedTime)
		}
	}
	return result
}

// livePartialFileStrategy keeps only the mode-specific persistence choice. Its
// cleanup ticket is durable, but its ranges intentionally are not.
type livePartialFileStrategy struct {
	materialization transfer.MaterializationFile
	destination     FileDestination
	file            *LiveOwnedFile
	binding         transfer.MaterializedFileBinding
	cleanup         func(*LiveOwnedFile) error

	mu         sync.Mutex
	written    content.RangeSet
	generation transfer.CheckpointGeneration
	closed     bool
	warnings   []MetadataWarning
}

type partialFileLifecycle interface {
	transfer.FileTransaction
	MetadataWarnings() []MetadataWarning
}

// PartialFileTransaction owns both restart-durable and live-only strategies.
// The wrapper is the only lifecycle surfaced to outputsession, which prevents a
// mode-specific writer from becoming a second range/settlement framework.
type PartialFileTransaction struct {
	strategy partialFileLifecycle
}

var _ transfer.FileTransaction = (*PartialFileTransaction)(nil)

func WrapPartialFileTransaction(strategy partialFileLifecycle) (*PartialFileTransaction, error) {
	if strategy == nil || strategy.Binding().Target().OutputSessionID().IsZero() {
		return nil, ErrInvalidConfiguration
	}
	return &PartialFileTransaction{strategy: strategy}, nil
}

func (transaction *PartialFileTransaction) Binding() transfer.MaterializedFileBinding {
	if transaction == nil || transaction.strategy == nil {
		return transfer.MaterializedFileBinding{}
	}
	return transaction.strategy.Binding()
}

func (transaction *PartialFileTransaction) MetadataWarnings() []MetadataWarning {
	if transaction == nil || transaction.strategy == nil {
		return nil
	}
	return transaction.strategy.MetadataWarnings()
}

func (transaction *PartialFileTransaction) WriteRange(ctx context.Context, offset uint64, data []byte) error {
	if transaction == nil || transaction.strategy == nil {
		return fileContractError(ErrTransactionClosed)
	}
	return transaction.strategy.WriteRange(ctx, offset, data)
}

func (transaction *PartialFileTransaction) Checkpoint(ctx context.Context) (transfer.VerifiedDurableRanges, error) {
	if transaction == nil || transaction.strategy == nil {
		return transfer.VerifiedDurableRanges{}, fileContractError(ErrTransactionClosed)
	}
	return transaction.strategy.Checkpoint(ctx)
}

func (transaction *PartialFileTransaction) Commit(ctx context.Context) (transfer.FileSettlement, error) {
	if transaction == nil || transaction.strategy == nil {
		return transfer.FileSettlement{}, fileContractError(ErrTransactionClosed)
	}
	return transaction.strategy.Commit(ctx)
}

func (transaction *PartialFileTransaction) Pause(ctx context.Context, reason transfer.FilePauseReason) (transfer.FileSettlement, error) {
	if transaction == nil || transaction.strategy == nil {
		return transfer.FileSettlement{}, fileContractError(ErrTransactionClosed)
	}
	return transaction.strategy.Pause(ctx, reason)
}

func (transaction *PartialFileTransaction) Retire(ctx context.Context, reason transfer.FileRetireReason) (transfer.FileSettlement, error) {
	if transaction == nil || transaction.strategy == nil {
		return transfer.FileSettlement{}, fileContractError(ErrTransactionClosed)
	}
	return transaction.strategy.Retire(ctx, reason)
}

var _ partialFileLifecycle = (*livePartialFileStrategy)(nil)

func NewLivePartialFileTransaction(
	file transfer.MaterializationFile,
	destination FileDestination,
	owned *LiveOwnedFile,
	cleanup func(*LiveOwnedFile) error,
) (*PartialFileTransaction, error) {
	if destination == nil || owned == nil || cleanup == nil || destination.Target() != file.Target() {
		return nil, ErrInvalidConfiguration
	}
	identity, err := outputIdentity(owned.ObjectID())
	if err != nil {
		return nil, err
	}
	binding, err := transfer.BindFileMaterializationTarget(file.Target(), identity)
	if err != nil {
		return nil, err
	}
	ranges, _ := content.NewRangeSet(nil)
	strategy := &livePartialFileStrategy{
		materialization: file, destination: destination, file: owned,
		binding: binding, cleanup: cleanup, written: ranges,
	}
	return WrapPartialFileTransaction(strategy)
}

func (transaction *livePartialFileStrategy) Binding() transfer.MaterializedFileBinding {
	if transaction == nil {
		return transfer.MaterializedFileBinding{}
	}
	return transaction.binding
}

func (transaction *livePartialFileStrategy) MetadataWarnings() []MetadataWarning {
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	return append([]MetadataWarning(nil), transaction.warnings...)
}

func (transaction *livePartialFileStrategy) WriteRange(ctx context.Context, offset uint64, data []byte) error {
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	if err := transaction.validateOpen(ctx); err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	if offset > transaction.binding.ExactSize() || uint64(len(data)) > transaction.binding.ExactSize()-offset ||
		offset > math.MaxInt64 || uint64(len(data)) > math.MaxInt64-offset {
		return fileContractError(ErrRangeOutOfBounds)
	}
	end := offset + uint64(len(data))
	if rangesIntersect(transaction.written, offset, end) {
		return fileContractError(ErrRangeOverlap)
	}
	added, err := content.NewRangeSet([]content.Range{{Offset: offset, End: end}})
	if err != nil {
		return fileContractError(err)
	}
	next, err := transfer.MergeRanges(transaction.written, added)
	if err != nil {
		return fileContractError(err)
	}
	written, writeErr := transaction.file.WriteAt(data, int64(offset))
	if written != len(data) {
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		return collaboratorError(ctx, writeErr)
	}
	transaction.written = next
	return nil
}

func (transaction *livePartialFileStrategy) Checkpoint(
	ctx context.Context,
) (transfer.VerifiedDurableRanges, error) {
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	return transaction.checkpointLocked(ctx)
}

func (transaction *livePartialFileStrategy) checkpointLocked(
	ctx context.Context,
) (transfer.VerifiedDurableRanges, error) {
	if err := transaction.validateOpen(ctx); err != nil {
		return transfer.VerifiedDurableRanges{}, err
	}
	if err := transaction.file.Sync(); err != nil {
		return transfer.VerifiedDurableRanges{}, collaboratorError(ctx, err)
	}
	if transaction.generation == ^transfer.CheckpointGeneration(0) {
		return transfer.VerifiedDurableRanges{}, fileContractError(ErrCheckpointBinding)
	}
	transaction.generation++
	// Live-only generations order in-process checkpoints, but empty ranges ensure
	// no later process can mistake that ordering evidence for restart have-state.
	empty, _ := content.NewRangeSet(nil)
	return transfer.VerifyDurableRanges(transaction.binding, transaction.generation, empty)
}

func (transaction *livePartialFileStrategy) Commit(ctx context.Context) (transfer.FileSettlement, error) {
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	if err := transaction.validateOpen(ctx); err != nil {
		return transfer.FileSettlement{}, err
	}
	if !transfer.RangesCoverFile(transaction.binding.ExactSize(), transaction.written) {
		return transfer.FileSettlement{}, fileContractError(ErrIncompleteFile)
	}
	// Modified time is best effort: exact size and authenticated byte coverage
	// decide success; metadata cannot invalidate already verified content.
	if err := transaction.file.SetModifiedTime(transaction.materialization.Descriptor().ModifiedTime()); err != nil {
		transaction.warnings = append(transaction.warnings, MetadataWarning{kind: MetadataModifiedTimeWarning})
	}
	if err := transaction.file.Sync(); err != nil {
		return transfer.FileSettlement{}, collaboratorError(ctx, err)
	}
	expectation, err := NewFinalExpectation(
		transaction.binding.ObjectIdentity(), transaction.binding.ExactSize(),
	)
	if err != nil {
		return transfer.FileSettlement{}, err
	}
	final, err := transaction.destination.PublishNoReplace(ctx, transaction.file, expectation)
	if err != nil || !final.valid() {
		return transaction.blockPublicationLocked()
	}
	if final.Condition() == FinalCollision {
		settlement, settlementErr := transfer.NewTransactionCollisionFileSettlement(transaction.binding)
		if settlementErr != nil {
			_ = transaction.close(false)
			return transfer.FileSettlement{}, settlementErr
		}
		if cleanupErr := transaction.close(true); cleanupErr != nil {
			return itemBlockedSettlement(transaction.binding, transfer.ItemBlockRetirementUncertain)
		}
		return settlement, nil
	}
	if final.Condition() != FinalOwnedExact {
		return transaction.blockPublicationLocked()
	}
	if err := transaction.destination.SyncFinalParent(ctx); err != nil {
		return transaction.blockPublicationLocked()
	}
	settlement, err := transfer.NewTransientPublishedFileSettlement(transaction.binding)
	if err == nil {
		settlement, err = settlement.WithMetadataWarnings(settlementMetadataWarnings(transaction.warnings))
	}
	_ = transaction.close(true)
	return settlement, err
}

func (transaction *livePartialFileStrategy) blockPublicationLocked() (transfer.FileSettlement, error) {
	settlement, err := itemBlockedSettlement(
		transaction.binding, transfer.ItemBlockPublicationAmbiguous,
	)
	// The journaled stage is the only recovery evidence available in live mode.
	// Closing handles is safe, but deleting that evidence would turn ambiguity
	// into an unprovable success or loss.
	_ = transaction.close(false)
	return settlement, err
}

func (transaction *livePartialFileStrategy) Pause(
	ctx context.Context,
	reason transfer.FilePauseReason,
) (transfer.FileSettlement, error) {
	if reason < transfer.FilePauseInterrupted || reason > transfer.FilePauseDependencyContract {
		return transfer.FileSettlement{}, transfer.ErrInvalidOutputSettlement
	}
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	if err := transaction.validateOpen(ctx); err != nil {
		return transfer.FileSettlement{}, err
	}
	checkpoint, err := transaction.checkpointLocked(ctx)
	if err != nil {
		return transfer.FileSettlement{}, err
	}
	settlement, err := transfer.NewVerifiedFileSettlement(transfer.FilePaused, checkpoint)
	if err != nil {
		_ = transaction.close(false)
		return transfer.FileSettlement{}, err
	}
	if cleanupErr := transaction.close(true); cleanupErr != nil {
		return itemBlockedSettlement(transaction.binding, transfer.ItemBlockRetirementUncertain)
	}
	return settlement, nil
}

func (transaction *livePartialFileStrategy) Retire(
	ctx context.Context,
	reason transfer.FileRetireReason,
) (transfer.FileSettlement, error) {
	if reason < transfer.FileRetireIsolatedPermanentSourceFailure || reason > transfer.FileRetireInvalidatedRevision {
		return transfer.FileSettlement{}, transfer.ErrInvalidOutputSettlement
	}
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	if err := transaction.validateOpen(ctx); err != nil {
		return transfer.FileSettlement{}, err
	}
	settlement, err := transfer.NewFailedFileSettlement(transaction.binding)
	if err != nil {
		_ = transaction.close(false)
		return transfer.FileSettlement{}, err
	}
	if cleanupErr := transaction.close(true); cleanupErr != nil {
		return itemBlockedSettlement(transaction.binding, transfer.ItemBlockRetirementUncertain)
	}
	return settlement, nil
}

func (transaction *livePartialFileStrategy) validateOpen(ctx context.Context) error {
	if transaction == nil || ctx == nil || transaction.closed || transaction.file == nil || transaction.destination == nil {
		return fileContractError(ErrTransactionClosed)
	}
	return ctx.Err()
}

func (transaction *livePartialFileStrategy) close(remove bool) error {
	if transaction.closed {
		return nil
	}
	transaction.closed = true
	var cleanupErr error
	if remove {
		cleanupErr = transaction.cleanup(transaction.file)
	}
	// Handle release cannot retract a stable settlement. Failed journal cleanup is
	// returned separately so callers can surface item-blocked; the durable ticket
	// remains available to later bounded cleanup.
	_ = errors.Join(transaction.file.Close(), transaction.destination.Close())
	transaction.file, transaction.destination = nil, nil
	return cleanupErr
}
