package receive

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine/internal/task"
)

type RecoveryAction uint8

const (
	RecoveryExit RecoveryAction = iota + 1
	RecoveryRetry
	RecoveryDiscard
)

type RecoveryReason uint8

const (
	RecoveryTemporaryFailure RecoveryReason = iota + 1
	RecoveryCollision
	RecoveryOwnershipUnknown
	RecoveryRevisionChanged
)

type RecoveryOutcome uint8

const (
	RecoveryNotNeeded RecoveryOutcome = iota
	RecoveryCheckpointsRetained
	RecoveryLiveProgressReleased
	RecoveryDiscarded
)

// Pause is a settled attempt while the operation still owns its output. The
// decision port is optional; unattended callers always get an explicit result.
type Pause struct {
	Operation receivecontract.OperationID
	Mode      OutputMode
	Reason    RecoveryReason
	Actions   []RecoveryAction
	Transfer  transfer.JobResult
}

func (pause Pause) Allows(action RecoveryAction) bool {
	for _, allowed := range pause.Actions {
		if action == allowed {
			return true
		}
	}
	return false
}

func Run(ctx context.Context, request Request, deps Dependencies) task.Completion[Result] {
	return runOperation(ctx, request, deps, runAttempt)
}

type attemptRunner func(context.Context, Request, Dependencies, *getOutputPreparation) task.Completion[Result]

func runOperation(ctx context.Context, request Request, deps Dependencies, attempt attemptRunner) (completed task.Completion[Result]) {
	deps = deps.normalized()
	deps.Control.Diagnostics = request.Diagnostics
	var retained getOutputPreparation
	defer func() {
		if retained.authority == nil {
			return
		}
		if err := retained.authority.Close(); err != nil {
			completed.Err = errors.Join(completed.Err, err)
			completed.CleanupError = errors.Join(completed.CleanupError, err)
			completed.Outcome, completed.FailureClass = task.OutcomeFailed, task.FailureLocal
		}
	}()
	for {
		completed = attempt(ctx, request, deps, &retained)
		if completed.Outcome == task.OutcomeSuccess {
			return
		}
		if retained.operation != nil && retained.operation.Valid() && retained.mode == OutputResumable {
			completed.Value.Recovery = RecoveryCheckpointsRetained
		} else if retained.operation != nil && retained.operation.Valid() && retained.mode == OutputLiveOnly {
			completed.Value.Recovery = RecoveryLiveProgressReleased
		}
		if ctx.Err() != nil || request.OnPause == nil || retained.operation == nil || !retained.operation.Valid() {
			return
		}
		control := retained.operation.Control
		if control == nil || !control.RetryAvailable() {
			return
		}
		paused := Pause{Operation: retained.operation.Intent.OperationID(), Mode: retained.mode,
			Reason: recoveryReason(completed.Value.Transfer), Actions: []RecoveryAction{RecoveryRetry, RecoveryDiscard, RecoveryExit},
			Transfer: completed.Value.Transfer}
		action, err := request.OnPause(ctx, paused)
		if ctx.Err() != nil {
			return
		}
		if err != nil || !paused.Allows(action) {
			completed.Err = errors.Join(completed.Err, err, ErrInvalidResult)
			return
		}
		deps.Control.Diagnostics.Emit(diagnostics.New("receive.output_recovery_decision", diagnostics.Fields{
			"operation": diagnostics.Identity(paused.Operation.Bytes()), "action": diagnostics.Uint(uint64(action)),
			"reason": diagnostics.Uint(uint64(paused.Reason)), "mode": diagnostics.Uint(uint64(paused.Mode)),
			"verified_bytes": diagnostics.Uint(paused.Transfer.Progress.VerifiedBytes),
		}))
		switch action {
		case RecoveryExit:
			return
		case RecoveryRetry:
			continue
		case RecoveryDiscard:
			if err := control.Discard(ctx); err != nil {
				completed.Err = errors.Join(completed.Err, err)
				completed.FailureClass = task.FailureLocal
			} else {
				completed.Value.Recovery = RecoveryDiscarded
			}
			return
		}
	}
}

func recoveryReason(result transfer.JobResult) RecoveryReason {
	outcomes := result.Progress.FileOutcomes
	switch {
	case outcomes.RevisionConflictFiles != 0:
		return RecoveryRevisionChanged
	case outcomes.OwnedObjectUnknownFiles != 0 || outcomes.ItemBlockedFiles != 0:
		return RecoveryOwnershipUnknown
	case outcomes.CollisionFiles != 0:
		return RecoveryCollision
	default:
		return RecoveryTemporaryFailure
	}
}
