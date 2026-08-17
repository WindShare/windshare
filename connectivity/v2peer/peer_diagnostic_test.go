package v2peer

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/windshare/windshare/connectivity/v2signal"
)

func TestSenderAttemptTerminalTypedCodeTable(t *testing.T) {
	for _, test := range []struct {
		name              string
		result            error
		primary           error
		operationCanceled bool
		wantCode          TypedPeerErrorCode
		wantScope         AttemptFailureScope
	}{
		{
			name: "negotiation", result: ErrNegotiation, primary: ErrNegotiation,
			wantCode: TypedPeerErrorNegotiation, wantScope: AttemptFailureScopeAttempt,
		},
		{
			name: "timeout", result: errAttemptTimeout, primary: errAttemptTimeout,
			wantCode: TypedPeerErrorTimeout, wantScope: AttemptFailureScopeAttempt,
		},
		{
			name: "candidate", result: errCandidateLimit, primary: errCandidateLimit,
			wantCode: TypedPeerErrorCandidates, wantScope: AttemptFailureScopeAttempt,
		},
		{
			name: "admission", result: errChannelAdmission, primary: errChannelAdmission,
			wantCode: TypedPeerErrorAdmission, wantScope: AttemptFailureScopeAttempt,
		},
		{
			name:     "signaling contract",
			result:   errors.Join(ErrProtocol, v2signal.ErrInvalidSignal),
			primary:  errors.Join(ErrProtocol, v2signal.ErrInvalidSignal),
			wantCode: TypedPeerErrorSignaling, wantScope: AttemptFailureScopeAttempt,
		},
		{
			name: "attempt cancelled", result: context.Canceled, primary: ErrNegotiation,
			operationCanceled: true,
			wantCode:          TypedPeerErrorCancelled, wantScope: AttemptFailureScopeAttempt,
		},
		{
			name: "runtime stopped", result: context.Canceled, primary: context.Canceled,
			wantCode: TypedPeerErrorStopped, wantScope: AttemptFailureScopeSession,
		},
		{
			name: "unexpected", wantCode: TypedPeerErrorUnexpected,
			wantScope: AttemptFailureScopeAttempt,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := attemptFailure(test.result, test.primary, test.operationCanceled)
			if failure.TypedPeerErrorCode != test.wantCode || failure.Scope != test.wantScope {
				t.Fatalf("terminal failure = %#v", failure)
			}
		})
	}
}

func TestSenderAttemptStreamSaturationCannotBlockSettlement(t *testing.T) {
	factory := mustTestFactory(t, Config{
		SenderAttemptObservationCapacity:  1,
		PeerDiagnosticObservationCapacity: 2,
	})
	recorder := newSenderAttemptRecorder(factory, newTestPeerSession(201).sessionID, testBinding(202))
	recorder.complete(SenderAttemptStarted, SenderCandidateCounts{}, nil, nil)
	recorder.complete(SenderAttemptOfferReceived, SenderCandidateCounts{}, nil, nil)
	recorder.complete(SenderAttemptAnswerCreated, SenderCandidateCounts{}, nil, nil)
	recorder.fail(SenderAttemptFailure{
		Scope: AttemptFailureScopeAttempt, TypedPeerErrorCode: TypedPeerErrorNegotiation,
	})

	completion := factory.CompleteObservations()
	if completion.Attempts.Enqueued != 1 || completion.Attempts.Loss.CapacityDropped != 3 {
		t.Fatalf("attempt completion = %#v", completion.Attempts)
	}
	diagnostic := <-factory.PeerDiagnostics()
	if diagnostic.Reason != PeerDiagnosticStreamCapacity || diagnostic.Count != 3 {
		t.Fatalf("stream capacity diagnostic = %#v", diagnostic)
	}
}

func TestReceiverTerminationStreamSaturationCannotBlockPublication(t *testing.T) {
	factory, err := NewReceiverFactory(ReceiverFactoryConfig{
		ReceiverTerminationObservationCapacity: 1,
		PeerDiagnosticObservationCapacity:      2,
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt := &ReceiverAttempt{factory: factory}
	attempt.emitTerminationTrace(ReceiverTerminationTrace{localGeneration: 1})
	attempt.emitTerminationTrace(ReceiverTerminationTrace{localGeneration: 2})
	attempt.emitTerminationTrace(ReceiverTerminationTrace{localGeneration: 3})

	completion := factory.CompleteObservations()
	if completion.Terminations.Enqueued != 1 || completion.Terminations.Loss.CapacityDropped != 2 {
		t.Fatalf("termination completion = %#v", completion.Terminations)
	}
	diagnostic := <-factory.PeerDiagnostics()
	if diagnostic.Category != PeerDiagnosticReceiverTermination ||
		diagnostic.Reason != PeerDiagnosticStreamCapacity || diagnostic.Count != 2 {
		t.Fatalf("stream capacity diagnostic = %#v", diagnostic)
	}
}

func TestPeerDiagnosticStreamIsCumulativeBoundedAndSaturating(t *testing.T) {
	reporter, err := newPeerDiagnosticReporter(2)
	if err != nil {
		t.Fatal(err)
	}
	reporter.report(PeerDiagnosticSenderAttempt, PeerDiagnosticCleanupResidue)
	reporter.report(PeerDiagnosticSenderAttempt, PeerDiagnosticCleanupResidue)
	reporter.report(PeerDiagnosticSenderAttempt, PeerDiagnosticCleanupResidue)
	completion := reporter.completeObservations()
	if completion != (ObservationCompletion{Enqueued: 2, Loss: ObservationLoss{CapacityDropped: 1}}) {
		t.Fatalf("diagnostic completion = %#v", completion)
	}
	first := <-reporter.observations()
	second := <-reporter.observations()
	if first.Count != 1 || second.Count != 2 {
		t.Fatalf("cumulative diagnostic snapshots = %#v, %#v", first, second)
	}

	reporter, err = newPeerDiagnosticReporter(1)
	if err != nil {
		t.Fatal(err)
	}
	reporter.reportCount(PeerDiagnosticReceiverTermination, PeerDiagnosticEvidenceCapacity, math.MaxUint64)
	reporter.report(PeerDiagnosticReceiverTermination, PeerDiagnosticEvidenceCapacity)
	reporter.completeObservations()
	if observation := <-reporter.observations(); observation.Count != math.MaxUint64 {
		t.Fatalf("saturating diagnostic = %#v", observation)
	}
}

func TestPeerDiagnosticsHaveClosedTextFreeVocabulary(t *testing.T) {
	configType := reflect.TypeFor[Config]()
	for _, retired := range []string{"Observer", "DiagnosticObserver"} {
		if _, exists := configType.FieldByName(retired); exists {
			t.Fatalf("callback field %q remains in sender configuration", retired)
		}
	}
	receiverConfigType := reflect.TypeFor[ReceiverFactoryConfig]()
	for _, retired := range []string{"OnTermination", "OnTerminationContext", "DiagnosticObserver"} {
		if _, exists := receiverConfigType.FieldByName(retired); exists {
			t.Fatalf("callback field %q remains in receiver configuration", retired)
		}
	}
	diagnosticType := reflect.TypeFor[PeerDiagnosticObservation]()
	wantFields := []string{"Category", "Reason", "Count"}
	if diagnosticType.NumField() != len(wantFields) {
		t.Fatalf("diagnostic field count = %d", diagnosticType.NumField())
	}
	for index, want := range wantFields {
		if got := diagnosticType.Field(index).Name; got != want {
			t.Fatalf("diagnostic field[%d] = %q, want %q", index, got, want)
		}
	}
	validPairs := 0
	for _, category := range []PeerDiagnosticCategory{
		PeerDiagnosticSenderAttempt,
		PeerDiagnosticReceiverTermination,
	} {
		for _, reason := range []PeerDiagnosticReason{
			PeerDiagnosticStreamCapacity,
			PeerDiagnosticEvidenceCapacity,
			PeerDiagnosticCleanupResidue,
		} {
			if _, valid := peerDiagnosticIndex(category, reason); valid {
				validPairs++
			}
		}
	}
	if validPairs != peerDiagnosticCounterCount {
		t.Fatalf("valid diagnostic pair count = %d", validPairs)
	}
}

func TestSenderCleanupResidueProducesOnlyClosedDiagnostic(t *testing.T) {
	factory := mustTestFactory(t, Config{PeerDiagnosticObservationCapacity: 1})
	attempt := newPeerAttempt(peerAttemptConfig{
		factory:   factory,
		session:   newTestPeerSession(211),
		operation: testOperationID(212),
		offer:     v2signal.Offer{Binding: testBinding(213), SDP: "v=0\r\n"},
	})
	attempt.recorder.begin()
	providerCanary := errors.New("provider cleanup text must remain private")
	if err := attempt.finish(context.Background(), ErrNegotiation, providerCanary, false); !errors.Is(err, providerCanary) {
		t.Fatalf("finish result = %v", err)
	}
	factory.CompleteObservations()
	observation := <-factory.PeerDiagnostics()
	if observation.Category != PeerDiagnosticSenderAttempt ||
		observation.Reason != PeerDiagnosticCleanupResidue || observation.Count != 1 {
		t.Fatalf("cleanup diagnostic = %#v", observation)
	}
}
