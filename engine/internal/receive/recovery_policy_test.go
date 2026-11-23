package receive

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/link"
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
				request := Request{Capability: link.Link{Suite: link.SuiteSenderAuthenticated}, Connectivity: ConnectivityRelayOnly}
				if interactive {
					request.OnPause = func(context.Context, Pause) (RecoveryAction, error) {
						t.Fatal("invalid selection offered an unchanged-request retry")
						return RecoveryExit, nil
					}
				}
				completed := runOperation(t.Context(), request, Dependencies{}, func(_ context.Context, _ preparedRequest, _ Dependencies, retained *getOutputPreparation) task.Completion[Result] {
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
			request := Request{Capability: link.Link{Suite: link.SuiteSenderAuthenticated}, Connectivity: ConnectivityRelayOnly, OnPause: func(_ context.Context, pause Pause) (RecoveryAction, error) {
				prompts++
				if !test.retry || pause.Recovery.Reason != test.reason || !pause.Allows(RecoveryRetry) || authority.closes != 0 {
					t.Fatalf("inappropriate pause: %+v", pause)
				}
				return RecoveryRetry, nil
			}}
			completed := runOperation(t.Context(), request, Dependencies{}, func(_ context.Context, _ preparedRequest, _ Dependencies, retained *getOutputPreparation) task.Completion[Result] {
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
				completed.Value.Recovery.Retryable() || completed.Value.Recovery.Retention != RetentionCheckpoints {
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

func TestStoppedShareRecoveryUsesRetainedOutputAuthority(t *testing.T) {
	for _, test := range []struct {
		name      string
		mode      OutputMode
		busy      bool
		stopErr   error
		action    RecoveryAction
		retention ProgressRetention
		stops     int
		discards  int
	}{
		{name: "unattended", mode: OutputResumable, retention: RetentionCheckpoints, stops: 1},
		{name: "exit keeps partials", mode: OutputResumable, action: RecoveryExit, retention: RetentionCheckpoints, stops: 1},
		{name: "discard", mode: OutputResumable, action: RecoveryDiscard, retention: RetentionDiscarded, stops: 1, discards: 1},
		{name: "live only", mode: OutputLiveOnly, action: RecoveryExit, retention: RetentionLiveReleased},
		{name: "busy", mode: OutputResumable, busy: true, action: RecoveryExit, retention: RetentionCheckpoints},
		{name: "recording failed", mode: OutputResumable, stopErr: errors.New("registry write failed"), action: RecoveryExit, retention: RetentionCheckpoints, stops: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared, authority := recoveryTestPreparation(t, test.mode)
			authority.busy, authority.stopErr = test.busy, test.stopErr
			attempts, prompts := 0, 0
			request := Request{Capability: link.Link{Suite: link.SuiteSenderAuthenticated}, Connectivity: ConnectivityRelayOnly}
			if test.action != 0 {
				request.OnPause = func(_ context.Context, pause Pause) (RecoveryAction, error) {
					prompts++
					if pause.Recovery.ShareStop != ShareStopRelays || pause.Allows(RecoveryRetry) ||
						!pause.Allows(RecoveryDiscard) || !pause.Allows(RecoveryExit) || authority.stops != 1 {
						t.Fatalf("stopped share pause = %+v, stops=%d", pause, authority.stops)
					}
					return test.action, nil
				}
			}
			completed := runOperation(t.Context(), request, Dependencies{}, func(_ context.Context, _ preparedRequest, _ Dependencies, retained *getOutputPreparation) task.Completion[Result] {
				attempts++
				*retained = prepared
				return settlePreparationFailure(stepNetworkFailure, Failure{Cause: relayset.ErrReceiverShareStopped}, nil, nil)
			})
			wantPrompts := 0
			if test.action != 0 && test.mode == OutputResumable && !test.busy && test.stopErr == nil {
				wantPrompts = 1
			}
			recovery := completed.Value.Recovery
			if attempts != 1 || prompts != wantPrompts || authority.stops != test.stops || authority.discards != test.discards ||
				recovery.ShareStop != ShareStopRelays || recovery.Retryable() || recovery.Retention != test.retention {
				t.Fatalf("recovery=%+v attempts=%d prompts=%d stops=%d discards=%d", recovery, attempts, prompts, authority.stops, authority.discards)
			}
			if test.stopErr != nil && (!errors.Is(completed.Err, test.stopErr) || recovery.Reason != RecoveryNeedsInspection) {
				t.Fatalf("missing output failure: recovery=%+v err=%v", recovery, completed.Err)
			}
		})
	}
}

// A share never re-admits a changed source under its link, so the same request
// cannot finish; offering retry would only repeat the drift failure.
func TestSourceDriftOffersDiscardInsteadOfSameLinkRetry(t *testing.T) {
	drift, err := transferfault.NewSource(transferfault.ScopeFileLocal, transferfault.SourceRevisionChanged)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		collision bool
		action    RecoveryAction
		retention ProgressRetention
		discards  int
	}{
		{name: "unattended", retention: RetentionCheckpoints},
		{name: "exit keeps partials", action: RecoveryExit, retention: RetentionCheckpoints},
		{name: "discard", action: RecoveryDiscard, retention: RetentionDiscarded, discards: 1},
		{name: "drift outranks a movable collision", collision: true, action: RecoveryExit, retention: RetentionCheckpoints},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared, authority := recoveryTestPreparation(t, OutputResumable)
			attempts, prompts := 0, 0
			request := Request{Capability: link.Link{Suite: link.SuiteSenderAuthenticated}, Connectivity: ConnectivityRelayOnly}
			if test.action != 0 {
				request.OnPause = func(_ context.Context, pause Pause) (RecoveryAction, error) {
					prompts++
					if pause.Recovery.Reason != RecoverySourceChanged || pause.Allows(RecoveryRetry) ||
						!pause.Allows(RecoveryDiscard) || !pause.Allows(RecoveryExit) {
						t.Fatalf("source drift pause = %+v", pause)
					}
					return test.action, nil
				}
			}
			completed := runOperation(t.Context(), request, Dependencies{}, func(_ context.Context, _ preparedRequest, _ Dependencies, retained *getOutputPreparation) task.Completion[Result] {
				attempts++
				*retained = prepared
				job := transfer.JobResult{Outcome: transfer.DirectTreeOutcomePartial, SourceDriftFault: drift}
				if test.collision {
					job.Progress.FileOutcomes.CollisionFiles = 1
				}
				result, err := Settle(SettlementInput{Result: job})
				if err != nil {
					t.Fatal(err)
				}
				return result
			})
			wantPrompts := 0
			if test.action != 0 {
				wantPrompts = 1
			}
			recovery := completed.Value.Recovery
			if attempts != 1 || prompts != wantPrompts || authority.discards != test.discards ||
				completed.FailureClass != task.FailureSourceDrift || recovery.Reason != RecoverySourceChanged ||
				recovery.Retryable() || recovery.ShareStopped() || recovery.Retention != test.retention {
				t.Fatalf("recovery=%+v class=%d attempts=%d prompts=%d discards=%d", recovery, completed.FailureClass, attempts, prompts, authority.discards)
			}
		})
	}
}
