package v2peer

import (
	"context"
	"fmt"
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
	factory.turnPeers = func(nativepeer.AttemptRequest, p.TURNGrant) (turnPeerConnection, error) {
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
		senderFactory.turnPeers = func(nativepeer.AttemptRequest, p.TURNGrant) (turnPeerConnection, error) {
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
