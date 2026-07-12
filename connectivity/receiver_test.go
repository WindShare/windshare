package connectivity

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session"
)

type recordingPool struct {
	mu       sync.Mutex
	channels []session.FrameChannel
	added    chan session.FrameChannel
	err      error
}

func newRecordingPool() *recordingPool {
	return &recordingPool{added: make(chan session.FrameChannel, 16)}
}

func (p *recordingPool) AddChannel(channel session.FrameChannel) error {
	p.mu.Lock()
	if p.err != nil {
		err := p.err
		p.mu.Unlock()
		return err
	}
	p.channels = append(p.channels, channel)
	p.mu.Unlock()
	p.added <- channel
	return nil
}

func (p *recordingPool) setErr(err error) {
	p.mu.Lock()
	p.err = err
	p.mu.Unlock()
}

type offerCall struct {
	ctx       context.Context
	signaling Signaling
	result    chan peerResult
}

type uncomparablePeerChannel struct {
	*fakePeerChannel
	marker []byte
}

func TestReceiverPoolAdmitsRelayThenOpenPeer(t *testing.T) {
	calls := make(chan offerCall, 1)
	factory := offerFactoryFunc(func(ctx context.Context, signaling Signaling) (PeerChannel, error) {
		call := offerCall{ctx: ctx, signaling: signaling, result: make(chan peerResult, 1)}
		calls <- call
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case result := <-call.result:
			return result.channel, result.err
		}
	})
	channelPool := newRecordingPool()
	receiver, err := NewReceiverPool(t.Context(), channelPool, factory, ReceiverPoolOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = receiver.Close() })
	relayChannel := newFakePeerChannel()
	signaling := &inertSignaling{id: "first"}
	if err := receiver.AddRelay(relayChannel, signaling); err != nil {
		t.Fatal(err)
	}
	if got := waitChannel(t, channelPool.added); got != relayChannel {
		t.Fatal("relay was not admitted first")
	}
	call := <-calls
	if call.signaling != signaling {
		t.Fatal("offer factory received the wrong signaling route")
	}
	peer := newFakePeerChannel()
	call.result <- peerResult{channel: peer}
	if got := waitChannel(t, channelPool.added); got != peer {
		t.Fatal("open P2P channel was not admitted")
	}
	peer.closeWithError(nil)
}

func TestReceiverPoolUsesRejoinedSignalingAfterOldPeerEnds(t *testing.T) {
	calls := make(chan offerCall, 2)
	factory := offerFactoryFunc(func(ctx context.Context, signaling Signaling) (PeerChannel, error) {
		call := offerCall{ctx: ctx, signaling: signaling, result: make(chan peerResult, 1)}
		calls <- call
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case result := <-call.result:
			return result.channel, result.err
		}
	})
	channelPool := newRecordingPool()
	receiver, err := NewReceiverPool(t.Context(), channelPool, factory, ReceiverPoolOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = receiver.Close() })
	firstSignals := &inertSignaling{id: "first"}
	secondSignals := &inertSignaling{id: "rejoin"}
	firstRelay := newFakePeerChannel()
	if err := receiver.AddRelay(firstRelay, firstSignals); err != nil {
		t.Fatal(err)
	}
	if got := waitChannel(t, channelPool.added); got != firstRelay {
		t.Fatal("first relay was not admitted")
	}
	firstCall := <-calls
	firstPeer := newFakePeerChannel()
	firstCall.result <- peerResult{channel: firstPeer}
	if got := waitChannel(t, channelPool.added); got != firstPeer {
		t.Fatal("first peer was not admitted")
	}
	rejoinRelay := newFakePeerChannel()
	if err := receiver.AddRelay(rejoinRelay, secondSignals); err != nil {
		t.Fatal(err)
	}
	if got := waitChannel(t, channelPool.added); got != rejoinRelay {
		t.Fatal("rejoined relay was not admitted")
	}
	select {
	case <-calls:
		t.Fatal("rejoin started a duplicate P2P path while the old path was healthy")
	case <-time.After(50 * time.Millisecond):
	}
	firstPeer.closeWithError(nil)
	var secondCall offerCall
	select {
	case secondCall = <-calls:
	case <-time.After(orchestrationTestTimeout):
		t.Fatal("old P2P closure did not use the rejoined signaling route")
	}
	if secondCall.signaling != secondSignals {
		t.Fatal("replacement P2P attempt used stale signaling")
	}
	secondPeer := newFakePeerChannel()
	secondCall.result <- peerResult{channel: secondPeer}
	if got := waitChannel(t, channelPool.added); got != secondPeer {
		t.Fatal("replacement P2P channel was not admitted")
	}
	secondPeer.closeWithError(nil)
}

func TestReceiverPoolClosesLateFactoryResultAfterCancellation(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	latePeer := newFakePeerChannel()
	factory := offerFactoryFunc(func(ctx context.Context, _ Signaling) (PeerChannel, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		return latePeer, nil
	})
	channelPool := newRecordingPool()
	receiver, err := NewReceiverPool(t.Context(), channelPool, factory, ReceiverPoolOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := receiver.AddRelay(newFakePeerChannel(), &inertSignaling{}); err != nil {
		t.Fatal(err)
	}
	waitChannel(t, channelPool.added)
	<-started
	closed := make(chan error, 1)
	go func() { closed <- receiver.Close() }()
	<-canceled
	close(release)
	if err := waitError(t, closed); err != nil {
		t.Fatal(err)
	}
	select {
	case <-latePeer.Done():
	case <-time.After(orchestrationTestTimeout):
		t.Fatal("late factory result was not closed")
	}
	select {
	case channel := <-channelPool.added:
		t.Fatalf("late channel was admitted after Close: %T", channel)
	default:
	}
}

func TestReceiverPoolReportsFactoryAndAdmissionFailures(t *testing.T) {
	wantFactoryErr := errors.New("offer failed")
	reported := make(chan error, 2)
	factory := offerFactoryFunc(func(context.Context, Signaling) (PeerChannel, error) {
		return nil, wantFactoryErr
	})
	channelPool := newRecordingPool()
	receiver, err := NewReceiverPool(t.Context(), channelPool, factory, ReceiverPoolOptions{
		OnPeerError: func(err error) { reported <- err },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	if err := receiver.AddRelay(newFakePeerChannel(), &inertSignaling{}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-reported:
		if !errors.Is(err, wantFactoryErr) {
			t.Fatalf("reported error = %v", err)
		}
	case <-time.After(orchestrationTestTimeout):
		t.Fatal("factory failure was not reported")
	}
}

func TestReceiverPoolClosesPeerRejectedByScheduler(t *testing.T) {
	wantErr := errors.New("scheduler already finished")
	peer := newFakePeerChannel()
	release := make(chan struct{})
	channelPool := newRecordingPool()
	reported := make(chan error, 1)
	receiver, err := NewReceiverPool(t.Context(), channelPool, offerFactoryFunc(
		func(context.Context, Signaling) (PeerChannel, error) {
			<-release
			return peer, nil
		},
	), ReceiverPoolOptions{OnPeerError: func(err error) { reported <- err }})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	if err := receiver.AddRelay(newFakePeerChannel(), &inertSignaling{}); err != nil {
		t.Fatal(err)
	}
	// Reject only the P2P admission: the relay above must be adopted first so
	// the offer attempt starts at all.
	channelPool.setErr(wantErr)
	close(release)
	select {
	case err := <-reported:
		if !errors.Is(err, wantErr) {
			t.Fatalf("reported admission error = %v", err)
		}
	case <-time.After(orchestrationTestTimeout):
		t.Fatal("scheduler rejection was not reported")
	}
	select {
	case <-peer.Done():
	case <-time.After(orchestrationTestTimeout):
		t.Fatal("scheduler-rejected peer was not closed")
	}
}

func TestReceiverPoolRejectsMissingDependenciesAndClosedUse(t *testing.T) {
	factory := offerFactoryFunc(func(context.Context, Signaling) (PeerChannel, error) {
		return newFakePeerChannel(), nil
	})
	var missingContext context.Context
	if _, err := NewReceiverPool(missingContext, newRecordingPool(), factory, ReceiverPoolOptions{}); !errors.Is(err, ErrNilDependency) {
		t.Fatalf("nil context error = %v", err)
	}
	receiver, err := NewReceiverPool(t.Context(), newRecordingPool(), factory, ReceiverPoolOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := receiver.Close(); err != nil {
		t.Fatal(err)
	}
	if err := receiver.AddRelay(newFakePeerChannel(), &inertSignaling{}); !errors.Is(err, ErrReceiverPoolClosed) {
		t.Fatalf("AddRelay after Close = %v", err)
	}
}

func TestReceiverPoolClosesLateFactoryResultAfterParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	latePeer := newFakePeerChannel()
	started := make(chan struct{})
	factory := offerFactoryFunc(func(callCtx context.Context, _ Signaling) (PeerChannel, error) {
		close(started)
		<-callCtx.Done()
		return latePeer, nil
	})
	channelPool := newRecordingPool()
	receiver, err := NewReceiverPool(ctx, channelPool, factory, ReceiverPoolOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	if err := receiver.AddRelay(newFakePeerChannel(), &inertSignaling{}); err != nil {
		t.Fatal(err)
	}
	waitChannel(t, channelPool.added)
	<-started
	cancel()
	select {
	case <-latePeer.Done():
	case <-time.After(orchestrationTestTimeout):
		t.Fatal("parent cancellation leaked a late factory channel")
	}
	select {
	case channel := <-channelPool.added:
		t.Fatalf("parent cancellation admitted late channel %T", channel)
	default:
	}
}

func TestReceiverPoolDoesNotAdmitRelayAfterParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	channelPool := newRecordingPool()
	receiver, err := NewReceiverPool(ctx, channelPool, offerFactoryFunc(
		func(context.Context, Signaling) (PeerChannel, error) { return newFakePeerChannel(), nil },
	), ReceiverPoolOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	if err := receiver.AddRelay(newFakePeerChannel(), &inertSignaling{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("AddRelay after parent cancellation = %v", err)
	}
	select {
	case channel := <-channelPool.added:
		t.Fatalf("canceled pool admitted relay %T", channel)
	default:
	}
}

func TestReceiverPoolTracksUncomparablePeerWithoutInterfaceEquality(t *testing.T) {
	base := newFakePeerChannel()
	peer := uncomparablePeerChannel{fakePeerChannel: base, marker: []byte("uncomparable")}
	channelPool := newRecordingPool()
	receiver, err := NewReceiverPool(t.Context(), channelPool, offerFactoryFunc(
		func(context.Context, Signaling) (PeerChannel, error) { return peer, nil },
	), ReceiverPoolOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	if err := receiver.AddRelay(newFakePeerChannel(), &inertSignaling{}); err != nil {
		t.Fatal(err)
	}
	waitChannel(t, channelPool.added)
	waitChannel(t, channelPool.added)
	if !receiver.PeerAvailable() {
		t.Fatal("admitted peer was not reported available")
	}
	base.closeWithError(nil)
	deadline := time.After(orchestrationTestTimeout)
	for receiver.PeerAvailable() {
		select {
		case <-receiver.PeerChanges():
		case <-deadline:
			t.Fatal("closed peer remained available")
		}
	}
}

func TestReceiverPoolReportsEstablishedPeerFailure(t *testing.T) {
	wantErr := errors.New("established P2P path failed")
	peer := newFakePeerChannel()
	reported := make(chan error, 1)
	channelPool := newRecordingPool()
	receiver, err := NewReceiverPool(t.Context(), channelPool, offerFactoryFunc(
		func(context.Context, Signaling) (PeerChannel, error) { return peer, nil },
	), ReceiverPoolOptions{OnPeerError: func(err error) { reported <- err }})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	if err := receiver.AddRelay(newFakePeerChannel(), &inertSignaling{}); err != nil {
		t.Fatal(err)
	}
	waitChannel(t, channelPool.added)
	waitChannel(t, channelPool.added)
	peer.closeWithError(wantErr)
	select {
	case err := <-reported:
		if !errors.Is(err, wantErr) {
			t.Fatalf("reported established peer error = %v", err)
		}
	case <-time.After(orchestrationTestTimeout):
		t.Fatal("established peer failure was not reported")
	}
}

func waitChannel(t *testing.T, channels <-chan session.FrameChannel) session.FrameChannel {
	t.Helper()
	select {
	case channel := <-channels:
		return channel
	case <-time.After(orchestrationTestTimeout):
		t.Fatal("timed out waiting for channel admission")
		return nil
	}
}
