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

type Transaction struct {
	engine          *Engine
	materialization transfer.MaterializationFile
	destination     FileDestination
	file            OwnedFile
	binding         transfer.MaterializedFileBinding

	mu      sync.Mutex
	record  checkpointmodel.Record
	pending content.RangeSet
	state   transactionState
}

var _ transfer.FileTransaction = (*Transaction)(nil)

func (transaction *Transaction) Binding() transfer.MaterializedFileBinding {
	if transaction == nil {
		return transfer.MaterializedFileBinding{}
	}
	return transaction.binding
}

func (transaction *Transaction) WriteRange(ctx context.Context, offset uint64, data []byte) error {
	if transaction == nil || transaction.engine == nil {
		return fileContractError(ErrInvalidConfiguration)
	}
	sequence := transaction.engine.nextSequence()
	transaction.mu.Lock()
	previous := transaction.record.Phase()
	err := transaction.writeRangeLocked(ctx, offset, data)
	next := transaction.record.Phase()
	transaction.mu.Unlock()
	transaction.engine.emit(transaction.engine.traceEvent(
		sequence, TraceWriteRange, traceOutcomeForError(err), previous, next, traceFault(err),
	))
	return err
}

func (transaction *Transaction) writeRangeLocked(ctx context.Context, offset uint64, data []byte) error {
	if err := transaction.validateOpen(ctx); err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	if offset > transaction.binding.ExactSize() ||
		uint64(len(data)) > transaction.binding.ExactSize()-offset ||
		offset > math.MaxInt64 || uint64(len(data)) > math.MaxInt64-offset {
		return fileContractError(ErrRangeOutOfBounds)
	}
	end := offset + uint64(len(data))
	verified, err := contentRanges(transaction.record)
	if err != nil {
		return fileContractError(err)
	}
	if rangesIntersect(verified, offset, end) || rangesIntersect(transaction.pending, offset, end) {
		return fileContractError(ErrRangeOverlap)
	}
	writtenRange, err := content.NewRangeSet([]content.Range{{Offset: offset, End: end}})
	if err != nil {
		return fileContractError(err)
	}
	pending, err := transfer.MergeRanges(transaction.pending, writtenRange)
	if err != nil {
		return fileContractError(err)
	}
	written, writeErr := transaction.file.WriteAt(data, int64(offset))
	if written != len(data) {
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		if written > 0 {
			return publicationAmbiguousError(writeErr)
		}
		return collaboratorError(ctx, writeErr)
	}
	transaction.pending = pending
	return nil
}

func (transaction *Transaction) Checkpoint(ctx context.Context) (transfer.VerifiedDurableRanges, error) {
	if transaction == nil || transaction.engine == nil {
		return transfer.VerifiedDurableRanges{}, fileContractError(ErrInvalidConfiguration)
	}
	sequence := transaction.engine.nextSequence()
	transaction.mu.Lock()
	previous := transaction.record.Phase()
	checkpoint, err := transaction.checkpointLocked(ctx)
	next := transaction.record.Phase()
	transaction.mu.Unlock()
	transaction.engine.emit(transaction.engine.traceEvent(
		sequence, TraceCheckpoint, traceOutcomeForError(err), previous, next, traceFault(err),
	))
	return checkpoint, err
}

func (transaction *Transaction) checkpointLocked(ctx context.Context) (transfer.VerifiedDurableRanges, error) {
	if err := transaction.validateOpen(ctx); err != nil {
		return transfer.VerifiedDurableRanges{}, err
	}
	if transaction.pending.IsEmpty() {
		return durableRanges(transaction.binding, transaction.record)
	}
	if err := transaction.file.Sync(); err != nil {
		return transfer.VerifiedDurableRanges{}, collaboratorError(ctx, err)
	}
	verified, err := contentRanges(transaction.record)
	if err != nil {
		return transfer.VerifiedDurableRanges{}, fileContractError(err)
	}
	merged, err := transfer.MergeRanges(verified, transaction.pending)
	if err != nil {
		return transfer.VerifiedDurableRanges{}, fileContractError(err)
	}
	candidate, err := checkpointmodel.AdvanceGeneration(
		transaction.record, checkpointRanges(merged), checkpointmodel.PhaseActive,
		checkpointmodel.CommitCandidate,
	)
	if err != nil {
		return transfer.VerifiedDurableRanges{}, fileContractError(err)
	}
	next, err := checkpointmodel.Promote(candidate, checkpointmodel.PhaseActive, checkpointmodel.CommitVerified)
	if err != nil {
		return transfer.VerifiedDurableRanges{}, fileContractError(err)
	}
	previous := transaction.record
	if _, err := transaction.engine.storeRecord(ctx, &previous, candidate); err != nil {
		return transfer.VerifiedDurableRanges{}, err
	}
	// The candidate is the crash image between byte durability and verified
	// checkpoint promotion; persisting only the promoted image would erase
	// the recovery cut that distinguishes a completed sync from an unknown one.
	transaction.record = candidate
	if _, err := transaction.engine.storeRecord(ctx, &candidate, next); err != nil {
		return transfer.VerifiedDurableRanges{}, err
	}
	transaction.record = next
	transaction.pending, _ = content.NewRangeSet(nil)
	return durableRanges(transaction.binding, next)
}

func (transaction *Transaction) Commit(ctx context.Context) (transfer.FileSettlement, error) {
	if transaction == nil || transaction.engine == nil {
		return transfer.FileSettlement{}, fileContractError(ErrInvalidConfiguration)
	}
	sequence := transaction.engine.nextSequence()
	transaction.mu.Lock()
	previous := transaction.record.Phase()
	settlement, err := transaction.commitLocked(ctx)
	next := transaction.record.Phase()
	transaction.mu.Unlock()
	transaction.engine.emit(transaction.engine.traceEvent(
		sequence, TracePublish, traceOutcomeForSettlement(settlement, err), previous, next, traceFault(err),
	))
	return settlement, err
}

func (transaction *Transaction) commitLocked(ctx context.Context) (transfer.FileSettlement, error) {
	checkpoint, err := transaction.checkpointLocked(ctx)
	if err != nil {
		return transfer.FileSettlement{}, err
	}
	if !transfer.RangesCoverFile(transaction.binding.ExactSize(), checkpoint.Ranges()) {
		return transfer.FileSettlement{}, fileContractError(ErrIncompleteFile)
	}
	modified := transaction.materialization.Descriptor.ModifiedTime()
	if err := transaction.file.SetModifiedTime(modified); err != nil {
		return transfer.FileSettlement{}, collaboratorError(ctx, err)
	}
	matches, err := transaction.file.MetadataMatches(transaction.binding.ExactSize(), modified)
	if err != nil || !matches {
		return transfer.FileSettlement{}, collaboratorError(ctx, errors.Join(err, ErrPortContract))
	}
	if err := transaction.file.Sync(); err != nil {
		return transfer.FileSettlement{}, collaboratorError(ctx, err)
	}
	publishing, err := publishingRecord(transaction.record)
	if err != nil {
		return transfer.FileSettlement{}, fileContractError(err)
	}
	previous := transaction.record
	if _, err := transaction.engine.storeRecord(ctx, &previous, publishing); err != nil {
		return transfer.FileSettlement{}, err
	}
	transaction.record = publishing
	return transaction.publishFromDurableState(ctx, true)
}

func (transaction *Transaction) recoverPublication(ctx context.Context, retry bool) (transfer.FileStart, error) {
	transaction.mu.Lock()
	settlement, err := transaction.publishFromDurableState(ctx, retry)
	transaction.mu.Unlock()
	return settlementStart(settlement, err)
}

func (transaction *Transaction) publishFromDurableState(
	ctx context.Context,
	retry bool,
) (transfer.FileSettlement, error) {
	if transaction.record.Phase() != checkpointmodel.PhasePublishing ||
		transaction.record.CommitState() != checkpointmodel.CommitVerified {
		return transfer.FileSettlement{}, checkpointBindingError(ErrCheckpointBinding)
	}
	expectation, err := expectationFor(transaction.materialization, transaction.record)
	if err != nil {
		return transfer.FileSettlement{}, fileContractError(err)
	}
	final, err := transaction.destination.ObserveFinal(ctx, expectation)
	if err != nil || !final.valid() {
		return transfer.FileSettlement{}, publicationAmbiguousError(err)
	}
	if final.Condition() == FinalAbsent && retry {
		final, err = transaction.destination.PublishNoReplace(ctx, transaction.file, expectation)
		if err != nil || !final.valid() {
			return transfer.FileSettlement{}, publicationAmbiguousError(err)
		}
	}
	if final.Condition() == FinalCollision {
		paused, transitionErr := pauseRecord(transaction.record)
		if transitionErr == nil {
			_, transitionErr = transaction.engine.storeRecord(ctx, &transaction.record, paused)
		}
		if transitionErr != nil {
			return transfer.FileSettlement{}, transitionErr
		}
		transaction.record = paused
		settlement, settlementErr := verifiedSettlement(transfer.FilePublishBlocked, transaction.binding, paused)
		return settlement, joinFailures(ctx, settlementErr, transaction.closeResourcesLocked(ctx))
	}
	if final.Condition() != FinalOwnedExact {
		return transfer.FileSettlement{}, publicationAmbiguousError(ErrTargetOwnershipUnknown)
	}
	if err := transaction.destination.SyncFinalParent(ctx); err != nil {
		return transfer.FileSettlement{}, publicationAmbiguousError(err)
	}
	published, err := publishedRecord(transaction.record)
	if err != nil {
		return transfer.FileSettlement{}, fileContractError(err)
	}
	previous := transaction.record
	if _, err := transaction.engine.storeRecord(ctx, &previous, published); err != nil {
		return transfer.FileSettlement{}, err
	}
	transaction.record = published
	if err := transaction.retainPublishedWitness(ctx); err != nil {
		return transfer.FileSettlement{}, err
	}
	settlement, err := verifiedSettlement(transfer.FilePublished, transaction.binding, published)
	return settlement, joinFailures(ctx, err, transaction.closeResourcesLocked(ctx))
}

func (transaction *Transaction) Pause(
	ctx context.Context,
	reason transfer.FilePauseReason,
) (transfer.FileSettlement, error) {
	if reason < transfer.FilePauseInterrupted || reason > transfer.FilePauseDependencyContract {
		return transfer.FileSettlement{}, fileContractError(transfer.ErrInvalidOutputSettlement)
	}
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	if _, err := transaction.checkpointLocked(ctx); err != nil {
		return transfer.FileSettlement{}, err
	}
	next, err := pauseRecord(transaction.record)
	if err != nil {
		return transfer.FileSettlement{}, fileContractError(err)
	}
	previous := transaction.record
	if _, err := transaction.engine.storeRecord(ctx, &previous, next); err != nil {
		return transfer.FileSettlement{}, err
	}
	transaction.record = next
	settlement, err := verifiedSettlement(transfer.FilePaused, transaction.binding, next)
	return settlement, joinFailures(ctx, err, transaction.closeResourcesLocked(ctx))
}

func (transaction *Transaction) Retire(
	ctx context.Context,
	reason transfer.FileRetireReason,
) (transfer.FileSettlement, error) {
	var retirement checkpointmodel.RetirementReason
	switch reason {
	case transfer.FileRetireIsolatedPermanentSourceFailure:
		retirement = checkpointmodel.RetirementIsolatedFailure
	case transfer.FileRetireInvalidatedRevision:
		retirement = checkpointmodel.RetirementInvalidatedRevision
	default:
		return transfer.FileSettlement{}, fileContractError(ErrRetirementUnauthorized)
	}
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	if err := transaction.validateOpen(ctx); err != nil {
		return transfer.FileSettlement{}, err
	}
	next, err := retiredRecord(transaction.record, retirement)
	if err != nil {
		return transfer.FileSettlement{}, fileContractError(err)
	}
	previous := transaction.record
	if _, err := transaction.engine.storeRecord(ctx, &previous, next); err != nil {
		return transfer.FileSettlement{}, err
	}
	transaction.record = next
	if err := transaction.closeFileLocked(ctx); err != nil {
		return transfer.FileSettlement{}, err
	}
	if err := transaction.cleanupOwned(ctx); err != nil {
		return transfer.FileSettlement{}, err
	}
	settlement, err := transfer.NewRetiredFileSettlement(transaction.binding)
	return settlement, joinFailures(ctx, err, transaction.closeDestinationLocked(ctx))
}

func (transaction *Transaction) validateOpen(ctx context.Context) error {
	if ctx == nil {
		return fileContractError(ErrInvalidConfiguration)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if transaction.state != transactionOpen || transaction.engine == nil || transaction.file == nil ||
		transaction.destination == nil || transaction.record.Phase() != checkpointmodel.PhaseActive ||
		transaction.record.CommitState() != checkpointmodel.CommitVerified {
		return bindingError(ErrTransactionClosed)
	}
	return nil
}

func (transaction *Transaction) retainPublishedWitness(ctx context.Context) error {
	for _, step := range []RetirementStep{RetirementRemoveStage, RetirementSyncStageNamespace} {
		observation, err := transaction.engine.platform.ApplyRetirement(ctx, transaction.record.OwnedObjectID(), step)
		if err != nil || !observation.validFor(transaction.record.OwnedObjectID()) {
			return collaboratorError(ctx, errors.Join(err, ErrRetirementAmbiguous))
		}
	}
	return nil
}

func (transaction *Transaction) cleanupOwned(ctx context.Context) error {
	for _, step := range []RetirementStep{
		RetirementRemoveStage, RetirementSyncStageNamespace,
		RetirementRemoveAnchor, RetirementSyncAnchorNamespace,
	} {
		observation, err := transaction.engine.platform.ApplyRetirement(ctx, transaction.record.OwnedObjectID(), step)
		if err != nil || !observation.validFor(transaction.record.OwnedObjectID()) {
			return collaboratorError(ctx, errors.Join(err, ErrRetirementAmbiguous))
		}
	}
	return nil
}

func (transaction *Transaction) closeFileLocked(ctx context.Context) error {
	err := collaboratorError(ctx, closeOwnedFile(transaction.file))
	transaction.file = nil
	return err
}

func (transaction *Transaction) closeDestinationLocked(ctx context.Context) error {
	err := collaboratorError(ctx, closeDestination(transaction.destination))
	transaction.destination = nil
	transaction.state = transactionClosed
	return err
}

func (transaction *Transaction) closeResourcesLocked(ctx context.Context) error {
	return joinFailures(ctx, transaction.closeFileLocked(ctx), transaction.closeDestinationLocked(ctx))
}

func rangesIntersect(ranges content.RangeSet, offset, end uint64) bool {
	for _, current := range ranges.Ranges() {
		if current.Offset >= end {
			return false
		}
		if current.End > offset {
			return true
		}
	}
	return false
}

func verifiedSettlement(
	kind transfer.FileSettlementKind,
	binding transfer.MaterializedFileBinding,
	record checkpointmodel.Record,
) (transfer.FileSettlement, error) {
	durable, err := durableRanges(binding, record)
	if err != nil {
		return transfer.FileSettlement{}, err
	}
	return transfer.NewVerifiedFileSettlement(kind, durable)
}

func quarantinedSettlement(
	binding transfer.MaterializedFileBinding,
	record checkpointmodel.Record,
) (transfer.FileSettlement, error) {
	if record.Phase() != checkpointmodel.PhaseQuarantined ||
		record.CommitState() != checkpointmodel.CommitQuarantined {
		return transfer.FileSettlement{}, ErrCheckpointBinding
	}
	reference, err := transfer.NewMaterializationStateRef(
		binding.OutputSessionID(), binding.Locator().Digest(),
	)
	if err != nil {
		return transfer.FileSettlement{}, err
	}
	return transfer.NewTransactionQuarantinedFileSettlement(
		binding, reference, transferQuarantineReason(record.QuarantineReason()),
	)
}

func transferQuarantineReason(reason checkpointmodel.QuarantineReason) transfer.QuarantineReason {
	switch reason {
	case checkpointmodel.QuarantinePublicationHistory,
		checkpointmodel.QuarantineFinalMismatch,
		checkpointmodel.QuarantineFinalUnsafe,
		checkpointmodel.QuarantineMetadataMismatch:
		return transfer.QuarantinePublicationAmbiguous
	case checkpointmodel.QuarantinePartialObjectCreation:
		return transfer.QuarantineRetirementMismatch
	case checkpointmodel.QuarantineUpdateTemporary,
		checkpointmodel.QuarantineOutputObjectDuplicate:
		return transfer.QuarantineStateCorrupt
	default:
		return transfer.QuarantineOwnershipMismatch
	}
}
