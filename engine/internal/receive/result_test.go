package receive

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/engine/internal/task"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

type opaqueCanaryError struct{ value string }

func (e opaqueCanaryError) Error() string { return e.value }
func TestReceiveSettlementPreservesOutcomeAndFailurePrecedence(t *testing.T) {
	destination := "C:/downloads/result"
	success := successfulJobResult(t)
	sourceDrift, err := transferfault.NewSource(transferfault.ScopeFileLocal, transferfault.SourceRevisionChanged)
	if err != nil {
		t.Fatal(err)
	}
	sessionTerminal, err := transferfault.NewSession(transferfault.ScopeSessionTerminal, transferfault.SessionTransport)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		mutate     func(*SettlementInput)
		wantStatus task.Outcome
		wantExit   task.FailureClass
		wantDrift  bool
	}{
		{"success", func(*SettlementInput) {}, task.OutcomeSuccess, task.FailureNone, false},
		{"remote cleanup diagnostic preserves success", func(input *SettlementInput) {
			input.Result.RevisionCleanupFailures = []transfer.RevisionCleanupFailure{{Cause: context.DeadlineExceeded, Interruption: transfer.TransferInterruptionDeadline}}
			input.Result.OmittedRevisionCleanupFailures = 3
		}, task.OutcomeSuccess, task.FailureNone, false},
		{"nominal success invariant failure", func(input *SettlementInput) {
			input.Result.Progress.PublishedBytes--
		}, task.OutcomeFailed, task.FailureLocal, false},
		{"partial missing selection", func(input *SettlementInput) {
			input.Result.Outcome = transfer.DirectTreeOutcomePartial
			input.Result.SelectionResolutionFailure = fmt.Errorf("selection wrapper: %w", transfer.ErrSelectionTargetMissing)
		}, task.OutcomePartial, task.FailureUsage, false},
		{"exact missing selection outranks caller cancel", func(input *SettlementInput) {
			input.Result.Outcome = transfer.DirectTreeOutcomePartial
			input.Result.SelectionResolutionFailure = transfer.ErrSelectionTargetMissing
			input.ContextError = context.Canceled
		}, task.OutcomePartial, task.FailureUsage, false},
		{"inexact complete discovery does not prove missing selection", func(input *SettlementInput) {
			input.Result.Outcome = transfer.DirectTreeOutcomePartial
			input.Result.SelectionResolutionFailure = fmt.Errorf("selection wrapper: %w", transfer.ErrSelectionTargetMissing)
			input.Result.Progress.CountersExact = false
		}, task.OutcomePartial, task.FailureLocal, false},
		{"failed discovery does not prove missing selection", func(input *SettlementInput) {
			input.Result.Outcome = transfer.DirectTreeOutcomePartial
			input.Result.SelectionResolutionFailure = transfer.ErrSelectionTargetMissing
			input.Result.Progress.Discovery = transfer.DiscoveryFailed
		}, task.OutcomePartial, task.FailureLocal, false},
		{"open discovery does not prove missing selection", func(input *SettlementInput) {
			input.Result.Outcome = transfer.DirectTreeOutcomePartial
			input.Result.SelectionResolutionFailure = transfer.ErrSelectionTargetMissing
			input.Result.Progress.Discovery = transfer.DiscoveryOpen
		}, task.OutcomePartial, task.FailureLocal, false},
		{"drift outranks exact missing selection network and cancel", func(input *SettlementInput) {
			input.Result.Outcome = transfer.DirectTreeOutcomePartial
			input.Result.SelectionResolutionFailure = transfer.ErrSelectionTargetMissing
			input.Result.SourceDriftFault = sourceDrift
			input.RuntimeError = opaqueCanaryError{"relay-token-canary"}
			input.ContextError = context.Canceled
		}, task.OutcomePartial, task.FailureSourceDrift, true},
		{"drift outranks network and cancel", func(input *SettlementInput) {
			input.Result.Outcome = transfer.DirectTreeOutcomePaused
			input.Result.SourceDriftFault = sourceDrift
			input.Result.SourceDriftFailure = opaqueCanaryError{"catalog/path/canary"}
			input.RuntimeError = opaqueCanaryError{"relay-token-canary"}
			input.ContextError = context.Canceled
		}, task.OutcomePaused, task.FailureSourceDrift, true},
		{"session fault is network", func(input *SettlementInput) {
			input.Result.Outcome = transfer.DirectTreeOutcomePaused
			input.Result.TerminationFault = sessionTerminal
		}, task.OutcomePaused, task.FailureNetwork, false},
		{"runtime failure outranks cancel", func(input *SettlementInput) {
			input.Result.Outcome = transfer.DirectTreeOutcomeFailed
			input.RuntimeError = opaqueCanaryError{"provider-url-canary"}
			input.ContextError = context.Canceled
		}, task.OutcomeFailed, task.FailureNetwork, false},
		{"caller cancel is local exit", func(input *SettlementInput) {
			input.Result.Outcome = transfer.DirectTreeOutcomePaused
			input.ContextError = context.Canceled
		}, task.OutcomePaused, task.FailureLocal, false},
		{"ordinary partial remains local", func(input *SettlementInput) {
			input.Result.Outcome = transfer.DirectTreeOutcomePartial
		}, task.OutcomePartial, task.FailureLocal, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := SettlementInput{Result: success, Destination: destination, Elapsed: 3 * time.Second}
			test.mutate(&input)
			result, err := Settle(input)
			if err != nil {
				t.Fatal(err)
			}
			if result.Outcome != test.wantStatus || result.FailureClass != test.wantExit || (result.FailureClass == task.FailureSourceDrift) != test.wantDrift {
				t.Fatalf("result outcome/class/drift = %d/%d/%t want %d/%d/%t", result.Outcome, result.FailureClass, (result.FailureClass == task.FailureSourceDrift), test.wantStatus, test.wantExit, test.wantDrift)
			}
			failure, classified := errors.AsType[Failure](result.Err)
			if !result.Settlement.Valid() || classified != (test.wantStatus != task.OutcomeSuccess) || classified && failure.Error() == "" {
				t.Fatalf("settlement lost structured failure: %+v", result)
			}
		})
	}
}

func TestReceiveFailureRetainsTypedReasonWithoutLowLevelCause(t *testing.T) {
	fault, err := transferfault.NewSource(transferfault.ScopeFileLocal, transferfault.SourceRevisionChanged)
	if err != nil {
		t.Fatal(err)
	}
	for _, reason := range []Failure{
		{Code: FailureInvalidInput},
		{Local: LocalDestinationCollision},
		{Fault: fault},
		{Interruption: transfer.TransferInterruptionCanceled},
		{Cause: context.Canceled},
		{},
	} {
		if reason.Error() == "" || !errors.Is(reason, reason.Cause) && reason.Cause != nil {
			t.Fatalf("failure cannot be inspected: %+v", reason)
		}
		completed := settlePreparationFailure(stepInvalidRequest, reason, nil, nil)
		retained, ok := errors.AsType[Failure](completed.Err)
		if !ok || retained != reason || completed.Outcome != task.OutcomeFailed || completed.FailureClass != task.FailureUsage || !completed.Settlement.Valid() {
			t.Fatalf("typed failure changed: %+v", completed)
		}
	}
}

func TestReceiveSettlementRetainsSelectedReasonAndDiagnosticCauses(t *testing.T) {
	fault, err := transferfault.NewSource(transferfault.ScopeFileLocal, transferfault.SourceRevisionChanged)
	if err != nil {
		t.Fatal(err)
	}
	admission, runtime, connection := errors.New("admission"), errors.New("runtime"), errors.New("connection")
	cleanup, termination, settlement := errors.New("cleanup"), errors.New("termination"), errors.New("settlement")
	selection, source := errors.New("selection"), errors.New("source")
	result, err := Settle(SettlementInput{
		Result: transfer.JobResult{Outcome: transfer.DirectTreeOutcomePaused, SourceDriftFault: fault,
			TerminationCause: termination, SettlementFailure: settlement, SelectionResolutionFailure: selection, SourceDriftFailure: source},
		AdmissionError: admission, RuntimeError: runtime, ConnectionError: connection, ContextError: context.Canceled, CleanupError: cleanup,
	})
	failure, classified := errors.AsType[Failure](result.Err)
	if err != nil || !classified || failure.Fault != fault || result.Outcome != task.OutcomeFailed || result.FailureClass != task.FailureSourceDrift || result.CleanupError != cleanup {
		t.Fatalf("settlement changed selected failure: %+v, %v", result, err)
	}
	for _, cause := range []error{admission, runtime, connection, context.Canceled, cleanup, termination, settlement, selection, source} {
		if !errors.Is(result.Err, cause) {
			t.Fatalf("settlement lost diagnostic %v: %v", cause, result.Err)
		}
	}
}

func successfulJobResult(t *testing.T) transfer.JobResult {
	t.Helper()
	settlement, err := transfer.NewDirectTreeSettlement(transfer.DirectTreeSettlementSuccess)
	if err != nil {
		t.Fatal(err)
	}
	return transfer.JobResult{
		Outcome: transfer.DirectTreeOutcomeSuccess, Settlement: settlement,
		SucceededFiles: 2,
		Progress: transfer.ReceiveProgressSnapshot{
			DiscoveredFiles: 2, DiscoveredBytes: 100,
			PublishedFiles: 2, PublishedBytes: 100,
			VerifiedBytes: 100, NewlyVerifiedBytes: 60,
			FileOutcomes: transfer.FileOutcomeSummary{DownloadedFiles: 1, ResumedFiles: 1},
			Discovery:    transfer.DiscoveryComplete, CountersExact: true,
		},
	}
}

func peerSessionTerminalError(t *testing.T, code uint16) error {
	t.Helper()
	body, err := protocolsession.EncodeSessionTerminal(protocolsession.SessionTerminal{Code: code, Message: "Sender closed"})
	if err != nil {
		t.Fatal(err)
	}
	message, err := protocolsession.NewMessage(protocolsession.MessageSessionTerminal, nil, body)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Errorf("pump failed: %w", protocolsession.NewPeerSessionTerminalError(message))
}

func TestReceiveSettlementSeparatesStopEvidenceFromPrimaryFailure(t *testing.T) {
	sessionTerminal, err := transferfault.NewSession(transferfault.ScopeSessionTerminal, transferfault.SessionTransport)
	if err != nil {
		t.Fatal(err)
	}
	outputPause, err := transferfault.NewSession(transferfault.ScopeOutputPause, transferfault.SessionTransport)
	if err != nil {
		t.Fatal(err)
	}
	outputContract, err := transferfault.NewOutput(transferfault.ScopeOutputPause, transferfault.OutputContract)
	if err != nil {
		t.Fatal(err)
	}
	sourceDrift, err := transferfault.NewSource(transferfault.ScopeFileLocal, transferfault.SourceRevisionChanged)
	if err != nil {
		t.Fatal(err)
	}
	outputIO, err := transferfault.NewOutput(transferfault.ScopeOutputPause, transferfault.OutputStateIO)
	if err != nil {
		t.Fatal(err)
	}
	peerStop := peerSessionTerminalError(t, session.SessionStoppedCode)
	tests := []struct {
		name      string
		mutate    func(*SettlementInput)
		wantStop  ShareStopSource
		wantCode  FailureCode
		wantFault transferfault.Fault
	}{
		{"sender terminal", func(input *SettlementInput) {
			input.RuntimeError = peerStop
		}, ShareStopSender, FailureShareStopped, transferfault.Fault{}},
		{"aggregate relay stop during an output pause", func(input *SettlementInput) {
			input.Result.TerminationFault = outputPause
			input.Result.TerminationCause = relayset.ErrReceiverShareStopped
		}, ShareStopRelays, FailureShareStopped, transferfault.Fault{}},
		{"sender stop surfacing as a dependency contract", func(input *SettlementInput) {
			input.Result.TerminationFault = transferfault.DependencyContractFault()
			input.RuntimeError = peerStop
		}, ShareStopSender, FailureShareStopped, transferfault.Fault{}},
		{"output contract keeps precedence", func(input *SettlementInput) {
			input.Result.SettlementFault = outputContract
			input.RuntimeError = peerStop
		}, ShareStopSender, 0, outputContract},
		{"output interruption keeps precedence", func(input *SettlementInput) {
			input.Result.TerminationFault = outputIO
			input.ConnectionError = relayset.ErrReceiverShareStopped
		}, ShareStopRelays, 0, outputIO},
		{"source drift keeps precedence", func(input *SettlementInput) {
			input.Result.SourceDriftFault = sourceDrift
			input.RuntimeError = peerStop
		}, ShareStopSender, 0, sourceDrift},
		{"one stopped relay", func(input *SettlementInput) {
			input.ConnectionError = &relayv2.RelayError{Code: v2.ErrorStopped}
		}, ShareStopUnconfirmed, 0, sessionTerminal},
		{"other peer terminal", func(input *SettlementInput) {
			input.RuntimeError = peerSessionTerminalError(t, protocolsession.SessionTerminalCodeLast-1)
		}, ShareStopUnconfirmed, 0, sessionTerminal},
		{"transport failure", func(input *SettlementInput) {
			input.RuntimeError = opaqueCanaryError{"relay-token-canary"}
		}, ShareStopUnconfirmed, 0, sessionTerminal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := SettlementInput{Result: successfulJobResult(t)}
			input.Result.Outcome = transfer.DirectTreeOutcomePaused
			input.Result.TerminationFault = sessionTerminal
			test.mutate(&input)
			result, err := Settle(input)
			if err != nil {
				t.Fatal(err)
			}
			failure, ok := errors.AsType[Failure](result.Err)
			if !ok || failure.Code != test.wantCode || failure.Fault != test.wantFault {
				t.Fatalf("failure = %+v, want code %d fault %v", failure, test.wantCode, test.wantFault)
			}
			wantRetry := test.wantStop == ShareStopUnconfirmed
			recovery := result.Value.Recovery
			if recovery.ShareStop != test.wantStop || recovery.Retryable() != wantRetry ||
				(result.Outcome == task.OutcomePaused) != wantRetry {
				t.Fatalf("recovery = %+v outcome=%d, want stop %v retryable %t", recovery, result.Outcome, test.wantStop, wantRetry)
			}
			if input.RuntimeError != nil && !errors.Is(result.Err, input.RuntimeError) {
				t.Fatal("settlement dropped the diagnostic runtime cause")
			}
		})
	}
}

func TestPreparationFailureAfterConfirmedRelayStop(t *testing.T) {
	cause := fmt.Errorf("join: %w", relayset.ErrReceiverShareStopped)
	cleanupErr := errors.New("lease release failed")
	for _, cleanup := range []error{nil, cleanupErr} {
		completed := settlePreparationFailure(stepNetworkFailure, Failure{Cause: cause}, nil, cleanup)
		failure, ok := errors.AsType[Failure](completed.Err)
		if recovery := completed.Value.Recovery; !ok || recovery.ShareStop != ShareStopRelays ||
			recovery.Retryable() || !errors.Is(completed.Err, cause) {
			t.Fatalf("settled preparation = %+v", completed)
		}
		if cleanup == nil && failure.Code != FailureShareStopped ||
			cleanup != nil && (!errors.Is(failure, cleanup) || completed.Value.Recovery.Reason != RecoveryNeedsInspection) {
			t.Fatalf("primary failure = %+v, cleanup=%v", failure, cleanup)
		}
	}
}
