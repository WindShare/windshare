package transfer

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	diag "github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/internal/failurestate"
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
	TransferRevisionCleanupSettled
	TransferCapacityRetryScheduled
	TransferCapacityRetryReady
	TransferCapacityRetrySucceeded
	TransferCapacityBudgetPaused
	TransferCapacityWaitCanceled
	TransferCapacitySessionEnded
	TransferJobSettled
	TransferDeliverySkipped
	TransferFileRevisionReadmission
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
	FileID                catalog.FileID
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
	CapacityOperationID   protocolsession.OperationID
	CapacityAttempt       uint64
	CapacityHint          time.Duration
	CapacityJitter        time.Duration
	CapacityDelay         time.Duration
	CapacityAccumulated   time.Duration
	CapacityActiveWaiters uint32
	RevisionReadmission   RevisionReadmissionTrace
	Fault                 fault.Fault
	Interruption          TransferInterruption
	Failed                bool
}

type RevisionReadmissionTrace struct {
	Previous, Current content.FileRevision
	Decision          string
	ComparisonReason  content.RevisionComparisonReason
	Retained, Reused  uint64
	Elapsed           time.Duration
}

func (r *jobRun) traceFileRevisionRetry(
	plan plannedFile, reopened *content.RevisionReopenedError, decision string,
	retained, reused uint64, started time.Time,
) {
	r.job.trace(TransferLifecycleTrace{
		Stage: TransferFileRevisionReadmission, OutputSessionID: r.output.SessionID(), FileID: plan.file,
		RevisionReadmission: RevisionReadmissionTrace{
			Previous: reopened.Previous().FileRevision(), Current: reopened.Current().FileRevision(), Decision: decision,
			ComparisonReason: reopened.Comparison().Reason, Retained: retained, Reused: reused, Elapsed: time.Since(started),
		},
	})
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
		FileID:        plan.file,
		FileSelection: plan.selectionDecision,
		Fault:         failurestate.FaultOf(failure),
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
	case revisionwait.TraceSessionEnded:
		stage = TransferCapacitySessionEnded
	}
	trace := TransferLifecycleTrace{
		Stage: stage, ProtocolSessionID: event.ProtocolSession, FileSelection: plan.selectionDecision,
		CapacityOperationID:   event.ProtocolOperation,
		CapacityAttempt:       event.Attempt,
		CapacityHint:          event.Hint,
		CapacityJitter:        event.Jitter,
		CapacityDelay:         event.Delay,
		CapacityAccumulated:   event.AccumulatedWait,
		CapacityActiveWaiters: event.ActiveWaiters,
		Failed: event.Stage == revisionwait.TraceBudgetPaused ||
			event.Stage == revisionwait.TraceWaitCanceled || event.Stage == revisionwait.TraceSessionEnded,
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
	case revisionwait.TraceSessionEnded:
		trace.Fault = mustSessionFault(fault.ScopeSessionTerminal, fault.SessionTransport)
	}
	r.job.trace(trace)
}

// DiagnosticName belongs to the source vocabulary; recorders accept new values.
func (value TransferLifecycleStage) DiagnosticName() string {
	switch value {
	case TransferDeliverySkipped:
		return "delivery_skipped"
	case TransferFileRevisionReadmission:
		return "file_revision_readmission"
	case TransferDiscoveryStarted:
		return "discovery_started"
	case TransferGenerationCommitted:
		return "generation_committed"
	case TransferDiscoveryCompleted:
		return "discovery_completed"
	case TransferAdmissionStarted:
		return "admission_started"
	case TransferAdmissionCompleted:
		return "admission_completed"
	case TransferDirectoryAdmitted:
		return "directory_admitted"
	case TransferDirectoryFinalized:
		return "directory_finalized"
	case TransferFileEnqueued:
		return "file_enqueued"
	case TransferFileStarted:
		return "file_started"
	case TransferFileAdmitted:
		return "file_admitted"
	case TransferFileFirstWrite:
		return "file_first_write"
	case TransferFileSettled:
		return "file_settled"
	case TransferRevisionCleanupSettled:
		return "revision_cleanup_settled"
	case TransferCapacityRetryScheduled:
		return "capacity_retry_scheduled"
	case TransferCapacityRetryReady:
		return "capacity_retry_ready"
	case TransferCapacityRetrySucceeded:
		return "capacity_retry_succeeded"
	case TransferCapacityBudgetPaused:
		return "capacity_budget_paused"
	case TransferCapacityWaitCanceled:
		return "capacity_wait_canceled"
	case TransferCapacitySessionEnded:
		return "capacity_session_ended"
	case TransferJobSettled:
		return "job_settled"
	default:
		return "unknown_" + strconv.FormatUint(uint64(value), 10)
	}
}

// DiagnosticName belongs to the source vocabulary; recorders accept new values.
func (value FileSelectionDecision) DiagnosticName() string {
	if value == 0 {
		return "none"
	}
	switch value {
	case FileSelectionInherited:
		return "inherited"
	case FileSelectionNodeOverride:
		return "node_override"
	case FileSelectionCatalogPathTarget:
		return "catalog_path_target"
	default:
		return "unknown_" + strconv.FormatUint(uint64(value), 10)
	}
}

const lifecycleEventName = "transfer_lifecycle"

func (v TransferLifecycleTrace) DiagnosticRecord() diag.Record {
	f := diag.Fields{"stage": diag.Text(v.Stage.DiagnosticName()), "receive_operation_id": diag.Identity(v.ReceiveOperationID.Bytes()),
		"transfer_job_id": diag.Identity(v.TransferJobID.Bytes()), "file_selection": diag.Text(v.FileSelection.DiagnosticName()),
		"file_settlement": diag.Text(v.FileSettlement.DiagnosticName()), "tree_settlement": diag.Text(v.DirectTreeSettlement.DiagnosticName()),
		"failed": diag.Bool(v.Failed), "progress": v.Progress.DiagnosticValue()}
	if v.ItemBlockReason != 0 {
		f["item_block_reason"] = diag.Text(v.ItemBlockReason.DiagnosticName())
	}
	if !v.FileID.IsZero() {
		f["file_id"] = diag.Identity(v.FileID.Bytes())
	}
	c := diag.Fields{"protocol_session_id": diag.Identity(v.ProtocolSessionID[:]),
		"receive_operation_id": diag.Identity(v.ReceiveOperationID.Bytes()), "transfer_job_id": diag.Identity(v.TransferJobID.Bytes())}
	if !v.ReceiveIntentDigest.IsZero() {
		c["receive_intent_digest"] = diag.Identity(v.ReceiveIntentDigest.Bytes())
	}
	if !v.OutputSessionID.IsZero() {
		c["output_session_id"] = diag.Identity(v.OutputSessionID.Bytes())
	}
	f["plan_kind"] = diag.Number(float64(v.PlanKind))
	f["discovery"] = diag.Text(v.Discovery.DiagnosticName())
	f["connection_size_class"] = diag.Number(float64(v.ConnectionSizeClass))
	if v.Stage >= TransferCapacityRetryScheduled && v.Stage <= TransferCapacitySessionEnded {
		f["capacity"] = diag.Object(diag.Fields{
			"protocol_operation_id": diag.Identity(v.CapacityOperationID[:]), "attempt": diag.Uint(v.CapacityAttempt),
			"hint_ms": diag.Millis(v.CapacityHint), "jitter_ms": diag.Millis(v.CapacityJitter), "delay_ms": diag.Millis(v.CapacityDelay),
			"accumulated_wait_ms": diag.Millis(v.CapacityAccumulated), "active_waiters": diag.Number(float64(v.CapacityActiveWaiters))})
		c["protocol_operation_id"] = diag.Identity(v.CapacityOperationID[:])
	}
	if v.Stage == TransferFileRevisionReadmission {
		revision := v.RevisionReadmission
		f["revision_readmission"] = diag.Object(diag.Fields{
			"previous_revision": diag.Identity(revision.Previous.Bytes()), "current_revision": diag.Identity(revision.Current.Bytes()),
			"decision": diag.Text(revision.Decision), "comparison_reason": diag.Text(string(revision.ComparisonReason)),
			"retained_bytes": diag.Uint(revision.Retained), "reused_bytes": diag.Uint(revision.Reused),
			"elapsed_ms": diag.Millis(revision.Elapsed),
		})
	}
	if v.Failed {
		f["interruption"] = diag.Number(float64(v.Interruption))
		if !v.Fault.IsZero() {
			f["fault"] = diag.Object(diag.Fields{"domain": diag.Number(float64(v.Fault.Domain())), "scope": diag.Number(float64(v.Fault.Scope())), "code": diag.Number(float64(v.Fault.Code()))})
		}
	}
	return diag.New(lifecycleEventName, f).Correlate(c)
}
func (v ReceiveProgressSnapshot) DiagnosticValue() diag.Value {
	return diag.Object(diag.Fields{"discovery": diag.Text(v.Discovery.DiagnosticName()), "counters_exact": diag.Bool(v.CountersExact),
		"source_omitted_entries": diag.Uint(v.SourceOmittedEntries),
		"discovered_files":       diag.Uint(v.DiscoveredFiles), "discovered_bytes": diag.Uint(v.DiscoveredBytes),
		"published_files": diag.Uint(v.PublishedFiles), "published_bytes": diag.Uint(v.PublishedBytes),
		"verified_bytes": diag.Uint(v.VerifiedBytes), "previously_published_bytes": diag.Uint(v.PreviouslyPublishedBytes),
		"newly_verified_bytes":   diag.Uint(v.NewlyVerifiedBytes),
		"restart_reusable_bytes": diag.Uint(v.RestartReusableBytes),
		"file_outcomes":          v.FileOutcomes.DiagnosticValue(),
		"capacity_wait": diag.Object(diag.Fields{"active_waiters": diag.Number(float64(v.CapacityWait.ActiveWaiters)),
			"active_since":        diag.Text(v.CapacityWait.ActiveSince.Format(time.RFC3339Nano)),
			"visible_after":       diag.Text(v.CapacityWait.VisibleAfter.Format(time.RFC3339Nano)),
			"accumulated_wait_ms": diag.Millis(v.CapacityWait.AccumulatedWait), "attempts": diag.Uint(v.CapacityWait.Attempts)})})
}

func (v FileOutcomeSummary) DiagnosticValue() diag.Value {
	return diag.Object(diag.Fields{
		"downloaded_files":           diag.Uint(v.DownloadedFiles),
		"resumed_files":              diag.Uint(v.ResumedFiles),
		"previously_published_files": diag.Uint(v.PreviouslyPublishedFiles),
		"paused_files":               diag.Uint(v.PausedFiles),
		"collision_files":            diag.Uint(v.CollisionFiles),
		"failed_files":               diag.Uint(v.FailedFiles),
		"item_blocked_files":         diag.Uint(v.ItemBlockedFiles),
		"revision_conflict_files":    diag.Uint(v.RevisionConflictFiles),
		"checkpoint_invalid_files":   diag.Uint(v.CheckpointInvalidFiles),
		"owned_object_unknown_files": diag.Uint(v.OwnedObjectUnknownFiles),
		"modified_time_warnings":     diag.Uint(v.ModifiedTimeWarnings),
	})
}
