package v2peer

import (
	"context"
	"sync"
	"testing"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type blockedCandidateSession struct {
	*testPeerSession
	started chan struct{}
	release chan struct{}
}

func (session *blockedCandidateSession) SendPeerControl(
	ctx context.Context,
	kind protocolsession.MessageKind,
	operation protocolsession.OperationID,
	body []byte,
) (protocolsession.OperationDisposition, error) {
	if kind == protocolsession.MessagePeerCandidate {
		close(session.started)
		select {
		case <-ctx.Done():
			return protocolsession.OperationDrop, context.Cause(ctx)
		case <-session.release:
		}
	}
	return session.testPeerSession.SendPeerControl(ctx, kind, operation, body)
}

type blockedCandidateOperation struct {
	*receiverTestOperation
	started chan struct{}
	release chan struct{}
}

func (operation *blockedCandidateOperation) SendCandidate(ctx context.Context, body []byte) (protocolsession.OperationDisposition, error) {
	close(operation.started)
	select {
	case <-ctx.Done():
		return protocolsession.OperationDrop, context.Cause(ctx)
	case <-operation.release:
		return operation.receiverTestOperation.SendCandidate(ctx, body)
	}
}

func TestReceiverAdmissionProgressesWhileCandidateSendIsBlocked(t *testing.T) {
	var operation *blockedCandidateOperation
	harness := newReceiverHarness(t, func(config *ReceiverFactoryConfig, signaling *receiverTestSignaling) {
		operation = &blockedCandidateOperation{
			receiverTestOperation: signaling.operation.(*receiverTestOperation),
			started:               make(chan struct{}), release: make(chan struct{}),
		}
		signaling.open = func(context.Context, ReceiverSignalingOperationBinding, []byte) (ReceiverSignalingOperation, error) {
			return operation, nil
		}
	})
	t.Cleanup(func() {
		close(operation.release)
		_ = harness.attempt.Close()
		if harness.admittedOwner != nil {
			_ = harness.admittedOwner.Close()
		}
	})
	harness.answer(t)
	harness.peer.emitCandidate(&pion.ICECandidate{
		Foundation: "1", Priority: 1, Address: "10.0.0.8", Port: 43000,
		Protocol: pion.ICEProtocolUDP, Typ: pion.ICECandidateTypeHost, Component: 1,
	})
	receiveTest(t, operation.started)
	harness.openAndAwaitLane(t)
}

func TestSenderAdmissionProgressesWhileCandidateSendIsBlocked(t *testing.T) {
	peer := newTestPeerConnection()
	channel := newTestPeerChannel()
	session := &blockedCandidateSession{
		testPeerSession: newTestPeerSession(131),
		started:         make(chan struct{}),
		release:         make(chan struct{}),
	}
	factory := mustTestFactory(t, Config{
		PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) {
			return peer, nil
		}),
		DataChannels: DataChannelAdapterFunc(func(*pion.DataChannel) (PeerDataChannel, error) {
			return channel, nil
		}),
	})
	attempt := newPeerAttempt(peerAttemptConfig{
		factory: factory, session: session, operation: testOperationID(132),
		offer:  v2signal.Offer{Binding: testBinding(133), SDP: "v=0\r\ns=receiver\r\n"},
		onDone: func(*peerAttempt, error) {},
	})
	var work sync.WaitGroup
	work.Add(1)
	attempt.start(context.Background(), &work)
	t.Cleanup(func() {
		close(session.release)
		attempt.stop(context.Canceled)
		_ = channel.Close()
		receiveTest(t, attempt.done)
		work.Wait()
		select {
		case owner := <-session.ownedAdmissions:
			_ = owner.Close()
		default:
		}
	})
	receiveTest(t, session.controls)
	peer.emitCandidate(&pion.ICECandidate{
		Foundation: "1", Priority: 1, Address: "10.0.0.8", Port: 43000,
		Protocol: pion.ICEProtocolUDP, Typ: pion.ICECandidateTypeHost, Component: 1,
	})
	receiveTest(t, session.started)
	// An already-open direct channel must not inherit relay backpressure from
	// optional candidate discovery. Leave that send blocked through admission.
	peer.emitDataChannel(&pion.DataChannel{})
	if admitted := receiveTest(t, session.admissions); admitted != channel {
		t.Fatal("admission did not receive the open direct channel")
	}
	waitForTest(t, attempt.attached.Load)
	ctx, cancel := context.WithTimeout(context.Background(), peerTestTimeout)
	defer cancel()
	if err := attempt.cancelOperation(ctx); err != nil {
		t.Fatalf("signaling retirement waited on the blocked candidate: %v", err)
	}
	if channel.State() != framechannel.Open {
		t.Fatal("retiring blocked signaling closed the admitted channel")
	}
}
