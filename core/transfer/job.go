package transfer

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/protocolsession"
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
	ErrInvalidTransferJob     = errors.New("transfer job configuration is invalid")
	ErrTransferJobRun         = errors.New("transfer job may only run once")
	ErrCatalogIdentity        = errors.New("catalog snapshot does not match the requested share and directory")
	ErrRevisionIdentity       = errors.New("opened revision does not match the selected catalog file")
	ErrOutputContract         = errors.New("output session violated its file transaction contract")
	ErrCatalogEntriesOmitted  = errors.New("catalog directory omitted children")
	ErrCatalogCursorContract  = errors.New("catalog reader returned no page cursor")
	ErrOutputPublishBlocked   = errors.New("output publication is blocked by an existing final path")
	ErrOutputQuarantined      = errors.New("output file needs manual ownership review")
	ErrOutputRetired          = errors.New("output file retirement completed without content transfer")
	ErrGenerationReplayBudget = errors.New("transfer generation replay page budget exceeded")
	errRangeReaderContract    = errors.New("range reader violated its requested output interval")
)

type JobOutcome uint8

const (
	JobSucceeded JobOutcome = iota + 1
	JobCompletedWithErrors
	JobPausedOutcome
)

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
}

type FileJobFailure struct {
	FileID              catalog.FileID
	Path                string
	Stage               FailureStage
	Cause               error
	Settlement          FileSettlement
	SettlementFailure   error
	LeaseReleaseFailure error
}

type JobResult struct {
	Outcome        JobOutcome
	Settlement     JobSettlement
	TransferJobID  TransferJobID
	IntentDigest   TransferIntentDigest
	TransferIntent TransferIntent
	// SelectionObservation is diagnostic only and is normally zero for the
	// incremental path, which does not materialize a whole-tree snapshot.
	SelectionObservation SelectionObservationV1
	// SelectionIdentity is an in-memory catalog observation, never a checkpoint key.
	SelectionIdentity        SelectionIdentity
	Measure                  SelectionMeasure
	Directories              []DirectoryJobFailure
	Files                    []FileJobFailure
	OmittedDirectoryFailures uint64
	OmittedFileFailures      uint64
	// SelectionResolutionFailure is separate from bounded diagnostics because a
	// proven explicit miss is an authoritative job outcome, not an incidental detail.
	SelectionResolutionFailure error
	// SourceDriftFailure preserves the first semantic drift outcome even when its
	// per-file or per-directory diagnostic is omitted by the retention budget.
	SourceDriftFailure error
	SucceededFiles     uint64
	TerminationCause   error
	SettlementFailure  error
}

type CatalogReader interface {
	// A cursor owns one authenticated generation and releases every page before
	// advancing. Implementations must be safe for concurrent directory cursors.
	OpenDirectoryPages(context.Context, catalog.DirectoryID) (catalog.DirectoryPageCursor, error)
}

type DirectoryDiscoveryFailure interface {
	error
	DirectoryFailure()
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

type TransferJobConfig struct {
	ShareInstance catalog.ShareInstance
	SyntheticRoot catalog.DirectoryID
	Rules         SelectionRules
	// Intent and JobID are established before construction so a job can never
	// open an output namespace before picker confirmation or change its trace
	// identity while running.
	Intent TransferIntent
	JobID  TransferJobID
	// ProtocolSessionID correlates production transfer traces with the
	// authenticated session. Standalone jobs may leave it zero.
	ProtocolSessionID protocolsession.ProtocolSessionID
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
	Output                OutputAuthority
	SettlementTimeout     time.Duration
	Tracer                TransferLifecycleTracer
}

type TransferJob struct {
	share              catalog.ShareInstance
	root               catalog.DirectoryID
	rules              SelectionRules
	intent             TransferIntent
	jobID              TransferJobID
	protocolSessionID  protocolsession.ProtocolSessionID
	selectionRequest   CanonicalSelectionRequest
	catalog            CatalogReader
	revisions          RevisionClient
	blocks             RangeReader
	outputAuthority    OutputAuthority
	settlementTimeout  time.Duration
	queueCapacity      int
	replayPageCapacity int
	tracer             TransferLifecycleTracer
	tracker            selectionTracker

	mu      sync.Mutex
	traceMu sync.Mutex
	started bool
}

func NewTransferJob(config TransferJobConfig) (*TransferJob, error) {
	if config.ShareInstance.IsZero() || config.SyntheticRoot.IsZero() || !config.Rules.validSnapshot() ||
		!config.Intent.valid() || config.JobID.IsZero() ||
		config.Catalog == nil || config.Revisions == nil || config.Blocks == nil || config.Output == nil ||
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
	selectionRequest, err := NewCanonicalSelectionRequest(config.ShareInstance, config.SyntheticRoot, config.Rules)
	if err != nil {
		return nil, ErrInvalidTransferJob
	}
	intent := config.Intent
	intentRequest, intentErr := NewCanonicalSelectionRequest(intent.ShareInstance(), intent.SyntheticRoot(), intent.SelectionRules())
	if intentErr != nil || intent.ShareInstance() != config.ShareInstance || intent.SyntheticRoot() != config.SyntheticRoot ||
		!bytes.Equal(intentRequest.Bytes(), selectionRequest.Bytes()) {
		return nil, ErrInvalidTransferJob
	}
	return &TransferJob{
		share: config.ShareInstance, root: config.SyntheticRoot, rules: config.Rules,
		intent: intent, jobID: config.JobID, protocolSessionID: config.ProtocolSessionID,
		selectionRequest: selectionRequest,
		catalog:          config.Catalog, revisions: config.Revisions, blocks: config.Blocks,
		outputAuthority:   config.Output,
		settlementTimeout: timeout, queueCapacity: queueCapacity, replayPageCapacity: replayPageCapacity,
		tracer: config.Tracer, tracker: newSelectionTracker(),
	}, nil
}

func (j *TransferJob) Measure() SelectionMeasure { return j.tracker.snapshot() }

func (j *TransferJob) JobID() TransferJobID               { return j.jobID }
func (j *TransferJob) Intent() TransferIntent             { return j.intent }
func (j *TransferJob) IntentDigest() TransferIntentDigest { return j.intent.Digest() }

// SelectionMeasures publishes monotonic discovery updates. Discovery now owns
// the first job phase, so no duplicate catalog walk can race output admission.
func (j *TransferJob) SelectionMeasures() <-chan SelectionMeasure { return j.tracker.Updates() }

func (j *TransferJob) Run(ctx context.Context) JobResult {
	j.mu.Lock()
	if j.started {
		j.mu.Unlock()
		return JobResult{Outcome: JobPausedOutcome, TransferJobID: j.jobID,
			IntentDigest: j.intent.Digest(), TransferIntent: j.intent,
			Measure: j.Measure(), TerminationCause: ErrTransferJobRun}
	}
	j.started = true
	j.mu.Unlock()

	runContext, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	state, err := newJobRun(j)
	if err != nil {
		j.tracker.failDiscovery()
		j.tracker.finishDiscovery()
		j.tracker.closeUpdates()
		return JobResult{
			Outcome: JobPausedOutcome, TransferJobID: j.jobID,
			IntentDigest: j.intent.Digest(), TransferIntent: j.intent,
			Measure: j.Measure(), TerminationCause: err,
		}
	}
	if cause := context.Cause(runContext); cause != nil {
		state.terminationCause = cause
		j.tracker.failDiscovery()
		j.tracker.finishDiscovery()
		j.tracker.closeUpdates()
		return state.finish(ctx)
	}
	j.trace(TransferLifecycleTrace{
		Stage: TransferAdmissionStarted, TransferJobID: j.jobID, IntentDigest: j.intent.Digest(),
	})
	output, err := j.outputAuthority.OpenOutput(runContext, j.intent)
	if err != nil {
		state.terminationCause = err
		j.trace(TransferLifecycleTrace{
			Stage: TransferAdmissionCompleted, TransferJobID: j.jobID,
			IntentDigest: j.intent.Digest(), Failed: true,
		})
		j.tracker.failDiscovery()
		j.tracker.finishDiscovery()
		j.tracker.closeUpdates()
		return state.finish(ctx)
	}
	if output != nil {
		// OpenOutput has already created a durable namespace. Owning it before
		// validation guarantees a contract mismatch is settled, not abandoned.
		state.output = output
		state.admitted = true
	}
	if err := validateOutputSession(j.intent, output); err != nil {
		state.terminationCause = err
		j.trace(TransferLifecycleTrace{
			Stage: TransferAdmissionCompleted, TransferJobID: j.jobID,
			IntentDigest: j.intent.Digest(), Failed: true,
		})
		j.tracker.failDiscovery()
		j.tracker.finishDiscovery()
		j.tracker.closeUpdates()
		return state.finish(ctx)
	}
	j.trace(TransferLifecycleTrace{
		Stage: TransferAdmissionCompleted, TransferJobID: j.jobID,
		IntentDigest: j.intent.Digest(), OutputSessionID: output.SessionID(),
	})
	fileQueue := make(chan transferQueueItem, j.queueCapacity)
	workerErr := make(chan error, 1)
	workerDone := make(chan struct{})
	go state.transferQueueWorker(runContext, fileQueue, cancel, workerErr, workerDone)
	j.trace(TransferLifecycleTrace{
		Stage: TransferDiscoveryStarted, TransferJobID: j.jobID, IntentDigest: j.intent.Digest(),
		DirectoryID: j.root, Discovery: DiscoveryOpen,
	})
	discoveryErr := state.discoverIncremental(runContext, fileQueue)
	if discoveryErr != nil {
		j.tracker.failDiscovery()
		state.terminationCause = discoveryErr
		cancel(discoveryErr)
	}
	if discoveryErr == nil && !state.discoveryFailed {
		if missingErr := j.rules.missingTargetsError(
			state.matchedPaths, state.matchedDirectories, state.matchedFiles,
		); missingErr != nil {
			// Absence proven by a complete catalog is a closed selection result,
			// not a resumable transport or output failure.
			state.selectionResolutionFailure = missingErr
		}
	}
	j.tracker.finishDiscovery()
	j.trace(TransferLifecycleTrace{
		Stage: TransferDiscoveryCompleted, TransferJobID: j.jobID,
		IntentDigest: j.intent.Digest(), DirectoryID: j.root, DirectoryGeneration: state.rootGeneration,
		Discovery: j.Measure().Discovery, SelectionClass: j.Measure().Class(),
		Failed: discoveryErr != nil || state.discoveryFailed,
	})
	close(fileQueue)
	<-workerDone
	select {
	case workerFailure := <-workerErr:
		if state.terminationCause == nil {
			state.terminationCause = workerFailure
			cancel(workerFailure)
		}
	default:
	}
	j.tracker.closeUpdates()
	return state.finish(ctx)
}

func (r *jobRun) transferPlannedFile(ctx context.Context, plan plannedFile) error {
	opened, ready, err := r.openSelectedRevision(ctx, plan)
	if err != nil || !ready {
		return err
	}
	locator, err := NewPathOutputLocator(plan.path)
	if err != nil {
		return r.rejectUnstartedFile(ctx, plan, opened, NewJobDependencyContractError(err))
	}
	target, err := NewOutputFileTarget(
		r.output.BackendID(), r.output.SessionID(), opened.Descriptor, locator,
	)
	if err != nil {
		return r.rejectUnstartedFile(ctx, plan, opened, NewJobDependencyContractError(err))
	}
	if plan.parentAdmission.IsZero() {
		return r.rejectUnstartedFile(ctx, plan, opened, ErrDirectoryAdmissionMismatch)
	}
	start, err := r.output.BeginFile(ctx, OutputFile{
		Path: plan.path, ExpectedSize: plan.expectedSize, Descriptor: opened.Descriptor, Target: target,
		ParentAdmission: plan.parentAdmission,
	})
	if err != nil {
		r.traceFileLifecycle(TransferFileAdmitted, plan, true)
		releaseErr := r.releaseRevision(ctx, opened.LeaseID)
		inspection := inspectLifecycleError(err)
		failure := FileJobFailure{
			FileID: plan.file, Path: plan.path, Stage: FailureFileOutput,
			Cause: err, LeaseReleaseFailure: releaseErr,
		}
		if inspection.jobTerminal() || isJobTerminalError(releaseErr) ||
			inspection.outputRequiresJobPause(r.output.Capabilities()) {
			r.recordFileFailure(failure)
			return errors.Join(err, releaseErr)
		}
		r.recordFileFailure(failure)
		return nil
	}
	if settlement, immediate := start.ImmediateSettlement(); immediate {
		if err := validateImmediateFileSettlement(target, settlement); err != nil {
			r.traceFileLifecycle(TransferFileAdmitted, plan, true)
			return r.rejectImmediateSettlement(ctx, plan, opened, settlement, err)
		}
		r.traceFileLifecycle(TransferFileAdmitted, plan, false)
		return r.handleImmediateSettlement(ctx, plan, opened, settlement)
	}
	transaction, durable, transactional := start.Transaction()
	if !transactional || !start.valid() {
		r.traceFileLifecycle(TransferFileAdmitted, plan, true)
		return r.rejectImmediateSettlement(ctx, plan, opened, start.settlement, ErrOutputContract)
	}
	if err := validateOutputTransaction(target, transaction, durable); err != nil {
		r.traceFileLifecycle(TransferFileAdmitted, plan, true)
		return r.settleFailedFile(ctx, plan, opened, transaction, FailureFileOutput, err, 0)
	}
	r.traceFileLifecycle(TransferFileAdmitted, plan, false)
	completed, err := r.transferMissingRanges(ctx, plan, opened, transaction, durable)
	if err != nil || !completed {
		return err
	}
	return r.commitTransferredFile(ctx, plan, opened, transaction)
}

func (r *jobRun) rejectUnstartedFile(
	ctx context.Context,
	plan plannedFile,
	opened OpenedRevision,
	cause error,
) error {
	releaseErr := r.releaseRevision(ctx, opened.LeaseID)
	r.recordFileFailure(FileJobFailure{
		FileID: plan.file, Path: plan.path, Stage: FailureFileOutput, Cause: cause,
		LeaseReleaseFailure: releaseErr,
	})
	return errors.Join(cause, releaseErr)
}

func (r *jobRun) rejectImmediateSettlement(
	ctx context.Context,
	plan plannedFile,
	opened OpenedRevision,
	settlement FileSettlement,
	cause error,
) error {
	fault := outputContractFault(cause)
	releaseErr := r.releaseRevision(ctx, opened.LeaseID)
	r.settlementFailure = errors.Join(r.settlementFailure, fault)
	r.recordFileFailure(FileJobFailure{
		FileID: plan.file, Path: plan.path, Stage: FailureFileOutput, Cause: fault,
		Settlement: settlement, SettlementFailure: fault, LeaseReleaseFailure: releaseErr,
	})
	r.traceFileSettlement(plan, settlement, true)
	return fault
}

func (r *jobRun) openSelectedRevision(ctx context.Context, plan plannedFile) (OpenedRevision, bool, error) {
	opened, err := r.job.revisions.OpenRevision(ctx, plan.file)
	if err != nil {
		if isJobTerminalError(err) {
			return OpenedRevision{}, false, err
		}
		r.recordFileFailure(FileJobFailure{
			FileID: plan.file, Path: plan.path, Stage: FailureRevisionOpen, Cause: err,
		})
		return OpenedRevision{}, false, nil
	}
	if err := validateOpenedPlanFile(
		r.job.share, plan.file, plan.expectedSize, plan.modified, opened,
	); err != nil {
		releaseErr := r.releaseRevision(ctx, opened.LeaseID)
		r.recordFileFailure(FileJobFailure{
			FileID: plan.file, Path: plan.path, Stage: FailureRevisionIdentity,
			Cause: err, LeaseReleaseFailure: releaseErr,
		})
		if releaseErr != nil && isJobTerminalError(releaseErr) {
			return OpenedRevision{}, false, releaseErr
		}
		return OpenedRevision{}, false, nil
	}
	return opened, true, nil
}

func (r *jobRun) handleImmediateSettlement(
	ctx context.Context,
	plan plannedFile,
	opened OpenedRevision,
	settlement FileSettlement,
) error {
	releaseErr := r.releaseRevision(ctx, opened.LeaseID)
	r.acceptFileSettlement(settlement)
	r.traceFileSettlement(plan, settlement, releaseErr != nil)
	switch settlement.Kind() {
	case FilePublished:
		r.succeeded++
		r.job.tracker.completeFile(plan.expectedSize)
		if releaseErr != nil {
			r.recordFileFailure(FileJobFailure{
				FileID: plan.file, Path: plan.path, Stage: FailureLeaseRelease,
				Cause: releaseErr,
			})
		}
		if isJobTerminalError(releaseErr) {
			return releaseErr
		}
		return nil
	case FileCollision:
		r.recordFileFailure(FileJobFailure{
			FileID: plan.file, Path: plan.path, Stage: FailureFileOutput,
			Cause: ErrOutputPublishBlocked, Settlement: settlement,
			LeaseReleaseFailure: releaseErr,
		})
	case FilePublishBlocked:
		r.recordFileFailure(FileJobFailure{
			FileID: plan.file, Path: plan.path, Stage: FailureFileOutput,
			Cause: ErrOutputPublishBlocked, Settlement: settlement,
			LeaseReleaseFailure: releaseErr,
		})
	case FileQuarantined:
		r.recordFileFailure(FileJobFailure{
			FileID: plan.file, Path: plan.path, Stage: FailureFileOutput,
			Cause: ErrOutputQuarantined, Settlement: settlement,
			LeaseReleaseFailure: releaseErr,
		})
	case FileRetired:
		// A recovered retirement is already-authorized durable cleanup. Preserve
		// its exact settlement without creating a transaction or attempting a
		// second settlement whose reason this run cannot authenticate.
		r.recordFileFailure(FileJobFailure{
			FileID: plan.file, Path: plan.path, Stage: FailureFileOutput,
			Cause: ErrOutputRetired, Settlement: settlement,
			LeaseReleaseFailure: releaseErr,
		})
	default:
		fault := outputContractFault(nil)
		r.settlementFailure = errors.Join(r.settlementFailure, fault)
		return fault
	}
	if isJobTerminalError(releaseErr) {
		return releaseErr
	}
	return nil
}
