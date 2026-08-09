package fileexecution

import (
	"errors"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
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
	TraceQuarantine
)

type TraceOutcome uint8

const (
	TraceSucceeded TraceOutcome = iota + 1
	TraceReconciled
	TraceCollision
	TraceNoChange
	TraceNeedsAttention
)

// TraceEvent deliberately excludes paths, file IDs, handles, and raw errors.
// Stable operation identity plus a per-engine sequence reconstructs control flow
// without making diagnostics a source of filesystem authority.
type TraceEvent struct {
	OperationID  receivecontract.OperationID
	IntentDigest transfer.ReceiveIntentDigest
	SessionID    transfer.OutputSessionID
	Sequence     uint64
	Operation    TraceOperation
	Outcome      TraceOutcome
	Previous     checkpointmodel.Phase
	Next         checkpointmodel.Phase
	Fault        fault.Fault
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

func traceFault(err error) fault.Fault {
	result := fault.NormalizeBoundaryError(err)
	value, _ := result.Fault()
	return value
}

func traceOutcomeForError(err error) TraceOutcome {
	if err == nil {
		return TraceSucceeded
	}
	if errors.Is(err, ErrTargetOwnershipUnknown) || errors.Is(err, ErrPublicationAmbiguous) ||
		errors.Is(err, ErrRetirementAmbiguous) {
		return TraceNeedsAttention
	}
	return TraceNoChange
}

func traceOutcomeForSettlement(settlement transfer.FileSettlement, err error) TraceOutcome {
	if err != nil {
		return traceOutcomeForError(err)
	}
	if settlement.Kind() == transfer.FileCollision || settlement.Kind() == transfer.FilePublishBlocked {
		return TraceCollision
	}
	return TraceSucceeded
}
