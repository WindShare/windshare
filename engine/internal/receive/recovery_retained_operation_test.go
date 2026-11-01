package receive

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/engine/internal/task"
)

func TestReceiveRetryKeepsFrozenSelection(t *testing.T) {
	prepared, _ := recoveryTestPreparation(t, OutputResumable)
	paths := []string{"tree"}
	request := Request{
		Capability: link.Link{Suite: link.SuiteSenderAuthenticated}, Connectivity: ConnectivityRelayOnly, Only: paths,
		OnPause: func(context.Context, Pause) (RecoveryAction, error) {
			paths[0] = "tree/"
			return RecoveryRetry, nil
		},
	}
	attempts := 0
	completed := runOperation(t.Context(), request, Dependencies{}, func(_ context.Context, input preparedRequest, _ Dependencies, retained *getOutputPreparation) task.Completion[Result] {
		attempts++
		if !input.selection.DirectorySelectedAt(catalog.DirectoryID{}, "tree", false) {
			t.Fatal("receive attempt lost the original selection")
		}
		if attempts == 1 {
			*retained = prepared
			return settlePreparationFailure(stepNetworkFailure, Failure{}, nil, nil)
		}
		if attempts != 2 {
			t.Fatal("unexpected extra receive attempt")
		}
		return task.Completion[Result]{Outcome: task.OutcomeSuccess}
	})
	if attempts != 2 || completed.Outcome != task.OutcomeSuccess {
		t.Fatalf("retry reinterpreted caller input: attempts=%d result=%+v", attempts, completed)
	}
}

func TestRetryPreparationFailureRetainsOperationInExitResult(t *testing.T) {
	prepared, authority := recoveryTestPreparation(t, OutputResumable)
	operation := prepared.operation.Intent.OperationID()
	attempts, decisions := 0, 0
	request := Request{Capability: link.Link{Suite: link.SuiteSenderAuthenticated}, Connectivity: ConnectivityRelayOnly, OnPause: func(_ context.Context, pause Pause) (RecoveryAction, error) {
		decisions++
		if pause.Operation != operation {
			t.Fatal("pause lost the retained operation")
		}
		if decisions == 1 {
			return RecoveryRetry, nil
		}
		return RecoveryExit, nil
	}}
	completed := runOperation(t.Context(), request, Dependencies{}, func(_ context.Context, _ preparedRequest, _ Dependencies, retained *getOutputPreparation) task.Completion[Result] {
		attempts++
		if attempts == 1 {
			*retained = prepared
			failed := settlePreparationFailure(stepNetworkFailure, Failure{Cause: errors.New("transfer paused")}, nil, nil)
			failed.Value.Operation = operation
			return failed
		}
		return settlePreparationFailure(stepNetworkFailure, Failure{Cause: errors.New("relay unavailable during preparation")}, nil, nil)
	})
	if attempts != 2 || decisions != 2 || completed.Value.Operation != operation || operation.IsZero() ||
		completed.Value.Recovery.Retention != RetentionCheckpoints || authority.closes != 1 || authority.discards != 0 {
		t.Fatalf("retained result: %+v attempts=%d decisions=%d authority=%+v", completed, attempts, decisions, authority)
	}
}
