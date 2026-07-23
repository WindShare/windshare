package webrtc

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/internal/testnetwork"
)

const pionLoopbackTimeout = 10 * time.Second

type pionChannelPair struct {
	left      *Channel
	right     *Channel
	leftRaw   *pion.DataChannel
	rightRaw  *pion.DataChannel
	leftPeer  *pion.PeerConnection
	rightPeer *pion.PeerConnection
}

func TestPionLoopbackPreservesMaximumFramesAndTerminal(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	pair := newPionChannelPair(t)

	leftFrame := patternedLoopbackFrame(0x31, framechannel.MaxFrameSize)
	if err := pair.left.Send(context.Background(), leftFrame); err != nil {
		t.Fatalf("left Send maximum frame: %v", err)
	}
	if got := receiveLoopbackFrame(t, pair.right); !bytes.Equal(got, leftFrame) {
		t.Fatal("left-to-right maximum frame changed")
	}

	rightFrame := patternedLoopbackFrame(0x32, framechannel.MaxFrameSize)
	if err := pair.right.Send(context.Background(), rightFrame); err != nil {
		t.Fatalf("right Send maximum frame: %v", err)
	}
	if got := receiveLoopbackFrame(t, pair.left); !bytes.Equal(got, rightFrame) {
		t.Fatal("right-to-left maximum frame changed")
	}

	terminal := patternedLoopbackFrame(0x33, 257)
	result := make(chan error, 1)
	go func() { result <- pair.left.SendTerminal(context.Background(), terminal) }()
	if got := receiveLoopbackFrame(t, pair.right); !bytes.Equal(got, terminal) {
		t.Fatal("terminal frame changed")
	}
	assertLoopbackRecvClosed(t, pair.right)
	if err := receiveLoopbackError(t, result); err != nil {
		t.Fatalf("SendTerminal: %v", err)
	}
	waitLoopbackDone(t, pair.left)
	waitLoopbackDone(t, pair.right)
	if pair.left.Err() != nil || pair.right.Err() != nil {
		t.Fatalf("terminal close errors: left=%v right=%v", pair.left.Err(), pair.right.Err())
	}
}

func TestPionLoopbackBackpressureCancellationAndRecovery(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	pair := newPionBlockedPeerPair(t)
	frame := patternedLoopbackFrame(0x41, framechannel.MaxFrameSize)
	if err := pair.channel.Send(context.Background(), frame); err != nil {
		t.Fatalf("send blocking probe: %v", err)
	}
	pair.waitHandlerEntered(t)

	ctx, cancel := context.WithCancel(context.Background())
	canceled := make(chan error, 1)
	go func() {
		for {
			if err := pair.channel.Send(ctx, frame); err != nil {
				canceled <- err
				return
			}
		}
	}()
	pair.waitHighWater(t)
	cancel()
	if err := receiveLoopbackError(t, canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Pion Send = %v, want context.Canceled", err)
	}

	recoveryHigh := pair.armHighWater()
	recovered := make(chan error, 1)
	go func() {
		for {
			if err := pair.channel.Send(context.Background(), frame); err != nil {
				recovered <- err
				return
			}
			select {
			case <-recoveryHigh:
				recovered <- nil
				return
			default:
			}
		}
	}()
	pair.waitHighWater(t, recoveryHigh)
	pair.releaseHandler()
	if err := receiveLoopbackError(t, recovered); err != nil {
		t.Fatalf("Pion Send after low-water recovery: %v", err)
	}
}

func TestPionLoopbackRemoteCloseWakesBackpressure(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	pair := newPionBlockedPeerPair(t)
	frame := patternedLoopbackFrame(0x51, framechannel.MaxFrameSize)
	if err := pair.channel.Send(context.Background(), frame); err != nil {
		t.Fatalf("send blocking probe: %v", err)
	}
	pair.waitHandlerEntered(t)

	result := make(chan error, 1)
	go func() {
		for {
			if err := pair.channel.Send(context.Background(), frame); err != nil {
				result <- err
				return
			}
		}
	}()
	pair.waitHighWater(t)
	if err := pair.remote.Close(); err != nil {
		t.Fatalf("close remote Pion DataChannel: %v", err)
	}
	pair.releaseHandler()
	if err := receiveLoopbackError(t, result); err == nil {
		t.Fatal("blocked Pion Send succeeded after remote close")
	}
	waitLoopbackDone(t, pair.channel)
}

func newPionChannelPair(t *testing.T) pionChannelPair {
	t.Helper()
	leftPeer := newPeerConnection(t)
	rightPeer := newPeerConnection(t)
	t.Cleanup(func() {
		_ = leftPeer.Close()
		_ = rightPeer.Close()
	})

	type remoteResult struct {
		channel *Channel
		raw     *pion.DataChannel
		err     error
	}
	remoteReady := make(chan remoteResult, 1)
	rightPeer.OnDataChannel(func(raw *pion.DataChannel) {
		channel, err := NewChannel(raw)
		remoteReady <- remoteResult{channel: channel, raw: raw, err: err}
	})

	leftRaw, err := leftPeer.CreateDataChannel(ChannelLabel, DefaultDataChannelInit())
	if err != nil {
		t.Fatalf("create left DataChannel: %v", err)
	}
	left, err := NewChannel(leftRaw)
	if err != nil {
		t.Fatalf("wrap left DataChannel: %v", err)
	}

	negotiatePeers(t, leftPeer, rightPeer)
	var remote remoteResult
	select {
	case remote = <-remoteReady:
	case <-time.After(pionLoopbackTimeout):
		t.Fatal("timeout waiting for remote DataChannel")
	}
	if remote.err != nil {
		t.Fatalf("wrap right DataChannel: %v", remote.err)
	}
	waitLoopbackOpened(t, left)
	waitLoopbackOpened(t, remote.channel)
	t.Cleanup(func() {
		_ = left.Close()
		_ = remote.channel.Close()
	})
	return pionChannelPair{
		left:      left,
		right:     remote.channel,
		leftRaw:   leftRaw,
		rightRaw:  remote.raw,
		leftPeer:  leftPeer,
		rightPeer: rightPeer,
	}
}

type pionBlockedPeerPair struct {
	channel  *Channel
	raw      *pion.DataChannel
	remote   *pion.DataChannel
	observer *highWaterObserver
	high     <-chan struct{}

	entered     chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
}

func newPionBlockedPeerPair(t *testing.T) *pionBlockedPeerPair {
	t.Helper()
	leftPeer := newPeerConnection(t)
	rightPeer := newPeerConnection(t)
	pair := &pionBlockedPeerPair{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	t.Cleanup(func() {
		pair.releaseHandler()
		if pair.channel != nil {
			_ = pair.channel.Close()
		}
		_ = leftPeer.Close()
		_ = rightPeer.Close()
	})

	type remoteResult struct {
		channel *pion.DataChannel
		err     error
	}
	remoteReady := make(chan remoteResult, 1)
	rightPeer.OnDataChannel(func(channel *pion.DataChannel) {
		var first sync.Once
		channel.OnMessage(func(pion.DataChannelMessage) {
			first.Do(func() {
				close(pair.entered)
				<-pair.release
			})
		})
		channel.OnOpen(func() { remoteReady <- remoteResult{channel: channel} })
		channel.OnError(func(err error) {
			select {
			case remoteReady <- remoteResult{err: err}:
			default:
			}
		})
	})

	leftRaw, err := leftPeer.CreateDataChannel(ChannelLabel, DefaultDataChannelInit())
	if err != nil {
		t.Fatalf("create sender DataChannel: %v", err)
	}
	observed := newHighWaterObserver(leftRaw, defaultHighWaterBytes)
	left, err := newChannel(observed, defaultFlowControl)
	if err != nil {
		t.Fatalf("wrap sender DataChannel: %v", err)
	}
	pair.channel = left
	pair.raw = leftRaw
	pair.observer = observed
	pair.high = observed.highSeen

	negotiatePeers(t, leftPeer, rightPeer)
	select {
	case result := <-remoteReady:
		if result.err != nil {
			t.Fatalf("remote DataChannel error: %v", result.err)
		}
		pair.remote = result.channel
	case <-time.After(pionLoopbackTimeout):
		t.Fatal("timeout waiting for raw remote DataChannel")
	}
	waitLoopbackOpened(t, left)
	return pair
}

func (p *pionBlockedPeerPair) waitHandlerEntered(t *testing.T) {
	t.Helper()
	select {
	case <-p.entered:
	case <-time.After(pionLoopbackTimeout):
		t.Fatal("remote Pion message handler was not entered")
	}
}

func (p *pionBlockedPeerPair) waitHighWater(t *testing.T, channels ...<-chan struct{}) {
	t.Helper()
	high := p.high
	if len(channels) == 1 {
		high = channels[0]
	}
	select {
	case <-high:
	case <-time.After(pionLoopbackTimeout):
		t.Fatal("Pion buffered amount did not reach the high-water boundary")
	}
}

func (p *pionBlockedPeerPair) armHighWater() <-chan struct{} {
	return p.observer.arm()
}

func (p *pionBlockedPeerPair) releaseHandler() {
	p.releaseOnce.Do(func() { close(p.release) })
}

func newPeerConnection(t *testing.T) *pion.PeerConnection {
	t.Helper()
	testnetwork.RequireOSNetwork(t)
	peer, err := pion.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatalf("create PeerConnection: %v", err)
	}
	return peer
}

func negotiatePeers(t *testing.T, offerer, answerer *pion.PeerConnection) {
	t.Helper()
	offer, err := offerer.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	offerGathered := pion.GatheringCompletePromise(offerer)
	if err := offerer.SetLocalDescription(offer); err != nil {
		t.Fatalf("set offerer local description: %v", err)
	}
	waitGathering(t, offerGathered, "offerer")
	if err := answerer.SetRemoteDescription(*offerer.LocalDescription()); err != nil {
		t.Fatalf("set answerer remote description: %v", err)
	}

	answer, err := answerer.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("create answer: %v", err)
	}
	answerGathered := pion.GatheringCompletePromise(answerer)
	if err := answerer.SetLocalDescription(answer); err != nil {
		t.Fatalf("set answerer local description: %v", err)
	}
	waitGathering(t, answerGathered, "answerer")
	if err := offerer.SetRemoteDescription(*answerer.LocalDescription()); err != nil {
		t.Fatalf("set offerer remote description: %v", err)
	}
}

func waitGathering(t *testing.T, done <-chan struct{}, peer string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(pionLoopbackTimeout):
		t.Fatalf("timeout waiting for %s ICE gathering", peer)
	}
}

func waitLoopbackOpened(t *testing.T, channel *Channel) {
	t.Helper()
	select {
	case <-channel.Opened():
	case <-channel.Done():
		t.Fatalf("Pion channel closed before opening: %v", channel.Err())
	case <-time.After(pionLoopbackTimeout):
		t.Fatal("timeout waiting for Pion channel open")
	}
}

func waitLoopbackDone(t *testing.T, channel *Channel) {
	t.Helper()
	select {
	case <-channel.Done():
	case <-time.After(pionLoopbackTimeout):
		t.Fatal("timeout waiting for Pion channel close")
	}
}

func receiveLoopbackFrame(t *testing.T, channel *Channel) framechannel.Frame {
	t.Helper()
	select {
	case frame, ok := <-channel.Recv():
		if !ok {
			t.Fatal("Pion Recv closed before expected frame")
		}
		return frame
	case <-time.After(pionLoopbackTimeout):
		t.Fatal("timeout waiting for Pion frame")
		return nil
	}
}

func assertLoopbackRecvClosed(t *testing.T, channel *Channel) {
	t.Helper()
	select {
	case frame, ok := <-channel.Recv():
		if ok {
			t.Fatalf("Pion Recv yielded late frame: %x", frame)
		}
	case <-time.After(pionLoopbackTimeout):
		t.Fatal("timeout waiting for Pion Recv close")
	}
}

func receiveLoopbackError(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(pionLoopbackTimeout):
		t.Fatal("timeout waiting for Pion operation")
		return nil
	}
}

func patternedLoopbackFrame(marker byte, size int) framechannel.Frame {
	frame := make(framechannel.Frame, size)
	if size == 0 {
		return frame
	}
	frame[0] = marker
	for index := 1; index < len(frame); index++ {
		frame[index] = byte((index*31 + 17) % 251)
	}
	return frame
}

type highWaterObserver struct {
	pionDataChannel
	highWater uint64
	highSeen  chan struct{}
	mu        sync.Mutex
}

func newHighWaterObserver(channel *pion.DataChannel, highWater uint64) *highWaterObserver {
	return &highWaterObserver{
		pionDataChannel: pionDataChannel{DataChannel: channel},
		highWater:       highWater,
		highSeen:        make(chan struct{}),
	}
}

func (o *highWaterObserver) BufferedAmount() uint64 {
	amount := o.pionDataChannel.BufferedAmount()
	if amount >= o.highWater {
		o.mu.Lock()
		if o.highSeen != nil {
			close(o.highSeen)
			o.highSeen = nil
		}
		o.mu.Unlock()
	}
	return amount
}

func (o *highWaterObserver) arm() <-chan struct{} {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.highSeen = make(chan struct{})
	return o.highSeen
}
