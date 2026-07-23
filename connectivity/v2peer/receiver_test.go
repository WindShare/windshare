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
	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	transportwebrtc "github.com/windshare/windshare/transport/webrtc"
)

type receiverTestControl struct {
	kind protocolsession.MessageKind
	body []byte
}

func (control receiverTestControl) Kind() protocolsession.MessageKind { return control.kind }
func (control receiverTestControl) Body() []byte                      { return bytes.Clone(control.body) }

type receiverTestOperation struct {
	id                   protocolsession.OperationID
	controls             chan ReceiverControl
	candidates           chan []byte
	cancelled            chan struct{}
	cancelState          chan framechannel.ChannelState
	channel              *receiverTestChannel
	cancelOnce           sync.Once
	cancelCalls          atomic.Int32
	terminationMu        sync.Mutex
	termination          ReceiverSignalingTermination
	binding              ReceiverSignalingOperationBinding
	maximumContinuations int
	maximumHook          func() (int, bool)
	sendCandidateError   error
}

func newReceiverTestOperation() *receiverTestOperation {
	return &receiverTestOperation{
		id: testOperationID(151), controls: make(chan ReceiverControl, 16),
		candidates: make(chan []byte, 16), cancelled: make(chan struct{}),
		cancelState:          make(chan framechannel.ChannelState, 1),
		maximumContinuations: DefaultMaxCandidates,
	}
}

func (operation *receiverTestOperation) OperationID() protocolsession.OperationID {
	return operation.id
}

func (operation *receiverTestOperation) MaximumContinuations() (int, bool) {
	if operation.maximumHook != nil {
		return operation.maximumHook()
	}
	select {
	case <-operation.cancelled:
		return 0, false
	default:
	}
	return operation.maximumContinuations, operation.maximumContinuations > 0
}

func (operation *receiverTestOperation) SendCandidate(
	ctx context.Context,
	body []byte,
) (protocolsession.OperationDisposition, error) {
	if operation.sendCandidateError != nil {
		return protocolsession.OperationDrop, operation.sendCandidateError
	}
	select {
	case <-ctx.Done():
		return protocolsession.OperationDrop, ctx.Err()
	case operation.candidates <- bytes.Clone(body):
		return protocolsession.OperationDeliver, nil
	}
}

func (operation *receiverTestOperation) Receive(ctx context.Context) ReceiverSignalingReceiveResult {
	select {
	case <-ctx.Done():
		termination := operation.finishTermination(
			ReceiverTerminalLocal,
			ctx.Err(),
		)
		return receiverTestTerminalResult(termination)
	case <-operation.cancelled:
		return receiverTestTerminalResult(operation.terminationResult())
	case control := <-operation.controls:
		return NewReceiverSignalingControlResult(control)
	}
}

func (operation *receiverTestOperation) Terminate(
	context.Context,
) ReceiverSignalingTermination {
	operation.cancelCalls.Add(1)
	return operation.finishTermination(ReceiverTerminalLocal, nil)
}

func (operation *receiverTestOperation) finishTermination(
	owner ReceiverTerminalOwner,
	cause error,
) ReceiverSignalingTermination {
	return operation.finishTerminationWithDecision(receiverTestDecision(owner), cause)
}

func (operation *receiverTestOperation) finishTerminationWithDecision(
	decision receiverAttemptDecision,
	cause error,
) ReceiverSignalingTermination {
	diagnostics, truncated := snapshotReceiverCauseWithStatus(cause)
	operation.terminationMu.Lock()
	if !operation.termination.valid {
		operation.termination = ReceiverSignalingTermination{
			operationToken: operation.binding.token,
			valid:          true,
			decision:       decision,
			diagnostics:    diagnostics, diagnosticsTruncated: truncated,
		}
	} else {
		operation.termination.decision = mergeReceiverAttemptDecisions(
			operation.termination.decision,
			decision,
		)
		operation.termination.diagnostics = joinReceiverResiduals([]error{
			operation.termination.diagnostics,
			diagnostics,
		})
		operation.termination.diagnosticsTruncated =
			operation.termination.diagnosticsTruncated || truncated
	}
	termination := operation.termination
	operation.terminationMu.Unlock()
	operation.cancelOnce.Do(func() {
		if operation.channel != nil {
			operation.cancelState <- operation.channel.State()
		}
		close(operation.cancelled)
	})
	return termination
}

func (operation *receiverTestOperation) terminationResult() ReceiverSignalingTermination {
	operation.terminationMu.Lock()
	defer operation.terminationMu.Unlock()
	return operation.termination
}

func receiverTestTerminalResult(
	termination ReceiverSignalingTermination,
) ReceiverSignalingReceiveResult {
	return NewReceiverSignalingTerminationResult(termination)
}

func receiverTestDecision(owner ReceiverTerminalOwner) receiverAttemptDecision {
	switch owner {
	case ReceiverTerminalLocal:
		return receiverOperationDecision(owner, ReceiverProvenanceLocalExplicitStop)
	case ReceiverTerminalRemote:
		return receiverOperationDecision(owner, ReceiverProvenanceRemoteOperationRejected)
	case ReceiverTerminalRuntime:
		return receiverAttemptDecision{
			transitionOwner:       owner,
			transitionProvenance:  ReceiverProvenanceRuntimeStopping,
			disposition:           ReceiverDispositionSessionUnavailable,
			consequenceProvenance: ReceiverProvenanceRuntimeStopping,
		}
	default:
		return receiverOperationDecision(
			ReceiverTerminalUnbound,
			ReceiverProvenanceSignalingAdapterContract,
		)
	}
}

func TestReceiverAttemptMalformedSemanticCancelsOnlyExactOperation(t *testing.T) {
	for _, test := range []struct {
		name string
		kind protocolsession.MessageKind
	}{
		{name: "answer", kind: protocolsession.MessagePeerAnswer},
		{name: "candidate", kind: protocolsession.MessagePeerCandidate},
	} {
		t.Run(test.name, func(t *testing.T) {
			parent := t.Context()
			harness := newReceiverHarnessWithContext(t, parent, nil)
			harness.operation.controls <- receiverTestControl{kind: test.kind, body: []byte{0xff}}
			select {
			case <-harness.attempt.Done():
			case <-time.After(peerTestTimeout):
				t.Fatal("malformed peer control did not finish its attempt")
			}
			if harness.attempt.Err() == nil || errors.Is(harness.attempt.Err(), context.Canceled) {
				t.Fatalf("malformed peer control error=%v", harness.attempt.Err())
			}
			if parent.Err() != nil {
				t.Fatalf("operation-local peer failure canceled parent: %v", parent.Err())
			}
			receiveTest(t, harness.operation.cancelled)
			if state := receiveTest(t, harness.operation.cancelState); state != framechannel.Open {
				t.Fatalf("signaling canceled after physical close: %v", state)
			}
			if calls := harness.operation.cancelCalls.Load(); calls != 1 {
				t.Fatalf("signaling cancel calls=%d", calls)
			}
			harness.attempt.inboxMu.Lock()
			closed, queued := harness.attempt.closed, len(harness.attempt.events)
			harness.attempt.inboxMu.Unlock()
			if !closed || queued != 0 {
				t.Fatalf("failed attempt closed=%v queued=%d", closed, queued)
			}

			sibling := newReceiverHarnessWithContext(t, parent, nil)
			sibling.answer(t)
			sibling.openAndAwaitLane(t)
			if err := sibling.attempt.Close(); err != nil {
				t.Fatalf("sibling attempt close: %v", err)
			}
		})
	}
}

func TestReceiverAttemptRejectsUnexpectedAuthenticatedControlKind(t *testing.T) {
	harness := newReceiverHarness(t, nil)
	harness.operation.controls <- receiverTestControl{kind: protocolsession.MessageCatalogResult}
	receiveTest(t, harness.attempt.Done())

	outcome := harness.attempt.Outcome()
	if !errors.Is(harness.attempt.Err(), ErrProtocol) ||
		!errors.Is(outcome.RetainedCause(), ErrProtocol) ||
		outcome.TransitionAuthority() != ReceiverTerminalLocal ||
		outcome.TransitionProvenance() != ReceiverProvenanceLocalExplicitStop ||
		outcome.Disposition() != ReceiverDispositionFallbackAllowed ||
		!outcome.LocallyCanceled() {
		t.Fatalf("unexpected authenticated control outcome=%+v", outcome)
	}
}

type receiverTestSignaling struct {
	operation ReceiverSignalingOperation
	offers    chan []byte
	open      func(
		context.Context,
		ReceiverSignalingOperationBinding,
		[]byte,
	) (ReceiverSignalingOperation, error)
}

func (signaling *receiverTestSignaling) OpenPeerOperation(
	ctx context.Context,
	binding ReceiverSignalingOperationBinding,
	offer []byte,
) (ReceiverSignalingOperation, error) {
	signaling.offers <- bytes.Clone(offer)
	var operation ReceiverSignalingOperation
	var err error
	if signaling.open != nil {
		operation, err = signaling.open(ctx, binding, offer)
	} else {
		operation = signaling.operation
	}
	if bound, ok := operation.(interface {
		bindReceiverSignalingOperation(ReceiverSignalingOperationBinding)
	}); ok {
		bound.bindReceiverSignalingOperation(binding)
	}
	return operation, err
}

func (operation *receiverTestOperation) bindReceiverSignalingOperation(
	binding ReceiverSignalingOperationBinding,
) {
	operation.terminationMu.Lock()
	operation.binding = binding
	operation.terminationMu.Unlock()
}

type receiverTestPeerConnection struct {
	mu             sync.Mutex
	onCandidate    func(*pion.ICECandidate)
	onState        func(pion.PeerConnectionState)
	onData         func(*pion.DataChannel)
	local          *pion.SessionDescription
	remote         chan pion.SessionDescription
	added          chan pion.ICECandidateInit
	closed         chan struct{}
	closeOnce      sync.Once
	raw            *pion.DataChannel
	createChannels atomic.Int32
	channelLabel   string
	channelInit    *pion.DataChannelInit
}

func newReceiverTestPeerConnection() *receiverTestPeerConnection {
	return &receiverTestPeerConnection{
		remote: make(chan pion.SessionDescription, 4), added: make(chan pion.ICECandidateInit, 16),
		closed: make(chan struct{}), raw: &pion.DataChannel{},
	}
}

func (peer *receiverTestPeerConnection) OnICECandidate(callback func(*pion.ICECandidate)) {
	peer.mu.Lock()
	peer.onCandidate = callback
	peer.mu.Unlock()
}

func (peer *receiverTestPeerConnection) OnConnectionStateChange(callback func(pion.PeerConnectionState)) {
	peer.mu.Lock()
	peer.onState = callback
	peer.mu.Unlock()
}

func (peer *receiverTestPeerConnection) OnDataChannel(callback func(*pion.DataChannel)) {
	peer.mu.Lock()
	peer.onData = callback
	peer.mu.Unlock()
}

func (peer *receiverTestPeerConnection) CreateDataChannel(
	label string,
	init *pion.DataChannelInit,
) (*pion.DataChannel, error) {
	peer.createChannels.Add(1)
	peer.channelLabel = label
	peer.channelInit = init
	return peer.raw, nil
}

func (*receiverTestPeerConnection) CreateOffer(*pion.OfferOptions) (pion.SessionDescription, error) {
	return pion.SessionDescription{Type: pion.SDPTypeOffer, SDP: "v=0\r\ns=pre-gather-offer\r\n"}, nil
}

func (peer *receiverTestPeerConnection) SetLocalDescription(description pion.SessionDescription) error {
	description.SDP = "v=0\r\ns=authoritative-local-offer\r\n"
	peer.mu.Lock()
	peer.local = &description
	peer.mu.Unlock()
	return nil
}

func (peer *receiverTestPeerConnection) LocalDescription() *pion.SessionDescription {
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if peer.local == nil {
		return nil
	}
	result := *peer.local
	return &result
}

func (peer *receiverTestPeerConnection) SetRemoteDescription(description pion.SessionDescription) error {
	peer.remote <- description
	return nil
}

func (peer *receiverTestPeerConnection) AddICECandidate(candidate pion.ICECandidateInit) error {
	peer.added <- candidate
	return nil
}

func (peer *receiverTestPeerConnection) Close() error {
	peer.closeOnce.Do(func() { close(peer.closed) })
	return nil
}

func (peer *receiverTestPeerConnection) emitCandidate(candidate *pion.ICECandidate) {
	peer.mu.Lock()
	callback := peer.onCandidate
	peer.mu.Unlock()
	callback(candidate)
}

func (peer *receiverTestPeerConnection) emitUnexpectedDataChannel(channel *pion.DataChannel) {
	peer.mu.Lock()
	callback := peer.onData
	peer.mu.Unlock()
	callback(channel)
}

type receiverTestChannel struct {
	receive   chan framechannel.Frame
	opened    chan struct{}
	done      chan struct{}
	openOnce  sync.Once
	closeOnce sync.Once
	state     atomic.Uint32
}

func newReceiverTestChannel() *receiverTestChannel {
	channel := &receiverTestChannel{
		receive: make(chan framechannel.Frame), opened: make(chan struct{}), done: make(chan struct{}),
	}
	channel.state.Store(uint32(framechannel.Open))
	return channel
}

func (*receiverTestChannel) Send(context.Context, framechannel.Frame) error         { return nil }
func (*receiverTestChannel) SendTerminal(context.Context, framechannel.Frame) error { return nil }
func (channel *receiverTestChannel) Recv() <-chan framechannel.Frame                { return channel.receive }
func (channel *receiverTestChannel) State() framechannel.ChannelState {
	return framechannel.ChannelState(channel.state.Load())
}
func (channel *receiverTestChannel) Opened() <-chan struct{} { return channel.opened }
func (channel *receiverTestChannel) Done() <-chan struct{}   { return channel.done }
func (*receiverTestChannel) Err() error                      { return nil }
func (channel *receiverTestChannel) open()                   { channel.openOnce.Do(func() { close(channel.opened) }) }
func (channel *receiverTestChannel) Close() error {
	channel.closeOnce.Do(func() {
		channel.state.Store(uint32(framechannel.Closed))
		close(channel.done)
		close(channel.receive)
	})
	return nil
}

type receiverTestLanes struct {
	requested   chan uint32
	attachments chan protocolsession.FrameChannel
	lane        sessionruntime.LaneIdentity
	err         error
}

func newReceiverTestLanes() *receiverTestLanes {
	return &receiverTestLanes{
		requested: make(chan uint32, 4), attachments: make(chan protocolsession.FrameChannel, 4),
		lane: sessionruntime.LaneIdentity{ID: 17, Epoch: 4},
	}
}

func (lanes *receiverTestLanes) RequestLane(
	ctx context.Context,
	requested uint32,
) (sessionruntime.LaneAttachmentGrant, error) {
	select {
	case <-ctx.Done():
		return sessionruntime.LaneAttachmentGrant{}, ctx.Err()
	case lanes.requested <- requested:
	}
	return sessionruntime.LaneAttachmentGrant{
		LaneID: lanes.lane.ID, LaneEpoch: lanes.lane.Epoch, OperationID: testOperationID(152),
	}, lanes.err
}

func (lanes *receiverTestLanes) AttachLane(
	ctx context.Context,
	_ sessionruntime.LaneAttachmentGrant,
	channel protocolsession.FrameChannel,
) (sessionruntime.LaneIdentity, error) {
	select {
	case <-ctx.Done():
		return sessionruntime.LaneIdentity{}, ctx.Err()
	case lanes.attachments <- channel:
	}
	return lanes.lane, lanes.err
}

type receiverHarness struct {
	peer      *receiverTestPeerConnection
	channel   *receiverTestChannel
	operation *receiverTestOperation
	signaling *receiverTestSignaling
	lanes     *receiverTestLanes
	attempt   *ReceiverAttempt
	binding   v2signal.Binding
}

func newReceiverHarness(t *testing.T, configure func(*ReceiverFactoryConfig, *receiverTestSignaling)) *receiverHarness {
	return newReceiverHarnessWithContext(t, context.Background(), configure)
}

func newReceiverHarnessWithContext(
	t *testing.T,
	parent context.Context,
	configure func(*ReceiverFactoryConfig, *receiverTestSignaling),
) *receiverHarness {
	t.Helper()
	peer := newReceiverTestPeerConnection()
	channel := newReceiverTestChannel()
	operation := newReceiverTestOperation()
	operation.channel = channel
	signaling := &receiverTestSignaling{operation: operation, offers: make(chan []byte, 2)}
	lanes := newReceiverTestLanes()
	config := ReceiverFactoryConfig{
		PeerConnections: ReceiverPeerConnectionFactoryFunc(
			func(pion.Configuration) (ReceiverPeerConnection, error) { return peer, nil },
		),
		DataChannels: DataChannelAdapterFunc(
			func(*pion.DataChannel) (PeerDataChannel, error) { return channel, nil },
		),
		AttemptTimeout: peerTestTimeout,
		Random:         bytes.NewReader(bytes.Repeat([]byte{0x5a}, v2signal.IdentityBytes*2)),
	}
	if configure != nil {
		configure(&config, signaling)
	}
	factory, err := NewReceiverFactory(config)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := factory.Start(parent, signaling, lanes)
	if err != nil {
		t.Fatal(err)
	}
	offerBody := receiveTest(t, signaling.offers)
	offer, err := v2signal.DecodeOffer(offerBody)
	if err != nil {
		t.Fatal(err)
	}
	return &receiverHarness{
		peer: peer, channel: channel, operation: operation, signaling: signaling,
		lanes: lanes, attempt: attempt, binding: offer.Binding,
	}
}

func (harness *receiverHarness) answer(t *testing.T) {
	t.Helper()
	body, err := v2signal.EncodeAnswer(v2signal.Answer{
		Binding: harness.binding, SDP: "v=0\r\ns=sender-answer\r\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	harness.operation.controls <- receiverTestControl{kind: protocolsession.MessagePeerAnswer, body: body}
	remote := receiveTest(t, harness.peer.remote)
	if remote.Type != pion.SDPTypeAnswer || remote.SDP != "v=0\r\ns=sender-answer\r\n" {
		t.Fatalf("remote answer = %#v", remote)
	}
}

func (harness *receiverHarness) openAndAwaitLane(t *testing.T) {
	t.Helper()
	harness.channel.open()
	if requested := receiveTest(t, harness.lanes.requested); requested != 0 {
		t.Fatalf("requested lane = %d", requested)
	}
	if attached := receiveTest(t, harness.lanes.attachments); attached != harness.channel {
		t.Fatalf("attached channel = %T", attached)
	}
	select {
	case <-harness.attempt.Ready():
	case <-time.After(peerTestTimeout):
		t.Fatal("receiver peer lane did not become ready")
	}
}

func TestReceiverAttemptNegotiatesTricklesAndAttachesOneLane(t *testing.T) {
	harness := newReceiverHarness(t, nil)
	if harness.peer.createChannels.Load() != 1 || harness.peer.channelLabel != transportwebrtc.ChannelLabel {
		t.Fatalf("created DataChannels = %d, label = %q", harness.peer.createChannels.Load(), harness.peer.channelLabel)
	}
	if harness.peer.channelInit == nil {
		t.Fatal("receiver did not apply the canonical DataChannel profile")
	}
	offer := harness.peer.LocalDescription()
	if offer == nil || offer.SDP != "v=0\r\ns=authoritative-local-offer\r\n" {
		t.Fatalf("local offer = %#v", offer)
	}

	queued := v2signal.Candidate{Binding: harness.binding, Candidate: "candidate:sender-before-answer"}
	queuedBody, _ := v2signal.EncodeCandidate(queued)
	harness.operation.controls <- receiverTestControl{kind: protocolsession.MessagePeerCandidate, body: queuedBody}
	harness.answer(t)
	if added := receiveTest(t, harness.peer.added); added.Candidate != queued.Candidate {
		t.Fatalf("queued remote candidate = %#v", added)
	}

	harness.peer.emitCandidate(&pion.ICECandidate{
		Foundation: "1", Priority: 1, Address: "127.0.0.1", Protocol: pion.ICEProtocolUDP,
		Port: 43211, Typ: pion.ICECandidateTypeHost, Component: 1,
	})
	localBody := receiveTest(t, harness.operation.candidates)
	local, err := v2signal.DecodeCandidate(localBody)
	if err != nil || local.Binding != harness.binding || local.Candidate == "" {
		t.Fatalf("local candidate = %#v, %v", local, err)
	}

	harness.openAndAwaitLane(t)
	if lane, ok := harness.attempt.Lane(); !ok || lane != harness.lanes.lane {
		t.Fatalf("attached lane = %+v, %v", lane, ok)
	}
	select {
	case <-harness.operation.cancelled:
		t.Fatal("signaling operation ended before its peer path detached")
	default:
	}
	if err := harness.attempt.Close(); err != nil {
		t.Fatal(err)
	}
	if state := receiveTest(t, harness.operation.cancelState); state != framechannel.Open {
		t.Fatalf("explicit shutdown canceled signaling after physical close: %v", state)
	}
	receiveTest(t, harness.operation.cancelled)
	receiveTest(t, harness.peer.closed)
}

func TestReceiverAttemptRequiresCandidateLimitEqualToOperationAuthority(t *testing.T) {
	for _, mismatch := range []struct {
		name      string
		leaf      int
		authority int
	}{
		{name: "leaf above authority", leaf: DefaultMaxCandidates, authority: DefaultMaxCandidates - 1},
		{name: "leaf below authority", leaf: 2, authority: DefaultMaxCandidates},
	} {
		t.Run(mismatch.name, func(t *testing.T) {
			harness := newReceiverHarness(t, func(config *ReceiverFactoryConfig, signaling *receiverTestSignaling) {
				config.MaxCandidates = mismatch.leaf
				signaling.operation.(*receiverTestOperation).maximumContinuations = mismatch.authority
			})
			select {
			case <-harness.attempt.Done():
			case <-time.After(peerTestTimeout):
				t.Fatal("receiver did not reject a candidate-limit mismatch")
			}
			if err := harness.attempt.Err(); !errors.Is(err, ErrConfig) ||
				!errors.Is(err, protocolsession.ErrContinuationAuthority) {
				t.Fatalf("candidate-limit mismatch error = %v", err)
			}
			if calls := harness.operation.cancelCalls.Load(); calls != 1 {
				t.Fatalf("mismatched operation cancel calls = %d, want 1", calls)
			}
			outcome := harness.attempt.Outcome()
			if outcome.TransitionAuthority() != ReceiverTerminalLocal ||
				outcome.TransitionProvenance() != ReceiverProvenanceSignalingAdapterContract ||
				outcome.Disposition() != ReceiverDispositionFallbackAllowed ||
				outcome.ConsequenceProvenance() != ReceiverProvenanceSignalingAdapterContract ||
				!errors.Is(outcome.RetainedCause(), ErrProtocol) {
				t.Fatalf("candidate-limit adapter outcome=%+v", outcome)
			}
		})
	}
}

func TestReceiverAttemptCloseCannotInvalidateAuthorityValidation(t *testing.T) {
	maximumEntered := make(chan struct{})
	releaseMaximum := make(chan struct{})
	var operation *receiverTestOperation
	harness := newReceiverHarness(t, func(_ *ReceiverFactoryConfig, signaling *receiverTestSignaling) {
		operation = signaling.operation.(*receiverTestOperation)
		operation.maximumHook = func() (int, bool) {
			close(maximumEntered)
			<-releaseMaximum
			select {
			case <-operation.cancelled:
				return 0, false
			default:
				return operation.maximumContinuations, true
			}
		}
	})
	receiveTest(t, maximumEntered)

	closed := make(chan error, 1)
	go func() { closed <- harness.attempt.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned before authority validation completed: %v", err)
	default:
	}
	close(releaseMaximum)
	if err := receiveTest(t, closed); err != nil {
		t.Fatalf("Close raced authority validation: %v", err)
	}
	if calls := operation.cancelCalls.Load(); calls != 1 {
		t.Fatalf("exact operation Terminate calls=%d, want 1", calls)
	}
	outcome := harness.attempt.Outcome()
	if outcome.TransitionAuthority() != ReceiverTerminalLocal || !outcome.LocallyCanceled() ||
		errors.Is(outcome.RetainedCause(), protocolsession.ErrContinuationAuthority) {
		t.Fatalf("authority-validation Close outcome=%+v", outcome)
	}
}

func TestReceiverAttemptRuntimeShutdownDuringAuthorityValidationStaysRuntimeOwned(t *testing.T) {
	maximumEntered := make(chan struct{})
	releaseMaximum := make(chan struct{})
	var operation *receiverTestOperation
	harness := newReceiverHarness(t, func(_ *ReceiverFactoryConfig, signaling *receiverTestSignaling) {
		operation = signaling.operation.(*receiverTestOperation)
		operation.maximumHook = func() (int, bool) {
			close(maximumEntered)
			<-releaseMaximum
			// Core snapshots this authority when OpenPeerOperation admits the exact
			// generation, so runtime cleanup cannot invalidate the immutable budget.
			return operation.maximumContinuations, true
		}
	})
	receiveTest(t, maximumEntered)
	operation.finishTermination(
		ReceiverTerminalRuntime,
		sessionruntime.ErrRuntimeClosed,
	)
	close(releaseMaximum)
	receiveTest(t, harness.attempt.Done())

	outcome := harness.attempt.Outcome()
	if outcome.TransitionAuthority() != ReceiverTerminalRuntime ||
		outcome.Disposition() != ReceiverDispositionSessionUnavailable ||
		outcome.TransitionProvenance() != ReceiverProvenanceRuntimeStopping ||
		outcome.ConsequenceProvenance() != ReceiverProvenanceRuntimeStopping ||
		!errors.Is(outcome.RetainedCause(), sessionruntime.ErrRuntimeClosed) ||
		errors.Is(outcome.RetainedCause(), ErrConfig) ||
		errors.Is(outcome.RetainedCause(), protocolsession.ErrContinuationAuthority) ||
		outcome.HasRetainedCauseClass(ReceiverCauseConfiguration) ||
		!outcome.HasRetainedCauseClass(ReceiverCauseRuntimeClosed) {
		t.Fatalf("runtime/authority interleaving outcome=%+v", outcome)
	}
}

func TestReceiverAttemptTerminatesZeroIDOperation(t *testing.T) {
	harness := newReceiverHarness(t, func(_ *ReceiverFactoryConfig, signaling *receiverTestSignaling) {
		signaling.operation.(*receiverTestOperation).id = protocolsession.OperationID{}
	})
	receiveTest(t, harness.attempt.Done())

	if !errors.Is(harness.attempt.Err(), ErrNegotiation) {
		t.Fatalf("zero-ID operation result=%v", harness.attempt.Err())
	}
	if calls := harness.operation.cancelCalls.Load(); calls != 1 {
		t.Fatalf("zero-ID exact operation Terminate calls=%d, want 1", calls)
	}
	outcome := harness.attempt.Outcome()
	if outcome.LocalGeneration() == 0 || outcome.OperationID() != (protocolsession.OperationID{}) ||
		outcome.TransitionAuthority() != ReceiverTerminalLocal ||
		outcome.TransitionProvenance() != ReceiverProvenanceSignalingAdapterContract ||
		outcome.Disposition() != ReceiverDispositionFallbackAllowed ||
		outcome.ConsequenceProvenance() != ReceiverProvenanceSignalingAdapterContract ||
		!errors.Is(outcome.RetainedCause(), ErrProtocol) {
		t.Fatalf("zero-ID operation correlation outcome=%+v", outcome)
	}
}

func TestReceiverAttemptDoesNotApplyTerminalBenignPolicyToWorkflowFailure(t *testing.T) {
	harness := newReceiverHarness(t, func(_ *ReceiverFactoryConfig, signaling *receiverTestSignaling) {
		signaling.operation.(*receiverTestOperation).sendCandidateError = sessionruntime.ErrOperationMissing
	})
	harness.peer.emitCandidate(&pion.ICECandidate{
		Foundation: "workflow", Priority: 1, Address: "127.0.0.1", Protocol: pion.ICEProtocolUDP,
		Port: 43212, Typ: pion.ICECandidateTypeHost, Component: 1,
	})
	receiveTest(t, harness.attempt.Done())

	outcome := harness.attempt.Outcome()
	if outcome.TransitionAuthority() != ReceiverTerminalLocal ||
		!errors.Is(outcome.RetainedCause(), sessionruntime.ErrOperationMissing) ||
		outcome.TransitionProvenance() != ReceiverProvenanceLocalExplicitStop ||
		outcome.Disposition() != ReceiverDispositionFallbackAllowed ||
		!outcome.LocallyCanceled() {
		t.Fatalf("workflow operation-missing outcome=%+v", outcome)
	}
}

func TestReceiverAttemptAcceptsAlignedLowerCandidateLimit(t *testing.T) {
	const alignedLimit = 2
	harness := newReceiverHarness(t, func(config *ReceiverFactoryConfig, signaling *receiverTestSignaling) {
		config.MaxCandidates = alignedLimit
		signaling.operation.(*receiverTestOperation).maximumContinuations = alignedLimit
	})
	defer harness.attempt.Close()
	harness.answer(t)
	select {
	case <-harness.attempt.Done():
		t.Fatalf("aligned lower candidate limit ended attempt: %v", harness.attempt.Err())
	default:
	}
}

func TestReceiverAttemptPhysicalLossCancelsSignalingWithoutEndingParent(t *testing.T) {
	parent := t.Context()
	harness := newReceiverHarnessWithContext(t, parent, nil)
	harness.answer(t)
	harness.openAndAwaitLane(t)
	if err := harness.channel.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-harness.attempt.Done():
	case <-time.After(peerTestTimeout):
		t.Fatal("failed peer path did not finish")
	}
	if parent.Err() != nil {
		t.Fatal("peer failure canceled the relay/transfer parent")
	}
	receiveTest(t, harness.operation.cancelled)
}

func TestReceiverAttemptRejectsAuthenticatedBindingSubstitution(t *testing.T) {
	harness := newReceiverHarness(t, nil)
	wrong := harness.binding
	wrong.AttemptID[0] ^= 0xff
	body, err := v2signal.EncodeAnswer(v2signal.Answer{Binding: wrong, SDP: "v=0\r\ns=substituted\r\n"})
	if err != nil {
		t.Fatal(err)
	}
	harness.operation.controls <- receiverTestControl{kind: protocolsession.MessagePeerAnswer, body: body}
	select {
	case <-harness.attempt.Done():
	case <-time.After(peerTestTimeout):
		t.Fatal("binding substitution did not finish the attempt")
	}
	outcome := harness.attempt.Outcome()
	if outcome.Disposition() != ReceiverDispositionSessionUnsafe ||
		outcome.ConsequenceProvenance() != ReceiverProvenanceAuthenticatedAnswerBindingMismatch ||
		!outcome.RequiresSessionClose() {
		t.Fatalf("binding substitution outcome=%+v error=%v", outcome, harness.attempt.Err())
	}
	receiveTest(t, harness.operation.cancelled)
}

func TestReceiverAttemptRejectsSecondAuthenticatedAnswer(t *testing.T) {
	harness := newReceiverHarness(t, nil)
	harness.answer(t)
	body, err := v2signal.EncodeAnswer(v2signal.Answer{
		Binding: harness.binding, SDP: "v=0\r\ns=second-sender-answer\r\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	harness.operation.controls <- receiverTestControl{kind: protocolsession.MessagePeerAnswer, body: body}
	select {
	case <-harness.attempt.Done():
	case <-time.After(peerTestTimeout):
		t.Fatal("second authenticated answer did not finish the attempt")
	}
	outcome := harness.attempt.Outcome()
	if outcome.Disposition() != ReceiverDispositionSessionUnsafe ||
		outcome.ConsequenceProvenance() != ReceiverProvenanceAuthenticatedSecondAnswer ||
		!outcome.RequiresSessionClose() {
		t.Fatalf("second authenticated answer outcome=%+v error=%v", outcome, harness.attempt.Err())
	}
}

func TestReceiverAttemptIsolatesSenderCreatedDataChannelToPeerPath(t *testing.T) {
	parent := t.Context()
	harness := newReceiverHarnessWithContext(t, parent, nil)
	harness.peer.emitUnexpectedDataChannel(nil)
	select {
	case <-harness.attempt.Done():
	case <-time.After(peerTestTimeout):
		t.Fatal("unexpected sender DataChannel did not finish the attempt")
	}
	outcome := harness.attempt.Outcome()
	if !errors.Is(harness.attempt.Err(), errChannelAdmission) ||
		outcome.Disposition() != ReceiverDispositionFallbackAllowed ||
		outcome.RequiresSessionClose() {
		t.Fatalf("unexpected sender DataChannel outcome=%+v error=%v", outcome, harness.attempt.Err())
	}
	if parent.Err() != nil {
		t.Fatal("unexpected sender DataChannel ended the relay-backed parent")
	}
	receiveTest(t, harness.operation.cancelled)
}

func TestReceiverAttemptTimeoutIncludesSignalingOpen(t *testing.T) {
	harness := newReceiverHarness(t, func(config *ReceiverFactoryConfig, signaling *receiverTestSignaling) {
		config.AttemptTimeout = 20 * time.Millisecond
		signaling.open = func(
			ctx context.Context,
			_ ReceiverSignalingOperationBinding,
			_ []byte,
		) (ReceiverSignalingOperation, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
	})
	select {
	case <-harness.attempt.Done():
	case <-time.After(peerTestTimeout):
		t.Fatal("signaling open ignored the attempt deadline")
	}
	if !errors.Is(harness.attempt.Err(), errAttemptTimeout) {
		t.Fatalf("signaling timeout error = %v", harness.attempt.Err())
	}
	select {
	case <-harness.lanes.requested:
		t.Fatal("failed peer negotiation touched the relay-backed lane runtime")
	default:
	}
}

func TestReceiverAttemptEnforcesRemoteCandidateBudget(t *testing.T) {
	harness := newReceiverHarness(t, func(config *ReceiverFactoryConfig, signaling *receiverTestSignaling) {
		config.MaxCandidates = 1
		signaling.operation.(*receiverTestOperation).maximumContinuations = 1
	})
	for _, semantic := range []string{"candidate:sender-one", "candidate:sender-two"} {
		candidate := v2signal.Candidate{Binding: harness.binding, Candidate: semantic}
		body, err := v2signal.EncodeCandidate(candidate)
		if err != nil {
			t.Fatal(err)
		}
		harness.operation.controls <- receiverTestControl{kind: protocolsession.MessagePeerCandidate, body: body}
	}
	select {
	case <-harness.attempt.Done():
	case <-time.After(peerTestTimeout):
		t.Fatal("candidate overflow did not finish the attempt")
	}
	if !errors.Is(harness.attempt.Err(), errCandidateLimit) {
		t.Fatalf("candidate overflow error = %v", harness.attempt.Err())
	}
}
