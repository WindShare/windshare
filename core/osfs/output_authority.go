package osfs

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/outputruntime"
	"github.com/windshare/windshare/core/osfs/internal/pathfailure"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

var (
	ErrOutOfRange  = outputfault.ErrOutOfRange
	ErrPathEscape  = outputfault.ErrPathEscape
	ErrPathTooLong = pathfailure.ErrTooLong
)

type FilesystemOutputCertificationID string

const (
	FilesystemOutputCertificationLinuxExt4ProcessRestart   FilesystemOutputCertificationID = "linux/ext4/process-restart/v2"
	FilesystemOutputCertificationWindowsNTFSProcessRestart FilesystemOutputCertificationID = "windows/ntfs/process-restart/v1"
)

type FilesystemResumeRoot struct {
	RootPath string
}

type FilesystemOutputFailureStage uint8

const (
	FilesystemOutputFailureDestinationBinding FilesystemOutputFailureStage = iota + 1
	FilesystemOutputFailureInventoryPaging
	FilesystemOutputFailureActiveLookup
	FilesystemOutputFailureOperationAcquisition
	FilesystemOutputFailureOperationAdmission
	FilesystemOutputFailureCheckpointReconciliation
	FilesystemOutputFailureNativeDurability
	FilesystemOutputFailureAuthorityClose
)

func (stage FilesystemOutputFailureStage) Valid() bool {
	return stage >= FilesystemOutputFailureDestinationBinding &&
		stage <= FilesystemOutputFailureAuthorityClose
}

func (stage FilesystemOutputFailureStage) String() string {
	switch stage {
	case FilesystemOutputFailureDestinationBinding:
		return "destination_binding"
	case FilesystemOutputFailureInventoryPaging:
		return "inventory_paging"
	case FilesystemOutputFailureActiveLookup:
		return "active_lookup"
	case FilesystemOutputFailureOperationAcquisition:
		return "operation_acquisition"
	case FilesystemOutputFailureOperationAdmission:
		return "operation_admission"
	case FilesystemOutputFailureCheckpointReconciliation:
		return "checkpoint_reconciliation"
	case FilesystemOutputFailureNativeDurability:
		return "native_durability"
	case FilesystemOutputFailureAuthorityClose:
		return "authority_close"
	default:
		return ""
	}
}

type FilesystemCheckpointReconciliationStep uint8

const (
	FilesystemCheckpointCandidateObservation FilesystemCheckpointReconciliationStep = iota + 1
	FilesystemCheckpointStageDurability
	FilesystemCheckpointNamespaceDurability
	FilesystemCheckpointRecordPromotion
)

func (step FilesystemCheckpointReconciliationStep) Valid() bool {
	return step >= FilesystemCheckpointCandidateObservation &&
		step <= FilesystemCheckpointRecordPromotion
}

func (step FilesystemCheckpointReconciliationStep) String() string {
	switch step {
	case FilesystemCheckpointCandidateObservation:
		return "candidate_observation"
	case FilesystemCheckpointStageDurability:
		return "stage_durability"
	case FilesystemCheckpointNamespaceDurability:
		return "namespace_durability"
	case FilesystemCheckpointRecordPromotion:
		return "record_promotion"
	default:
		return ""
	}
}

type FilesystemNativeErrorClass uint8

const (
	FilesystemNativeErrorAccessDenied FilesystemNativeErrorClass = iota + 1
	FilesystemNativeErrorSharingViolation
	FilesystemNativeErrorNotFound
	FilesystemNativeErrorInvalidHandle
	FilesystemNativeErrorUnsupported
	FilesystemNativeErrorIO
	FilesystemNativeErrorUnknown
)

func (class FilesystemNativeErrorClass) Valid() bool {
	return class >= FilesystemNativeErrorAccessDenied && class <= FilesystemNativeErrorUnknown
}

func (class FilesystemNativeErrorClass) String() string {
	switch class {
	case FilesystemNativeErrorAccessDenied:
		return "access_denied"
	case FilesystemNativeErrorSharingViolation:
		return "sharing_violation"
	case FilesystemNativeErrorNotFound:
		return "not_found"
	case FilesystemNativeErrorInvalidHandle:
		return "invalid_handle"
	case FilesystemNativeErrorUnsupported:
		return "unsupported"
	case FilesystemNativeErrorIO:
		return "io"
	case FilesystemNativeErrorUnknown:
		return "unknown"
	default:
		return ""
	}
}

type FilesystemOutputStateReason uint8

const (
	FilesystemOutputStateReasonNone FilesystemOutputStateReason = iota + 1
	FilesystemOutputStateDestinationOwnershipUnknown
	FilesystemOutputStateRegistryOwnershipUnknown
	FilesystemOutputStateLeaseOwnershipUnknown
	FilesystemOutputStateOperationOwnershipUnknown
	FilesystemOutputStateCleanupUncertain
)

func (reason FilesystemOutputStateReason) Valid() bool {
	return reason >= FilesystemOutputStateReasonNone &&
		reason <= FilesystemOutputStateCleanupUncertain
}

func (reason FilesystemOutputStateReason) String() string {
	switch reason {
	case FilesystemOutputStateReasonNone:
		return "none"
	case FilesystemOutputStateDestinationOwnershipUnknown:
		return "destination-ownership-unknown"
	case FilesystemOutputStateRegistryOwnershipUnknown:
		return "registry-ownership-unknown"
	case FilesystemOutputStateLeaseOwnershipUnknown:
		return "lease-ownership-unknown"
	case FilesystemOutputStateOperationOwnershipUnknown:
		return "operation-ownership-unknown"
	case FilesystemOutputStateCleanupUncertain:
		return "cleanup-uncertain"
	default:
		return ""
	}
}

type FilesystemOutputDiagnostic struct {
	Stage              FilesystemOutputFailureStage
	ReconciliationStep FilesystemCheckpointReconciliationStep
	NativeErrorClass   FilesystemNativeErrorClass
	FaultDomain        uint8
	NormalizedScope    uint8
	NormalizedCode     uint16
}

func (diagnostic FilesystemOutputDiagnostic) Valid() bool {
	if !diagnostic.Stage.Valid() {
		return false
	}
	if diagnostic.ReconciliationStep != 0 {
		if !diagnostic.ReconciliationStep.Valid() ||
			diagnostic.Stage != FilesystemOutputFailureCheckpointReconciliation &&
				diagnostic.Stage != FilesystemOutputFailureNativeDurability {
			return false
		}
	}
	if diagnostic.NativeErrorClass != 0 && !diagnostic.NativeErrorClass.Valid() {
		return false
	}
	faultZero := diagnostic.FaultDomain == 0 &&
		diagnostic.NormalizedScope == 0 &&
		diagnostic.NormalizedCode == 0
	faultComplete := diagnostic.FaultDomain != 0 &&
		diagnostic.NormalizedScope != 0 &&
		diagnostic.NormalizedCode != 0
	return faultZero || faultComplete
}

type FilesystemOutputDiagnosticCarrier interface {
	FilesystemOutputDiagnostic() FilesystemOutputDiagnostic
}

func FilesystemOutputDiagnosticFor(err error) (FilesystemOutputDiagnostic, bool) {
	var carrier FilesystemOutputDiagnosticCarrier
	if errors.As(err, &carrier) {
		diagnostic := carrier.FilesystemOutputDiagnostic()
		return diagnostic, diagnostic.Valid()
	}
	internal, ok := outputruntime.FilesystemOutputDiagnosticFor(err)
	if !ok {
		return FilesystemOutputDiagnostic{}, false
	}
	diagnostic := projectFilesystemOutputDiagnostic(internal)
	return diagnostic, diagnostic.Valid()
}

type FilesystemOutputTraceOperation uint8

const (
	TraceFilesystemCertified FilesystemOutputTraceOperation = iota + 1
	TraceFeatureProbeCompleted
	TraceCheckpointNamespaceOpened
	TraceNativeLock
	TraceSessionOpened
	TraceCheckpointReconciled
	TraceRuntimeDecision
)

type FilesystemOutputRootDisposition string

const (
	FilesystemOutputCallerProvidedContainer FilesystemOutputRootDisposition = "caller-provided-container"
	FilesystemOutputAuthorityCreatedRoot    FilesystemOutputRootDisposition = "authority-created-root"
)

type FilesystemOutputRuntimeComponent uint8

const (
	FilesystemOutputRuntimeSession FilesystemOutputRuntimeComponent = iota + 1
	FilesystemOutputRuntimeDirectory
	FilesystemOutputRuntimeFile
	FilesystemOutputRuntimeCheckpoint
)

type FilesystemOutputRuntimeOperation uint8

const (
	FilesystemOutputRuntimeOpenDirectTree FilesystemOutputRuntimeOperation = iota + 1
	FilesystemOutputRuntimeAcquireOperationLease
	FilesystemOutputRuntimeReconcileCheckpoints
	FilesystemOutputRuntimeAdmitDirectory
	FilesystemOutputRuntimeFinalizeDirectory
	FilesystemOutputRuntimeBeginFile
	FilesystemOutputRuntimeWriteRange
	FilesystemOutputRuntimeCheckpointFile
	FilesystemOutputRuntimeCommitFile
	FilesystemOutputRuntimePauseFile
	FilesystemOutputRuntimeRetireFile
	FilesystemOutputRuntimePauseTree
	FilesystemOutputRuntimeFinalizeTree
	FilesystemOutputRuntimeMaterializeDirectory
	FilesystemOutputRuntimeCreateOwnedFile
	FilesystemOutputRuntimeRecoverFile
	FilesystemOutputRuntimePublishFile
	FilesystemOutputRuntimeQuarantineFile
	FilesystemOutputRuntimeAdmitDestination
	FilesystemOutputRuntimeFirstWrite
	FilesystemOutputRuntimeCleanup
)

type FilesystemOutputRuntimeDecision uint8

const (
	FilesystemOutputRuntimeValidated FilesystemOutputRuntimeDecision = iota + 1
	FilesystemOutputRuntimeReserved
	FilesystemOutputRuntimeCoalesced
	FilesystemOutputRuntimeRejected
	FilesystemOutputRuntimeRolledBack
	FilesystemOutputRuntimeAdmitted
	FilesystemOutputRuntimeActive
	FilesystemOutputRuntimeSealed
	FilesystemOutputRuntimeSettled
	FilesystemOutputRuntimeAmbiguous
	FilesystemOutputRuntimeDraining
	FilesystemOutputRuntimeClosed
	FilesystemOutputRuntimeSucceeded
	FilesystemOutputRuntimeReconciled
	FilesystemOutputRuntimeCollision
	FilesystemOutputRuntimeNoChange
	FilesystemOutputRuntimeNeedsAttention
	FilesystemOutputRuntimeIsolatedFailure
	FilesystemOutputRuntimeCleanupPending
)

// FilesystemCheckpointDecision is the closed, privacy-safe result of resolving
// one local checkpoint lineage. It intentionally exposes no path, revision, or
// owned-object identity.
type FilesystemCheckpointDecision uint8

const (
	FilesystemCheckpointAbsent FilesystemCheckpointDecision = iota + 1
	FilesystemCheckpointExact
	FilesystemCheckpointRevisionConflict
	FilesystemCheckpointOwnershipConflict
	FilesystemCheckpointInvalid
)

type FilesystemOutputNativeLockScope uint8

const FilesystemOutputNativeLockSession FilesystemOutputNativeLockScope = 1

type FilesystemOutputNativeLockMilestone uint8

const (
	FilesystemOutputNativeLockAcquired FilesystemOutputNativeLockMilestone = iota + 1
	FilesystemOutputNativeLockContended
	FilesystemOutputNativeLockAcquireFailed
	FilesystemOutputNativeLockReleased
	FilesystemOutputNativeLockReleaseReportedFailure
)

// FilesystemOutputTrace projects only milestones emitted by the native
// output-session graph. Durable recovery authority remains inside checkpointstore
// and resumeauthority rather than leaking through telemetry values.
type FilesystemOutputTrace struct {
	Operation              FilesystemOutputTraceOperation
	ReceiveIntentDigest    transfer.ReceiveIntentDigest
	ReceiveOperationID     receivecontract.OperationID
	SessionID              transfer.OutputSessionID
	Certification          FilesystemOutputCertificationID
	NativeLockScope        FilesystemOutputNativeLockScope
	NativeLockMilestone    FilesystemOutputNativeLockMilestone
	RootOpenDisposition    FilesystemOutputRootDisposition
	RuntimeComponent       FilesystemOutputRuntimeComponent
	RuntimeOperation       FilesystemOutputRuntimeOperation
	RuntimeDecision        FilesystemOutputRuntimeDecision
	CheckpointDecision     FilesystemCheckpointDecision
	OperationID            uint64
	ClaimID                uint64
	FaultDomain            uint8
	NormalizedFaultScope   uint8
	NormalizedFaultCode    uint16
	NodeClaimCount         uint64
	DirectoryClaimCount    uint64
	FileClaimCount         uint64
	ActiveFileClaimCount   uint64
	ReservedFileSlotCount  uint64
	DirectoryMetadataBytes uint64
	CheckpointRecordCount  uint64
	FailureStage           FilesystemOutputFailureStage
	ReconciliationStep     FilesystemCheckpointReconciliationStep
	NativeErrorClass       FilesystemNativeErrorClass
	Failed                 bool
}

type FilesystemOutputTracer interface {
	TraceFilesystemOutput(FilesystemOutputTrace)
}

type FilesystemOutputTraceFunc func(FilesystemOutputTrace)

func (function FilesystemOutputTraceFunc) TraceFilesystemOutput(event FilesystemOutputTrace) {
	if function != nil {
		function(event)
	}
}

type FilesystemOutputAuthorityConfig struct {
	RootPath   string
	CreateRoot bool
	Tracer     FilesystemOutputTracer
}

type FilesystemOutputAuthority struct {
	authority *outputruntime.Authority
}

func NewFilesystemOutputAuthority(config FilesystemOutputAuthorityConfig) (*FilesystemOutputAuthority, error) {
	runtimeAuthority, err := newOutputRuntime(config.RootPath, config.CreateRoot, config.Tracer)
	if err != nil {
		return nil, err
	}
	return &FilesystemOutputAuthority{authority: runtimeAuthority}, nil
}

type FilesystemOutputExecutionMode struct {
	mode outputruntime.ExecutionMode
}

func (mode FilesystemOutputExecutionMode) Resumable() bool { return mode.mode.Resumable() }
func (mode FilesystemOutputExecutionMode) LiveOnly() bool  { return mode.mode.LiveOnly() }
func (mode FilesystemOutputExecutionMode) Valid() bool     { return mode.mode.Valid() }

type FilesystemOutputLookupKind uint8

const (
	FilesystemOutputLookupMiss FilesystemOutputLookupKind = iota + 1
	FilesystemOutputLookupReopened
	FilesystemOutputLookupAlreadyRunning
	FilesystemOutputLookupNeedsAttention
	FilesystemOutputLookupAmbiguous
)

type FilesystemOutputLookup struct {
	lookup outputruntime.ActiveLookup
}

func (lookup FilesystemOutputLookup) Kind() FilesystemOutputLookupKind {
	switch lookup.lookup.Kind() {
	case outputruntime.ActiveLookupMiss:
		return FilesystemOutputLookupMiss
	case outputruntime.ActiveLookupReopened:
		return FilesystemOutputLookupReopened
	case outputruntime.ActiveLookupAlreadyRunning:
		return FilesystemOutputLookupAlreadyRunning
	case outputruntime.ActiveLookupNeedsAttention:
		return FilesystemOutputLookupNeedsAttention
	case outputruntime.ActiveLookupAmbiguous:
		return FilesystemOutputLookupAmbiguous
	default:
		return 0
	}
}

func (lookup FilesystemOutputLookup) StateReason() FilesystemOutputStateReason {
	return projectFilesystemOutputStateReason(lookup.lookup.StateReason())
}

func (lookup FilesystemOutputLookup) Operation() FilesystemOutputOperation {
	return FilesystemOutputOperation{operation: lookup.lookup.Operation()}
}

type FilesystemOutputOperation struct {
	operation *outputruntime.Operation
}

func (operation FilesystemOutputOperation) ReceiveIntent() (transfer.ReceiveIntent, bool) {
	if operation.operation == nil {
		return transfer.ReceiveIntent{}, false
	}
	return operation.operation.ReceiveIntent()
}

func (operation FilesystemOutputOperation) ExecutionMode() FilesystemOutputExecutionMode {
	if operation.operation == nil {
		return FilesystemOutputExecutionMode{}
	}
	return FilesystemOutputExecutionMode{mode: operation.operation.ExecutionMode()}
}

type NativeDirectTreeReservationKind uint8

const (
	NativeDirectTreeReserved NativeDirectTreeReservationKind = iota + 1
	NativeDirectTreeReopened
	NativeDirectTreeNeedsAttention
)

// NativeDirectTreeReservation reports whether the authority created or reopened
// one exact receive intent. Needs-attention deliberately carries no intent so
// callers cannot accidentally merge ambiguous destination ownership.
type NativeDirectTreeReservation struct {
	reservation outputruntime.NativeDirectTreeReservation
}

func (reservation NativeDirectTreeReservation) Kind() NativeDirectTreeReservationKind {
	switch reservation.reservation.Kind() {
	case outputruntime.NativeDirectTreeReserved:
		return NativeDirectTreeReserved
	case outputruntime.NativeDirectTreeReopened:
		return NativeDirectTreeReopened
	case outputruntime.NativeDirectTreeNeedsAttention:
		return NativeDirectTreeNeedsAttention
	default:
		return 0
	}
}

func (reservation NativeDirectTreeReservation) ReceiveIntent() (transfer.ReceiveIntent, bool) {
	return reservation.reservation.ReceiveIntent()
}

func (authority *FilesystemOutputAuthority) BindDestination(
	ctx context.Context,
) (FilesystemOutputExecutionMode, error) {
	if authority == nil || authority.authority == nil {
		return FilesystemOutputExecutionMode{}, transfer.ErrInvalidOutputBinding
	}
	mode, err := authority.authority.BindDestination(ctx)
	return FilesystemOutputExecutionMode{mode: mode}, err
}

func (authority *FilesystemOutputAuthority) LookupActive(
	ctx context.Context,
	selection transfer.SelectionSpec,
) (FilesystemOutputLookup, error) {
	if authority == nil || authority.authority == nil {
		return FilesystemOutputLookup{}, transfer.ErrInvalidOutputBinding
	}
	lookup, err := authority.authority.LookupActive(ctx, selection)
	return FilesystemOutputLookup{lookup: lookup}, outputruntime.DiagnoseFilesystemOutputFailure(
		outputruntime.FilesystemOutputFailureActiveLookup, err,
	)
}

func (authority *FilesystemOutputAuthority) CreateOperation(
	ctx context.Context,
	lookup FilesystemOutputLookup,
	artifact receivecontract.ArtifactSpec,
) (FilesystemOutputOperation, error) {
	if authority == nil || authority.authority == nil {
		return FilesystemOutputOperation{}, transfer.ErrInvalidOutputBinding
	}
	operation, err := authority.authority.CreateOperation(ctx, lookup.lookup, artifact)
	return FilesystemOutputOperation{operation: operation}, outputruntime.DiagnoseFilesystemOutputFailure(
		outputruntime.FilesystemOutputFailureOperationAdmission, err,
	)
}

func (authority *FilesystemOutputAuthority) OpenOperation(
	ctx context.Context,
	operation FilesystemOutputOperation,
) (transfer.DirectTreeSession, error) {
	if authority == nil || authority.authority == nil {
		return nil, transfer.ErrInvalidOutputBinding
	}
	session, err := authority.authority.OpenOperation(ctx, operation.operation)
	return session, outputruntime.DiagnoseFilesystemOutputFailure(
		outputruntime.FilesystemOutputFailureOperationAcquisition, err,
	)
}

func (authority *FilesystemOutputAuthority) Close() error {
	if authority == nil || authority.authority == nil {
		return nil
	}
	return authority.authority.Close()
}

func (authority *FilesystemOutputAuthority) ReserveDirectTree(
	ctx context.Context,
	selection transfer.SelectionSpec,
	artifact receivecontract.ArtifactSpec,
) (NativeDirectTreeReservation, error) {
	if authority == nil || authority.authority == nil {
		return NativeDirectTreeReservation{}, transfer.ErrInvalidOutputBinding
	}
	reservation, err := authority.authority.ReserveDirectTree(ctx, selection, artifact)
	return NativeDirectTreeReservation{reservation: reservation}, err
}

func (authority *FilesystemOutputAuthority) OpenDirectTree(
	ctx context.Context,
	intent transfer.ReceiveIntent,
) (transfer.DirectTreeSession, error) {
	if authority == nil || authority.authority == nil {
		return nil, transfer.ErrInvalidOutputBinding
	}
	return authority.authority.OpenDirectTree(ctx, intent)
}

func newOutputRuntime(rootPath string, createRoot bool, tracer FilesystemOutputTracer) (*outputruntime.Authority, error) {
	var projected outputruntime.FilesystemOutputTracer
	if tracer != nil {
		projected = outputRuntimeTracer{target: tracer}
	}
	return outputruntime.New(outputruntime.Config{
		RootPath: rootPath, CreateRoot: createRoot, Tracer: projected,
		PlatformFactory: openNativeOutputPlatform,
	})
}
