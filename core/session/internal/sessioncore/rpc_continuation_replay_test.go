package sessioncore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

const continuationReplayTestTimeout = time.Second

func TestRPCContinuationRetriesStoppedPreferredWriterAcrossLane(t *testing.T) {
	fixture := newContinuationReplayFixture(t, 101)
	stopWriterBeforeAdmission(t, fixture.preferred.writer)
	startContinuationReplayWriter(t, fixture.replacement.writer)

	disposition, err := fixture.rpc.SendContinuation(
		context.Background(), fixture.call, protocolsession.MessagePeerCandidate, []byte{0xf6},
	)
	if err != nil || disposition != protocolsession.OperationDeliver {
		t.Fatalf("continuation after stopped preferred writer = disposition %d, error %v", disposition, err)
	}
	fixture.assertReplacementOrder()
}

func TestRPCContinuationRetriesStaleDrainedPreadmissionDropAcrossLane(t *testing.T) {
	fixture := newContinuationReplayFixture(t, 102)
	startContinuationReplayWriter(t, fixture.replacement.writer)
	waitContext := newContinuationAwaitBarrier()
	result := make(chan continuationSendResult, 1)
	go func() {
		disposition, err := fixture.rpc.SendContinuation(
			waitContext, fixture.call, protocolsession.MessagePeerCandidate, []byte{0xf6},
		)
		result <- continuationSendResult{disposition: disposition, err: err}
	}()

	select {
	case <-waitContext.awaited:
	case <-time.After(continuationReplayTestTimeout):
		t.Fatal("continuation did not reach writer wait")
	}
	stopWriterBeforeAdmission(t, fixture.preferred.writer)

	select {
	case sent := <-result:
		if sent.err != nil || sent.disposition != protocolsession.OperationDeliver {
			t.Fatalf("continuation after stale queue drain = disposition %d, error %v", sent.disposition, sent.err)
		}
	case <-time.After(continuationReplayTestTimeout):
		t.Fatal("continuation did not retry after stale queue drain")
	}
	fixture.assertReplacementOrder()
}

func TestRPCContinuationRetriesClaimedPretransportFailureAcrossLane(t *testing.T) {
	fixture := newContinuationReplayFixture(t, 104)
	pretransportErr := errors.New("candidate seal failed")
	failingWriter, err := newLaneWriter(
		fixture.preferred.channel,
		continuationFailingSealer{err: pretransportErr},
	)
	if err != nil {
		t.Fatal(err)
	}
	fixture.runtime.lanes.mu.Lock()
	preferred := fixture.runtime.lanes.active[fixture.preferred.identity.ID]
	preferred.writer = failingWriter
	fixture.runtime.lanes.mu.Unlock()
	fixture.preferred.writer = failingWriter
	startContinuationReplayWriter(t, failingWriter)
	startContinuationReplayWriter(t, fixture.replacement.writer)

	disposition, err := fixture.rpc.SendContinuation(
		context.Background(), fixture.call, protocolsession.MessagePeerCandidate, []byte{0xf6},
	)
	if err != nil || disposition != protocolsession.OperationDeliver {
		t.Fatalf("continuation after pretransport failure = disposition %d, error %v", disposition, err)
	}
	fixture.assertReplacementOrder()
}

func TestRPCContinuationGateCoversAutomaticReplacementRetry(t *testing.T) {
	fixture := newTrackedContinuationReplayFixture(t, 106)
	pretransportErr := errors.New("blocked candidate seal failed")
	blocker := &continuationBlockingFailingSealer{
		entered: make(chan struct{}), release: make(chan struct{}), err: pretransportErr,
	}
	fixture.replacePreferredWriter(t, blocker)
	startContinuationReplayWriter(t, fixture.preferred.writer)
	startContinuationReplayWriter(t, fixture.replacement.writer)
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(blocker.release) }) })

	first := make(chan continuationSendResult, 1)
	go func() {
		disposition, err := fixture.rpc.SendContinuation(
			context.Background(), fixture.call, protocolsession.MessagePeerCandidate, []byte{0xf6},
		)
		first <- continuationSendResult{disposition: disposition, err: err}
	}()
	select {
	case <-blocker.entered:
	case <-time.After(continuationReplayTestTimeout):
		t.Fatal("first candidate did not reach replacement-triggering seal")
	}

	secondContext := newContinuationAwaitBarrier()
	second := make(chan continuationSendResult, 1)
	go func() {
		disposition, err := fixture.rpc.SendContinuation(
			secondContext, fixture.call, protocolsession.MessagePeerCandidate, []byte{0xf6},
		)
		second <- continuationSendResult{disposition: disposition, err: err}
	}()
	select {
	case <-secondContext.awaited:
	case <-time.After(continuationReplayTestTimeout):
		t.Fatal("second candidate did not reach the operation gate")
	}
	select {
	case result := <-second:
		t.Fatalf("second candidate escaped replacement retry owner: disposition %d, error %v", result.disposition, result.err)
	default:
	}

	releaseOnce.Do(func() { close(blocker.release) })
	select {
	case result := <-first:
		if result.disposition != protocolsession.OperationDeliver || result.err != nil {
			t.Fatalf("replacement retry owner = disposition %d, error %v", result.disposition, result.err)
		}
	case <-time.After(continuationReplayTestTimeout):
		t.Fatal("replacement retry owner did not settle")
	}
	select {
	case result := <-second:
		if result.disposition != protocolsession.OperationDrop || result.err != nil {
			t.Fatalf("exact waiter after committed retry = disposition %d, error %v", result.disposition, result.err)
		}
	case <-time.After(continuationReplayTestTimeout):
		t.Fatal("exact waiter did not observe committed replacement retry")
	}
	fixture.assertReplacementOrder()
}

func TestOperationCandidateGateCancellationDoesNotLeakToken(t *testing.T) {
	call := &Call{done: make(chan struct{})}
	releaseOwner, err := call.acquireCandidateSend(context.Background(), context.Background())
	if err != nil {
		t.Fatal(err)
	}

	waiterBase, cancelWaiter := context.WithCancel(context.Background())
	waiterContext := &continuationAwaitBarrier{
		Context: waiterBase, awaited: make(chan struct{}),
	}
	waiter := make(chan error, 1)
	go func() {
		release, acquireErr := call.acquireCandidateSend(waiterContext, context.Background())
		if release != nil {
			release()
		}
		waiter <- acquireErr
	}()
	select {
	case <-waiterContext.awaited:
	case <-time.After(continuationReplayTestTimeout):
		t.Fatal("waiter did not reach candidate gate")
	}
	cancelWaiter()
	select {
	case acquireErr := <-waiter:
		if !errors.Is(acquireErr, context.Canceled) {
			t.Fatalf("canceled waiter error = %v", acquireErr)
		}
	case <-time.After(continuationReplayTestTimeout):
		t.Fatal("canceled candidate waiter did not return")
	}

	releaseOwner()
	thirdContext, cancelThird := context.WithTimeout(context.Background(), continuationReplayTestTimeout)
	defer cancelThird()
	releaseThird, err := call.acquireCandidateSend(thirdContext, context.Background())
	if err != nil {
		t.Fatalf("candidate gate leaked after canceled waiter: %v", err)
	}
	releaseThird()
}

type continuationFailingSealer struct{ err error }

func (sealer continuationFailingSealer) Seal([]byte) (protocolsession.SealedEnvelope, error) {
	return protocolsession.SealedEnvelope{}, sealer.err
}

type continuationBlockingFailingSealer struct {
	entered chan struct{}
	release chan struct{}
	err     error
}

func (sealer *continuationBlockingFailingSealer) Seal([]byte) (protocolsession.SealedEnvelope, error) {
	close(sealer.entered)
	<-sealer.release
	return protocolsession.SealedEnvelope{}, sealer.err
}

func TestRPCContinuationDoesNotRetryAdmittedUnknownAcrossLane(t *testing.T) {
	fixture := newContinuationReplayFixture(t, 103)
	startContinuationReplayWriter(t, fixture.replacement.writer)

	sendStarted := make(chan struct{})
	releaseSend := make(chan struct{})
	var sendStartedOnce sync.Once
	var releaseOnce sync.Once
	fixture.preferredChannel.onSend = func(framechannel.Frame) {
		sendStartedOnce.Do(func() { close(sendStarted) })
		<-releaseSend
	}
	startContinuationReplayWriter(t, fixture.preferred.writer)
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseSend) }) })

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan continuationSendResult, 1)
	go func() {
		disposition, err := fixture.rpc.SendContinuation(
			ctx, fixture.call, protocolsession.MessagePeerCandidate, []byte{0xf6},
		)
		result <- continuationSendResult{disposition: disposition, err: err}
	}()
	select {
	case <-sendStarted:
	case <-time.After(continuationReplayTestTimeout):
		t.Fatal("preferred writer did not reach admitted transport send")
	}
	cancel()

	select {
	case sent := <-result:
		if sent.disposition != protocolsession.OperationDeliver ||
			!errors.Is(sent.err, context.Canceled) || !errors.Is(sent.err, errContinuationNotDelivered) {
			t.Fatalf("admitted unsettled continuation = disposition %d, error %v", sent.disposition, sent.err)
		}
	case <-time.After(continuationReplayTestTimeout):
		t.Fatal("admitted continuation did not return its unsettled result")
	}
	if got := fixture.replacementChannel.sends.Load(); got != 0 {
		t.Fatalf("admitted Unknown retried %d replacement-lane frame(s)", got)
	}
	fixture.call.stateMu.Lock()
	replayRetained := fixture.call.hasReplay
	fixture.call.stateMu.Unlock()
	if !replayRetained || fixture.call.lane != fixture.runtime.initial {
		t.Fatalf("admitted Unknown migrated or consumed request replay: retained=%t lane=%+v",
			replayRetained, fixture.call.lane)
	}
	releaseOnce.Do(func() { close(releaseSend) })
}

type countingMemoryChannel struct {
	*memoryChannel
	sends atomic.Int32
}

func (channel *countingMemoryChannel) Send(ctx context.Context, frame framechannel.Frame) error {
	channel.sends.Add(1)
	return channel.memoryChannel.Send(ctx, frame)
}

type continuationReplayFixture struct {
	t                   *testing.T
	runtime             *Runtime
	rpc                 *Requests
	call                *Call
	preferred           selectedLane
	preferredChannel    *memoryChannel
	replacement         *RuntimeLane
	replacementChannel  *countingMemoryChannel
	replacementPeer     *memoryChannel
	replacementEnvelope *protocolsession.EnvelopeOpener
}

func newContinuationReplayFixture(t *testing.T, operationByte byte) *continuationReplayFixture {
	return newContinuationReplayFixtureWithContinuations(t, operationByte, nil)
}

func newTrackedContinuationReplayFixture(t *testing.T, operationByte byte) *continuationReplayFixture {
	return newContinuationReplayFixtureWithContinuations(t, operationByte, continuationReplayClassifier{})
}

func newContinuationReplayFixtureWithContinuations(
	t *testing.T,
	operationByte byte,
	continuations protocolsession.OperationContinuationClassifier,
) *continuationReplayFixture {
	return newContinuationReplayFixtureWithOptions(t, operationByte, protocolsession.OperationLimits{}, continuations)
}

func newContinuationReplayFixtureWithOptions(
	t *testing.T,
	operationByte byte,
	limits protocolsession.OperationLimits,
	continuations protocolsession.OperationContinuationClassifier,
) *continuationReplayFixture {
	t.Helper()
	runtime, preferredChannel := newUnstartedRuntimeWithContinuations(
		t, protocolsession.RoleReceiver, limits, nil, continuations,
	)
	preferred, err := runtime.lanes.selectLane(&runtime.initial)
	if err != nil {
		t.Fatal(err)
	}

	replacementBase, replacementPeer := newMemoryChannelPair()
	replacementChannel := &countingMemoryChannel{memoryChannel: replacementBase}
	replacementIdentity := session.LaneIdentity{ID: runtime.initial.ID + 1, Epoch: 1}
	replacement, err := runtime.lanes.add(
		replacementIdentity, replacementChannel, permissiveInboundAuthenticator(), false,
	)
	if err != nil {
		_ = replacementPeer.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = replacementPeer.Close() })
	binding := protocolsession.EnvelopeBinding{
		ShareInstance: runtime.share, ProtocolSessionID: runtime.sessionID,
		LaneID: replacementIdentity.ID, LaneEpoch: replacementIdentity.Epoch,
		Direction: protocolsession.DirectionReceiverToSender,
	}
	replacementEnvelope, err := protocolsession.NewEnvelopeOpener(
		trafficKey(runtime.keys, protocolsession.DirectionReceiverToSender), binding,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(replacementEnvelope.Destroy)

	rpc := NewRequests(runtime)
	operationID := protocolsession.OperationID(operationByte)
	offer, err := protocolsession.NewMessage(protocolsession.MessagePeerOffer, &operationID, []byte{0xf6})
	if err != nil {
		t.Fatal(err)
	}
	op, maxCont, hasLimit, release, err := runtime.operations.AdmitOutboundRequest(offer, nil)
	if err != nil {
		t.Fatalf("seed ambiguous request replay: error=%v", err)
	}
	release(false)
	call := &Call{
		id: operationID, messages: make(chan operationResponse, 1), done: make(chan struct{}),
		lane: runtime.initial,
	}
	if !call.setAuthority(op, maxCont, hasLimit) {
		t.Fatal("seed continuation call authority")
	}
	call.setRequestReplay(offer)
	rpc.calls[operationID] = call
	t.Cleanup(func() { rpc.End(call) })
	return &continuationReplayFixture{
		t: t, runtime: runtime, rpc: rpc, call: call,
		preferred: preferred, preferredChannel: preferredChannel,
		replacement: replacement, replacementChannel: replacementChannel,
		replacementPeer: replacementPeer, replacementEnvelope: replacementEnvelope,
	}
}

func (fixture *continuationReplayFixture) replacePreferredWriter(
	t *testing.T,
	sealer envelopeSealer,
) {
	t.Helper()
	writer, err := newLaneWriter(fixture.preferred.channel, sealer)
	if err != nil {
		t.Fatal(err)
	}
	fixture.runtime.lanes.mu.Lock()
	lane := fixture.runtime.lanes.active[fixture.preferred.identity.ID]
	if lane == nil {
		fixture.runtime.lanes.mu.Unlock()
		t.Fatal("preferred runtime lane is unavailable")
	}
	lane.writer = writer
	fixture.runtime.lanes.mu.Unlock()
	fixture.preferred.writer = writer
}

type continuationReplayClassifier struct{}

func (continuationReplayClassifier) BeginOperationContinuation(
	kind protocolsession.MessageKind,
	body []byte,
) (protocolsession.OperationContinuationContract, bool, error) {
	if kind != protocolsession.MessagePeerOffer {
		return nil, false, nil
	}
	if !bytes.Equal(body, []byte{0xf6}) {
		return nil, true, protocolsession.ErrInvalidMessage
	}
	return continuationReplayAuthority{}, true, nil
}

func (continuationReplayClassifier) ClassifyUnboundOperationContinuation(
	kind protocolsession.MessageKind,
	body []byte,
) (protocolsession.OperationContinuationScope, bool, error) {
	if kind != protocolsession.MessagePeerCandidate {
		return protocolsession.OperationContinuationScope{}, false, nil
	}
	if !bytes.Equal(body, []byte{0xf6}) {
		return protocolsession.OperationContinuationScope{}, true, protocolsession.ErrInvalidMessage
	}
	return continuationReplayScope(), true, nil
}

type continuationReplayAuthority struct{}

func (continuationReplayAuthority) ClassifyOperationContinuation(
	kind protocolsession.MessageKind,
	body []byte,
) ([sha256.Size]byte, bool, error) {
	if kind != protocolsession.MessagePeerCandidate {
		return [sha256.Size]byte{}, false, nil
	}
	if !bytes.Equal(body, []byte{0xf6}) {
		return [sha256.Size]byte{}, true, protocolsession.ErrInvalidMessage
	}
	return sha256.Sum256(body), true, nil
}

func (continuationReplayAuthority) OperationContinuationScope() protocolsession.OperationContinuationScope {
	return continuationReplayScope()
}

func (continuationReplayAuthority) MaximumContinuations() int { return 4 }

func continuationReplayScope() protocolsession.OperationContinuationScope {
	return protocolsession.OperationContinuationScope(sha256.Sum256([]byte("continuation-replay-test")))
}

func (fixture *continuationReplayFixture) assertReplacementOrder() {
	fixture.t.Helper()
	received := fixture.replacementPeer.Recv()
	wantKinds := []protocolsession.MessageKind{
		protocolsession.MessagePeerOffer,
		protocolsession.MessagePeerCandidate,
	}
	for index, wantKind := range wantKinds {
		select {
		case frame := <-received:
			opened, err := fixture.replacementEnvelope.Open(frame)
			if err != nil {
				fixture.t.Fatalf("open replacement frame %d: %v", index, err)
			}
			message, err := protocolsession.DecodeMessage(opened.Plaintext)
			if err != nil {
				fixture.t.Fatalf("decode replacement frame %d: %v", index, err)
			}
			operationID, ok := message.OperationID()
			if !ok || operationID != fixture.call.id || message.Kind() != wantKind {
				fixture.t.Fatalf("replacement frame %d = kind %d operation %x", index, message.Kind(), operationID)
			}
		case <-time.After(continuationReplayTestTimeout):
			fixture.t.Fatalf("replacement frame %d was not physically sent", index)
		}
	}
	if got := fixture.replacementChannel.sends.Load(); got != int32(len(wantKinds)) {
		fixture.t.Fatalf("replacement lane sent %d frames; want one request replay and one continuation", got)
	}
	select {
	case frame := <-received:
		fixture.t.Fatalf("replacement lane emitted an extra frame (%d bytes)", len(frame))
	default:
	}
	fixture.call.stateMu.Lock()
	replayRetained := fixture.call.hasReplay
	fixture.call.stateMu.Unlock()
	if !replayRetained || fixture.call.lane != fixture.replacement.identity {
		fixture.t.Fatalf("replacement commit retained replay=%t lane=%+v, want %+v",
			replayRetained, fixture.call.lane, fixture.replacement.identity)
	}
}

type continuationSendResult struct {
	disposition protocolsession.OperationDisposition
	err         error
}

type continuationAwaitBarrier struct {
	context.Context
	awaited chan struct{}
	once    sync.Once
}

func newContinuationAwaitBarrier() *continuationAwaitBarrier {
	return &continuationAwaitBarrier{Context: context.Background(), awaited: make(chan struct{})}
}

func (ctx *continuationAwaitBarrier) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.awaited) })
	return ctx.Context.Done()
}

func stopWriterBeforeAdmission(t *testing.T, writer *laneWriter) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := writer.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop preferred writer: %v", err)
	}
}

type failingContinuationChannel struct {
	protocolsession.FrameChannel
	succeedFirst int32
	calls        atomic.Int32
	err          error
}

func (c *failingContinuationChannel) Send(ctx context.Context, frame framechannel.Frame) error {
	if c.calls.Add(1) <= c.succeedFirst {
		return c.FrameChannel.Send(ctx, frame)
	}
	return c.err
}

func (c *failingContinuationChannel) SendTerminal(ctx context.Context, frame framechannel.Frame) error {
	return c.Send(ctx, frame)
}

func TestRPCContinuationMixedNotSentThenUncertainClearsNotSentEvidence(t *testing.T) {
	fixture := newContinuationReplayFixture(t, 108)
	pretransportErr := errors.New("preferred lane rejected before transport")
	uncertainErr := errors.New("replacement lane acceptance unknown")

	preferredCh := &failingContinuationChannel{
		FrameChannel: fixture.preferred.channel,
		err:          framechannel.RejectSend(pretransportErr),
	}
	preferredWriter, err := newLaneWriter(preferredCh, fixture.runtime.lanes.active[fixture.preferred.identity.ID].sealer)
	if err != nil {
		t.Fatal(err)
	}
	// Let the replayed PeerOffer succeed on the replacement lane so the retried
	// PeerCandidate itself reaches the transport and returns an uncertain error.
	replacementCh := &failingContinuationChannel{
		FrameChannel: fixture.replacement.channel,
		succeedFirst: 1,
		err:          uncertainErr,
	}
	replacementWriter, err := newLaneWriter(replacementCh, fixture.runtime.lanes.active[fixture.replacement.identity.ID].sealer)
	if err != nil {
		t.Fatal(err)
	}
	fixture.runtime.lanes.mu.Lock()
	fixture.runtime.lanes.active[fixture.preferred.identity.ID].writer = preferredWriter
	fixture.runtime.lanes.active[fixture.replacement.identity.ID].writer = replacementWriter
	fixture.runtime.lanes.mu.Unlock()
	startContinuationReplayWriter(t, preferredWriter)
	startContinuationReplayWriter(t, replacementWriter)

	disposition, sendErr := fixture.rpc.SendContinuation(
		context.Background(), fixture.call, protocolsession.MessagePeerCandidate, []byte{0xf6},
	)
	if disposition != protocolsession.OperationDeliver || sendErr == nil {
		t.Fatalf("mixed continuation = (disposition=%d, err=%v), want (OperationDeliver, non-nil)", disposition, sendErr)
	}
	if protocolsession.IsNotSent(sendErr) || errors.Is(sendErr, protocolsession.ErrNotSent) {
		t.Fatalf("mixed continuation leaked not-sent classification: %v", sendErr)
	}
	if !errors.Is(sendErr, pretransportErr) || !errors.Is(sendErr, uncertainErr) {
		t.Fatalf("mixed continuation lost attempt causes: %v", sendErr)
	}
}

type blockingFailingContinuationChannel struct {
	protocolsession.FrameChannel
	enteredOnce sync.Once
	entered     chan struct{}
	release     chan struct{}
	err         error
}

func (c *blockingFailingContinuationChannel) Send(_ context.Context, _ framechannel.Frame) error {
	c.enteredOnce.Do(func() { close(c.entered) })
	<-c.release
	return c.err
}

func (c *blockingFailingContinuationChannel) SendTerminal(ctx context.Context, frame framechannel.Frame) error {
	return c.Send(ctx, frame)
}

func TestRPCContinuationCancelInFlightPreAcceptanceFailureReleasesPin(t *testing.T) {
	tests := []struct {
		name      string
		opByte    byte
		wireFault func(t *testing.T, fixture *continuationReplayFixture) (entered <-chan struct{}, release func())
	}{
		{
			name:   "seal failure",
			opByte: 109,
			wireFault: func(t *testing.T, fixture *continuationReplayFixture) (<-chan struct{}, func()) {
				blocker := &continuationBlockingFailingSealer{
					entered: make(chan struct{}),
					release: make(chan struct{}),
					err:     errors.New("in-flight candidate seal failed"),
				}
				fixture.replacePreferredWriter(t, blocker)
				var once sync.Once
				release := func() { once.Do(func() { close(blocker.release) }) }
				t.Cleanup(release)
				return blocker.entered, release
			},
		},
		{
			name:   "pre-acceptance sendFrame rejection",
			opByte: 110,
			wireFault: func(t *testing.T, fixture *continuationReplayFixture) (<-chan struct{}, func()) {
				blockingCh := &blockingFailingContinuationChannel{
					FrameChannel: fixture.preferred.channel,
					entered:      make(chan struct{}),
					release:      make(chan struct{}),
					err:          framechannel.RejectSend(errors.New("pre-acceptance channel rejection")),
				}
				fixture.runtime.lanes.mu.Lock()
				preferredLane := fixture.runtime.lanes.active[fixture.preferred.identity.ID]
				sealer := preferredLane.sealer
				fixture.runtime.lanes.mu.Unlock()
				writer, err := newLaneWriter(blockingCh, sealer)
				if err != nil {
					t.Fatal(err)
				}
				fixture.runtime.lanes.mu.Lock()
				preferredLane.writer = writer
				fixture.runtime.lanes.mu.Unlock()
				fixture.preferred.writer = writer
				var once sync.Once
				release := func() { once.Do(func() { close(blockingCh.release) }) }
				t.Cleanup(release)
				return blockingCh.entered, release
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newContinuationReplayFixtureWithOptions(
				t,
				tc.opByte,
				protocolsession.OperationLimits{MaxActive: 4, MaxRecentTombstones: 1},
				continuationReplayClassifier{},
			)
			entered, release := tc.wireFault(t, fixture)
			startContinuationReplayWriter(t, fixture.preferred.writer)

			ctx, cancel := context.WithCancel(context.Background())
			resultCh := make(chan continuationSendResult, 1)
			go func() {
				disposition, err := fixture.rpc.SendContinuation(
					ctx, fixture.call, protocolsession.MessagePeerCandidate, []byte{0xf6},
				)
				resultCh <- continuationSendResult{disposition: disposition, err: err}
			}()

			select {
			case <-entered:
			case <-time.After(continuationReplayTestTimeout):
				t.Fatal("continuation did not enter writeInFlight delivery")
			}
			cancel()

			select {
			case sent := <-resultCh:
				if sent.disposition != protocolsession.OperationDeliver ||
					!errors.Is(sent.err, context.Canceled) || protocolsession.IsNotSent(sent.err) {
					t.Fatalf("cancel-in-flight continuation = (disp=%d, err=%v), want uncertain canceled delivery",
						sent.disposition, sent.err)
				}
			case <-time.After(continuationReplayTestTimeout):
				t.Fatal("canceled in-flight continuation did not return")
			}

			op1 := fixture.call.Operation()
			if transitioned, err := fixture.runtime.operations.CancelOperation(op1.ID()); err != nil || !transitioned {
				t.Fatalf("cancel op1 = (%t, %v)", transitioned, err)
			}

			// Admit and retire a second pinned op so tombstones exceed MaxRecentTombstones=1.
			// While op1's in-flight write is still running, op1 must remain pinned.
			op2ID := op1.ID() + 10
			op2Msg, err := protocolsession.NewMessage(protocolsession.MessageListChildren, &op2ID, []byte{0xa0})
			if err != nil {
				t.Fatal(err)
			}
			op2, _, _, releaseOp2, err := fixture.runtime.operations.AdmitOutboundRequest(op2Msg, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer releaseOp2(false)
			if transitioned, err := fixture.runtime.operations.CancelOperation(op2.ID()); err != nil || !transitioned {
				t.Fatalf("cancel op2 = (%t, %v)", transitioned, err)
			}
			if !op1.IsCurrent() || !op2.IsCurrent() || fixture.runtime.operations.TombstoneCount() != 2 {
				t.Fatalf("in-flight op1 was unpinned early: op1Current=%t op2Current=%t tombstones=%d",
					op1.IsCurrent(), op2.IsCurrent(), fixture.runtime.operations.TombstoneCount())
			}

			// Let the in-flight write fail pre-acceptance (onDone(false)); attemptPin
			// must still release and evict the overflowing op1 tombstone.
			release()
			select {
			case <-fixture.preferred.writer.Done():
			case <-time.After(continuationReplayTestTimeout):
				t.Fatal("preferred writer did not settle after pre-acceptance failure")
			}
			if op1.IsCurrent() || !op2.IsCurrent() || fixture.runtime.operations.TombstoneCount() != 1 {
				t.Fatalf("op1 leaked inFlight pin after pre-acceptance failure: op1Current=%t op2Current=%t tombstones=%d",
					op1.IsCurrent(), op2.IsCurrent(), fixture.runtime.operations.TombstoneCount())
			}
		})
	}
}

func TestRPCCancelOnReplacementLaneDoesNotReplayCancelledRequest(t *testing.T) {
	cancelBody, err := contentflow.EncodeCancelReason(contentflow.CancelReasonOutputAbort)
	if err != nil {
		t.Fatal(err)
	}

	assertSingleCancelOnly := func(t *testing.T, fixture *continuationReplayFixture) {
		t.Helper()
		select {
		case frame := <-fixture.replacementPeer.Recv():
			opened, openErr := fixture.replacementEnvelope.Open(frame)
			if openErr != nil {
				t.Fatalf("open replacement cancel frame: %v", openErr)
			}
			msg, decErr := protocolsession.DecodeMessage(opened.Plaintext)
			if decErr != nil {
				t.Fatalf("decode replacement cancel frame: %v", decErr)
			}
			opID, ok := msg.OperationID()
			if !ok || opID != fixture.call.id || msg.Kind() != protocolsession.MessageCancel {
				t.Fatalf("replacement frame = (kind=%d, op=%d), want (MessageCancel, %d)", msg.Kind(), opID, fixture.call.id)
			}
		case <-time.After(continuationReplayTestTimeout):
			t.Fatal("replacement lane did not transmit MessageCancel")
		}
		if got := fixture.replacementChannel.sends.Load(); got != 1 {
			t.Fatalf("replacement lane sent %d frames; want only MessageCancel without request replay", got)
		}
		if lane := fixture.call.currentLane(); lane != fixture.runtime.initial {
			t.Fatalf("best-effort cancel overwrote call.lane = %+v, want %+v", lane, fixture.runtime.initial)
		}
	}

	t.Run("CancelAndEnd after preferred lane stopped", func(t *testing.T) {
		fixture := newContinuationReplayFixture(t, 111)
		stopWriterBeforeAdmission(t, fixture.preferred.writer)
		startContinuationReplayWriter(t, fixture.replacement.writer)

		if cancelErr := fixture.rpc.CancelAndEnd(fixture.call, contentflow.CancelReasonOutputAbort); cancelErr != nil {
			t.Fatalf("CancelAndEnd: %v", cancelErr)
		}
		assertSingleCancelOnly(t, fixture)
	})

	t.Run("SendContinuation MessageCancel after preferred lane closing", func(t *testing.T) {
		fixture := newContinuationReplayFixture(t, 112)
		fixture.runtime.lanes.markSelectedClosing(fixture.preferred)
		startContinuationReplayWriter(t, fixture.replacement.writer)

		disposition, sendErr := fixture.rpc.SendContinuation(
			context.Background(), fixture.call, protocolsession.MessageCancel, cancelBody,
		)
		if sendErr != nil || disposition != protocolsession.OperationDeliver {
			t.Fatalf("SendContinuation(MessageCancel) = (%d, %v), want (OperationDeliver, nil)", disposition, sendErr)
		}
		assertSingleCancelOnly(t, fixture)
	})

	t.Run("replacement lane with single control queue slot left", func(t *testing.T) {
		fixture := newContinuationReplayFixture(t, 113)
		fixture.runtime.lanes.markSelectedClosing(fixture.preferred)

		fillID := protocolsession.OperationID(900)
		filler, fillErr := protocolsession.NewMessage(protocolsession.MessageListChildren, &fillID, []byte{0xa0})
		if fillErr != nil {
			t.Fatal(fillErr)
		}
		for i := range protocolsession.ControlQueueFrameLimit - 1 {
			if eqErr := fixture.replacement.writer.EnqueueControl(filler, nil); eqErr != nil {
				t.Fatalf("fill control queue slot %d: %v", i, eqErr)
			}
		}

		if cancelErr := fixture.rpc.CancelAndEnd(fixture.call, contentflow.CancelReasonOutputAbort); cancelErr != nil {
			t.Fatalf("CancelAndEnd with 1 queue slot left: %v", cancelErr)
		}
		startContinuationReplayWriter(t, fixture.replacement.writer)

		var lastMsg protocolsession.Message
		for i := range protocolsession.ControlQueueFrameLimit {
			select {
			case frame := <-fixture.replacementPeer.Recv():
				opened, openErr := fixture.replacementEnvelope.Open(frame)
				if openErr != nil {
					t.Fatalf("open frame %d: %v", i, openErr)
				}
				msg, decErr := protocolsession.DecodeMessage(opened.Plaintext)
				if decErr != nil {
					t.Fatalf("decode frame %d: %v", i, decErr)
				}
				if opID, ok := msg.OperationID(); ok && opID == fixture.call.id && msg.Kind() != protocolsession.MessageCancel {
					t.Fatalf("cancelled request was replayed at frame %d: kind=%d", i, msg.Kind())
				}
				lastMsg = msg
			case <-time.After(continuationReplayTestTimeout):
				t.Fatalf("timed out draining frame %d", i)
			}
		}
		opID, ok := lastMsg.OperationID()
		if !ok || opID != fixture.call.id || lastMsg.Kind() != protocolsession.MessageCancel {
			t.Fatalf("final queued slot = (kind=%d, op=%d), want (MessageCancel, %d)", lastMsg.Kind(), opID, fixture.call.id)
		}
	})
}

func startContinuationReplayWriter(t *testing.T, writer *laneWriter) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- writer.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(continuationReplayTestTimeout):
			t.Error("continuation test writer did not stop")
		}
	})
}
