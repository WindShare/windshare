package sessionruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/protocolsession"
)

const continuationReplayTestTimeout = time.Second

func TestRPCContinuationRetriesStoppedPreferredWriterAcrossLane(t *testing.T) {
	fixture := newContinuationReplayFixture(t, 101)
	stopWriterBeforeAdmission(t, fixture.preferred.writer)
	startContinuationReplayWriter(t, fixture.replacement.writer)

	outcome, err := fixture.rpc.sendContinuation(
		context.Background(), fixture.call, protocolsession.MessagePeerCandidate, []byte{0xf6},
	)
	if err != nil || outcome != protocolsession.SendOutcomeDelivered {
		t.Fatalf("continuation after stopped preferred writer = outcome %d, error %v", outcome, err)
	}
	fixture.assertReplacementOrder()
}

func TestRPCContinuationRetriesStaleDrainedPreadmissionDropAcrossLane(t *testing.T) {
	fixture := newContinuationReplayFixture(t, 102)
	startContinuationReplayWriter(t, fixture.replacement.writer)
	waitContext := newContinuationAwaitBarrier()
	result := make(chan continuationSendResult, 1)
	go func() {
		outcome, err := fixture.rpc.sendContinuation(
			waitContext, fixture.call, protocolsession.MessagePeerCandidate, []byte{0xf6},
		)
		result <- continuationSendResult{outcome: outcome, err: err}
	}()

	select {
	case <-waitContext.awaited:
	case <-time.After(continuationReplayTestTimeout):
		t.Fatal("continuation did not reach receipt settlement")
	}
	// Waiting for Await's Done observation proves the item was published before
	// stopping the writer, so this exercises stale queue drain rather than the
	// simpler stopped-writer rejection at TryAuthorizedControl.
	stopWriterBeforeAdmission(t, fixture.preferred.writer)

	select {
	case sent := <-result:
		if sent.err != nil || sent.outcome != protocolsession.SendOutcomeDelivered {
			t.Fatalf("continuation after stale queue drain = outcome %d, error %v", sent.outcome, sent.err)
		}
	case <-time.After(continuationReplayTestTimeout):
		t.Fatal("continuation did not retry after stale queue drain")
	}
	fixture.assertReplacementOrder()
}

func TestRPCContinuationRetriesClaimedPretransportFailureAcrossLane(t *testing.T) {
	fixture := newContinuationReplayFixture(t, 104)
	pretransportErr := errors.New("candidate sequence preflight failed")
	failingWriter, err := protocolsession.NewSessionWriter(
		fixture.preferred.channel,
		continuationPretransportFailingSealer{err: pretransportErr},
		fixture.runtime.router,
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

	outcome, err := fixture.rpc.sendContinuation(
		context.Background(), fixture.call, protocolsession.MessagePeerCandidate, []byte{0xf6},
	)
	if err != nil || outcome != protocolsession.SendOutcomeDelivered {
		t.Fatalf("continuation after pretransport failure = outcome %d, error %v", outcome, err)
	}
	fixture.assertReplacementOrder()
}

func TestRPCContinuationGateSerializesRollbackBeforeExactRetry(t *testing.T) {
	fixture := newTrackedContinuationReplayFixture(t, 105)
	blocker := &continuationBlockingSealer{
		base: fixture.preferredRuntimeSealer(), entered: make(chan struct{}), release: make(chan struct{}),
	}
	fixture.replacePreferredWriter(t, blocker)
	startContinuationReplayWriter(t, fixture.preferred.writer)
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(blocker.release) }) })
	var physicalSends atomic.Int32
	fixture.preferredChannel.onSend = func(framechannel.Frame) { physicalSends.Add(1) }

	firstContext, cancelFirst := context.WithCancel(context.Background())
	first := make(chan continuationSendResult, 1)
	go func() {
		outcome, err := fixture.rpc.sendContinuation(
			firstContext, fixture.call, protocolsession.MessagePeerCandidate, []byte{0xf6},
		)
		first <- continuationSendResult{outcome: outcome, err: err}
	}()
	select {
	case <-blocker.entered:
	case <-time.After(continuationReplayTestTimeout):
		t.Fatal("first candidate did not reach claimed sequence preflight")
	}

	secondContext := newContinuationAwaitBarrier()
	second := make(chan continuationSendResult, 1)
	go func() {
		outcome, err := fixture.rpc.sendContinuation(
			secondContext, fixture.call, protocolsession.MessagePeerCandidate, []byte{0xf6},
		)
		second <- continuationSendResult{outcome: outcome, err: err}
	}()
	select {
	case <-secondContext.awaited:
	case <-time.After(continuationReplayTestTimeout):
		t.Fatal("second candidate did not reach the operation gate")
	}
	select {
	case result := <-second:
		t.Fatalf("second candidate escaped pending owner: outcome %d, error %v", result.outcome, result.err)
	default:
	}

	cancelFirst()
	select {
	case result := <-first:
		if result.outcome != protocolsession.SendOutcomeDropped ||
			!errors.Is(result.err, context.Canceled) || !errors.Is(result.err, errContinuationNotDelivered) {
			t.Fatalf("canceled owner = outcome %d, error %v", result.outcome, result.err)
		}
	case <-time.After(continuationReplayTestTimeout):
		t.Fatal("canceled candidate owner did not settle")
	}
	releaseOnce.Do(func() { close(blocker.release) })
	select {
	case result := <-second:
		if result.outcome != protocolsession.SendOutcomeDelivered || result.err != nil {
			t.Fatalf("exact retry after rollback = outcome %d, error %v", result.outcome, result.err)
		}
	case <-time.After(continuationReplayTestTimeout):
		t.Fatal("exact retry did not publish after owner rollback")
	}
	if got := physicalSends.Load(); got != 1 {
		t.Fatalf("preferred lane physically sent %d candidate(s), want one", got)
	}
}

func TestRPCContinuationGateCoversAutomaticReplacementRetry(t *testing.T) {
	fixture := newTrackedContinuationReplayFixture(t, 106)
	pretransportErr := errors.New("blocked candidate sequence preflight failed")
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
		outcome, err := fixture.rpc.sendContinuation(
			context.Background(), fixture.call, protocolsession.MessagePeerCandidate, []byte{0xf6},
		)
		first <- continuationSendResult{outcome: outcome, err: err}
	}()
	select {
	case <-blocker.entered:
	case <-time.After(continuationReplayTestTimeout):
		t.Fatal("first candidate did not reach replacement-triggering preflight")
	}

	secondContext := newContinuationAwaitBarrier()
	second := make(chan continuationSendResult, 1)
	go func() {
		outcome, err := fixture.rpc.sendContinuation(
			secondContext, fixture.call, protocolsession.MessagePeerCandidate, []byte{0xf6},
		)
		second <- continuationSendResult{outcome: outcome, err: err}
	}()
	select {
	case <-secondContext.awaited:
	case <-time.After(continuationReplayTestTimeout):
		t.Fatal("second candidate did not reach the operation gate")
	}
	select {
	case result := <-second:
		t.Fatalf("second candidate escaped replacement retry owner: outcome %d, error %v", result.outcome, result.err)
	default:
	}

	releaseOnce.Do(func() { close(blocker.release) })
	select {
	case result := <-first:
		if result.outcome != protocolsession.SendOutcomeDelivered || result.err != nil {
			t.Fatalf("replacement retry owner = outcome %d, error %v", result.outcome, result.err)
		}
	case <-time.After(continuationReplayTestTimeout):
		t.Fatal("replacement retry owner did not settle")
	}
	select {
	case result := <-second:
		if result.outcome != protocolsession.SendOutcomeDropped || result.err != nil {
			t.Fatalf("exact waiter after committed retry = outcome %d, error %v", result.outcome, result.err)
		}
	case <-time.After(continuationReplayTestTimeout):
		t.Fatal("exact waiter did not observe committed replacement retry")
	}
	fixture.assertReplacementOrder()
}

func TestOperationCandidateGateCancellationDoesNotLeakToken(t *testing.T) {
	call := &operationCall{done: make(chan struct{})}
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

type continuationPretransportFailingSealer struct{ err error }

func (sealer continuationPretransportFailingSealer) NextSequence() (uint64, error) {
	return 0, sealer.err
}

func (continuationPretransportFailingSealer) Seal([]byte) (protocolsession.SealedEnvelope, error) {
	panic("Seal must not run after NextSequence failure")
}

type continuationBlockingSealer struct {
	base    protocolsession.OutboundEnvelopeSealer
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (sealer *continuationBlockingSealer) NextSequence() (uint64, error) {
	sealer.once.Do(func() {
		close(sealer.entered)
		<-sealer.release
	})
	return sealer.base.NextSequence()
}

func (sealer *continuationBlockingSealer) Seal(plaintext []byte) (protocolsession.SealedEnvelope, error) {
	return sealer.base.Seal(plaintext)
}

type continuationBlockingFailingSealer struct {
	entered chan struct{}
	release chan struct{}
	err     error
}

func (sealer *continuationBlockingFailingSealer) NextSequence() (uint64, error) {
	close(sealer.entered)
	<-sealer.release
	return 0, sealer.err
}

func (*continuationBlockingFailingSealer) Seal([]byte) (protocolsession.SealedEnvelope, error) {
	panic("Seal must not run after NextSequence failure")
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
	// Release precedes writer cleanup under testing.Cleanup's LIFO order, even if
	// an assertion aborts while the transport callback owns the send path.
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseSend) }) })

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan continuationSendResult, 1)
	go func() {
		outcome, err := fixture.rpc.sendContinuation(
			ctx, fixture.call, protocolsession.MessagePeerCandidate, []byte{0xf6},
		)
		result <- continuationSendResult{outcome: outcome, err: err}
	}()
	select {
	case <-sendStarted:
	case <-time.After(continuationReplayTestTimeout):
		t.Fatal("preferred writer did not reach admitted transport send")
	}
	cancel()

	select {
	case sent := <-result:
		if sent.outcome != protocolsession.SendOutcomeUnknown ||
			!errors.Is(sent.err, context.Canceled) || !errors.Is(sent.err, errContinuationNotDelivered) {
			t.Fatalf("admitted unsettled continuation = outcome %d, error %v", sent.outcome, sent.err)
		}
	case <-time.After(continuationReplayTestTimeout):
		t.Fatal("admitted continuation did not return its unsettled snapshot")
	}
	if got := fixture.replacementChannel.sends.Load(); got != 0 {
		t.Fatalf("admitted Unknown retried %d replacement-lane frame(s)", got)
	}
	fixture.call.stateMu.Lock()
	replayRetained := !fixture.call.replay.IsZero()
	fixture.call.stateMu.Unlock()
	if !replayRetained || fixture.call.lane != fixture.runtime.initial {
		t.Fatalf("admitted Unknown migrated or consumed request replay: retained=%t lane=%+v",
			replayRetained, fixture.call.lane)
	}
	releaseOnce.Do(func() { close(releaseSend) })
}

type continuationReplayFixture struct {
	t                   *testing.T
	runtime             *runtimeCore
	rpc                 *rpcClient
	call                *operationCall
	preferred           selectedLane
	preferredChannel    *memoryChannel
	replacement         *runtimeLane
	replacementChannel  *observedMemoryChannel
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
	t.Helper()
	runtime, preferredChannel := newUnstartedRuntimeWithContinuations(
		t, protocolsession.RoleReceiver, protocolsession.OperationLimits{}, nil, continuations,
	)
	preferred, err := runtime.lanes.selectLane(&runtime.initial)
	if err != nil {
		t.Fatal(err)
	}

	replacementBase, replacementPeer := newMemoryChannelPair()
	replacementChannel := &observedMemoryChannel{memoryChannel: replacementBase}
	replacementIdentity := LaneIdentity{ID: runtime.initial.ID + 1, Epoch: 1}
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

	rpc := newRPCClient(
		runtime, bytes.NewReader(bytes.Repeat([]byte{operationByte}, protocolsession.IdentityBytes)),
	)
	operationID := id16[protocolsession.OperationID](operationByte)
	offer, err := protocolsession.NewMessage(protocolsession.MessagePeerOffer, &operationID, []byte{0xf6})
	if err != nil {
		t.Fatal(err)
	}
	admission, err := runtime.router.AdmitOutbound(offer, protocolsession.OutboundOperationPermit{})
	if err != nil || admission.Replay.IsZero() {
		t.Fatalf("seed ambiguous request replay: admission=%+v, error=%v", admission, err)
	}
	call := &operationCall{
		id: operationID, messages: make(chan operationResponse, 1), done: make(chan struct{}),
		lane: runtime.initial,
	}
	if !call.setAuthority(admission.Generation, admission.Operation) ||
		!call.setRequestReplay(offer, admission.Replay) {
		t.Fatal("seed continuation call authority")
	}
	rpc.calls[operationID] = call
	t.Cleanup(func() { rpc.end(call) })
	return &continuationReplayFixture{
		t: t, runtime: runtime, rpc: rpc, call: call,
		preferred: preferred, preferredChannel: preferredChannel,
		replacement: replacement, replacementChannel: replacementChannel,
		replacementPeer: replacementPeer, replacementEnvelope: replacementEnvelope,
	}
}

func (fixture *continuationReplayFixture) preferredRuntimeSealer() protocolsession.OutboundEnvelopeSealer {
	fixture.t.Helper()
	fixture.runtime.lanes.mu.Lock()
	defer fixture.runtime.lanes.mu.Unlock()
	lane := fixture.runtime.lanes.active[fixture.preferred.identity.ID]
	if lane == nil || lane.sealer == nil {
		fixture.t.Fatal("preferred runtime sealer is unavailable")
	}
	return lane.sealer
}

func (fixture *continuationReplayFixture) replacePreferredWriter(
	t *testing.T,
	sealer protocolsession.OutboundEnvelopeSealer,
) {
	t.Helper()
	writer, err := protocolsession.NewSessionWriter(
		fixture.preferred.channel, sealer, runtimeLanePolicy{runtime: fixture.runtime},
	)
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
) (protocolsession.OperationContinuationAuthority, bool, error) {
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
	replayRetained := !fixture.call.replay.IsZero()
	fixture.call.stateMu.Unlock()
	if !replayRetained || fixture.call.lane != fixture.replacement.identity {
		fixture.t.Fatalf("replacement commit retained replay=%t lane=%+v, want %+v",
			replayRetained, fixture.call.lane, fixture.replacement.identity)
	}
}

type continuationSendResult struct {
	outcome protocolsession.SendOutcome
	err     error
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

func stopWriterBeforeAdmission(t *testing.T, writer *protocolsession.SessionWriter) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := writer.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop preferred writer: %v", err)
	}
}

func startContinuationReplayWriter(t *testing.T, writer *protocolsession.SessionWriter) {
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
