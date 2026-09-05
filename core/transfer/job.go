// Package transfer coordinates receiver-scoped file-local block demand across
// authenticated protocol lanes.
package transfer

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/catalogwalk"
	"github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/core/transfer/revisionwait"
)

const (
	DefaultOutputSettlementTimeout = 30 * time.Second
	MaximumOutputSettlementTimeout = 5 * time.Minute
	DefaultTransferQueueCapacity   = 8
	MaximumTransferQueueCapacity   = 256
	DefaultGenerationReplayPages   = (catalog.MaxDirectoryEntries + catalog.MaxCatalogPageEntries - 1) / catalog.MaxCatalogPageEntries
	MaximumGenerationReplayPages   = catalog.MaxDirectoryEntries
)

var (
	ErrInvalidTransferJob      = errors.New("transfer job configuration is invalid")
	ErrTransferJobRun          = errors.New("transfer job may only run once")
	ErrCatalogIdentity         = errors.New("catalog snapshot does not match the requested share and directory")
	ErrRevisionIdentity        = errors.New("opened revision does not match the selected catalog file")
	ErrOutputContract          = errors.New("output session violated its file transaction contract")
	ErrCatalogEntriesOmitted   = errors.New("catalog directory omitted children")
	ErrCatalogCursorContract   = errors.New("catalog reader returned no page cursor")
	ErrOutputPublishBlocked    = errors.New("output publication is blocked by an existing final path")
	ErrOutputQuarantined       = errors.New("output file needs manual ownership review")
	ErrOutputRetired           = errors.New("output file retirement completed without content transfer")
	ErrGenerationReplayBudget  = errors.New("transfer generation replay page budget exceeded")
	ErrFrozenSourceDrift       = errors.New("authenticated source no longer matches the frozen output anchor")
	ErrRevisionWaitUnavailable = errors.New("authenticated revision capacity wait policy is unavailable")
	errRangeReaderContract     = errors.New("range reader violated its requested output interval")
)

type DirectTreeOutcome uint8

const (
	DirectTreeOutcomeSuccess DirectTreeOutcome = iota + 1
	DirectTreeOutcomePartial
	DirectTreeOutcomePaused
	DirectTreeOutcomeFailed
)

// TransferInterruption is the closed, command-facing reason that caller
// authority ended a job. Keeping it separate from Fault prevents an expected
// local stop from being misreported as an unknown collaborator failure.
type TransferInterruption uint8

const (
	TransferInterruptionCanceled TransferInterruption = iota + 1
	TransferInterruptionDeadline
)

func (interruption TransferInterruption) Valid() bool {
	return interruption >= TransferInterruptionCanceled && interruption <= TransferInterruptionDeadline
}

type FailureStage uint8

const (
	FailureDirectoryDiscovery FailureStage = iota + 1
	FailureOutputAdmission
	FailureDirectoryOutput
	FailureRevisionOpen
	FailureRevisionIdentity
	FailureBlockTransfer
	FailureFileOutput
	FailureLeaseRelease
)

type DirectoryJobFailure struct {
	DirectoryID catalog.DirectoryID
	Path        string
	Stage       FailureStage
	Cause       error
	// Fault is the policy value captured when the collaborator returned. Cause is
	// diagnostic only and cannot be reinterpreted as settlement authority.
	Fault fault.Fault
}

type FileJobFailure struct {
	FileID              catalog.FileID
	Path                string
	Stage               FailureStage
	Cause               error
	Fault               fault.Fault
	Settlement          FileSettlement
	SettlementFailure   error
	SettlementFault     fault.Fault
	LeaseReleaseFailure error
	LeaseReleaseFault   fault.Fault
}

// FileOutcomeSummary is the bounded command-facing projection of typed file
// settlements. It intentionally carries counts rather than paths or checkpoint
// facts so consumers can report provenance without retaining a second manifest.
type FileOutcomeSummary struct {
	DownloadedFiles         uint64
	ResumedFiles            uint64
	PausedFiles             uint64
	CollisionFiles          uint64
	FailedFiles             uint64
	ItemBlockedFiles        uint64
	RevisionConflictFiles   uint64
	CheckpointInvalidFiles  uint64
	OwnedObjectUnknownFiles uint64
	ModifiedTimeWarnings    uint64
}

func (summary FileOutcomeSummary) PublishedFiles() uint64 {
	published, _ := checkedAdd(summary.DownloadedFiles, summary.ResumedFiles)
	return published
}

type JobResult struct {
	Outcome             DirectTreeOutcome
	Settlement          DirectTreeSettlement
	TransferJobID       TransferJobID
	ReceiveIntentDigest ReceiveIntentDigest
	ReceiveIntent       ReceiveIntent
	// SelectionObservation is diagnostic only and is normally zero for the
	// incremental path, which does not materialize a whole-tree snapshot.
	SelectionObservation     SelectionObservationV1
	Progress                 ReceiveProgressSnapshot
	Directories              []DirectoryJobFailure
	Files                    []FileJobFailure
	OmittedDirectoryFailures uint64
	OmittedFileFailures      uint64
	// SelectionResolutionFailure is separate from bounded diagnostics because a
	// proven explicit miss is an authoritative job outcome, not an incidental detail.
	SelectionResolutionFailure error
	// SourceDriftFailure preserves the first semantic drift outcome even when its
	// per-file or per-directory diagnostic is omitted by the retention budget.
	SourceDriftFailure      error
	SourceDriftFault        fault.Fault
	SucceededFiles          uint64
	TerminationCause        error
	TerminationFault        fault.Fault
	TerminationInterruption TransferInterruption
	SettlementFailure       error
	SettlementFault         fault.Fault
	SettlementInterruption  TransferInterruption
}

type CatalogReader interface {
	// A cursor owns one authenticated generation and releases every page before
	// advancing. Implementations must be safe for concurrent directory cursors.
	OpenDirectoryPages(context.Context, catalog.DirectoryID) (catalog.DirectoryPageCursor, error)
}

type OpenedRevision struct {
	LeaseID    content.LeaseID
	Descriptor content.FileRevisionDescriptor
}

func NewOpenedRevision(lease content.LeaseID, descriptor content.FileRevisionDescriptor) (OpenedRevision, error) {
	if lease.IsZero() || descriptor.ShareInstance().IsZero() || descriptor.FileID().IsZero() || descriptor.FileRevision().IsZero() {
		return OpenedRevision{}, ErrRevisionIdentity
	}
	return OpenedRevision{LeaseID: lease, Descriptor: descriptor}, nil
}

type RevisionClient interface {
	OpenRevision(context.Context, catalog.FileID) (OpenedRevision, error)
	ReleaseRevision(context.Context, content.LeaseID) error
}

type RangeReader interface {
	ReadRange(context.Context, content.LeaseID, content.FileRevisionDescriptor, content.Range, RangeSink) error
}

type SessionIdentity interface {
	ProtocolSessionID() protocolsession.ProtocolSessionID
}

type TransferJobConfig struct {
	// ReceiveIntent and JobID are established before construction so a job can
	// never open a materialization namespace before destination confirmation or
	// change either stable operation identity or per-run trace identity.
	ReceiveIntent ReceiveIntent
	JobID         TransferJobID
	// Session supplies the current authenticated generation for capacity authority
	// and traces. Its lifetime may span replacements; standalone jobs leave it nil.
	Session SessionIdentity
	// FileQueueCapacity bounds discovery-to-output buffering. A full queue
	// deliberately blocks the catalog walk instead of buffering an unbounded
	// whole-tree selection.
	FileQueueCapacity int
	// GenerationReplayPages bounds the commitments retained while authenticating
	// one directory before its entries are replayed through FileQueueCapacity.
	GenerationReplayPages int
	Catalog               CatalogReader
	Revisions             RevisionClient
	Blocks                RangeReader
	Materializer          DirectTreeMaterializer
	SettlementTimeout     time.Duration
	Tracer                TransferLifecycleTracer
	// RevisionWait is required only for runtimes that can produce authenticated
	// CapacitySignal values. Keeping it explicit prevents generic quota errors
	// from silently acquiring retry authority.
	RevisionWait *revisionwait.Config
}

type TransferJob struct {
	share              catalog.ShareInstance
	root               catalog.DirectoryID
	rules              SelectionRules
	intent             ReceiveIntent
	jobID              TransferJobID
	session            SessionIdentity
	selectionSpec      SelectionSpec
	catalog            CatalogReader
	revisions          RevisionClient
	blocks             RangeReader
	outputAuthority    DirectTreeMaterializer
	settlementTimeout  time.Duration
	queueCapacity      int
	replayPageCapacity int
	catalogWalkLimits  catalogwalk.Limits
	projector          ordinaryoutput.ArtifactPathProjector
	coordinates        directTreeCoordinateProjector
	tracer             TransferLifecycleTracer
	revisionWait       *revisionwait.Coordinator
	progress           receiveProgressTracker

	mu      sync.Mutex
	traceMu sync.Mutex
	started bool
}

func NewTransferJob(config TransferJobConfig) (*TransferJob, error) {
	intent := config.ReceiveIntent
	if !intent.valid() || intent.MaterializationPlan().Kind() != receivecontract.PlanDirectTree ||
		config.JobID.IsZero() ||
		config.Catalog == nil || config.Revisions == nil || config.Blocks == nil || config.Materializer == nil ||
		config.SettlementTimeout < 0 || config.SettlementTimeout > MaximumOutputSettlementTimeout ||
		config.FileQueueCapacity < 0 || config.FileQueueCapacity > MaximumTransferQueueCapacity ||
		config.GenerationReplayPages < 0 || config.GenerationReplayPages > MaximumGenerationReplayPages {
		return nil, ErrInvalidTransferJob
	}
	timeout := config.SettlementTimeout
	if timeout == 0 {
		timeout = DefaultOutputSettlementTimeout
	}
	queueCapacity := config.FileQueueCapacity
	if queueCapacity == 0 {
		queueCapacity = DefaultTransferQueueCapacity
	}
	replayPageCapacity := config.GenerationReplayPages
	if replayPageCapacity == 0 {
		replayPageCapacity = DefaultGenerationReplayPages
	}
	selection := intent.SelectionSpec()
	if selection.IsZero() {
		return nil, ErrInvalidTransferJob
	}
	projector, err := OrdinaryOutputArtifactPathProjector(intent)
	if err != nil {
		return nil, ErrInvalidTransferJob
	}
	coordinates, err := newDirectTreeCoordinateProjector(intent)
	if err != nil || coordinates.artifact != projector {
		return nil, ErrInvalidTransferJob
	}
	walkLimits, ok := catalogwalk.NewLimits(
		uint32(replayPageCapacity),
		catalog.MaxDirectoryEntries,
		uint64(replayPageCapacity)*(catalog.CatalogPageMemoryOverhead+catalog.MaxCatalogPageObjectBytes),
	)
	if !ok {
		return nil, ErrInvalidTransferJob
	}
	var revisionWait *revisionwait.Coordinator
	if config.RevisionWait != nil {
		revisionWait, err = revisionwait.NewCoordinator(*config.RevisionWait)
		if err != nil {
			return nil, errors.Join(ErrInvalidTransferJob, err)
		}
	}
	return &TransferJob{
		share: intent.ShareInstance(), root: intent.SyntheticRoot(), rules: intent.SelectionRules(),
		intent: intent, jobID: config.JobID, session: config.Session,
		selectionSpec: selection,
		catalog:       config.Catalog, revisions: config.Revisions, blocks: config.Blocks,
		outputAuthority:   config.Materializer,
		settlementTimeout: timeout, queueCapacity: queueCapacity, replayPageCapacity: replayPageCapacity,
		catalogWalkLimits: walkLimits, projector: projector, coordinates: coordinates,
		tracer: config.Tracer, revisionWait: revisionWait, progress: newReceiveProgressTracker(),
	}, nil
}

func (j *TransferJob) Progress() ReceiveProgressSnapshot { return j.progress.snapshotValue() }

func (j *TransferJob) JobID() TransferJobID         { return j.jobID }
func (j *TransferJob) ReceiveIntent() ReceiveIntent { return j.intent }
func (j *TransferJob) ReceiveIntentDigest() ReceiveIntentDigest {
	return j.intent.Digest()
}

// ProgressSnapshots coalesces updates for prompt connection-size admission.
// Presentation and trace sampling should poll Progress at their own cadence.
func (j *TransferJob) ProgressSnapshots() <-chan ReceiveProgressSnapshot { return j.progress.Updates() }

func (j *TransferJob) Run(ctx context.Context) JobResult {
	j.mu.Lock()
	if j.started {
		j.mu.Unlock()
		failure := dependencyContractFailure(ErrTransferJobRun)
		return JobResult{Outcome: DirectTreeOutcomePaused, TransferJobID: j.jobID,
			ReceiveIntentDigest: j.intent.Digest(), ReceiveIntent: j.intent,
			Progress: j.Progress(), TerminationCause: failure,
			TerminationFault: closedLifecycleFault(failure)}
	}
	j.started = true
	j.mu.Unlock()

	runContext, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	state, err := newJobRun(j)
	if err != nil {
		failure := dependencyContractFailure(err)
		j.failUnstartedDiscovery()
		return JobResult{
			Outcome: DirectTreeOutcomePaused, TransferJobID: j.jobID,
			ReceiveIntentDigest: j.intent.Digest(), ReceiveIntent: j.intent,
			Progress: j.Progress(), TerminationCause: failure,
			TerminationFault: closedLifecycleFault(failure),
		}
	}
	if cause := context.Cause(runContext); cause != nil {
		state.terminationCause = cancellationFailure(runContext, cause)
		j.failUnstartedDiscovery()
		return state.finish(ctx)
	}
	if failure := j.admitRunOutput(runContext, state); failure != nil {
		state.terminationCause = failure
		j.failUnstartedDiscovery()
		return state.finish(ctx)
	}

	fileQueue := make(chan transferQueueItem, j.queueCapacity)
	workerErr := make(chan *lifecycleFailure, 1)
	workerDone := make(chan struct{})
	go state.transferQueueWorker(runContext, fileQueue, cancel, workerErr, workerDone)
	j.trace(TransferLifecycleTrace{
		Stage: TransferDiscoveryStarted, TransferJobID: j.jobID, ReceiveIntentDigest: j.intent.Digest(),
		Discovery: DiscoveryOpen,
	})
	discoveryFailure := admitInternalFailure(state.discoverIncremental(runContext, fileQueue))
	return state.completeDiscovery(ctx, runContext, cancel, fileQueue, workerErr, workerDone, discoveryFailure)
}

func (j *TransferJob) admitRunOutput(ctx context.Context, state *jobRun) *lifecycleFailure {
	j.trace(TransferLifecycleTrace{
		Stage: TransferAdmissionStarted, TransferJobID: j.jobID, ReceiveIntentDigest: j.intent.Digest(),
	})
	output, rawOutputErr := j.outputAuthority.OpenDirectTree(ctx, j.intent)
	failure := admitInternalFailure(normalizeOutputBoundary(ctx, rawOutputErr))
	if output != nil {
		bindingFailure := admitInternalFailure(validateDirectTreeSession(j.intent, output))
		if bindingFailure != nil {
			// A foreign session is not authority to mutate, even for cleanup or pause.
			// Prefer the binding failure over a simultaneous adapter error because the
			// returned capability cannot safely participate in any later settlement.
			failure = bindingFailure
		} else {
			// A collaborator can cross a durable mutation boundary before reporting an
			// error. Retaining only an exactly bound capability lets finish request a
			// stable pause without touching another operation's namespace.
			state.output = output
			state.admitted = true
		}
	} else if failure == nil {
		failure = admitInternalFailure(validateDirectTreeSession(j.intent, nil))
	}
	if failure == nil {
		admissionScope := j.coordinates.scope
		if !admissionScope.valid() {
			// Intent validation precedes OpenDirectTree, so failure to project its receipt
			// scope is an internal boundary violation rather than backend authority.
			failure = dependencyContractFailure(ErrInvalidDirectoryAdmission)
		} else {
			state.directoryAdmissionScope = admissionScope
		}
	}
	if failure != nil {
		j.trace(TransferLifecycleTrace{
			Stage: TransferAdmissionCompleted, TransferJobID: j.jobID,
			ReceiveIntentDigest: j.intent.Digest(), Fault: closedLifecycleFault(failure), Failed: true,
		})
		return failure
	}
	j.trace(TransferLifecycleTrace{
		Stage: TransferAdmissionCompleted, TransferJobID: j.jobID,
		ReceiveIntentDigest: j.intent.Digest(), OutputSessionID: output.SessionID(),
	})
	return nil
}

func (j *TransferJob) failUnstartedDiscovery() {
	// Consumers must observe one terminal discovery transition even when output
	// admission fails before the catalog can safely be opened.
	j.progress.failDiscovery()
	j.progress.finishDiscovery()
	j.progress.closeUpdates()
}

func (r *jobRun) completeDiscovery(
	ctx context.Context,
	runContext context.Context,
	cancel context.CancelCauseFunc,
	fileQueue chan transferQueueItem,
	workerErr <-chan *lifecycleFailure,
	workerDone <-chan struct{},
	discoveryFailure *lifecycleFailure,
) JobResult {
	workerCause := closedContextCause(runContext)
	workerInterruptedDiscovery := (workerCause != nil && (workerCause == discoveryFailure || (discoveryFailure != nil && discoveryFailure.policy.canceled))) ||
		(discoveryFailure != nil && discoveryFailure.policy.canceled)
	discoveryIncomplete := discoveryFailure != nil &&
		(!workerInterruptedDiscovery || !r.catalogTraversalComplete)
	if discoveryFailure != nil {
		if discoveryIncomplete {
			r.job.progress.failDiscovery()
		}
		r.terminationCause = discoveryFailure
		cancel(discoveryFailure)
	}
	if !discoveryIncomplete && !r.discoveryFailed {
		if missingErr := r.job.rules.missingTargetsError(
			r.matchedPaths, r.matchedDirectories, r.matchedFiles,
		); missingErr != nil {
			// Absence proven by a complete catalog is a closed selection result,
			// not a resumable transport or output failure.
			r.selectionResolutionFailure = missingErr
		}
	}
	r.job.progress.finishDiscovery()
	progress := r.job.Progress()
	r.job.trace(TransferLifecycleTrace{
		Stage: TransferDiscoveryCompleted, TransferJobID: r.job.jobID,
		ReceiveIntentDigest: r.job.intent.Digest(),
		Discovery:           progress.Discovery, ConnectionSizeClass: progress.ConnectionSizeClass(),
		Fault: fault.Join(
			discoveryCompletionFault(discoveryFailure, discoveryIncomplete), r.discoveryFaultSnapshot(),
		),
		Interruption: closedLifecycleInterruption(discoveryFailure),
		Failed:       discoveryIncomplete || r.discoveryFailed,
	})
	close(fileQueue)
	<-workerDone
	// A worker error aborts runContext, which may cause active discovery to exit
	// with a generic cancellation failure. Once the worker finishes, adopt its
	// concrete failure as the primary root cause instead of keeping the cancellation symptom.
	select {
	case workerFailure := <-workerErr:
		if r.terminationCause == nil || r.terminationCause.policy.canceled || workerInterruptedDiscovery {
			r.terminationCause = workerFailure
			cancel(workerFailure)
		} else {
			r.terminationCause = mergeLifecycleFailures(r.terminationCause, workerFailure)
		}
	default:
	}
	r.job.progress.closeUpdates()
	return r.finish(ctx)
}

func discoveryCompletionFault(
	failure *lifecycleFailure,
	incomplete bool,
) fault.Fault {
	if !incomplete {
		return fault.Fault{}
	}
	return closedLifecycleFault(failure)
}

func (r *jobRun) transferPlannedFile(ctx context.Context, plan plannedFile) error {
	opened, ready, err := r.openSelectedRevision(ctx, plan)
	if err != nil || !ready {
		return err
	}
	file, err := newMaterializationFile(
		r.job.coordinates, plan.sourcePath, plan.materializationRelativePath,
		opened.Descriptor, r.output.SessionID(), plan.parent,
	)
	if err != nil || file.ArtifactPath() != plan.artifactPath || file.ExpectedSize() != plan.expectedSize {
		return r.rejectUnstartedFile(ctx, plan, opened, dependencyContractFailure(errors.Join(ErrOutputContract, err)))
	}
	target := file.Target()
	start, err := r.output.BeginFile(ctx, file)
	err = normalizeOutputBoundary(ctx, err)
	if err != nil {
		r.traceFileLifecycle(TransferFileAdmitted, plan, err)
		releaseErr := r.releaseRevision(ctx, opened.LeaseID)
		policy := lifecyclePolicyFor(err)
		failure := FileJobFailure{
			FileID: plan.file, Path: plan.failurePath(), Stage: FailureFileOutput,
			Cause: err, LeaseReleaseFailure: releaseErr,
		}
		if policy.jobTerminal() || isJobTerminalError(releaseErr) ||
			policy.outputRequiresJobPause(r.output.Capabilities()) {
			r.recordFileFailure(failure)
			return joinLifecycleFailures(err, releaseErr)
		}
		r.recordFileFailure(failure)
		return nil
	}
	if settlement, immediate := start.ImmediateSettlement(); immediate {
		if err := validateImmediateFileSettlement(target, settlement); err != nil {
			contractFailure := outputContractFault(err)
			r.traceFileLifecycle(TransferFileAdmitted, plan, contractFailure)
			return r.rejectImmediateSettlement(ctx, plan, opened, settlement, contractFailure)
		}
		if checkpoint, ok := settlement.VerifiedCheckpoint(); ok {
			recovered, exact := rangeSetByteCount(checkpoint.Ranges())
			if !exact || recovered > plan.expectedSize {
				contractFailure := outputContractFault(ErrOutputContract)
				r.traceFileLifecycle(TransferFileAdmitted, plan, contractFailure)
				return r.rejectImmediateSettlement(ctx, plan, opened, settlement, contractFailure)
			}
			r.job.progress.addRecoveredVerified(recovered)
		}
		r.traceFileLifecycle(TransferFileAdmitted, plan, nil)
		return r.handleImmediateSettlement(ctx, plan, opened, settlement)
	}
	transaction, durable, transactional := start.Transaction()
	if !transactional || !start.valid() {
		contractFailure := outputContractFault(ErrOutputContract)
		r.traceFileLifecycle(TransferFileAdmitted, plan, contractFailure)
		return r.rejectImmediateSettlement(ctx, plan, opened, start.settlement, contractFailure)
	}
	if err := validateOutputTransaction(target, transaction, durable); err != nil {
		r.traceFileLifecycle(TransferFileAdmitted, plan, err)
		return r.settleFailedFile(ctx, plan, opened, transaction, FailureFileOutput, err, 0, nil)
	}
	fileProgress, recovered, validProgress := newFileTransferProgress(
		transaction, durable, r.output.Capabilities().Durability != DurabilityNone,
	)
	if !validProgress {
		contractFailure := outputContractFault(ErrOutputContract)
		r.traceFileLifecycle(TransferFileAdmitted, plan, contractFailure)
		return r.settleFailedFile(
			ctx, plan, opened, transaction, FailureFileOutput, contractFailure, 0, nil,
		)
	}
	r.job.progress.addRecoveredVerified(recovered)
	r.traceFileLifecycle(TransferFileAdmitted, plan, nil)
	completed, err := r.transferMissingRanges(ctx, plan, opened, transaction, &fileProgress)
	if err != nil || !completed {
		return err
	}
	return r.commitTransferredFile(ctx, plan, opened, transaction, &fileProgress)
}

func (r *jobRun) rejectUnstartedFile(
	ctx context.Context,
	plan plannedFile,
	opened OpenedRevision,
	cause error,
) error {
	releaseErr := r.releaseRevision(ctx, opened.LeaseID)
	r.recordFileFailure(FileJobFailure{
		FileID: plan.file, Path: plan.failurePath(), Stage: FailureFileOutput, Cause: cause,
		LeaseReleaseFailure: releaseErr,
	})
	return joinLifecycleFailures(cause, releaseErr)
}

func (r *jobRun) rejectImmediateSettlement(
	ctx context.Context,
	plan plannedFile,
	opened OpenedRevision,
	settlement FileSettlement,
	cause error,
) error {
	fault := cause
	releaseErr := r.releaseRevision(ctx, opened.LeaseID)
	r.settlementFailure = mergeLifecycleFailures(r.settlementFailure, fault)
	r.recordFileFailure(FileJobFailure{
		FileID: plan.file, Path: plan.failurePath(), Stage: FailureFileOutput, Cause: fault,
		Settlement: settlement, SettlementFailure: fault, LeaseReleaseFailure: releaseErr,
	})
	r.traceFileSettlement(plan, settlement, joinLifecycleFailures(fault, releaseErr))
	return fault
}

func (r *jobRun) handleImmediateSettlement(
	ctx context.Context,
	plan plannedFile,
	opened OpenedRevision,
	settlement FileSettlement,
) error {
	releaseErr := r.releaseRevision(ctx, opened.LeaseID)
	r.job.progress.acceptFileSettlement(settlement, plan.expectedSize)
	r.traceFileSettlement(plan, settlement, releaseErr)
	switch settlement.Kind() {
	case FilePublished:
		if releaseErr != nil {
			r.recordFileFailure(FileJobFailure{
				FileID: plan.file, Path: plan.failurePath(), Stage: FailureLeaseRelease,
				Cause: releaseErr,
			})
		}
		if isJobTerminalError(releaseErr) {
			return releaseErr
		}
		return nil
	case FileCollision:
		r.recordFileFailure(FileJobFailure{
			FileID: plan.file, Path: plan.failurePath(), Stage: FailureFileOutput,
			Cause: ErrOutputPublishBlocked, Settlement: settlement,
			LeaseReleaseFailure: releaseErr,
		})
	case FileItemBlocked:
		r.recordFileFailure(FileJobFailure{
			FileID: plan.file, Path: plan.failurePath(), Stage: FailureFileOutput,
			Cause: ErrOutputQuarantined, Settlement: settlement,
			LeaseReleaseFailure: releaseErr,
		})
	case FileFailed:
		// A recovered retirement is already-authorized durable cleanup. Preserve
		// its exact settlement without creating a transaction or attempting a
		// second settlement whose reason this run cannot authenticate.
		r.recordFileFailure(FileJobFailure{
			FileID: plan.file, Path: plan.failurePath(), Stage: FailureFileOutput,
			Cause: ErrOutputRetired, Settlement: settlement,
			LeaseReleaseFailure: releaseErr,
		})
	default:
		contractFailure := outputContractFault(nil)
		r.settlementFailure = mergeLifecycleFailures(r.settlementFailure, contractFailure)
		return contractFailure
	}
	if isJobTerminalError(releaseErr) {
		return releaseErr
	}
	return nil
}

func (job *TransferJob) protocolSessionID() protocolsession.ProtocolSessionID {
	if job.session == nil {
		return protocolsession.ProtocolSessionID{}
	}
	return job.session.ProtocolSessionID()
}
