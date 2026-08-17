package v2peer

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestSenderEvidenceIdentityBudgetConfigurationIsExplicitAndBounded(t *testing.T) {
	factory := mustTestFactory(t, Config{})
	if factory.maxSessionEvidenceIdentities != DefaultMaxSessionEvidenceIdentities ||
		factory.maxSessionEvidenceIdentities <= 0 {
		t.Fatalf("default evidence identity budget = %d", factory.maxSessionEvidenceIdentities)
	}
	for name, config := range map[string]Config{
		"negative": {MaxSessionEvidenceIdentities: -1},
		"above implementation bound": {
			MaxSessionEvidenceIdentities: maximumSessionEvidenceIdentities + 1,
		},
		"cannot cover active attempts": {
			MaxActiveAttempts: 2, MaxSessionEvidenceIdentities: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewFactory(config); !errors.Is(err, ErrConfig) {
				t.Fatalf("invalid evidence budget error = %v", err)
			}
		})
	}
}

func TestSenderEvidenceAuthorityBoundsNormalClaimsAndOwnsOneTerminalIdentity(t *testing.T) {
	authority := newSenderEvidenceAuthority(2)
	firstBinding := testBinding(140)
	secondBinding := testBinding(142)
	boundaryBinding := testBinding(144)
	operation := testPeerOperation(testOperationID(146))

	if claim := authority.claim(operation, firstBinding); !claim.acquired || claim.sessionTerminal {
		t.Fatalf("first claim = %#v", claim)
	}
	if claim := authority.claim(operation, firstBinding); claim.acquired || claim.sessionTerminal {
		t.Fatalf("first replay = %#v", claim)
	}
	if claim := authority.claim(operation, secondBinding); !claim.acquired || claim.sessionTerminal {
		t.Fatalf("second claim = %#v", claim)
	}
	if claim := authority.claim(operation, boundaryBinding); !claim.acquired || !claim.sessionTerminal {
		t.Fatalf("boundary claim = %#v", claim)
	}
	if claim := authority.claim(operation, boundaryBinding); claim.acquired || !claim.sessionTerminal {
		t.Fatalf("boundary replay = %#v", claim)
	}
	if claim := authority.claim(operation, testBinding(148)); claim.acquired || !claim.sessionTerminal {
		t.Fatalf("post-terminal claim = %#v", claim)
	}
	if authority.claimCount() != 2 || !authority.claimed(boundaryBinding) {
		t.Fatalf("bounded authority = %#v", authority)
	}
	authority.reset()
	if authority.claimCount() != 0 || authority.terminal {
		t.Fatalf("reset authority = %#v", authority)
	}
}

func TestSenderEvidenceCapacityTerminalizesBoundaryAndEndsProtocolSession(t *testing.T) {
	collector := &senderObservationCollector{}
	var peerCreations atomic.Int32
	factory := mustTestFactory(t, Config{
		MaxActiveAttempts:            1,
		MaxSessionEvidenceIdentities: 1,
		Observer:                     SenderAttemptObserverFunc(collector.observe),
		PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) {
			peerCreations.Add(1)
			return nil, errors.New("synthetic first attempt failure")
		}),
	})
	session := newTestPeerSession(150)
	handler, ctx, cancel, runDone := startSenderTestRuntime(t, factory, session)
	defer cancel()

	_, firstBinding, _ := sendSenderTestOffer(t, handler, ctx, 151)
	waitForTest(t, func() bool { return len(collector.forAttempt(firstBinding.AttemptID)) == 3 })
	receiveTest(t, session.failures)
	waitForTest(t, func() bool {
		handler.mu.Lock()
		defer handler.mu.Unlock()
		return len(handler.attempts) == 0
	})

	boundaryOperation, boundaryBinding, boundaryContext := sendSenderTestOffer(t, handler, ctx, 153)
	if err := receiveTest(t, runDone); !errors.Is(err, ErrEvidenceIdentityCapacity) {
		t.Fatalf("capacity run result = %v", err)
	}
	waitForTest(t, func() bool {
		return len(collector.forAttempt(boundaryBinding.AttemptID)) == 3
	})
	assertUnstartedSenderTerminal(
		t,
		collector.forAttempt(boundaryBinding.AttemptID),
		AttemptFailureScopeSession,
		TypedPeerErrorStopped,
	)
	terminal := collector.forAttempt(boundaryBinding.AttemptID)[2]
	if terminal.Failure.Message != peerEvidenceCapacityFailureMessage {
		t.Fatalf("capacity terminal = %#v", terminal)
	}
	if peerCreations.Load() != 1 {
		t.Fatalf("peer creations = %d, want 1", peerCreations.Load())
	}
	select {
	case failure := <-session.failures:
		t.Fatalf("capacity boundary emitted an operation failure: %#v", failure)
	default:
	}
	if handler.evidenceAuthority.claimCount() != 0 || handler.evidenceAuthority.terminal {
		t.Fatalf("completed session retained evidence authority: %#v", handler.evidenceAuthority)
	}

	body, err := v2signal.EncodeOffer(v2signal.Offer{Binding: boundaryBinding, SDP: "v=0\r\n"})
	if err != nil {
		t.Fatal(err)
	}
	message := testMessage(t, protocolsession.MessagePeerOffer, boundaryOperation, body)
	if err := handler.HandleMessage(boundaryContext, message); !errors.Is(err, context.Canceled) {
		t.Fatalf("post-terminal replay = %v", err)
	}
	if observations := collector.forAttempt(boundaryBinding.AttemptID); len(observations) != 3 {
		t.Fatalf("post-terminal replay restarted evidence: %#v", observations)
	}
}

func TestSenderEvidenceCapacityConcurrentBoundaryHasOneTerminalOwner(t *testing.T) {
	collector := &senderObservationCollector{}
	var peerCreations atomic.Int32
	factory := mustTestFactory(t, Config{
		MaxActiveAttempts:            1,
		MaxSessionEvidenceIdentities: 1,
		Observer:                     SenderAttemptObserverFunc(collector.observe),
		PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) {
			peerCreations.Add(1)
			return nil, errors.New("peer creation must not run")
		}),
	})
	handler := newDirectTestHandler(t, factory, newTestPeerSession(155))
	handler.mu.Lock()
	if claim := handler.claimEvidenceLocked(testPeerOperation(testOperationID(156)), testBinding(157)); !claim.acquired || claim.sessionTerminal {
		handler.mu.Unlock()
		t.Fatalf("prime claim = %#v", claim)
	}
	handler.mu.Unlock()
	for len(handler.events) < cap(handler.events) {
		handler.events <- handlerEvent{kind: handlerReject}
	}

	type concurrentOffer struct {
		binding v2signal.Binding
		message protocolsession.Message
		ctx     context.Context
	}
	offers := make([]concurrentOffer, 0, 2)
	for seed := byte(158); seed < 160; seed++ {
		operation := testOperationID(seed)
		binding := testBinding(seed + 2)
		body, err := v2signal.EncodeOffer(v2signal.Offer{Binding: binding, SDP: "v=0\r\n"})
		if err != nil {
			t.Fatal(err)
		}
		message := testMessage(t, protocolsession.MessagePeerOffer, operation, body)
		offers = append(offers, concurrentOffer{
			binding: binding,
			message: message,
			ctx:     testPeerMessageContext(t, context.Background(), message),
		})
	}

	start := make(chan struct{})
	errorsSeen := make(chan error, len(offers))
	var work sync.WaitGroup
	for _, offer := range offers {
		work.Go(func() {
			<-start
			errorsSeen <- handler.HandleMessage(offer.ctx, offer.message)
		})
	}
	close(start)
	work.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if !errors.Is(err, ErrEvidenceIdentityCapacity) {
			t.Fatalf("concurrent boundary result = %v", err)
		}
	}
	waitForTest(t, func() bool {
		total := 0
		for _, offer := range offers {
			total += len(collector.forAttempt(offer.binding.AttemptID))
		}
		return total == 3
	})
	terminalStreams := 0
	for _, offer := range offers {
		observations := collector.forAttempt(offer.binding.AttemptID)
		if len(observations) == 0 {
			continue
		}
		terminalStreams++
		assertUnstartedSenderTerminal(
			t, observations, AttemptFailureScopeSession, TypedPeerErrorStopped,
		)
		if observations[2].Failure.Message != peerEvidenceCapacityFailureMessage {
			t.Fatalf("concurrent terminal = %#v", observations[2])
		}
	}
	if terminalStreams != 1 || handler.evidenceAuthority.claimCount() != 1 || peerCreations.Load() != 0 {
		t.Fatalf(
			"concurrent authority: terminals=%d claims=%d peerCreations=%d",
			terminalStreams,
			handler.evidenceAuthority.claimCount(),
			peerCreations.Load(),
		)
	}
	handler.stopAll()
}

func TestSenderEvidenceCapacityObserverPanicStillEndsAndCleansSession(t *testing.T) {
	var observations atomic.Int32
	diagnostics := &peerDiagnosticCollector{}
	var peerCreations atomic.Int32
	factory := mustTestFactory(t, Config{
		MaxActiveAttempts:            1,
		MaxSessionEvidenceIdentities: 1,
		Observer: SenderAttemptObserverFunc(func(SenderAttemptObservation) {
			observations.Add(1)
			panic("synthetic capacity observer panic")
		}),
		DiagnosticObserver: PeerDiagnosticObserverFunc(diagnostics.observe),
		PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) {
			peerCreations.Add(1)
			return nil, errors.New("peer creation must not run")
		}),
	})
	handler := newDirectTestHandler(t, factory, newTestPeerSession(161))
	handler.mu.Lock()
	handler.claimEvidenceLocked(testPeerOperation(testOperationID(162)), testBinding(163))
	handler.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- handler.Run(ctx) }()
	defer cancel()

	operation := testOperationID(164)
	binding := testBinding(165)
	body, err := v2signal.EncodeOffer(v2signal.Offer{Binding: binding, SDP: "v=0\r\n"})
	if err != nil {
		t.Fatal(err)
	}
	message := testMessage(t, protocolsession.MessagePeerOffer, operation, body)
	if err := handler.HandleMessage(
		testPeerMessageContext(t, ctx, message), message,
	); err != nil {
		t.Fatalf("enqueue capacity boundary: %v", err)
	}
	if err := receiveTest(t, runDone); !errors.Is(err, ErrEvidenceIdentityCapacity) {
		t.Fatalf("observer panic capacity result = %v", err)
	}
	waitForTest(t, func() bool { return observations.Load() == 3 })
	if observations.Load() != 3 || peerCreations.Load() != 0 {
		t.Fatalf("observer boundary: observations=%d peers=%d", observations.Load(), peerCreations.Load())
	}
	waitForTest(t, func() bool {
		panicObservation, panicOK := diagnostics.latest(
			PeerDiagnosticSenderAttempt,
			PeerDiagnosticObserverPanic,
		)
		capacityObservation, capacityOK := diagnostics.latest(
			PeerDiagnosticSenderAttempt,
			PeerDiagnosticEvidenceCapacity,
		)
		return panicOK && panicObservation.Count == 3 &&
			capacityOK && capacityObservation.Count == 1
	})
	if handler.evidenceAuthority.claimCount() != 0 || handler.evidenceAuthority.terminal {
		t.Fatalf("observer panic retained authority: %#v", handler.evidenceAuthority)
	}
}
