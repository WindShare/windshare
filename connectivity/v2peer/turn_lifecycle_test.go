package v2peer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/turnsession"
	"github.com/windshare/windshare/connectivity/v2signal"
	coreSession "github.com/windshare/windshare/core/session"
	p "github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/turngrant"
	"github.com/windshare/windshare/core/transfer/lanes"
)

type testTURNPeer struct{ *receiverTestPeerConnection }

func (*testTURNPeer) SelectedPeerRoute() (lanes.Route, bool) { return lanes.RouteTURN, true }

func (*testTURNPeer) CreateAnswer(*pion.AnswerOptions) (pion.SessionDescription, error) {
	return pion.SessionDescription{Type: pion.SDPTypeAnswer, SDP: "v=0\r\n"}, nil
}

func testTURNGrant(seed byte) p.TURNGrant {
	return p.TURNGrant{ServiceID: "home", GrantID: p.TURNGrantID{seed}, URLs: []string{"turn:127.0.0.1:3478"}, Username: "user", Password: "pass"}
}

func newTURNReceiverHarness(t *testing.T, capacity *turnsession.Capacity, seed byte) *receiverHarness {
	t.Helper()
	peer := newReceiverTestPeerConnection()
	channel := newReceiverTestChannel()
	operation := newReceiverTestOperation()
	operation.channel = channel
	h := &receiverHarness{
		peer: peer, channel: channel, operation: operation, lanes: newReceiverTestLanes(), binding: testBinding(seed),
		signaling: &receiverTestSignaling{operation: operation, offers: make(chan []byte, 1)},
	}
	h.binding.Purpose = v2signal.TURNRelay
	factory, err := newTestReceiverFactory(t, ReceiverFactoryConfig{
		TURNCapacity: capacity,
		DataChannels: DataChannelAdapterFunc(func(*pion.DataChannel) (PeerDataChannel, error) { return channel, nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	factory.turnPeers = func(context.Context, nativepeer.AttemptRequest, p.TURNGrant) (turnPeerConnection, error) {
		return &testTURNPeer{peer}, nil
	}
	h.factory = factory
	return h
}

func TestReceiverTURNPermitEndsAtAuthenticatedAttachment(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		capacity := turnsession.NewCapacity()
		grant := testTURNGrant(1)
		// Independent factories represent separate users of one engine's negotiation budget.
		var pending []*receiverHarness
		for i := range p.MaxGrantsPerMessage {
			h := newTURNReceiverHarness(t, capacity, byte(i+1))
			attempt, err := h.factory.StartTURNBinding(t.Context(), h.signaling, h.lanes, h.binding, grant)
			if err != nil {
				t.Fatal(err)
			}
			h.attempt = attempt
			defer attempt.Close()
			offer, err := v2signal.DecodeOffer(receiveTest(t, h.signaling.offers))
			if err != nil || offer.TURNGrantID != grant.GrantID || offer.Binding != h.binding {
				t.Fatalf("outbound TURN offer = %+v, err = %v", offer.Binding, err)
			}
			h.answer(t)
			pending = append(pending, h)
		}

		next := newTURNReceiverHarness(t, capacity, 10)
		type started struct {
			attempt *ReceiverAttempt
			err     error
		}
		start := make(chan started, 1)
		go func() {
			a, err := next.factory.StartTURNBinding(t.Context(), next.signaling, next.lanes, next.binding, grant)
			start <- started{a, err}
		}()
		synctest.Wait()
		if len(start) != 0 {
			t.Fatal("fifth TURN negotiation started before any lane attached")
		}

		first := pending[0]
		first.openAndAwaitLane(t)
		defer first.admittedOwner.Close()
		fifth := receiveTest(t, start)
		if fifth.err != nil {
			t.Fatal(fifth.err)
		}
		defer fifth.attempt.Close()
		for _, h := range pending[1:] {
			h.openAndAwaitLane(t)
			defer h.admittedOwner.Close()
		}

		acquire := func() <-chan func() {
			result := make(chan func(), 1)
			go func() {
				give, err := capacity.Acquire(t.Context())
				if err == nil {
					result <- give
				}
			}()
			return result
		}
		// The fifth negotiation is the only remaining permit owner.
		for range p.MaxGrantsPerMessage - 1 {
			give := receiveTest(t, acquire())
			defer give()
		}
		last := acquire()
		synctest.Wait()
		if len(last) != 0 {
			t.Fatal("the unadmitted fifth negotiation lost its permit")
		}
		if err := fifth.attempt.Close(); err != nil {
			t.Fatal(err)
		}
		give := receiveTest(t, last)
		defer give()
		for _, h := range pending {
			select {
			case <-h.attempt.Done():
				t.Fatal("reusing all four permits ended an installed lane's signaling")
			case <-h.channel.Done():
				t.Fatal("reusing all four permits closed an installed channel")
			default:
			}
		}

		extra := acquire()
		if err := first.attempt.Close(); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if len(extra) != 0 {
			t.Fatal("closing an installed attempt returned a second permit")
		}
		give()
		receiveTest(t, extra)()
	})
}

type multiServiceTURNProvider struct{ count int }

func (provider multiServiceTURNProvider) Issue(_ context.Context, _ turngrant.IssueRequest) turngrant.IssueResult {
	result := turngrant.IssueResult{Status: p.TURNOK}
	for i := range provider.count {
		result.Credentials = append(result.Credentials, turngrant.Credential{
			ServiceID: fmt.Sprintf("turn-%d", i+1),
			URLs:      []string{"turn:127.0.0.1:3478"},
			Username:  "user",
			Password:  "pass",
		})
	}
	return result
}

func TestPairedFullLoadTURNNegotiationCompletesThroughAttachmentWithIsolatedCapacities(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		senderCapacity := turnsession.NewCapacity()
		receiverCapacity := turnsession.NewCapacity()
		clock := &handshakeClock{now: time.Unix(100, 0)}

		senderFactory := mustTestFactory(t, Config{
			TURNCapacity: senderCapacity,
			TURNProvider: multiServiceTURNProvider{count: p.MaxGrantsPerMessage},
			Now:          clock.Now,
			DataChannels: DataChannelAdapterFunc(func(*pion.DataChannel) (PeerDataChannel, error) {
				return newTestPeerChannel(), nil
			}),
		})
		senderPeers := make(chan *testTURNPeer, p.MaxGrantsPerMessage)
		senderFactory.turnPeers = func(context.Context, nativepeer.AttemptRequest, p.TURNGrant) (turnPeerConnection, error) {
			peer := &testTURNPeer{newReceiverTestPeerConnection()}
			senderPeers <- peer
			return peer, nil
		}

		session := newTestPeerSession(7)
		handler, ctx, stop, done := startSenderTestRuntime(t, senderFactory, session)
		defer stopSenderTestRuntime(t, stop, done)
		synctest.Wait()

		grants := requestTestTURNGrants(t, ctx, session, 1)
		if len(grants) != p.MaxGrantsPerMessage {
			t.Fatalf("issued %d TURN grants, want %d", len(grants), p.MaxGrantsPerMessage)
		}

		receivers := make([]*receiverHarness, 0, p.MaxGrantsPerMessage)
		for i, grant := range grants {
			h := newTURNReceiverHarness(t, receiverCapacity, byte(i+1))
			h.binding.PeerSlot = uint8(i)
			attempt, err := h.factory.StartTURNBinding(ctx, h.signaling, h.lanes, h.binding, grant)
			if err != nil {
				t.Fatal(err)
			}
			h.attempt = attempt
			defer attempt.Close()
			offerBody := receiveTest(t, h.signaling.offers)
			op := testPeerOperation(testOperationID(byte(i + 1)))
			if err := handler.HandleMessage(ctx, op, testMessage(t, p.MessagePeerOffer, op.ID(), offerBody)); err != nil {
				t.Fatal(err)
			}
			receivers = append(receivers, h)
		}

		// All four paired offers receive answers while all four receiver permits remain held.
		for range p.MaxGrantsPerMessage {
			control := receiveTest(t, session.controls)
			if control.kind != p.MessagePeerAnswer {
				t.Fatalf("unexpected sender control kind %v", control.kind)
			}
		}

		for i, h := range receivers {
			h.answer(t)
			h.openAndAwaitLane(t)
			defer h.admittedOwner.Close()

			session.lane = coreSession.LaneIdentity{ID: uint32(10 + i), Epoch: 1}
			peer := receiveTest(t, senderPeers)
			peer.emitUnexpectedDataChannel(&pion.DataChannel{})
			receiveTest(t, session.admissions)
			owner := receiveTest(t, session.ownedAdmissions)
			defer owner.Close()
			synctest.Wait()
		}

		// After authenticated attachment on both sides, every permit is returned.
		for _, pool := range []*turnsession.Capacity{receiverCapacity, senderCapacity} {
			var releases []func()
			for range p.MaxGrantsPerMessage {
				release, err := pool.Acquire(ctx)
				if err != nil {
					t.Fatalf("permit remained held after attachment: %v", err)
				}
				releases = append(releases, release)
			}
			for _, release := range releases {
				release()
			}
		}
	})
}

// blockingDialTURNPeer models Pion's gatherer barrier: Close blocks until the
// attempt's dial context cancels, matching an unresponsive TCP/TLS TURN dial.
type blockingDialTURNPeer struct {
	*testTURNPeer
	dialCtx  context.Context
	mu       sync.Mutex
	iceState func(pion.ICEConnectionState)
}

func (peer *blockingDialTURNPeer) OnICEConnectionStateChange(cb func(pion.ICEConnectionState)) {
	peer.mu.Lock()
	peer.iceState = cb
	peer.mu.Unlock()
}

func (peer *blockingDialTURNPeer) emitICE(state pion.ICEConnectionState) {
	peer.mu.Lock()
	cb := peer.iceState
	peer.mu.Unlock()
	if cb != nil {
		cb(state)
	}
}

func (peer *blockingDialTURNPeer) Close() error {
	<-peer.dialCtx.Done()
	return peer.testTURNPeer.Close()
}

func assertAllTURNPermitsAvailable(t *testing.T, ctx context.Context, capacity *turnsession.Capacity) {
	t.Helper()
	var releases []func()
	for i := range p.MaxGrantsPerMessage {
		probeCtx, cancel := context.WithTimeout(ctx, time.Second)
		release, err := capacity.Acquire(probeCtx)
		cancel()
		if err != nil {
			t.Fatalf("TURN permit %d still occupied after teardown: %v", i+1, err)
		}
		releases = append(releases, release)
	}
	for _, release := range releases {
		release()
	}
}

func TestTURNNegotiationCancelAndTimeoutUnblockTeardownAndReleasePermits(t *testing.T) {
	t.Run("receiver cancel", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			capacity := turnsession.NewCapacity()
			h := newTURNReceiverHarness(t, capacity, 1)
			dialCtxCh := make(chan context.Context, 1)
			h.factory.turnPeers = func(ctx context.Context, _ nativepeer.AttemptRequest, _ p.TURNGrant) (turnPeerConnection, error) {
				dialCtxCh <- ctx
				return &blockingDialTURNPeer{testTURNPeer: &testTURNPeer{h.peer}, dialCtx: ctx}, nil
			}
			attempt, err := h.factory.StartTURNBinding(t.Context(), h.signaling, h.lanes, h.binding, testTURNGrant(1))
			if err != nil {
				t.Fatal(err)
			}
			receiveTest(t, h.signaling.offers)
			dialCtx := receiveTest(t, dialCtxCh)
			if err := attempt.Close(); err != nil {
				t.Fatalf("attempt Close: %v", err)
			}
			if dialCtx.Err() == nil {
				t.Fatal("receiver TURN dial context was not canceled on Close")
			}
			assertAllTURNPermitsAvailable(t, t.Context(), capacity)
		})
	})

	t.Run("receiver negotiation timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			capacity := turnsession.NewCapacity()
			peer := newReceiverTestPeerConnection()
			channel := newReceiverTestChannel()
			operation := newReceiverTestOperation()
			operation.channel = channel
			binding := testBinding(2)
			binding.Purpose = v2signal.TURNRelay
			signaling := &receiverTestSignaling{operation: operation, offers: make(chan []byte, 1)}
			factory, err := newTestReceiverFactory(t, ReceiverFactoryConfig{
				TURNCapacity: capacity,
				DataChannels: DataChannelAdapterFunc(func(*pion.DataChannel) (PeerDataChannel, error) { return channel, nil }),
			})
			if err != nil {
				t.Fatal(err)
			}
			opened := make(chan *blockingDialTURNPeer, 1)
			factory.turnPeers = func(ctx context.Context, _ nativepeer.AttemptRequest, _ p.TURNGrant) (turnPeerConnection, error) {
				p := &blockingDialTURNPeer{testTURNPeer: &testTURNPeer{peer}, dialCtx: ctx}
				opened <- p
				return p, nil
			}
			attempt, err := factory.StartTURNBinding(t.Context(), signaling, newReceiverTestLanes(), binding, testTURNGrant(2))
			if err != nil {
				t.Fatal(err)
			}
			receiveTest(t, signaling.offers)
			turnPeer := receiveTest(t, opened)
			// Entering ICE checking must keep the dial context alive across the preparation window.
			turnPeer.emitICE(pion.ICEConnectionStateChecking)
			time.Sleep(PeerSignalingPreparationBudget + time.Second)
			synctest.Wait()
			if turnPeer.dialCtx.Err() != nil {
				t.Fatalf("TURN dial context canceled early during ICE checking: %v", context.Cause(turnPeer.dialCtx))
			}
			time.Sleep(PeerICECheckingBudget)
			synctest.Wait()
			select {
			case <-attempt.Done():
			default:
				t.Fatal("timed-out receiver TURN attempt remained blocked in teardown")
			}
			if !errors.Is(context.Cause(turnPeer.dialCtx), ErrPeerNegotiationTimeout) {
				t.Fatalf("receiver TURN dial cause = %v, want ErrPeerNegotiationTimeout", context.Cause(turnPeer.dialCtx))
			}
			assertAllTURNPermitsAvailable(t, t.Context(), capacity)
		})
	})

	t.Run("sender cancel and timeout", func(t *testing.T) {
		for _, timeout := range []bool{false, true} {
			name := "cancel"
			if timeout {
				name = "timeout"
			}
			t.Run(name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					capacity := turnsession.NewCapacity()
					clock := &handshakeClock{now: time.Unix(100, 0)}
					factory := mustTestFactory(t, Config{
						TURNCapacity: capacity,
						TURNProvider: multiServiceTURNProvider{count: 1},
						Now:          clock.Now,
					})
					dialCtxCh := make(chan context.Context, 1)
					factory.turnPeers = func(ctx context.Context, _ nativepeer.AttemptRequest, _ p.TURNGrant) (turnPeerConnection, error) {
						dialCtxCh <- ctx
						return &blockingDialTURNPeer{testTURNPeer: &testTURNPeer{newReceiverTestPeerConnection()}, dialCtx: ctx}, nil
					}
					session := newTestPeerSession(9)
					handler, ctx, stop, done := startSenderTestRuntime(t, factory, session)
					defer stopSenderTestRuntime(t, stop, done)
					synctest.Wait()

					grant := requestTestTURNGrants(t, ctx, session, 1)[0]
					binding := testBinding(20)
					binding.Purpose = v2signal.TURNRelay
					op := sendTestTURNOffer(t, ctx, handler, 1, binding, grant.GrantID)
					receiveTest(t, session.controls)
					dialCtx := receiveTest(t, dialCtxCh)

					if timeout {
						time.Sleep(PeerSignalingPreparationBudget + time.Second)
						synctest.Wait()
						receiveTest(t, session.failures)
						if !errors.Is(context.Cause(dialCtx), ErrPeerNegotiationTimeout) {
							t.Fatalf("sender TURN dial cause = %v, want ErrPeerNegotiationTimeout", context.Cause(dialCtx))
						}
					} else {
						if err := handler.Cancel(ctx, op); err != nil {
							t.Fatalf("handler Cancel: %v", err)
						}
						synctest.Wait()
						if dialCtx.Err() == nil {
							t.Fatal("sender TURN dial context was not canceled on Cancel")
						}
					}
					assertAllTURNPermitsAvailable(t, ctx, capacity)
				})
			})
		}
	})
}

func TestRealTURNPeerAttemptCancelsUnresponsiveTLSEndpointAndReleasesPermit(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	accepted := make(chan struct{}, 2)
	var acceptedMu sync.Mutex
	var acceptedConns []net.Conn
	defer func() {
		acceptedMu.Lock()
		for _, conn := range acceptedConns {
			_ = conn.Close()
		}
		acceptedMu.Unlock()
	}()
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			acceptedMu.Lock()
			acceptedConns = append(acceptedConns, conn)
			acceptedMu.Unlock()
			select {
			case accepted <- struct{}{}:
			default:
			}
		}
	}()

	capacity := turnsession.NewCapacity()
	operation := newReceiverTestOperation()
	signaling := &receiverTestSignaling{operation: operation, offers: make(chan []byte, 1)}
	factory, err := newTestReceiverFactory(t, ReceiverFactoryConfig{TURNCapacity: capacity})
	if err != nil {
		t.Fatal(err)
	}
	binding := testBinding(30)
	binding.Purpose = v2signal.TURNRelay
	grant := p.TURNGrant{
		ServiceID: "blackhole-tls",
		GrantID:   p.TURNGrantID{30},
		URLs:      []string{"turns:" + listener.Addr().String() + "?transport=tcp"},
		Username:  "user",
		Password:  "pass",
	}

	attempt, err := factory.StartTURNBinding(t.Context(), signaling, newReceiverTestLanes(), binding, grant)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		_ = attempt.Close()
		t.Fatal("real Pion TURN gatherer did not dial the local TLS TCP endpoint")
	}

	closed := make(chan error, 1)
	go func() { closed <- attempt.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("attempt Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceling real TURN attempt blocked on unresponsive TLS endpoint")
	}
	assertAllTURNPermitsAvailable(t, t.Context(), capacity)
}
