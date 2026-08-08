package fileexecution

import (
	"context"
	"io"
	"math"
	"sync"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
)

type transactionState uint8

type terminalTransition func(
	context.Context,
) (transfer.FileSettlement, outputsession.MutationCut, bool, error)

const (
	transactionOpen transactionState = iota + 1
	transactionClosed
)

type Transaction struct {
	engine      *Engine
	claim       outputsession.FileClaim
	destination FileDestination
	file        OwnedFile
	binding     transfer.OutputFileBinding
	owned       OwnedCondition

	mu      sync.Mutex
	record  checkpointmodel.Record
	pending content.RangeSet
	state   transactionState
}

var _ outputsession.FileTransactionExecutor = (*Transaction)(nil)

func (transaction *Transaction) Binding() transfer.OutputFileBinding {
	if transaction == nil {
		return transfer.OutputFileBinding{}
	}
	return transaction.binding
}

func (transaction *Transaction) WriteRange(
	ctx context.Context,
	offset uint64,
	data []byte,
) (outputsession.MutationCut, error) {
	if transaction == nil || transaction.engine == nil {
		return outputsession.MutationNoChange, fileContractError(ErrInvalidConfiguration)
	}
	operationID := transaction.engine.nextOperationID()
	transaction.mu.Lock()
	previous := transaction.record.Phase()
	cut, outcome, err := transaction.writeRangeLocked(ctx, offset, data)
	event := transaction.engine.traceEvent(
		transaction.claim.ID(), operationID, TraceWriteRange, outcome,
		previous, transaction.record.Phase(), traceFault(err),
	)
	transaction.mu.Unlock()
	transaction.engine.emit(event)
	return cut, err
}

func (transaction *Transaction) writeRangeLocked(
	ctx context.Context,
	offset uint64,
	data []byte,
) (outputsession.MutationCut, TraceOutcome, error) {
	if err := transaction.validateOpen(ctx); err != nil {
		return outputsession.MutationNoChange, TraceNoChange, err
	}
	if len(data) == 0 {
		return outputsession.MutationStable, TraceSucceeded, nil
	}
	if offset > transaction.binding.ExactSize() ||
		uint64(len(data)) > transaction.binding.ExactSize()-offset ||
		offset > math.MaxInt64 || uint64(len(data)) > math.MaxInt64-offset {
		return outputsession.MutationNoChange, TraceNoChange, fileContractError(ErrRangeOutOfBounds)
	}
	end := offset + uint64(len(data))
	verified, err := contentRanges(transaction.record)
	if err != nil {
		return outputsession.MutationNoChange, TraceNoChange, fileContractError(err)
	}
	if rangesIntersect(verified, offset, end) || rangesIntersect(transaction.pending, offset, end) {
		return outputsession.MutationNoChange, TraceNoChange, fileContractError(ErrRangeOverlap)
	}
	written, err := content.NewRangeSet([]content.Range{{Offset: offset, End: end}})
	if err != nil {
		return outputsession.MutationNoChange, TraceNoChange, fileContractError(err)
	}
	pending, err := transfer.MergeRanges(transaction.pending, written)
	if err != nil {
		return outputsession.MutationNoChange, TraceNoChange, fileContractError(err)
	}
	count, writeErr := transaction.file.WriteAt(data, int64(offset))
	switch {
	case count == len(data):
		transaction.pending = pending
		return outputsession.MutationStable, traceOutcomeForReconcile(writeErr), nil
	case count == 0:
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		return outputsession.MutationNoChange, TraceNoChange, collaboratorError(ctx, writeErr)
	default:
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		return outputsession.MutationAmbiguous, TraceNeedsAttention, collaboratorError(ctx, writeErr)
	}
}

func (transaction *Transaction) Checkpoint(
	ctx context.Context,
) (transfer.VerifiedDurableRanges, outputsession.MutationCut, error) {
	if transaction == nil || transaction.engine == nil {
		return transfer.VerifiedDurableRanges{}, outputsession.MutationNoChange,
			fileContractError(ErrInvalidConfiguration)
	}
	operationID := transaction.engine.nextOperationID()
	transaction.mu.Lock()
	previous := transaction.record.Phase()
	checkpoint, cut, reconciled, err := transaction.checkpointLocked(ctx)
	outcome := traceOutcome(reconciled)
	if err != nil {
		outcome = TraceNoChange
		if cut == outputsession.MutationAmbiguous {
			outcome = TraceNeedsAttention
		}
	}
	event := transaction.engine.traceEvent(
		transaction.claim.ID(), operationID, TraceCheckpoint, outcome,
		previous, transaction.record.Phase(), traceFault(err),
	)
	transaction.mu.Unlock()
	transaction.engine.emit(event)
	return checkpoint, cut, err
}

func (transaction *Transaction) checkpointLocked(
	ctx context.Context,
) (transfer.VerifiedDurableRanges, outputsession.MutationCut, bool, error) {
	if err := transaction.validateOpen(ctx); err != nil {
		return transfer.VerifiedDurableRanges{}, outputsession.MutationNoChange, false, err
	}
	if transaction.pending.IsEmpty() {
		durable, err := durableRanges(transaction.binding, transaction.record)
		if err != nil {
			return transfer.VerifiedDurableRanges{}, outputsession.MutationNoChange, false, fileContractError(err)
		}
		return durable, outputsession.MutationStable, false, nil
	}
	if err := transaction.file.Sync(); err != nil {
		return transfer.VerifiedDurableRanges{}, outputsession.MutationNoChange, false, collaboratorError(ctx, err)
	}
	verified, err := contentRanges(transaction.record)
	if err != nil {
		return transfer.VerifiedDurableRanges{}, outputsession.MutationNoChange, false, fileContractError(err)
	}
	merged, err := transfer.MergeRanges(verified, transaction.pending)
	if err != nil {
		return transfer.VerifiedDurableRanges{}, outputsession.MutationNoChange, false, fileContractError(err)
	}
	candidate, err := checkpointmodel.AdvanceGeneration(
		transaction.record, checkpointRanges(merged), checkpointmodel.PhaseActive,
		checkpointmodel.CommitCandidate,
	)
	if err != nil {
		return transfer.VerifiedDurableRanges{}, outputsession.MutationNoChange, false, fileContractError(err)
	}
	next, err := checkpointmodel.Promote(
		candidate, checkpointmodel.PhaseActive, checkpointmodel.CommitVerified,
	)
	if err != nil {
		return transfer.VerifiedDurableRanges{}, outputsession.MutationNoChange, false, fileContractError(err)
	}
	previous := transaction.record
	cut, reconciled, err := transaction.engine.storeRecord(ctx, &previous, next)
	if err != nil {
		return transfer.VerifiedDurableRanges{}, cut, reconciled, err
	}
	transaction.record = next
	transaction.pending, _ = content.NewRangeSet(nil)
	durable, err := durableRanges(transaction.binding, next)
	if err != nil {
		return transfer.VerifiedDurableRanges{}, outputsession.MutationAmbiguous, reconciled, fileContractError(err)
	}
	return durable, outputsession.MutationStable, reconciled, nil
}

func (transaction *Transaction) Pause(
	ctx context.Context,
	reason transfer.FilePauseReason,
) (transfer.FileSettlement, outputsession.MutationCut, error) {
	return transaction.runTerminalTransition(ctx, TracePause, func(ctx context.Context) (
		transfer.FileSettlement,
		outputsession.MutationCut,
		bool,
		error,
	) {
		return transaction.pauseLocked(ctx, reason)
	})
}

func (transaction *Transaction) runTerminalTransition(
	ctx context.Context,
	operation TraceOperation,
	transition terminalTransition,
) (transfer.FileSettlement, outputsession.MutationCut, error) {
	if transaction == nil || transaction.engine == nil || transition == nil {
		return transfer.FileSettlement{}, outputsession.MutationNoChange,
			fileContractError(ErrInvalidConfiguration)
	}
	operationID := transaction.engine.nextOperationID()
	transaction.mu.Lock()
	previousPhase := transaction.record.Phase()
	settlement, cut, reconciled, err := transition(ctx)
	event := transaction.engine.traceEvent(
		transaction.claim.ID(), operationID, operation,
		terminalTraceOutcome(cut, reconciled, err),
		previousPhase, transaction.record.Phase(), traceFault(err),
	)
	transaction.mu.Unlock()
	transaction.engine.emit(event)
	return settlement, cut, err
}

func terminalTraceOutcome(
	cut outputsession.MutationCut,
	reconciled bool,
	err error,
) TraceOutcome {
	if err == nil {
		return traceOutcome(reconciled)
	}
	if cut == outputsession.MutationAmbiguous {
		return TraceNeedsAttention
	}
	return TraceNoChange
}

func (transaction *Transaction) pauseLocked(
	ctx context.Context,
	reason transfer.FilePauseReason,
) (transfer.FileSettlement, outputsession.MutationCut, bool, error) {
	if reason < transfer.FilePauseInterrupted || reason > transfer.FilePauseDependencyContract {
		return transfer.FileSettlement{}, outputsession.MutationNoChange, false,
			fileContractError(transfer.ErrInvalidOutputSettlement)
	}
	if _, cut, _, err := transaction.checkpointLocked(ctx); err != nil {
		return transfer.FileSettlement{}, cut, false, err
	}
	next, err := pauseRecord(transaction.record)
	if err != nil {
		return transfer.FileSettlement{}, outputsession.MutationNoChange, false, fileContractError(err)
	}
	previous := transaction.record
	cut, reconciled, err := transaction.engine.storeRecord(ctx, &previous, next)
	if err != nil {
		return transfer.FileSettlement{}, cut, reconciled, err
	}
	transaction.record = next
	settlement, err := verifiedSettlement(transfer.FilePaused, transaction.binding, next)
	if err != nil {
		return transfer.FileSettlement{}, outputsession.MutationAmbiguous, reconciled, fileContractError(err)
	}
	if err := transaction.closeResourcesLocked(ctx); err != nil {
		return transfer.FileSettlement{}, outputsession.MutationAmbiguous, reconciled, err
	}
	return settlement, outputsession.MutationStable, reconciled, nil
}

func (transaction *Transaction) validateOpen(ctx context.Context) error {
	if ctx == nil {
		return fileContractError(ErrInvalidConfiguration)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if transaction.state != transactionOpen || transaction.engine == nil ||
		transaction.file == nil || transaction.destination == nil ||
		transaction.record.Phase() != checkpointmodel.PhaseActive ||
		transaction.record.CommitState() != checkpointmodel.CommitVerified {
		return bindingError(ErrTransactionClosed)
	}
	return nil
}

func (transaction *Transaction) closeResourcesLocked(ctx context.Context) error {
	fileErr := collaboratorError(ctx, closeOwnedFile(transaction.file))
	destinationErr := collaboratorError(ctx, closeDestination(transaction.destination))
	transaction.file = nil
	transaction.destination = nil
	transaction.state = transactionClosed
	if fileErr != nil || destinationErr != nil {
		return joinFailures(ctx, fileErr, destinationErr)
	}
	return nil
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
