package fileexecution

import (
	"context"
	"errors"
	"io"
	"math"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
)

func (transaction *resumablePartialFileTransaction) MetadataWarnings() []MetadataWarning {
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	return append([]MetadataWarning(nil), transaction.warnings...)
}

func (transaction *resumablePartialFileTransaction) Binding() transfer.MaterializedFileBinding {
	if transaction == nil {
		return transfer.MaterializedFileBinding{}
	}
	return transaction.binding
}

func (transaction *resumablePartialFileTransaction) WriteRange(ctx context.Context, offset uint64, data []byte) error {
	if transaction == nil || transaction.engine == nil {
		return fileContractError(ErrInvalidConfiguration)
	}
	sequence := transaction.engine.nextSequence()
	transaction.mu.Lock()
	previous := transaction.record.Phase()
	err := transaction.writeRangeLocked(ctx, offset, data)
	next := transaction.record.Phase()
	transaction.mu.Unlock()
	transaction.engine.emit(transaction.traceEvent(
		sequence, TraceWriteRange, traceOutcomeForError(err), previous, next, traceFault(err),
	))
	return err
}

func (transaction *resumablePartialFileTransaction) writeRangeLocked(ctx context.Context, offset uint64, data []byte) error {
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
	if written != len(data) || writeErr != nil {
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		if written > 0 {
			return fileStateError(writeErr)
		}
		return collaboratorError(ctx, writeErr)
	}
	transaction.pending = pending
	if transaction.flushTimer == nil {
		transaction.flushTimer = transaction.engine.afterFunc(CheckpointInterval, transaction.flushOnDeadline)
	}
	return nil
}

func (transaction *resumablePartialFileTransaction) Commit(ctx context.Context) (transfer.FileSettlement, error) {
	if transaction == nil || transaction.engine == nil {
		return transfer.FileSettlement{}, fileContractError(ErrInvalidConfiguration)
	}
	sequence := transaction.engine.nextSequence()
	transaction.mu.Lock()
	previous := transaction.record.Phase()
	settlement, err := transaction.commitLocked(ctx)
	next := transaction.record.Phase()
	cleanupErr := transaction.receiptCleanupErr
	transaction.mu.Unlock()
	transaction.engine.emit(transaction.traceEvent(
		sequence, traceOperationForSettlement(TracePublish, settlement, err),
		traceOutcomeForSettlement(settlement, err), previous, next, traceFault(err),
	))
	if next == checkpointmodel.PhasePublished {
		transaction.traceReceipt(cleanupErr)
	}
	return settlement, err
}

func (transaction *resumablePartialFileTransaction) commitLocked(ctx context.Context) (transfer.FileSettlement, error) {
	checkpoint, err := transaction.checkpointLocked(ctx)
	if err != nil {
		return transfer.FileSettlement{}, err
	}
	if !transfer.RangesCoverFile(transaction.binding.ExactSize(), checkpoint.Ranges()) {
		return transfer.FileSettlement{}, fileContractError(ErrIncompleteFile)
	}
	modified := transaction.materialization.Descriptor().ModifiedTime()
	// Metadata is presentation, not content authority. The exact-size data object
	// and authenticated full range coverage already decide file correctness;
	// an unsupported or rejected mtime must remain a best-effort warning.
	if err := transaction.file.SetModifiedTime(modified); err != nil {
		transaction.warnings = append(transaction.warnings, MetadataWarning{kind: MetadataModifiedTimeWarning})
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

func (transaction *resumablePartialFileTransaction) recoverPublication(ctx context.Context, retry bool) (transfer.FileStart, error) {
	transaction.mu.Lock()
	settlement, err := transaction.publishFromDurableState(ctx, retry)
	published := transaction.record.Phase() == checkpointmodel.PhasePublished
	cleanupErr := transaction.receiptCleanupErr
	transaction.mu.Unlock()
	if published {
		transaction.traceReceipt(cleanupErr)
	}
	return settlementStart(settlement, err)
}

func (transaction *resumablePartialFileTransaction) publishFromDurableState(
	ctx context.Context,
	retry bool,
) (transfer.FileSettlement, error) {
	if transaction.record.Phase() != checkpointmodel.PhasePublishing ||
		transaction.record.CommitState() != checkpointmodel.CommitVerified {
		return transfer.FileSettlement{}, checkpointBindingError(ErrCheckpointBinding)
	}
	expectation, err := expectationFor(transaction.record)
	if err != nil {
		return transfer.FileSettlement{}, fileContractError(err)
	}
	final, err := settlePublication(ctx, transaction.destination, transaction.file, expectation, retry)
	if err != nil {
		// An interrupted observation or flush proves neither success nor lost
		// ownership. Retain the publishing record and reconcile it on retry.
		return transfer.FileSettlement{}, collaboratorError(ctx, err)
	}
	if !final.valid() {
		return transaction.quarantineAndSettleLocked(ctx, checkpointmodel.QuarantinePublicationHistory)
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
		settlement, settlementErr := transfer.NewTransactionCollisionFileSettlement(transaction.binding)
		_ = transaction.closeResourcesLocked(ctx)
		return settlement, settlementErr
	}
	if final.Condition() == FinalUnsafe {
		return transaction.quarantineAndSettleLocked(ctx, checkpointmodel.QuarantineFinalUnsafe)
	}
	if final.Condition() != FinalMatchesPublication {
		return transaction.quarantineAndSettleLocked(ctx, checkpointmodel.QuarantinePublicationHistory)
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
	// The durable terminal record is the receipt. Private anchors can now be
	// released without making later continuation depend on the public path.
	cleanupErr := transaction.closeFileLocked(ctx)
	if cleanupErr == nil {
		cleanupErr = transaction.cleanupOwned(ctx)
	}
	transaction.receiptCleanupErr = cleanupErr
	// A final found after restart establishes past delivery only. Keep normal
	// resume fast rather than rereading public bytes that users may have edited.
	if !retry {
		settlement, settlementErr := transfer.NewPreviouslyPublishedFileSettlement(transaction.binding)
		_ = transaction.closeResourcesLocked(ctx)
		return settlement, settlementErr
	}
	settlement, err := verifiedSettlement(transfer.FilePublished, transaction.binding, published)
	if err == nil {
		provenance := transfer.FileDownloaded
		if transaction.resumed {
			provenance = transfer.FileResumed
		}
		settlement, err = settlement.WithPublicationProvenance(provenance)
	}
	if err == nil {
		settlement, err = settlement.WithMetadataWarnings(settlementMetadataWarnings(transaction.warnings))
	}
	// Cleanup failure retains the receipt; operation cleanup can retry later.
	_ = transaction.closeResourcesLocked(ctx)
	return settlement, err
}

func (transaction *resumablePartialFileTransaction) quarantineAndSettleLocked(
	ctx context.Context,
	reason checkpointmodel.QuarantineReason,
) (transfer.FileSettlement, error) {
	next, err := quarantineRecord(transaction.record, reason)
	if err != nil {
		return transfer.FileSettlement{}, fileContractError(err)
	}
	previous := transaction.record
	if _, err := transaction.engine.storeRecord(ctx, &previous, next); err != nil {
		return transfer.FileSettlement{}, err
	}
	transaction.record = next
	settlement, err := quarantinedSettlement(transaction.binding, next)
	_ = transaction.closeResourcesLocked(ctx)
	return settlement, err
}

func (transaction *resumablePartialFileTransaction) Pause(
	ctx context.Context,
	reason transfer.FilePauseReason,
) (transfer.FileSettlement, error) {
	if reason < transfer.FilePauseInterrupted || reason > transfer.FilePauseDependencyContract {
		return transfer.FileSettlement{}, fileContractError(transfer.ErrInvalidOutputSettlement)
	}
	if transaction == nil || transaction.engine == nil {
		return transfer.FileSettlement{}, fileContractError(ErrInvalidConfiguration)
	}
	sequence := transaction.engine.nextSequence()
	transaction.mu.Lock()
	previous := transaction.record.Phase()
	settlement, err := transaction.pauseLocked(ctx)
	next := transaction.record.Phase()
	transaction.mu.Unlock()
	transaction.engine.emit(transaction.traceEvent(
		sequence, traceOperationForSettlement(TracePause, settlement, err),
		traceOutcomeForSettlement(settlement, err), previous, next, traceFault(err),
	))
	return settlement, err
}

func (transaction *resumablePartialFileTransaction) pauseLocked(
	ctx context.Context,
) (transfer.FileSettlement, error) {
	if transaction.record.Phase() == checkpointmodel.PhasePublishing && transaction.state == transactionOpen {
		// Publication already has a complete durable record. Do not erase that
		// ordering cut when its namespace observation or flush was interrupted.
		settlement, err := verifiedSettlement(transfer.FilePaused, transaction.binding, transaction.record)
		return settlement, joinFailures(ctx, err, transaction.closeResourcesLocked(ctx))
	}
	if _, err := transaction.checkpointLocked(ctx); err != nil {
		return transfer.FileSettlement{}, joinFailures(ctx, err, transaction.closeResourcesLocked(ctx))
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
	_ = transaction.closeResourcesLocked(ctx)
	return settlement, err
}

func (transaction *resumablePartialFileTransaction) Retire(
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
	if transaction == nil || transaction.engine == nil {
		return transfer.FileSettlement{}, fileContractError(ErrInvalidConfiguration)
	}
	sequence := transaction.engine.nextSequence()
	transaction.mu.Lock()
	previous := transaction.record.Phase()
	settlement, err := transaction.retireLocked(ctx, retirement)
	next := transaction.record.Phase()
	transaction.mu.Unlock()
	transaction.engine.emit(transaction.traceEvent(
		sequence, traceOperationForSettlement(TraceRetire, settlement, err),
		traceOutcomeForSettlement(settlement, err), previous, next, traceFault(err),
	))
	return settlement, err
}

func (transaction *resumablePartialFileTransaction) retireLocked(
	ctx context.Context,
	retirement checkpointmodel.RetirementReason,
) (transfer.FileSettlement, error) {
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
		settlement, quarantineErr := transaction.quarantineAndSettleLocked(
			ctx, checkpointmodel.QuarantinePartialObjectCreation,
		)
		if quarantineErr != nil {
			return transfer.FileSettlement{}, joinFailures(ctx, quarantineErr, err)
		}
		return settlement, nil
	}
	if err := transaction.cleanupOwned(ctx); err != nil {
		settlement, quarantineErr := transaction.quarantineAndSettleLocked(
			ctx, checkpointmodel.QuarantinePartialObjectCreation,
		)
		if quarantineErr != nil {
			return transfer.FileSettlement{}, joinFailures(ctx, quarantineErr, err)
		}
		return settlement, nil
	}
	settlement, err := transfer.NewFailedFileSettlement(transaction.binding)
	_ = transaction.closeDestinationLocked(ctx)
	return settlement, err
}

func (transaction *resumablePartialFileTransaction) validateOpen(ctx context.Context) error {
	if ctx == nil {
		return fileContractError(ErrInvalidConfiguration)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if transaction.flushFailure != nil {
		return transaction.flushFailure
	}
	if transaction.state != transactionOpen || transaction.engine == nil || transaction.file == nil ||
		transaction.destination == nil || transaction.record.Phase() != checkpointmodel.PhaseActive ||
		transaction.record.CommitState() != checkpointmodel.CommitVerified {
		return bindingError(ErrTransactionClosed)
	}
	return nil
}

func (transaction *resumablePartialFileTransaction) cleanupOwned(ctx context.Context) error {
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

func (transaction *resumablePartialFileTransaction) closeFileLocked(ctx context.Context) error {
	if transaction.flushTimer != nil {
		transaction.flushTimer.Stop()
		transaction.flushTimer = nil
	}
	err := collaboratorError(ctx, closeOwnedFile(transaction.file))
	transaction.file = nil
	return err
}

func (transaction *resumablePartialFileTransaction) closeDestinationLocked(ctx context.Context) error {
	err := collaboratorError(ctx, closeDestination(transaction.destination))
	transaction.destination = nil
	transaction.state = transactionClosed
	return err
}

func (transaction *resumablePartialFileTransaction) closeResourcesLocked(ctx context.Context) error {
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
	return itemBlockedSettlement(binding, transferQuarantineReason(record.QuarantineReason()))
}

func itemBlockedSettlement(
	binding transfer.MaterializedFileBinding,
	reason transfer.ItemBlockReason,
) (transfer.FileSettlement, error) {
	reference, err := transfer.NewMaterializationStateRef(
		binding.OutputSessionID(), binding.Locator().Digest(),
	)
	if err != nil {
		return transfer.FileSettlement{}, err
	}
	return transfer.NewTransactionItemBlockedFileSettlement(binding, reference, reason)
}

func transferQuarantineReason(reason checkpointmodel.QuarantineReason) transfer.ItemBlockReason {
	switch reason {
	case checkpointmodel.QuarantinePublicationHistory,
		checkpointmodel.QuarantineFinalMismatch,
		checkpointmodel.QuarantineFinalUnsafe,
		checkpointmodel.QuarantineMetadataMismatch:
		return transfer.ItemBlockPublicationAmbiguous
	case checkpointmodel.QuarantinePartialObjectCreation:
		return transfer.ItemBlockRetirementUncertain
	case checkpointmodel.QuarantineUpdateTemporary,
		checkpointmodel.QuarantineOutputObjectDuplicate:
		return transfer.ItemBlockStateCorrupt
	default:
		return transfer.ItemBlockOwnershipUnknown
	}
}
