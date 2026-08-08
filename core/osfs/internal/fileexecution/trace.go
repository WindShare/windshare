package fileexecution

import (
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
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

// TraceEvent intentionally omits paths, receipt material, native handles, and
// raw collaborator errors. Claim and operation IDs are sufficient to reconstruct
// the runtime path without turning diagnostics into filesystem authority.
type TraceEvent struct {
	IntentDigest transfer.TransferIntentDigest
	SessionID    transfer.OutputSessionID
	ClaimID      outputsession.ClaimID
	OperationID  uint64
	Operation    TraceOperation
	Outcome      TraceOutcome
	Previous     checkpointmodel.Phase
	Next         checkpointmodel.Phase
	Fault        fault.Fault
}

type TraceSink interface {
	TraceFileExecution(TraceEvent)
}

type TraceSinkFunc func(TraceEvent)

func (function TraceSinkFunc) TraceFileExecution(event TraceEvent) {
	if function != nil {
		function(event)
	}
}

func (engine *Engine) nextOperationID() uint64 {
	if engine == nil {
		return 0
	}
	return engine.operationSequence.Add(1)
}

func (engine *Engine) emit(event TraceEvent) {
	if engine != nil && engine.trace != nil {
		engine.trace.TraceFileExecution(event)
	}
}

func (engine *Engine) traceEvent(
	claimID outputsession.ClaimID,
	operationID uint64,
	operation TraceOperation,
	outcome TraceOutcome,
	previous checkpointmodel.Phase,
	next checkpointmodel.Phase,
	value fault.Fault,
) TraceEvent {
	return TraceEvent{
		IntentDigest: engine.intent.Digest(), SessionID: engine.sessionID,
		ClaimID: claimID, OperationID: operationID, Operation: operation,
		Outcome: outcome, Previous: previous, Next: next, Fault: value,
	}
}

func traceFault(err error) fault.Fault {
	result := fault.NormalizeBoundaryError(err)
	value, _ := result.Fault()
	return value
}
