package transfer

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/transfer/internal/failurestate"
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
		return false, failurestate.DependencyContract(err)
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
		return false, failurestate.FromCancellation(ctx, err)
	case revisionwait.WaitGenerationEnded:
		if failurestate.IsCancellation(ctx, err) {
			return false, failurestate.FromCancellation(ctx, err)
		}
		if errors.Is(err, revisionwait.ErrGenerationContract) {
			return false, failurestate.DependencyContract(err)
		}
		return false, sessionTransportFailure(err)
	default:
		attempt.stopWait()
		return false, failurestate.DependencyContract(err)
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
	if !opened.Handle.IsZero() {
		r.releaseRevision(ctx, plan, opened.Handle)
	}
	if isJobTerminalError(err) {
		return OpenedRevision{}, false, err
	}
	r.recordFileFailure(FileJobFailure{
		FileID: plan.file, Path: plan.failurePath(), Stage: FailureRevisionOpen, Cause: err,
	})
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
		r.releaseRevision(ctx, plan, opened.Handle)
		r.recordFileFailure(FileJobFailure{
			FileID: plan.file, Path: plan.failurePath(), Stage: FailureRevisionIdentity,
			Cause: err,
		})
		return OpenedRevision{}, false, nil
	}
	return opened, true, nil
}
