package transfer

import (
	"context"
	"errors"
	"time"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer/internal/failurestate"
)

func (r *jobRun) transferPlannedFile(ctx context.Context, plan plannedFile) error {
	if handled, err := r.skipDeliveredFile(ctx, plan); handled || err != nil {
		return err
	}
	var retry *fileRevisionRetry
	for {
		opened, stage, err := r.openSelectedRevision(ctx, plan)
		if err != nil {
			return r.rejectFileAdmission(plan, retry, stage, err)
		}
		if retry != nil {
			retry.current = opened.Descriptor
			retry.comparison = content.CompareRevisions(retry.previous, opened.Descriptor)
			if !retry.comparison.AllowsReuse() {
				r.releaseRevision(ctx, plan, opened.Handle)
				err := normalizeSourceBoundary(ctx, content.NewRevisionReopenedError(retry.previous, opened.Descriptor))
				return r.rejectFileAdmission(plan, retry, FailureRevisionIdentity, err)
			}
		}
		retry, err = r.transferFileAttempt(ctx, plan, opened, retry)
		if retry == nil {
			return err
		}
	}
}

func (r *jobRun) transferFileAttempt(
	ctx context.Context, plan plannedFile, opened OpenedRevision, retry *fileRevisionRetry,
) (*fileRevisionRetry, error) {
	file, err := newMaterializationFile(
		r.job.coordinates, plan.sourcePath, plan.materializationRelativePath,
		opened.Descriptor, r.output.SessionID(), plan.parent,
	)
	if err != nil || file.ArtifactPath() != plan.artifactPath || file.ExpectedSize() != plan.expectedSize {
		return nil, r.rejectUnstartedFile(ctx, plan, opened, failurestate.DependencyContract(errors.Join(ErrOutputContract, err)))
	}
	target := file.Target()
	start, err := r.output.BeginFile(ctx, file)
	err = normalizeOutputBoundary(ctx, err)
	if err != nil {
		r.traceFileLifecycle(TransferFileAdmitted, plan, err)
		r.releaseRevision(ctx, plan, opened.Handle)
		return nil, r.rejectFileAdmission(plan, retry, FailureFileOutput, err)
	}
	if settlement, immediate := start.ImmediateSettlement(); immediate {
		if err := validateImmediateFileSettlement(target, settlement); err != nil {
			contractFailure := outputContractFault(err)
			r.traceFileLifecycle(TransferFileAdmitted, plan, contractFailure)
			return nil, r.rejectImmediateSettlement(ctx, plan, opened, settlement, contractFailure)
		}
		if checkpoint, ok := settlement.VerifiedProgress(); ok {
			recovered, exact := rangeSetByteCount(checkpoint.Ranges())
			if !exact || recovered > plan.expectedSize {
				contractFailure := outputContractFault(ErrOutputContract)
				r.traceFileLifecycle(TransferFileAdmitted, plan, contractFailure)
				return nil, r.rejectImmediateSettlement(ctx, plan, opened, settlement, contractFailure)
			}
			r.admitFileCoverage(retry, checkpoint.Ranges(), 0, recovered)
		} else if retry != nil {
			r.traceFileRevisionRetry(plan, retry.previous, opened.Descriptor, "admission-blocked", retry.comparison, retry.progress.persistedBytes, 0, retry.started)
		}
		r.traceFileLifecycle(TransferFileAdmitted, plan, nil)
		return nil, r.handleImmediateSettlement(ctx, plan, opened, settlement)
	}
	transaction, durable, transactional := start.Transaction()
	if !transactional || !start.valid() {
		contractFailure := outputContractFault(ErrOutputContract)
		r.traceFileLifecycle(TransferFileAdmitted, plan, contractFailure)
		return nil, r.rejectImmediateSettlement(ctx, plan, opened, start.settlement, contractFailure)
	}
	if err := validateOutputTransaction(target, transaction, durable); err != nil {
		r.traceFileLifecycle(TransferFileAdmitted, plan, err)
		return nil, r.settleFailedFile(ctx, plan, opened, transaction, FailureFileOutput, err, 0, nil)
	}
	fileProgress, recovered, validProgress := newFileTransferProgress(
		transaction, durable, r.output.Capabilities().Durability != DurabilityNone,
	)
	if !validProgress {
		contractFailure := outputContractFault(ErrOutputContract)
		r.traceFileLifecycle(TransferFileAdmitted, plan, contractFailure)
		return nil, r.settleFailedFile(
			ctx, plan, opened, transaction, FailureFileOutput, contractFailure, 0, nil,
		)
	}
	r.admitFileCoverage(retry, fileProgress.initial, fileProgress.persistedBytes, recovered)
	if retry != nil {
		reused := overlapBytes(fileProgress.initial, retry.progress.trusted.Ranges())
		r.traceFileRevisionRetry(plan, retry.previous, opened.Descriptor, "readmitted", retry.comparison, retry.progress.persistedBytes, reused, retry.started)
	}
	r.traceFileLifecycle(TransferFileAdmitted, plan, nil)
	completed, err := r.transferMissingRanges(ctx, plan, opened, transaction, &fileProgress)
	if reopened, ok := err.(*content.RevisionReopenedError); ok {
		return r.settleReopenedRevision(ctx, plan, opened, transaction, &fileProgress, reopened, retry != nil)
	}
	if err != nil || !completed {
		return nil, err
	}
	return nil, r.commitTransferredFile(ctx, plan, opened, transaction, &fileProgress)
}

func (r *jobRun) rejectUnstartedFile(
	ctx context.Context,
	plan plannedFile,
	opened OpenedRevision,
	cause error,
) error {
	r.releaseRevision(ctx, plan, opened.Handle)
	r.recordFileFailure(FileJobFailure{
		FileID: plan.file, Path: plan.failurePath(), Stage: FailureFileOutput, Cause: cause,
	})
	return cause
}

func (r *jobRun) rejectImmediateSettlement(
	ctx context.Context,
	plan plannedFile,
	opened OpenedRevision,
	settlement FileSettlement,
	cause error,
) error {
	fault := cause
	r.releaseRevision(ctx, plan, opened.Handle)
	r.settlementFailure = failurestate.Merge(r.settlementFailure, fault)
	r.recordFileFailure(FileJobFailure{
		FileID: plan.file, Path: plan.failurePath(), Stage: FailureFileOutput, Cause: fault,
		Settlement: settlement, SettlementFailure: fault,
	})
	r.traceFileSettlement(plan, settlement, fault)
	return fault
}

func (r *jobRun) handleImmediateSettlement(
	ctx context.Context,
	plan plannedFile,
	opened OpenedRevision,
	settlement FileSettlement,
) error {
	r.job.progress.acceptFileSettlement(settlement, plan.expectedSize)
	r.traceFileSettlement(plan, settlement, nil)
	r.releaseRevision(ctx, plan, opened.Handle)
	switch settlement.Kind() {
	case FilePublished, FilePreviouslyPublished:
		return nil
	case FileCollision:
		r.recordFileFailure(FileJobFailure{
			FileID: plan.file, Path: plan.failurePath(), Stage: FailureFileOutput,
			Cause: ErrOutputPublishBlocked, Settlement: settlement,
		})
	case FileItemBlocked:
		r.recordFileFailure(FileJobFailure{
			FileID: plan.file, Path: plan.failurePath(), Stage: FailureFileOutput,
			Cause: ErrOutputQuarantined, Settlement: settlement,
		})
	case FileFailed:
		// A recovered retirement is already-authorized durable cleanup. Preserve
		// its exact settlement without creating a transaction or attempting a
		// second settlement whose reason this run cannot authenticate.
		r.recordFileFailure(FileJobFailure{
			FileID: plan.file, Path: plan.failurePath(), Stage: FailureFileOutput,
			Cause: ErrOutputRetired, Settlement: settlement,
		})
	default:
		contractFailure := outputContractFault(nil)
		r.settlementFailure = failurestate.Merge(r.settlementFailure, contractFailure)
		return contractFailure
	}
	return nil
}

// A single readmission covers a routine reconnect without keeping a changing
// file at the head of the queue or repeatedly opening its output writer.
type fileRevisionRetry struct {
	previous, current content.FileRevisionDescriptor
	comparison        content.RevisionComparisonEvidence
	progress          fileTransferProgress
	settlement        FileSettlement
	started           time.Time
}

func (r *jobRun) settleReopenedRevision(
	ctx context.Context, plan plannedFile, opened OpenedRevision, transaction FileTransaction,
	progress *fileTransferProgress, reopened *content.RevisionReopenedError, retried bool,
) (*fileRevisionRetry, error) {
	started := time.Now()
	cause := normalizeSourceBoundary(ctx, reopened)
	if ctx.Err() != nil {
		cause = failurestate.FromCancellation(ctx, context.Cause(ctx))
	}
	comparison := reopened.Comparison()
	reason := filePauseReason(cause)
	decision := "evidence-unavailable"
	switch {
	case ctx.Err() != nil:
		decision = "canceled"
	case comparison.Comparison == content.RevisionComparisonMismatch:
		decision = "observed-change"
	case !comparison.AllowsReuse():
	case retried:
		decision = "retry-limit"
	case !progress.retainsRanges || r.output.Capabilities().Durability == DurabilityNone:
		decision = "output-not-resumable"
	default:
		reason, decision = FilePauseRevisionReopened, "readmit"
	}
	// The range reader has drained before returning, and no output call remains
	// active. Pause forces a durable cut of the old attempt before the next open.
	settlement, err := r.pauseFileTransaction(ctx, plan, opened, transaction, reason, progress)
	if err != nil {
		decision = "settlement-failed"
	}
	r.traceFileRevisionRetry(plan, opened.Descriptor, reopened.Current(), decision, comparison, progress.persistedBytes, 0, started)
	if err == nil && settlement.Kind() == FilePaused && reason == FilePauseRevisionReopened {
		return &fileRevisionRetry{
			previous: opened.Descriptor, current: reopened.Current(), comparison: comparison,
			progress: *progress, settlement: settlement, started: started,
		}, nil
	}

	r.recordFileFailure(FileJobFailure{
		FileID: plan.file, Path: plan.failurePath(), Stage: FailureBlockTransfer, Cause: cause,
		Settlement: settlement, SettlementFailure: err,
	})
	r.traceFileSettlement(plan, settlement, err)
	if err != nil {
		r.settlementFailure = failurestate.Merge(r.settlementFailure, err)
		return nil, failurestate.Join(cause, err)
	}
	r.job.progress.acceptFileSettlement(settlement, plan.expectedSize)
	if lifecyclePolicyFor(cause).jobTerminal() {
		return nil, cause
	}
	return nil, nil
}

func (r *jobRun) rejectFileAdmission(plan plannedFile, retry *fileRevisionRetry, stage FailureStage, err error) error {
	policy := lifecyclePolicyFor(err)
	failure := FileJobFailure{FileID: plan.file, Path: plan.failurePath(), Stage: stage, Cause: err}
	if retry != nil {
		failure.Settlement = retry.settlement
		r.job.progress.acceptFileSettlement(retry.settlement, plan.expectedSize)
		r.traceFileRevisionRetry(plan, retry.previous, retry.current, "admission-failed",
			retry.comparison, retry.progress.persistedBytes, 0, retry.started)
		r.traceFileSettlement(plan, retry.settlement, nil)
	}
	if retry != nil || !policy.jobTerminal() || stage == FailureFileOutput {
		r.recordFileFailure(failure)
	}
	if policy.jobTerminal() || policy.outputRequiresJobPause(r.output.Capabilities()) {
		return err
	}
	return nil
}

func (r *jobRun) admitFileCoverage(retry *fileRevisionRetry, admitted content.RangeSet, persisted, verified uint64) {
	if retry == nil {
		r.job.progress.addRecoveredVerified(verified)
		r.job.progress.replaceRestartReusable(0, persisted)
		return
	}
	prior := retry.progress.trusted.Ranges()
	priorTotal, _ := rangeSetByteCount(prior)
	initialTotal, _ := rangeSetByteCount(retry.progress.initial)
	// Bytes written earlier in this same task stay newly verified only where
	// the new output admission still covers them.
	newly := overlapBytes(admitted, prior) - overlapBytes(admitted, retry.progress.initial)
	r.job.progress.replaceReadmittedCoverage(
		priorTotal, priorTotal-initialTotal, retry.progress.persistedBytes, verified, newly, persisted,
	)
}

func overlapBytes(left, right content.RangeSet) uint64 {
	a, b := left.Ranges(), right.Ranges()
	var total uint64
	for i, j := 0, 0; i < len(a) && j < len(b); {
		start, end := max(a[i].Offset, b[j].Offset), min(a[i].End, b[j].End)
		if end > start {
			total += end - start
		}
		if a[i].End <= b[j].End {
			i++
		} else {
			j++
		}
	}
	return total
}
