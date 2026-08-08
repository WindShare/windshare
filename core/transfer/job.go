// Package transfer coordinates receiver-scoped file-local block demand across
// authenticated protocol lanes.
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
	"github.com/windshare/windshare/core/transfer/fault"
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

type JobResult struct {
	Outcome        JobOutcome
	Settlement     JobSettlement
	TransferJobID  TransferJobID
	IntentDigest   TransferIntentDigest
	TransferIntent TransferIntent
	// SelectionObservation is diagnostic only and is normally zero for the
	// incremental path, which does not materialize a whole-tree snapshot.
	SelectionObservation     SelectionObservationV1
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
	SourceDriftFault   fault.Fault
	SucceededFiles     uint64
	TerminationCause   error
	TerminationFault   fault.Fault
	SettlementFailure  error
	SettlementFault    fault.Fault
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
		failure := dependencyContractFailure(ErrTransferJobRun)
		return JobResult{Outcome: JobPausedOutcome, TransferJobID: j.jobID,
			IntentDigest: j.intent.Digest(), TransferIntent: j.intent,
			Measure: j.Measure(), TerminationCause: failure,
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
			Outcome: JobPausedOutcome, TransferJobID: j.jobID,
			IntentDigest: j.intent.Digest(), TransferIntent: j.intent,
			Measure: j.Measure(), TerminationCause: failure,
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
		Stage: TransferDiscoveryStarted, TransferJobID: j.jobID, IntentDigest: j.intent.Digest(),
		DirectoryID: j.root, Discovery: DiscoveryOpen,
	})
	discoveryFailure := admitInternalFailure(state.discoverIncremental(runContext, fileQueue))
	return state.completeDiscovery(ctx, runContext, cancel, fileQueue, workerErr, workerDone, discoveryFailure)
}

func (j *TransferJob) admitRunOutput(ctx context.Context, state *jobRun) *lifecycleFailure {
	j.trace(TransferLifecycleTrace{
		Stage: TransferAdmissionStarted, TransferJobID: j.jobID, IntentDigest: j.intent.Digest(),
	})
	output, rawOutputErr := j.outputAuthority.OpenOutput(ctx, j.intent)
	failure := admitInternalFailure(normalizeOutputBoundary(ctx, rawOutputErr))
	if output != nil {
		// A collaborator can cross a durable mutation boundary before reporting an
		// error. Retaining the returned capability lets finish request a stable
		// pause instead of abandoning that namespace.
		state.output = output
		state.admitted = true
	}
	if failure == nil {
		failure = admitInternalFailure(validateOutputSession(j.intent, output))
	}
	if failure == nil {
		admissionScope, err := NewDirectoryAdmissionScope(j.intent)
		if err != nil {
			// Intent validation precedes OpenOutput, so failure to project its receipt
			// scope is an internal boundary violation rather than backend authority.
			failure = dependencyContractFailure(err)
		} else {
			state.directoryAdmissionScope = admissionScope
		}
	}
	if failure != nil {
		j.trace(TransferLifecycleTrace{
			Stage: TransferAdmissionCompleted, TransferJobID: j.jobID,
			IntentDigest: j.intent.Digest(), Fault: closedLifecycleFault(failure), Failed: true,
		})
		return failure
	}
	j.trace(TransferLifecycleTrace{
		Stage: TransferAdmissionCompleted, TransferJobID: j.jobID,
		IntentDigest: j.intent.Digest(), OutputSessionID: output.SessionID(),
	})
	return nil
}

func (j *TransferJob) failUnstartedDiscovery() {
	// Consumers must observe one terminal discovery transition even when output
	// admission fails before the catalog can safely be opened.
	j.tracker.failDiscovery()
	j.tracker.finishDiscovery()
	j.tracker.closeUpdates()
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
	workerInterruptedDiscovery := workerCause != nil && workerCause == discoveryFailure
	discoveryIncomplete := discoveryFailure != nil &&
		(!workerInterruptedDiscovery || !r.catalogTraversalComplete)
	if discoveryFailure != nil {
		if discoveryIncomplete {
			r.job.tracker.failDiscovery()
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
	r.job.tracker.finishDiscovery()
	r.job.trace(TransferLifecycleTrace{
		Stage: TransferDiscoveryCompleted, TransferJobID: r.job.jobID,
		IntentDigest: r.job.intent.Digest(), DirectoryID: r.job.root, DirectoryGeneration: r.rootGeneration,
		Discovery: r.job.Measure().Discovery, SelectionClass: r.job.Measure().Class(),
		Fault: fault.Join(
			discoveryCompletionFault(discoveryFailure, discoveryIncomplete), r.discoveryFaultSnapshot(),
		),
		Failed: discoveryIncomplete || r.discoveryFailed,
	})
	close(fileQueue)
	<-workerDone
	select {
	case workerFailure := <-workerErr:
		if r.terminationCause == nil || workerInterruptedDiscovery {
			r.terminationCause = workerFailure
			cancel(workerFailure)
		}
	default:
	}
	r.job.tracker.closeUpdates()
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
	locator, err := NewPathOutputLocator(plan.path)
	if err != nil {
		return r.rejectUnstartedFile(ctx, plan, opened, dependencyContractFailure(err))
	}
	target, err := NewOutputFileTarget(
		r.output.BackendID(), r.output.SessionID(), opened.Descriptor, locator,
	)
	if err != nil {
		return r.rejectUnstartedFile(ctx, plan, opened, dependencyContractFailure(err))
	}
	if plan.parentAdmission.IsZero() {
		return r.rejectUnstartedFile(ctx, plan, opened, dependencyContractFailure(ErrDirectoryAdmissionMismatch))
	}
	start, err := r.output.BeginFile(ctx, OutputFile{
		Path: plan.path, ExpectedSize: plan.expectedSize, Descriptor: opened.Descriptor, Target: target,
		ParentAdmission: plan.parentAdmission,
	})
	err = normalizeOutputBoundary(ctx, err)
	if err != nil {
		r.traceFileLifecycle(TransferFileAdmitted, plan, err)
		releaseErr := r.releaseRevision(ctx, opened.LeaseID)
		policy := lifecyclePolicyFor(err)
		failure := FileJobFailure{
			FileID: plan.file, Path: plan.path, Stage: FailureFileOutput,
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
		return r.settleFailedFile(ctx, plan, opened, transaction, FailureFileOutput, err, 0)
	}
	r.traceFileLifecycle(TransferFileAdmitted, plan, nil)
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
		FileID: plan.file, Path: plan.path, Stage: FailureFileOutput, Cause: fault,
		Settlement: settlement, SettlementFailure: fault, LeaseReleaseFailure: releaseErr,
	})
	r.traceFileSettlement(plan, settlement, joinLifecycleFailures(fault, releaseErr))
	return fault
}

func (r *jobRun) openSelectedRevision(ctx context.Context, plan plannedFile) (OpenedRevision, bool, error) {
	opened, rawOpenErr := r.job.revisions.OpenRevision(ctx, plan.file)
	err := normalizeSourceBoundary(ctx, rawOpenErr)
	if err != nil {
		var releaseErr error
		if !opened.LeaseID.IsZero() {
			releaseErr = r.releaseRevision(ctx, opened.LeaseID)
		}
		if isJobTerminalError(err) {
			return OpenedRevision{}, false, joinLifecycleFailures(err, releaseErr)
		}
		r.recordFileFailure(FileJobFailure{
			FileID: plan.file, Path: plan.path, Stage: FailureRevisionOpen, Cause: err,
			LeaseReleaseFailure: releaseErr,
		})
		if isJobTerminalError(releaseErr) {
			return OpenedRevision{}, false, releaseErr
		}
		return OpenedRevision{}, false, nil
	}
	if err := validateOpenedPlanFile(
		r.job.share, plan.file, plan.expectedSize, plan.modified, opened,
	); err != nil {
		err = sourceChangedFailure(err)
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
	r.traceFileSettlement(plan, settlement, releaseErr)
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
		contractFailure := outputContractFault(nil)
		r.settlementFailure = mergeLifecycleFailures(r.settlementFailure, contractFailure)
		return contractFailure
	}
	if isJobTerminalError(releaseErr) {
		return releaseErr
	}
	return nil
}
