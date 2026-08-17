package v2peer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestSenderRecoverableOfferRejectionTerminalizesIdentityOnceForSession(t *testing.T) {
	collector := &senderObservationCollector{}
	now := time.Unix(9_000, 0)
	factory := mustTestFactory(t, Config{
		Now:                          func() time.Time { return now },
		RetiredBindingTTL:            time.Minute,
		MaxActiveAttempts:            1,
		MaxSessionEvidenceIdentities: 2,
		Observer:                     SenderAttemptObserverFunc(collector.observe),
	})
	session := newTestPeerSession(121)
	handler, ctx, cancel, runDone := startSenderTestRuntime(t, factory, session)
	operation := testOperationID(122)
	binding := testBinding(123)
	body := recoverableRejectedOffer(t, binding)
	message := testMessage(t, protocolsession.MessagePeerOffer, operation, body)
	messageContext := testPeerMessageContext(t, ctx, message)

	if err := handler.HandleMessage(messageContext, message); err != nil {
		t.Fatalf("first malformed offer: %v", err)
	}
	if failure := receiveTest(t, session.failures); failure.operation != operation {
		t.Fatalf("first rejection = %#v", failure)
	}
	waitForTest(t, func() bool { return len(collector.forAttempt(binding.AttemptID)) == 3 })
	terminal := collector.forAttempt(binding.AttemptID)[2]
	if terminal.Stage != SenderAttemptFailed || terminal.Failure == nil ||
		terminal.Failure.FailedAtStage != SenderAttemptAnswerCreated ||
		terminal.Failure.TypedPeerErrorCode != TypedPeerErrorSignaling {
		t.Fatalf("recovered rejection terminal = %#v", terminal)
	}

	// Replay retention is deliberately finite. Its session claim remains exact
	// until the separately named evidence budget ends the entire session.
	now = now.Add(2 * time.Minute)
	if err := handler.HandleMessage(messageContext, message); err != nil {
		t.Fatalf("replayed malformed offer: %v", err)
	}
	receiveTest(t, session.failures)
	if observations := collector.forAttempt(binding.AttemptID); len(observations) != 3 {
		t.Fatalf("expired replay tombstone restarted evidence: %#v", observations)
	}
	handler.mu.Lock()
	claims := handler.evidenceAuthority.claimCount()
	exhausted := handler.evidenceAuthority.terminal
	handler.mu.Unlock()
	if claims != 1 || exhausted {
		t.Fatalf("expired replay consumed evidence budget: claims=%d terminal=%t", claims, exhausted)
	}
	stopSenderTestRuntime(t, cancel, runDone)
}

func TestRecoverOfferBindingRequiresUnambiguousValidIdentityPrefix(t *testing.T) {
	binding := testBinding(124)
	valid := recoverableRejectedOffer(t, binding)
	if recovered, ok := recoverOfferBinding(valid); !ok || recovered != binding {
		t.Fatalf("recovered binding = %#v, %t", recovered, ok)
	}

	zero := binding
	zero.AttemptID = v2signal.AttemptID{}
	for name, encoded := range map[string][]byte{
		"invalid cbor": {0xff},
		"short array":  mustEncodeCBOR(t, []any{uint64(v2signal.SignalingSchemaVersion)}),
		"wrong version": mustEncodeCBOR(t, []any{
			uint64(v2signal.SignalingSchemaVersion + 1), binding.PeerPathID[:], binding.AttemptID[:], "invalid",
		}),
		"zero identity": recoverableRejectedOffer(t, zero),
	} {
		t.Run(name, func(t *testing.T) {
			if recovered, ok := recoverOfferBinding(encoded); ok {
				t.Fatalf("recovered invalid prefix %#v", recovered)
			}
		})
	}
}

func recoverableRejectedOffer(t *testing.T, binding v2signal.Binding) []byte {
	t.Helper()
	// An integer tail is invalid SDP, but the schema/path/attempt prefix is exact
	// and therefore sufficient to terminalize the evidence identity.
	return mustEncodeCBOR(t, []any{
		uint64(v2signal.SignalingSchemaVersion),
		binding.PeerPathID[:],
		binding.AttemptID[:],
		uint64(7),
	})
}

func mustEncodeCBOR(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := cbor.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestSenderEvidenceClaimsReleaseOnlyWithProtocolSession(t *testing.T) {
	factory := mustTestFactory(t, Config{})
	handler := newDirectTestHandler(t, factory, newTestPeerSession(125))
	binding := testBinding(126)
	operation := testPeerOperation(testOperationID(127))
	handler.mu.Lock()
	first := handler.claimEvidenceLocked(operation, binding)
	second := handler.claimEvidenceLocked(operation, binding)
	if !first.acquired || second.acquired {
		handler.mu.Unlock()
		t.Fatal("evidence claim was not unique")
	}
	handler.mu.Unlock()
	if err := handler.startAttempt(
		context.Background(), operation, v2signal.Offer{Binding: binding, SDP: "v=0\r\n"},
	); !errors.Is(err, v2signal.ErrSignalBinding) {
		t.Fatalf("claimed identity restarted: %v", err)
	}
	handler.stopAll()
	if handler.evidenceAuthority.claimCount() != 0 {
		t.Fatalf("session shutdown retained evidence claims: %#v", handler.evidenceAuthority)
	}
}

func TestSenderCanceledBeforeEnqueueStillTerminalizesFirstBinding(t *testing.T) {
	for _, test := range []struct {
		name        string
		stopRuntime bool
		wantScope   AttemptFailureScope
		wantCode    TypedPeerErrorCode
	}{
		{
			name:      "caller cancellation while runtime remains authoritative",
			wantScope: AttemptFailureScopeAttempt,
			wantCode:  TypedPeerErrorCancelled,
		},
		{
			name:        "protocol session shutdown",
			stopRuntime: true,
			wantScope:   AttemptFailureScopeSession,
			wantCode:    TypedPeerErrorStopped,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			collector := &senderObservationCollector{}
			factory := mustTestFactory(t, Config{
				Observer: SenderAttemptObserverFunc(collector.observe),
			})
			handler := newDirectTestHandler(t, factory, newTestPeerSession(128))
			runtimeContext := context.Background()
			if test.stopRuntime {
				var cancelRuntime context.CancelFunc
				runtimeContext, cancelRuntime = context.WithCancel(context.Background())
				cancelRuntime()
			}
			handler.mu.Lock()
			handler.runtimeContext = runtimeContext
			handler.mu.Unlock()

			operation := testOperationID(129)
			binding := testBinding(130)
			body, err := v2signal.EncodeOffer(v2signal.Offer{Binding: binding, SDP: "v=0\r\n"})
			if err != nil {
				t.Fatal(err)
			}
			message := testMessage(t, protocolsession.MessagePeerOffer, operation, body)
			callerContext, cancelCaller := context.WithCancel(context.Background())
			messageContext := testPeerMessageContext(t, callerContext, message)
			cancelCaller()

			if err := handler.HandleMessage(messageContext, message); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled enqueue error = %v", err)
			}
			waitForTest(t, func() bool {
				return len(collector.forAttempt(binding.AttemptID)) == 3
			})
			assertUnstartedSenderTerminal(
				t,
				collector.forAttempt(binding.AttemptID),
				test.wantScope,
				test.wantCode,
			)
			if err := handler.HandleMessage(messageContext, message); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled replay error = %v", err)
			}
			if observations := collector.forAttempt(binding.AttemptID); len(observations) != 3 {
				t.Fatalf("canceled enqueue replay restarted evidence: %#v", observations)
			}
		})
	}
}

func TestSenderShutdownTerminalizesQueuedOfferBeforeClaimsRelease(t *testing.T) {
	collector := &senderObservationCollector{}
	factory := mustTestFactory(t, Config{
		Observer: SenderAttemptObserverFunc(collector.observe),
	})
	handler := newDirectTestHandler(t, factory, newTestPeerSession(131))
	operation := testOperationID(132)
	binding := testBinding(133)
	body, err := v2signal.EncodeOffer(v2signal.Offer{Binding: binding, SDP: "v=0\r\n"})
	if err != nil {
		t.Fatal(err)
	}
	message := testMessage(t, protocolsession.MessagePeerOffer, operation, body)
	if err := handler.HandleMessage(
		testPeerMessageContext(t, context.Background(), message),
		message,
	); err != nil {
		t.Fatalf("queue offer: %v", err)
	}

	handler.stopAll()
	waitForTest(t, func() bool { return len(collector.forAttempt(binding.AttemptID)) == 3 })
	assertUnstartedSenderTerminal(
		t,
		collector.forAttempt(binding.AttemptID),
		AttemptFailureScopeSession,
		TypedPeerErrorStopped,
	)
	if handler.evidenceAuthority.claimCount() != 0 {
		t.Fatalf("shutdown retained claims after publishing terminals: %#v", handler.evidenceAuthority)
	}
}

func TestSenderShutdownWaitsForAdmittedIngressBeforeReleasingClaims(t *testing.T) {
	collector := &senderObservationCollector{}
	factory := mustTestFactory(t, Config{
		Observer: SenderAttemptObserverFunc(collector.observe),
	})
	handler := newDirectTestHandler(t, factory, newTestPeerSession(134))
	operation := testOperationID(135)
	binding := testBinding(136)
	body, err := v2signal.EncodeOffer(v2signal.Offer{Binding: binding, SDP: "v=0\r\n"})
	if err != nil {
		t.Fatal(err)
	}
	message := testMessage(t, protocolsession.MessagePeerOffer, operation, body)
	messageContext := testPeerMessageContext(t, context.Background(), message)
	if !handler.beginIngress() {
		t.Fatal("test ingress was not admitted")
	}

	stopDone := make(chan struct{})
	go func() {
		handler.stopAll()
		close(stopDone)
	}()
	waitForTest(t, func() bool {
		handler.inboxMu.Lock()
		defer handler.inboxMu.Unlock()
		return handler.closed
	})
	select {
	case <-stopDone:
		t.Fatal("shutdown released claims while admitted ingress remained active")
	default:
	}

	if err := handler.handleMessage(messageContext, message); !errors.Is(err, context.Canceled) {
		t.Fatalf("crossing ingress error = %v", err)
	}
	handler.ingress.Done()
	receiveTest(t, stopDone)
	waitForTest(t, func() bool { return len(collector.forAttempt(binding.AttemptID)) == 3 })
	assertUnstartedSenderTerminal(
		t,
		collector.forAttempt(binding.AttemptID),
		AttemptFailureScopeSession,
		TypedPeerErrorStopped,
	)
	if handler.evidenceAuthority.claimCount() != 0 {
		t.Fatalf("crossing shutdown retained claims: %#v", handler.evidenceAuthority)
	}

	if err := handler.HandleMessage(messageContext, message); !errors.Is(err, context.Canceled) {
		t.Fatalf("post-shutdown ingress error = %v", err)
	}
	if observations := collector.forAttempt(binding.AttemptID); len(observations) != 3 {
		t.Fatalf("post-shutdown ingress restarted evidence: %#v", observations)
	}
}

func TestSenderDistinctBindingRejectionIsNotSuppressedByActiveOperation(t *testing.T) {
	collector := &senderObservationCollector{}
	factory := mustTestFactory(t, Config{
		Observer: SenderAttemptObserverFunc(collector.observe),
	})
	handler := newDirectTestHandler(t, factory, newTestPeerSession(137))
	operation := testPeerOperation(testOperationID(138))
	activeBinding := testBinding(139)
	rejectedBinding := testBinding(140)

	handler.mu.Lock()
	handler.attempts[operation] = &peerAttempt{}
	handler.bindings[activeBinding] = operation
	if claim := handler.claimEvidenceLocked(operation, activeBinding); !claim.acquired {
		handler.mu.Unlock()
		t.Fatalf("active evidence claim = %#v", claim)
	}
	handler.mu.Unlock()

	event := handlerEvent{
		kind:      handlerOffer,
		operation: operation,
		offer:     v2signal.Offer{Binding: rejectedBinding, SDP: "v=0\r\n"},
	}
	if err := handler.terminalizeUnstartedOffer(event, senderAttemptCancelledFailure()); err != nil {
		t.Fatalf("distinct binding rejection: %v", err)
	}
	waitForTest(t, func() bool {
		return len(collector.forAttempt(rejectedBinding.AttemptID)) == 3
	})
	assertUnstartedSenderTerminal(
		t,
		collector.forAttempt(rejectedBinding.AttemptID),
		AttemptFailureScopeAttempt,
		TypedPeerErrorCancelled,
	)

	if err := handler.terminalizeUnstartedOffer(event, senderAttemptCancelledFailure()); err != nil {
		t.Fatalf("distinct binding replay: %v", err)
	}
	if observations := collector.forAttempt(rejectedBinding.AttemptID); len(observations) != 3 {
		t.Fatalf("distinct binding replay restarted evidence: %#v", observations)
	}
}

func assertUnstartedSenderTerminal(
	t *testing.T,
	observations []SenderAttemptObservation,
	wantScope AttemptFailureScope,
	wantCode TypedPeerErrorCode,
) {
	t.Helper()
	if len(observations) != 3 || observations[0].Stage != SenderAttemptStarted ||
		observations[1].Stage != SenderAttemptOfferReceived {
		t.Fatalf("unstarted sender evidence = %#v", observations)
	}
	terminal := observations[2]
	if terminal.Stage != SenderAttemptFailed || terminal.Failure == nil ||
		terminal.Failure.FailedAtStage != SenderAttemptAnswerCreated ||
		terminal.Failure.Scope != wantScope || terminal.Failure.TypedPeerErrorCode != wantCode {
		t.Fatalf("unstarted sender terminal = %#v", terminal)
	}
}
