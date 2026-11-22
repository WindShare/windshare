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
	"github.com/windshare/windshare/engine/output"
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
	RecoverySourceOmitted
	RecoveryInvalidRequest
	RecoveryNeedsInspection
)

type ShareStopSource uint8

const (
	ShareStopUnconfirmed ShareStopSource = iota
	ShareStopSender
	ShareStopRelays
	ShareStopRecorded
)

func (source ShareStopSource) String() string {
	switch source {
	case ShareStopUnconfirmed:
		return "unconfirmed"
	case ShareStopSender:
		return "sender"
	case ShareStopRelays:
		return "relay_set"
	case ShareStopRecorded:
		return "recorded"
	default:
		return ""
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
	ShareStop ShareStopSource
}

func (outcome RecoveryOutcome) ShareStopped() bool { return outcome.ShareStop != ShareStopUnconfirmed }

// Retryable describes replaying the same request after any local remediation.
// Link viability and the primary fault are independent of output ownership.
func (outcome RecoveryOutcome) Retryable() bool {
	if outcome.ShareStopped() {
		return false
	}
	switch outcome.Reason {
	case RecoveryTransferFailure, RecoveryCollision, RecoveryOwnershipUnknown, RecoveryRevisionChanged:
		return true
	default:
		return false
	}
}

// Pause is a settled attempt while the operation still owns its output. The
// decision port is optional; unattended callers always get an explicit result.
type Pause struct {
	Operation receivecontract.OperationID
	Recovery  RecoveryOutcome
	Actions   []RecoveryAction
	Transfer  transfer.JobResult
}

func (pause Pause) Allows(action RecoveryAction) bool {
	return slices.Contains(pause.Actions, action)
}

func Run(ctx context.Context, request Request, deps Dependencies) task.Completion[Result] {
	return runOperation(ctx, request, deps, runAttempt)
}

type attemptRunner func(context.Context, preparedRequest, Dependencies, *getOutputPreparation) task.Completion[Result]

func runOperation(ctx context.Context, request Request, deps Dependencies, attempt attemptRunner) (completed task.Completion[Result]) {
	deps = deps.normalized()
	deps.Control.Diagnostics = request.Diagnostics
	startedAt := deps.Clock.Now()
	input, err := prepareRequest(request)
	if err != nil {
		deps.Control.Diagnostics.Emit(diagnostics.New("receive.request_rejected", diagnostics.Fields{
			"selection_path_count": diagnostics.Uint(uint64(len(request.Only))), "cause": diagnostics.Text(err.Error()),
		}).Correlate(diagnostics.Fields{"receive_operation_id": diagnostics.Identity(request.OperationID.Bytes())}))
		completed = settlePreparationFailure(stepInvalidRequest, Failure{Code: FailureInvalidInput, Cause: err}, ctx.Err(), nil)
		completed.Value.Destination = request.Destination
		completed.Value.Connectivity.Final = true
		completed.Value.Elapsed = max(deps.Clock.Now().Sub(startedAt), 0)
		return completed
	}
	var retained getOutputPreparation
	defer func() {
		if retained.authority == nil {
			return
		}
		if err := retained.authority.Close(); err != nil {
			requireOutputInspection(&completed, err)
		}
	}()
	for attemptNumber := uint64(1); ; attemptNumber++ {
		completed = attempt(ctx, input, deps, &retained)
		if completed.Outcome == task.OutcomeSuccess || completed.Value.Recovery.Reason == RecoverySourceOmitted ||
			retained.operation == nil || !retained.operation.Valid() {
			return
		}
		control := settleRetainedOutput(&completed, &retained, deps.Control)
		recovery := completed.Value.Recovery
		actions := pauseActions(recovery, control != nil)
		deps.Control.Diagnostics.Emit(diagnostics.New("receive.output_recovery_evaluated", diagnostics.Fields{
			"operation": diagnostics.Identity(completed.Value.Operation.Bytes()), "attempt": diagnostics.Uint(attemptNumber),
			"failure_class": diagnostics.Uint(uint64(completed.FailureClass)),
			"reason":        diagnostics.Uint(uint64(recovery.Reason)),
			"retention":     diagnostics.Uint(uint64(recovery.Retention)),
			"share_stop":    diagnostics.Text(recovery.ShareStop.String()),
			"output_idle":   diagnostics.Bool(control != nil), "retry_allowed": diagnostics.Bool(slices.Contains(actions, RecoveryRetry)),
		}))
		if ctx.Err() != nil || request.OnPause == nil || len(actions) == 0 {
			return
		}
		paused := Pause{Operation: completed.Value.Operation, Recovery: recovery,
			Actions: actions, Transfer: completed.Value.Transfer}
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
			"reason": diagnostics.Uint(uint64(recovery.Reason)), "retention": diagnostics.Uint(uint64(recovery.Retention)),
			"verified_bytes": diagnostics.Uint(paused.Transfer.Progress.VerifiedBytes),
		}))
		switch action {
		case RecoveryExit:
			return
		case RecoveryRetry:
			continue
		case RecoveryDiscard:
			if err := control.Discard(ctx); err != nil {
				requireOutputInspection(&completed, err)
			} else {
				completed.Value.Recovery.Retention = RetentionDiscarded
			}
			return
		}
	}
}

func settleRetainedOutput(completed *task.Completion[Result], retained *getOutputPreparation, taskControl task.Control) output.OperationControl {
	result := &completed.Value
	result.Operation = retained.operation.Intent.OperationID()
	switch retained.mode {
	case OutputResumable:
		result.Recovery.Retention = RetentionCheckpoints
	case OutputLiveOnly:
		result.Recovery.Retention = RetentionLiveReleased
	}
	control := retained.operation.Control
	if control == nil || !control.Idle() {
		return nil
	}
	if result.Recovery.ShareStopped() && retained.mode == OutputResumable {
		ctx, cancel := taskControl.CleanupContext()
		defer cancel()
		if err := control.RecordShareStopped(ctx); err != nil {
			requireOutputInspection(completed, err)
		}
	}
	return control
}

func requireOutputInspection(completed *task.Completion[Result], err error) {
	completed.Err = errors.Join(Failure{Cause: err}, completed.Err)
	completed.CleanupError = errors.Join(completed.CleanupError, err)
	completed.Outcome, completed.FailureClass = task.OutcomeFailed, task.FailureLocal
	completed.Value.Recovery.Reason = RecoveryNeedsInspection
}

func pauseActions(recovery RecoveryOutcome, idle bool) []RecoveryAction {
	switch {
	case !idle || recovery.Reason == RecoveryNeedsInspection:
		return nil
	case recovery.Retryable():
		return []RecoveryAction{RecoveryRetry, RecoveryDiscard, RecoveryExit}
	case recovery.ShareStopped() && recovery.Retention == RetentionCheckpoints:
		return []RecoveryAction{RecoveryDiscard, RecoveryExit}
	default:
		return nil
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
	if failure.Local == LocalSourceOmitted {
		return RecoverySourceOmitted
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
