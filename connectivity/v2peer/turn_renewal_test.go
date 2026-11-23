package v2peer

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/v2signal"
	coreSession "github.com/windshare/windshare/core/session"
	p "github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/turngrant"
)

type renewalTestProvider struct{ now func() time.Time }

func (provider renewalTestProvider) Issue(_ context.Context, r turngrant.IssueRequest) turngrant.IssueResult {
	services := r.ServiceIDs
	validFor := time.Hour
	if len(services) == 0 {
		services = []string{"home", "other"}
		validFor = 90 * time.Second
	}
	result := turngrant.IssueResult{Status: p.TURNOK}
	for _, id := range services {
		result.Credentials = append(result.Credentials, turngrant.Credential{
			ServiceID: id, URLs: []string{"turn:127.0.0.1:3478"}, Username: "user", Password: "pass", ExpiresAt: provider.now().Add(validFor),
		})
	}
	return result
}

func TestSenderTURNRenewalWhilePreviousTransportDrains(t *testing.T) {
	for _, before := range []bool{true, false} {
		name := "offer before cancel"
		if before {
			name = "cancel before offer"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) { testSenderTURNRenewal(t, before) })
		})
	}
}

func testSenderTURNRenewal(t *testing.T, cancelFirst bool) {
	var elapsed atomic.Int64
	now := func() time.Time { return time.Unix(elapsed.Load(), 0) }
	factory := mustTestFactory(t, Config{
		TURNProvider: renewalTestProvider{now: now}, Now: now,
		PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) { return newTestPeerConnection(), nil }),
		DataChannels:    DataChannelAdapterFunc(func(*pion.DataChannel) (PeerDataChannel, error) { return newTestPeerChannel(), nil }),
	})
	opened := make(chan *testTURNPeer, 1)
	factory.turnPeers = func(nativepeer.AttemptRequest, p.TURNGrant) (turnPeerConnection, error) {
		peer := &testTURNPeer{newReceiverTestPeerConnection()}
		opened <- peer
		return peer, nil
	}
	session := newTestPeerSession(1)
	oldLane := session.lane
	newLane := coreSession.LaneIdentity{ID: oldLane.ID + 1, Epoch: 1}
	retired := make(chan [2]coreSession.LaneIdentity, 1)
	drain := make(chan struct{})
	finishDrain := sync.OnceFunc(func() { close(drain) })
	var oldOwner p.FrameChannel
	session.retireReplacement = func(old, successor coreSession.LaneIdentity) bool {
		retired <- [2]coreSession.LaneIdentity{old, successor}
		<-drain
		return oldOwner.Close() == nil
	}
	handler, ctx, stop, finished := startSenderTestRuntime(t, factory, session)
	defer stopSenderTestRuntime(t, stop, finished)
	defer finishDrain()
	synctest.Wait()

	grants := func(target ...string) []p.TURNGrant {
		t.Helper()
		return requestTestTURNGrants(t, ctx, session, uint64(elapsed.Add(60)), target...)
	}

	var operation byte
	offer := func(binding v2signal.Binding, grant p.TURNGrantID, wantAnswer bool) p.Operation {
		t.Helper()
		operation++
		op := sendTestTURNOffer(t, ctx, handler, operation, binding, grant)
		select {
		case c := <-session.controls:
			if !wantAnswer || c.kind != p.MessagePeerAnswer || c.operation != op.ID() {
				t.Fatalf("unexpected answer for %v", binding)
			}
		case f := <-session.failures:
			if wantAnswer || f.code != p.PeerOperationCodeNegotiation || f.operation != op.ID() {
				t.Fatalf("unexpected failure for %v: %v", binding, f)
			}
		case <-time.After(peerTestTimeout):
			t.Fatal("missing TURN offer response")
		}
		return op
	}
	attach := func(peer *testTURNPeer) p.FrameChannel {
		t.Helper()
		peer.emitUnexpectedDataChannel(&pion.DataChannel{})
		receiveTest(t, session.admissions)
		owner := receiveTest(t, session.ownedAdmissions)
		synctest.Wait()
		return owner
	}
	nextBinding := func(b v2signal.Binding) v2signal.Binding {
		b.AttemptID[0]++
		b.AttemptSequence++
		return b
	}
	initial := grants()
	binding := testBinding(40)
	binding.Purpose = v2signal.TURNRelay
	old := offer(binding, initial[0].GrantID, true)
	oldPeer := receiveTest(t, opened)
	refreshed := grants("home")[0]
	current := nextBinding(binding)
	offer(current, refreshed.GrantID, false) // a still-negotiating predecessor owns the path
	oldOwner = attach(oldPeer)
	defer oldOwner.Close()
	for i := 1; i < SenderMaxActivePeerAttemptsPerSession; i++ {
		direct := testBinding(byte(60 + i))
		direct.PeerSlot = uint8(i)
		offer(direct, p.TURNGrantID{}, true)
	}
	offer(testBinding(85), p.TURNGrantID{}, false) // an unrelated ninth path still exceeds the session budget
	if cancelFirst {
		if err := handler.Cancel(ctx, old); err != nil {
			t.Fatal(err)
		}
	}
	current = nextBinding(current)
	offer(current, initial[0].GrantID, false) // overlapping use requires a fresh grant
	current = nextBinding(current)
	offer(current, initial[1].GrantID, false) // a different service cannot take over a live path
	current = nextBinding(current)
	offer(current, p.TURNGrantID{42}, false)
	current = nextBinding(current)
	wrongSlot := current
	wrongSlot.PeerSlot++
	offer(wrongSlot, refreshed.GrantID, false)
	current = nextBinding(current)
	direct := current
	direct.Purpose = v2signal.Recovery
	offer(direct, p.TURNGrantID{}, false)
	current = nextBinding(current)
	elapsed.Add(60) // the attached predecessor's credentials can expire before its transport closes
	failed := offer(current, refreshed.GrantID, true)
	session.admitErr = errors.New("replacement admission refused")
	attach(receiveTest(t, opened))
	if failure := receiveTest(t, session.failures); failure.operation != failed.ID() {
		t.Fatalf("wrong failed admission: %v", failure)
	}
	if len(retired) != 0 || oldPeer.closeCalls.Load() != 0 {
		t.Fatal("failed successor retired the live predecessor")
	}
	session.admitErr = nil
	session.lane = newLane
	current = nextBinding(current)
	successor := offer(current, refreshed.GrantID, true)
	newPeer := receiveTest(t, opened)
	if !cancelFirst {
		if err := handler.Cancel(ctx, old); err != nil {
			t.Fatal(err)
		}
	}
	newOwner := attach(newPeer)
	defer newOwner.Close()
	if exact := receiveTest(t, retired); exact != [2]coreSession.LaneIdentity{oldLane, newLane} {
		t.Fatalf("retired wrong predecessor or successor: %v", exact)
	}

	third := grants("home")[0]
	later := nextBinding(current)
	offer(later, third.GrantID, false) // at most one previous transport can still be draining
	candidate := v2signal.Candidate{Binding: current, Candidate: "candidate:successor"}
	body, err := v2signal.EncodeCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.HandleMessage(ctx, successor, testMessage(t, p.MessagePeerCandidate, successor.ID(), body)); err != nil {
		t.Fatal(err)
	}
	if got := receiveTest(t, newPeer.added); got.Candidate != candidate.Candidate {
		t.Fatalf("successor got %q", got.Candidate)
	}
	finishDrain()
	receiveTest(t, oldPeer.closed)
	synctest.Wait()
	// A fresh operation, attempt ID, and valid grant leave only the old path sequence to reject.
	replay := binding
	replay.AttemptID = testBinding(99).AttemptID
	offer(replay, third.GrantID, false)
	offer(nextBinding(later), third.GrantID, true)
	unattached := receiveTest(t, opened)
	stop()
	synctest.Wait()
	if len(finished) != 0 || newPeer.closeCalls.Load() != 0 || unattached.closeCalls.Load() != 1 {
		t.Fatal("sender shutdown lost ownership of a pending attempt or the installed lane's drain")
	}
}
