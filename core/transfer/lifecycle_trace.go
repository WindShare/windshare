package transfer

import (
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/session/protocolsession"
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

// TransferLifecycleTrace is deliberately text-free. TransferJobID and
// IntentDigest correlate the run and its durable checkpoint namespace. A
// SelectionObservation, when present, is audit context only.
type TransferLifecycleTrace struct {
	Stage                TransferLifecycleStage
	ShareInstance        catalog.ShareInstance
	ProtocolSessionID    protocolsession.ProtocolSessionID
	TransferJobID        TransferJobID
	IntentDigest         TransferIntentDigest
	OutputSessionID      OutputSessionID
	SelectionObservation SelectionObservationV1
	// SelectionIdentity is retained only for audit correlation; durable state
	// uses IntentDigest and file-local bindings instead.
	SelectionIdentity   SelectionIdentity
	DirectoryID         catalog.DirectoryID
	DirectoryGeneration catalog.DirectoryGeneration
	FileID              catalog.FileID
	Discovery           DiscoveryStatus
	SelectionClass      SelectionClass
	FileSelection       FileSelectionDecision
	FileSettlement      FileSettlementKind
	JobSettlement       JobSettlementKind
	Failed              bool
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
	if event.ShareInstance.IsZero() {
		event.ShareInstance = j.share
	}
	if event.ProtocolSessionID.IsZero() {
		event.ProtocolSessionID = j.protocolSessionID
	}
	if event.IntentDigest.IsZero() {
		event.IntentDigest = j.intent.Digest()
	}
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

func (r *jobRun) traceFileLifecycle(stage TransferLifecycleStage, plan plannedFile, failed bool) {
	r.job.trace(TransferLifecycleTrace{
		Stage: stage, OutputSessionID: r.output.SessionID(),
		SelectionObservation: r.selectionObservation,
		DirectoryID:          plan.parentDirectory,
		DirectoryGeneration:  plan.parentGeneration,
		FileID:               plan.file,
		FileSelection:        plan.selectionDecision,
		Failed:               failed,
	})
}
