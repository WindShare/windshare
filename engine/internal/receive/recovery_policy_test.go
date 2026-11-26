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
	for _, interactive := range []bool{false, true} {
		t.Run(fmt.Sprintf("interactive=%t", interactive), func(t *testing.T) {
			prepared, authority := recoveryTestPreparation(t)
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
			if attempts != 1 || authority.closes != 1 || authority.discards != 0 ||
				completed.FailureClass != task.FailureUsage || completed.Outcome != task.OutcomePartial ||
				completed.Value.Recovery != (RecoveryOutcome{Retention: RetentionCheckpoints, Reason: RecoverySelectionMissing}) {
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
			prepared, authority := recoveryTestPreparation(t)
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

func recoveryTestPreparation(t *testing.T) (getOutputPreparation, *retainedAttemptAuthority) {
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
	operation.Control = authority
	return getOutputPreparation{authority: authority, operation: &operation}, authority
}

func TestStoppedShareRecoveryUsesRetainedOutputAuthority(t *testing.T) {
	for _, test := range []struct {
		name      string
		busy      bool
		stopErr   error
		action    RecoveryAction
		retention ProgressRetention
		stops     int
		discards  int
	}{
		{name: "unattended", retention: RetentionCheckpoints, stops: 1},
		{name: "exit keeps partials", action: RecoveryExit, retention: RetentionCheckpoints, stops: 1},
		{name: "discard", action: RecoveryDiscard, retention: RetentionDiscarded, stops: 1, discards: 1},
		{name: "busy", busy: true, action: RecoveryExit, retention: RetentionCheckpoints},
		{name: "recording failed", stopErr: errors.New("registry write failed"), action: RecoveryExit, retention: RetentionCheckpoints, stops: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared, authority := recoveryTestPreparation(t)
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
			if test.action != 0 && !test.busy && test.stopErr == nil {
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

func TestNonIdleRetainedOutputRequiresInspectionInsteadOfAdvisingRetry(t *testing.T) {
	outputIO, err := transferfault.NewOutput(transferfault.ScopeOutputPause, transferfault.OutputStateIO)
	if err != nil {
		t.Fatal(err)
	}
	prepared, authority := recoveryTestPreparation(t)
	// Mirrors FinalizeTree(Success) failing during control-dir retirement:
	// the operation transitioned out of idle into cleanup-pending and cannot
	// be resumed via wind resume retry.
	authority.busy = true
	prompts := 0
	request := Request{
		Capability:   link.Link{Suite: link.SuiteSenderAuthenticated},
		Connectivity: ConnectivityRelayOnly,
		OnPause: func(context.Context, Pause) (RecoveryAction, error) {
			prompts++
			return RecoveryRetry, nil
		},
	}
	completed := runOperation(t.Context(), request, Dependencies{}, func(_ context.Context, _ preparedRequest, _ Dependencies, retained *getOutputPreparation) task.Completion[Result] {
		*retained = prepared
		result, settleErr := Settle(SettlementInput{Result: transfer.JobResult{
			Outcome:           transfer.DirectTreeOutcomePaused,
			SettlementFailure: transferfault.Wrap(outputIO, errors.New("retire control dir failed")),
			SettlementFault:   outputIO,
		}})
		if settleErr != nil {
			t.Fatal(settleErr)
		}
		return result
	})
	if prompts != 0 || completed.Value.Recovery.Reason != RecoveryNeedsInspection || completed.Value.Recovery.Retryable() {
		t.Fatalf("non-idle output failure recovery = %+v prompts=%d, want RecoveryNeedsInspection (non-retryable)", completed.Value.Recovery, prompts)
	}
	if !errors.Is(completed.Err, completed.Value.Transfer.SettlementFailure) {
		t.Fatalf("settlement failure lost from completion error: %v", completed.Err)
	}
}

// pureSourceDrift builds a finalized partial job whose only unpublished file
// changed at the source while every other discovered file was published.
func pureSourceDrift(t *testing.T) SettlementInput {
	t.Helper()
	drift, err := transferfault.NewSource(transferfault.ScopeFileLocal, transferfault.SourceRevisionChanged)
	if err != nil {
		t.Fatal(err)
	}
	return SettlementInput{Result: transfer.JobResult{
		Outcome: transfer.DirectTreeOutcomePartial, SourceDriftFault: drift,
		Files: []transfer.FileJobFailure{{Path: "d/b.txt", Stage: transfer.FailureRevisionOpen, Fault: drift}},
		Progress: transfer.ReceiveProgressSnapshot{
			Discovery: transfer.DiscoveryComplete, CountersExact: true, DiscoveredFiles: 2, PublishedFiles: 1,
			FileOutcomes: transfer.FileOutcomeSummary{DownloadedFiles: 1},
		},
	}}
}

func TestSourceDriftOffersDiscardInsteadOfSameLinkRetry(t *testing.T) {
	for _, test := range []struct {
		name      string
		action    RecoveryAction
		retention ProgressRetention
		discards  int
	}{
		{name: "unattended", retention: RetentionCheckpoints},
		{name: "exit keeps partials", action: RecoveryExit, retention: RetentionCheckpoints},
		{name: "discard", action: RecoveryDiscard, retention: RetentionDiscarded, discards: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared, authority := recoveryTestPreparation(t)
			attempts, prompts := 0, 0
			request := Request{Capability: link.Link{Suite: link.SuiteSenderAuthenticated}, Connectivity: ConnectivityRelayOnly}
			if test.action != 0 {
				request.OnPause = func(_ context.Context, pause Pause) (RecoveryAction, error) {
					prompts++
					if pause.Recovery.Reason != RecoverySourceChanged || !pause.Recovery.NeedsNewLink() ||
						!pause.Recovery.HasSourceDrift || pause.Allows(RecoveryRetry) ||
						!pause.Allows(RecoveryDiscard) || !pause.Allows(RecoveryExit) {
						t.Fatalf("source drift pause = %+v", pause)
					}
					return test.action, nil
				}
			}
			completed := runOperation(t.Context(), request, Dependencies{}, func(_ context.Context, _ preparedRequest, _ Dependencies, retained *getOutputPreparation) task.Completion[Result] {
				attempts++
				*retained = prepared
				result, err := Settle(pureSourceDrift(t))
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
				!recovery.NeedsNewLink() || !recovery.HasSourceDrift ||
				recovery.Retryable() || recovery.ShareStopped() || recovery.Retention != test.retention {
				t.Fatalf("recovery=%+v class=%d attempts=%d prompts=%d discards=%d", recovery, completed.FailureClass, attempts, prompts, authority.discards)
			}
		})
	}
}

func TestSourceDriftKeepsRetryWhileOtherWorkCanFinish(t *testing.T) {
	session, err := transferfault.NewSession(transferfault.ScopeSessionTerminal, transferfault.SessionTransport)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*SettlementInput)
		reason RecoveryReason
	}{
		{"interruption", func(input *SettlementInput) {
			input.ContextError = context.Canceled
			input.Result.Outcome = transfer.DirectTreeOutcomePaused
			input.Result.TerminationCause, input.Result.TerminationInterruption = context.Canceled, transfer.TransferInterruptionCanceled
			input.Result.Progress.DiscoveredFiles++
			input.Result.Progress.FileOutcomes.PausedFiles++
			input.Result.Progress.RestartReusableBytes = 1 << 20
		}, RecoveryTransferFailure},
		{"network loss", func(input *SettlementInput) {
			input.ConnectionError = errors.New("relay connection lost")
			input.Result.Outcome = transfer.DirectTreeOutcomePaused
			input.Result.TerminationCause, input.Result.TerminationFault = errors.New("session ended"), session
			input.Result.Progress.DiscoveredFiles++
		}, RecoveryTransferFailure},
		{"collision", func(input *SettlementInput) {
			input.Result.Progress.DiscoveredFiles++
			input.Result.Progress.FileOutcomes.CollisionFiles++
		}, RecoveryCollision},
		{"checkpoint revision conflict", func(input *SettlementInput) {
			input.Result.Progress.DiscoveredFiles++
			input.Result.Progress.FileOutcomes.ItemBlockedFiles++
			input.Result.Progress.FileOutcomes.RevisionConflictFiles++
		}, RecoveryCheckpointRevisionConflict},
		{"unchanged file still unfinished", func(input *SettlementInput) {
			input.Result.Progress.DiscoveredFiles++
		}, RecoveryTransferFailure},
		{"unattributed omitted failure", func(input *SettlementInput) {
			input.Result.OmittedFileFailures = 1
		}, RecoveryTransferFailure},
		{"incomplete discovery", func(input *SettlementInput) {
			input.Result.Progress.Discovery = transfer.DiscoveryOpen
		}, RecoveryTransferFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared, authority := recoveryTestPreparation(t)
			attempts, prompts := 0, 0
			request := Request{Capability: link.Link{Suite: link.SuiteSenderAuthenticated}, Connectivity: ConnectivityRelayOnly, OnPause: func(_ context.Context, pause Pause) (RecoveryAction, error) {
				prompts++
				if prompts > 1 {
					if pause.Recovery.Reason != RecoverySourceChanged || !pause.Recovery.NeedsNewLink() || pause.Allows(RecoveryRetry) {
						t.Fatalf("pure drift after retry pause = %+v", pause.Recovery)
					}
					return RecoveryExit, nil
				}
				if pause.Recovery.Reason != test.reason || pause.Recovery.NeedsNewLink() ||
					!pause.Recovery.HasSourceDrift || !pause.Allows(RecoveryRetry) || !pause.Allows(RecoveryDiscard) {
					t.Fatalf("mixed drift pause = %+v", pause.Recovery)
				}
				return RecoveryRetry, nil
			}}
			completed := runOperation(t.Context(), request, Dependencies{}, func(_ context.Context, _ preparedRequest, _ Dependencies, retained *getOutputPreparation) task.Completion[Result] {
				attempts++
				if attempts > 1 {
					result, err := Settle(pureSourceDrift(t))
					if err != nil {
						t.Fatal(err)
					}
					return result
				}
				*retained = prepared
				input := pureSourceDrift(t)
				test.mutate(&input)
				result, err := Settle(input)
				if err != nil {
					t.Fatal(err)
				}
				return result
			})
			recovery := completed.Value.Recovery
			if attempts != 2 || prompts != 2 || authority.discards != 0 || completed.FailureClass != task.FailureSourceDrift ||
				recovery.Reason != RecoverySourceChanged || !recovery.NeedsNewLink() || !recovery.HasSourceDrift || recovery.Retryable() {
				t.Fatalf("recovery=%+v attempts=%d prompts=%d discards=%d", recovery, attempts, prompts, authority.discards)
			}
		})
	}
}
