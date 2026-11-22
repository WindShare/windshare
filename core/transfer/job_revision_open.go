package transfer

import (
	"context"

	"github.com/windshare/windshare/core/transfer/internal/failurestate"
	"github.com/windshare/windshare/core/transfer/revisionwait"
)

func (r *jobRun) openSelectedRevision(ctx context.Context, plan plannedFile) (OpenedRevision, FailureStage, error) {
	attempt := revisionOpenAttempt{run: r, plan: plan}
	for {
		opened, rawOpenErr := r.job.revisions.OpenRevision(ctx, plan.file)
		signal, capacityBusy := revisionwait.MatchCapacitySignal(rawOpenErr)
		if capacityBusy {
			retry, err := attempt.waitForCapacity(ctx, opened, signal)
			if retry {
				continue
			}
			return OpenedRevision{}, FailureRevisionOpen, err
		}
		attempt.finishWait(ctx, rawOpenErr)
		return r.admitOpenedRevision(ctx, plan, opened, rawOpenErr)
	}
}

type revisionOpenAttempt struct {
	run    *jobRun
	plan   plannedFile
	waited bool
}

func (attempt *revisionOpenAttempt) traceFunc() func(revisionwait.Trace) {
	return func(event revisionwait.Trace) {
		attempt.run.traceRevisionWait(attempt.plan, event)
	}
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
	attempt.waited = true
	outcome, err := attempt.run.job.revisionWait.Wait(
		ctx, signal, attempt.run.job.progress.setRevisionWait, attempt.traceFunc(),
	)
	return attempt.classifyWaitOutcome(ctx, outcome, err)
}

func (attempt *revisionOpenAttempt) capacityContractError(
	ctx context.Context,
	opened OpenedRevision,
	signal *revisionwait.CapacitySignal,
) error {
	run := attempt.run
	sessionID := run.job.protocolSessionID()
	if !sessionID.IsZero() && signal.ProtocolSession() != sessionID {
		return failurestate.DependencyContract(ErrRevisionIdentity)
	}
	if !opened.Handle.IsZero() {
		// A capacity denial cannot grant a capability. Retiring it first keeps a
		// broken adapter from leaking sender capacity while the job fails closed.
		run.releaseRevision(ctx, attempt.plan, opened.Handle)
		return failurestate.DependencyContract(ErrRevisionIdentity)
	}
	if run.job.revisionWait == nil {
		return failurestate.DependencyContract(ErrRevisionWaitUnavailable)
	}
	return nil
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
		attempt.waited = false
		return false, resourceBudgetFailure(err)
	case revisionwait.WaitCanceled:
		attempt.waited = false
		return false, failurestate.FromCancellation(ctx, err)
	case revisionwait.WaitSessionEnded:
		attempt.waited = false
		if failurestate.IsCancellation(ctx, err) {
			return false, failurestate.FromCancellation(ctx, err)
		}
		return false, sessionTransportFailure(err)
	default:
		attempt.stopWait()
		return false, failurestate.DependencyContract(err)
	}
}

func (attempt *revisionOpenAttempt) finishWait(ctx context.Context, openErr error) {
	if !attempt.waited {
		return
	}
	attempt.waited = false
	observe := attempt.run.job.progress.setRevisionWait
	if openErr == nil {
		attempt.run.job.revisionWait.Succeed(observe, attempt.traceFunc())
		return
	}
	if cause := context.Cause(ctx); cause != nil {
		// The retry RPC observed cancellation, so it is neither a successful
		// admission nor a scheduler stop even though progress must be cleared.
		attempt.run.job.revisionWait.Cancel(cause, observe, attempt.traceFunc())
		return
	}
	attempt.run.job.revisionWait.Stop(observe)
}

func (attempt *revisionOpenAttempt) stopWait() {
	if !attempt.waited {
		return
	}
	attempt.waited = false
	attempt.run.job.revisionWait.Stop(attempt.run.job.progress.setRevisionWait)
}

func (r *jobRun) admitOpenedRevision(
	ctx context.Context, plan plannedFile, opened OpenedRevision, rawOpenErr error,
) (OpenedRevision, FailureStage, error) {
	if err := normalizeSourceBoundary(ctx, rawOpenErr); err != nil {
		if !opened.Handle.IsZero() {
			r.releaseRevision(ctx, plan, opened.Handle)
		}
		return OpenedRevision{}, FailureRevisionOpen, err
	}
	if err := validateOpenedPlanFile(r.job.share, plan.file, plan.expectedSize, plan.modified, opened); err != nil {
		r.releaseRevision(ctx, plan, opened.Handle)
		return OpenedRevision{}, FailureRevisionIdentity, normalizeSourceBoundary(ctx, err)
	}
	return opened, 0, nil
}
