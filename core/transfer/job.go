package transfer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/protocolsession"
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

type atomicRequestedRangeSink struct {
	mu        sync.Mutex
	target    RangeSink
	requested content.Range
	data      []byte
	covered   []uint64
	count     uint64
	failure   error
	sealed    bool
}

func newAtomicRequestedRangeSink(
	requested content.Range,
	target RangeSink,
) (*atomicRequestedRangeSink, error) {
	if target == nil || requested.Offset >= requested.End {
		return nil, rangeReaderContractError(errors.New("requested range is empty or has no output sink"))
	}
	length := requested.End - requested.Offset
	maxInt := uint64(^uint(0) >> 1)
	if length > uint64(catalog.MaxChunkSize) || length > maxInt {
		return nil, rangeReaderContractError(errors.New("requested range exceeds the atomic protocol bound"))
	}
	return &atomicRequestedRangeSink{
		target: target, requested: requested,
		data: make([]byte, int(length)), covered: make([]uint64, (length+63)/64),
	}, nil
}

func (sink *atomicRequestedRangeSink) WriteRange(
	ctx context.Context,
	offset uint64,
	data []byte,
) error {
	if err := ctx.Err(); err != nil {
		sink.mu.Lock()
		if sink.failure == nil {
			sink.failure = err
		}
		sink.mu.Unlock()
		return err
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.failure != nil {
		return sink.failure
	}
	if sink.sealed {
		return sink.failContractLocked(errors.New("range reader wrote after returning"))
	}
	if len(data) == 0 || offset < sink.requested.Offset || offset >= sink.requested.End ||
		uint64(len(data)) > sink.requested.End-offset {
		return sink.failContractLocked(errors.New("range write escapes the requested interval"))
	}
	start := offset - sink.requested.Offset
	end := start + uint64(len(data))
	for index := start; index < end; index++ {
		if sink.covered[index/64]&(uint64(1)<<(index%64)) != 0 {
			return sink.failContractLocked(errors.New("range write overlaps bytes already supplied"))
		}
	}
	copy(sink.data[int(start):int(end)], data)
	for index := start; index < end; index++ {
		sink.covered[index/64] |= uint64(1) << (index % 64)
	}
	sink.count += uint64(len(data))
	return nil
}

func (sink *atomicRequestedRangeSink) Flush(ctx context.Context) error {
	if sink == nil {
		return rangeReaderContractError(errors.New("range reader returned no atomic sink"))
	}
	sink.mu.Lock()
	if sink.failure != nil {
		err := sink.failure
		sink.mu.Unlock()
		return err
	}
	if sink.sealed {
		err := sink.failContractLocked(errors.New("requested range was flushed more than once"))
		sink.mu.Unlock()
		return err
	}
	if sink.count != uint64(len(sink.data)) {
		err := sink.failContractLocked(errors.New("range reader returned before covering the requested interval"))
		sink.mu.Unlock()
		return err
	}
	sink.sealed = true
	target, offset, data := sink.target, sink.requested.Offset, sink.data
	sink.mu.Unlock()
	return target.WriteRange(ctx, offset, data)
}

func (sink *atomicRequestedRangeSink) Failure() error {
	if sink == nil {
		return rangeReaderContractError(errors.New("range reader returned no atomic sink"))
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return sink.failure
}

func (sink *atomicRequestedRangeSink) failContractLocked(cause error) error {
	if sink.failure == nil {
		sink.failure = rangeReaderContractError(cause)
	}
	return sink.failure
}

func rangeReaderContractError(cause error) error {
	return NewJobDependencyContractError(errors.Join(errRangeReaderContract, cause))
}

func (r *jobRun) settleFailedFile(
	ctx context.Context,
	plan plannedFile,
	opened OpenedRevision,
	transaction FileTransaction,
	stage FailureStage,
	cause error,
	retireReason FileRetireReason,
) error {
	if !retireReason.valid() || isJobTerminalError(cause) || isOutputFailure(cause) ||
		outputFailureRequiresJobPause(cause, r.output.Capabilities()) {
		return r.pauseFailedFile(ctx, plan, opened, transaction, stage, cause)
	}
	settleContext, cancel := r.job.newSettlementContext(ctx)
	settlement, settlementErr := transaction.Retire(settleContext, retireReason)
	cancel()
	settlementErr = validateSettlementFailure(settlementErr)
	releaseErr := r.releaseRevision(ctx, opened.LeaseID)
	failure := FileJobFailure{
		FileID: plan.file, Path: plan.path, Stage: stage, Cause: cause,
		Settlement: settlement, SettlementFailure: settlementErr,
		LeaseReleaseFailure: releaseErr,
	}
	valid := settlementErr == nil && settlement.matchesBinding(transaction.Binding()) &&
		(settlement.Kind() == FileRetired || settlement.Kind() == FileQuarantined)
	if !valid && settlementErr == nil {
		settlementErr = outputContractFault(nil)
		failure.SettlementFailure = settlementErr
	}
	if settlementErr == nil {
		r.acceptFileSettlement(settlement)
	}
	r.files = append(r.files, failure)
	r.traceFileSettlement(plan.file, settlement, settlementErr != nil)
	if settlementErr != nil {
		r.settlementFailure = errors.Join(r.settlementFailure, settlementErr)
		return cause
	}
	if isJobTerminalError(releaseErr) {
		return releaseErr
	}
	return nil
}

func (r *jobRun) pauseFailedFile(
	ctx context.Context,
	plan plannedFile,
	opened OpenedRevision,
	transaction FileTransaction,
	stage FailureStage,
	cause error,
) error {
	reason := filePauseReason(cause)
	settleContext, cancel := r.job.newSettlementContext(ctx)
	settlement, settlementErr := transaction.Pause(settleContext, reason)
	cancel()
	settlementErr = validateSettlementFailure(settlementErr)
	releaseErr := r.releaseRevision(ctx, opened.LeaseID)
	if settlementErr == nil && (!settlement.matchesBinding(transaction.Binding()) ||
		settlement.Kind() != FilePaused && settlement.Kind() != FileQuarantined) {
		settlementErr = outputContractFault(nil)
	}
	if settlementErr == nil {
		r.acceptFileSettlement(settlement)
	}
	r.files = append(r.files, FileJobFailure{
		FileID: plan.file, Path: plan.path, Stage: stage, Cause: cause,
		Settlement: settlement, SettlementFailure: settlementErr,
		LeaseReleaseFailure: releaseErr,
	})
	r.traceFileSettlement(plan.file, settlement, settlementErr != nil)
	if settlementErr != nil {
		r.settlementFailure = errors.Join(r.settlementFailure, settlementErr)
	}
	return cause
}

func (r *jobRun) commitTransferredFile(
	ctx context.Context,
	plan plannedFile,
	opened OpenedRevision,
	transaction FileTransaction,
) error {
	if cause := context.Cause(ctx); cause != nil {
		return r.pauseFailedFile(ctx, plan, opened, transaction, FailureFileOutput, cause)
	}
	settleContext, cancel := r.job.newSettlementContext(ctx)
	settlement, settlementErr := transaction.Commit(settleContext)
	cancel()
	settlementErr = validateSettlementFailure(settlementErr)
	if settlementErr != nil {
		r.settlementFailure = errors.Join(r.settlementFailure, settlementErr)
		releaseErr := r.releaseRevision(ctx, opened.LeaseID)
		r.files = append(r.files, FileJobFailure{
			FileID: plan.file, Path: plan.path, Stage: FailureFileOutput,
			Settlement: settlement, SettlementFailure: settlementErr,
			LeaseReleaseFailure: releaseErr,
		})
		r.traceFileSettlement(plan.file, settlement, true)
		if isJobTerminalError(releaseErr) {
			return releaseErr
		}
		return nil
	}
	if !settlement.matchesBinding(transaction.Binding()) || settlement.Kind() != FilePublished &&
		settlement.Kind() != FilePublishBlocked && settlement.Kind() != FileQuarantined {
		return r.rejectCommitSettlement(ctx, plan, opened, settlement)
	}
	r.acceptFileSettlement(settlement)
	releaseErr := r.releaseRevision(ctx, opened.LeaseID)
	r.traceFileSettlement(plan.file, settlement, releaseErr != nil)
	switch settlement.Kind() {
	case FilePublished:
		r.succeeded++
		if releaseErr != nil {
			r.files = append(r.files, FileJobFailure{
				FileID: plan.file, Path: plan.path, Stage: FailureLeaseRelease, Cause: releaseErr,
			})
		}
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
	}
	if isJobTerminalError(releaseErr) {
		return releaseErr
	}
	return nil
}

func (r *jobRun) rejectCommitSettlement(
	ctx context.Context,
	plan plannedFile,
	opened OpenedRevision,
	settlement FileSettlement,
) error {
	contractFailure := outputContractFault(nil)
	releaseErr := r.releaseRevision(ctx, opened.LeaseID)
	r.settlementFailure = errors.Join(r.settlementFailure, contractFailure)
	r.files = append(r.files, FileJobFailure{
		FileID: plan.file, Path: plan.path, Stage: FailureFileOutput, Cause: contractFailure,
		Settlement: settlement, SettlementFailure: contractFailure,
		LeaseReleaseFailure: releaseErr,
	})
	r.traceFileSettlement(plan.file, settlement, true)
	return errors.Join(contractFailure, releaseErr)
}

func (r *jobRun) releaseRevision(ctx context.Context, lease content.LeaseID) error {
	settleContext, cancel := r.job.newSettlementContext(ctx)
	err := r.job.revisions.ReleaseRevision(settleContext, lease)
	cancel()
	return err
}

func (r *jobRun) finish(ctx context.Context) JobResult {
	if r.terminationCause == nil && r.settlementFailure == nil {
		if cause := context.Cause(ctx); cause != nil {
			r.terminationCause = cause
		}
	}
	outcome := JobSucceeded
	if len(r.directories) != 0 || len(r.files) != 0 {
		outcome = JobCompletedWithErrors
	}
	if r.admitted {
		if r.terminationCause != nil || r.settlementFailure != nil {
			outcome = JobPausedOutcome
			r.pauseJob(ctx)
		} else {
			r.completeJob(ctx, outcome)
			if r.settlementFailure != nil {
				outcome = JobPausedOutcome
			}
		}
	} else if r.terminationCause != nil || r.settlementFailure != nil {
		outcome = JobPausedOutcome
	}
	return JobResult{
		Outcome: outcome, Settlement: r.settlement,
		ResumeIntent: r.resumeIntent, SelectionIdentity: r.selectionIdentity,
		Measure: r.job.Measure(), Directories: slices.Clone(r.directories), Files: slices.Clone(r.files),
		SucceededFiles: r.succeeded, TerminationCause: r.terminationCause,
		SettlementFailure: r.settlementFailure,
	}
}

func (r *jobRun) pauseJob(ctx context.Context) {
	reason := jobPauseReason(r.terminationCause, r.settlementFailure)
	settleContext, cancel := r.job.newSettlementContext(ctx)
	settlement, err := r.output.PauseJob(settleContext, reason)
	cancel()
	err = validateSettlementFailure(err)
	if err == nil {
		if settlement.Kind() != JobPaused && settlement.Kind() != JobPausedNeedsAttention ||
			r.needsAttention && settlement.Kind() != JobPausedNeedsAttention {
			err = outputContractFault(nil)
		}
	}
	if err != nil {
		r.settlementFailure = errors.Join(r.settlementFailure, err)
	} else {
		r.settlement = settlement
	}
	r.job.trace(TransferLifecycleTrace{
		Stage: TransferJobSettled, OutputSessionID: r.output.SessionID(), ResumeIntent: r.resumeIntent,
		JobSettlement: settlement.Kind(), Failed: err != nil,
	})
}

func (r *jobRun) completeJob(ctx context.Context, outcome JobOutcome) {
	settleContext, cancel := r.job.newSettlementContext(ctx)
	settlement, err := r.output.CompleteJob(settleContext, outcome)
	cancel()
	err = validateSettlementFailure(err)
	if err == nil {
		if settlement.Kind() != JobClosed && settlement.Kind() != JobPausedNeedsAttention ||
			r.needsAttention && settlement.Kind() != JobPausedNeedsAttention {
			err = outputContractFault(nil)
		}
	}
	if err != nil {
		r.settlementFailure = errors.Join(r.settlementFailure, err)
		r.job.trace(TransferLifecycleTrace{
			Stage: TransferJobSettled, OutputSessionID: r.output.SessionID(), ResumeIntent: r.resumeIntent,
			JobSettlement: settlement.Kind(), Failed: true,
		})
		return
	}
	r.settlement = settlement
	r.job.trace(TransferLifecycleTrace{
		Stage: TransferJobSettled, OutputSessionID: r.output.SessionID(), ResumeIntent: r.resumeIntent,
		JobSettlement: settlement.Kind(),
	})
}

func (j *TransferJob) newSettlementContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), j.settlementTimeout)
}

func filePauseReason(cause error) FilePauseReason {
	switch {
	case errors.Is(cause, context.Canceled), errors.Is(cause, context.DeadlineExceeded):
		return FilePauseInterrupted
	case isSessionFailure(cause):
		return FilePauseSessionFailure
	case isOutputFailure(cause):
		return FilePauseOutputFailure
	default:
		return FilePauseTransportFailure
	}
}

func jobPauseReason(cause, settlementFailure error) JobPauseReason {
	if settlementFailure != nil {
		return JobPauseOutputFailure
	}
	switch {
	case errors.Is(cause, context.Canceled), errors.Is(cause, context.DeadlineExceeded):
		return JobPauseInterrupted
	case isSessionFailure(cause):
		return JobPauseSessionFailure
	case isOutputFailure(cause):
		return JobPauseOutputFailure
	default:
		return JobPauseTransportFailure
	}
}

func fileRetireReason(cause error) FileRetireReason {
	if errors.Is(cause, content.ErrRevisionDrift) || errors.Is(cause, ErrBlockInvalidated) {
		return FileRetireInvalidatedRevision
	}
	var permanent interface{ IsolatedPermanentSourceFailure() }
	if errors.As(cause, &permanent) {
		return FileRetireIsolatedPermanentSourceFailure
	}
	return 0
}

func validateOpenedFile(share catalog.ShareInstance, entry catalog.Entry, opened OpenedRevision) error {
	file, isFile := entry.FileID()
	descriptor := opened.Descriptor
	if !isFile || opened.LeaseID.IsZero() || descriptor.ShareInstance() != share || descriptor.FileID() != file ||
		descriptor.FileRevision().IsZero() || descriptor.ExactSize() != entry.ExpectedSize() {
		return ErrRevisionIdentity
	}
	if entry.ModifiedTime().Present() && descriptor.ModifiedTime() != entry.ModifiedTime() {
		return ErrRevisionIdentity
	}
	return nil
}

func validateOutputTransaction(
	target OutputFileTarget,
	transaction FileTransaction,
	durable VerifiedDurableRanges,
) error {
	if transaction == nil {
		return outputContractFault(nil)
	}
	binding := transaction.Binding()
	if validateOutputFileBinding(target, binding) != nil || durable.Binding() != binding {
		return outputContractFault(nil)
	}
	return nil
}

func validateImmediateFileSettlement(
	target OutputFileTarget,
	settlement FileSettlement,
) error {
	// matchesTarget already proves the settlement's kind-specific binding and
	// quarantine invariants. Immediate pause remains forbidden because it would
	// bypass the transaction that owns resumable progress.
	if !settlement.matchesTarget(target) || settlement.Kind() == FilePaused {
		return ErrOutputContract
	}
	return nil
}

func validateOutputFileBinding(
	target OutputFileTarget,
	binding OutputFileBinding,
) error {
	if !target.valid() || !binding.valid() || binding.Target() != target {
		return ErrOutputContract
	}
	return nil
}

func splitAtBlockBoundaries(ranges content.RangeSet, geometry content.FileGeometry) []content.Range {
	result := make([]content.Range, 0)
	chunk := uint64(geometry.ChunkSize())
	for _, current := range ranges.Ranges() {
		for offset := current.Offset; offset < current.End; {
			next := min(current.End, ((offset/chunk)+1)*chunk)
			result = append(result, content.Range{Offset: offset, End: next})
			offset = next
		}
	}
	return result
}

func rangeContains(ranges content.RangeSet, target content.Range) bool {
	for _, current := range ranges.Ranges() {
		if current.Offset <= target.Offset && current.End >= target.End {
			return true
		}
	}
	return false
}

func rangesContain(available, required content.RangeSet) bool {
	availableRanges := available.Ranges()
	availableIndex := 0
	for _, requiredRange := range required.Ranges() {
		for availableIndex < len(availableRanges) && availableRanges[availableIndex].End < requiredRange.End {
			availableIndex++
		}
		if availableIndex == len(availableRanges) || availableRanges[availableIndex].Offset > requiredRange.Offset ||
			availableRanges[availableIndex].End < requiredRange.End {
			return false
		}
	}
	return true
}

func (r *jobRun) traceFileSettlement(file catalog.FileID, settlement FileSettlement, failed bool) {
	r.job.trace(TransferLifecycleTrace{
		Stage: TransferFileSettled, OutputSessionID: r.output.SessionID(), ResumeIntent: r.resumeIntent,
		FileID:         file,
		FileSettlement: settlement.Kind(), Failed: failed,
	})
}

func (r *jobRun) acceptFileSettlement(settlement FileSettlement) {
	if settlement.Kind() == FilePublishBlocked || settlement.Kind() == FileQuarantined {
		r.needsAttention = true
	}
}

type SessionFailureError struct{ cause error }

// IsolatedPermanentSourceFailureError is explicit retirement authority. A raw
// range error is intentionally insufficient because it may be retryable
// transport failure or may have originated in the output sink.
type IsolatedPermanentSourceFailureError struct{ cause error }

func NewIsolatedPermanentSourceFailure(cause error) error {
	if cause == nil {
		cause = errors.New("file source failed permanently")
	}
	return &IsolatedPermanentSourceFailureError{cause: cause}
}

func (failure *IsolatedPermanentSourceFailureError) Error() string {
	return fmt.Sprintf("transfer isolated permanent source failure: %v", failure.cause)
}
func (failure *IsolatedPermanentSourceFailureError) Unwrap() error { return failure.cause }
func (failure *IsolatedPermanentSourceFailureError) IsolatedPermanentSourceFailure() {
}

func NewSessionFailure(cause error) error {
	if cause == nil {
		cause = errors.New("protocol session failed")
	}
	return &SessionFailureError{cause: cause}
}

func (e *SessionFailureError) Error() string   { return fmt.Sprintf("transfer session: %v", e.cause) }
func (e *SessionFailureError) Unwrap() error   { return e.cause }
func (e *SessionFailureError) SessionFailure() {}

func isSessionFailure(err error) bool {
	var scoped interface{ SessionFailure() }
	return errors.As(err, &scoped) || errors.Is(err, protocolsession.ErrSessionTerminated) ||
		errors.Is(err, protocolsession.ErrPeerSessionTerminal) || errors.Is(err, protocolsession.ErrWriterTerminal) ||
		errors.Is(err, protocolsession.ErrWriterStopped) || errors.Is(err, ErrLaneClosed)
}

func IsSessionFailure(err error) bool { return isSessionFailure(err) }

// JobResourceBudgetError terminates one transfer because a local, bounded
// resource policy was exhausted. It must not be attributed to the peer session.
type JobResourceBudgetError struct{ cause error }

func NewJobResourceBudgetError(cause error) error {
	if cause == nil {
		cause = errors.New("transfer job resource budget exceeded")
	}
	return &JobResourceBudgetError{cause: cause}
}

func (e *JobResourceBudgetError) Error() string {
	return fmt.Sprintf("transfer job resource budget: %v", e.cause)
}
func (e *JobResourceBudgetError) Unwrap() error { return e.cause }
func (e *JobResourceBudgetError) JobFatal()     {}

// JobDependencyContractError is a local collaborator breach, not peer fault.
type JobDependencyContractError struct{ cause error }

func NewJobDependencyContractError(cause error) error {
	if cause == nil {
		cause = errors.New("transfer job dependency contract violated")
	}
	return &JobDependencyContractError{cause: cause}
}

func (e *JobDependencyContractError) Error() string {
	return fmt.Sprintf("transfer job dependency contract: %v", e.cause)
}
func (e *JobDependencyContractError) Unwrap() error { return e.cause }
func (e *JobDependencyContractError) JobFatal()     {}

func isJobFatal(err error) bool {
	var fatal interface{ JobFatal() }
	return errors.As(err, &fatal)
}

func isJobTerminalError(err error) bool {
	return isSessionFailure(err) || isJobFatal(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
