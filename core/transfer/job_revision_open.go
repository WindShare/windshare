package transfer

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/transfer/revisionwait"
)

func (r *jobRun) openSelectedRevision(ctx context.Context, plan plannedFile) (OpenedRevision, bool, error) {
	attempt := revisionOpenAttempt{run: r, plan: plan}
	for {
		opened, rawOpenErr := r.job.revisions.OpenRevision(ctx, plan.file)
		signal, capacityBusy := revisionwait.MatchCapacitySignal(rawOpenErr)
		if capacityBusy {
			retry, err := attempt.waitForCapacity(ctx, opened, signal)
			if retry {
				continue
			}
			return OpenedRevision{}, false, err
		}
		attempt.finishWait(ctx, rawOpenErr)
		return r.finishRevisionOpen(ctx, plan, opened, rawOpenErr)
	}
}

type revisionOpenAttempt struct {
	run  *jobRun
	plan plannedFile
	wait *revisionwait.Operation
}

func (attempt *revisionOpenAttempt) waitForCapacity(
	ctx context.Context,
	opened OpenedRevision,
	signal *revisionwait.CapacitySignal,
) (bool, error) {
	if err := attempt.capacityContractError(ctx, opened, signal); err != nil {
		attempt.stopWait()
		return false, err
	}
	if err := attempt.ensureWait(); err != nil {
		return false, dependencyContractFailure(err)
	}
	outcome, err := attempt.wait.Wait(ctx, signal)
	return attempt.classifyWaitOutcome(ctx, outcome, err)
}

func (attempt *revisionOpenAttempt) capacityContractError(
	ctx context.Context,
	opened OpenedRevision,
	signal *revisionwait.CapacitySignal,
) error {
	run := attempt.run
	if !run.job.protocolSessionID.IsZero() && signal.ProtocolSession() != run.job.protocolSessionID {
		return dependencyContractFailure(ErrRevisionIdentity)
	}
	if !opened.LeaseID.IsZero() {
		// A capacity denial cannot grant a capability. Retiring it first keeps a
		// broken adapter from leaking sender capacity while the job fails closed.
		releaseErr := run.releaseRevision(ctx, opened.LeaseID)
		return joinClosedLifecycleFailures(
			dependencyContractFailure(ErrRevisionIdentity), admitInternalFailure(releaseErr),
		)
	}
	if run.job.revisionWait == nil {
		return dependencyContractFailure(ErrRevisionWaitUnavailable)
	}
	return nil
}

func (attempt *revisionOpenAttempt) ensureWait() error {
	if attempt.wait != nil {
		return nil
	}
	wait, err := attempt.run.job.revisionWait.NewOperation(
		revisionwait.ObserverFunc(attempt.run.job.progress.setRevisionWait),
		revisionwait.TraceFunc(func(event revisionwait.Trace) {
			attempt.run.traceRevisionWait(attempt.plan, event)
		}),
	)
	if err == nil {
		attempt.wait = wait
	}
	return err
}

func (attempt *revisionOpenAttempt) classifyWaitOutcome(
	ctx context.Context,
	outcome revisionwait.WaitOutcome,
	err error,
) (bool, error) {
	switch outcome {
	case revisionwait.WaitRetry:
		return true, nil
	case revisionwait.WaitBudgetPaused:
		return false, resourceBudgetFailure(err)
	case revisionwait.WaitCanceled:
		return false, cancellationFailure(ctx, err)
	case revisionwait.WaitGenerationEnded:
		if cancellation := boundaryCancellation(ctx, err); cancellation != cancellationNone {
			return false, cancellationFailure(ctx, err)
		}
		if errors.Is(err, revisionwait.ErrGenerationContract) {
			return false, dependencyContractFailure(err)
		}
		return false, sessionTransportFailure(err)
	default:
		attempt.stopWait()
		return false, dependencyContractFailure(err)
	}
}

func (attempt *revisionOpenAttempt) finishWait(ctx context.Context, openErr error) {
	if attempt.wait == nil {
		return
	}
	if openErr == nil {
		attempt.wait.Succeed()
		return
	}
	if cause := context.Cause(ctx); cause != nil {
		// The retry RPC observed cancellation, so it is neither a successful
		// admission nor a scheduler stop even though progress must be cleared.
		attempt.wait.Cancel(cause)
		return
	}
	attempt.stopWait()
}

func (attempt *revisionOpenAttempt) stopWait() {
	if attempt.wait != nil {
		attempt.wait.Stop()
	}
}

func (r *jobRun) finishRevisionOpen(
	ctx context.Context,
	plan plannedFile,
	opened OpenedRevision,
	rawOpenErr error,
) (OpenedRevision, bool, error) {
	err := normalizeSourceBoundary(ctx, rawOpenErr)
	if err == nil {
		return r.validateOpenedRevision(ctx, plan, opened)
	}
	var releaseErr error
	if !opened.LeaseID.IsZero() {
		releaseErr = r.releaseRevision(ctx, opened.LeaseID)
	}
	if isJobTerminalError(err) {
		return OpenedRevision{}, false, joinLifecycleFailures(err, releaseErr)
	}
	r.recordFileFailure(FileJobFailure{
		FileID: plan.file, Path: plan.failurePath(), Stage: FailureRevisionOpen, Cause: err,
		LeaseReleaseFailure: releaseErr,
	})
	if isJobTerminalError(releaseErr) {
		return OpenedRevision{}, false, releaseErr
	}
	return OpenedRevision{}, false, nil
}

func (r *jobRun) validateOpenedRevision(
	ctx context.Context,
	plan plannedFile,
	opened OpenedRevision,
) (OpenedRevision, bool, error) {
	if err := validateOpenedPlanFile(
		r.job.share, plan.file, plan.expectedSize, plan.modified, opened,
	); err != nil {
		err = sourceChangedFailure(err)
		releaseErr := r.releaseRevision(ctx, opened.LeaseID)
		r.recordFileFailure(FileJobFailure{
			FileID: plan.file, Path: plan.failurePath(), Stage: FailureRevisionIdentity,
			Cause: err, LeaseReleaseFailure: releaseErr,
		})
		if releaseErr != nil && isJobTerminalError(releaseErr) {
			return OpenedRevision{}, false, releaseErr
		}
		return OpenedRevision{}, false, nil
	}
	return opened, true, nil
}
