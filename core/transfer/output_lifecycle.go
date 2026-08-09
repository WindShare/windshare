package transfer

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

var ErrInvalidOutputSettlement = errors.New("transfer output settlement is invalid")

// FileSettlementKind is durable output state, not an error classification.
// Keeping collision and quarantine out of the error channel lets a job continue
// other files without guessing backend policy from wrapped error text.
type FileSettlementKind uint8

const (
	FilePublished FileSettlementKind = iota + 1
	FilePaused
	FileRetired
	FileCollision
	FilePublishBlocked
	FileQuarantined
)

type FileSettlement struct {
	kind             FileSettlementKind
	target           FileMaterializationTarget
	binding          MaterializedFileBinding
	hasBinding       bool
	checkpoint       VerifiedDurableRanges
	hasCheckpoint    bool
	stateRef         MaterializationStateRef
	quarantineReason QuarantineReason
}

func NewCollisionFileSettlement(target FileMaterializationTarget) (FileSettlement, error) {
	if !target.valid() {
		return FileSettlement{}, ErrInvalidOutputSettlement
	}
	return FileSettlement{kind: FileCollision, target: target}, nil
}

func (s FileSettlement) Kind() FileSettlementKind          { return s.kind }
func (s FileSettlement) Target() FileMaterializationTarget { return s.target }

func NewRetiredFileSettlement(binding MaterializedFileBinding) (FileSettlement, error) {
	if !binding.valid() {
		return FileSettlement{}, ErrInvalidOutputSettlement
	}
	return FileSettlement{
		kind: FileRetired, target: binding.Target(), binding: binding, hasBinding: true,
	}, nil
}

// NewTransientPublishedFileSettlement represents publication whose bytes were
// consumed sequentially but are not resumable at any declared durability level.
// The transfer job proves full transient coverage before accepting this result.
func NewTransientPublishedFileSettlement(binding MaterializedFileBinding) (FileSettlement, error) {
	if !binding.valid() {
		return FileSettlement{}, ErrInvalidOutputSettlement
	}
	return FileSettlement{
		kind: FilePublished, target: binding.Target(), binding: binding, hasBinding: true,
	}, nil
}

func NewVerifiedFileSettlement(
	kind FileSettlementKind,
	checkpoint VerifiedDurableRanges,
) (FileSettlement, error) {
	if kind != FilePublished && kind != FilePaused && kind != FilePublishBlocked ||
		!checkpoint.Binding().valid() {
		return FileSettlement{}, ErrInvalidOutputSettlement
	}
	if (kind == FilePublished || kind == FilePublishBlocked) &&
		!RangesCoverFile(checkpoint.Binding().ExactSize(), checkpoint.Ranges()) {
		return FileSettlement{}, ErrInvalidOutputSettlement
	}
	return FileSettlement{
		kind: kind, target: checkpoint.Binding().Target(), binding: checkpoint.Binding(), hasBinding: true,
		checkpoint: checkpoint, hasCheckpoint: true,
	}, nil
}

func (s FileSettlement) VerifiedCheckpoint() (VerifiedDurableRanges, bool) {
	return s.checkpoint, s.hasCheckpoint
}

func (s FileSettlement) MaterializedBinding() (MaterializedFileBinding, bool) {
	return s.binding, s.hasBinding
}

type MaterializationStateRef struct {
	session OutputSessionID
	locator MaterializationLocatorDigest
}

func NewMaterializationStateRef(session OutputSessionID, locator MaterializationLocatorDigest) (MaterializationStateRef, error) {
	if session.IsZero() || locator == (MaterializationLocatorDigest{}) {
		return MaterializationStateRef{}, ErrInvalidOutputSettlement
	}
	return MaterializationStateRef{session: session, locator: locator}, nil
}

func (reference MaterializationStateRef) OutputSessionID() OutputSessionID {
	return reference.session
}
func (reference MaterializationStateRef) LocatorDigest() MaterializationLocatorDigest {
	return reference.locator
}
func (reference MaterializationStateRef) IsZero() bool {
	return reference.session.IsZero() || reference.locator == (MaterializationLocatorDigest{})
}

type QuarantineReason uint16

const (
	QuarantineStateCorrupt QuarantineReason = iota + 1
	QuarantineOwnershipMismatch
	QuarantinePublicationAmbiguous
	QuarantineRetirementMismatch
)

func NewImmediateQuarantinedFileSettlement(
	target FileMaterializationTarget,
	reference MaterializationStateRef,
	reason QuarantineReason,
) (FileSettlement, error) {
	if !validQuarantineSettlement(target, reference, reason) {
		return FileSettlement{}, ErrInvalidOutputSettlement
	}
	return FileSettlement{
		kind: FileQuarantined, target: target, stateRef: reference, quarantineReason: reason,
	}, nil
}

func NewTransactionQuarantinedFileSettlement(
	binding MaterializedFileBinding,
	reference MaterializationStateRef,
	reason QuarantineReason,
) (FileSettlement, error) {
	if !binding.valid() || !validQuarantineSettlement(binding.Target(), reference, reason) {
		return FileSettlement{}, ErrInvalidOutputSettlement
	}
	return FileSettlement{
		kind: FileQuarantined, target: binding.Target(), binding: binding, hasBinding: true,
		stateRef: reference, quarantineReason: reason,
	}, nil
}

func validQuarantineSettlement(
	target FileMaterializationTarget,
	reference MaterializationStateRef,
	reason QuarantineReason,
) bool {
	return target.valid() && !reference.IsZero() &&
		reference.OutputSessionID() == target.OutputSessionID() &&
		reference.LocatorDigest() == target.Locator().Digest() &&
		reason >= QuarantineStateCorrupt && reason <= QuarantineRetirementMismatch
}

func (s FileSettlement) Quarantine() (MaterializationStateRef, QuarantineReason, bool) {
	return s.stateRef, s.quarantineReason, s.kind == FileQuarantined && !s.stateRef.IsZero() && s.quarantineReason != 0
}

func (kind FileSettlementKind) valid() bool {
	return kind >= FilePublished && kind <= FileQuarantined
}

func (settlement FileSettlement) valid() bool {
	switch settlement.kind {
	case FileCollision:
		return settlement.target.valid() && !settlement.hasBinding && !settlement.hasCheckpoint &&
			settlement.stateRef.IsZero() && settlement.quarantineReason == 0
	case FileRetired:
		return settlement.target.valid() && settlement.hasBinding && settlement.binding.valid() &&
			settlement.binding.Target() == settlement.target && !settlement.hasCheckpoint &&
			settlement.stateRef.IsZero() && settlement.quarantineReason == 0
	case FilePublished:
		binding, bound := settlement.MaterializedBinding()
		if !bound || !binding.valid() || settlement.target != binding.Target() ||
			!settlement.stateRef.IsZero() || settlement.quarantineReason != 0 {
			return false
		}
		checkpoint, durable := settlement.VerifiedCheckpoint()
		return !durable || checkpoint.Binding() == binding &&
			RangesCoverFile(checkpoint.Binding().ExactSize(), checkpoint.Ranges())
	case FilePaused, FilePublishBlocked:
		checkpoint, ok := settlement.VerifiedCheckpoint()
		binding, bound := settlement.MaterializedBinding()
		if !ok || !bound || !binding.valid() || checkpoint.Binding() != binding ||
			settlement.target != binding.Target() || !settlement.stateRef.IsZero() || settlement.quarantineReason != 0 {
			return false
		}
		return (settlement.kind == FilePaused) || RangesCoverFile(checkpoint.Binding().ExactSize(), checkpoint.Ranges())
	case FileQuarantined:
		reference, reason, ok := settlement.Quarantine()
		if !ok || !validQuarantineSettlement(settlement.target, reference, reason) || settlement.hasCheckpoint {
			return false
		}
		return !settlement.hasBinding || settlement.binding.valid() && settlement.binding.Target() == settlement.target
	default:
		return false
	}
}

func (settlement FileSettlement) matchesTarget(target FileMaterializationTarget) bool {
	return settlement.valid() && target.valid() && settlement.Target() == target
}

func (settlement FileSettlement) matchesBinding(binding MaterializedFileBinding) bool {
	if !settlement.matchesTarget(binding.Target()) || !binding.valid() {
		return false
	}
	switch settlement.Kind() {
	case FilePublished:
		if checkpoint, durable := settlement.VerifiedCheckpoint(); durable {
			return checkpoint.Binding() == binding
		}
		settledBinding, ok := settlement.MaterializedBinding()
		return ok && settledBinding == binding
	case FilePaused, FilePublishBlocked:
		checkpoint, _ := settlement.VerifiedCheckpoint()
		return checkpoint.Binding() == binding
	case FileQuarantined, FileRetired:
		settledBinding, ok := settlement.MaterializedBinding()
		return ok && settledBinding == binding
	default:
		return false
	}
}

func (settlement FileSettlement) matchesCommittedOutput(
	binding MaterializedFileBinding,
	capabilities DirectTreeCapabilities,
) bool {
	if !settlement.matchesBinding(binding) {
		return false
	}
	if settlement.Kind() != FilePublished {
		return true
	}
	_, durablePublication := settlement.VerifiedCheckpoint()
	return durablePublication == (capabilities.Durability != DurabilityNone)
}

type FilePauseReason uint8

const (
	FilePauseInterrupted FilePauseReason = iota + 1
	FilePauseShutdown
	FilePauseTransportFailure
	FilePauseSessionFailure
	FilePauseOutputFailure
	FilePauseResourceBudget
	FilePauseDependencyContract
)

func (reason FilePauseReason) valid() bool {
	return reason >= FilePauseInterrupted && reason <= FilePauseDependencyContract
}

type FileRetireReason uint8

const (
	FileRetireIsolatedPermanentSourceFailure FileRetireReason = iota + 1
	FileRetireInvalidatedRevision
)

func (reason FileRetireReason) valid() bool {
	return reason >= FileRetireIsolatedPermanentSourceFailure && reason <= FileRetireInvalidatedRevision
}

type JobPauseReason uint8

const (
	JobPauseInterrupted JobPauseReason = iota + 1
	JobPauseShutdown
	JobPauseTransportFailure
	JobPauseSessionFailure
	JobPauseOutputFailure
	JobPauseResourceBudget
	JobPauseDependencyContract
)

func (reason JobPauseReason) valid() bool {
	return reason >= JobPauseInterrupted && reason <= JobPauseDependencyContract
}

type DirectTreeSettlementKind uint8

const (
	DirectTreeSettlementPublished DirectTreeSettlementKind = iota + 1
	DirectTreeSettlementPartialDirectory
	DirectTreeSettlementResumable
	DirectTreeSettlementNeedsAttention
)

type DirectTreeSettlement struct {
	kind DirectTreeSettlementKind
}

func NewDirectTreeSettlement(kind DirectTreeSettlementKind) (DirectTreeSettlement, error) {
	if !kind.valid() {
		return DirectTreeSettlement{}, ErrInvalidOutputSettlement
	}
	return DirectTreeSettlement{kind: kind}, nil
}

func (s DirectTreeSettlement) Kind() DirectTreeSettlementKind { return s.kind }

func (kind DirectTreeSettlementKind) valid() bool {
	return kind >= DirectTreeSettlementPublished && kind <= DirectTreeSettlementNeedsAttention
}

type FileTransaction interface {
	Binding() MaterializedFileBinding
	WriteRange(context.Context, uint64, []byte) error
	Checkpoint(context.Context) (VerifiedDurableRanges, error)
	Commit(context.Context) (FileSettlement, error)
	Pause(context.Context, FilePauseReason) (FileSettlement, error)
	Retire(context.Context, FileRetireReason) (FileSettlement, error)
}

type fileStartKind uint8

const (
	fileStartTransaction fileStartKind = iota + 1
	fileStartSettlement
)

// FileStart is a sum type. An immediate settlement never exposes a transaction,
// so a caller cannot accidentally act on an already terminal result. FileRetired
// is valid here only when a backend deterministically finishes a persisted
// retirement before returning from BeginFile.
type FileStart struct {
	kind        fileStartKind
	transaction FileTransaction
	durable     VerifiedDurableRanges
	settlement  FileSettlement
}

func NewFileTransactionStart(transaction FileTransaction, durable VerifiedDurableRanges) (FileStart, error) {
	if transaction == nil || !transaction.Binding().valid() || durable.Binding() != transaction.Binding() {
		return FileStart{}, ErrInvalidOutputSettlement
	}
	return FileStart{kind: fileStartTransaction, transaction: transaction, durable: durable}, nil
}

func NewFileSettlementStart(settlement FileSettlement) (FileStart, error) {
	switch settlement.Kind() {
	case FilePublished, FileCollision, FilePublishBlocked, FileQuarantined, FileRetired:
		_, durablePublication := settlement.VerifiedCheckpoint()
		if !settlement.valid() || settlement.Kind() == FilePublished && !durablePublication {
			return FileStart{}, ErrInvalidOutputSettlement
		}
		return FileStart{kind: fileStartSettlement, settlement: settlement}, nil
	default:
		return FileStart{}, ErrInvalidOutputSettlement
	}
}

func (start FileStart) Transaction() (FileTransaction, VerifiedDurableRanges, bool) {
	if start.kind != fileStartTransaction || start.transaction == nil {
		return nil, VerifiedDurableRanges{}, false
	}
	return start.transaction, start.durable, true
}

func (start FileStart) ImmediateSettlement() (FileSettlement, bool) {
	return start.settlement, start.kind == fileStartSettlement && start.settlement.valid()
}

func (start FileStart) valid() bool {
	if transaction, durable, ok := start.Transaction(); ok {
		return transaction.Binding().valid() && durable.Binding() == transaction.Binding()
	}
	_, ok := start.ImmediateSettlement()
	return ok
}

// DirectTreeMaterializer is the output-root boundary. It receives a confirmed intent,
// prepares only the root/recovery namespace, and leaves descendant creation to
// per-generation admissions after catalog terminal evidence arrives.
type DirectTreeMaterializer interface {
	OpenDirectTree(context.Context, ReceiveIntent) (DirectTreeSession, error)
}

type DirectTreeMaterializerFunc func(context.Context, ReceiveIntent) (DirectTreeSession, error)

func (function DirectTreeMaterializerFunc) OpenDirectTree(
	ctx context.Context,
	intent ReceiveIntent,
) (DirectTreeSession, error) {
	if function == nil {
		return nil, ErrInvalidOutputBinding
	}
	return function(ctx, intent)
}

// DirectTreeSessionBinding proves that an opened output namespace belongs to
// the exact immutable receive authority passed to the materializer. Keeping the
// three identities together prevents a backend from accidentally reusing a
// session across an intent, operation, or destination reservation boundary.
type DirectTreeSessionBinding struct {
	intentDigest  ReceiveIntentDigest
	operationID   receivecontract.OperationID
	bindingDigest receivecontract.BindingDigest
}

func BindDirectTreeSession(intent ReceiveIntent) (DirectTreeSessionBinding, error) {
	if intent.IsZero() || intent.MaterializationPlan().Kind() != receivecontract.PlanDirectTree {
		return DirectTreeSessionBinding{}, ErrInvalidOutputBinding
	}
	return DirectTreeSessionBinding{
		intentDigest:  intent.Digest(),
		operationID:   intent.OperationID(),
		bindingDigest: intent.BindingDigest(),
	}, nil
}

func (binding DirectTreeSessionBinding) ReceiveIntentDigest() ReceiveIntentDigest {
	return binding.intentDigest
}

func (binding DirectTreeSessionBinding) OperationID() receivecontract.OperationID {
	return binding.operationID
}

func (binding DirectTreeSessionBinding) BindingDigest() receivecontract.BindingDigest {
	return binding.bindingDigest
}

func (binding DirectTreeSessionBinding) valid() bool {
	return !binding.intentDigest.IsZero() && !binding.operationID.IsZero() && !binding.bindingDigest.IsZero()
}

// DirectTreeSession exists after the confirmed intent/root namespace is admitted.
// Descendant directories are admitted independently as generations commit.
// Finalization seals the authenticated claim, including the synthetic root; an
// exact settled retry returns the same cached DirectorySettlement, while a
// foreign receipt must fail before output mutation.
type DirectTreeSession interface {
	SessionID() OutputSessionID
	Binding() DirectTreeSessionBinding
	Capabilities() DirectTreeCapabilities
	AdmitDirectory(context.Context, MaterializationDirectory) (DirectoryAdmission, error)
	FinalizeDirectory(context.Context, DirectoryAdmission) (DirectorySettlement, error)
	BeginFile(context.Context, MaterializationFile) (FileStart, error)
	PauseTree(context.Context, JobPauseReason) (DirectTreeSettlement, error)
	FinalizeTree(context.Context, DirectTreeOutcome) (DirectTreeSettlement, error)
}

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

// TransferLifecycleTrace is deliberately text-free and identity-minimal.
// OperationID, ReceiveIntentDigest, and TransferJobID correlate stable authority
// and one run without exposing catalog identities, names, or plaintext paths.
type TransferLifecycleTrace struct {
	Stage                TransferLifecycleStage
	OperationID          receivecontract.OperationID
	PlanKind             receivecontract.MaterializationPlanKind
	ProtocolSessionID    protocolsession.ProtocolSessionID
	TransferJobID        TransferJobID
	ReceiveIntentDigest  ReceiveIntentDigest
	OutputSessionID      OutputSessionID
	Discovery            DiscoveryStatus
	ConnectionSizeClass  ConnectionSizeClass
	FileSelection        FileSelectionDecision
	FileSettlement       FileSettlementKind
	DirectTreeSettlement DirectTreeSettlementKind
	DiscoveredFileCount  uint64
	DiscoveredBytes      uint64
	CompletedFileCount   uint64
	CompletedBytes       uint64
	Fault                fault.Fault
	Failed               bool
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
		event.ProtocolSessionID = j.protocolSessionID
	}
	if event.ReceiveIntentDigest.IsZero() {
		event.ReceiveIntentDigest = j.intent.Digest()
	}
	if event.OperationID.IsZero() {
		event.OperationID = j.intent.OperationID()
	}
	if event.PlanKind == 0 {
		event.PlanKind = j.intent.MaterializationPlan().Kind()
	}
	measure := j.Measure()
	event.DiscoveredFileCount = measure.DiscoveredFiles
	event.DiscoveredBytes = measure.DiscoveredBytes
	event.CompletedFileCount = measure.CompletedFiles
	event.CompletedBytes = measure.CompletedBytes
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
		Failed:        failure != nil,
	})
}
