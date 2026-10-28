package fileexecution

import (
	"context"
	"time"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
)

// CheckpointTimer lets tests drive an idle flush without sleeping.
type CheckpointTimer interface{ Stop() bool }

const (
	CheckpointByteLimit = 8 << 20
	CheckpointInterval  = 2 * time.Second
)

func (transaction *resumablePartialFileTransaction) Accept(ctx context.Context) (transfer.FileProgress, error) {
	if transaction == nil || transaction.engine == nil {
		return transfer.FileProgress{}, fileContractError(ErrInvalidConfiguration)
	}
	transaction.mu.Lock()
	previous := transaction.record.Phase()
	generation := transaction.record.CheckpointGeneration()
	defer func() {
		next := transaction.record.Phase()
		flushed := transaction.record.CheckpointGeneration() != generation
		transaction.mu.Unlock()
		if flushed {
			transaction.engine.emit(transaction.traceEvent(transaction.engine.nextSequence(), TraceCheckpoint,
				TraceSucceeded, previous, next, traceFault(nil)))
		}
	}()
	if err := transaction.validateOpen(ctx); err != nil {
		return transfer.FileProgress{}, err
	}
	var unsaved uint64
	for _, interval := range transaction.pending.Ranges() {
		unsaved += interval.Length()
	}
	if unsaved >= CheckpointByteLimit || unsaved > 0 && transaction.engine.now().Sub(transaction.lastCheckpoint) >= CheckpointInterval {
		if _, err := transaction.checkpointLocked(ctx); err != nil {
			return transfer.FileProgress{}, err
		}
	}
	persisted, err := durableRanges(transaction.binding, transaction.record)
	if err != nil {
		return transfer.FileProgress{}, err
	}
	accepted, err := transfer.MergeRanges(persisted.Ranges(), transaction.pending)
	if err != nil {
		return transfer.FileProgress{}, err
	}
	if transaction.acceptedGeneration == ^transfer.CheckpointGeneration(0) {
		return transfer.FileProgress{}, fileContractError(ErrCheckpointBinding)
	}
	transaction.acceptedGeneration++
	progress, err := transfer.VerifyProcessRanges(transaction.binding, transaction.acceptedGeneration, accepted)
	if err != nil {
		return transfer.FileProgress{}, err
	}
	return transfer.NewFileProgress(progress, persisted)
}

func (transaction *resumablePartialFileTransaction) flushOnDeadline() {
	transaction.mu.Lock()
	if transaction.state != transactionOpen {
		transaction.mu.Unlock()
		return
	}
	previous := transaction.record.Phase()
	_, err := transaction.checkpointLocked(context.Background())
	transaction.flushFailure = err
	next := transaction.record.Phase()
	transaction.mu.Unlock()
	transaction.engine.emit(transaction.traceEvent(transaction.engine.nextSequence(), TraceCheckpoint,
		traceOutcomeForError(err), previous, next, traceFault(err)))
}

func (transaction *resumablePartialFileTransaction) Checkpoint(ctx context.Context) (transfer.VerifiedRanges, error) {
	if transaction == nil || transaction.engine == nil {
		return transfer.VerifiedRanges{}, fileContractError(ErrInvalidConfiguration)
	}
	sequence := transaction.engine.nextSequence()
	transaction.mu.Lock()
	previous := transaction.record.Phase()
	checkpoint, err := transaction.checkpointLocked(ctx)
	next := transaction.record.Phase()
	transaction.mu.Unlock()
	transaction.engine.emit(transaction.traceEvent(
		sequence, TraceCheckpoint, traceOutcomeForError(err), previous, next, traceFault(err),
	))
	return checkpoint, err
}

func (transaction *resumablePartialFileTransaction) checkpointLocked(ctx context.Context) (transfer.VerifiedRanges, error) {
	if err := transaction.validateOpen(ctx); err != nil {
		return transfer.VerifiedRanges{}, err
	}
	if transaction.pending.IsEmpty() {
		return durableRanges(transaction.binding, transaction.record)
	}
	if err := transaction.file.Sync(); err != nil {
		return transfer.VerifiedRanges{}, collaboratorError(ctx, err)
	}
	verified, err := contentRanges(transaction.record)
	if err != nil {
		return transfer.VerifiedRanges{}, fileContractError(err)
	}
	merged, err := transfer.MergeRanges(verified, transaction.pending)
	if err != nil {
		return transfer.VerifiedRanges{}, fileContractError(err)
	}
	next, err := checkpointmodel.AdvanceDurableRanges(transaction.record, checkpointRanges(merged))
	if err != nil {
		return transfer.VerifiedRanges{}, fileContractError(err)
	}
	previous := transaction.record
	if _, err := transaction.engine.storeRecord(ctx, &previous, next); err != nil {
		return transfer.VerifiedRanges{}, err
	}
	transaction.record = next
	transaction.pending, _ = content.NewRangeSet(nil)
	transaction.lastCheckpoint = transaction.engine.now()
	if transaction.flushTimer != nil {
		transaction.flushTimer.Stop()
		transaction.flushTimer = nil
	}
	return durableRanges(transaction.binding, next)
}
