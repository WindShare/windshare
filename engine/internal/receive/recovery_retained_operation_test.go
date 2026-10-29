package receive

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/engine/internal/task"
)

func TestRetryPreparationFailureRetainsOperationInExitResult(t *testing.T) {
	prepared, authority := recoveryTestPreparation(t, OutputResumable)
	operation := prepared.operation.Intent.OperationID()
	attempts, decisions := 0, 0
	request := Request{OnPause: func(_ context.Context, pause Pause) (RecoveryAction, error) {
		decisions++
		if pause.Operation != operation {
			t.Fatal("pause lost the retained operation")
		}
		if decisions == 1 {
			return RecoveryRetry, nil
		}
		return RecoveryExit, nil
	}}
	completed := runOperation(t.Context(), request, Dependencies{}, func(_ context.Context, _ Request, _ Dependencies, retained *getOutputPreparation) task.Completion[Result] {
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
