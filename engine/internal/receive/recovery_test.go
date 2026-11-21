package receive

import (
	"context"
	"errors"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"testing"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/engine/internal/task"
	"github.com/windshare/windshare/engine/output"
)

type retainedAttemptAuthority struct {
	getOutputAuthority
	closes     int
	discards   int
	stops      int
	stopErr    error
	busy       bool
	nameReport output.CompatibleNameReport
}

func (authority *retainedAttemptAuthority) Close() error { authority.closes++; return nil }
func (authority *retainedAttemptAuthority) Idle() bool {
	return authority.closes == 0 && !authority.busy
}
func (authority *retainedAttemptAuthority) RecordShareStopped(context.Context) error {
	authority.stops++
	return authority.stopErr
}
func (authority *retainedAttemptAuthority) Discard(context.Context) error {
	authority.discards++
	return nil
}
func (authority *retainedAttemptAuthority) CompatibleNameReport() output.CompatibleNameReport {
	return authority.nameReport
}

func TestReceiveAttemptLifetimeKeepsOneOperationUntilDecision(t *testing.T) {
	for _, test := range []struct {
		name       string
		mode       OutputMode
		action     RecoveryAction
		cancel     bool
		unattended bool
		attempts   int
		outcome    ProgressRetention
		discarded  int
	}{
		{"retry", OutputLiveOnly, RecoveryRetry, false, false, 2, RetentionNone, 0},
		{"discard", OutputResumable, RecoveryDiscard, false, false, 1, RetentionDiscarded, 1},
		{"exit restart", OutputResumable, RecoveryExit, false, false, 1, RetentionCheckpoints, 0},
		{"exit live", OutputLiveOnly, RecoveryExit, false, false, 1, RetentionLiveReleased, 0},
		{"cancel", OutputResumable, RecoveryRetry, true, false, 1, RetentionCheckpoints, 0},
		{"unattended", OutputLiveOnly, RecoveryRetry, false, true, 1, RetentionLiveReleased, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			selection := getReopenSelection(t, true, nil)
			decision, err := ordinaryoutput.NewSyntheticSelectionShape(ordinaryoutput.ShapeFallbackMultipleRoots)
			if err != nil {
				t.Fatal(err)
			}
			events := []string{}
			source := &layoutRecordingGetOutputAuthority{selection: selection, events: &events}
			admission, err := resolveGetOutputOperation(context.Background(), source, &fixedGetShapeResolver{decision: decision, events: &events}, selection, OutputCreateOrResume, receivecontract.OperationID{})
			if err != nil {
				t.Fatal(err)
			}
			authority := &retainedAttemptAuthority{}
			operation := admission.operation
			operation.Mode, operation.Control = test.mode, authority
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			attempts, decisions := 0, 0
			request := Request{Capability: link.Link{Suite: link.SuiteSenderAuthenticated}, Connectivity: ConnectivityRelayOnly, OnPause: func(_ context.Context, pause Pause) (RecoveryAction, error) {
				decisions++
				if authority.closes != 0 || pause.Operation != operation.Intent.OperationID() || !pause.Allows(test.action) {
					t.Fatal("decision lost retained authority")
				}
				return test.action, nil
			}}
			if test.unattended {
				request.OnPause = nil
			}
			result := runOperation(ctx, request, Dependencies{}, func(_ context.Context, _ preparedRequest, _ Dependencies, retained *getOutputPreparation) task.Completion[Result] {
				attempts++
				if attempts == 1 {
					*retained = getOutputPreparation{authority: authority, operation: &operation, mode: test.mode}
					if test.cancel {
						cancel()
					}
					return settlePreparationFailure(stepLocalFailure, Failure{}, nil, nil)
				}
				if retained.operation != &operation || authority.closes != 0 {
					t.Fatal("retry replaced or released output authority")
				}
				return task.Completion[Result]{Outcome: task.OutcomeSuccess}
			})
			if attempts != test.attempts || result.Value.Recovery.Retention != test.outcome || authority.closes != 1 || authority.discards != test.discarded {
				t.Fatalf("attempts=%d recovery=%v close=%d discard=%d", attempts, result.Value.Recovery, authority.closes, authority.discards)
			}
			if (test.cancel || test.unattended) && decisions != 0 {
				t.Fatal("unattended or canceled receive requested input")
			}
			if len(events) != 3 {
				t.Fatalf("operation reserved more than once: %v", events)
			}
		})
	}
}

func TestExplicitResumeNeverCreatesMissingOperation(t *testing.T) {
	selection := getReopenSelection(t, true, nil)
	events := []string{}
	authority := &layoutRecordingGetOutputAuthority{selection: selection, events: &events}
	resolver := &fixedGetShapeResolver{}
	_, err := resolveGetOutputOperation(context.Background(), authority, resolver, selection, OutputResumeExisting, receivecontract.OperationID{})
	if !errors.Is(err, ErrRetainedOutputMissing) || resolver.calls != 0 || len(events) != 1 {
		t.Fatalf("missing resume: %v events=%v shape=%d", err, events, resolver.calls)
	}
	observation, _ := newTestObservation(t)
	step := reportGetOutputAdmissionFailure(observation, err)
	completed := settlePreparationFailure(step, observation.failureSnapshot(), nil, nil)
	failure, ok := errors.AsType[Failure](completed.Err)
	if !ok || failure.Code != FailureRetainedOutputMissing || completed.FailureClass != task.FailureLocal || !errors.Is(completed.Err, ErrRetainedOutputMissing) {
		t.Fatalf("resume admission lost its refusal reason: %+v", completed)
	}
}

func TestReceiveDiscardPropagatesAndValidatesCompatibleNameReport(t *testing.T) {
	selection := getReopenSelection(t, true, nil)
	decision, err := ordinaryoutput.NewSyntheticSelectionShape(ordinaryoutput.ShapeFallbackMultipleRoots)
	if err != nil {
		t.Fatal(err)
	}
	wantReport := output.CompatibleNameReport{
		RepairedCount: 2, ReportName: "windshare-names-abc-1-0.json", PageCount: 1,
	}
	for _, tc := range []struct {
		name       string
		report     output.CompatibleNameReport
		wantReason RecoveryReason
	}{
		{"valid report", wantReport, RecoveryTransferFailure},
		{"invalid report requires inspection", output.CompatibleNameReport{RepairedCount: 1}, RecoveryNeedsInspection},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []string{}
			source := &layoutRecordingGetOutputAuthority{selection: selection, events: &events}
			admission, err := resolveGetOutputOperation(context.Background(), source, &fixedGetShapeResolver{decision: decision, events: &events}, selection, OutputCreateOrResume, receivecontract.OperationID{})
			if err != nil {
				t.Fatal(err)
			}
			authority := &retainedAttemptAuthority{nameReport: tc.report}
			operation := admission.operation
			operation.Mode, operation.Control = OutputResumable, authority
			request := Request{
				Capability: link.Link{Suite: link.SuiteSenderAuthenticated}, Connectivity: ConnectivityRelayOnly,
				OnPause: func(context.Context, Pause) (RecoveryAction, error) { return RecoveryDiscard, nil },
			}
			result := runOperation(context.Background(), request, Dependencies{}, func(_ context.Context, _ preparedRequest, _ Dependencies, retained *getOutputPreparation) task.Completion[Result] {
				*retained = getOutputPreparation{authority: authority, operation: &operation, mode: OutputResumable}
				return settlePreparationFailure(stepLocalFailure, Failure{}, nil, nil)
			})
			if result.Value.Recovery.Reason != tc.wantReason {
				t.Fatalf("recovery reason = %v, want %v", result.Value.Recovery.Reason, tc.wantReason)
			}
			if tc.report.Valid() && result.Value.CompatibleNameReport != tc.report {
				t.Fatalf("discard report = %+v, want %+v", result.Value.CompatibleNameReport, tc.report)
			}
			if !tc.report.Valid() && !errors.Is(result.Err, ErrInvalidResult) {
				t.Fatalf("invalid discard report err = %v, want %v", result.Err, ErrInvalidResult)
			}
		})
	}
}

var _ output.OperationControl = (*retainedAttemptAuthority)(nil)
