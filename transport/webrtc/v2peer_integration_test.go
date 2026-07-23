package webrtc_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/catalog"
	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/internal/testnetwork"
	transportwebrtc "github.com/windshare/windshare/transport/webrtc"
)

const (
	v2PeerIntegrationTimeout       = 10 * time.Second
	v2PeerIntegrationMaxCandidates = 16
)

type integrationPeerIngress struct {
	mu          sync.Mutex
	router      *protocolsession.RoleRouter
	authorityMu sync.Mutex
	generations map[protocolsession.OperationID]protocolsession.OperationGeneration
}

type integrationPeerTrackedHandler struct {
	ingress *integrationPeerIngress
	handler sessionruntime.SenderPeerHandler
}

func (handler integrationPeerTrackedHandler) HandleMessage(
	ctx context.Context,
	message protocolsession.Message,
) error {
	operationID, ok := message.OperationID()
	if !ok {
		return protocolsession.ErrInvalidOperationID
	}
	generation, ok := protocolsession.OperationGenerationFromContext(ctx, operationID)
	if !ok || generation.IsZero() {
		return protocolsession.ErrUnknownOperation
	}
	handler.ingress.authorityMu.Lock()
	handler.ingress.generations[operationID] = generation
	handler.ingress.authorityMu.Unlock()
	return handler.handler.HandleMessage(ctx, message)
}

type integrationPeerCancelHandler struct {
	handler sessionruntime.SenderPeerHandler
}

func (handler integrationPeerCancelHandler) HandleMessage(
	ctx context.Context,
	message protocolsession.Message,
) error {
	operationID, ok := message.OperationID()
	if !ok {
		return protocolsession.ErrInvalidOperationID
	}
	return handler.handler.Cancel(ctx, operationID)
}

func newIntegrationPeerIngress(
	classifier protocolsession.OperationContinuationClassifier,
	handler sessionruntime.SenderPeerHandler,
) (*integrationPeerIngress, error) {
	table, err := protocolsession.NewOperationTableWithContinuations(
		protocolsession.OperationLimits{MaxActive: 4, MaxTombstones: 4},
		nil,
		classifier,
	)
	if err != nil {
		return nil, err
	}
	router, err := protocolsession.NewRoleRouter(protocolsession.RoleSender, table)
	if err != nil {
		return nil, err
	}
	ingress := &integrationPeerIngress{
		router: router, generations: make(map[protocolsession.OperationID]protocolsession.OperationGeneration),
	}
	tracked := integrationPeerTrackedHandler{ingress: ingress, handler: handler}
	for _, kind := range []protocolsession.MessageKind{
		protocolsession.MessagePeerOffer,
		protocolsession.MessagePeerCandidate,
	} {
		if err := router.RegisterHandler(kind, tracked); err != nil {
			router.Close()
			return nil, err
		}
	}
	if err := router.RegisterHandler(
		protocolsession.MessageCancel,
		integrationPeerCancelHandler{handler: handler},
	); err != nil {
		router.Close()
		return nil, err
	}
	return ingress, nil
}

func (ingress *integrationPeerIngress) Route(
	ctx context.Context,
	message protocolsession.Message,
) (protocolsession.OperationDisposition, error) {
	ingress.mu.Lock()
	defer ingress.mu.Unlock()
	disposition, err := ingress.router.RouteInbound(ctx, message)
	if err != nil || disposition != protocolsession.OperationDeliver {
		return disposition, err
	}
	event, err := ingress.router.Next(ctx)
	if err != nil {
		return protocolsession.OperationDrop, err
	}
	return disposition, ingress.router.Dispatch(ctx, event)
}

func (ingress *integrationPeerIngress) Deliver(
	ctx context.Context,
	message protocolsession.Message,
) error {
	disposition, err := ingress.Route(ctx, message)
	if err != nil {
		return err
	}
	if disposition != protocolsession.OperationDeliver {
		return fmt.Errorf("integration peer control disposition %d", disposition)
	}
	return nil
}

func (ingress *integrationPeerIngress) Cancel(
	ctx context.Context,
	operationID protocolsession.OperationID,
) error {
	body, err := contentflow.EncodeCancelReason(contentflow.CancelReasonSuperseded)
	if err != nil {
		return err
	}
	message, err := protocolsession.NewMessage(protocolsession.MessageCancel, &operationID, body)
	if err != nil {
		return err
	}
	return ingress.Deliver(ctx, message)
}

func (ingress *integrationPeerIngress) MaximumContinuations(
	operationID protocolsession.OperationID,
) (int, bool) {
	ingress.authorityMu.Lock()
	generation, ok := ingress.generations[operationID]
	ingress.authorityMu.Unlock()
	if !ok {
		return 0, false
	}
	return generation.MaximumContinuations()
}

func (ingress *integrationPeerIngress) Close() {
	if ingress != nil && ingress.router != nil {
		ingress.router.Close()
	}
}

type integrationPeerSession struct {
	share     catalog.ShareInstance
	sessionID protocolsession.ProtocolSessionID
	offerer   *pion.PeerConnection
	binding   v2signal.Binding
	operation protocolsession.OperationID

	mu      sync.Mutex
	channel protocolsession.FrameChannel
	failure error
	answer  chan struct{}
	admit   chan sessionruntime.LaneIdentity
}

func (session *integrationPeerSession) ShareInstance() catalog.ShareInstance { return session.share }
func (session *integrationPeerSession) ProtocolSessionID() protocolsession.ProtocolSessionID {
	return session.sessionID
}
func (session *integrationPeerSession) SendPeerControl(
	_ context.Context,
	kind protocolsession.MessageKind,
	operation protocolsession.OperationID,
	body []byte,
) (protocolsession.OperationDisposition, error) {
	if operation != session.operation {
		return protocolsession.OperationDrop, errors.New("sender changed the peer operation identity")
	}
	switch kind {
	case protocolsession.MessagePeerAnswer:
		answer, err := v2signal.DecodeAnswer(body)
		if err != nil || answer.Binding != session.binding {
			return protocolsession.OperationDrop, errors.Join(errors.New("sender answer changed the signaling binding"), err)
		}
		if err := session.offerer.SetRemoteDescription(pion.SessionDescription{
			Type: pion.SDPTypeAnswer,
			SDP:  answer.SDP,
		}); err != nil {
			return protocolsession.OperationDeliver, err
		}
		close(session.answer)
		return protocolsession.OperationDeliver, nil
	case protocolsession.MessagePeerCandidate:
		candidate, err := v2signal.DecodeCandidate(body)
		if err != nil || candidate.Binding != session.binding {
			return protocolsession.OperationDrop, errors.Join(errors.New("sender candidate changed the signaling binding"), err)
		}
		return protocolsession.OperationDeliver, session.offerer.AddICECandidate(pion.ICECandidateInit{
			Candidate: candidate.Candidate, SDPMid: candidate.SDPMid,
			SDPMLineIndex: candidate.SDPMLineIndex, UsernameFragment: candidate.UsernameFragment,
		})
	default:
		return protocolsession.OperationDrop, fmt.Errorf("unexpected peer control kind %d", kind)
	}
}
func (session *integrationPeerSession) FailPeerOperation(
	_ context.Context,
	_ protocolsession.OperationID,
	_ uint16,
	message string,
) error {
	session.mu.Lock()
	session.failure = errors.New(message)
	session.mu.Unlock()
	return nil
}
func (session *integrationPeerSession) AdmitPeerChannel(
	ctx context.Context,
	channel protocolsession.FrameChannel,
) (sessionruntime.LaneIdentity, error) {
	opened, ok := channel.(interface{ Opened() <-chan struct{} })
	if !ok {
		return sessionruntime.LaneIdentity{}, errors.New("peer adapter omitted its open signal")
	}
	select {
	case <-ctx.Done():
		return sessionruntime.LaneIdentity{}, ctx.Err()
	case <-opened.Opened():
	}
	if err := channel.Send(ctx, framechannel.Frame("sender-to-browser")); err != nil {
		return sessionruntime.LaneIdentity{}, err
	}
	select {
	case <-ctx.Done():
		return sessionruntime.LaneIdentity{}, ctx.Err()
	case frame, ok := <-channel.Recv():
		if !ok || !bytes.Equal(frame, []byte("browser-to-sender")) {
			return sessionruntime.LaneIdentity{}, errors.New("peer FrameChannel changed binary frame delivery")
		}
	}
	lane := sessionruntime.LaneIdentity{ID: 17, Epoch: 3}
	session.mu.Lock()
	session.channel = channel
	session.mu.Unlock()
	session.admit <- lane
	return lane, nil
}

func TestV2PeerSenderNegotiatesRealPionDataChannel(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	offerer, err := pion.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatalf("create browser-side PeerConnection: %v", err)
	}
	t.Cleanup(func() { _ = offerer.Close() })

	raw, err := offerer.CreateDataChannel(
		transportwebrtc.ChannelLabel,
		transportwebrtc.DefaultDataChannelInit(),
	)
	if err != nil {
		t.Fatalf("create browser-side DataChannel: %v", err)
	}
	browserChannel, err := transportwebrtc.NewChannel(raw)
	if err != nil {
		t.Fatalf("wrap browser-side DataChannel: %v", err)
	}
	t.Cleanup(func() { _ = browserChannel.Close() })

	share, _ := catalog.ShareInstanceFromBytes(bytes.Repeat([]byte{1}, catalog.IdentityBytes))
	sessionID, _ := protocolsession.ProtocolSessionIDFromBytes(
		bytes.Repeat([]byte{2}, protocolsession.IdentityBytes),
	)
	operation, _ := protocolsession.OperationIDFromBytes(
		bytes.Repeat([]byte{3}, protocolsession.IdentityBytes),
	)
	var binding v2signal.Binding
	copy(binding.PeerPathID[:], bytes.Repeat([]byte{4}, v2signal.IdentityBytes))
	copy(binding.AttemptID[:], bytes.Repeat([]byte{5}, v2signal.IdentityBytes))
	session := &integrationPeerSession{
		share: share, sessionID: sessionID, offerer: offerer, binding: binding, operation: operation,
		answer: make(chan struct{}), admit: make(chan sessionruntime.LaneIdentity, 1),
	}
	factory, err := v2peer.NewFactory(v2peer.Config{
		Configuration: pion.Configuration{}, AttemptTimeout: v2PeerIntegrationTimeout,
		MaxCandidates: v2PeerIntegrationMaxCandidates,
	})
	if err != nil {
		t.Fatalf("create sender peer factory: %v", err)
	}
	handler, err := factory.NewSenderPeerHandler(session)
	if err != nil {
		t.Fatalf("create sender peer handler: %v", err)
	}
	ingress, err := newIntegrationPeerIngress(factory, handler)
	if err != nil {
		t.Fatalf("create tracked sender peer ingress: %v", err)
	}
	t.Cleanup(ingress.Close)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- handler.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runDone:
		case <-time.After(v2PeerIntegrationTimeout):
			t.Error("sender peer handler did not stop")
		}
	})

	remoteCandidates := make(chan pion.ICECandidateInit, 32)
	offerer.OnICECandidate(func(candidate *pion.ICECandidate) {
		if candidate != nil {
			remoteCandidates <- candidate.ToJSON()
		}
	})
	forwardDone := make(chan error, 1)
	startForwarding := make(chan struct{})
	go func() {
		<-startForwarding
		for {
			select {
			case <-ctx.Done():
				forwardDone <- nil
				return
			case candidate := <-remoteCandidates:
				body, encodeErr := v2signal.EncodeCandidate(v2signal.Candidate{
					Binding: binding, Candidate: candidate.Candidate, SDPMid: candidate.SDPMid,
					SDPMLineIndex: candidate.SDPMLineIndex, UsernameFragment: candidate.UsernameFragment,
				})
				if encodeErr != nil {
					forwardDone <- encodeErr
					return
				}
				message, messageErr := protocolsession.NewMessage(
					protocolsession.MessagePeerCandidate, &operation, body,
				)
				if messageErr == nil {
					_, messageErr = ingress.Route(ctx, message)
				}
				if messageErr != nil {
					forwardDone <- messageErr
					return
				}
			}
		}
	}()

	offer, err := offerer.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create browser offer: %v", err)
	}
	if err := offerer.SetLocalDescription(offer); err != nil {
		t.Fatalf("set browser offer: %v", err)
	}
	localOffer := offerer.LocalDescription()
	if localOffer == nil {
		t.Fatal("browser PeerConnection omitted local offer")
	}
	offerBody, err := v2signal.EncodeOffer(v2signal.Offer{Binding: binding, SDP: localOffer.SDP})
	if err != nil {
		t.Fatalf("encode browser offer: %v", err)
	}
	offerMessage, err := protocolsession.NewMessage(
		protocolsession.MessagePeerOffer, &operation, offerBody,
	)
	if err != nil {
		t.Fatalf("create peer offer message: %v", err)
	}
	if err := ingress.Deliver(ctx, offerMessage); err != nil {
		t.Fatalf("deliver peer offer: %v", err)
	}
	close(startForwarding)

	select {
	case <-session.answer:
	case <-time.After(v2PeerIntegrationTimeout):
		t.Fatal("sender did not return a Pion answer")
	}
	browserExchange := make(chan error, 1)
	go func() {
		select {
		case <-ctx.Done():
			browserExchange <- ctx.Err()
			return
		case <-browserChannel.Opened():
		}
		select {
		case <-ctx.Done():
			browserExchange <- ctx.Err()
		case frame, ok := <-browserChannel.Recv():
			if !ok || !bytes.Equal(frame, []byte("sender-to-browser")) {
				browserExchange <- errors.New("browser received the wrong peer frame")
				return
			}
			browserExchange <- browserChannel.Send(ctx, framechannel.Frame("browser-to-sender"))
		}
	}()

	select {
	case lane := <-session.admit:
		if lane.ID != 17 || lane.Epoch != 3 {
			t.Fatalf("admitted lane = %#v", lane)
		}
	case <-time.After(v2PeerIntegrationTimeout):
		t.Fatal("real Pion DataChannel was not admitted")
	}
	if err := <-browserExchange; err != nil {
		t.Fatalf("peer FrameChannel exchange: %v", err)
	}
	session.mu.Lock()
	failure := session.failure
	session.mu.Unlock()
	if failure != nil {
		t.Fatalf("sender reported peer failure: %v", failure)
	}

	if err := ingress.Cancel(ctx, operation); err != nil {
		t.Fatalf("cancel signaling operation: %v", err)
	}
	if err := browserChannel.Close(); err != nil {
		t.Fatalf("close browser peer channel: %v", err)
	}
	cancel()
	select {
	case err := <-forwardDone:
		if err != nil {
			t.Fatalf("forward browser candidates: %v", err)
		}
	case <-time.After(v2PeerIntegrationTimeout):
		t.Fatal("candidate forwarder did not stop")
	}
}
