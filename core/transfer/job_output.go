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
) error {
	policy := lifecyclePolicyFor(cause)
	if !retireReason.valid() || policy.jobTerminal() || policy.outputFailure() {
		return r.pauseFailedFile(ctx, plan, opened, transaction, stage, cause)
	}
	settleContext, cancel := r.job.newSettlementContext(ctx)
	settlement, rawSettlementErr := transaction.Retire(settleContext, retireReason)
	settlementErr := normalizeOutputBoundary(settleContext, rawSettlementErr)
	cancel()
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
) error {
	if cause := context.Cause(ctx); cause != nil {
		return r.pauseFailedFile(
			ctx, plan, opened, transaction, FailureFileOutput, cancellationFailure(ctx, cause),
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
			FileID: plan.file, Path: plan.path, Stage: FailureFileOutput,
			Cause: settlementErr, Settlement: settlement, SettlementFailure: settlementErr,
			LeaseReleaseFailure: releaseErr,
		})
		r.traceFileSettlement(plan, settlement, joinLifecycleFailures(settlementErr, releaseErr))
		// Commit is the transaction's terminal publication operation. Once its
		// settlement cannot be proven, no sibling or directory finalization may
		// advance the same job namespace before PauseJob takes ownership.
		return joinLifecycleFailures(settlementErr, releaseErr)
	}
	if !settlement.matchesCommittedOutput(transaction.Binding(), r.output.Capabilities()) || settlement.Kind() != FilePublished &&
		settlement.Kind() != FilePublishBlocked && settlement.Kind() != FileQuarantined {
		return r.rejectCommitSettlement(ctx, plan, opened, settlement)
	}
	r.acceptFileSettlement(settlement)
	releaseErr := r.releaseRevision(ctx, opened.LeaseID)
	r.traceFileSettlement(plan, settlement, releaseErr)
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
	r.settlementFailure = mergeLifecycleFailures(r.settlementFailure, contractFailure)
	r.recordFileFailure(FileJobFailure{
		FileID: plan.file, Path: plan.path, Stage: FailureFileOutput, Cause: contractFailure,
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
	outcome := JobSucceeded
	if len(directories) != 0 || len(files) != 0 || omittedDirectories != 0 || omittedFiles != 0 ||
		r.selectionResolutionFailure != nil || sourceDriftFailure != nil {
		outcome = JobCompletedWithErrors
	}
	outcome = r.settleJob(ctx, outcome)
	return JobResult{
		Outcome: outcome, Settlement: r.settlement,
		TransferJobID: r.job.jobID, IntentDigest: r.job.intent.Digest(), TransferIntent: r.job.intent,
		SelectionObservation: r.selectionObservation,
		Measure:              r.job.Measure(), Directories: directories, Files: files,
		OmittedDirectoryFailures: omittedDirectories, OmittedFileFailures: omittedFiles,
		SelectionResolutionFailure: r.selectionResolutionFailure,
		SourceDriftFailure:         lifecycleError(sourceDriftFailure),
		SourceDriftFault:           closedLifecycleFault(sourceDriftFailure),
		SucceededFiles:             r.succeeded, TerminationCause: lifecycleError(r.terminationCause),
		TerminationFault:  closedLifecycleFault(r.terminationCause),
		SettlementFailure: lifecycleError(r.settlementFailure),
		SettlementFault:   closedLifecycleFault(r.settlementFailure),
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
	settlement, rawSettlementErr := r.output.PauseJob(settleContext, reason)
	err := normalizeOutputBoundary(settleContext, rawSettlementErr)
	cancel()
	if err == nil {
		if settlement.Kind() != JobPaused && settlement.Kind() != JobPausedNeedsAttention ||
			r.needsAttention && settlement.Kind() != JobPausedNeedsAttention {
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
		SelectionObservation: r.selectionObservation,
		JobSettlement:        settlement.Kind(),
		Fault: fault.Join(
			closedLifecycleFault(r.terminationCause), closedLifecycleFault(r.settlementFailure),
		),
		Failed: err != nil,
	})
}

func (r *jobRun) completeJob(ctx context.Context, outcome JobOutcome) {
	settleContext, cancel := r.job.newSettlementContext(ctx)
	settlement, rawSettlementErr := r.output.CompleteJob(settleContext, outcome)
	err := normalizeOutputBoundary(settleContext, rawSettlementErr)
	cancel()
	if err == nil {
		if settlement.Kind() != JobClosed && settlement.Kind() != JobPausedNeedsAttention ||
			r.needsAttention && settlement.Kind() != JobPausedNeedsAttention {
			err = outputContractFault(nil)
		}
	}
	if err != nil {
		r.settlementFailure = mergeLifecycleFailures(r.settlementFailure, err)
		r.job.trace(TransferLifecycleTrace{
			Stage: TransferJobSettled, OutputSessionID: r.output.SessionID(),
			SelectionObservation: r.selectionObservation,
			JobSettlement:        settlement.Kind(), Fault: closedFault(err), Failed: true,
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

func (policy lifecyclePolicy) outputRequiresJobPause(capabilities OutputCapabilities) bool {
	if policy.jobTerminal() {
		return true
	}
	if !policy.outputFailure() || policy.value.Scope() != fault.ScopeFileLocal {
		return false
	}
	return !outputCanIsolateFileFailure(capabilities)
}

func (policy lifecyclePolicy) outputCanContinueAfterFileSettlement(
	capabilities OutputCapabilities,
) bool {
	return !policy.canceled && policy.outputFailure() &&
		policy.value.Scope() == fault.ScopeFileLocal && outputCanIsolateFileFailure(capabilities)
}

func outputCanIsolateFileFailure(capabilities OutputCapabilities) bool {
	return capabilities.FileFailureIsolation ||
		capabilities.Mode == OutputZIPStream && capabilities.ArchiveBoundary == ArchiveFailureAtMemberStart
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
	r.job.trace(TransferLifecycleTrace{
		Stage: TransferFileSettled, OutputSessionID: r.output.SessionID(),
		SelectionObservation: r.selectionObservation,
		DirectoryID:          plan.parentDirectory,
		DirectoryGeneration:  plan.parentGeneration,
		FileID:               plan.file,
		FileSelection:        plan.selectionDecision,
		FileSettlement:       settlement.Kind(),
		Fault:                closedFault(failure),
		Failed:               failure != nil,
	})
}

func (r *jobRun) acceptFileSettlement(settlement FileSettlement) {
	if settlement.Kind() == FilePublishBlocked || settlement.Kind() == FileQuarantined {
		r.needsAttention = true
	}
}
