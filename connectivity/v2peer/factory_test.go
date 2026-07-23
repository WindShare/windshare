package v2peer

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/catalog"
	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

const peerTestTimeout = 2 * time.Second

type capturedControl struct {
	kind      protocolsession.MessageKind
	operation protocolsession.OperationID
	body      []byte
}

type capturedFailure struct {
	operation protocolsession.OperationID
	code      uint16
	message   string
}

type testPeerSession struct {
	share      catalog.ShareInstance
	sessionID  protocolsession.ProtocolSessionID
	controls   chan capturedControl
	failures   chan capturedFailure
	admissions chan protocolsession.FrameChannel
	lane       sessionruntime.LaneIdentity
	admitErr   error
}

func newTestPeerSession(seed byte) *testPeerSession {
	share, _ := catalog.ShareInstanceFromBytes(bytes.Repeat([]byte{seed}, catalog.IdentityBytes))
	sessionID, _ := protocolsession.ProtocolSessionIDFromBytes(
		bytes.Repeat([]byte{seed + 1}, protocolsession.IdentityBytes),
	)
	return &testPeerSession{
		share: share, sessionID: sessionID,
		controls: make(chan capturedControl, 16), failures: make(chan capturedFailure, 16),
		admissions: make(chan protocolsession.FrameChannel, 4),
		lane:       sessionruntime.LaneIdentity{ID: 9, Epoch: 2},
	}
}

func TestPeerFactoriesRejectCandidateLimitAboveProtocol(t *testing.T) {
	invalid := v2signal.MaximumCandidates + 1
	if _, err := NewFactory(Config{MaxCandidates: invalid}); !errors.Is(err, ErrConfig) {
		t.Fatalf("sender candidate limit error = %v", err)
	}
	if _, err := NewReceiverFactory(ReceiverFactoryConfig{MaxCandidates: invalid}); !errors.Is(err, ErrConfig) {
		t.Fatalf("receiver candidate limit error = %v", err)
	}
}

func (session *testPeerSession) ShareInstance() catalog.ShareInstance { return session.share }
func (session *testPeerSession) ProtocolSessionID() protocolsession.ProtocolSessionID {
	return session.sessionID
}
func (session *testPeerSession) SendPeerControl(
	_ context.Context,
	kind protocolsession.MessageKind,
	operation protocolsession.OperationID,
	body []byte,
) (protocolsession.OperationDisposition, error) {
	session.controls <- capturedControl{kind: kind, operation: operation, body: bytes.Clone(body)}
	return protocolsession.OperationDeliver, nil
}
func (session *testPeerSession) FailPeerOperation(
	_ context.Context,
	operation protocolsession.OperationID,
	code uint16,
	message string,
) error {
	session.failures <- capturedFailure{operation: operation, code: code, message: message}
	return nil
}
func (session *testPeerSession) AdmitPeerChannel(
	_ context.Context,
	channel protocolsession.FrameChannel,
) (sessionruntime.LaneIdentity, error) {
	session.admissions <- channel
	return session.lane, session.admitErr
}

type testPeerConnection struct {
	mu          sync.Mutex
	onCandidate func(*pion.ICECandidate)
	onState     func(pion.PeerConnectionState)
	onData      func(*pion.DataChannel)
	remote      chan pion.SessionDescription
	added       chan pion.ICECandidateInit
	closed      chan struct{}
	closeOnce   sync.Once
	answer      pion.SessionDescription
	local       *pion.SessionDescription
	localSDP    string
}

func newTestPeerConnection() *testPeerConnection {
	return &testPeerConnection{
		remote: make(chan pion.SessionDescription, 2), added: make(chan pion.ICECandidateInit, 8),
		closed: make(chan struct{}),
		answer: pion.SessionDescription{Type: pion.SDPTypeAnswer, SDP: "v=0\r\ns=windshare-test\r\n"},
	}
}

func (peer *testPeerConnection) OnICECandidate(callback func(*pion.ICECandidate)) {
	peer.mu.Lock()
	peer.onCandidate = callback
	peer.mu.Unlock()
}
func (peer *testPeerConnection) OnConnectionStateChange(callback func(pion.PeerConnectionState)) {
	peer.mu.Lock()
	peer.onState = callback
	peer.mu.Unlock()
}
func (peer *testPeerConnection) OnDataChannel(callback func(*pion.DataChannel)) {
	peer.mu.Lock()
	peer.onData = callback
	peer.mu.Unlock()
}
func (peer *testPeerConnection) SetRemoteDescription(description pion.SessionDescription) error {
	peer.remote <- description
	return nil
}
func (peer *testPeerConnection) CreateAnswer(*pion.AnswerOptions) (pion.SessionDescription, error) {
	return peer.answer, nil
}
func (peer *testPeerConnection) SetLocalDescription(description pion.SessionDescription) error {
	peer.mu.Lock()
	if peer.localSDP != "" {
		description.SDP = peer.localSDP
	}
	peer.local = &description
	peer.mu.Unlock()
	return nil
}
func (peer *testPeerConnection) LocalDescription() *pion.SessionDescription {
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if peer.local == nil {
		return nil
	}
	copy := *peer.local
	return &copy
}
func (peer *testPeerConnection) AddICECandidate(candidate pion.ICECandidateInit) error {
	peer.added <- candidate
	return nil
}
func (peer *testPeerConnection) Close() error {
	peer.closeOnce.Do(func() { close(peer.closed) })
	return nil
}
func (peer *testPeerConnection) emitCandidate(candidate *pion.ICECandidate) {
	peer.mu.Lock()
	callback := peer.onCandidate
	peer.mu.Unlock()
	callback(candidate)
}
func (peer *testPeerConnection) emitDataChannel(channel *pion.DataChannel) {
	peer.mu.Lock()
	callback := peer.onData
	peer.mu.Unlock()
	callback(channel)
}

type testPeerChannel struct {
	receive   chan framechannel.Frame
	opened    chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	state     atomic.Uint32
}

func newTestPeerChannel() *testPeerChannel {
	opened := make(chan struct{})
	close(opened)
	channel := &testPeerChannel{receive: make(chan framechannel.Frame), opened: opened, done: make(chan struct{})}
	channel.state.Store(uint32(framechannel.Open))
	return channel
}
func (*testPeerChannel) Send(context.Context, framechannel.Frame) error         { return nil }
func (*testPeerChannel) SendTerminal(context.Context, framechannel.Frame) error { return nil }
func (channel *testPeerChannel) Recv() <-chan framechannel.Frame                { return channel.receive }
func (channel *testPeerChannel) State() framechannel.ChannelState {
	return framechannel.ChannelState(channel.state.Load())
}
func (channel *testPeerChannel) Close() error {
	channel.closeOnce.Do(func() {
		channel.state.Store(uint32(framechannel.Closed))
		close(channel.done)
		close(channel.receive)
	})
	return nil
}
func (channel *testPeerChannel) Done() <-chan struct{}   { return channel.done }
func (channel *testPeerChannel) Opened() <-chan struct{} { return channel.opened }
func (*testPeerChannel) Err() error                      { return nil }

func TestSenderHandlerAnswersCandidatesAndAdmitsPeerChannel(t *testing.T) {
	peer := newTestPeerConnection()
	peer.localSDP = "v=0\r\ns=authoritative-local-answer\r\n"
	channel := newTestPeerChannel()
	factory := mustTestFactory(t, Config{
		PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) {
			return peer, nil
		}),
		DataChannels: DataChannelAdapterFunc(func(raw *pion.DataChannel) (PeerDataChannel, error) {
			if raw == nil {
				t.Fatal("adapter received nil DataChannel")
			}
			return channel, nil
		}),
	})
	session := newTestPeerSession(1)
	interfaceHandler, err := factory.NewSenderPeerHandler(session)
	if err != nil {
		t.Fatalf("create handler: %v", err)
	}
	handler := interfaceHandler.(*senderHandler)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- handler.Run(ctx) }()

	operation := testOperationID(20)
	binding := testBinding(40)
	offerBody, _ := v2signal.EncodeOffer(v2signal.Offer{Binding: binding, SDP: "v=0\r\ns=receiver\r\n"})
	offerMessage := testMessage(t, protocolsession.MessagePeerOffer, operation, offerBody)
	operationContext := testPeerMessageContext(t, ctx, offerMessage)
	operationKey := testPeerOperationFromContext(t, operationContext, operation)
	if err := handler.HandleMessage(operationContext, offerMessage); err != nil {
		t.Fatalf("offer: %v", err)
	}
	remote := receiveTest(t, peer.remote)
	if remote.Type != pion.SDPTypeOffer || remote.SDP != "v=0\r\ns=receiver\r\n" {
		t.Fatalf("remote offer = %#v", remote)
	}
	answerControl := receiveTest(t, session.controls)
	answer, err := v2signal.DecodeAnswer(answerControl.body)
	if err != nil || answerControl.kind != protocolsession.MessagePeerAnswer ||
		answerControl.operation != operation || answer.Binding != binding || answer.SDP != peer.localSDP {
		t.Fatalf("answer control = %#v, answer=%#v, err=%v", answerControl, answer, err)
	}

	remoteCandidate := v2signal.Candidate{Binding: binding, Candidate: "candidate:receiver"}
	remoteBody, _ := v2signal.EncodeCandidate(remoteCandidate)
	if err := handler.HandleMessage(
		operationContext,
		testMessage(t, protocolsession.MessagePeerCandidate, operation, remoteBody),
	); err != nil {
		t.Fatalf("remote candidate: %v", err)
	}
	if added := receiveTest(t, peer.added); added.Candidate != remoteCandidate.Candidate {
		t.Fatalf("added candidate = %#v", added)
	}

	peer.emitCandidate(&pion.ICECandidate{
		Foundation: "1", Priority: 1, Address: "127.0.0.1", Protocol: pion.ICEProtocolUDP,
		Port: 43210, Typ: pion.ICECandidateTypeHost, Component: 1,
	})
	candidateControl := receiveTest(t, session.controls)
	candidate, err := v2signal.DecodeCandidate(candidateControl.body)
	if err != nil || candidateControl.kind != protocolsession.MessagePeerCandidate ||
		candidateControl.operation != operation || candidate.Binding != binding || candidate.Candidate == "" {
		t.Fatalf("candidate control = %#v, candidate=%#v, err=%v", candidateControl, candidate, err)
	}

	peer.emitDataChannel(&pion.DataChannel{})
	if admitted := receiveTest(t, session.admissions); admitted != channel {
		t.Fatalf("admitted channel = %T, want injected channel", admitted)
	}
	if err := channel.Close(); err != nil {
		t.Fatalf("close peer channel: %v", err)
	}
	closedFailure := receiveTest(t, session.failures)
	if closedFailure.operation != operation || closedFailure.code != protocolsession.PeerOperationCodeAdmission {
		t.Fatalf("uncanceled channel close failure = %#v", closedFailure)
	}
	receiveTest(t, peer.closed)
	waitForTest(t, func() bool {
		handler.mu.Lock()
		defer handler.mu.Unlock()
		_, retired := handler.retiredOperations[operationKey]
		return retired
	})

	replayBody, _ := v2signal.EncodeOffer(v2signal.Offer{Binding: binding, SDP: "v=0\r\ns=replay\r\n"})
	replayOperation := testOperationID(21)
	replayMessage := testMessage(t, protocolsession.MessagePeerOffer, replayOperation, replayBody)
	if err := handler.HandleMessage(
		testPeerMessageContext(t, ctx, replayMessage), replayMessage,
	); err != nil {
		t.Fatalf("enqueue replay offer: %v", err)
	}
	replayFailure := receiveTest(t, session.failures)
	if replayFailure.operation != testOperationID(21) ||
		replayFailure.code != protocolsession.PeerOperationCodeNegotiation {
		t.Fatalf("fresh-operation binding replay failure = %#v", replayFailure)
	}
	cancel()
	if err := receiveTest(t, runDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("handler stop after isolated replay = %v", err)
	}
}

func TestSenderHandlerRetainsBindingAcrossCompletionAndCancelUntilExpiry(t *testing.T) {
	now := time.Unix(1_000, 0)
	factory := mustTestFactory(t, Config{
		Now: func() time.Time { return now }, RetiredBindingTTL: time.Minute,
		MaxActiveAttempts: 1, MaxRetiredBindings: 1,
		PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) {
			return nil, errors.New("stop test attempt")
		}),
	})
	handler := newDirectTestHandler(t, factory, newTestPeerSession(3))
	operation := testOperationID(30)
	binding := testBinding(60)
	attempt := newPeerAttempt(peerAttemptConfig{
		factory: factory, session: handler.session, operation: operation,
		offer: v2signal.Offer{Binding: binding, SDP: "v=0\r\n"}, onDone: handler.attemptDone,
	})
	handler.attempts[testPeerOperation(operation)] = attempt
	handler.bindings[binding] = testPeerOperation(operation)
	handler.attemptDone(attempt, context.Canceled)

	newOperation := testOperationID(31)
	if err := handler.startAttempt(
		context.Background(), testPeerOperation(newOperation), v2signal.Offer{Binding: binding, SDP: "v=0\r\n"},
	); !errors.Is(err, v2signal.ErrSignalBinding) {
		t.Fatalf("canceled binding replay = %v", err)
	}
	blockedBinding := testBinding(61)
	if err := handler.startAttempt(
		context.Background(), testPeerOperation(newOperation), v2signal.Offer{Binding: blockedBinding, SDP: "v=0\r\n"},
	); !errors.Is(err, ErrReplayCapacity) {
		t.Fatalf("reserved replay budget = %v", err)
	}
	now = now.Add(30 * time.Second)
	handler.retireRejectedOffer(testPeerOperation(newOperation), blockedBinding)

	// Exhaustion fails closed for a full retention window measured from the
	// rejected offer, even after the older tombstone itself expires.
	now = now.Add(30 * time.Second)
	if err := handler.startAttempt(
		context.Background(), testPeerOperation(newOperation), v2signal.Offer{Binding: binding, SDP: "v=0\r\n"},
	); !errors.Is(err, ErrReplayCapacity) {
		t.Fatalf("replay exhaustion did not fail closed: %v", err)
	}
	now = now.Add(30 * time.Second)
	if err := handler.startAttempt(
		context.Background(), testPeerOperation(newOperation), v2signal.Offer{Binding: binding, SDP: "v=0\r\n"},
	); err != nil {
		t.Fatalf("binding reuse after fail-closed expiry: %v", err)
	}
	handler.work.Wait()

	// A new ProtocolSession receives an isolated handler and may reuse an old
	// path/attempt pair because the authenticated session authority changed.
	reconnected := newDirectTestHandler(t, factory, newTestPeerSession(9))
	if err := reconnected.startAttempt(
		context.Background(), testPeerOperation(testOperationID(32)), v2signal.Offer{Binding: binding, SDP: "v=0\r\n"},
	); err != nil {
		t.Fatalf("binding reuse after reconnect: %v", err)
	}
	reconnected.work.Wait()
}

func TestSenderHandlerCapacityFailureEndsOnlyRejectedOperation(t *testing.T) {
	peer := newTestPeerConnection()
	factory := mustTestFactory(t, Config{
		MaxActiveAttempts: 1,
		PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) {
			return peer, nil
		}),
	})
	session := newTestPeerSession(17)
	interfaceHandler, _ := factory.NewSenderPeerHandler(session)
	handler := interfaceHandler.(*senderHandler)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- handler.Run(ctx) }()

	firstOperation := testOperationID(120)
	firstBinding := testBinding(121)
	firstBody, _ := v2signal.EncodeOffer(v2signal.Offer{Binding: firstBinding, SDP: "v=0\r\n"})
	firstMessage := testMessage(t, protocolsession.MessagePeerOffer, firstOperation, firstBody)
	firstContext := testPeerMessageContext(t, ctx, firstMessage)
	firstKey := testPeerOperationFromContext(t, firstContext, firstOperation)
	if err := handler.HandleMessage(
		firstContext, firstMessage,
	); err != nil {
		t.Fatalf("first offer: %v", err)
	}
	firstAnswer := receiveTest(t, session.controls)
	if firstAnswer.operation != firstOperation {
		t.Fatalf("first answer operation = %x", firstAnswer.operation)
	}

	rejectedOperation := testOperationID(122)
	rejectedBody, _ := v2signal.EncodeOffer(v2signal.Offer{
		Binding: testBinding(123), SDP: "v=0\r\n",
	})
	rejectedMessage := testMessage(t, protocolsession.MessagePeerOffer, rejectedOperation, rejectedBody)
	if err := handler.HandleMessage(
		testPeerMessageContext(t, ctx, rejectedMessage), rejectedMessage,
	); err != nil {
		t.Fatalf("capacity offer: %v", err)
	}
	rejected := receiveTest(t, session.failures)
	if rejected.operation != rejectedOperation || rejected.code != protocolsession.PeerOperationCodeNegotiation {
		t.Fatalf("capacity failure = %#v", rejected)
	}
	select {
	case err := <-runDone:
		t.Fatalf("capacity failure stopped handler: %v", err)
	default:
	}

	if err := handler.Cancel(firstContext, firstOperation); err != nil {
		t.Fatalf("cancel first attempt: %v", err)
	}
	waitForTest(t, func() bool {
		handler.mu.Lock()
		defer handler.mu.Unlock()
		return handler.attempts[firstKey] == nil
	})
	freshOperation := testOperationID(124)
	freshBinding := testBinding(125)
	freshBody, _ := v2signal.EncodeOffer(v2signal.Offer{Binding: freshBinding, SDP: "v=0\r\n"})
	freshMessage := testMessage(t, protocolsession.MessagePeerOffer, freshOperation, freshBody)
	freshContext := testPeerMessageContext(t, ctx, freshMessage)
	if err := handler.HandleMessage(
		freshContext, freshMessage,
	); err != nil {
		t.Fatalf("fresh offer after isolated capacity failure: %v", err)
	}
	freshAnswerControl := receiveTest(t, session.controls)
	freshAnswer, err := v2signal.DecodeAnswer(freshAnswerControl.body)
	if err != nil || freshAnswerControl.operation != freshOperation || freshAnswer.Binding != freshBinding {
		t.Fatalf("fresh answer after capacity failure = %#v/%#v, %v", freshAnswerControl, freshAnswer, err)
	}
	if err := handler.Cancel(freshContext, freshOperation); err != nil {
		t.Fatalf("cancel fresh attempt: %v", err)
	}
	cancel()
	if err := receiveTest(t, runDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("handler stop = %v", err)
	}
}

func TestSenderHandlerQueueOverflowTombstonesRejectedOfferBinding(t *testing.T) {
	factory := mustTestFactory(t, Config{
		PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) {
			return nil, errors.New("unexpected attempt")
		}),
	})
	session := newTestPeerSession(19)
	handler := newDirectTestHandler(t, factory, session)
	for len(handler.events) < cap(handler.events) {
		handler.events <- handlerEvent{kind: handlerReject, operation: testPeerOperation(testOperationID(byte(len(handler.events) + 1)))}
	}
	operation := testOperationID(130)
	binding := testBinding(131)
	body, _ := v2signal.EncodeOffer(v2signal.Offer{Binding: binding, SDP: "v=0\r\n"})
	overflowMessage := testMessage(t, protocolsession.MessagePeerOffer, operation, body)
	if err := handler.HandleMessage(
		testPeerMessageContext(t, context.Background(), overflowMessage), overflowMessage,
	); err != nil {
		t.Fatalf("overflowed offer: %v", err)
	}
	failure := receiveTest(t, session.failures)
	if failure.operation != operation || failure.code != protocolsession.PeerOperationCodeNegotiation {
		t.Fatalf("overflow failure = %#v", failure)
	}
	if err := handler.startAttempt(
		context.Background(), testPeerOperation(testOperationID(132)), v2signal.Offer{Binding: binding, SDP: "v=0\r\n"},
	); !errors.Is(err, v2signal.ErrSignalBinding) {
		t.Fatalf("overflowed offer binding replay = %v", err)
	}
}

func TestSenderHandlerSuppressesOfferCanceledBeforePublication(t *testing.T) {
	factory := mustTestFactory(t, Config{})
	handler := newDirectTestHandler(t, factory, newTestPeerSession(27))
	operation := testOperationID(140)
	body, _ := v2signal.EncodeOffer(v2signal.Offer{
		Binding: testBinding(141), SDP: "v=0\r\n",
	})
	message := testMessage(t, protocolsession.MessagePeerOffer, operation, body)
	operations, _ := protocolsession.NewOperationTable(
		protocolsession.OperationLimits{MaxActive: 2, MaxTombstones: 2}, nil,
	)
	admission, err := operations.ObserveInbound(protocolsession.DirectionReceiverToSender, message)
	if err != nil {
		t.Fatal(err)
	}
	cancel, _ := protocolsession.NewMessage(protocolsession.MessageCancel, &operation, []byte{1})
	if _, err := operations.ObserveInbound(protocolsession.DirectionReceiverToSender, cancel); err != nil {
		t.Fatal(err)
	}
	messageContext := protocolsession.WithOperationGeneration(context.Background(), admission.Generation)
	if err := handler.HandleMessage(messageContext, message); err != nil {
		t.Fatalf("stale offer suppression: %v", err)
	}
	if len(handler.events) != 0 {
		t.Fatal("canceled peer offer reached the negotiation queue")
	}

	sibling := testOperationID(142)
	siblingMessage := testMessage(t, protocolsession.MessagePeerOffer, sibling, body)
	if err := handler.HandleMessage(
		testPeerMessageContext(t, context.Background(), siblingMessage), siblingMessage,
	); err != nil {
		t.Fatalf("sibling offer: %v", err)
	}
	if len(handler.events) != 1 {
		t.Fatal("canceled generation suppressed an unrelated peer offer")
	}
	handler.stopAll()
}

func TestSenderHandlerReportsPermanentPeerFailuresAndClosesHostileNilChannel(t *testing.T) {
	peer := newTestPeerConnection()
	factory := mustTestFactory(t, Config{
		PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) {
			return peer, nil
		}),
		DataChannels: DataChannelAdapterFunc(func(*pion.DataChannel) (PeerDataChannel, error) {
			t.Fatal("nil DataChannel reached adapter")
			return nil, nil
		}),
	})
	session := newTestPeerSession(5)
	interfaceHandler, _ := factory.NewSenderPeerHandler(session)
	handler := interfaceHandler.(*senderHandler)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- handler.Run(ctx) }()
	operation := testOperationID(70)
	body, _ := v2signal.EncodeOffer(v2signal.Offer{Binding: testBinding(80), SDP: "v=0\r\n"})
	message := testMessage(t, protocolsession.MessagePeerOffer, operation, body)
	if err := handler.HandleMessage(testPeerMessageContext(t, ctx, message), message); err != nil {
		t.Fatalf("offer: %v", err)
	}
	receiveTest(t, session.controls)
	peer.emitDataChannel(nil)
	failure := receiveTest(t, session.failures)
	if failure.operation != operation || failure.code != protocolsession.PeerOperationCodeAdmission ||
		failure.message != "Peer channel admission failed" {
		t.Fatalf("peer failure = %#v", failure)
	}
	receiveTest(t, peer.closed)
	cancel()
	if err := receiveTest(t, runDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("handler stop = %v", err)
	}
}

func TestSenderHandlerBoundsTimeoutAndCandidateFlood(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*Config)
		stimulate func(*testing.T, *testPeerConnection, *testPeerSession)
		wantCode  uint16
	}{
		{
			name:      "timeout",
			configure: func(config *Config) { config.AttemptTimeout = 20 * time.Millisecond },
			stimulate: func(*testing.T, *testPeerConnection, *testPeerSession) {},
			wantCode:  protocolsession.PeerOperationCodeTimeout,
		},
		{
			name:      "candidate flood",
			configure: func(config *Config) { config.MaxCandidates = 1 },
			stimulate: func(t *testing.T, peer *testPeerConnection, session *testPeerSession) {
				for port := uint16(43000); port < 43002; port++ {
					peer.emitCandidate(&pion.ICECandidate{
						Foundation: "1", Priority: 1, Address: "127.0.0.1", Protocol: pion.ICEProtocolUDP,
						Port: port, Typ: pion.ICECandidateTypeHost, Component: 1,
					})
				}
				control := receiveTest(t, session.controls)
				if control.kind != protocolsession.MessagePeerCandidate {
					t.Fatalf("first bounded candidate kind = %d", control.kind)
				}
			},
			wantCode: protocolsession.PeerOperationCodeCandidates,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			peer := newTestPeerConnection()
			config := Config{
				PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) {
					return peer, nil
				}),
			}
			test.configure(&config)
			factory := mustTestFactory(t, config)
			session := newTestPeerSession(11)
			interfaceHandler, _ := factory.NewSenderPeerHandler(session)
			handler := interfaceHandler.(*senderHandler)
			ctx, cancel := context.WithCancel(context.Background())
			runDone := make(chan error, 1)
			go func() { runDone <- handler.Run(ctx) }()
			operation := testOperationID(90)
			body, _ := v2signal.EncodeOffer(v2signal.Offer{
				Binding: testBinding(91), SDP: "v=0\r\n",
			})
			message := testMessage(t, protocolsession.MessagePeerOffer, operation, body)
			if err := handler.HandleMessage(
				testPeerMessageContext(t, ctx, message), message,
			); err != nil {
				t.Fatalf("offer: %v", err)
			}
			receiveTest(t, session.controls)
			test.stimulate(t, peer, session)
			failure := receiveTest(t, session.failures)
			if failure.operation != operation || failure.code != test.wantCode {
				t.Fatalf("bounded failure = %#v", failure)
			}
			receiveTest(t, peer.closed)
			cancel()
			if err := receiveTest(t, runDone); !errors.Is(err, context.Canceled) {
				t.Fatalf("handler stop = %v", err)
			}
		})
	}
}

func TestSenderHandlerCancellationKeepsAnAdmittedLaneUntilPhysicalClose(t *testing.T) {
	peer := newTestPeerConnection()
	channel := newTestPeerChannel()
	factory := mustTestFactory(t, Config{
		PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) {
			return peer, nil
		}),
		DataChannels: DataChannelAdapterFunc(func(*pion.DataChannel) (PeerDataChannel, error) {
			return channel, nil
		}),
	})
	session := newTestPeerSession(13)
	interfaceHandler, _ := factory.NewSenderPeerHandler(session)
	handler := interfaceHandler.(*senderHandler)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- handler.Run(ctx) }()
	operation := testOperationID(100)
	body, _ := v2signal.EncodeOffer(v2signal.Offer{Binding: testBinding(101), SDP: "v=0\r\n"})
	message := testMessage(t, protocolsession.MessagePeerOffer, operation, body)
	operationContext := testPeerMessageContext(t, ctx, message)
	operationKey := testPeerOperationFromContext(t, operationContext, operation)
	if err := handler.HandleMessage(
		operationContext, message,
	); err != nil {
		t.Fatalf("offer: %v", err)
	}
	receiveTest(t, session.controls)
	peer.emitDataChannel(&pion.DataChannel{})
	receiveTest(t, session.admissions)
	var attempt *peerAttempt
	waitForTest(t, func() bool {
		handler.mu.Lock()
		attempt = handler.attempts[operationKey]
		handler.mu.Unlock()
		return attempt != nil && attempt.attached.Load()
	})
	if err := handler.Cancel(operationContext, operation); err != nil {
		t.Fatalf("cancel attached operation: %v", err)
	}
	select {
	case <-peer.closed:
		t.Fatal("operation cancellation closed an admitted PeerConnection")
	default:
	}
	select {
	case <-channel.done:
		t.Fatal("operation cancellation closed an admitted DataChannel")
	default:
	}
	if err := channel.Close(); err != nil {
		t.Fatal(err)
	}
	receiveTest(t, peer.closed)
	select {
	case failure := <-session.failures:
		t.Fatalf("canceled peer operation emitted failure %#v", failure)
	default:
	}
	cancel()
	if err := receiveTest(t, runDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("handler stop = %v", err)
	}
}

func TestSenderHandlerStopClosesUnattachedPeerWithoutOperationFailure(t *testing.T) {
	peer := newTestPeerConnection()
	factory := mustTestFactory(t, Config{
		PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) {
			return peer, nil
		}),
	})
	session := newTestPeerSession(15)
	interfaceHandler, _ := factory.NewSenderPeerHandler(session)
	handler := interfaceHandler.(*senderHandler)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- handler.Run(ctx) }()
	body, _ := v2signal.EncodeOffer(v2signal.Offer{Binding: testBinding(111), SDP: "v=0\r\n"})
	operation := testOperationID(110)
	message := testMessage(t, protocolsession.MessagePeerOffer, operation, body)
	if err := handler.HandleMessage(
		testPeerMessageContext(t, ctx, message), message,
	); err != nil {
		t.Fatalf("offer: %v", err)
	}
	receiveTest(t, session.controls)
	cancel()
	if err := receiveTest(t, runDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("handler stop = %v", err)
	}
	receiveTest(t, peer.closed)
	select {
	case failure := <-session.failures:
		t.Fatalf("session stop emitted peer operation failure %#v", failure)
	default:
	}
}

func mustTestFactory(t *testing.T, config Config) *Factory {
	t.Helper()
	factory, err := NewFactory(config)
	if err != nil {
		t.Fatalf("create factory: %v", err)
	}
	return factory
}

func newDirectTestHandler(t *testing.T, factory *Factory, session *testPeerSession) *senderHandler {
	t.Helper()
	handler, err := factory.NewSenderPeerHandler(session)
	if err != nil {
		t.Fatalf("create handler: %v", err)
	}
	return handler.(*senderHandler)
}

func testBinding(seed byte) v2signal.Binding {
	var binding v2signal.Binding
	copy(binding.PeerPathID[:], bytes.Repeat([]byte{seed}, v2signal.IdentityBytes))
	copy(binding.AttemptID[:], bytes.Repeat([]byte{seed + 1}, v2signal.IdentityBytes))
	return binding
}

func testOperationID(seed byte) protocolsession.OperationID {
	operation, _ := protocolsession.OperationIDFromBytes(
		bytes.Repeat([]byte{seed}, protocolsession.IdentityBytes),
	)
	return operation
}

func testPeerOperation(operation protocolsession.OperationID) peerOperation {
	return peerOperation{id: operation}
}

func testPeerMessageContext(
	t *testing.T,
	parent context.Context,
	message protocolsession.Message,
) context.Context {
	t.Helper()
	operations, err := protocolsession.NewOperationTable(
		protocolsession.OperationLimits{MaxActive: 4, MaxTombstones: 4}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := operations.ObserveInbound(protocolsession.DirectionReceiverToSender, message)
	if err != nil || admission.Disposition != protocolsession.OperationDeliver {
		t.Fatalf("admit peer message: disposition=%d error=%v", admission.Disposition, err)
	}
	return protocolsession.WithOperationGeneration(parent, admission.Generation)
}

func testPeerOperationFromContext(
	t *testing.T,
	ctx context.Context,
	operation protocolsession.OperationID,
) peerOperation {
	t.Helper()
	generation, ok := protocolsession.OperationGenerationFromContext(ctx, operation)
	if !ok {
		t.Fatal("peer test context lost operation generation")
	}
	return peerOperation{id: operation, generation: generation}
}

func testMessage(
	t *testing.T,
	kind protocolsession.MessageKind,
	operation protocolsession.OperationID,
	body []byte,
) protocolsession.Message {
	t.Helper()
	message, err := protocolsession.NewMessage(kind, &operation, body)
	if err != nil {
		t.Fatalf("create message: %v", err)
	}
	return message
}

func receiveTest[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(peerTestTimeout):
		t.Fatal("timed out waiting for peer event")
		var zero T
		return zero
	}
}

func waitForTest(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(peerTestTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("timed out waiting for peer state")
		case <-ticker.C:
		}
	}
}
