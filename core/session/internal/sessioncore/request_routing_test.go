package sessioncore

import (
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/requestlane"
	"testing"
	"time"
)

func TestRequestReservationCleanupDoesNotUpdateReplacementEpoch(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	now := time.Unix(100, 0)
	runtime.now = func() time.Time { return now }
	kind := protocolsession.MessageReleaseLease
	channel, peer := newMemoryChannelPair()
	t.Cleanup(func() { _ = peer.Close() })
	identity := sessioncontract.LaneIdentity{ID: 2, Epoch: 1}
	old, err := runtime.lanes.add(identity, channel, permissiveInboundAuthenticator(), false)
	if err != nil {
		t.Fatal(err)
	}
	old.requests = requestlane.New(time.Millisecond)
	_, reserved, estimate, err := runtime.lanes.selectRequestLane(nil, kind)
	if err != nil {
		t.Fatal(err)
	}
	call := newOperationCall(protocolsession.OperationID{}, kind, now, 0, false, false)
	call.requests.Reserve(reserved, estimate)
	if !runtime.lanes.detach(identity) {
		t.Fatal("retire old incarnation")
	}
	channel, nextPeer := newMemoryChannelPair()
	t.Cleanup(func() { _ = nextPeer.Close() })
	identity.Epoch++
	replacement, err := runtime.lanes.add(identity, channel, permissiveInboundAuthenticator(), false)
	if err != nil {
		t.Fatal(err)
	}
	call.requests.Complete(now.Add(time.Second))
	call.close()
	if got := replacement.requests.Estimate(kind, now, 0); got.Response != requestlane.InitialResponse || got.Pending != 0 {
		t.Fatalf("late completion polluted replacement: %+v", got)
	}
	if call.requests.Reserve(replacement.requests.Reserve(kind, now), estimate) {
		t.Fatal("closed call reserved")
	}
	if replacement.requests.Estimate(kind, now, 0).Pending != 0 {
		t.Fatal("closed call leaked reservation")
	}
}

func TestControlSelectionAccountsForCongestionAndExactLaneAuthority(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	secondChannel, peer := newMemoryChannelPair()
	t.Cleanup(func() { _ = peer.Close() })
	second := sessioncontract.LaneIdentity{ID: 2, Epoch: 1}
	if _, err := runtime.lanes.add(second, secondChannel, permissiveInboundAuthenticator(), false); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0)
	runtime.now = func() time.Time { return now }
	runtime.lanes.active[runtime.initial.ID].requests = requestlane.New(10 * time.Millisecond)
	runtime.lanes.active[second.ID].requests = requestlane.New(15 * time.Millisecond)
	kind := protocolsession.MessageOpenRevisions
	first, reservation, _, err := runtime.lanes.selectRequestLane(nil, kind)
	if err != nil || first.identity != runtime.initial {
		t.Fatalf("first selection %v %v", first.identity, err)
	}
	next, otherReservation, _, err := runtime.lanes.selectRequestLane(nil, kind)
	if err != nil || next.identity != second {
		t.Fatalf("pending reservation not charged: %v %v", next.identity, err)
	}
	otherReservation.Abandon()
	reservation.Abandon()
	runtime.lanes.queuedContent = func(id sessioncontract.LaneIdentity) time.Duration {
		if id == runtime.initial {
			return time.Second
		}
		return 0
	}
	next, reservation, estimate, err := runtime.lanes.selectRequestLane(nil, kind)
	if err != nil || next.identity != second || estimate.Pending != 0 {
		t.Fatalf("content queue selection: %+v %v", next.identity, err)
	}
	reservation.Abandon()
	exact, exactReservation, _, err := runtime.lanes.selectRequestLane(&runtime.initial, kind)
	if err != nil || exact.identity != runtime.initial || exactReservation != nil {
		t.Fatalf("explicit authority overridden: %+v %v", exact.identity, err)
	}
	if !runtime.lanes.detach(second) {
		t.Fatal("detach")
	}
	next, reservation, _, err = runtime.lanes.selectRequestLane(nil, kind)
	if err != nil || next.identity != runtime.initial {
		t.Fatalf("surviving relay unavailable: %+v %v", next.identity, err)
	}
	reservation.Abandon()
}
