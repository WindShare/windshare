package receive

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/engine/internal/task"
)

func TestSourceOmissionsDoNotOfferRetryOrClaimRetainedCheckpoints(t *testing.T) {
	for _, mode := range []OutputMode{OutputResumable, OutputLiveOnly} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			prepared, authority := recoveryTestPreparation(t, mode)
			request := Request{
				Capability: link.Link{Suite: link.SuiteSenderAuthenticated}, Connectivity: ConnectivityRelayOnly,
				OnPause: func(context.Context, Pause) (RecoveryAction, error) {
					t.Fatal("source omissions offered a retry of already-published content")
					return RecoveryExit, nil
				},
			}
			attempts := 0
			completed := runOperation(t.Context(), request, Dependencies{}, func(_ context.Context, _ preparedRequest, _ Dependencies, retained *getOutputPreparation) task.Completion[Result] {
				attempts++
				*retained = prepared
				job := successfulJobResult(t)
				job.Progress.SourceOmittedEntries = 1
				result, err := Settle(SettlementInput{Result: job})
				if err != nil {
					t.Fatal(err)
				}
				return result
			})
			if attempts != 1 || authority.closes != 1 || authority.discards != 0 ||
				completed.Outcome != task.OutcomePartial || completed.FailureClass != task.FailureLocal ||
				completed.Value.Recovery != (RecoveryOutcome{Reason: RecoverySourceOmitted}) {
				t.Fatalf("source omission recovery=%+v attempts=%d closes=%d discards=%d", completed, attempts, authority.closes, authority.discards)
			}
		})
	}
}

func TestSourceOmissionsPreserveActualFailurePrecedence(t *testing.T) {
	cleanup := errors.New("cleanup failed")
	for _, test := range []struct {
		name    string
		mutate  func(*SettlementInput)
		outcome task.Outcome
	}{
		{"published", func(*SettlementInput) {}, task.OutcomePartial},
		{"cleanup", func(input *SettlementInput) { input.CleanupError = cleanup }, task.OutcomeFailed},
		{"inconsistent publication", func(input *SettlementInput) { input.Result.Progress.PublishedBytes-- }, task.OutcomeFailed},
		{"paused", func(input *SettlementInput) {
			input.Result.Outcome = transfer.DirectTreeOutcomePaused
			input.ContextError = context.Canceled
		}, task.OutcomePaused},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := SettlementInput{Result: successfulJobResult(t)}
			input.Result.Progress.SourceOmittedEntries = 2
			test.mutate(&input)
			completed, err := Settle(input)
			failure, ok := errors.AsType[Failure](completed.Err)
			if err != nil || !ok || completed.Outcome != test.outcome ||
				(failure.Local == LocalSourceOmitted) != (test.name == "published") {
				t.Fatalf("source omission masked a failure: %+v %v", completed, err)
			}
		})
	}
}
