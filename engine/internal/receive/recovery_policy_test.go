package receive

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine/internal/task"
)

func TestInvalidSelectionPreservesProgressWithoutOfferingSameRequestRetry(t *testing.T) {
	for _, mode := range []OutputMode{OutputResumable, OutputLiveOnly} {
		for _, interactive := range []bool{false, true} {
			t.Run(fmt.Sprintf("mode=%d/interactive=%t", mode, interactive), func(t *testing.T) {
				prepared, authority := recoveryTestPreparation(t, mode)
				attempts := 0
				request := Request{}
				if interactive {
					request.OnPause = func(context.Context, Pause) (RecoveryAction, error) {
						t.Fatal("invalid selection offered an unchanged-request retry")
						return RecoveryExit, nil
					}
				}
				completed := runOperation(t.Context(), request, Dependencies{}, func(_ context.Context, _ Request, _ Dependencies, retained *getOutputPreparation) task.Completion[Result] {
					attempts++
					if attempts > 1 {
						t.Fatal("repeated a request whose selection is missing")
					}
					*retained = prepared
					result, err := Settle(SettlementInput{Result: transfer.JobResult{
						Outcome:                    transfer.DirectTreeOutcomePartial,
						SelectionResolutionFailure: transfer.ErrSelectionTargetMissing,
						Progress: transfer.ReceiveProgressSnapshot{
							Discovery: transfer.DiscoveryComplete, CountersExact: true,
							RestartReusableBytes: 128, PreviouslyPublishedBytes: 256,
							FileOutcomes: transfer.FileOutcomeSummary{PreviouslyPublishedFiles: 1},
						},
					}})
					if err != nil {
						t.Fatal(err)
					}
					return result
				})
				wantRetention := RetentionCheckpoints
				if mode == OutputLiveOnly {
					wantRetention = RetentionLiveReleased
				}
				if attempts != 1 || authority.closes != 1 || authority.discards != 0 ||
					completed.FailureClass != task.FailureUsage || completed.Outcome != task.OutcomePartial ||
					completed.Value.Recovery != (RecoveryOutcome{Retention: wantRetention, Reason: RecoverySelectionMissing}) {
					t.Fatalf("invalid selection recovery=%+v attempts=%d close=%d discard=%d", completed, attempts, authority.closes, authority.discards)
				}
				progress := completed.Value.Transfer.Progress
				if progress.RestartReusableBytes != 128 || progress.PreviouslyPublishedBytes != 256 ||
					progress.FileOutcomes.PreviouslyPublishedFiles != 1 || completed.Value.Operation != prepared.operation.Intent.OperationID() {
					t.Fatalf("selection failure lost existing progress or operation identity: %+v", completed.Value)
				}
			})
		}
	}
}

func TestRecoveryDistinguishesRequestAndContractFailuresFromRepairableFailures(t *testing.T) {
	outputIO, err := transferfault.NewOutput(transferfault.ScopeOutputPause, transferfault.OutputStateIO)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		class     task.FailureClass
		failure   Failure
		cleanup   error
		collision bool
		reason    RecoveryReason
		retry     bool
	}{
		{"network", task.FailureNetwork, Failure{Cause: errors.New("connection lost")}, nil, false, RecoveryTransferFailure, true},
		{"output IO can be repaired", task.FailureLocal, Failure{Fault: outputIO}, nil, false, RecoveryTransferFailure, true},
		{"collision can be moved", task.FailureLocal, Failure{}, nil, true, RecoveryCollision, true},
		{"invalid request", task.FailureUsage, Failure{Code: FailureInvalidInput}, nil, false, RecoveryInvalidRequest, false},
		{"output contract", task.FailureLocal, Failure{Code: FailureOutputContract}, nil, false, RecoveryNeedsInspection, false},
		{"dependency contract", task.FailureLocal, Failure{Fault: transferfault.DependencyContractFault()}, nil, false, RecoveryNeedsInspection, false},
		{"cleanup failure", task.FailureLocal, Failure{}, errors.New("lease release failed"), false, RecoveryNeedsInspection, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared, authority := recoveryTestPreparation(t, OutputResumable)
			attempts, prompts := 0, 0
			request := Request{OnPause: func(_ context.Context, pause Pause) (RecoveryAction, error) {
				prompts++
				if !test.retry || pause.Reason != test.reason || !pause.Allows(RecoveryRetry) || authority.closes != 0 {
					t.Fatalf("inappropriate pause: %+v", pause)
				}
				return RecoveryRetry, nil
			}}
			completed := runOperation(t.Context(), request, Dependencies{}, func(_ context.Context, _ Request, _ Dependencies, retained *getOutputPreparation) task.Completion[Result] {
				attempts++
				if attempts == 1 {
					*retained = prepared
					if test.collision {
						job := transfer.JobResult{Outcome: transfer.DirectTreeOutcomePartial}
						job.Progress.FileOutcomes.CollisionFiles = 1
						result, err := Settle(SettlementInput{Result: job})
						if err != nil {
							t.Fatal(err)
						}
						return result
					}
					step := stepLocalFailure
					switch test.class {
					case task.FailureUsage:
						step = stepInvalidRequest
					case task.FailureNetwork:
						step = stepNetworkFailure
					}
					return settlePreparationFailure(step, test.failure, nil, test.cleanup)
				}
				if attempts != 2 || !test.retry || retained.operation != prepared.operation {
					t.Fatal("retry changed the retained operation or repeated a terminal failure")
				}
				return task.Completion[Result]{Outcome: task.OutcomeSuccess}
			})
			if test.retry {
				if attempts != 2 || prompts != 1 || completed.Outcome != task.OutcomeSuccess {
					t.Fatalf("repairable failure was not retried: %+v attempts=%d prompts=%d", completed, attempts, prompts)
				}
			} else if attempts != 1 || prompts != 0 || completed.Value.Recovery.Reason != test.reason ||
				completed.Value.Recovery.Reason.Retryable() || completed.Value.Recovery.Retention != RetentionCheckpoints {
				t.Fatalf("terminal failure recovery=%+v attempts=%d prompts=%d", completed, attempts, prompts)
			}
			if authority.closes != 1 || authority.discards != 0 {
				t.Fatalf("recovery changed output ownership: close=%d discard=%d", authority.closes, authority.discards)
			}
		})
	}
}

func recoveryTestPreparation(t *testing.T, mode OutputMode) (getOutputPreparation, *retainedAttemptAuthority) {
	t.Helper()
	selection := getReopenSelection(t, true, nil)
	decision, err := ordinaryoutput.NewSyntheticSelectionShape(ordinaryoutput.ShapeFallbackMultipleRoots)
	if err != nil {
		t.Fatal(err)
	}
	events := []string{}
	source := &layoutRecordingGetOutputAuthority{selection: selection, events: &events}
	admission, err := resolveGetOutputOperation(t.Context(), source, &fixedGetShapeResolver{decision: decision, events: &events}, selection, OutputCreateOrResume, receivecontract.OperationID{})
	if err != nil {
		t.Fatal(err)
	}
	authority := &retainedAttemptAuthority{}
	operation := admission.operation
	operation.Mode, operation.Control = mode, authority
	return getOutputPreparation{authority: authority, operation: &operation, mode: mode}, authority
}
