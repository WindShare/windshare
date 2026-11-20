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
	opened, stage, err := r.openSelectedRevision(ctx, plan)
	var retry *fileRevisionRetry
	for {
		if err != nil {
			return r.rejectFileAdmission(plan, retry, stage, err)
		}
		retry, err = r.transferFileAttempt(ctx, plan, opened, retry)
		if retry == nil {
			return err
		}
		// The paused transaction handed back the same handle with its already
		// acquired revision. Recheck the catalog before opening the new output.
		opened.Descriptor = retry.reopened.Current()
		opened, stage, err = r.admitOpenedRevision(ctx, plan, opened, nil)
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
		return nil, r.admitImmediateSettlement(ctx, plan, opened, target, settlement, retry)
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
	fileProgress.recovered = r.reconcileFileCoverage(retry, fileProgress.initial, fileProgress.persistedBytes, recovered)
	if retry != nil {
		reused := overlapBytes(fileProgress.initial, retry.progress.trusted.Ranges())
		r.traceFileRevisionRetry(plan, retry.reopened, "readmitted", retry.progress.persistedBytes, reused, retry.started)
	}
	r.traceFileLifecycle(TransferFileAdmitted, plan, nil)
	completed, err := r.transferMissingRanges(ctx, plan, opened, transaction, &fileProgress)
	if reopened, ok := errors.AsType[*content.RevisionReopenedError](err); ok {
		return r.settleReopenedRevision(ctx, plan, opened, transaction, &fileProgress, reopened, retry)
	}
	if err != nil || !completed {
		return nil, err
	}
	return nil, r.commitTransferredFile(ctx, plan, opened, transaction, &fileProgress)
}

func (r *jobRun) admitImmediateSettlement(
	ctx context.Context,
	plan plannedFile,
	opened OpenedRevision,
	target FileMaterializationTarget,
	settlement FileSettlement,
	retry *fileRevisionRetry,
) error {
	if err := validateImmediateFileSettlement(target, settlement); err != nil {
		contractFailure := outputContractFault(err)
		r.traceFileLifecycle(TransferFileAdmitted, plan, contractFailure)
		return r.rejectImmediateSettlement(ctx, plan, opened, settlement, contractFailure)
	}
	if err := r.reconcileImmediateSettlementCoverage(plan, settlement, retry); err != nil {
		r.traceFileLifecycle(TransferFileAdmitted, plan, err)
		return r.rejectImmediateSettlement(ctx, plan, opened, settlement, err)
	}
	r.traceFileLifecycle(TransferFileAdmitted, plan, nil)
	return r.handleImmediateSettlement(ctx, plan, opened, settlement)
}

func (r *jobRun) reconcileImmediateSettlementCoverage(
	plan plannedFile,
	settlement FileSettlement,
	retry *fileRevisionRetry,
) error {
	if checkpoint, ok := settlement.VerifiedProgress(); ok {
		recovered, exact := rangeSetByteCount(checkpoint.Ranges())
		if !exact || recovered > plan.expectedSize {
			return outputContractFault(ErrOutputContract)
		}
		r.reconcileFileCoverage(retry, checkpoint.Ranges(), 0, recovered)
		return nil
	}
	if retry == nil {
		return nil
	}
	if _, reason, blocked := settlement.ItemBlock(); blocked &&
		(reason == ItemBlockOwnedObjectUnknown || reason == ItemBlockOwnershipUnknown) {
		// The pause proved durable coverage at the time. The new
		// ownership result withdraws it for this file alone.
		r.reconcileFileCoverage(retry, content.RangeSet{}, 0, 0)
	}
	r.traceFileRevisionRetry(plan, retry.reopened, "admission-blocked", retry.progress.persistedBytes, 0, retry.started)
	return nil
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

// Allow one readmission with zero newly persisted bytes (for example a reconnect
// before the first durable range flushes); stop once a second consecutive reopen
// still makes no durable progress.
const maxStalledRevisionReadmissions = 1

// Readmission continues across routine reconnects as long as durable coverage
// advances, and stops once repeated reopens make no progress.
type fileRevisionRetry struct {
	reopened       *content.RevisionReopenedError
	progress       fileTransferProgress
	settlement     FileSettlement
	started        time.Time
	stalledReopens int
}

func (r *jobRun) settleReopenedRevision(
	ctx context.Context, plan plannedFile, opened OpenedRevision, transaction FileTransaction,
	progress *fileTransferProgress, reopened *content.RevisionReopenedError, priorRetry *fileRevisionRetry,
) (*fileRevisionRetry, error) {
	started := time.Now()
	cause := normalizeSourceBoundary(ctx, reopened)
	if ctx.Err() != nil {
		cause = failurestate.FromCancellation(ctx, context.Cause(ctx))
	}
	priorStalled := 0
	if priorRetry != nil {
		priorStalled = priorRetry.stalledReopens
	}
	initialBytes, _ := rangeSetByteCount(progress.initial)
	trustedBytes, _ := rangeSetByteCount(progress.trusted.Ranges())
	nextStalled := 0
	if trustedBytes <= initialBytes {
		nextStalled = priorStalled + 1
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
	case nextStalled > maxStalledRevisionReadmissions:
		decision = "retry-limit"
	case !progress.retainsRanges || r.output.Capabilities().Durability == DurabilityNone:
		decision = "output-not-resumable"
	default:
		reason, decision = FilePauseRevisionReopened, "readmit"
	}
	// The range reader has drained before returning, and no output call remains
	// active. A durable pause is required before handing the lease to a new writer.
	settlement, err := r.pauseFileTransaction(ctx, transaction, reason, progress)
	if err != nil {
		decision = "settlement-failed"
	} else if settlement.Kind() == FilePaused && reason == FilePauseRevisionReopened {
		// Base stall accounting on durable bytes confirmed by pause settlement,
		// not merely writes buffered before checkpoint persistence.
		if progress.persistedBytes > initialBytes {
			nextStalled = 0
		} else {
			nextStalled = priorStalled + 1
		}
		if nextStalled > maxStalledRevisionReadmissions {
			decision = "retry-limit"
		}
	}
	r.traceFileRevisionRetry(plan, reopened, decision, progress.persistedBytes, 0, started)
	if err == nil && settlement.Kind() == FilePaused && decision == "readmit" {
		return &fileRevisionRetry{
			reopened: reopened, progress: *progress, settlement: settlement, started: started,
			stalledReopens: nextStalled,
		}, nil
	}

	r.releaseRevision(ctx, plan, opened.Handle)
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
		r.traceFileRevisionRetry(plan, retry.reopened, "admission-failed", retry.progress.persistedBytes, 0, retry.started)
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

func (r *jobRun) reconcileFileCoverage(
	retry *fileRevisionRetry, admitted content.RangeSet, persisted, verified uint64,
) content.RangeSet {
	if retry == nil {
		r.job.progress.addRecoveredVerified(verified)
		r.job.progress.replaceRestartReusable(0, persisted)
		return admitted
	}
	prior := retry.progress.trusted.Ranges()
	priorTotal, _ := rangeSetByteCount(prior)
	priorNewly := subtractRanges(prior, retry.progress.recovered)
	previousNewly, _ := rangeSetByteCount(priorNewly)
	// Bytes written earlier in this same task stay newly verified only where
	// the new output admission still covers them; any other admitted range
	// remains pre-existing recovered coverage.
	keptNewly := intersectRanges(admitted, priorNewly)
	newly, _ := rangeSetByteCount(keptNewly)
	r.job.progress.replaceReadmittedCoverage(
		priorTotal, previousNewly, retry.progress.persistedBytes, verified, newly, persisted,
	)
	return subtractRanges(admitted, keptNewly)
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

func intersectRanges(left, right content.RangeSet) content.RangeSet {
	a, b := left.Ranges(), right.Ranges()
	if len(a) == 0 || len(b) == 0 {
		return content.RangeSet{}
	}
	var result []content.Range
	for i, j := 0, 0; i < len(a) && j < len(b); {
		start, end := max(a[i].Offset, b[j].Offset), min(a[i].End, b[j].End)
		if end > start {
			result = append(result, content.Range{Offset: start, End: end})
		}
		if a[i].End <= b[j].End {
			i++
		} else {
			j++
		}
	}
	out, _ := content.NewRangeSet(result)
	return out
}

func subtractRanges(left, right content.RangeSet) content.RangeSet {
	a, b := left.Ranges(), right.Ranges()
	if len(a) == 0 || len(b) == 0 {
		return left
	}
	var result []content.Range
	j := 0
	for _, current := range a {
		cursor := current.Offset
		for j < len(b) && b[j].End <= cursor {
			j++
		}
		for j < len(b) && b[j].Offset < current.End {
			if b[j].Offset > cursor {
				result = append(result, content.Range{Offset: cursor, End: b[j].Offset})
			}
			if b[j].End >= current.End {
				cursor = current.End
				break
			}
			cursor = b[j].End
			j++
		}
		if cursor < current.End {
			result = append(result, content.Range{Offset: cursor, End: current.End})
		}
	}
	out, _ := content.NewRangeSet(result)
	return out
}
