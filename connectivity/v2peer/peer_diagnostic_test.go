package v2peer

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"

	"github.com/windshare/windshare/connectivity/v2signal"
)

type peerDiagnosticCollector struct {
	mu           sync.Mutex
	observations []PeerDiagnosticObservation
}

func (collector *peerDiagnosticCollector) observe(observation PeerDiagnosticObservation) {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	collector.observations = append(collector.observations, observation)
}

func (collector *peerDiagnosticCollector) latest(
	category PeerDiagnosticCategory,
	reason PeerDiagnosticReason,
) (PeerDiagnosticObservation, bool) {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	for index := len(collector.observations) - 1; index >= 0; index-- {
		observation := collector.observations[index]
		if observation.Category == category && observation.Reason == reason {
			return observation, true
		}
	}
	return PeerDiagnosticObservation{}, false
}

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

func TestSenderObserverCapacityDoesNotBlockAttemptSettlement(t *testing.T) {
	observerStarted := make(chan struct{})
	releaseObserver := make(chan struct{})
	var startedOnce sync.Once
	diagnostics := &peerDiagnosticCollector{}
	factory := mustTestFactory(t, Config{
		SenderObservationCapacity: 1,
		Observer: SenderAttemptObserverFunc(func(SenderAttemptObservation) {
			startedOnce.Do(func() { close(observerStarted) })
			<-releaseObserver
		}),
		DiagnosticObserver: PeerDiagnosticObserverFunc(diagnostics.observe),
	})
	recorder := newSenderAttemptRecorder(factory, newTestPeerSession(201).sessionID, testBinding(202))
	recorder.complete(SenderAttemptStarted, SenderCandidateCounts{}, nil, nil)
	receiveTest(t, observerStarted)

	settled := make(chan struct{})
	go func() {
		recorder.complete(SenderAttemptOfferReceived, SenderCandidateCounts{}, nil, nil)
		recorder.complete(SenderAttemptAnswerCreated, SenderCandidateCounts{}, nil, nil)
		recorder.fail(SenderAttemptFailure{
			Scope: AttemptFailureScopeAttempt, TypedPeerErrorCode: TypedPeerErrorNegotiation,
		})
		close(settled)
	}()
	receiveTest(t, settled)
	waitForTest(t, func() bool {
		observation, ok := diagnostics.latest(
			PeerDiagnosticSenderAttempt,
			PeerDiagnosticObserverCapacity,
		)
		return ok && observation.Count >= 1
	})
	close(releaseObserver)
}

func TestReceiverTerminationObserverCapacityDoesNotBlockPublication(t *testing.T) {
	observerStarted := make(chan struct{})
	releaseObserver := make(chan struct{})
	var startedOnce sync.Once
	diagnostics := &peerDiagnosticCollector{}
	factory, err := NewReceiverFactory(ReceiverFactoryConfig{
		TerminationObservationCapacity: 1,
		OnTermination: func(ReceiverTerminationTrace) {
			startedOnce.Do(func() { close(observerStarted) })
			<-releaseObserver
		},
		DiagnosticObserver: PeerDiagnosticObserverFunc(diagnostics.observe),
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt := &ReceiverAttempt{factory: factory}
	attempt.emitTerminationTrace(ReceiverTerminationTrace{})
	receiveTest(t, observerStarted)

	published := make(chan struct{})
	go func() {
		attempt.emitTerminationTrace(ReceiverTerminationTrace{})
		attempt.emitTerminationTrace(ReceiverTerminationTrace{})
		close(published)
	}()
	receiveTest(t, published)
	waitForTest(t, func() bool {
		observation, ok := diagnostics.latest(
			PeerDiagnosticReceiverTermination,
			PeerDiagnosticObserverCapacity,
		)
		return ok && observation.Count == 1
	})
	close(releaseObserver)
}

func TestPeerDiagnosticsAreClosedTextFreeAndSaturating(t *testing.T) {
	configType := reflect.TypeFor[Config]()
	if _, exists := configType.FieldByName("OnError"); exists {
		t.Fatal("raw error callback remains part of sender configuration")
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
	if got := saturatingAdd(math.MaxUint64-1, 2); got != math.MaxUint64 {
		t.Fatalf("saturating count = %d", got)
	}
}

func TestSenderCleanupResidueProducesOnlyClosedDiagnostic(t *testing.T) {
	diagnostics := &peerDiagnosticCollector{}
	factory := mustTestFactory(t, Config{
		DiagnosticObserver: PeerDiagnosticObserverFunc(diagnostics.observe),
	})
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
	waitForTest(t, func() bool {
		observation, ok := diagnostics.latest(
			PeerDiagnosticSenderAttempt,
			PeerDiagnosticCleanupResidue,
		)
		return ok && observation.Count == 1
	})
}
