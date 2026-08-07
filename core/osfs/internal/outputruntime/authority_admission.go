package outputruntime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointcleaner"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/incrementaladmission"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

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

const maxFilesystemOutputTransactions = 32

const filesystemOutputBackendID = transfer.NativeFilesystemOutputBackendID

type outputSessionIDGenerator interface {
	NewOutputSessionID() (transfer.OutputSessionID, error)
}

type outputObjectIDGenerator interface {
	NewOutputObjectID() (resumestate.OutputObjectID, error)
}

// PlatformFactory is the build-tagged native boundary supplied by osfs. The
// portable state machine never imports a platform backend, keeping dependencies
// directed from the public facade toward runtime policy and native capability ports.
type PlatformFactory func(string, bool) (outputcap.Platform, error)

type CheckpointCleanupFunc func(
	context.Context,
	checkpointcleaner.OneShotCheckpointCleanerConfig,
) (checkpointcleaner.CheckpointCleanupReport, error)

type Config struct {
	RootPath          string
	CreateRoot        bool
	Tracer            FilesystemOutputTracer
	PlatformFactory   PlatformFactory
	CheckpointCleanup CheckpointCleanupFunc
}

type Authority struct {
	rootPath          string
	createRoot        bool
	sessionIDs        outputSessionIDGenerator
	objectIDs         outputObjectIDGenerator
	tracer            FilesystemOutputTracer
	platformFactory   PlatformFactory
	checkpointCleanup CheckpointCleanupFunc
	random            io.Reader
}

func New(config Config) (*Authority, error) {
	cleanup := config.CheckpointCleanup
	if cleanup == nil {
		cleanup = checkpointcleaner.CleanOwnedNamespace
	}
	return &Authority{
		rootPath: config.RootPath, createRoot: config.CreateRoot,
		sessionIDs: cryptographicOutputSessionIDs{}, objectIDs: cryptographicOutputObjectIDs{},
		tracer: config.Tracer, platformFactory: config.PlatformFactory,
		checkpointCleanup: cleanup, random: rand.Reader,
	}, nil
}

type cryptographicOutputSessionIDs struct{}

func (cryptographicOutputSessionIDs) NewOutputSessionID() (transfer.OutputSessionID, error) {
	var raw [transfer.OutputSessionIdentityBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return transfer.OutputSessionID{}, err
	}
	return transfer.OutputSessionIDFromBytes(raw[:])
}

type cryptographicOutputObjectIDs struct{}

func (cryptographicOutputObjectIDs) NewOutputObjectID() (resumestate.OutputObjectID, error) {
	return resumestate.NewOutputObjectID()
}

func (authority *Authority) trace(event FilesystemOutputTrace) {
	if authority != nil && authority.tracer != nil {
		authority.tracer.TraceFilesystemOutput(event)
	}
}

type outputSelectionAdmission struct {
	selection    transfer.OutputSelection
	intentDigest transfer.TransferIntentDigest
	files        map[string]transfer.OutputSelectionFile
	dirs         map[string]transfer.OutputSelectionDirectory
	// incremental is a distinct runtime authority variant, not a flag layered on
	// the frozen selection. It carries the live generation/file ledger through
	// terminal reopen without pretending those claims were encoded in a durable
	// header.
	incremental *incrementalOutputAdmission
	// Session-local secret authenticates incremental directory admission without
	// becoming durable identity or leaving the output runtime.
	admissionSecret [sha256.Size]byte
	ancestry        outputAncestrySnapshot
	validation      *outputAncestryValidation
	resuming        bool
}

// OpenOutput opens the receiver-owned root authority before catalog discovery.
// It certifies the exact picker target, probes recoverability, bootstraps (or
// validates) the control namespace, and retains the certified platform handle
// for the lazy per-generation session. No user directory is materialized here;
// only the backend's reserved control namespace may be installed.
func (authority *Authority) OpenOutput(
	ctx context.Context,
	intent transfer.TransferIntent,
) (transfer.OutputSession, error) {
	if authority == nil || intent.IsZero() || authority.platformFactory == nil || authority.rootPath == "" ||
		authority.sessionIDs == nil || authority.objectIDs == nil || authority.random == nil {
		return nil, transfer.ErrInvalidTransferIntent
	}
	target := intent.OutputTarget()
	if target.Kind() != transfer.OutputFilesystemRootTarget || target.RootPath() == "" ||
		!filepath.IsAbs(target.RootPath()) || filepath.Clean(target.RootPath()) != filepath.Clean(authority.rootPath) {
		return nil, transfer.ErrInvalidOutputBinding
	}
	if intent.BackendID() != filesystemOutputBackendID || intent.Format() != transfer.OutputNativeTree {
		return nil, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	platform, err := authority.platformFactory(authority.rootPath, authority.createRoot)
	if err != nil {
		return nil, outputnamespace.RootFault("certify output filesystem", err)
	}
	if platform == nil || platform.Root() == nil {
		if platform != nil {
			_ = platform.Close()
		}
		return nil, outputnamespace.RootFault("certify output filesystem", transfer.ErrInvalidOutputBinding)
	}
	platformOwned := true
	defer func() {
		if platformOwned {
			_ = platform.Close()
		}
	}()
	authority.trace(FilesystemOutputTrace{
		Operation:     TraceFilesystemCertified,
		Certification: filesystemOutputCertificationFromState(platform.Certification()),
	})
	if err := validateOutputCreateAuthority(platform.Root()); err != nil {
		return nil, err
	}
	if err := platform.ProbeRecoverableFeatures(); err != nil {
		return nil, outputnamespace.RootFault("probe output filesystem", err)
	}
	authority.trace(FilesystemOutputTrace{
		Operation:     TraceFeatureProbeCompleted,
		Certification: filesystemOutputCertificationFromState(platform.Certification()),
	})
	rootBinding, err := platform.RootBinding()
	if err != nil || rootBinding.IsZero() {
		if err == nil {
			err = transfer.ErrInvalidOutputBinding
		}
		return nil, outputnamespace.RootFault("bind output root", err)
	}
	capabilities, capErr := transfer.NewOutputCapabilities(transfer.OutputCapabilities{
		Durability: transfer.DurabilityProcessRestart, Mode: transfer.OutputNativeTree,
		RandomWrite: true, FileFailureIsolation: true, ModifiedTime: true,
		ArchiveBoundary: transfer.ArchiveFailureNotApplicable,
	})
	if capErr != nil {
		return nil, capErr
	}
	// Cleanup must establish ownership before the live runtime can open its
	// checkpoint claim. Otherwise merely opening a picker target could mint the
	// proof later used to authorize destructive retirement of unrelated data.
	if err := authority.cleanCheckpointNamespace(ctx, platform, intent.Digest()); err != nil {
		return nil, err
	}
	checkpointClaim, err := checkpointstore.Open(checkpointstore.Config{
		Root: platform.Root(), BackendID: filesystemOutputBackendID,
		RootIdentity: rootBinding.Bytes(), Intent: intent.Digest(),
	})
	if err != nil {
		return nil, outputnamespace.RootFault("open FileCheckpointV1 namespace", err)
	}
	checkpointOwned := true
	defer func() {
		if checkpointOwned {
			_ = checkpointClaim.Close()
		}
	}()
	sessionID, err := authority.sessionIDs.NewOutputSessionID()
	if err != nil {
		return nil, err
	}
	secret, err := incrementaladmission.NewSecret(authority.random)
	if err != nil {
		return nil, err
	}
	authority.trace(FilesystemOutputTrace{
		Operation:     TraceControlBootstrap,
		SessionID:     sessionID,
		Certification: filesystemOutputCertificationFromState(platform.Certification()),
	})
	platformOwned = false
	checkpointOwned = false
	return &incrementalOutputSession{
		authority: authority, intent: intent, backend: filesystemOutputBackendID,
		sessionID: sessionID, capabilities: capabilities, platform: platform,
		rootBinding: rootBinding, secret: secret, checkpoint: checkpointClaim,
		directories: make(map[string]incrementalDirectoryRecord),
		byID:        make(map[catalog.DirectoryID]string),
		files:       make(map[string]incrementalFileAdmission),
	}, nil
}

func (authority *Authority) cleanCheckpointNamespace(
	ctx context.Context,
	platform outputcap.Platform,
	intentDigest transfer.TransferIntentDigest,
) error {
	if authority == nil || authority.checkpointCleanup == nil || platform == nil {
		return transfer.ErrInvalidOutputBinding
	}
	report, err := authority.checkpointCleanup(ctx, checkpointcleaner.OneShotCheckpointCleanerConfig{
		Platform: platform, BackendID: filesystemOutputBackendID,
	})
	failed := err != nil || report.Status != checkpointcleaner.CheckpointCleanupStatusComplete ||
		!report.Complete || report.NeedsAttention()
	authority.trace(FilesystemOutputTrace{
		Operation: TraceCheckpointCleanup, IntentDigest: intentDigest,
		CleanupRemoved: report.Removed, CleanupQuarantined: report.Quarantined,
		CleanupSkipped: report.Skipped, Failed: failed,
	})
	if err != nil {
		return outputnamespace.RootFault("clean retired output namespace", err)
	}
	if failed {
		return outputnamespace.RootFault(
			"clean retired output namespace",
			fmt.Errorf(
				"%w: status=%d complete=%t attention=%q",
				checkpointcleaner.ErrCheckpointCleanerOwnership,
				report.Status,
				report.Complete,
				report.Attention,
			),
		)
	}
	return nil
}

func frozenSelectionAdmissionFault(operation string, cause error, requiresPause bool) error {
	fault := outputfault.New(
		transfer.OutputFaultSession,
		transfer.OutputFaultNamespaceUnsafe,
		fmt.Errorf("%s: %w", operation, cause),
	)
	if !requiresPause {
		return fault
	}
	return transfer.NewOutputSessionError(fault, true)
}

func (authority *Authority) closeOutputAdmissionAncestry(
	admission *outputSelectionAdmission,
) error {
	if admission == nil || admission.validation == nil {
		return nil
	}
	err := admission.validation.Close()
	admission.validation = nil
	if err == nil {
		return nil
	}
	authority.traceOutputAncestry(
		admission.selection, transfer.OutputSessionID{}, resumestate.LocatorDigest{}, admission.ancestry,
		len(admission.ancestry.entries), outputAncestryAdmissionBoundary(admission.resuming),
		outputAncestryTraceDecision(errors.Join(errOutputAncestryUnsafe, err)),
	)
	return outputAncestryPauseFault("close output ancestry admission guard", err)
}

func acquireOutputPublicOperationGuard(platform outputcap.Platform) (outputcap.PublicOperationGuard, error) {
	guard, err := platform.AcquirePublicOperationGuard()
	if err != nil {
		return nil, err
	}
	if guard == nil {
		return nil, errors.New("output platform returned a nil public operation guard")
	}
	return guard, nil
}
