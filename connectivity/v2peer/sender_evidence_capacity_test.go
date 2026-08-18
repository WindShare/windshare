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
	const documentedBrowserSessionAttemptCeiling = 8
	factory := mustTestFactory(t, Config{})
	if SenderMaxSessionEvidenceIdentities != 64 ||
		factory.maxSessionEvidenceIdentities != SenderMaxSessionEvidenceIdentities {
		t.Fatalf("default evidence identity budget = %d", factory.maxSessionEvidenceIdentities)
	}
	ordinaryIdentityCapacity := SenderMaxSessionEvidenceIdentities - senderEvidenceTerminalIdentityReserve
	if documentedBrowserSessionAttemptCeiling*8 != SenderMaxSessionEvidenceIdentities ||
		documentedBrowserSessionAttemptCeiling >= ordinaryIdentityCapacity {
		t.Fatalf(
			"browser/session evidence relationship = %d/%d ordinary=%d",
			documentedBrowserSessionAttemptCeiling,
			SenderMaxSessionEvidenceIdentities,
			ordinaryIdentityCapacity,
		)
	}
	for name, config := range map[string]Config{
		"negative":            {MaxSessionEvidenceIdentities: -1},
		"no terminal reserve": {MaxSessionEvidenceIdentities: 1},
		"above semantic ceiling": {
			MaxSessionEvidenceIdentities: SenderMaxSessionEvidenceIdentities + 1,
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
	authority := newSenderEvidenceAuthority(3)
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
	if authority.retainedIdentityCount() != 3 || len(authority.claims) != 2 ||
		!authority.claimed(boundaryBinding) {
		t.Fatalf("bounded authority = %#v", authority)
	}
	authority.reset()
	if authority.retainedIdentityCount() != 0 || authority.terminal {
		t.Fatalf("reset authority = %#v", authority)
	}
}

func TestSenderEvidenceAuthorityRetainsAtMostDocumentedIdentityCeiling(t *testing.T) {
	authority := newSenderEvidenceAuthority(SenderMaxSessionEvidenceIdentities)
	operation := testPeerOperation(testOperationID(170))
	ordinaryCapacity := SenderMaxSessionEvidenceIdentities - senderEvidenceTerminalIdentityReserve
	for index := range ordinaryCapacity {
		binding := testBinding(byte(index + 1))
		if claim := authority.claim(operation, binding); !claim.acquired || claim.sessionTerminal {
			t.Fatalf("ordinary claim %d = %#v", index, claim)
		}
	}
	boundary := testBinding(byte(ordinaryCapacity + 1))
	if claim := authority.claim(operation, boundary); !claim.acquired || !claim.sessionTerminal {
		t.Fatalf("capacity boundary = %#v", claim)
	}
	if authority.retainedIdentityCount() != SenderMaxSessionEvidenceIdentities ||
		len(authority.claims) != ordinaryCapacity || !authority.claimed(boundary) {
		t.Fatalf("bounded authority = %#v", authority)
	}
	if claim := authority.claim(operation, testBinding(byte(ordinaryCapacity+2))); claim.acquired || !claim.sessionTerminal {
		t.Fatalf("post-ceiling claim = %#v", claim)
	}
	if authority.retainedIdentityCount() != SenderMaxSessionEvidenceIdentities {
		t.Fatalf("post-ceiling identities = %d", authority.retainedIdentityCount())
	}
}

func TestSenderEvidenceCapacityTerminalizesBoundaryAndEndsProtocolSession(t *testing.T) {
	collector := &senderObservationCollector{}
	var peerCreations atomic.Int32
	factory := mustTestFactoryWithSenderCollector(t, collector, Config{
		MaxSessionEvidenceIdentities: 2,
		PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) {
			peerCreations.Add(1)
			return nil, errors.New("synthetic first attempt failure")
		}),
	})
	session := newTestPeerSession(150)
	handler, ctx, cancel, runDone := startSenderTestRuntime(t, factory, session)
	defer cancel()

	_, firstBinding, _ := sendSenderTestOffer(t, handler, ctx, 151)
	waitForTest(t, func() bool {
		return senderAttemptReachedTerminal(collector.forAttempt(firstBinding.AttemptID), SenderAttemptFailed)
	})
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
		return senderAttemptReachedTerminal(collector.forAttempt(boundaryBinding.AttemptID), SenderAttemptFailed)
	})
	assertUnstartedSenderTerminal(
		t,
		collector.forAttempt(boundaryBinding.AttemptID),
		AttemptFailureScopeSession,
		TypedPeerErrorStopped,
	)
	terminal := collector.forAttempt(boundaryBinding.AttemptID)[1]
	if terminal.Failure.Message != "" {
		t.Fatalf("capacity terminal exposed an internal message: %#v", terminal)
	}
	if peerCreations.Load() != 1 {
		t.Fatalf("peer creations = %d, want 1", peerCreations.Load())
	}
	select {
	case failure := <-session.failures:
		t.Fatalf("capacity boundary emitted an operation failure: %#v", failure)
	default:
	}
	if handler.evidenceAuthority.retainedIdentityCount() != 0 || handler.evidenceAuthority.terminal {
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
	if observations := collector.forAttempt(boundaryBinding.AttemptID); len(observations) != 2 {
		t.Fatalf("post-terminal replay restarted evidence: %#v", observations)
	}
}

func TestSenderEvidenceCapacityConcurrentBoundaryHasOneTerminalOwner(t *testing.T) {
	collector := &senderObservationCollector{}
	var peerCreations atomic.Int32
	factory := mustTestFactoryWithSenderCollector(t, collector, Config{
		MaxSessionEvidenceIdentities: 2,
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
		return total == 2
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
		if observations[1].Failure.Message != "" {
			t.Fatalf("concurrent terminal exposed an internal message: %#v", observations[1])
		}
	}
	if terminalStreams != 1 || handler.evidenceAuthority.retainedIdentityCount() != 2 ||
		len(handler.evidenceAuthority.claims) != 1 || peerCreations.Load() != 0 {
		t.Fatalf(
			"concurrent authority: terminals=%d claims=%d peerCreations=%d",
			terminalStreams,
			handler.evidenceAuthority.retainedIdentityCount(),
			peerCreations.Load(),
		)
	}
	handler.stopAll()
}

func TestSenderEvidenceCapacityWithUnreadStreamStillEndsAndCleansSession(t *testing.T) {
	var peerCreations atomic.Int32
	factory := mustTestFactory(t, Config{
		MaxSessionEvidenceIdentities:      2,
		SenderAttemptObservationCapacity:  1,
		PeerDiagnosticObservationCapacity: 4,
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
		t.Fatalf("unread stream capacity result = %v", err)
	}
	if peerCreations.Load() != 0 {
		t.Fatalf("peer creations = %d", peerCreations.Load())
	}
	completion := factory.CompleteObservations()
	if completion.Attempts != (ObservationCompletion{Enqueued: 1, Loss: ObservationLoss{CapacityDropped: 1}}) {
		t.Fatalf("attempt completion = %#v", completion.Attempts)
	}
	diagnostics := make(map[PeerDiagnosticReason]PeerDiagnosticObservation)
	for observation := range factory.PeerDiagnostics() {
		diagnostics[observation.Reason] = observation
	}
	if diagnostics[PeerDiagnosticEvidenceCapacity].Count != 1 ||
		diagnostics[PeerDiagnosticStreamCapacity].Count != 1 {
		t.Fatalf("capacity diagnostics = %#v", diagnostics)
	}
	if handler.evidenceAuthority.retainedIdentityCount() != 0 || handler.evidenceAuthority.terminal {
		t.Fatalf("unread stream retained authority: %#v", handler.evidenceAuthority)
	}
}
