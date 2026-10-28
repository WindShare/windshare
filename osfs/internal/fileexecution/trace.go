package fileexecution

import (
	"errors"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
)

type TraceOperation uint8

const (
	TraceBeginFile TraceOperation = iota + 1
	TraceCreateOwnedFile
	TraceRecoverFile
	TraceWriteRange
	TraceCheckpoint
	TracePublish
	TracePause
	TraceRetire
	TraceItemBlocked
	TraceDeliveryReceipt
	TraceReleaseLiveFile
	TraceLiveRetention
)

type TraceOutcome uint8

const (
	TraceSucceeded TraceOutcome = iota + 1
	TraceReconciled
	TraceCollision
	TraceNoChange
	TraceNeedsAttention
)

// Task and logical file identities correlate recovery decisions across restarts
// without making diagnostics a source of filesystem authority.
type TraceEvent struct {
	FileID            catalog.FileID
	OperationID       receivecontract.OperationID
	IntentDigest      transfer.ReceiveIntentDigest
	SessionID         transfer.OutputSessionID
	Sequence          uint64
	Operation         TraceOperation
	Outcome           TraceOutcome
	Previous          checkpointmodel.Phase
	Next              checkpointmodel.Phase
	Decision          checkpointmodel.CheckpointLineageDecision
	Fault             fault.Fault
	RetainedLiveFiles uint64
}

type TraceSink interface{ TraceFileExecution(TraceEvent) }

type TraceSinkFunc func(TraceEvent)

func (function TraceSinkFunc) TraceFileExecution(event TraceEvent) {
	if function != nil {
		function(event)
	}
}

func (engine *Engine) nextSequence() uint64 {
	if engine == nil {
		return 0
	}
	return engine.sequence.Add(1)
}

func (engine *Engine) emit(event TraceEvent) {
	if engine != nil && engine.trace != nil {
		engine.trace.TraceFileExecution(event)
	}
}

func (engine *Engine) traceEvent(
	sequence uint64,
	operation TraceOperation,
	outcome TraceOutcome,
	previous checkpointmodel.Phase,
	next checkpointmodel.Phase,
	value fault.Fault,
) TraceEvent {
	return TraceEvent{
		OperationID: engine.intent.OperationID(), IntentDigest: engine.intent.Digest(),
		SessionID: engine.sessionID, Sequence: sequence, Operation: operation,
		Outcome: outcome, Previous: previous, Next: next, Fault: value,
	}
}

func (engine *Engine) checkpointDecisionTrace(
	sequence uint64,
	decision checkpointmodel.CheckpointLineageDecision,
	file catalog.FileID,
) TraceEvent {
	outcome := TraceReconciled
	if decision == checkpointmodel.CheckpointLineageDecisionAbsent {
		outcome = TraceNoChange
	}
	event := engine.traceEvent(sequence, TraceCheckpoint, outcome, 0, 0, fault.Fault{})
	event.Decision = decision
	event.FileID = file
	return event
}

func traceFault(err error) fault.Fault {
	result := fault.NormalizeBoundaryError(err)
	value, _ := result.Fault()
	return value
}

func traceOutcomeForError(err error) TraceOutcome {
	if err == nil {
		return TraceSucceeded
	}
	if errors.Is(err, ErrTargetOwnershipUnknown) {
		return TraceNeedsAttention
	}
	return TraceNoChange
}

func traceOutcomeForSettlement(settlement transfer.FileSettlement, err error) TraceOutcome {
	if err != nil {
		return traceOutcomeForError(err)
	}
	if settlement.Kind() == transfer.FileCollision {
		return TraceCollision
	}
	if settlement.Kind() == transfer.FileItemBlocked {
		return TraceReconciled
	}
	return TraceSucceeded
}

func traceOperationForSettlement(
	operation TraceOperation,
	settlement transfer.FileSettlement,
	err error,
) TraceOperation {
	if err == nil && settlement.Kind() == transfer.FileItemBlocked {
		return TraceItemBlocked
	}
	return operation
}

func (transaction *resumablePartialFileTransaction) traceEvent(sequence uint64, operation TraceOperation, outcome TraceOutcome, previous, next checkpointmodel.Phase, value fault.Fault) TraceEvent {
	event := transaction.engine.traceEvent(sequence, operation, outcome, previous, next, value)
	event.FileID = transaction.binding.FileID()
	return event
}
func (transaction *resumablePartialFileTransaction) traceReceipt(err error) {
	transaction.engine.emit(transaction.traceEvent(transaction.engine.nextSequence(), TraceDeliveryReceipt,
		traceOutcomeForError(err), checkpointmodel.PhasePublished, checkpointmodel.PhasePublished, traceFault(err)))
}
