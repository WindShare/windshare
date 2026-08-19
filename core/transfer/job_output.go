package transfer

import (
	"context"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer/fault"
)

type lifecyclePolicy struct {
	value    fault.Fault
	canceled bool
}

func (r *jobRun) settleFailedFile(
	ctx context.Context,
	plan plannedFile,
	opened OpenedRevision,
	transaction FileTransaction,
	stage FailureStage,
	cause error,
	retireReason FileRetireReason,
	progress *fileTransferProgress,
) error {
	policy := lifecyclePolicyFor(cause)
	if !retireReason.valid() || policy.jobTerminal() || policy.outputFailure() {
		return r.pauseFailedFile(ctx, plan, opened, transaction, stage, cause, progress)
	}
	settleContext, cancel := r.job.newSettlementContext(ctx)
	settlement, rawSettlementErr := transaction.Retire(settleContext, retireReason)
	settlementErr := normalizeOutputBoundary(settleContext, rawSettlementErr)
	cancel()
	releaseErr := r.releaseRevision(ctx, opened.LeaseID)
	failure := FileJobFailure{
		FileID: plan.file, Path: plan.failurePath(), Stage: stage, Cause: cause,
		Settlement: settlement, SettlementFailure: settlementErr,
		LeaseReleaseFailure: releaseErr,
	}
	valid := settlementErr == nil && settlement.matchesBinding(transaction.Binding()) &&
		(settlement.Kind() == FileFailed || settlement.Kind() == FileItemBlocked)
	if !valid && settlementErr == nil {
		settlementErr = outputContractFault(nil)
		failure.SettlementFailure = settlementErr
	}
	if settlementErr == nil {
		settlementErr = r.acceptTransactionFileSettlement(plan, transaction, progress, settlement)
		failure.SettlementFailure = settlementErr
	}
	r.recordFileFailure(failure)
	r.traceFileSettlement(plan, settlement, joinLifecycleFailures(settlementErr, releaseErr))
	if settlementErr != nil {
		r.settlementFailure = mergeLifecycleFailures(r.settlementFailure, settlementErr)
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
	progress *fileTransferProgress,
) error {
	policy := lifecyclePolicyFor(cause)
	isolatedOutputFailure := policy.outputCanContinueAfterFileSettlement(r.output.Capabilities())
	isolatedSourceFailure := policy.sourceFileLocal() && !policy.jobTerminal()
	reason := filePauseReason(cause)
	settleContext, cancel := r.job.newSettlementContext(ctx)
	settlement, rawSettlementErr := transaction.Pause(settleContext, reason)
	settlementErr := normalizeOutputBoundary(settleContext, rawSettlementErr)
	cancel()
	releaseErr := r.releaseRevision(ctx, opened.LeaseID)
	if settlementErr == nil && (!settlement.matchesBinding(transaction.Binding()) ||
		settlement.Kind() != FilePaused && settlement.Kind() != FileItemBlocked && settlement.Kind() != FileFailed) {
		settlementErr = outputContractFault(nil)
	}
	if settlementErr == nil {
		settlementErr = r.acceptTransactionFileSettlement(plan, transaction, progress, settlement)
	}
	r.recordFileFailure(FileJobFailure{
		FileID: plan.file, Path: plan.failurePath(), Stage: stage, Cause: cause,
		Settlement: settlement, SettlementFailure: settlementErr,
		LeaseReleaseFailure: releaseErr,
	})
	r.traceFileSettlement(plan, settlement, joinLifecycleFailures(settlementErr, releaseErr))
	if settlementErr != nil {
		r.settlementFailure = mergeLifecycleFailures(r.settlementFailure, settlementErr)
		return joinLifecycleFailures(cause, releaseErr, settlementErr)
	}
	if isJobTerminalError(releaseErr) {
		return joinLifecycleFailures(cause, releaseErr)
	}
	// A verified file-local pause restores the output session invariant. Keeping
	// the original fault in JobResult is sufficient; returning it here would
	// incorrectly turn an isolated transaction into a worker-wide cancellation.
	if isolatedOutputFailure || isolatedSourceFailure {
		return nil
	}
	return joinLifecycleFailures(cause, releaseErr)
}

func (r *jobRun) commitTransferredFile(
	ctx context.Context,
	plan plannedFile,
	opened OpenedRevision,
	transaction FileTransaction,
	progress *fileTransferProgress,
) error {
	if cause := context.Cause(ctx); cause != nil {
		return r.pauseFailedFile(
			ctx, plan, opened, transaction, FailureFileOutput, cancellationFailure(ctx, cause), progress,
		)
	}
	settleContext, cancel := r.job.newSettlementContext(ctx)
	settlement, rawSettlementErr := transaction.Commit(settleContext)
	settlementErr := normalizeOutputBoundary(settleContext, rawSettlementErr)
	cancel()
	if settlementErr != nil {
		r.settlementFailure = mergeLifecycleFailures(r.settlementFailure, settlementErr)
		releaseErr := r.releaseRevision(ctx, opened.LeaseID)
		r.recordFileFailure(FileJobFailure{
			FileID: plan.file, Path: plan.failurePath(), Stage: FailureFileOutput,
			Cause: settlementErr, Settlement: settlement, SettlementFailure: settlementErr,
			LeaseReleaseFailure: releaseErr,
		})
		r.traceFileSettlement(plan, settlement, joinLifecycleFailures(settlementErr, releaseErr))
		// Commit is the transaction's terminal publication operation. Once its
		// settlement cannot be proven, no sibling or directory finalization may
		// advance the same job namespace before PauseTree takes ownership.
		return joinLifecycleFailures(settlementErr, releaseErr)
	}
	if !settlement.matchesCommittedOutput(transaction.Binding(), r.output.Capabilities()) || settlement.Kind() != FilePublished &&
		settlement.Kind() != FileCollision && settlement.Kind() != FileItemBlocked {
		return r.rejectCommitSettlement(ctx, plan, opened, settlement)
	}
	if err := r.acceptTransactionFileSettlement(plan, transaction, progress, settlement); err != nil {
		return r.rejectCommitSettlement(ctx, plan, opened, settlement)
	}
	releaseErr := r.releaseRevision(ctx, opened.LeaseID)
	r.traceFileSettlement(plan, settlement, releaseErr)
	switch settlement.Kind() {
	case FilePublished:
		if releaseErr != nil {
			r.recordFileFailure(FileJobFailure{
				FileID: plan.file, Path: plan.failurePath(), Stage: FailureLeaseRelease, Cause: releaseErr,
			})
		}
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
	r.settlementFailure = mergeLifecycleFailures(r.settlementFailure, contractFailure)
	r.recordFileFailure(FileJobFailure{
		FileID: plan.file, Path: plan.failurePath(), Stage: FailureFileOutput, Cause: contractFailure,
		Settlement: settlement, SettlementFailure: contractFailure,
		LeaseReleaseFailure: releaseErr,
	})
	r.traceFileSettlement(plan, settlement, joinLifecycleFailures(contractFailure, releaseErr))
	return joinLifecycleFailures(contractFailure, releaseErr)
}

func (r *jobRun) releaseRevision(ctx context.Context, lease content.LeaseID) error {
	settleContext, cancel := r.job.newSettlementContext(ctx)
	rawReleaseErr := r.job.revisions.ReleaseRevision(settleContext, lease)
	err := normalizeSourceBoundary(settleContext, rawReleaseErr)
	cancel()
	return err
}

func (r *jobRun) finish(ctx context.Context) JobResult {
	if r.terminationCause == nil && r.settlementFailure == nil {
		if cause := context.Cause(ctx); cause != nil {
			r.terminationCause = cancellationFailure(ctx, cause)
		}
	}
	directories, files, omittedDirectories, omittedFiles, sourceDriftFailure := r.failureSnapshot()
	if sourceDriftFailure == nil && r.terminationCause != nil && r.terminationCause.policy.sourceDrift() {
		sourceDriftFailure = r.terminationCause
	}
	outcome := DirectTreeOutcomeSuccess
	if len(directories) != 0 || len(files) != 0 || omittedDirectories != 0 || omittedFiles != 0 ||
		r.selectionResolutionFailure != nil || sourceDriftFailure != nil || !r.selectionFullyPublished() {
		outcome = DirectTreeOutcomePartial
	}
	outcome = r.settleJob(ctx, outcome)
	progress := r.job.Progress()
	return JobResult{
		Outcome: outcome, Settlement: r.settlement,
		TransferJobID: r.job.jobID, ReceiveIntentDigest: r.job.intent.Digest(), ReceiveIntent: r.job.intent,
		SelectionObservation: r.selectionObservation,
		Progress:             progress, Directories: directories, Files: files,
		OmittedDirectoryFailures: omittedDirectories, OmittedFileFailures: omittedFiles,
		SelectionResolutionFailure: r.selectionResolutionFailure,
		SourceDriftFailure:         lifecycleError(sourceDriftFailure),
		SourceDriftFault:           closedLifecycleFault(sourceDriftFailure),
		SucceededFiles:             progress.PublishedFiles,
		TerminationCause:           lifecycleError(r.terminationCause),
		TerminationFault:           closedLifecycleFault(r.terminationCause),
		TerminationInterruption:    closedLifecycleInterruption(r.terminationCause),
		SettlementFailure:          lifecycleError(r.settlementFailure),
		SettlementFault:            closedLifecycleFault(r.settlementFailure),
		SettlementInterruption:     closedLifecycleInterruption(r.settlementFailure),
	}
}

// Success is a durable claim, not a presentation-layer count heuristic. The
// reducer requires one published settlement for every discovered file before
// FinalizeTree may retire the active operation.
func (r *jobRun) selectionFullyPublished() bool {
	progress := r.job.Progress()
	return progress.Discovery == DiscoveryComplete && progress.CountersExact &&
		progress.DiscoveredFiles == progress.PublishedFiles &&
		progress.DiscoveredBytes == progress.PublishedBytes &&
		progress.PublishedFiles == progress.FileOutcomes.PublishedFiles()
}

func (r *jobRun) settleJob(ctx context.Context, outcome DirectTreeOutcome) DirectTreeOutcome {
	failed := r.terminationCause != nil || r.settlementFailure != nil
	if !r.admitted {
		if failed {
			return DirectTreeOutcomePaused
		}
		return outcome
	}
	if failed {
		r.pauseJob(ctx)
		if r.settlement.Kind() == DirectTreeSettlementFailed {
			return DirectTreeOutcomeFailed
		}
		return DirectTreeOutcomePaused
	}
	r.completeJob(ctx, outcome)
	if r.settlementFailure != nil {
		return DirectTreeOutcomePaused
	}
	if r.settlement.Kind() == DirectTreeSettlementFailed {
		return DirectTreeOutcomeFailed
	}
	return outcome
}

func (r *jobRun) pauseJob(ctx context.Context) {
	reason := jobPauseReason(r.terminationCause, r.settlementFailure)
	settleContext, cancel := r.job.newSettlementContext(ctx)
	settlement, rawSettlementErr := r.output.PauseTree(settleContext, reason)
	err := normalizeOutputBoundary(settleContext, rawSettlementErr)
	cancel()
	if err == nil {
		if settlement.Kind() != DirectTreeSettlementPaused && settlement.Kind() != DirectTreeSettlementFailed ||
			r.needsAttention && settlement.Kind() != DirectTreeSettlementFailed {
			err = outputContractFault(nil)
		}
	}
	if err != nil {
		r.settlementFailure = mergeLifecycleFailures(r.settlementFailure, err)
	} else {
		r.settlement = settlement
	}
	r.job.trace(TransferLifecycleTrace{
		Stage: TransferJobSettled, OutputSessionID: r.output.SessionID(),
		DirectTreeSettlement: settlement.Kind(),
		Fault: fault.Join(
			closedLifecycleFault(r.terminationCause), closedLifecycleFault(r.settlementFailure),
		),
		Failed: err != nil,
	})
}

func (r *jobRun) completeJob(ctx context.Context, outcome DirectTreeOutcome) {
	settleContext, cancel := r.job.newSettlementContext(ctx)
	settlement, rawSettlementErr := r.output.FinalizeTree(settleContext, outcome)
	err := normalizeOutputBoundary(settleContext, rawSettlementErr)
	cancel()
	if err == nil {
		expected := DirectTreeSettlementSuccess
		if outcome == DirectTreeOutcomePartial {
			expected = DirectTreeSettlementPartial
		}
		if outcome != DirectTreeOutcomeSuccess && outcome != DirectTreeOutcomePartial ||
			settlement.Kind() != expected && settlement.Kind() != DirectTreeSettlementFailed ||
			r.needsAttention && settlement.Kind() != DirectTreeSettlementFailed {
			err = outputContractFault(nil)
		}
	}
	if err != nil {
		r.settlementFailure = mergeLifecycleFailures(r.settlementFailure, err)
		r.job.trace(TransferLifecycleTrace{
			Stage: TransferJobSettled, OutputSessionID: r.output.SessionID(),
			DirectTreeSettlement: settlement.Kind(),
			Fault:                closedFault(err),
			Interruption:         closedInterruption(err),
			Failed:               true,
		})
		return
	}
	r.settlement = settlement
	r.job.trace(TransferLifecycleTrace{
		Stage: TransferJobSettled, OutputSessionID: r.output.SessionID(),
		DirectTreeSettlement: settlement.Kind(),
	})
}

func (j *TransferJob) newSettlementContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), j.settlementTimeout)
}

func filePauseReason(cause error) FilePauseReason {
	policy := lifecyclePolicyFor(cause)
	switch {
	case policy.canceled:
		return FilePauseInterrupted
	case policy.jobTerminalSession():
		return FilePauseSessionFailure
	case policy.outputFailure():
		return FilePauseOutputFailure
	case isSessionCode(policy.value, fault.SessionResourceBudget):
		return FilePauseResourceBudget
	case isSessionCode(policy.value, fault.SessionDependencyContract):
		return FilePauseDependencyContract
	default:
		return FilePauseTransportFailure
	}
}

func jobPauseReason(cause, settlementFailure *lifecycleFailure) JobPauseReason {
	if settlementFailure != nil {
		return JobPauseOutputFailure
	}
	policy := lifecyclePolicy{}
	if cause != nil {
		policy = cause.policy
	}
	switch {
	case policy.canceled:
		return JobPauseInterrupted
	case policy.jobTerminalSession():
		return JobPauseSessionFailure
	case policy.outputFailure():
		return JobPauseOutputFailure
	case isSessionCode(policy.value, fault.SessionResourceBudget):
		return JobPauseResourceBudget
	case isSessionCode(policy.value, fault.SessionDependencyContract):
		return JobPauseDependencyContract
	default:
		return JobPauseTransportFailure
	}
}

func fileRetireReason(cause error) FileRetireReason {
	return lifecyclePolicyFor(cause).retireReason()
}

func lifecyclePolicyFor(err error) lifecyclePolicy {
	if err == nil {
		return lifecyclePolicy{}
	}
	if failure, ok := admitLifecycleFailure(err); ok {
		return failure.policy
	}
	// A raw value reaching policy is itself a dependency-contract breach. This
	// fail-closed projection never inspects its wrapping graph.
	return lifecyclePolicy{value: fault.DependencyContractFault()}
}

func (policy lifecyclePolicy) jobTerminal() bool {
	return policy.canceled || policy.value.Valid() && policy.value.Scope() >= fault.ScopeOutputPause
}

func (policy lifecyclePolicy) jobTerminalSession() bool {
	return policy.value.Valid() && policy.value.Scope() == fault.ScopeSessionTerminal
}

func (policy lifecyclePolicy) outputFailure() bool {
	if !policy.value.Valid() {
		return false
	}
	return policy.value.Domain() == fault.DomainOutput || policy.value.Domain() == fault.DomainCheckpoint
}

func (policy lifecyclePolicy) outputRequiresJobPause(capabilities DirectTreeCapabilities) bool {
	if policy.jobTerminal() {
		return true
	}
	if !policy.outputFailure() || policy.value.Scope() != fault.ScopeFileLocal {
		return false
	}
	return !outputCanIsolateFileFailure(capabilities)
}

func (policy lifecyclePolicy) outputCanContinueAfterFileSettlement(
	capabilities DirectTreeCapabilities,
) bool {
	return !policy.canceled && policy.outputFailure() &&
		policy.value.Scope() == fault.ScopeFileLocal && outputCanIsolateFileFailure(capabilities)
}

func outputCanIsolateFileFailure(capabilities DirectTreeCapabilities) bool {
	return capabilities.FileFailureIsolation
}

func (policy lifecyclePolicy) sourceFileLocal() bool {
	return policy.value.Valid() && policy.value.Domain() == fault.DomainSource &&
		policy.value.Scope() == fault.ScopeFileLocal
}

func (policy lifecyclePolicy) sourceDrift() bool {
	if policy.value.Domain() == fault.DomainCatalog {
		code, ok := policy.value.CatalogCode()
		return ok && code == fault.CatalogDirectoryStale
	}
	code, ok := policy.value.SourceCode()
	return ok && (code == fault.SourceRevisionChanged || code == fault.SourceRevisionInvalidated)
}

func (policy lifecyclePolicy) invalidatedRevision() bool {
	code, ok := policy.value.SourceCode()
	return ok && code == fault.SourceRevisionInvalidated
}

func (policy lifecyclePolicy) directoryDiscovery() bool {
	return policy.value.Valid() && policy.value.Domain() == fault.DomainCatalog &&
		policy.value.Scope() == fault.ScopeDirectoryLocal
}

func (policy lifecyclePolicy) retireReason() FileRetireReason {
	reason, ok := fault.RetirementFor(policy.value)
	if !ok {
		return 0
	}
	switch reason {
	case fault.RetirementPermanentSource:
		return FileRetireIsolatedPermanentSourceFailure
	case fault.RetirementInvalidatedRevision:
		return FileRetireInvalidatedRevision
	default:
		return 0
	}
}

func (r *jobRun) traceFileSettlement(plan plannedFile, settlement FileSettlement, failure error) {
	_, itemBlockReason, _ := settlement.ItemBlock()
	r.job.trace(TransferLifecycleTrace{
		Stage: TransferFileSettled, OutputSessionID: r.output.SessionID(),
		FileSelection:   plan.selectionDecision,
		FileSettlement:  settlement.Kind(),
		ItemBlockReason: itemBlockReason,
		Fault:           closedFault(failure),
		Interruption:    closedInterruption(failure),
		Failed:          failure != nil,
	})
}

func (r *jobRun) acceptTransactionFileSettlement(
	plan plannedFile,
	transaction FileTransaction,
	progress *fileTransferProgress,
	settlement FileSettlement,
) error {
	if progress == nil {
		if _, hasCheckpoint := settlement.VerifiedCheckpoint(); hasCheckpoint {
			return outputContractFault(nil)
		}
	} else {
		delta, valid := progress.reconcileSettlement(transaction, settlement)
		if !valid {
			return outputContractFault(nil)
		}
		if delta != 0 {
			r.job.progress.addNewlyVerified(delta)
		}
	}
	// Outcome and publication counters move together before lifecycle emission,
	// preventing observers from seeing a settled file with stale progress.
	r.job.progress.acceptFileSettlement(settlement, plan.expectedSize)
	return nil
}
