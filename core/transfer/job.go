package transfer

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

const (
	DefaultOutputSettlementTimeout = 30 * time.Second
	MaximumOutputSettlementTimeout = 5 * time.Minute
)

var (
	ErrInvalidTransferJob    = errors.New("transfer job configuration is invalid")
	ErrTransferJobRun        = errors.New("transfer job may only run once")
	ErrCatalogIdentity       = errors.New("catalog snapshot does not match the requested share and directory")
	ErrRevisionIdentity      = errors.New("opened revision does not match the selected catalog file")
	ErrOutputContract        = errors.New("output session violated its file transaction contract")
	ErrCatalogEntriesOmitted = errors.New("catalog directory omitted children")
	ErrCatalogLeaseContract  = errors.New("catalog reader returned no release callback")
	ErrOutputPublishBlocked  = errors.New("output publication is blocked by an existing final path")
	ErrOutputQuarantined     = errors.New("output file needs manual ownership review")
	ErrOutputRetired         = errors.New("output file retirement completed without content transfer")
	errRangeReaderContract   = errors.New("range reader violated its requested output interval")
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
	Outcome           JobOutcome
	Settlement        JobSettlement
	ResumeIntent      ResumeIntent
	SelectionIdentity SelectionIdentity
	Measure           SelectionMeasure
	Directories       []DirectoryJobFailure
	Files             []FileJobFailure
	SucceededFiles    uint64
	TerminationCause  error
	SettlementFailure error
}

type CatalogReader interface {
	// Implementations must be safe for concurrent calls. The release callback is
	// mandatory even when err is a typed directory failure, because authenticated
	// failures also consume bounded cache memory. Returned immutable values remain
	// valid after release; release relinquishes source-cache accounting only.
	AcquireDirectory(context.Context, catalog.DirectoryID) (catalog.DirectorySnapshot, func(), error)
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
	ShareInstance     catalog.ShareInstance
	SyntheticRoot     catalog.DirectoryID
	Rules             SelectionRules
	Catalog           CatalogReader
	Revisions         RevisionClient
	Blocks            RangeReader
	Output            OutputAuthority
	SettlementTimeout time.Duration
	Tracer            TransferLifecycleTracer
}

type TransferJob struct {
	share             catalog.ShareInstance
	root              catalog.DirectoryID
	rules             SelectionRules
	selectionRequest  CanonicalSelectionRequest
	catalog           CatalogReader
	revisions         RevisionClient
	blocks            RangeReader
	outputAuthority   OutputAuthority
	settlementTimeout time.Duration
	tracer            TransferLifecycleTracer
	tracker           selectionTracker

	mu      sync.Mutex
	started bool
}

func NewTransferJob(config TransferJobConfig) (*TransferJob, error) {
	if config.ShareInstance.IsZero() || config.SyntheticRoot.IsZero() || !config.Rules.validSnapshot() ||
		config.Catalog == nil || config.Revisions == nil || config.Blocks == nil || config.Output == nil ||
		config.SettlementTimeout < 0 || config.SettlementTimeout > MaximumOutputSettlementTimeout {
		return nil, ErrInvalidTransferJob
	}
	timeout := config.SettlementTimeout
	if timeout == 0 {
		timeout = DefaultOutputSettlementTimeout
	}
	selectionRequest, err := NewCanonicalSelectionRequest(config.ShareInstance, config.SyntheticRoot, config.Rules)
	if err != nil {
		return nil, ErrInvalidTransferJob
	}
	return &TransferJob{
		share: config.ShareInstance, root: config.SyntheticRoot, rules: config.Rules,
		selectionRequest: selectionRequest,
		catalog:          config.Catalog, revisions: config.Revisions, blocks: config.Blocks,
		outputAuthority:   config.Output,
		settlementTimeout: timeout, tracer: config.Tracer, tracker: newSelectionTracker(),
	}, nil
}

func (j *TransferJob) Measure() SelectionMeasure { return j.tracker.snapshot() }

// SelectionMeasures publishes monotonic discovery updates. Discovery now owns
// the first job phase, so no duplicate catalog walk can race output admission.
func (j *TransferJob) SelectionMeasures() <-chan SelectionMeasure { return j.tracker.Updates() }

func (j *TransferJob) Run(ctx context.Context) JobResult {
	j.mu.Lock()
	if j.started {
		j.mu.Unlock()
		return JobResult{Outcome: JobPausedOutcome, Measure: j.Measure(), TerminationCause: ErrTransferJobRun}
	}
	j.started = true
	j.mu.Unlock()

	runContext, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	state := newJobRun(j)
	if cause := context.Cause(runContext); cause != nil {
		state.terminationCause = cause
		j.tracker.failDiscovery()
		j.tracker.finishDiscovery()
		j.tracker.closeUpdates()
		return state.finish(ctx)
	}
	j.trace(TransferLifecycleTrace{Stage: TransferDiscoveryStarted})
	selection, selectionReady, discoveryErr := state.discoverSelection(runContext)
	if discoveryErr != nil {
		j.tracker.failDiscovery()
		state.terminationCause = discoveryErr
		cancel(discoveryErr)
	}
	j.tracker.finishDiscovery()
	j.tracker.closeUpdates()
	j.trace(TransferLifecycleTrace{
		Stage:             TransferDiscoveryCompleted,
		SelectionIdentity: selection.Identity(), ResumeIntent: selection.ResumeIntent(),
		Failed: discoveryErr != nil || !selectionReady,
	})
	if discoveryErr != nil || !selectionReady {
		return state.finish(ctx)
	}

	state.setSelection(selection)
	j.trace(TransferLifecycleTrace{
		Stage:             TransferAdmissionStarted,
		SelectionIdentity: selection.Identity(), ResumeIntent: selection.ResumeIntent(),
	})
	output, err := j.outputAuthority.OpenSelection(runContext, selection)
	if err != nil {
		state.terminationCause = err
		j.trace(TransferLifecycleTrace{
			Stage:             TransferAdmissionCompleted,
			SelectionIdentity: selection.Identity(), ResumeIntent: selection.ResumeIntent(), Failed: true,
		})
		return state.finish(ctx)
	}
	if err := validateOutputSession(output); err != nil {
		state.terminationCause = err
		j.trace(TransferLifecycleTrace{
			Stage: TransferAdmissionCompleted, SelectionIdentity: selection.Identity(),
			ResumeIntent: selection.ResumeIntent(), Failed: true,
		})
		return state.finish(ctx)
	}
	state.output = output
	state.admitted = true
	j.trace(TransferLifecycleTrace{
		Stage: TransferAdmissionCompleted, OutputSessionID: output.SessionID(),
		SelectionIdentity: selection.Identity(), ResumeIntent: selection.ResumeIntent(),
	})
	for index := range state.plannedFiles {
		if cause := context.Cause(runContext); cause != nil {
			state.terminationCause = cause
			break
		}
		file := state.plannedFiles[index]
		if err := state.transferPlannedFile(runContext, file); err != nil {
			if state.terminationCause == nil {
				state.terminationCause = err
			}
			break
		}
		if state.settlementFailure != nil {
			break
		}
	}
	if state.terminationCause == nil && state.settlementFailure == nil {
		if err := state.finalizeSelectionDirectories(runContext); err != nil {
			state.terminationCause = err
		}
	}
	return state.finish(ctx)
}

func (r *jobRun) setSelection(selection OutputSelection) {
	r.selectionIdentity = selection.Identity()
	r.resumeIntent = selection.ResumeIntent()
	r.job.trace(TransferLifecycleTrace{
		Stage:             TransferSelectionFrozen,
		SelectionIdentity: selection.Identity(), ResumeIntent: selection.ResumeIntent(),
	})
}

func validateOutputSession(output OutputSession) error {
	if output == nil {
		return outputContractFault(nil)
	}
	if _, err := NewOutputBackendID(string(output.BackendID())); err != nil {
		return outputContractFault(nil)
	}
	capabilities := output.Capabilities()
	if _, err := NewOutputCapabilities(capabilities); err != nil || output.SessionID().IsZero() {
		return outputContractFault(nil)
	}
	return nil
}

func (r *jobRun) transferPlannedFile(ctx context.Context, plan plannedFile) error {
	r.job.trace(TransferLifecycleTrace{
		Stage: TransferFileStarted, OutputSessionID: r.output.SessionID(), ResumeIntent: r.resumeIntent,
		FileID: plan.file,
	})
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
	start, err := r.output.BeginFile(ctx, OutputFile{
		Path: plan.path, ExpectedSize: plan.entry.ExpectedSize(), Descriptor: opened.Descriptor, Target: target,
	})
	if err != nil {
		releaseErr := r.releaseRevision(ctx, opened.LeaseID)
		if isJobTerminalError(err) || outputFailureRequiresJobPause(err, r.output.Capabilities()) {
			return errors.Join(err, releaseErr)
		}
		r.files = append(r.files, FileJobFailure{
			FileID: plan.file, Path: plan.path, Stage: FailureFileOutput,
			Cause: err, LeaseReleaseFailure: releaseErr,
		})
		return nil
	}
	if settlement, immediate := start.ImmediateSettlement(); immediate {
		if err := validateImmediateFileSettlement(target, settlement); err != nil {
			return r.rejectImmediateSettlement(ctx, plan, opened, settlement, err)
		}
		return r.handleImmediateSettlement(ctx, plan, opened, settlement)
	}
	transaction, durable, transactional := start.Transaction()
	if !transactional || !start.valid() {
		return r.rejectImmediateSettlement(ctx, plan, opened, start.settlement, ErrOutputContract)
	}
	if err := validateOutputTransaction(target, transaction, durable); err != nil {
		return r.settleFailedFile(ctx, plan, opened, transaction, FailureFileOutput, err, 0)
	}
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
	r.files = append(r.files, FileJobFailure{
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
	r.files = append(r.files, FileJobFailure{
		FileID: plan.file, Path: plan.path, Stage: FailureFileOutput, Cause: fault,
		Settlement: settlement, SettlementFailure: fault, LeaseReleaseFailure: releaseErr,
	})
	r.traceFileSettlement(plan.file, settlement, true)
	return fault
}

func (r *jobRun) openSelectedRevision(ctx context.Context, plan plannedFile) (OpenedRevision, bool, error) {
	opened, err := r.job.revisions.OpenRevision(ctx, plan.file)
	if err != nil {
		if isJobTerminalError(err) {
			return OpenedRevision{}, false, err
		}
		r.files = append(r.files, FileJobFailure{
			FileID: plan.file, Path: plan.path, Stage: FailureRevisionOpen, Cause: err,
		})
		return OpenedRevision{}, false, nil
	}
	if err := validateOpenedFile(r.job.share, plan.entry, opened); err != nil {
		releaseErr := r.releaseRevision(ctx, opened.LeaseID)
		r.files = append(r.files, FileJobFailure{
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
	r.traceFileSettlement(plan.file, settlement, releaseErr != nil)
	switch settlement.Kind() {
	case FilePublished:
		r.succeeded++
		if releaseErr != nil {
			r.files = append(r.files, FileJobFailure{
				FileID: plan.file, Path: plan.path, Stage: FailureLeaseRelease,
				Cause: releaseErr,
			})
		}
		if isJobTerminalError(releaseErr) {
			return releaseErr
		}
		return nil
	case FileCollision:
		r.files = append(r.files, FileJobFailure{
			FileID: plan.file, Path: plan.path, Stage: FailureFileOutput,
			Cause: ErrOutputPublishBlocked, Settlement: settlement,
			LeaseReleaseFailure: releaseErr,
		})
	case FilePublishBlocked:
		r.files = append(r.files, FileJobFailure{
			FileID: plan.file, Path: plan.path, Stage: FailureFileOutput,
			Cause: ErrOutputPublishBlocked, Settlement: settlement,
			LeaseReleaseFailure: releaseErr,
		})
	case FileQuarantined:
		r.files = append(r.files, FileJobFailure{
			FileID: plan.file, Path: plan.path, Stage: FailureFileOutput,
			Cause: ErrOutputQuarantined, Settlement: settlement,
			LeaseReleaseFailure: releaseErr,
		})
	case FileRetired:
		// A recovered retirement is already-authorized durable cleanup. Preserve
		// its exact settlement without creating a transaction or attempting a
		// second settlement whose reason this run cannot authenticate.
		r.files = append(r.files, FileJobFailure{
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

func (r *jobRun) transferMissingRanges(
	ctx context.Context,
	plan plannedFile,
	opened OpenedRevision,
	transaction FileTransaction,
	durable VerifiedDurableRanges,
) (bool, error) {
	missing, err := MissingRanges(opened.Descriptor.ExactSize(), durable.Ranges())
	if err != nil {
		return false, r.settleFailedFile(
			ctx, plan, opened, transaction, FailureFileOutput, outputContractFault(err), 0,
		)
	}
	for _, requested := range splitAtBlockBoundaries(missing, opened.Descriptor.Geometry()) {
		buffered, bufferErr := newAtomicRequestedRangeSink(requested, transaction)
		if bufferErr != nil {
			return false, r.settleFailedFile(
				ctx, plan, opened, transaction, FailureBlockTransfer, bufferErr, 0,
			)
		}
		err := r.job.blocks.ReadRange(ctx, opened.LeaseID, opened.Descriptor, requested, buffered)
		if bufferedErr := buffered.Failure(); bufferedErr != nil {
			err = bufferedErr
		}
		if err != nil {
			if invalidator, ok := r.job.blocks.(interface {
				InvalidateRevision(catalog.FileID, content.FileRevision)
			}); ok && (errors.Is(err, content.ErrRevisionDrift) || errors.Is(err, ErrBlockInvalidated)) {
				invalidator.InvalidateRevision(plan.file, opened.Descriptor.FileRevision())
			}
			retireReason := fileRetireReason(err)
			return false, r.settleFailedFile(
				ctx, plan, opened, transaction, FailureBlockTransfer, err, retireReason,
			)
		}
		if err := buffered.Flush(ctx); err != nil {
			return false, r.settleFailedFile(
				ctx, plan, opened, transaction, FailureBlockTransfer, err, 0,
			)
		}
		checkpoint, checkpointErr := transaction.Checkpoint(ctx)
		if checkpointErr != nil {
			return false, r.settleFailedFile(
				ctx, plan, opened, transaction, FailureFileOutput, checkpointErr, 0,
			)
		}
		if checkpoint.Binding() != transaction.Binding() ||
			checkpoint.CheckpointGeneration() <= durable.CheckpointGeneration() ||
			!rangeContains(checkpoint.Ranges(), requested) || !rangesContain(checkpoint.Ranges(), durable.Ranges()) {
			return false, r.settleFailedFile(
				ctx, plan, opened, transaction, FailureFileOutput,
				outputContractFault(nil), 0,
			)
		}
		durable = checkpoint
	}
	if !RangesCoverFile(opened.Descriptor.ExactSize(), durable.Ranges()) {
		return false, r.settleFailedFile(
			ctx, plan, opened, transaction, FailureFileOutput,
			outputContractFault(nil), 0,
		)
	}
	return true, nil
}
