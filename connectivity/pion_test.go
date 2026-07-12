package connectivity

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/internal/testnetwork"
	"github.com/windshare/windshare/relay/protocol"
	transportwebrtc "github.com/windshare/windshare/transport/webrtc"
)

const peerTestTimeout = 10 * time.Second

type memorySignalWire struct {
	messages chan Signal
	closed   chan struct{}
	once     sync.Once
}

func newMemorySignalWire() *memorySignalWire {
	return &memorySignalWire{messages: make(chan Signal, 256), closed: make(chan struct{})}
}

func (w *memorySignalWire) close() { w.once.Do(func() { close(w.closed) }) }

type memorySignaling struct {
	inbound  *memorySignalWire
	outbound *memorySignalWire
}

func newMemorySignalingPair() (*memorySignaling, *memorySignaling, func()) {
	leftToRight := newMemorySignalWire()
	rightToLeft := newMemorySignalWire()
	left := &memorySignaling{inbound: rightToLeft, outbound: leftToRight}
	right := &memorySignaling{inbound: leftToRight, outbound: rightToLeft}
	return left, right, func() {
		leftToRight.close()
		rightToLeft.close()
	}
}

func (s *memorySignaling) Send(ctx context.Context, signal Signal) error {
	copySignal := Signal{Kind: signal.Kind, Payload: append([]byte(nil), signal.Payload...)}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.outbound.closed:
		return ErrSignalingClosed
	case s.outbound.messages <- copySignal:
		return nil
	}
}

func (s *memorySignaling) Receive(ctx context.Context) (Signal, error) {
	select {
	case <-ctx.Done():
		return Signal{}, ctx.Err()
	case <-s.inbound.closed:
		return Signal{}, ErrSignalingClosed
	case signal := <-s.inbound.messages:
		return signal, nil
	}
}

type peerResult struct {
	channel PeerChannel
	err     error
}

// terminalPendingChannel models D1's intentional Close barrier: once a local
// terminal is admitted, Close cannot settle until the parent transport aborts or
// an ACK arrives. These tests withhold the ACK, so parent closure is the only
// valid fatal-cleanup escape hatch.
type terminalPendingChannel struct {
	peer         *pion.PeerConnection
	stop         <-chan struct{}
	terminalSeen chan struct{}
	opened       chan struct{}
	done         chan struct{}
	recv         chan session.Frame

	terminalOnce sync.Once
	settleOnce   sync.Once
	mu           sync.Mutex
	err          error
}

func newTerminalPendingChannel(peer *pion.PeerConnection, stop <-chan struct{}) *terminalPendingChannel {
	opened := make(chan struct{})
	close(opened)
	return &terminalPendingChannel{
		peer:         peer,
		stop:         stop,
		terminalSeen: make(chan struct{}),
		opened:       opened,
		done:         make(chan struct{}),
		recv:         make(chan session.Frame),
	}
}

func (c *terminalPendingChannel) Opened() <-chan struct{}    { return c.opened }
func (c *terminalPendingChannel) Done() <-chan struct{}      { return c.done }
func (c *terminalPendingChannel) Recv() <-chan session.Frame { return c.recv }

func (c *terminalPendingChannel) State() session.ChannelState {
	select {
	case <-c.done:
		return session.Closed
	default:
		return session.Open
	}
}

func (c *terminalPendingChannel) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *terminalPendingChannel) Send(context.Context, session.Frame) error { return nil }

func (c *terminalPendingChannel) SendTerminal(_ context.Context, _ session.Frame) error {
	c.terminalOnce.Do(func() { close(c.terminalSeen) })
	// D1 cancellation is an admission boundary: once the terminal is on wire,
	// canceling its caller cannot let Close overtake the missing ACK.
	return c.waitForParentAbort(context.Background())
}

func (c *terminalPendingChannel) Close() error {
	return c.waitForParentAbort(context.Background())
}

func (c *terminalPendingChannel) waitForParentAbort(ctx context.Context) error {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if c.peer.ConnectionState() == pion.PeerConnectionStateClosed {
			c.settle(transportwebrtc.ErrTerminalNotAcknowledged)
			return c.Err()
		}
		select {
		case <-c.done:
			return c.Err()
		case <-ctx.Done():
			c.settle(ctx.Err())
			return ctx.Err()
		case <-c.stop:
			c.settle(context.Canceled)
			return context.Canceled
		case <-ticker.C:
		}
	}
}

func (c *terminalPendingChannel) settle(err error) {
	c.settleOnce.Do(func() {
		c.mu.Lock()
		c.err = err
		close(c.recv)
		close(c.done)
		c.mu.Unlock()
	})
}

func TestPionFactoryNegotiatesAndSurvivesSignalingLoss(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	ctx, cancel := context.WithTimeout(context.Background(), peerTestTimeout)
	defer cancel()
	offerSignals, answerSignals, closeSignals := newMemorySignalingPair()
	factory := NewPionChannelFactory(pion.Configuration{})
	answerResult := make(chan peerResult, 1)
	go func() {
		channel, err := factory.Answer(ctx, answerSignals)
		answerResult <- peerResult{channel: channel, err: err}
	}()
	offerChannel, err := factory.Offer(ctx, offerSignals)
	if err != nil {
		t.Fatalf("offer negotiation: %v", err)
	}
	answer := <-answerResult
	if answer.err != nil {
		t.Fatalf("answer negotiation: %v", answer.err)
	}
	answerChannel := answer.channel
	t.Cleanup(func() {
		_ = offerChannel.Close()
		_ = answerChannel.Close()
	})

	closeSignals()
	want := session.Frame{0x41, 0x42, 0x43}
	if err := offerChannel.Send(ctx, want); err != nil {
		t.Fatalf("send after signaling loss: %v", err)
	}
	select {
	case got := <-answerChannel.Recv():
		if string(got) != string(want) {
			t.Fatalf("received %x, want %x", got, want)
		}
	case <-ctx.Done():
		t.Fatal("timed out receiving P2P frame")
	}

	terminal := session.Frame{0x7f, 0x01}
	terminalResult := make(chan error, 1)
	go func() { terminalResult <- offerChannel.SendTerminal(ctx, terminal) }()
	select {
	case got, ok := <-answerChannel.Recv():
		if !ok || string(got) != string(terminal) {
			t.Fatalf("terminal receive = (%x, %v)", got, ok)
		}
	case <-ctx.Done():
		t.Fatal("timed out receiving terminal frame")
	}
	select {
	case _, ok := <-answerChannel.Recv():
		if ok {
			t.Fatal("terminal frame did not close receive stream")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for terminal receive closure")
	}
	if err := <-terminalResult; err != nil {
		t.Fatalf("terminal send: %v", err)
	}
}

type scriptedSignaling struct{ signals chan Signal }

func (s *scriptedSignaling) Send(context.Context, Signal) error { return nil }

func (s *scriptedSignaling) Receive(ctx context.Context) (Signal, error) {
	select {
	case <-ctx.Done():
		return Signal{}, ctx.Err()
	case signal := <-s.signals:
		return signal, nil
	}
}

func TestPionFactoryRejectsUnexpectedDescription(t *testing.T) {
	signaling := &scriptedSignaling{signals: make(chan Signal, 1)}
	signaling.signals <- Signal{Kind: "answer", Payload: []byte(`{"type":"answer","sdp":"v=0"}`)}
	factory := NewPionChannelFactory(pion.Configuration{})
	ctx, cancel := context.WithTimeout(context.Background(), peerTestTimeout)
	defer cancel()
	if _, err := factory.Answer(ctx, signaling); !errors.Is(err, ErrUnexpectedSignal) {
		t.Fatalf("unexpected description error = %v", err)
	}
}

func TestPionFactoryHonorsCancellationAndCopiesConfiguration(t *testing.T) {
	configuration := DefaultPionConfiguration()
	factory := NewPionChannelFactory(configuration)
	configuration.ICEServers[0].URLs[0] = "stun:mutated.invalid"
	if got := factory.configuration.ICEServers[0].URLs[0]; got != DefaultSTUNServer {
		t.Fatalf("factory STUN URL = %q", got)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := factory.Answer(canceled, &scriptedSignaling{signals: make(chan Signal)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled answer error = %v", err)
	}
}

func TestPionFactoryBoundsRemoteCandidatesBeforeDescription(t *testing.T) {
	signaling := &scriptedSignaling{signals: make(chan Signal, maxCandidatesPerPeer+1)}
	for range maxCandidatesPerPeer + 1 {
		signaling.signals <- Signal{Kind: protocol.SignalKindCandidate, Payload: json.RawMessage(`{}`)}
	}
	ctx, cancel := context.WithTimeout(context.Background(), peerTestTimeout)
	defer cancel()
	if _, err := NewPionChannelFactory(pion.Configuration{}).Answer(ctx, signaling); !errors.Is(err, ErrCandidateLimitExceeded) {
		t.Fatalf("candidate overflow error = %v", err)
	}
}

func TestPionChannelCloseReleasesOwnedPeerConnections(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	ctx, cancel := context.WithTimeout(context.Background(), peerTestTimeout)
	defer cancel()
	offerSignals, answerSignals, closeSignals := newMemorySignalingPair()
	defer closeSignals()
	factory := NewPionChannelFactory(pion.Configuration{})
	var peersMu sync.Mutex
	var peers []*pion.PeerConnection
	factory.newPeer = func(configuration pion.Configuration) (*pion.PeerConnection, error) {
		testnetwork.AssertOSNetwork()
		peer, err := pion.NewPeerConnection(configuration)
		if err == nil {
			peersMu.Lock()
			peers = append(peers, peer)
			peersMu.Unlock()
		}
		return peer, err
	}
	answerResult := make(chan peerResult, 1)
	go func() {
		channel, err := factory.Answer(ctx, answerSignals)
		answerResult <- peerResult{channel: channel, err: err}
	}()
	offerChannel, err := factory.Offer(ctx, offerSignals)
	if err != nil {
		t.Fatal(err)
	}
	answer := <-answerResult
	if answer.err != nil {
		t.Fatal(answer.err)
	}
	closeResults := make(chan error, 2)
	go func() { closeResults <- offerChannel.Close() }()
	go func() { closeResults <- answer.channel.Close() }()
	for range 2 {
		if err := <-closeResults; err != nil {
			t.Fatalf("close negotiated channel: %v", err)
		}
	}

	deadline := time.Now().Add(peerTestTimeout)
	for {
		peersMu.Lock()
		allClosed := len(peers) == 2
		for _, peer := range peers {
			allClosed = allClosed && peer.ConnectionState() == pion.PeerConnectionStateClosed
		}
		peersMu.Unlock()
		if allClosed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("explicit channel Close did not release every PeerConnection")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPionFactoryBoundsRemoteCandidatesAfterOpen(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	ctx, cancel := context.WithTimeout(context.Background(), peerTestTimeout)
	defer cancel()
	offerSignals, answerSignals, closeSignals := newMemorySignalingPair()
	defer closeSignals()
	factory := NewPionChannelFactory(pion.Configuration{})
	answerResult := make(chan peerResult, 1)
	go func() {
		channel, err := factory.Answer(ctx, answerSignals)
		answerResult <- peerResult{channel: channel, err: err}
	}()
	offerChannel, err := factory.Offer(ctx, offerSignals)
	if err != nil {
		t.Fatal(err)
	}
	answer := <-answerResult
	if answer.err != nil {
		t.Fatal(answer.err)
	}
	defer offerChannel.Close()
	defer answer.channel.Close()
	for range maxCandidatesPerPeer + 1 {
		if err := answerSignals.Send(ctx, Signal{
			Kind:    protocol.SignalKindCandidate,
			Payload: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-offerChannel.Done():
		if !errors.Is(offerChannel.Err(), ErrCandidateLimitExceeded) {
			t.Fatalf("post-open candidate overflow error = %v", offerChannel.Err())
		}
	case <-ctx.Done():
		t.Fatal("post-open candidate overflow did not close the peer")
	}
}

func TestPionFactoryRejectsAdditionalRemoteDataChannel(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	ctx, cancel := context.WithTimeout(context.Background(), peerTestTimeout)
	defer cancel()
	offerSignals, answerSignals, closeSignals := newMemorySignalingPair()
	defer closeSignals()
	rawPeer, err := pion.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer rawPeer.Close()
	rawPeer.OnICECandidate(func(candidate *pion.ICECandidate) {
		if candidate == nil {
			return
		}
		payload, marshalErr := json.Marshal(candidate.ToJSON())
		if marshalErr == nil {
			_ = offerSignals.Send(ctx, Signal{Kind: protocol.SignalKindCandidate, Payload: payload})
		}
	})
	for range 2 {
		if _, err := rawPeer.CreateDataChannel(
			transportwebrtc.ChannelLabel,
			transportwebrtc.DefaultDataChannelInit(),
		); err != nil {
			t.Fatal(err)
		}
	}
	offer, err := rawPeer.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rawPeer.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(rawPeer.LocalDescription())
	if err != nil {
		t.Fatal(err)
	}
	if err := offerSignals.Send(ctx, Signal{Kind: protocol.SignalKindOffer, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	driverDone := make(chan error, 1)
	go func() {
		for {
			signal, receiveErr := offerSignals.Receive(ctx)
			if receiveErr != nil {
				driverDone <- receiveErr
				return
			}
			switch signal.Kind {
			case protocol.SignalKindAnswer:
				description, decodeErr := decodeDescription(signal.Payload, pion.SDPTypeAnswer)
				if decodeErr == nil {
					decodeErr = rawPeer.SetRemoteDescription(description)
				}
				if decodeErr != nil {
					driverDone <- decodeErr
					return
				}
			case protocol.SignalKindCandidate:
				candidate, decodeErr := decodeCandidate(signal.Payload)
				if decodeErr == nil {
					decodeErr = rawPeer.AddICECandidate(candidate)
				}
				if decodeErr != nil {
					driverDone <- decodeErr
					return
				}
			}
		}
	}()

	channel, err := NewPionChannelFactory(pion.Configuration{}).Answer(ctx, answerSignals)
	if err != nil {
		if !errors.Is(err, ErrUnexpectedDataChannel) {
			t.Fatalf("additional DataChannel error = %v", err)
		}
		return
	}
	defer channel.Close()
	select {
	case <-channel.Done():
		if !errors.Is(channel.Err(), ErrUnexpectedDataChannel) {
			t.Fatalf("additional DataChannel terminal error = %v", channel.Err())
		}
	case driverErr := <-driverDone:
		t.Fatalf("raw signaling driver failed before rejection: %v", driverErr)
	case <-ctx.Done():
		t.Fatal("additional remote DataChannel was not rejected")
	}
}

func TestFatalPostOpenExitAbortsParentBeforeTerminalPendingCleanup(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	tests := []struct {
		name                 string
		remoteCandidateCount int
		want                 error
		trigger              func(context.CancelFunc, *pionNegotiation)
	}{
		{
			name: "additional remote DataChannel",
			want: ErrUnexpectedDataChannel,
			trigger: func(_ context.CancelFunc, negotiation *pionNegotiation) {
				negotiation.failures <- negotiationFailure{err: ErrUnexpectedDataChannel, fatalAfterOpen: true}
			},
		},
		{
			name: "local candidate overflow",
			want: ErrCandidateLimitExceeded,
			trigger: func(_ context.CancelFunc, negotiation *pionNegotiation) {
				negotiation.failures <- negotiationFailure{err: ErrCandidateLimitExceeded, fatalAfterOpen: true}
			},
		},
		{
			name:                 "remote candidate overflow",
			remoteCandidateCount: maxCandidatesPerPeer,
			want:                 ErrCandidateLimitExceeded,
			trigger: func(_ context.CancelFunc, negotiation *pionNegotiation) {
				negotiation.inbound <- Signal{Kind: protocol.SignalKindCandidate, Payload: json.RawMessage(`{}`)}
			},
		},
		{
			name: "unexpected post-open description",
			want: ErrUnexpectedSignal,
			trigger: func(_ context.CancelFunc, negotiation *pionNegotiation) {
				negotiation.inbound <- Signal{Kind: protocol.SignalKindOffer, Payload: json.RawMessage(`{}`)}
			},
		},
		{
			name: "invalid candidate encoding",
			want: ErrInvalidSignal,
			trigger: func(_ context.CancelFunc, negotiation *pionNegotiation) {
				negotiation.inbound <- Signal{Kind: protocol.SignalKindCandidate, Payload: json.RawMessage(`[]`)}
			},
		},
		{
			name: "AddICECandidate failure",
			want: ErrInvalidSignal,
			trigger: func(_ context.CancelFunc, negotiation *pionNegotiation) {
				negotiation.inbound <- Signal{Kind: protocol.SignalKindCandidate, Payload: json.RawMessage(`{"candidate":"not-a-candidate"}`)}
			},
		},
		{
			name:    "negotiation owner cancellation",
			want:    context.Canceled,
			trigger: func(cancel context.CancelFunc, _ *pionNegotiation) { cancel() },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testnetwork.RequireOSNetwork(t)
			peer, err := pion.NewPeerConnection(pion.Configuration{})
			if err != nil {
				t.Fatal(err)
			}
			factoryCtx, cancelFactory := context.WithCancel(context.Background())
			stop := make(chan struct{})
			channel := newTerminalPendingChannel(peer, stop)
			negotiation := &pionNegotiation{
				ctx:      factoryCtx,
				cancel:   cancelFactory,
				peer:     peer,
				inbound:  make(chan Signal, 1),
				failures: make(chan negotiationFailure, 1),
			}
			owned := negotiation.own(channel)
			maintenanceDone := make(chan struct{})
			go func() {
				negotiation.maintain(owned, true, test.remoteCandidateCount)
				close(maintenanceDone)
			}()

			terminalCtx, cancelTerminal := context.WithCancel(context.Background())
			terminalResult := make(chan error, 1)
			go func() { terminalResult <- owned.SendTerminal(terminalCtx, session.Frame{0x01}) }()
			select {
			case <-channel.terminalSeen:
			case <-time.After(orchestrationTestTimeout):
				close(stop)
				t.Fatal("terminal was not admitted")
			}
			test.trigger(cancelFactory, negotiation)

			select {
			case err := <-terminalResult:
				if !errors.Is(err, transportwebrtc.ErrTerminalNotAcknowledged) {
					t.Fatalf("terminal result = %v", err)
				}
			case <-time.After(orchestrationTestTimeout):
				close(stop)
				t.Fatal("parent abort did not release terminal send")
			}
			select {
			case <-owned.Done():
			case <-time.After(orchestrationTestTimeout):
				close(stop)
				t.Fatal("parent abort did not settle channel Done")
			}
			select {
			case <-maintenanceDone:
			case <-time.After(orchestrationTestTimeout):
				close(stop)
				t.Fatal("fatal maintenance path did not settle")
			}
			if peer.ConnectionState() != pion.PeerConnectionStateClosed {
				close(stop)
				t.Fatalf("PeerConnection state = %s", peer.ConnectionState())
			}
			if err := owned.Err(); !errors.Is(err, test.want) || !errors.Is(err, transportwebrtc.ErrTerminalNotAcknowledged) {
				close(stop)
				t.Fatalf("joined owner/channel error = %v; want %v and terminal-not-acknowledged", err, test.want)
			}
			cancelTerminal()
			select {
			case <-stop:
			default:
				close(stop)
			}
		})
	}
}

func TestHostileAbortUnpinsFatalPathAndSiblingCancellation(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	peer, err := pion.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	factoryCtx, cancelFactory := context.WithCancel(context.Background())
	stop := make(chan struct{})
	channel := newTerminalPendingChannel(peer, stop)
	negotiation := &pionNegotiation{
		ctx:      factoryCtx,
		cancel:   cancelFactory,
		peer:     peer,
		inbound:  make(chan Signal, 1),
		failures: make(chan negotiationFailure, 1),
	}
	owned := negotiation.own(channel)
	maintenanceDone := make(chan struct{})
	go func() {
		negotiation.maintain(owned, true, 0)
		close(maintenanceDone)
	}()

	wantFatal := errors.New("shared source failure")
	relayChannel := newFakePeerChannel()
	sender, err := NewSender(
		answerFactoryFunc(func(context.Context, Signaling) (PeerChannel, error) { return owned, nil }),
		SenderOptions{ClassifySessionError: func(err error) SendErrorDisposition {
			if errors.Is(err, wantFatal) {
				return SendShareFatal
			}
			return SendPathEnded
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		result <- sender.ServeReceiver(t.Context(), relayChannel, &inertSignaling{}, &recordingStore{err: wantFatal}, &recordingSealer{})
	}()
	request := mustRequest(t)
	go func() { channel.recv <- request }()
	select {
	case <-channel.terminalSeen:
	case <-time.After(orchestrationTestTimeout):
		close(stop)
		t.Fatal("peer path did not admit its source-failure terminal")
	}
	if err := relayChannel.deliver(request); err != nil {
		close(stop)
		t.Fatal(err)
	}
	// The relay path's fatal result cancels its sibling, but an admitted peer
	// terminal still cannot settle until the hostile owner exit aborts the parent.
	select {
	case err := <-result:
		close(stop)
		t.Fatalf("sender settled before parent abort: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	negotiation.failures <- negotiationFailure{err: ErrUnexpectedDataChannel, fatalAfterOpen: true}
	if err := waitError(t, result); !errors.Is(err, wantFatal) {
		close(stop)
		t.Fatalf("ServeReceiver error = %v", err)
	}
	select {
	case <-maintenanceDone:
	case <-time.After(orchestrationTestTimeout):
		close(stop)
		t.Fatal("hostile abort did not settle peer path maintenance")
	}
	if err := owned.Err(); !errors.Is(err, ErrUnexpectedDataChannel) || !errors.Is(err, transportwebrtc.ErrTerminalNotAcknowledged) {
		close(stop)
		t.Fatalf("peer path error = %v", err)
	}
	close(stop)
}
