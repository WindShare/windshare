package transfer

import (
	"context"
	"errors"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

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
	inspection := inspectLifecycleError(cause)
	if !retireReason.valid() || inspection.jobTerminal() || inspection.outputFailure || inspection.exhausted {
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
	r.recordFileFailure(failure)
	r.traceFileSettlement(plan, settlement, settlementErr != nil)
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
	inspection := inspectLifecycleError(cause)
	isolatedOutputFailure := inspection.outputCanContinueAfterFileSettlement(r.output.Capabilities())
	isolatedSourceFailure := inspection.isolatedFileSource && !inspection.jobTerminal() &&
		!inspection.outputFailure
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
	r.recordFileFailure(FileJobFailure{
		FileID: plan.file, Path: plan.path, Stage: stage, Cause: cause,
		Settlement: settlement, SettlementFailure: settlementErr,
		LeaseReleaseFailure: releaseErr,
	})
	r.traceFileSettlement(plan, settlement, settlementErr != nil)
	if settlementErr != nil {
		r.settlementFailure = errors.Join(r.settlementFailure, settlementErr)
		return errors.Join(cause, releaseErr)
	}
	if isJobTerminalError(releaseErr) {
		return errors.Join(cause, releaseErr)
	}
	// A verified file-local pause restores the output session invariant. Keeping
	// the original fault in JobResult is sufficient; returning it here would
	// incorrectly turn an isolated transaction into a worker-wide cancellation.
	if isolatedOutputFailure || isolatedSourceFailure {
		return nil
	}
	return errors.Join(cause, releaseErr)
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
		r.recordFileFailure(FileJobFailure{
			FileID: plan.file, Path: plan.path, Stage: FailureFileOutput,
			Cause: settlementErr, Settlement: settlement, SettlementFailure: settlementErr,
			LeaseReleaseFailure: releaseErr,
		})
		r.traceFileSettlement(plan, settlement, true)
		// Commit is the transaction's terminal publication operation. Once its
		// settlement cannot be proven, no sibling or directory finalization may
		// advance the same job namespace before PauseJob takes ownership.
		return errors.Join(settlementErr, releaseErr)
	}
	if !settlement.matchesCommittedOutput(transaction.Binding(), r.output.Capabilities()) || settlement.Kind() != FilePublished &&
		settlement.Kind() != FilePublishBlocked && settlement.Kind() != FileQuarantined {
		return r.rejectCommitSettlement(ctx, plan, opened, settlement)
	}
	r.acceptFileSettlement(settlement)
	releaseErr := r.releaseRevision(ctx, opened.LeaseID)
	r.traceFileSettlement(plan, settlement, releaseErr != nil)
	switch settlement.Kind() {
	case FilePublished:
		r.succeeded++
		r.job.tracker.completeFile(plan.expectedSize)
		if releaseErr != nil {
			r.recordFileFailure(FileJobFailure{
				FileID: plan.file, Path: plan.path, Stage: FailureLeaseRelease, Cause: releaseErr,
			})
		}
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
	r.recordFileFailure(FileJobFailure{
		FileID: plan.file, Path: plan.path, Stage: FailureFileOutput, Cause: contractFailure,
		Settlement: settlement, SettlementFailure: contractFailure,
		LeaseReleaseFailure: releaseErr,
	})
	r.traceFileSettlement(plan, settlement, true)
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
	directories, files, omittedDirectories, omittedFiles, sourceDriftFailure := r.failureSnapshot()
	if sourceDriftFailure == nil && isSourceDriftFailure(r.terminationCause) {
		sourceDriftFailure = r.terminationCause
	}
	outcome := JobSucceeded
	if len(directories) != 0 || len(files) != 0 || omittedDirectories != 0 || omittedFiles != 0 ||
		r.selectionResolutionFailure != nil || sourceDriftFailure != nil {
		outcome = JobCompletedWithErrors
	}
	outcome = r.settleJob(ctx, outcome)
	return JobResult{
		Outcome: outcome, Settlement: r.settlement,
		TransferJobID: r.job.jobID, IntentDigest: r.job.intent.Digest(), TransferIntent: r.job.intent,
		SelectionObservation: r.selectionObservation, SelectionIdentity: r.selectionIdentity,
		Measure: r.job.Measure(), Directories: directories, Files: files,
		OmittedDirectoryFailures: omittedDirectories, OmittedFileFailures: omittedFiles,
		SelectionResolutionFailure: r.selectionResolutionFailure,
		SourceDriftFailure:         sourceDriftFailure,
		SucceededFiles:             r.succeeded, TerminationCause: r.terminationCause,
		SettlementFailure: r.settlementFailure,
	}
}

func (r *jobRun) settleJob(ctx context.Context, outcome JobOutcome) JobOutcome {
	failed := r.terminationCause != nil || r.settlementFailure != nil
	if !r.admitted {
		if failed {
			return JobPausedOutcome
		}
		return outcome
	}
	if failed {
		r.pauseJob(ctx)
		return JobPausedOutcome
	}
	r.completeJob(ctx, outcome)
	if r.settlementFailure != nil {
		return JobPausedOutcome
	}
	return outcome
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
		Stage: TransferJobSettled, OutputSessionID: r.output.SessionID(),
		SelectionObservation: r.selectionObservation,
		JobSettlement:        settlement.Kind(), Failed: err != nil,
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
			Stage: TransferJobSettled, OutputSessionID: r.output.SessionID(),
			SelectionObservation: r.selectionObservation,
			JobSettlement:        settlement.Kind(), Failed: true,
		})
		return
	}
	r.settlement = settlement
	r.job.trace(TransferLifecycleTrace{
		Stage: TransferJobSettled, OutputSessionID: r.output.SessionID(),
		SelectionObservation: r.selectionObservation,
		JobSettlement:        settlement.Kind(),
	})
}

func (j *TransferJob) newSettlementContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), j.settlementTimeout)
}

func filePauseReason(cause error) FilePauseReason {
	inspection := inspectLifecycleError(cause)
	switch {
	case inspection.interrupted:
		return FilePauseInterrupted
	case inspection.jobTerminalSession():
		return FilePauseSessionFailure
	case inspection.outputFailure:
		return FilePauseOutputFailure
	case inspection.jobResourceBudget:
		return FilePauseResourceBudget
	case inspection.jobDependencyContract:
		return FilePauseDependencyContract
	default:
		return FilePauseTransportFailure
	}
}

func jobPauseReason(cause, settlementFailure error) JobPauseReason {
	if settlementFailure != nil {
		return JobPauseOutputFailure
	}
	inspection := inspectLifecycleError(cause)
	switch {
	case inspection.interrupted:
		return JobPauseInterrupted
	case inspection.jobTerminalSession():
		return JobPauseSessionFailure
	case inspection.outputFailure:
		return JobPauseOutputFailure
	case inspection.jobResourceBudget:
		return JobPauseResourceBudget
	case inspection.jobDependencyContract:
		return JobPauseDependencyContract
	default:
		return JobPauseTransportFailure
	}
}

type isolatedPermanentSourceFailure interface {
	error
	IsolatedPermanentSourceFailure()
}

type isolatedFileSourceFailure interface {
	error
	IsolatedFileSourceFailure()
}

func fileRetireReason(cause error) FileRetireReason {
	return inspectLifecycleError(cause).retireReason()
}

func validateOpenedFile(share catalog.ShareInstance, entry catalog.Entry, opened OpenedRevision) error {
	file, isFile := entry.FileID()
	if !isFile {
		return ErrRevisionIdentity
	}
	return validateOpenedPlanFile(share, file, entry.ExpectedSize(), entry.ModifiedTime(), opened)
}

func validateOpenedPlanFile(
	share catalog.ShareInstance,
	file catalog.FileID,
	expectedSize uint64,
	modified catalog.ModifiedTime,
	opened OpenedRevision,
) error {
	descriptor := opened.Descriptor
	if file.IsZero() || opened.LeaseID.IsZero() || descriptor.ShareInstance() != share ||
		descriptor.FileID() != file || descriptor.FileRevision().IsZero() ||
		descriptor.ExactSize() != expectedSize {
		return ErrRevisionIdentity
	}
	if modified.Present() && descriptor.ModifiedTime() != modified {
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

func checkpointExactlyAdvances(
	transaction FileTransaction,
	prior VerifiedDurableRanges,
	requested content.Range,
	next VerifiedDurableRanges,
) bool {
	requestedSet, err := content.NewRangeSet([]content.Range{requested})
	if err != nil {
		return false
	}
	expected, err := MergeRanges(prior.Ranges(), requestedSet)
	return err == nil && next.Binding() == transaction.Binding() &&
		next.CheckpointGeneration() > prior.CheckpointGeneration() &&
		exactRangeSetsEqual(next.Ranges(), expected)
}

func checkpointAcknowledgesTransientWrite(
	transaction FileTransaction,
	prior VerifiedDurableRanges,
	next VerifiedDurableRanges,
) bool {
	return next.Binding() == transaction.Binding() && next.Ranges().IsEmpty() &&
		next.CheckpointGeneration() > prior.CheckpointGeneration()
}

func exactRangeSetsEqual(left, right content.RangeSet) bool {
	leftRanges, rightRanges := left.Ranges(), right.Ranges()
	if len(leftRanges) != len(rightRanges) {
		return false
	}
	for index := range leftRanges {
		if leftRanges[index] != rightRanges[index] {
			return false
		}
	}
	return true
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

func (r *jobRun) traceFileSettlement(plan plannedFile, settlement FileSettlement, failed bool) {
	r.job.trace(TransferLifecycleTrace{
		Stage: TransferFileSettled, OutputSessionID: r.output.SessionID(),
		SelectionObservation: r.selectionObservation,
		DirectoryID:          plan.parentDirectory,
		DirectoryGeneration:  plan.parentGeneration,
		FileID:               plan.file,
		FileSelection:        plan.selectionDecision,
		FileSettlement:       settlement.Kind(), Failed: failed,
	})
}

func (r *jobRun) acceptFileSettlement(settlement FileSettlement) {
	if settlement.Kind() == FilePublishBlocked || settlement.Kind() == FileQuarantined {
		r.needsAttention = true
	}
}
