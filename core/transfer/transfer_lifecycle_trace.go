package transfer

import (
	"context"
	"errors"
	"time"

	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/core/transfer/revisionwait"
)

type TransferLifecycleStage uint8

const (
	TransferDiscoveryStarted TransferLifecycleStage = iota + 1
	TransferGenerationCommitted
	TransferDiscoveryCompleted
	TransferAdmissionStarted
	TransferAdmissionCompleted
	TransferDirectoryAdmitted
	TransferDirectoryFinalized
	TransferFileEnqueued
	TransferFileStarted
	TransferFileAdmitted
	TransferFileFirstWrite
	TransferFileSettled
	TransferCapacityRetryScheduled
	TransferCapacityRetryReady
	TransferCapacityRetrySucceeded
	TransferCapacityBudgetPaused
	TransferCapacityWaitCanceled
	TransferCapacityGenerationEnded
	TransferJobSettled
)

// FileSelectionDecision records the authenticated rule that admitted a file
// without exposing its catalog path in operational traces.
type FileSelectionDecision uint8

const (
	FileSelectionInherited FileSelectionDecision = iota + 1
	FileSelectionNodeOverride
	FileSelectionCatalogPathTarget
)

// TransferLifecycleTrace is deliberately text-free and identity-minimal.
// ReceiveOperationID, ReceiveIntentDigest, and TransferJobID correlate stable authority
// and one run without exposing catalog identities, names, or plaintext paths.
type TransferLifecycleTrace struct {
	Stage                 TransferLifecycleStage
	ReceiveOperationID    receivecontract.OperationID
	PlanKind              receivecontract.MaterializationPlanKind
	ProtocolSessionID     protocolsession.ProtocolSessionID
	TransferJobID         TransferJobID
	ReceiveIntentDigest   ReceiveIntentDigest
	OutputSessionID       OutputSessionID
	Discovery             DiscoveryStatus
	ConnectionSizeClass   ConnectionSizeClass
	FileSelection         FileSelectionDecision
	FileSettlement        FileSettlementKind
	ItemBlockReason       ItemBlockReason
	DirectTreeSettlement  DirectTreeSettlementKind
	Progress              ReceiveProgressSnapshot
	CapacityWaitID        revisionwait.WaitID
	CapacityGeneration    revisionwait.GenerationToken
	CapacityOperationID   protocolsession.OperationID
	CapacityAttempt       uint64
	CapacityHint          time.Duration
	CapacityJitter        time.Duration
	CapacityDelay         time.Duration
	CapacityAccumulated   time.Duration
	CapacityActiveWaiters uint32
	Fault                 fault.Fault
	Interruption          TransferInterruption
	Failed                bool
}

type TransferLifecycleTracer interface {
	TraceTransferLifecycle(TransferLifecycleTrace)
}

type TransferLifecycleTraceFunc func(TransferLifecycleTrace)

func (function TransferLifecycleTraceFunc) TraceTransferLifecycle(event TransferLifecycleTrace) {
	if function != nil {
		function(event)
	}
}

func (j *TransferJob) trace(event TransferLifecycleTrace) {
	if j == nil || j.tracer == nil {
		return
	}
	// Every lifecycle event belongs to the same immutable run namespace. Filling
	// these fields at the boundary prevents a new call site from silently
	// emitting an uncorrelatable legacy-only event.
	if event.TransferJobID.IsZero() {
		event.TransferJobID = j.jobID
	}
	if event.ProtocolSessionID.IsZero() {
		event.ProtocolSessionID = j.protocolSessionID()
	}
	if event.ReceiveIntentDigest.IsZero() {
		event.ReceiveIntentDigest = j.intent.Digest()
	}
	if event.ReceiveOperationID.IsZero() {
		event.ReceiveOperationID = j.intent.OperationID()
	}
	if event.PlanKind == 0 {
		event.PlanKind = j.intent.MaterializationPlan().Kind()
	}
	event.Progress = j.Progress()
	// Discovery and the file worker intentionally overlap. Serialize the
	// callback so a tracer can append to one audit stream without having to
	// provide its own scheduler-specific synchronization.
	j.traceMu.Lock()
	defer j.traceMu.Unlock()
	traceTransferLifecycle(j.tracer, event)
}

func traceTransferLifecycle(tracer TransferLifecycleTracer, event TransferLifecycleTrace) {
	// Tracing is diagnostic and must never become transfer or settlement
	// authority. Isolating panics here also keeps every call site consistent.
	defer func() { _ = recover() }()
	tracer.TraceTransferLifecycle(event)
}

func (r *jobRun) traceFileLifecycle(stage TransferLifecycleStage, plan plannedFile, failure error) {
	r.job.trace(TransferLifecycleTrace{
		Stage: stage, OutputSessionID: r.output.SessionID(),
		FileSelection: plan.selectionDecision,
		Fault:         closedFault(failure),
		Interruption:  closedInterruption(failure),
		Failed:        failure != nil,
	})
}

func (r *jobRun) traceRevisionWait(plan plannedFile, event revisionwait.Trace) {
	stage := TransferCapacityRetryScheduled
	switch event.Stage {
	case revisionwait.TraceRetryReady:
		stage = TransferCapacityRetryReady
	case revisionwait.TraceRetrySucceeded:
		stage = TransferCapacityRetrySucceeded
	case revisionwait.TraceBudgetPaused:
		stage = TransferCapacityBudgetPaused
	case revisionwait.TraceWaitCanceled:
		stage = TransferCapacityWaitCanceled
	case revisionwait.TraceGenerationEnded:
		stage = TransferCapacityGenerationEnded
	}
	trace := TransferLifecycleTrace{
		Stage: stage, ProtocolSessionID: event.ProtocolSession, FileSelection: plan.selectionDecision,
		CapacityWaitID: event.WaitID, CapacityGeneration: event.Generation,
		CapacityOperationID: event.ProtocolOperation,
		CapacityAttempt:     event.Attempt, CapacityHint: event.Hint,
		CapacityJitter: event.Jitter, CapacityDelay: event.Delay,
		CapacityAccumulated:   event.AccumulatedWait,
		CapacityActiveWaiters: event.ActiveWaiters,
		Failed: event.Stage == revisionwait.TraceBudgetPaused ||
			event.Stage == revisionwait.TraceWaitCanceled || event.Stage == revisionwait.TraceGenerationEnded,
	}
	if event.Stage == revisionwait.TraceWaitCanceled {
		switch {
		case errors.Is(event.Cause, context.DeadlineExceeded):
			trace.Interruption = TransferInterruptionDeadline
		case errors.Is(event.Cause, context.Canceled):
			trace.Interruption = TransferInterruptionCanceled
		}
	}
	switch event.Stage {
	case revisionwait.TraceBudgetPaused:
		trace.Fault = mustSessionFault(fault.ScopeOutputPause, fault.SessionResourceBudget)
	case revisionwait.TraceGenerationEnded:
		trace.Fault = mustSessionFault(fault.ScopeSessionTerminal, fault.SessionTransport)
	}
	r.job.trace(trace)
}
