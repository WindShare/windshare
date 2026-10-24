package nativepeer

import (
	"context"
	"net/netip"
	"slices"
	"testing"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/socketauthority"
	"github.com/windshare/windshare/connectivity/v2signal"
)

func TestReceiverFairnessCannotPromoteExplorationPastEssentialWork(t *testing.T) {
	gate, _, _ := newAdmissionFixture(t, 0)
	pool := socketauthority.New(socketauthority.Config{Capacity: 8})
	a, b := &NativePeerConnectivity{}, &NativePeerConnectivity{}
	var granted []v2signal.ConnectionPurpose
	var waiters []*admissionWaiter
	for i, purpose := range []v2signal.ConnectionPurpose{v2signal.InitialConnectivity, v2signal.Recovery, v2signal.ThroughputExploration} {
		request, err := pool.Request([16]byte{1}, 1, [16]byte{byte(i + 1)}, []netip.Addr{netip.MustParseAddr("127.0.0.1")})
		if err != nil {
			t.Fatal(err)
		}
		owner := a
		if purpose == v2signal.ThroughputExploration {
			owner = b
		}
		waiters = append(waiters, &admissionWaiter{purpose: purpose, owner: owner, sockets: request, ready: make(chan struct{}), started: gate.clock.Now(), observe: func(facts AdmissionFacts) {
			if facts.Kind == AdmissionGranted {
				granted = append(granted, facts.Purpose)
			}
		}})
	}
	gate.mu.Lock()
	gate.queue = append(gate.queue, waiters...)
	gate.drainLocked()
	gate.mu.Unlock()
	for _, waiter := range waiters {
		if waiter.permit != nil {
			defer waiter.permit.release()
		}
	}
	if !slices.Equal(granted, []v2signal.ConnectionPurpose{v2signal.InitialConnectivity, v2signal.Recovery, v2signal.ThroughputExploration}) {
		t.Fatalf("admission order = %v", granted)
	}
}

func TestEssentialReceiverReclaimsAnAdmittedOptionalSocketOwner(t *testing.T) {
	_, _, makeNative := newAdmissionFixture(t, 0)
	n := makeNative()
	n.config.Sockets = socketauthority.New(socketauthority.Config{Capacity: 3})
	first := startActual(t, n, 1)
	defer first.Close()
	n.SetAdmitted(attemptFor(1).ProtocolSessionID, attemptFor(1).Binding.PeerPathID, true)
	request := attemptFor(2)
	request.Binding.Purpose = v2signal.ThroughputExploration
	optional, err := n.NewPeerConnection(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	defer optional.Close()
	n.SetAdmitted(request.ProtocolSessionID, request.Binding.PeerPathID, true)
	third := startActual(t, n, 3)
	defer third.Close()
	n.SetAdmitted(attemptFor(3).ProtocolSessionID, attemptFor(3).Binding.PeerPathID, true)
	fourth := actualResult(t, queuedActual(t, n, 4, t.Context()))
	if fourth.err != nil {
		t.Fatal(fourth.err)
	}
	defer fourth.peer.Close()
	if optional.ConnectionState() != pion.PeerConnectionStateClosed {
		t.Fatal("essential start escaped before optional provider physically closed")
	}
	if first.ConnectionState() == pion.PeerConnectionStateClosed || third.ConnectionState() == pion.PeerConnectionStateClosed {
		t.Fatal("essential paths were reclaimed")
	}
}

func TestOptionalPreparationDefersWithoutSpendingReservedEssentialOpportunity(t *testing.T) {
	gate, _, makeNative := newAdmissionFixture(t, 0)
	n := makeNative()
	n.config.Sockets = socketauthority.New(socketauthority.Config{Capacity: 2})
	first := startActual(t, n, 1)
	defer first.Close()
	n.SetAdmitted(attemptFor(1).ProtocolSessionID, attemptFor(1).Binding.PeerPathID, true)
	request := attemptFor(2)
	request.Binding.Purpose = v2signal.ThroughputExploration
	if prepared, err := n.PrepareAttempt(t.Context(), request); err == nil {
		prepared.Close()
		t.Fatal("exploration consumed the last socket opportunity")
	}
	gate.mu.Lock()
	if len(gate.queue) != 0 || gate.starts != StartsPerWindow-1 {
		t.Error("deferral spent starts or left queued work")
	}
	gate.mu.Unlock()
	second := startActual(t, n, 3)
	defer second.Close()
	n.CloseSession(request.ProtocolSessionID)
	if err := n.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
