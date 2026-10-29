package receive

import (
	"context"
	"errors"
	"slices"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
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
	RecoveryTransferFailure RecoveryReason = iota + 1
	RecoveryCollision
	RecoveryOwnershipUnknown
	RecoveryRevisionChanged
	RecoverySelectionMissing
	RecoveryInvalidRequest
	RecoveryNeedsInspection
)

// Retryable describes replaying the same request, possibly after repairing its
// environment. Output ownership separately decides whether another attempt can start.
func (reason RecoveryReason) Retryable() bool {
	switch reason {
	case RecoveryTransferFailure, RecoveryCollision, RecoveryOwnershipUnknown, RecoveryRevisionChanged:
		return true
	default:
		return false
	}
}

type ProgressRetention uint8

const (
	RetentionNone ProgressRetention = iota
	RetentionCheckpoints
	RetentionLiveReleased
	RetentionDiscarded
)

// Retention is independent of the next step: an invalid selection can coexist
// with completed files and reusable partials that must remain owned and intact.
type RecoveryOutcome struct {
	Retention ProgressRetention
	Reason    RecoveryReason
}

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
	return slices.Contains(pause.Actions, action)
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
			completed.Value.Recovery.Reason = RecoveryNeedsInspection
		}
	}()
	for attemptNumber := uint64(1); ; attemptNumber++ {
		completed = attempt(ctx, request, deps, &retained)
		if completed.Outcome == task.OutcomeSuccess || retained.operation == nil || !retained.operation.Valid() {
			return
		}
		completed.Value.Operation = retained.operation.Intent.OperationID()
		switch retained.mode {
		case OutputResumable:
			completed.Value.Recovery.Retention = RetentionCheckpoints
		case OutputLiveOnly:
			completed.Value.Recovery.Retention = RetentionLiveReleased
		}
		control := retained.operation.Control
		attemptAvailable := control != nil && control.CanStartAttempt()
		retryAllowed := completed.Value.Recovery.Reason.Retryable() && attemptAvailable
		deps.Control.Diagnostics.Emit(diagnostics.New("receive.output_recovery_evaluated", diagnostics.Fields{
			"operation": diagnostics.Identity(completed.Value.Operation.Bytes()), "attempt": diagnostics.Uint(attemptNumber),
			"failure_class":     diagnostics.Uint(uint64(completed.FailureClass)),
			"reason":            diagnostics.Uint(uint64(completed.Value.Recovery.Reason)),
			"retention":         diagnostics.Uint(uint64(completed.Value.Recovery.Retention)),
			"attempt_available": diagnostics.Bool(attemptAvailable), "retry_allowed": diagnostics.Bool(retryAllowed),
		}))
		if ctx.Err() != nil || request.OnPause == nil || !retryAllowed {
			return
		}
		paused := Pause{Operation: completed.Value.Operation, Mode: retained.mode,
			Reason: completed.Value.Recovery.Reason, Actions: []RecoveryAction{RecoveryRetry, RecoveryDiscard, RecoveryExit},
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
				completed.Value.Recovery.Reason = RecoveryNeedsInspection
			} else {
				completed.Value.Recovery.Retention = RetentionDiscarded
			}
			return
		}
	}
}

func recoveryReason(class task.FailureClass, failure Failure, result transfer.JobResult, cleanupErr error) RecoveryReason {
	if cleanupErr != nil {
		return RecoveryNeedsInspection
	}
	// Missing selections require a new request even when a simultaneous source
	// fault took precedence in the terminal error.
	if provesMissingSelection(result) || failure.Local == LocalSelectionMissing {
		return RecoverySelectionMissing
	}
	if class == task.FailureUsage || failure.Code == FailureInvalidInput {
		return RecoveryInvalidRequest
	}
	switch failure.Code {
	case FailureOutputContract, FailurePeerConfiguration:
		return RecoveryNeedsInspection
	}
	if code, ok := failure.Fault.OutputCode(); ok && code == transferfault.OutputContract {
		return RecoveryNeedsInspection
	}
	if code, ok := failure.Fault.SessionCode(); ok && code == transferfault.SessionDependencyContract {
		return RecoveryNeedsInspection
	}
	outcomes := result.Progress.FileOutcomes
	switch {
	case outcomes.RevisionConflictFiles != 0:
		return RecoveryRevisionChanged
	case outcomes.OwnedObjectUnknownFiles != 0 || outcomes.ItemBlockedFiles != 0:
		return RecoveryOwnershipUnknown
	case outcomes.CollisionFiles != 0:
		return RecoveryCollision
	default:
		return RecoveryTransferFailure
	}
}
