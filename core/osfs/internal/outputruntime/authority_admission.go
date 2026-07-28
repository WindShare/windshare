package outputruntime

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"

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

const maxFilesystemOutputTransactions = 32

const filesystemOutputBackendName = "windshare/native-output/v3"

var filesystemOutputBackendID = func() transfer.OutputBackendID {
	backend, err := transfer.NewOutputBackendID(filesystemOutputBackendName)
	if err != nil {
		panic(err)
	}
	return backend
}()

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

type Config struct {
	RootPath        string
	CreateRoot      bool
	Tracer          FilesystemOutputTracer
	PlatformFactory PlatformFactory
}

type Authority struct {
	rootPath        string
	createRoot      bool
	sessionIDs      outputSessionIDGenerator
	objectIDs       outputObjectIDGenerator
	tracer          FilesystemOutputTracer
	platformFactory PlatformFactory
	random          io.Reader
}

func New(config Config) (*Authority, error) {
	return &Authority{
		rootPath: config.RootPath, createRoot: config.CreateRoot,
		sessionIDs: cryptographicOutputSessionIDs{}, objectIDs: cryptographicOutputObjectIDs{},
		tracer: config.Tracer, platformFactory: config.PlatformFactory, random: rand.Reader,
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
	selection  transfer.OutputSelection
	files      map[string]transfer.OutputSelectionFile
	dirs       map[string]transfer.OutputSelectionDirectory
	ancestry   outputAncestrySnapshot
	validation *outputAncestryValidation
	resuming   bool
}

// OpenSelection is the post-discovery authority boundary used by transfer.
// Keeping the output-root policy on this object prevents the transfer job from
// constructing filesystem state before the complete canonical selection exists.
func (authority *Authority) OpenSelection(
	ctx context.Context,
	selection transfer.OutputSelection,
) (transfer.OutputSession, error) {
	session, err := authority.openSelection(ctx, selection)
	if err != nil {
		return nil, err
	}
	return session, nil
}

func (authority *Authority) openSelection(
	ctx context.Context,
	requested transfer.OutputSelection,
) (*Session, error) {
	if authority == nil || authority.platformFactory == nil || authority.sessionIDs == nil || authority.objectIDs == nil ||
		authority.rootPath == "" {
		return nil, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	canonical := requested.CanonicalSelection()
	selection, err := canonical.BindPlan(requested)
	if err != nil || selection.ResumeIntent().IsZero() ||
		selection.ResumeIntent() != canonical.ResumeIntent() {
		return nil, errors.Join(transfer.ErrInvalidOutputSelection, err)
	}
	platform, err := authority.platformFactory(authority.rootPath, authority.createRoot)
	if err != nil {
		return nil, outputnamespace.RootFault("certify output filesystem", err)
	}
	platformOwned := true
	defer func() {
		if platformOwned {
			_ = platform.Close()
		}
	}()
	authority.trace(FilesystemOutputTrace{
		Operation: TraceFilesystemCertified, ResumeIntent: selection.ResumeIntent(),
		Certification: filesystemOutputCertificationFromState(platform.Certification()),
	})
	if err := validateReservedOutputSelection(platform, selection); err != nil {
		return nil, classifyFrozenSelectionFault(platform, selection, err)
	}
	admission, err := preflightOutputSelectionAdmission(platform, selection)
	if err != nil {
		return nil, classifyFrozenSelectionFault(platform, selection, err)
	}
	if err := preflightOutputSelectionParents(platform, selection); err != nil {
		return nil, classifyFrozenSelectionFault(
			platform,
			selection,
			frozenSelectionAdmissionFault("preflight selected output parents", err, false),
		)
	}
	if err := validateOutputCreateAuthority(platform.Root()); err != nil {
		return nil, outputnamespace.RootFault("validate output root mutation authority", err)
	}
	if err := preflightOutputSelectionAuthorities(platform, selection); err != nil {
		return nil, classifyFrozenSelectionFault(
			platform,
			selection,
			frozenSelectionAdmissionFault("validate selected output mutation authority", err, false),
		)
	}
	if err := platform.ProbeRecoverableFeatures(); err != nil {
		authority.trace(FilesystemOutputTrace{
			Operation: TraceFeatureProbeCompleted, ResumeIntent: selection.ResumeIntent(),
			Certification: filesystemOutputCertificationFromState(platform.Certification()), Failed: true,
		})
		return nil, outputnamespace.RootFault("probe output filesystem", err)
	}
	authority.trace(FilesystemOutputTrace{
		Operation: TraceFeatureProbeCompleted, ResumeIntent: selection.ResumeIntent(),
		Certification: filesystemOutputCertificationFromState(platform.Certification()),
	})
	if err := platform.ValidateSelectionMetadata(selection); err != nil {
		return nil, classifyFrozenSelectionFault(
			platform,
			selection,
			frozenSelectionAdmissionFault("validate selected output metadata representation", err, false),
		)
	}
	resumeIntentPresent, err := matchingOutputResumeIntentExists(platform, selection.ResumeIntent())
	if err != nil {
		return nil, err
	}
	var ancestryValidation *outputAncestryValidation
	admission.resuming = resumeIntentPresent
	ancestryBoundary := outputAncestryAdmissionBoundary(resumeIntentPresent)
	// NTFS exposes persistent Object IDs through CREATE_OR_GET only. Preparing
	// every freshly opened ancestry authority is therefore also the restart read:
	// an existing ID is reused, while a replacement may acquire only invisible
	// Object-ID/USN metadata before the header binding rejects it. A resume never
	// materializes missing user directories or writes WindShare state/content here.
	if resumeIntentPresent {
		ancestryValidation, err = prepareOutputSelectionAncestry(platform, selection)
	} else {
		ancestryValidation, err = prepareFreshOutputSelectionAncestry(platform, selection)
	}
	if err != nil {
		claimCount := 0
		if paths, pathErr := canonicalOutputAncestryPaths(selection); pathErr == nil {
			claimCount = len(paths)
		}
		authority.traceOutputAncestry(
			selection, transfer.OutputSessionID{}, resumestate.LocatorDigest{}, outputAncestrySnapshot{},
			claimCount, ancestryBoundary, outputAncestryTraceDecision(err),
		)
		return nil, outputAncestrySessionFault(
			"capture output ancestry", err, resumeIntentPresent,
		)
	}
	admission.ancestry = ancestryValidation.snapshot
	admission.validation = ancestryValidation
	authority.traceOutputAncestry(
		selection, transfer.OutputSessionID{}, resumestate.LocatorDigest{}, admission.ancestry,
		len(admission.ancestry.entries), ancestryBoundary, FilesystemOutputAncestryPrepared,
	)
	controlResult, err := authority.namespaceController().OpenOrBootstrapControl(platform)
	if err != nil {
		return nil, errors.Join(err, authority.closeOutputAdmissionAncestry(&admission))
	}
	control := controlResult.Namespace
	authority.trace(FilesystemOutputTrace{
		Operation: TraceControlBootstrap, ResumeIntent: selection.ResumeIntent(),
		Certification: filesystemOutputCertificationFromState(platform.Certification()),
	})
	session, _, _, err := authority.openOutputSession(ctx, platform, control, admission)
	if err != nil {
		_ = control.Close()
		return nil, errors.Join(err, authority.closeOutputAdmissionAncestry(&admission))
	}
	platformOwned = false
	if err := authority.closeOutputAdmissionAncestry(&admission); err != nil {
		return nil, errors.Join(err, session.closeHandles())
	}
	return session, nil
}

func matchingOutputResumeIntentExists(
	platform outputcap.Platform,
	intent transfer.ResumeIntent,
) (present bool, resultErr error) {
	if platform == nil || intent.IsZero() {
		return false, transfer.ErrInvalidOutputSelection
	}
	controller := outputnamespace.NewController(outputnamespace.ControllerConfig{Backend: filesystemOutputBackendID})
	control, err := controller.OpenInstalledControl(platform.Root(), platform)
	if isMissing(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := control.Close(); closeErr != nil {
			present = false
			resultErr = errors.Join(
				resultErr,
				outputfault.New(transfer.OutputFaultRoot, transfer.OutputFaultStateIO, closeErr),
			)
		}
	}()
	intentName := resumestate.ResumeNamespaceName(intent)
	kind, err := outputnamespace.ObserveExactEntry(control.Sessions(), intentName)
	if err != nil {
		return false, intentOutputFault("inspect matching resume-intent namespace", err)
	}
	if kind == outputcap.EntryAbsent {
		return false, nil
	}
	if kind != outputcap.EntryDirectory {
		return false, intentOutputFault("classify matching resume-intent namespace", outputfault.ErrIntentUnsafe)
	}
	directory, err := control.Sessions().OpenDirectory(intentName, true)
	if err != nil {
		return false, intentOutputFault("open matching resume-intent namespace", err)
	}
	if err := directory.Close(); err != nil {
		return false, outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	return true, nil
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

func classifyFrozenSelectionFault(
	platform outputcap.Platform,
	selection transfer.OutputSelection,
	fault error,
) error {
	if fault == nil || selection.ResumeIntent().IsZero() || errors.Is(fault, transfer.ErrInvalidOutputSelection) {
		return fault
	}
	resumeIntentPresent, stateErr := matchingOutputResumeIntentExists(platform, selection.ResumeIntent())
	if stateErr != nil {
		// A failed observation cannot prove this is a fresh selection. Preserve the
		// typed state/root fault and require the caller to leave any durable intent
		// untouched until its lifecycle can be established safely.
		return transfer.NewOutputSessionError(
			errors.Join(stateErr, fault),
			true,
		)
	}
	// This exact-entry observation is the lifecycle linearization point. A later
	// concurrent creator owns its own admission; it does not retroactively turn
	// this already-fresh rejection into a paused durable session.
	if !resumeIntentPresent {
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
