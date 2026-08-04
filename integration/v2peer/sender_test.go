package v2peer_test

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
	"github.com/windshare/windshare/internal/testloopback"
	"github.com/windshare/windshare/internal/testrun"
	transportwebrtc "github.com/windshare/windshare/transport/webrtc"
)

const (
	v2PeerIntegrationTimeout       = 10 * time.Second
	v2PeerIntegrationMaxCandidates = 16

	senderOfferCreateFailureReason       = "offer_create_failed"
	senderOfferSetLocalFailureReason     = "offer_set_local_failed"
	senderLocalOfferMissingFailureReason = "local_offer_missing"
	senderOfferEncodeFailureReason       = "offer_encode_failed"
	senderOfferMessageFailureReason      = "offer_message_create_failed"
	senderOfferDeliveryFailureReason     = "offer_delivery_failed"
	senderAnswerTimeoutFailureReason     = "answer_timeout"
	senderLaneIdentityFailureReason      = "lane_identity_mismatch"
	senderAdmissionTimeoutFailureReason  = "admission_timeout"
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
	requireLongPionIntegration(t)
	loopback := testloopback.New(t)
	trace := startIntegrationScenario(t, senderRealPionScenario, senderRealPionComponent)
	trace.RequireCleanup(t, "loopback fixture", func(context.Context) error {
		return loopback.Close()
	})
	offererAPI := loopback.NewPionAPI()
	senderAPI := loopback.NewPionAPI()
	offerer, err := offererAPI.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatalf("create browser-side PeerConnection: %v", err)
	}

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
	share, _ := catalog.ShareInstanceFromBytes(bytes.Repeat([]byte{1}, catalog.IdentityBytes))
	sessionID, _ := protocolsession.ProtocolSessionIDFromBytes(
		bytes.Repeat([]byte{2}, protocolsession.IdentityBytes),
	)
	protocolOperation, _ := protocolsession.OperationIDFromBytes(
		bytes.Repeat([]byte{3}, protocolsession.IdentityBytes),
	)
	var binding v2signal.Binding
	copy(binding.PeerPathID[:], bytes.Repeat([]byte{4}, v2signal.IdentityBytes))
	copy(binding.AttemptID[:], bytes.Repeat([]byte{5}, v2signal.IdentityBytes))
	session := &integrationPeerSession{
		share: share, sessionID: sessionID, offerer: offerer, binding: binding, operation: protocolOperation,
		answer: make(chan struct{}), admit: make(chan sessionruntime.LaneIdentity, 1),
	}
	factory, err := v2peer.NewFactory(v2peer.Config{
		Configuration: pion.Configuration{}, AttemptTimeout: v2PeerIntegrationTimeout,
		MaxCandidates: v2PeerIntegrationMaxCandidates,
		PeerConnections: v2peer.PeerConnectionFactoryFunc(
			func(configuration pion.Configuration) (v2peer.PeerConnection, error) {
				return senderAPI.NewPeerConnection(configuration)
			},
		),
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
	trace.RequireCleanup(t, "sender peer ingress", func(context.Context) error {
		ingress.Close()
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- handler.Run(ctx) }()
	trace.RequireCleanup(t, "sender peer handler", func(context.Context) error {
		cancel()
		return joinIntegrationRoutine(
			"sender peer handler",
			runDone,
			v2PeerIntegrationTimeout,
			func(err error) bool { return errors.Is(err, context.Canceled) },
		)
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
		select {
		case <-startForwarding:
		case <-ctx.Done():
			forwardDone <- nil
			return
		}
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
					protocolsession.MessagePeerCandidate, &protocolOperation, body,
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
	trace.RequireCleanup(t, "browser candidate forwarder", func(context.Context) error {
		cancel()
		return joinIntegrationRoutine(
			"browser candidate forwarder",
			forwardDone,
			v2PeerIntegrationTimeout,
			func(err error) bool { return err == nil },
		)
	})

	peerReadiness := startIntegrationPhase(
		t,
		trace,
		testrun.PeerReadyMilestone,
		nil,
		peerReadinessEvidenceStartFailureReason,
	)
	offer, err := offerer.CreateOffer(nil)
	if err != nil {
		failIntegrationPhase(t, peerReadiness, senderOfferCreateFailureReason, "create browser offer: %v", err)
	}
	if err := offerer.SetLocalDescription(offer); err != nil {
		failIntegrationPhase(
			t,
			peerReadiness,
			senderOfferSetLocalFailureReason,
			"set browser offer: %v",
			err,
		)
	}
	localOffer := offerer.LocalDescription()
	if localOffer == nil {
		failIntegrationPhase(
			t,
			peerReadiness,
			senderLocalOfferMissingFailureReason,
			"browser PeerConnection omitted local offer",
		)
	}
	offerBody, err := v2signal.EncodeOffer(v2signal.Offer{Binding: binding, SDP: localOffer.SDP})
	if err != nil {
		failIntegrationPhase(t, peerReadiness, senderOfferEncodeFailureReason, "encode browser offer: %v", err)
	}
	offerMessage, err := protocolsession.NewMessage(
		protocolsession.MessagePeerOffer, &protocolOperation, offerBody,
	)
	if err != nil {
		failIntegrationPhase(
			t,
			peerReadiness,
			senderOfferMessageFailureReason,
			"create peer offer message: %v",
			err,
		)
	}
	if err := ingress.Deliver(ctx, offerMessage); err != nil {
		failIntegrationPhase(
			t,
			peerReadiness,
			senderOfferDeliveryFailureReason,
			"deliver peer offer: %v",
			err,
		)
	}
	close(startForwarding)

	select {
	case <-session.answer:
	case <-time.After(v2PeerIntegrationTimeout):
		failIntegrationPhase(
			t,
			peerReadiness,
			senderAnswerTimeoutFailureReason,
			"sender did not return a Pion answer",
		)
	}
	if err := peerReadiness.Succeed(nil); err != nil {
		t.Fatalf("record sender peer readiness: %v", err)
	}
	browserExchangeDone := make(chan struct{})
	var browserExchangeErr error
	go func() {
		defer close(browserExchangeDone)
		select {
		case <-ctx.Done():
			browserExchangeErr = ctx.Err()
			return
		case <-browserChannel.Opened():
		}
		select {
		case <-ctx.Done():
			browserExchangeErr = ctx.Err()
		case frame, ok := <-browserChannel.Recv():
			if !ok || !bytes.Equal(frame, []byte("sender-to-browser")) {
				browserExchangeErr = errors.New("browser received the wrong peer frame")
				return
			}
			browserExchangeErr = browserChannel.Send(ctx, framechannel.Frame("browser-to-sender"))
		}
	}()
	trace.RequireCleanup(t, "browser frame exchange", func(context.Context) error {
		cancel()
		return joinIntegrationSignal(
			"browser frame exchange",
			browserExchangeDone,
			v2PeerIntegrationTimeout,
			func() error {
				if errors.Is(browserExchangeErr, context.Canceled) {
					return nil
				}
				return browserExchangeErr
			},
		)
	})
	// Retire the browser channel before canceling the handler context; otherwise
	// remote teardown can win the close race and hide whether local close worked.
	trace.RequireCleanup(t, "browser frame channel", func(context.Context) error {
		return browserChannel.Close()
	})

	laneAdoption := startIntegrationPhase(
		t,
		trace,
		testrun.LaneAdoptedMilestone,
		nil,
		laneAdoptionEvidenceStartFailureReason,
	)
	var admittedLane sessionruntime.LaneIdentity
	select {
	case admittedLane = <-session.admit:
		if admittedLane.ID != 17 || admittedLane.Epoch != 3 {
			failIntegrationPhase(
				t,
				laneAdoption,
				senderLaneIdentityFailureReason,
				"admitted lane = %#v",
				admittedLane,
			)
		}
	case <-time.After(v2PeerIntegrationTimeout):
		failIntegrationPhase(
			t,
			laneAdoption,
			senderAdmissionTimeoutFailureReason,
			"real Pion DataChannel was not admitted",
		)
	}
	if err := laneAdoption.Succeed(integrationLaneContext{
		LaneID: admittedLane.ID, LaneEpoch: admittedLane.Epoch,
	}); err != nil {
		t.Fatalf("record sender lane adoption: %v", err)
	}
	select {
	case <-browserExchangeDone:
		if browserExchangeErr != nil {
			t.Fatalf("peer FrameChannel exchange: %v", browserExchangeErr)
		}
	case <-time.After(v2PeerIntegrationTimeout):
		t.Fatal("browser frame exchange did not stop")
	}
	session.mu.Lock()
	failure := session.failure
	session.mu.Unlock()
	if failure != nil {
		t.Fatalf("sender reported peer failure: %v", failure)
	}

	if err := ingress.Cancel(ctx, protocolOperation); err != nil {
		t.Fatalf("cancel signaling operation: %v", err)
	}
	trace.RequireSuccess(t)
}
