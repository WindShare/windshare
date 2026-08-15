package osfs

import (
	"context"

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
	return FilesystemOutputLookup{lookup: lookup}, err
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
	return FilesystemOutputOperation{operation: operation}, err
}

func (authority *FilesystemOutputAuthority) OpenOperation(
	ctx context.Context,
	operation FilesystemOutputOperation,
) (transfer.DirectTreeSession, error) {
	if authority == nil || authority.authority == nil {
		return nil, transfer.ErrInvalidOutputBinding
	}
	return authority.authority.OpenOperation(ctx, operation.operation)
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
