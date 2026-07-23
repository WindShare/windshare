package v2peer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
)

const (
	concurrentCandidateReplays = 64
	senderCandidateTestBuffer  = DefaultMaxCandidates + 4
)

type senderCandidateHarness struct {
	handler          *senderHandler
	peer             *testPeerConnection
	session          *testPeerSession
	operation        protocolsession.OperationID
	operationContext context.Context
	binding          v2signal.Binding
	attempt          *peerAttempt
	cancel           context.CancelFunc
	runDone          chan error
	stopOnce         sync.Once
}

func newSenderCandidateHarness(t *testing.T, maxCandidates int) *senderCandidateHarness {
	t.Helper()
	peer := newTestPeerConnection()
	peer.added = make(chan pion.ICECandidateInit, senderCandidateTestBuffer)
	factory := mustTestFactory(t, Config{
		MaxCandidates: maxCandidates,
		PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) {
			return peer, nil
		}),
	})
	session := newTestPeerSession(71)
	interfaceHandler, err := factory.NewSenderPeerHandler(session)
	if err != nil {
		t.Fatal(err)
	}
	handler := interfaceHandler.(*senderHandler)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- handler.Run(ctx) }()
	operation := testOperationID(72)
	binding := testBinding(73)
	offerBody, err := v2signal.EncodeOffer(v2signal.Offer{
		Binding: binding,
		SDP:     "v=0\r\ns=candidate-budget\r\n",
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	offer := testMessage(t, protocolsession.MessagePeerOffer, operation, offerBody)
	operationContext := testPeerMessageContext(t, ctx, offer)
	operationKey := testPeerOperationFromContext(t, operationContext, operation)
	if err := handler.HandleMessage(operationContext, offer); err != nil {
		cancel()
		t.Fatal(err)
	}
	receiveTest(t, peer.remote)
	receiveTest(t, session.controls)
	var attempt *peerAttempt
	waitForTest(t, func() bool {
		handler.mu.Lock()
		attempt = handler.attempts[operationKey]
		handler.mu.Unlock()
		return attempt != nil
	})
	harness := &senderCandidateHarness{
		handler: handler, peer: peer, session: session,
		operation: operation, operationContext: operationContext, binding: binding, attempt: attempt,
		cancel: cancel, runDone: runDone,
	}
	t.Cleanup(func() { harness.stop(t) })
	return harness
}

func (harness *senderCandidateHarness) sendCandidate(t *testing.T, body []byte) error {
	t.Helper()
	return harness.handler.HandleMessage(
		harness.operationContext,
		testMessage(t, protocolsession.MessagePeerCandidate, harness.operation, body),
	)
}

func (harness *senderCandidateHarness) stop(t *testing.T) {
	t.Helper()
	harness.stopOnce.Do(func() {
		harness.cancel()
		if err := receiveTest(t, harness.runDone); !errors.Is(err, context.Canceled) {
			t.Errorf("stop sender candidate harness: %v", err)
		}
	})
}

func (harness *senderCandidateHarness) candidateOperationReleased() bool {
	harness.attempt.inboxMu.Lock()
	closed, queued := harness.attempt.closed, len(harness.attempt.events)
	harness.attempt.inboxMu.Unlock()
	return closed && queued == 0
}

func testCandidate(t *testing.T, binding v2signal.Binding, seed int) (v2signal.Candidate, []byte) {
	t.Helper()
	mid := "data"
	line := uint16(0)
	fragment := fmt.Sprintf("candidate-%d", seed)
	candidate := v2signal.Candidate{
		Binding: binding,
		Candidate: fmt.Sprintf(
			"candidate:%d 1 udp 2122260223 192.0.2.%d %d typ host",
			seed, seed, 5_000+seed,
		),
		SDPMid: &mid, SDPMLineIndex: &line, UsernameFragment: &fragment,
	}
	body, err := v2signal.EncodeCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	return candidate, body
}

func TestSenderCandidateDistinctBodiesEnforceLeafBudget(t *testing.T) {
	harness := newSenderCandidateHarness(t, 2)
	first, firstBody := testCandidate(t, harness.binding, 101)
	second, secondBody := testCandidate(t, harness.binding, 102)
	_, overflowBody := testCandidate(t, harness.binding, 103)
	for _, body := range [][]byte{firstBody, secondBody} {
		if err := harness.sendCandidate(t, body); err != nil {
			t.Fatal(err)
		}
	}
	for index, want := range []string{first.Candidate, second.Candidate} {
		if added := receiveTest(t, harness.peer.added); added.Candidate != want {
			t.Fatalf("distinct candidate %d=%#v", index, added)
		}
	}
	if err := harness.sendCandidate(t, overflowBody); err != nil {
		t.Fatal(err)
	}
	failure := receiveTest(t, harness.session.failures)
	if failure.operation != harness.operation || failure.code != protocolsession.PeerOperationCodeCandidates {
		t.Fatalf("candidate budget failure=%#v", failure)
	}
	receiveTest(t, harness.peer.closed)
	select {
	case overflow := <-harness.peer.added:
		t.Fatalf("over-budget candidate reached Pion: %#v", overflow)
	default:
	}
	waitForTest(t, harness.candidateOperationReleased)
}

func TestSenderCandidateBindingAndDecodeConflictsRemainOperationErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		body func(*testing.T, v2signal.Binding) []byte
	}{
		{
			name: "decode",
			body: func(*testing.T, v2signal.Binding) []byte { return []byte{1} },
		},
		{
			name: "binding",
			body: func(t *testing.T, binding v2signal.Binding) []byte {
				binding.AttemptID[0] ^= 0xff
				_, body := testCandidate(t, binding, 111)
				return body
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newSenderCandidateHarness(t, 1)
			if err := harness.sendCandidate(t, test.body(t, harness.binding)); err != nil {
				t.Fatalf("enqueue candidate conflict: %v", err)
			}
			failure := receiveTest(t, harness.session.failures)
			if failure.operation != harness.operation || failure.code != protocolsession.PeerOperationCodeCandidates {
				t.Fatalf("candidate conflict failure=%#v", failure)
			}
			receiveTest(t, harness.peer.closed)
			select {
			case added := <-harness.peer.added:
				t.Fatalf("candidate conflict reached Pion: %#v", added)
			default:
			}
		})
	}
}
