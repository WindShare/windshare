package osfs

import (
	"context"

	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/outputruntime"
	"github.com/windshare/windshare/core/osfs/internal/pathfailure"
	"github.com/windshare/windshare/core/transfer"
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
	FilesystemOutputRuntimeOpenOutput FilesystemOutputRuntimeOperation = iota + 1
	FilesystemOutputRuntimeAcquireIntentLease
	FilesystemOutputRuntimeReconcileCheckpoints
	FilesystemOutputRuntimeAdmitDirectory
	FilesystemOutputRuntimeFinalizeDirectory
	FilesystemOutputRuntimeBeginFile
	FilesystemOutputRuntimeWriteRange
	FilesystemOutputRuntimeCheckpointFile
	FilesystemOutputRuntimeCommitFile
	FilesystemOutputRuntimePauseFile
	FilesystemOutputRuntimeRetireFile
	FilesystemOutputRuntimePauseJob
	FilesystemOutputRuntimeCompleteJob
	FilesystemOutputRuntimeMaterializeDirectory
	FilesystemOutputRuntimeCreateOwnedFile
	FilesystemOutputRuntimeRecoverFile
	FilesystemOutputRuntimePublishFile
	FilesystemOutputRuntimeQuarantineFile
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
	IntentDigest           transfer.TransferIntentDigest
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

func (authority *FilesystemOutputAuthority) OpenOutput(
	ctx context.Context,
	intent transfer.TransferIntent,
) (transfer.OutputSession, error) {
	if authority == nil || authority.authority == nil {
		return nil, transfer.ErrInvalidOutputBinding
	}
	return authority.authority.OpenOutput(ctx, intent)
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
