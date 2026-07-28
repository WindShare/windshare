package osfs

import (
	"context"

	"github.com/windshare/windshare/core/osfs/internal/outputruntime"
	"github.com/windshare/windshare/core/transfer"
)

type FilesystemResumeRoot struct {
	RootPath string
}

type ResumeAttentionScope uint8

const (
	ResumeAttentionFile ResumeAttentionScope = iota + 1
	ResumeAttentionIntent
	ResumeAttentionRoot
	ResumeAttentionLegacy
)

type ResumeAttention struct {
	Scope  ResumeAttentionScope
	Code   string
	State  string
	Detail string
}

type ResumeStateKind uint8

const (
	ResumeStateRecoverable ResumeStateKind = iota + 1
	ResumeStateNeedsAttention
	ResumeStateLegacyUntrusted
	ResumeStateOpaqueUnsafe
)

// ResumeStateRef is an opaque, inventory-scoped authority token. The runtime
// value is intentionally wrapped instead of aliased so native pins and private
// namespace identities cannot escape the osfs facade.
type ResumeStateRef struct {
	authority outputruntime.ResumeStateRef
}

func (reference ResumeStateRef) ResumeIntent() transfer.ResumeIntent {
	return reference.authority.ResumeIntent()
}

func (reference ResumeStateRef) SessionID() transfer.OutputSessionID {
	return reference.authority.SessionID()
}

func (reference ResumeStateRef) Kind() ResumeStateKind {
	switch reference.authority.Kind() {
	case outputruntime.ResumeStateRecoverable:
		return ResumeStateRecoverable
	case outputruntime.ResumeStateNeedsAttention:
		return ResumeStateNeedsAttention
	case outputruntime.ResumeStateLegacyUntrusted:
		return ResumeStateLegacyUntrusted
	case outputruntime.ResumeStateOpaqueUnsafe:
		return ResumeStateOpaqueUnsafe
	default:
		return 0
	}
}

type ResumeStateSummary struct {
	Reference      ResumeStateRef
	Lifecycle      ResumeSessionLifecycle
	FileRecords    uint64
	AllocatedBytes uint64
	Attention      []ResumeAttention
}

// ResumeStateInventory owns runtime-native handles until each projected
// reference is consumed or the inventory is closed. Its zero value is inert.
type ResumeStateInventory struct {
	authority *outputruntime.ResumeStateInventory
}

func (inventory *ResumeStateInventory) Summaries() []ResumeStateSummary {
	if inventory == nil || inventory.authority == nil {
		return nil
	}
	runtimeSummaries := inventory.authority.Summaries()
	result := make([]ResumeStateSummary, len(runtimeSummaries))
	for index, summary := range runtimeSummaries {
		result[index] = ResumeStateSummary{
			Reference:   ResumeStateRef{authority: summary.Reference},
			Lifecycle:   resumeSessionLifecycleFromRuntime(summary.Lifecycle),
			FileRecords: summary.FileRecords, AllocatedBytes: summary.AllocatedBytes,
			Attention: projectResumeAttention(summary.Attention),
		}
	}
	return result
}

func (inventory *ResumeStateInventory) Close() error {
	if inventory == nil || inventory.authority == nil {
		return nil
	}
	authority := inventory.authority
	inventory.authority = nil
	return authority.Close()
}

type DiscardSettlementKind uint8

const (
	Discarded DiscardSettlementKind = iota + 1
	DiscardAlreadyAbsent
)

type DiscardSettlement struct {
	Kind         DiscardSettlementKind
	RemovedBytes uint64
}

type FilesystemOutputTraceOperation uint8

const (
	TraceFilesystemCertified FilesystemOutputTraceOperation = iota + 1
	TraceFeatureProbeCompleted
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
	ResumeIntent              transfer.ResumeIntent
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

func (authority *FilesystemOutputAuthority) OpenSelection(
	ctx context.Context,
	selection transfer.OutputSelection,
) (transfer.OutputSession, error) {
	if authority == nil || authority.authority == nil {
		return nil, transfer.ErrInvalidOutputBinding
	}
	return authority.authority.OpenSelection(ctx, selection)
}

func ListResumeState(ctx context.Context, root FilesystemResumeRoot) (*ResumeStateInventory, error) {
	authority, err := newOutputRuntime(root.RootPath, false, nil)
	if err != nil {
		return nil, err
	}
	inventory, err := authority.ListResumeState(ctx, root.RootPath)
	if err != nil {
		return nil, err
	}
	return &ResumeStateInventory{authority: inventory}, nil
}

func DiscardResumeState(ctx context.Context, reference ResumeStateRef) (DiscardSettlement, error) {
	authority, err := newOutputRuntime("", false, nil)
	if err != nil {
		return DiscardSettlement{}, err
	}
	settlement, err := authority.DiscardResumeState(ctx, reference.authority)
	if err != nil {
		return DiscardSettlement{}, err
	}
	return projectDiscardSettlement(settlement), nil
}

func newOutputRuntime(rootPath string, createRoot bool, tracer FilesystemOutputTracer) (*outputruntime.Authority, error) {
	var projected outputruntime.FilesystemOutputTracer
	if tracer != nil {
		projected = outputRuntimeTracer{target: tracer}
	}
	return outputruntime.New(outputruntime.Config{
		RootPath: rootPath, CreateRoot: createRoot, Tracer: projected,
		PlatformFactory: openOutputV3Platform,
	})
}
