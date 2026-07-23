package webrtc_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/catalog"
	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/internal/testnetwork"
)

type receiverIntegrationControl struct {
	kind protocolsession.MessageKind
	body []byte
}

func (control receiverIntegrationControl) Kind() protocolsession.MessageKind { return control.kind }
func (control receiverIntegrationControl) Body() []byte                      { return bytes.Clone(control.body) }

type receiverIntegrationResult struct {
	control v2peer.ReceiverControl
	err     error
}

type receiverIntegrationOperation struct {
	id      protocolsession.OperationID
	ingress *integrationPeerIngress
	results chan receiverIntegrationResult

	terminalOnce sync.Once
	terminalDone chan struct{}
	terminal     v2peer.ReceiverSignalingTermination
	binding      v2peer.ReceiverSignalingOperationBinding
}

func newReceiverIntegrationOperation(id protocolsession.OperationID) *receiverIntegrationOperation {
	return &receiverIntegrationOperation{
		id: id, results: make(chan receiverIntegrationResult, 64),
		terminalDone: make(chan struct{}),
	}
}

func (operation *receiverIntegrationOperation) OperationID() protocolsession.OperationID {
	return operation.id
}

func (operation *receiverIntegrationOperation) MaximumContinuations() (int, bool) {
	return operation.ingress.MaximumContinuations(operation.id)
}

func (operation *receiverIntegrationOperation) SendCandidate(
	ctx context.Context,
	body []byte,
) (protocolsession.OperationDisposition, error) {
	message, err := protocolsession.NewMessage(protocolsession.MessagePeerCandidate, &operation.id, body)
	if err != nil {
		return protocolsession.OperationDrop, err
	}
	return operation.ingress.Route(ctx, message)
}

func (operation *receiverIntegrationOperation) Receive(
	ctx context.Context,
) v2peer.ReceiverSignalingReceiveResult {
	select {
	case <-operation.terminalDone:
		return receiverIntegrationTerminalResult(operation.terminal)
	case <-ctx.Done():
		// The receive context is already exhausted; exact in-memory router cleanup
		// still needs an independent authority so the fixture cannot leak its call.
		terminal := operation.finish(
			context.Background(),
			ctx.Err(),
			true,
			v2peer.NewReceiverSignalingLocalTermination,
		)
		return receiverIntegrationTerminalResult(terminal)
	case result := <-operation.results:
		if result.err != nil {
			terminal := operation.finish(
				ctx,
				result.err,
				false,
				v2peer.NewReceiverSignalingRemoteTermination,
			)
			return receiverIntegrationTerminalResult(terminal)
		}
		select {
		case <-operation.terminalDone:
			return receiverIntegrationTerminalResult(operation.terminal)
		default:
			return v2peer.NewReceiverSignalingControlResult(result.control)
		}
	}
}

func (operation *receiverIntegrationOperation) Terminate(
	ctx context.Context,
) v2peer.ReceiverSignalingTermination {
	return operation.finish(
		ctx,
		nil,
		true,
		v2peer.NewReceiverSignalingLocalTermination,
	)
}

func (operation *receiverIntegrationOperation) finish(
	ctx context.Context,
	cause error,
	cancelExact bool,
	newTermination func(
		v2peer.ReceiverSignalingOperationBinding,
		error,
	) v2peer.ReceiverSignalingTermination,
) v2peer.ReceiverSignalingTermination {
	operation.terminalOnce.Do(func() {
		if cancelExact {
			cause = errors.Join(cause, operation.ingress.Cancel(ctx, operation.id))
		}
		operation.terminal = newTermination(operation.binding, cause)
		close(operation.terminalDone)
	})
	<-operation.terminalDone
	return operation.terminal
}

func receiverIntegrationTerminalResult(
	terminal v2peer.ReceiverSignalingTermination,
) v2peer.ReceiverSignalingReceiveResult {
	return v2peer.NewReceiverSignalingTerminationResult(terminal)
}

type receiverIntegrationSignaling struct {
	operation *receiverIntegrationOperation
}

func (signaling receiverIntegrationSignaling) OpenPeerOperation(
	ctx context.Context,
	binding v2peer.ReceiverSignalingOperationBinding,
	offer []byte,
) (v2peer.ReceiverSignalingOperation, error) {
	signaling.operation.binding = binding
	message, err := protocolsession.NewMessage(
		protocolsession.MessagePeerOffer,
		&signaling.operation.id,
		offer,
	)
	if err != nil {
		return nil, err
	}
	if err := signaling.operation.ingress.Deliver(ctx, message); err != nil {
		return nil, err
	}
	return signaling.operation, nil
}

type receiverIntegrationSenderSession struct {
	share     catalog.ShareInstance
	sessionID protocolsession.ProtocolSessionID
	operation *receiverIntegrationOperation
	lane      sessionruntime.LaneIdentity
	channels  chan protocolsession.FrameChannel
}

func (session *receiverIntegrationSenderSession) ShareInstance() catalog.ShareInstance {
	return session.share
}

func (session *receiverIntegrationSenderSession) ProtocolSessionID() protocolsession.ProtocolSessionID {
	return session.sessionID
}

func (session *receiverIntegrationSenderSession) SendPeerControl(
	ctx context.Context,
	kind protocolsession.MessageKind,
	operation protocolsession.OperationID,
	body []byte,
) (protocolsession.OperationDisposition, error) {
	if operation != session.operation.id {
		return protocolsession.OperationDrop, errors.New("sender changed the receiver peer operation identity")
	}
	select {
	case <-ctx.Done():
		return protocolsession.OperationDrop, ctx.Err()
	case session.operation.results <- receiverIntegrationResult{
		control: receiverIntegrationControl{kind: kind, body: bytes.Clone(body)},
	}:
		return protocolsession.OperationDeliver, nil
	}
}

func (session *receiverIntegrationSenderSession) FailPeerOperation(
	ctx context.Context,
	operation protocolsession.OperationID,
	_ uint16,
	message string,
) error {
	if operation != session.operation.id {
		return errors.New("sender failed a different peer operation")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case session.operation.results <- receiverIntegrationResult{err: errors.New(message)}:
		return nil
	}
}

func (session *receiverIntegrationSenderSession) AdmitPeerChannel(
	ctx context.Context,
	channel protocolsession.FrameChannel,
) (sessionruntime.LaneIdentity, error) {
	opened, ok := channel.(interface{ Opened() <-chan struct{} })
	if !ok {
		return sessionruntime.LaneIdentity{}, errors.New("sender peer channel omitted its open signal")
	}
	select {
	case <-ctx.Done():
		return sessionruntime.LaneIdentity{}, ctx.Err()
	case <-opened.Opened():
	}
	select {
	case <-ctx.Done():
		return sessionruntime.LaneIdentity{}, ctx.Err()
	case session.channels <- channel:
		return session.lane, nil
	}
}

type receiverIntegrationLanes struct {
	lane     sessionruntime.LaneIdentity
	channels chan protocolsession.FrameChannel
}

func (lanes receiverIntegrationLanes) RequestLane(
	context.Context,
	uint32,
) (sessionruntime.LaneAttachmentGrant, error) {
	return sessionruntime.LaneAttachmentGrant{
		LaneID: lanes.lane.ID, LaneEpoch: lanes.lane.Epoch,
		OperationID: integrationOperationID(0x42),
	}, nil
}

func (lanes receiverIntegrationLanes) AttachLane(
	ctx context.Context,
	_ sessionruntime.LaneAttachmentGrant,
	channel protocolsession.FrameChannel,
) (sessionruntime.LaneIdentity, error) {
	select {
	case <-ctx.Done():
		return sessionruntime.LaneIdentity{}, ctx.Err()
	case lanes.channels <- channel:
		return lanes.lane, nil
	}
}

func integrationOperationID(seed byte) protocolsession.OperationID {
	operation, _ := protocolsession.OperationIDFromBytes(
		bytes.Repeat([]byte{seed}, protocolsession.IdentityBytes),
	)
	return operation
}

func TestV2PeerReceiverAndSenderFactoriesInteroperateOverRealPion(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	share, _ := catalog.ShareInstanceFromBytes(bytes.Repeat([]byte{0x31}, catalog.IdentityBytes))
	sessionID, _ := protocolsession.ProtocolSessionIDFromBytes(
		bytes.Repeat([]byte{0x32}, protocolsession.IdentityBytes),
	)
	lane := sessionruntime.LaneIdentity{ID: 23, Epoch: 5}
	operation := newReceiverIntegrationOperation(integrationOperationID(0x33))
	senderSession := &receiverIntegrationSenderSession{
		share: share, sessionID: sessionID, operation: operation, lane: lane,
		channels: make(chan protocolsession.FrameChannel, 1),
	}
	senderFactory, err := v2peer.NewFactory(v2peer.Config{
		Configuration: pion.Configuration{}, AttemptTimeout: v2PeerIntegrationTimeout,
		MaxCandidates: v2PeerIntegrationMaxCandidates,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := senderFactory.NewSenderPeerHandler(senderSession)
	if err != nil {
		t.Fatal(err)
	}
	ingress, err := newIntegrationPeerIngress(senderFactory, handler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ingress.Close)
	operation.ingress = ingress

	ctx, cancel := context.WithTimeout(context.Background(), v2PeerIntegrationTimeout)
	defer cancel()
	handlerDone := make(chan error, 1)
	go func() { handlerDone <- handler.Run(ctx) }()

	receiverFactory, err := v2peer.NewReceiverFactory(v2peer.ReceiverFactoryConfig{
		Configuration: pion.Configuration{}, AttemptTimeout: v2PeerIntegrationTimeout,
		MaxCandidates: v2PeerIntegrationMaxCandidates,
	})
	if err != nil {
		t.Fatal(err)
	}
	receiverChannels := make(chan protocolsession.FrameChannel, 1)
	attempt, err := receiverFactory.Start(
		ctx,
		receiverIntegrationSignaling{operation: operation},
		receiverIntegrationLanes{lane: lane, channels: receiverChannels},
	)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-attempt.Ready():
	case <-attempt.Done():
		t.Fatalf("receiver Pion attempt failed before admission: %v", attempt.Err())
	case <-ctx.Done():
		t.Fatal("receiver Pion attempt did not become ready")
	}
	if attached, ok := attempt.Lane(); !ok || attached != lane {
		t.Fatalf("receiver attached lane = %+v, %v", attached, ok)
	}

	var senderChannel, receiverChannel protocolsession.FrameChannel
	select {
	case senderChannel = <-senderSession.channels:
	case <-ctx.Done():
		t.Fatal("sender did not admit its real Pion channel")
	}
	select {
	case receiverChannel = <-receiverChannels:
	case <-ctx.Done():
		t.Fatal("receiver did not attach its real Pion channel")
	}
	if err := receiverChannel.Send(ctx, framechannel.Frame("receiver-to-sender")); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-senderChannel.Recv():
		if !bytes.Equal(frame, []byte("receiver-to-sender")) {
			t.Fatalf("sender frame = %q", frame)
		}
	case <-ctx.Done():
		t.Fatal("sender did not receive the receiver frame")
	}
	if err := senderChannel.Send(ctx, framechannel.Frame("sender-to-receiver")); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-receiverChannel.Recv():
		if !bytes.Equal(frame, []byte("sender-to-receiver")) {
			t.Fatalf("receiver frame = %q", frame)
		}
	case <-ctx.Done():
		t.Fatal("receiver did not receive the sender frame")
	}

	if err := attempt.Close(); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-handlerDone:
	case <-time.After(v2PeerIntegrationTimeout):
		t.Fatal("sender peer handler did not stop")
	}
}
