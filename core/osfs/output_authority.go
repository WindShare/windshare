package osfs

import (
	"context"

	"github.com/windshare/windshare/core/osfs/internal/outputruntime"
	"github.com/windshare/windshare/core/transfer"
)

type FilesystemResumeRoot struct {
	RootPath string
}

type FilesystemOutputTraceOperation uint8

const (
	TraceFilesystemCertified FilesystemOutputTraceOperation = iota + 1
	TraceFeatureProbeCompleted
	TraceCheckpointCleanup
	TraceControlBootstrap
	TraceNativeLock
	TraceSessionOpened
	TraceFilePhaseTransition
	TraceFileRecoveryDecision
	TraceFileSettlement
	TraceSessionSettlement
	TraceStateInstallCutAdopted
	TraceAncestryValidation
)

type FilesystemOutputFileSettlementBoundary uint8

const (
	FilesystemOutputSettlementBeginFile FilesystemOutputFileSettlementBoundary = iota + 1
	FilesystemOutputSettlementCommit
	FilesystemOutputSettlementPause
	FilesystemOutputSettlementJobPause
	FilesystemOutputSettlementBeginFileCleanup
	FilesystemOutputSettlementRetire
)

type FilesystemOutputNativeLockScope uint8

const (
	FilesystemOutputNativeLockCoordinator FilesystemOutputNativeLockScope = iota + 1
	FilesystemOutputNativeLockSession
)

type FilesystemOutputNativeLockMilestone uint8

const (
	FilesystemOutputNativeLockAcquired FilesystemOutputNativeLockMilestone = iota + 1
	FilesystemOutputNativeLockContended
	FilesystemOutputNativeLockAcquireFailed
	FilesystemOutputNativeLockReleased
	FilesystemOutputNativeLockReleaseReportedFailure
)

type FilesystemOutputAncestryBoundary uint8

const (
	FilesystemOutputAncestryAdmission FilesystemOutputAncestryBoundary = iota + 1
	FilesystemOutputAncestryRestart
	FilesystemOutputAncestryBeginFile
	FilesystemOutputAncestryRecovery
	FilesystemOutputAncestryPublicationPre
	FilesystemOutputAncestryPublicationPost
	FilesystemOutputAncestryDirectoryFinalize
	FilesystemOutputAncestrySessionFinalize
)

type FilesystemOutputAncestryDecision uint8

const (
	FilesystemOutputAncestryPrepared FilesystemOutputAncestryDecision = iota + 1
	FilesystemOutputAncestryMatched
	FilesystemOutputAncestryMismatch
	FilesystemOutputAncestryAuthorityDenied
	FilesystemOutputAncestryStructuralUnsafe
)

type FilesystemOutputStateInstallStage uint8

const (
	FilesystemOutputStateCreate FilesystemOutputStateInstallStage = iota + 1
	FilesystemOutputStateReplace
)

type FilesystemOutputTrace struct {
	Operation                 FilesystemOutputTraceOperation
	IntentDigest              transfer.TransferIntentDigest
	SessionID                 transfer.OutputSessionID
	LocatorDigest             transfer.OutputLocatorDigest
	OutputObjectID            transfer.OutputObjectIdentity
	PreviousPhase             FilesystemOutputFilePhase
	NextPhase                 FilesystemOutputFilePhase
	RecoveryAction            FilesystemOutputRecoveryAction
	FileSettlement            transfer.FileSettlementKind
	FileSettlementBoundary    FilesystemOutputFileSettlementBoundary
	FilePauseReason           transfer.FilePauseReason
	FileRetireReason          transfer.FileRetireReason
	QuarantineReason          transfer.QuarantineReason
	JobSettlement             transfer.JobSettlementKind
	FailureScope              transfer.OutputFaultScope
	FailureCode               transfer.OutputFaultCode
	Certification             FilesystemOutputCertificationID
	StateGeneration           uint64
	StateInstallStage         FilesystemOutputStateInstallStage
	SelectionIdentity         transfer.SelectionIdentity
	OutputAncestryDigest      FilesystemOutputAncestryDigest
	AncestryBoundary          FilesystemOutputAncestryBoundary
	AncestryDecision          FilesystemOutputAncestryDecision
	AncestryClaimCount        uint32
	NativeLockScope           FilesystemOutputNativeLockScope
	NativeLockMilestone       FilesystemOutputNativeLockMilestone
	MutationReportedFailure   bool
	ParentSyncReportedFailure bool
	CleanupRemoved            uint64
	CleanupQuarantined        uint64
	CleanupSkipped            uint64
	Failed                    bool
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
