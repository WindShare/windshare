package transfer

import (
	"context"

	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/revisionwait"
)

func (r *jobRun) openSelectedRevision(ctx context.Context, plan plannedFile) (OpenedRevision, FailureStage, error) {
	attempt := revisionOpenAttempt{run: r, plan: plan}
	for {
		beforeSession := r.job.protocolSessionID()
		opened, rawOpenErr := r.job.revisions.OpenRevision(ctx, plan.file)
		signal, capacityBusy := revisionwait.MatchCapacitySignal(rawOpenErr)
		if capacityBusy {
			retry, err := attempt.waitForCapacity(ctx, beforeSession, opened, signal)
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
	beforeSession protocolsession.ProtocolSessionID,
	opened OpenedRevision,
	signal *revisionwait.CapacitySignal,
) (bool, error) {
	superseded, err := attempt.checkCapacityContract(ctx, beforeSession, opened, signal)
	if err != nil {
		attempt.stopWait()
		return false, err
	}
	if superseded {
		if cause := context.Cause(ctx); cause != nil {
			attempt.finishWait(ctx, cause)
			return false, fault.FromCancellation(ctx, cause)
		}
		return true, nil
	}
	attempt.waited = true
	outcome, err := attempt.run.job.revisionWait.Wait(
		ctx, signal, attempt.run.job.progress.setRevisionWait, attempt.traceFunc(),
	)
	return attempt.classifyWaitOutcome(ctx, outcome, err)
}

func (attempt *revisionOpenAttempt) checkCapacityContract(
	ctx context.Context,
	beforeSession protocolsession.ProtocolSessionID,
	opened OpenedRevision,
	signal *revisionwait.CapacitySignal,
) (bool, error) {
	run := attempt.run
	if !opened.Handle.IsZero() {
		// A capacity denial cannot grant a capability. Retiring it first keeps a
		// broken adapter from leaking sender capacity while the job fails closed.
		run.releaseRevision(ctx, attempt.plan, opened.Handle)
		return false, fault.DependencyContract(ErrRevisionIdentity)
	}
	if run.job.revisionWait == nil {
		return false, fault.DependencyContract(ErrRevisionWaitUnavailable)
	}
	sessionID := run.job.protocolSessionID()
	if !sessionID.IsZero() && signal.ProtocolSession() != sessionID {
		// Concurrent discovery can replace beforeSession just after this RPC returns
		// a capacity denial for that prior generation. Retry immediately on the fresh
		// session while keeping foreign signals fail-closed.
		if !beforeSession.IsZero() && signal.ProtocolSession() == beforeSession {
			return true, nil
		}
		return false, fault.DependencyContract(ErrRevisionIdentity)
	}
	return false, nil
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
		return false, fault.FromCancellation(ctx, err)
	case revisionwait.WaitSessionEnded:
		attempt.waited = false
		if fault.IsCancellation(ctx, err) {
			return false, fault.FromCancellation(ctx, err)
		}
		return false, sessionTransportFailure(err)
	default:
		attempt.stopWait()
		return false, fault.DependencyContract(err)
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
