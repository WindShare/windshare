package protocolsession

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/windshare/windshare/core/framechannel"
)

func factualAttempt(t *testing.T, sequence uint32, outcome SendOutcome) SendAttemptSnapshot {
	t.Helper()
	disposition := framechannel.SendAccepted
	if outcome == SendOutcomeDropped {
		disposition = framechannel.SendRejected
	}
	attempt, err := NewSettledSendAttempt(SendAttemptIdentity{
		ResponseSequence: 1, AttemptSequence: sequence, LaneID: sequence,
	}, SendCompletion{Settled: true, Admitted: true, Outcome: outcome, TransportDisposition: disposition})
	if err != nil {
		t.Fatal(err)
	}
	return attempt
}

func TestResponseSendEvidencePreservesEveryAttempt(t *testing.T) {
	tests := []struct {
		name     string
		outcomes []SendOutcome
		want     ResponseSendEvidence
	}{
		{"all dropped", []SendOutcome{SendOutcomeDropped, SendOutcomeDropped}, ResponseSendEvidenceDefinitelyNotSent},
		{"uncertain then dropped", []SendOutcome{SendOutcomeUnknown, SendOutcomeDropped}, ResponseSendEvidenceUncertain},
		{"dropped then uncertain", []SendOutcome{SendOutcomeDropped, SendOutcomeUnknown}, ResponseSendEvidenceUncertain},
		{"uncertain then confirmed", []SendOutcome{SendOutcomeUnknown, SendOutcomeTransportConfirmed}, ResponseSendEvidenceTransportConfirmed},
		{"confirmed retains precedence", []SendOutcome{SendOutcomeTransportConfirmed, SendOutcomeUnknown, SendOutcomeDropped}, ResponseSendEvidenceTransportConfirmed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var attempts []SendAttemptSnapshot
			for index, outcome := range test.outcomes {
				attempts = append(attempts, factualAttempt(t, uint32(index+1), outcome))
			}
			got, err := FoldResponseSendEvidence(attempts)
			if err != nil || got != test.want {
				t.Fatalf("evidence=%v err=%v, want %v", got, err, test.want)
			}
			result, err := NewResponseSendReturned(ResponseSendEndPolicySuppressed, attempts)
			if err != nil || result.Evidence() != test.want {
				t.Fatalf("policy end changed physical evidence: result=%+v err=%v", result, err)
			}
			cleaned := result.WithCleanup(SendCleanupFailed)
			if cleaned.Evidence() != test.want || cleaned.Cleanup() != SendCleanupFailed || result.Cleanup() != SendCleanupNone {
				t.Fatalf("cleanup changed evidence or original snapshot: result=%+v cleaned=%+v", result, cleaned)
			}
		})
	}
}

func TestResponseSendResultOwnsImmutableBoundedHistory(t *testing.T) {
	first := factualAttempt(t, 1, SendOutcomeUnknown)
	last := factualAttempt(t, 2, SendOutcomeTransportConfirmed)
	attempts := []SendAttemptSnapshot{first, last}
	result, err := NewResponseSendReturned(ResponseSendEndTransportConfirmed, attempts)
	if err != nil {
		t.Fatal(err)
	}
	attempts[0] = SendAttemptSnapshot{}
	if result.IsZero() || !result.Started() || result.AttemptCount() != 2 || result.End() != ResponseSendEndTransportConfirmed {
		t.Fatalf("result=%+v", result)
	}
	observed, ok := result.Attempt(0)
	if !ok || observed != first {
		t.Fatalf("caller mutated retained history: %+v", observed)
	}
	for _, index := range []int{-1, 2} {
		if attempt, exists := result.Attempt(index); exists || !attempt.IsZero() {
			t.Fatalf("out-of-range attempt %d = %+v, %v", index, attempt, exists)
		}
	}
	if _, pending := result.PendingAttempt(); pending {
		t.Fatal("settled response reports a pending attempt")
	}
}

func TestResponseSendPendingAndNotStartedHaveDifferentFacts(t *testing.T) {
	id := SendAttemptIdentity{ResponseSequence: 5, AttemptSequence: 1, LaneID: 3, LaneEpoch: 8}
	completion := SendCompletion{Admitted: true, Outcome: SendOutcomeUnknown}
	pending, err := NewPendingSendAttempt(id, completion)
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewResponseSendWaitingEnded(ResponseSendEndCallerCanceled, []SendAttemptSnapshot{pending})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := result.PendingAttempt(); !ok || got != id || !result.Started() ||
		result.Evidence() != ResponseSendEvidenceUncertain || pending.Settled() ||
		!pending.PolicyAdmitted() || pending.TransportDisposition() != 0 {
		t.Fatalf("pending evidence = %+v, %+v", result, pending)
	}
	completion.Settled = true
	completion.Outcome = SendOutcomeTransportConfirmed
	completion.TransportDisposition = framechannel.SendAccepted
	settled, err := NewSettledSendAttempt(id, completion)
	if err != nil || !settled.Settled() || settled.Outcome() != SendOutcomeTransportConfirmed ||
		result.Evidence() != ResponseSendEvidenceUncertain {
		t.Fatalf("settlement mutated previous evidence: %+v, %+v, %v", settled, result, err)
	}
	notStarted, err := NewResponseSendNotStarted(ResponseSendEndRouteUnavailable)
	if err != nil || notStarted.IsZero() || notStarted.Started() ||
		notStarted.Evidence() != ResponseSendEvidenceDefinitelyNotSent || notStarted.AttemptCount() != 0 {
		t.Fatalf("not started = %+v, %v", notStarted, err)
	}
	if _, pending := notStarted.PendingAttempt(); pending {
		t.Fatal("not-started response manufactured a receipt")
	}
	rejected, err := NewRejectedSendAttempt(id, ErrWriterStopped)
	if err != nil || rejected.Settled() || rejected.PolicyAdmitted() ||
		rejected.Outcome() != SendOutcomeDropped || rejected.End() != SendAttemptEndRejectedBeforeReceipt {
		t.Fatalf("rejected-before-receipt = %+v, %v", rejected, err)
	}
}

func TestSendSnapshotRejectsInvalidPhysicalFacts(t *testing.T) {
	id := SendAttemptIdentity{ResponseSequence: 1, AttemptSequence: 1, LaneID: 1}
	for _, badID := range []SendAttemptIdentity{
		{}, {ResponseSequence: 1, LaneID: 1}, {AttemptSequence: 1, LaneID: 1},
		{ResponseSequence: 1, AttemptSequence: 1},
		{ResponseSequence: 1, AttemptSequence: DefaultMaxLogicalLanes + 1, LaneID: 1},
	} {
		if _, err := NewRejectedSendAttempt(badID, ErrWriterStopped); !errors.Is(err, ErrSendAttemptSnapshot) {
			t.Fatalf("invalid identity accepted: %+v", badID)
		}
	}
	for _, completion := range []SendCompletion{
		{}, {Settled: true}, {Settled: true, Outcome: SendOutcome(255)},
		{Settled: true, Outcome: SendOutcomeTransportConfirmed},
		{Settled: true, Outcome: SendOutcomeUnknown, TransportDisposition: framechannel.SendRejected},
		{Settled: true, Outcome: SendOutcomeDropped, TransportDisposition: framechannel.SendAccepted},
	} {
		if attempt, err := NewSettledSendAttempt(id, completion); !errors.Is(err, ErrSendAttemptSnapshot) || !attempt.IsZero() {
			t.Fatalf("invalid settlement accepted: %+v, %+v, %v", completion, attempt, err)
		}
	}
	for _, completion := range []SendCompletion{
		{}, {Settled: true, Outcome: SendOutcomeUnknown},
		{Outcome: SendOutcomeDropped}, {Outcome: SendOutcomeUnknown, TransportDisposition: framechannel.SendAccepted},
	} {
		if _, err := NewPendingSendAttempt(id, completion); !errors.Is(err, ErrSendAttemptSnapshot) {
			t.Fatalf("invalid pending completion accepted: %+v", completion)
		}
	}
	for _, disposition := range []framechannel.SendDisposition{0, framechannel.SendRejected, framechannel.SendRetired} {
		if _, err := NewSettledSendAttempt(id, SendCompletion{Settled: true, Outcome: SendOutcomeDropped, TransportDisposition: disposition}); err != nil {
			t.Fatalf("valid pre-transport settlement rejected: %v", err)
		}
	}
}

func TestResponseSendInvalidAndUninitializedNeverBecomeBusinessEvidence(t *testing.T) {
	if !(ResponseSendResult{}).IsZero() || (ResponseSendResult{}).Started() ||
		(ResponseSendResult{}).Evidence() != ResponseSendEvidenceUninitialized ||
		(ResponseSendResult{}).End() != ResponseSendEndUninitialized {
		t.Fatal("zero result manufactured business evidence")
	}
	confirmed := factualAttempt(t, 1, SendOutcomeTransportConfirmed)
	for _, attempts := range [][]SendAttemptSnapshot{
		nil, {{}}, {confirmed, {}}, make([]SendAttemptSnapshot, DefaultMaxLogicalLanes+1),
		{{identity: confirmed.Identity(), end: SendAttemptEnd(255)}},
		{{identity: confirmed.Identity(), end: SendAttemptEndRejectedBeforeReceipt, outcome: SendOutcomeDropped, policyAdmitted: true}},
		{{identity: confirmed.Identity(), end: SendAttemptEndSettled, outcome: SendOutcome(255)}},
		{{end: SendAttemptEndWaitingEnded, outcome: SendOutcomeUnknown}},
	} {
		if evidence, err := FoldResponseSendEvidence(attempts); err == nil || evidence != ResponseSendEvidenceUninitialized {
			t.Fatalf("invalid history produced %v, %v", evidence, err)
		}
	}
	if !(ResponseSendResult{}).WithCleanup(SendCleanupFailed).IsZero() ||
		!(ResponseSendResult{evidence: ResponseSendEvidenceDefinitelyNotSent}).WithCleanup(SendCleanupKind(255)).IsZero() {
		t.Fatal("invalid cleanup initialized a result")
	}
}

func TestSendAttemptCauseSurvivesSuccessfulRetryAndBoundsDetail(t *testing.T) {
	id := SendAttemptIdentity{ResponseSequence: 1, AttemptSequence: 1, LaneID: 1}
	failure := errors.New(strings.Repeat("\u754c", MaximumSendAttemptCauseDetailBytes))
	first, err := NewSettledSendAttempt(id, SendCompletion{
		Settled: true, Admitted: true, Outcome: SendOutcomeUnknown,
		TransportDisposition: framechannel.SendAccepted, Err: failure,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewResponseSendReturned(ResponseSendEndTransportConfirmed, []SendAttemptSnapshot{
		first, factualAttempt(t, 2, SendOutcomeTransportConfirmed),
	})
	if err != nil {
		t.Fatal(err)
	}
	observed, _ := result.Attempt(0)
	cause := observed.Cause()
	if result.Evidence() != ResponseSendEvidenceTransportConfirmed ||
		cause.Kind() != SendAttemptCauseTransportFailure || !cause.Truncated() ||
		len(cause.Detail()) > MaximumSendAttemptCauseDetailBytes || !utf8.ValidString(cause.Detail()) {
		t.Fatalf("successful retry erased or corrupted prior cause: %+v", cause)
	}
	for _, test := range []struct {
		err  error
		want SendAttemptCauseKind
	}{
		{context.Canceled, SendAttemptCauseCanceled},
		{context.DeadlineExceeded, SendAttemptCauseDeadline},
		{ErrControlQueueFull, SendAttemptCauseControlQueueFull},
		{ErrDataQueueFull, SendAttemptCauseDataQueueFull},
		{ErrWriterStopped, SendAttemptCauseWriterStopped},
		{ErrWriterTerminal, SendAttemptCauseWriterTerminal},
		{errors.New("admission rejected"), SendAttemptCauseAdmissionFailure},
	} {
		rejected, err := NewRejectedSendAttempt(id, test.err)
		if err != nil || rejected.Cause().Kind() != test.want ||
			rejected.Cause().Detail() != test.err.Error() || rejected.Cause().Truncated() {
			t.Fatalf("rejection cause = %+v, %v", rejected.Cause(), err)
		}
	}
	pending, err := NewPendingSendAttempt(id, SendCompletion{Outcome: SendOutcomeUnknown, Err: context.DeadlineExceeded})
	if err != nil || pending.Cause().Kind() != SendAttemptCauseDeadline {
		t.Fatalf("pending cause=%+v, %v", pending.Cause(), err)
	}
	prepared, err := NewSettledSendAttempt(id, SendCompletion{Settled: true, Outcome: SendOutcomeDropped, Err: errors.New("bad nonce")})
	if err != nil || prepared.Cause().Kind() != SendAttemptCausePreparationFailure {
		t.Fatalf("preparation cause=%+v, %v", prepared.Cause(), err)
	}
	invalidText, err := NewRejectedSendAttempt(id, errors.New(string([]byte{255})))
	if err != nil || !utf8.ValidString(invalidText.Cause().Detail()) {
		t.Fatalf("invalid error text not bounded UTF-8: %+v", invalidText.Cause())
	}
}

func TestInvalidReceiptPreservesHistoryWithoutUnsentCleanupAuthority(t *testing.T) {
	prior := factualAttempt(t, 1, SendOutcomeDropped)
	result, err := NewResponseSendReturned(ResponseSendEndInvalidReceipt, []SendAttemptSnapshot{prior})
	if err != nil || result.IsZero() || !result.Started() ||
		result.Evidence() != ResponseSendEvidenceUninitialized {
		t.Fatalf("invalid receipt granted business evidence: %+v, %v", result, err)
	}
	cleaned := result.WithCleanup(SendCleanupOperationRetired)
	if attempt, ok := cleaned.Attempt(0); !ok || attempt != prior ||
		cleaned.End() != ResponseSendEndInvalidReceipt ||
		cleaned.Cleanup() != SendCleanupOperationRetired {
		t.Fatalf("cleanup erased invalid call history: %+v", cleaned)
	}
}

func TestResponseSendHistoryBoundaries(t *testing.T) {
	first := factualAttempt(t, 1, SendOutcomeDropped)
	second := factualAttempt(t, 2, SendOutcomeDropped)
	pending, err := NewPendingSendAttempt(first.Identity(), SendCompletion{Outcome: SendOutcomeUnknown})
	if err != nil {
		t.Fatal(err)
	}
	otherResponse := second
	otherResponse.identity.ResponseSequence++
	for _, test := range []struct {
		end      ResponseSendEnd
		attempts []SendAttemptSnapshot
	}{
		{ResponseSendEndUninitialized, []SendAttemptSnapshot{first}},
		{ResponseSendEnd(255), []SendAttemptSnapshot{first}},
		{ResponseSendEndTransportConfirmed, []SendAttemptSnapshot{first}},
		{ResponseSendEndPolicySuppressed, nil},
		{ResponseSendEndPolicySuppressed, []SendAttemptSnapshot{second}},
		{ResponseSendEndPolicySuppressed, []SendAttemptSnapshot{first, otherResponse}},
		{ResponseSendEndCallerCanceled, []SendAttemptSnapshot{pending}},
	} {
		if _, err := NewResponseSendReturned(test.end, test.attempts); err == nil {
			t.Fatalf("invalid returned boundary accepted: %+v", test)
		}
	}
	for _, end := range []ResponseSendEnd{ResponseSendEndUninitialized, ResponseSendEndTransportConfirmed, ResponseSendEndPolicySuppressed, ResponseSendEndInvalidReceipt} {
		if _, err := NewResponseSendNotStarted(end); err == nil {
			t.Fatalf("not-started end %d accepted", end)
		}
	}
	if _, err := NewResponseSendWaitingEnded(ResponseSendEndPolicySuppressed, []SendAttemptSnapshot{pending}); err == nil {
		t.Fatal("policy suppression manufactured stopped waiting")
	}
	for _, attempts := range [][]SendAttemptSnapshot{{first}, {pending, second}} {
		if _, err := NewResponseSendWaitingEnded(ResponseSendEndCallerCanceled, attempts); err == nil {
			t.Fatal("waiting-ended requires exactly one pending tail")
		}
	}
	attempts := make([]SendAttemptSnapshot, DefaultMaxLogicalLanes)
	for index := range attempts {
		attempts[index] = factualAttempt(t, uint32(index+1), SendOutcomeDropped)
	}
	if result, err := NewResponseSendReturned(ResponseSendEndAttemptsExhausted, attempts); err != nil || result.AttemptCount() != DefaultMaxLogicalLanes {
		t.Fatalf("maximum history rejected: %v", err)
	}
}
