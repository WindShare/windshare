package v2peer

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/turnsession"
	"github.com/windshare/windshare/connectivity/v2signal"
	p "github.com/windshare/windshare/core/session/protocolsession"
)

func requestTestTURNGrants(t *testing.T, ctx context.Context, session *testPeerSession, id uint64, services ...string) []p.TURNGrant {
	t.Helper()
	body, err := p.EncodeTURNDemand(p.TURNDemand{RequestID: id, Mode: p.TURNAll, ServiceIDs: services})
	if err != nil {
		t.Fatal(err)
	}
	session.sessionControlMu.Lock()
	handle := session.turnHandler
	session.sessionControlMu.Unlock()
	if err := handle(ctx, body); err != nil {
		t.Fatal(err)
	}
	reply, err := p.DecodeTURNGrants(receiveTest(t, session.sessionControls).body)
	if err != nil || reply.RequestID != id || reply.Status != p.TURNOK || len(reply.Grants) == 0 {
		t.Fatalf("TURN response ID=%d status=%v grants=%d err=%v", reply.RequestID, reply.Status, len(reply.Grants), err)
	}
	return reply.Grants
}

func sendTestTURNOffer(t *testing.T, ctx context.Context, handler *senderHandler, seed byte, binding v2signal.Binding, id p.TURNGrantID) p.Operation {
	t.Helper()
	op := testPeerOperation(testOperationID(seed))
	body, err := v2signal.EncodeOffer(v2signal.Offer{Binding: binding, TURNGrantID: id, SDP: "v=0\r\n"})
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.HandleMessage(ctx, op, testMessage(t, p.MessagePeerOffer, op.ID(), body)); err != nil {
		t.Fatal(err)
	}
	return op
}

func TestQueuedSenderTURNOfferOwnsItsGrantUntilExpiry(t *testing.T) {
	for _, test := range []struct {
		name      string
		remaining time.Duration
	}{
		{"still valid below wire precision", time.Microsecond},
		{"exact expiry", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) { testQueuedTURNOffer(t, test.remaining) })
		})
	}
}

func testQueuedTURNOffer(t *testing.T, remaining time.Duration) {
	clock := &handshakeClock{now: time.Unix(100, 0)}
	capacity := turnsession.NewCapacity()
	factory := mustTestFactory(t, Config{
		TURNCapacity: capacity, TURNProvider: renewalTestProvider{now: clock.Now}, Now: clock.Now,
	})
	opened := make(chan p.TURNGrant, 1)
	factory.turnPeers = func(_ context.Context, _ nativepeer.AttemptRequest, g p.TURNGrant) (turnPeerConnection, error) {
		opened <- g
		return &testTURNPeer{newReceiverTestPeerConnection()}, nil
	}
	session := newTestPeerSession(1)
	handler, ctx, stop, done := startSenderTestRuntime(t, factory, session)
	defer stopSenderTestRuntime(t, stop, done)
	synctest.Wait()
	var freeOne func()
	for range p.MaxGrantsPerMessage {
		give, err := capacity.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer give()
		freeOne = give
	}
	start := clock.Now()
	initial := requestTestTURNGrants(t, ctx, session, 1)[0]
	expiry := start.Add(time.Duration(initial.ValidForMS) * time.Millisecond)
	// Binding after half a millisecond exposes any expiry reconstructed from a wire TTL.
	clock.Advance(time.Millisecond / 2)
	binding := testBinding(40)
	binding.Purpose = v2signal.TURNRelay
	queued := sendTestTURNOffer(t, ctx, handler, 1, binding, initial.GrantID)
	synctest.Wait()
	if len(opened) != 0 || len(session.failures) != 0 {
		t.Fatal("offer did not wait for shared capacity")
	}

	previous := initial.GrantID
	for id := uint64(2); id <= 3; id++ {
		clock.Advance(2 * time.Second)
		next := requestTestTURNGrants(t, ctx, session, id, initial.ServiceID)[0]
		if next.GrantID == previous || next.GrantID == initial.GrantID {
			t.Fatal("targeted request did not rotate the original grant")
		}
		previous = next.GrantID
	}
	other := testBinding(41)
	other.Purpose = v2signal.TURNRelay
	late := sendTestTURNOffer(t, ctx, handler, 2, other, initial.GrantID)
	if failure := receiveTest(t, session.failures); failure.operation != late.ID() || failure.code != p.PeerOperationCodeNegotiation {
		t.Fatalf("new offer reused an evicted grant: operation=%d code=%d", failure.operation, failure.code)
	}

	// Advance only the credential clock so the independent preparation window is still open.
	clock.Advance(expiry.Sub(clock.Now()) - remaining)
	freeOne()
	if remaining > 0 {
		answer := receiveTest(t, session.controls)
		credential := receiveTest(t, opened)
		if answer.kind != p.MessagePeerAnswer || answer.operation != queued.ID() || credential.GrantID != initial.GrantID || credential.ValidForMS == 0 {
			t.Fatal("queued offer did not negotiate with its original unexpired temporary grant")
		}
		return
	}
	if failure := receiveTest(t, session.failures); failure.operation != queued.ID() || failure.code != p.PeerOperationCodeNegotiation || len(opened) != 0 {
		t.Fatalf("expired queued offer reached ICE: operation=%d code=%d opens=%d", failure.operation, failure.code, len(opened))
	}
}
