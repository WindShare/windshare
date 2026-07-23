package sessionruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestAmbiguousPeerOfferReplayPrecedesCandidateAcrossLaneMigration(t *testing.T) {
	for _, deliveredBeforeError := range []bool{false, true} {
		t.Run(fmt.Sprintf("delivered-before-error-%t", deliveredBeforeError), func(t *testing.T) {
			fixture := newVerticalFixture(t)
			handlers := make(chan *peerReplayHandler, 4)
			fixture.senderFactory.peers = peerReplayHandlerFactory(handlers, nil)

			initialSender, initialReceiverBase := newObservedChannelPair()
			initialReceiver := &ambiguousObservedChannel{observedMemoryChannel: initialReceiverBase}
			sender, receiver := connectPeerReplayPair(
				t, fixture.senderFactory, fixture.receiverFactory, initialSender, initialReceiver,
			)
			defer sender.Close()
			defer receiver.Close()
			handler := <-handlers

			secondary, _, secondaryReceiver, _ := attachObservedLane(
				t, fixture.senderFactory, receiver, 0,
			)
			initialLane, err := receiver.lanes.selectLane(&receiver.initial)
			if err != nil {
				t.Fatal(err)
			}
			secondarySendsBefore := secondaryReceiver.sends.Load()
			initialReceiver.failNextSend(
				deliveredBeforeError, errors.New("peer offer transport acceptance is ambiguous"),
			)

			offer := peerReplayBody(t, "offer-with-ambiguous-transport-result")
			call, err := receiver.rpc.beginOn(
				context.Background(), &receiver.initial, protocolsession.MessagePeerOffer, offer,
			)
			if err != nil {
				t.Fatalf("ambiguous peer offer was rejected locally: %v", err)
			}
			operation := &ReceiverPeerOperation{
				rpc: receiver.rpc, call: call, token: new(receiverPeerOperationToken),
			}
			operationID := operation.OperationID()
			if operationID.IsZero() {
				t.Fatal("ambiguous peer offer retained no operation authority")
			}
			if deliveredBeforeError {
				assertPeerReplayEvent(t, handler.events, protocolsession.MessagePeerOffer, operationID)
			}
			select {
			case <-initialLane.done:
			case <-time.After(time.Second):
				t.Fatal("ambiguous initial lane did not drain")
			}

			candidate := peerReplayBody(t, "candidate-after-offer")
			if _, err := operation.SendCandidate(context.Background(), candidate); err != nil {
				t.Fatalf("candidate migration: %v", err)
			}
			if !deliveredBeforeError {
				assertPeerReplayEvent(t, handler.events, protocolsession.MessagePeerOffer, operationID)
			}
			assertPeerReplayEvent(t, handler.events, protocolsession.MessagePeerCandidate, operationID)
			if got := secondaryReceiver.sends.Load() - secondarySendsBefore; got != 2 {
				t.Fatalf("replacement lane sent %d frames; want exact offer replay then candidate", got)
			}
			select {
			case event := <-handler.events:
				t.Fatalf("exact offer replay redispatched handler event kind=%d operation=%x", event.kind, event.operationID)
			default:
			}

			route := sender.routes.current(operationID)
			if route == nil {
				t.Fatal("sender lost the active peer route")
			}
			wantRoute := secondary
			if deliveredBeforeError {
				wantRoute = sender.initial
			}
			if route.preferred != wantRoute {
				t.Fatalf("sender route=%+v, want first physical route %+v", route.preferred, wantRoute)
			}
			if receiver.Err() != nil || sender.Err() != nil {
				t.Fatalf("request replay escaped its operation receiver=%v sender=%v", receiver.Err(), sender.Err())
			}

			assertReceiverPeerTermination(
				t,
				operation,
				operation.Terminate(context.Background()),
				ReceiverPeerTerminalAuthorityLocal,
				ReceiverPeerProvenanceLocalExplicitStop,
				ReceiverPeerTerminalOperationOnly,
				ReceiverPeerProvenanceLocalExplicitStop,
			)
			assertPeerReplayCancel(t, handler.cancels, operationID)
			waitSessionCondition(t, "peer replay operation drain", func() bool {
				return sender.routes.len() == 0 && sender.operations.ActiveCount() == 0
			})
			assertSiblingSessionHealthy(t, fixture, handlers)
		})
	}
}

func TestAmbiguousPeerOfferReplaySurvivesRepeatedLaneFailure(t *testing.T) {
	fixture := newVerticalFixture(t)
	handlers := make(chan *peerReplayHandler, 4)
	fixture.senderFactory.peers = peerReplayHandlerFactory(handlers, nil)

	initialSender, initialReceiverBase := newObservedChannelPair()
	initialReceiver := &ambiguousObservedChannel{observedMemoryChannel: initialReceiverBase}
	sender, receiver := connectPeerReplayPair(
		t, fixture.senderFactory, fixture.receiverFactory, initialSender, initialReceiver,
	)
	defer sender.Close()
	defer receiver.Close()
	handler := <-handlers

	failingIdentity, failingReceiver := attachAmbiguousPeerReplayLane(
		t, fixture.senderFactory, receiver,
	)
	healthyIdentity, _, healthyReceiver, _ := attachObservedLane(
		t, fixture.senderFactory, receiver, 0,
	)
	initialLane, err := receiver.lanes.selectLane(&receiver.initial)
	if err != nil {
		t.Fatal(err)
	}
	initialReceiver.failNextSend(false, errors.New("initial peer offer was not delivered"))
	offer := peerReplayBody(t, "offer-across-two-ambiguous-lanes")
	call, err := receiver.rpc.beginOn(
		context.Background(), &receiver.initial, protocolsession.MessagePeerOffer, offer,
	)
	if err != nil {
		t.Fatalf("ambiguous peer offer was rejected locally: %v", err)
	}
	operation := &ReceiverPeerOperation{
		rpc: receiver.rpc, call: call, token: new(receiverPeerOperationToken),
	}
	operationID := operation.OperationID()
	if operationID.IsZero() {
		t.Fatal("ambiguous peer offer retained no operation authority")
	}
	select {
	case <-initialLane.done:
	case <-time.After(time.Second):
		t.Fatal("ambiguous initial lane did not drain")
	}

	preferNextRuntimeLane(t, receiver, failingIdentity)
	failingSendsBefore := failingReceiver.sends.Load()
	healthySendsBefore := healthyReceiver.sends.Load()
	failingReceiver.failNextSend(false, errors.New("first request replay was not delivered"))
	candidate := peerReplayBody(t, "candidate-after-repeated-offer-failure")
	disposition, err := operation.SendCandidate(context.Background(), candidate)
	if err != nil || disposition != protocolsession.OperationDeliver {
		t.Fatalf("candidate after repeated offer failure = disposition %d, error %v", disposition, err)
	}
	assertPeerReplayEvent(t, handler.events, protocolsession.MessagePeerOffer, operationID)
	assertPeerReplayEvent(t, handler.events, protocolsession.MessagePeerCandidate, operationID)
	select {
	case event := <-handler.events:
		t.Fatalf("exact offer replay redispatched handler event kind=%d operation=%x", event.kind, event.operationID)
	default:
	}

	if got := failingReceiver.sends.Load() - failingSendsBefore; got != 1 {
		t.Fatalf("failed replacement lane sent %d frames; want only the offer replay", got)
	}
	if got := healthyReceiver.sends.Load() - healthySendsBefore; got != 2 {
		t.Fatalf("healthy replacement lane sent %d frames; want exact offer replay then candidate", got)
	}
	route := sender.routes.current(operationID)
	if route == nil || route.preferred != healthyIdentity {
		t.Fatalf("sender route=%+v, want healthy replay lane %+v", route, healthyIdentity)
	}
	if _, err := receiver.lanes.selectLane(&healthyIdentity); err != nil {
		t.Fatalf("receiver lost the healthy replay lane: %v", err)
	}
	if _, err := sender.lanes.selectLane(&healthyIdentity); err != nil {
		t.Fatalf("sender lost the healthy replay lane: %v", err)
	}
	if receiver.Err() != nil || sender.Err() != nil {
		t.Fatalf("dependency replay escaped its operation receiver=%v sender=%v", receiver.Err(), sender.Err())
	}

	assertReceiverPeerTermination(
		t,
		operation,
		operation.Terminate(context.Background()),
		ReceiverPeerTerminalAuthorityLocal,
		ReceiverPeerProvenanceLocalExplicitStop,
		ReceiverPeerTerminalOperationOnly,
		ReceiverPeerProvenanceLocalExplicitStop,
	)
	call.stateMu.Lock()
	callClosed := call.closed
	requestReleased := call.request.Kind() == 0
	replayReleased := call.replay.IsZero()
	call.stateMu.Unlock()
	if !callClosed || !requestReleased || !replayReleased {
		t.Fatalf(
			"operation close retained replay authority: closed=%t requestReleased=%t replayReleased=%t",
			callClosed, requestReleased, replayReleased,
		)
	}
	assertPeerReplayCancel(t, handler.cancels, operationID)
	waitSessionCondition(t, "repeated peer replay operation drain", func() bool {
		return sender.routes.len() == 0 && sender.operations.ActiveCount() == 0
	})
	assertSiblingSessionHealthy(t, fixture, handlers)
}

func TestAmbiguousPeerAnswerExactReplaySurvivesLaneMigration(t *testing.T) {
	fixture := newVerticalFixture(t)
	answer, err := protocolsession.EncodeBody(map[uint64]any{0: uint64(1), 1: "answer"})
	if err != nil {
		t.Fatal(err)
	}
	handlers := make(chan *peerReplayHandler, 4)
	fixture.senderFactory.peers = peerReplayHandlerFactory(handlers, answer)
	receiverConfig := fixture.receiverConfig
	receiverConfig.PeerControls = receiverPeerSemanticsForTest(protocolsession.SenderControlSemanticValidatorFunc(func(
		kind protocolsession.MessageKind,
		_ protocolsession.OperationID,
		semantic []byte,
	) error {
		if kind == protocolsession.MessagePeerAnswer && bytes.Equal(semantic, answer) {
			return nil
		}
		return protocolsession.ErrControlSemantic
	}))
	receiverFactory, err := NewReceiverFactory(receiverConfig)
	if err != nil {
		t.Fatal(err)
	}

	sender, receiver, initialSender, initialReceiver := connectObservedVerticalPair(
		t, fixture.senderFactory, receiverFactory,
	)
	defer sender.Close()
	defer receiver.Close()
	handler := <-handlers
	_, secondarySender, _, _ := attachObservedLane(t, fixture.senderFactory, receiver, 0)
	initialSenderLane, err := sender.lanes.selectLane(&sender.initial)
	if err != nil {
		t.Fatal(err)
	}
	requestSendsBefore := initialReceiver.sends.Load()
	initialAnswerSendsBefore := initialSender.sends.Load()
	secondaryAnswerSendsBefore := secondarySender.sends.Load()
	gate := initialSender.gateNextSendThenFail(errors.New("peer answer accepted before disconnect"))

	call, err := receiver.rpc.beginOn(
		context.Background(), &receiver.initial, protocolsession.MessagePeerOffer, peerReplayBody(t, "offer"),
	)
	if err != nil {
		t.Fatal(err)
	}
	operation := &ReceiverPeerOperation{
		rpc: receiver.rpc, call: call, token: new(receiverPeerOperationToken),
	}
	operationID := operation.OperationID()
	assertPeerReplayEvent(t, handler.events, protocolsession.MessagePeerOffer, operationID)
	select {
	case <-gate.started:
	case <-time.After(time.Second):
		t.Fatal("sender did not attempt the gated peer answer")
	}
	close(gate.release)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	receiveResult := operation.Receive(ctx)
	control := requireReceiverPeerControl(t, receiveResult)
	if control.Kind() != protocolsession.MessagePeerAnswer || !bytes.Equal(control.Body(), answer) {
		t.Fatalf("peer answer kind=%d body=%x", control.Kind(), control.Body())
	}
	select {
	case <-initialSenderLane.done:
	case <-time.After(time.Second):
		t.Fatal("ambiguous answer lane did not drain")
	}
	waitSessionCondition(t, "exact peer answer replay on replacement lane", func() bool {
		return secondarySender.sends.Load() == secondaryAnswerSendsBefore+1
	})
	if initialReceiver.sends.Load() != requestSendsBefore+1 ||
		initialSender.sends.Load() != initialAnswerSendsBefore+1 {
		t.Fatalf(
			"initial-lane attempts request=%d answer=%d",
			initialReceiver.sends.Load()-requestSendsBefore,
			initialSender.sends.Load()-initialAnswerSendsBefore,
		)
	}
	call.stateMu.Lock()
	queuedResponses := len(call.messages)
	call.stateMu.Unlock()
	if queuedResponses != 0 {
		t.Fatalf("exact answer replay reached the RPC sink %d extra time(s)", queuedResponses)
	}
	if receiver.Err() != nil || sender.Err() != nil {
		t.Fatalf("exact answer replay terminated the session receiver=%v sender=%v", receiver.Err(), sender.Err())
	}

	// The in-memory test channel closes one endpoint's receive queue only. Detach
	// its peer explicitly to model the transport-wide disconnect seen in production.
	receiver.lanes.detach(receiver.initial)
	assertReceiverPeerTermination(
		t,
		operation,
		operation.Terminate(context.Background()),
		ReceiverPeerTerminalAuthorityLocal,
		ReceiverPeerProvenanceLocalExplicitStop,
		ReceiverPeerTerminalOperationOnly,
		ReceiverPeerProvenanceLocalExplicitStop,
	)
	assertPeerReplayCancel(t, handler.cancels, operationID)
	waitSessionCondition(t, "peer answer operation drain", func() bool {
		return sender.routes.len() == 0 && sender.operations.ActiveCount() == 0
	})
	assertSiblingSessionHealthy(t, fixture, handlers)
}

type ambiguousObservedChannel struct {
	*observedMemoryChannel

	mu      sync.Mutex
	pending *ambiguousSend
}

type ambiguousSend struct {
	deliver bool
	err     error
}

func (channel *ambiguousObservedChannel) failNextSend(deliver bool, err error) {
	channel.mu.Lock()
	channel.pending = &ambiguousSend{deliver: deliver, err: err}
	channel.mu.Unlock()
}

func (channel *ambiguousObservedChannel) Send(ctx context.Context, frame framechannel.Frame) error {
	channel.mu.Lock()
	pending := channel.pending
	channel.pending = nil
	channel.mu.Unlock()
	if pending == nil {
		return channel.observedMemoryChannel.Send(ctx, frame)
	}
	channel.sends.Add(1)
	if pending.deliver {
		if err := channel.memoryChannel.Send(ctx, frame); err != nil {
			return err
		}
	}
	return pending.err
}

type peerReplayEvent struct {
	kind        protocolsession.MessageKind
	operationID protocolsession.OperationID
}

type peerReplayHandler struct {
	session SenderPeerSession
	answer  []byte
	events  chan peerReplayEvent
	cancels chan protocolsession.OperationID
}

func peerReplayHandlerFactory(
	created chan<- *peerReplayHandler,
	answer []byte,
) SenderPeerHandlerFactory {
	return SenderPeerHandlerFactoryFunc(func(session SenderPeerSession) (SenderPeerHandler, error) {
		handler := &peerReplayHandler{
			session: session,
			answer:  bytes.Clone(answer),
			events:  make(chan peerReplayEvent, 8),
			cancels: make(chan protocolsession.OperationID, 4),
		}
		created <- handler
		return handler, nil
	})
}

func (handler *peerReplayHandler) HandleMessage(
	ctx context.Context,
	message protocolsession.Message,
) error {
	operationID, ok := message.OperationID()
	if !ok {
		return ErrOperationMissing
	}
	handler.events <- peerReplayEvent{kind: message.Kind(), operationID: operationID}
	if message.Kind() == protocolsession.MessagePeerOffer && handler.answer != nil {
		_, err := handler.session.SendPeerControl(
			ctx, protocolsession.MessagePeerAnswer, operationID, handler.answer,
		)
		return err
	}
	return nil
}

func (handler *peerReplayHandler) Cancel(
	_ context.Context,
	operationID protocolsession.OperationID,
) error {
	handler.cancels <- operationID
	return nil
}

func (*peerReplayHandler) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func connectPeerReplayPair(
	t *testing.T,
	senderFactory *SenderFactory,
	receiverFactory *ReceiverFactory,
	senderChannel protocolsession.FrameChannel,
	receiverChannel protocolsession.FrameChannel,
) (*SenderRuntime, *ReceiverRuntime) {
	t.Helper()
	accepted := make(chan struct {
		runtime *SenderRuntime
		err     error
	}, 1)
	go func() {
		runtime, err := senderFactory.Accept(context.Background(), senderChannel)
		accepted <- struct {
			runtime *SenderRuntime
			err     error
		}{runtime: runtime, err: err}
	}()
	receiver, err := receiverFactory.Connect(context.Background(), receiverChannel)
	if err != nil {
		t.Fatal(err)
	}
	result := <-accepted
	if result.err != nil {
		receiver.Close()
		t.Fatal(result.err)
	}
	return result.runtime, receiver
}

func attachAmbiguousPeerReplayLane(
	t *testing.T,
	factory *SenderFactory,
	receiver *ReceiverRuntime,
) (LaneIdentity, *ambiguousObservedChannel) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	grant, err := receiver.RequestLane(ctx, 0)
	if err != nil {
		t.Fatalf("request ambiguous replay lane: %v", err)
	}
	senderChannel, receiverBase := newObservedChannelPair()
	receiverChannel := &ambiguousObservedChannel{observedMemoryChannel: receiverBase}
	senderResult := make(chan struct {
		identity LaneIdentity
		err      error
	}, 1)
	go func() {
		identity, attachErr := factory.Attach(ctx, senderChannel)
		senderResult <- struct {
			identity LaneIdentity
			err      error
		}{identity: identity, err: attachErr}
	}()
	receiverIdentity, receiverErr := receiver.AttachLane(ctx, grant, receiverChannel)
	senderAttached := <-senderResult
	if receiverErr != nil || senderAttached.err != nil {
		t.Fatalf("attach ambiguous replay lane: receiver=%v sender=%v", receiverErr, senderAttached.err)
	}
	if receiverIdentity != senderAttached.identity {
		t.Fatalf("ambiguous replay lane identity receiver=%+v sender=%+v", receiverIdentity, senderAttached.identity)
	}
	return receiverIdentity, receiverChannel
}

func preferNextRuntimeLane(t *testing.T, receiver *ReceiverRuntime, identity LaneIdentity) {
	t.Helper()
	receiver.lanes.mu.Lock()
	defer receiver.lanes.mu.Unlock()
	for index, laneID := range receiver.lanes.order {
		if laneID == identity.ID {
			receiver.lanes.next = uint64(index)
			return
		}
	}
	t.Fatalf("runtime lane %+v is absent from selection order", identity)
}

func assertPeerReplayEvent(
	t *testing.T,
	events <-chan peerReplayEvent,
	wantKind protocolsession.MessageKind,
	wantOperation protocolsession.OperationID,
) {
	t.Helper()
	select {
	case event := <-events:
		if event.kind != wantKind || event.operationID != wantOperation {
			t.Fatalf(
				"peer event kind=%d operation=%x, want kind=%d operation=%x",
				event.kind, event.operationID, wantKind, wantOperation,
			)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for peer event kind=%d operation=%x", wantKind, wantOperation)
	}
}

func assertPeerReplayCancel(
	t *testing.T,
	cancels <-chan protocolsession.OperationID,
	want protocolsession.OperationID,
) {
	t.Helper()
	select {
	case operationID := <-cancels:
		if operationID != want {
			t.Fatalf("peer cancel operation=%x, want %x", operationID, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for peer cancel operation=%x", want)
	}
}

func assertSiblingSessionHealthy(
	t *testing.T,
	fixture *verticalFixture,
	handlers <-chan *peerReplayHandler,
) {
	t.Helper()
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	<-handlers
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	grant, err := receiver.RequestLane(ctx, 0)
	if err != nil || grant.OperationID.IsZero() {
		t.Fatalf("sibling ProtocolSession lane request grant=%+v error=%v", grant, err)
	}
}

func peerReplayBody(t *testing.T, value string) []byte {
	t.Helper()
	body, err := protocolsession.EncodeBody(map[uint64]any{0: uint64(1), 1: value})
	if err != nil {
		t.Fatal(err)
	}
	return body
}
