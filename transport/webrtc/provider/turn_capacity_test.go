package provider

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
)

func turnRequest(path, replaces byte, sockets int) TURNRequest {
	var old [16]byte
	if replaces != 0 {
		old = [16]byte{replaces}
	}
	return TURNRequest{ProtocolSessionID: [16]byte{1}, PeerPathID: [16]byte{path},
		ReplacesPeerPathID: old, Service: iceconfig.Identity{Provider: "test", Account: "a", Profile: "p"}, Sockets: sockets}
}

func reserveTURN(t *testing.T, owner *TURNResourceOwner, request TURNRequest) *TURNReservation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	reservation, err := owner.Acquire(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reservation.Close)
	if err := reservation.claim(request.ProtocolSessionID, request.PeerPathID, request.Service); err != nil {
		t.Fatal(err)
	}
	return reservation
}

func awaitTURNQueue(t *testing.T, capacity *TURNCapacity, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		capacity.mu.Lock()
		length := len(capacity.queue)
		capacity.mu.Unlock()
		if length == count {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("TURN queue = %d, want %d", length, count)
		}
		runtime.Gosched()
	}
}

func TestTURNCapacityFullProcessReplacesWithoutTakingOrdinaryCapacity(t *testing.T) {
	capacity := NewTURNCapacity()
	owner := capacity.NewOwner()
	var ordinary []*TURNReservation
	for i := range MaximumTURNConnections {
		ordinary = append(ordinary, reserveTURN(t, owner, turnRequest(byte(i+1), 0, MaximumTURNSockets/MaximumTURNConnections)))
	}
	replacement := reserveTURN(t, owner, turnRequest(40, 1, ordinary[0].request.Sockets))
	if capacity.connections != MaximumTURNConnections || capacity.sockets != MaximumTURNSockets ||
		capacity.replacements != 1 || len(capacity.paths) != MaximumTURNConnections+1 {
		t.Fatalf("capacity lost or unbounded: %+v", capacity)
	}
	// The socket envelope is real: an overlap still gathers while every
	// ordinary socket reservation is in use.
	networks := make([]*turnNetwork, 0, len(ordinary)+1)
	for _, reservation := range append(ordinary, replacement) {
		network, err := newTURNNetwork(reservation)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(network.close)
		networks = append(networks, network)
		for range reservation.request.Sockets {
			if _, err := network.ListenPacket("udp4", "127.0.0.1:0"); err != nil {
				t.Fatal(err)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	pending := make(chan *TURNReservation, 1)
	failures := make(chan error, 1)
	go func() { lease, err := owner.Acquire(ctx, turnRequest(41, 2, 4)); pending <- lease; failures <- err }()
	awaitTURNQueue(t, capacity, 1)
	networks[0].close()
	next := await(t, pending)
	if err := await(t, failures); err != nil {
		t.Fatal(err)
	}
	cancel()
	t.Cleanup(next.Close)
	if replacement.predecessor != nil || next.predecessor != ordinary[1] {
		t.Fatal("handoff did not promote its successor and release the shared overlap")
	}
	if capacity.connections != MaximumTURNConnections || capacity.sockets != MaximumTURNSockets || capacity.replacements != 1 {
		t.Fatal("promotion changed steady capacity")
	}
	next.Close()
	if capacity.replacements != 0 || ordinary[1].successor != nil {
		t.Fatal("failed replacement retained capacity")
	}
	for _, network := range networks {
		network.close()
	}
	if capacity.connections != 0 || capacity.sockets != 0 || capacity.replacements != 0 || len(capacity.paths) != 0 {
		t.Fatal("capacity retained after cleanup")
	}
}

func TestTURNCapacityQueuedCancellationAndPredecessorValidation(t *testing.T) {
	capacity := NewTURNCapacity()
	owner := capacity.NewOwner()
	for i := range MaximumTURNConnections {
		reserveTURN(t, owner, turnRequest(byte(i+1), 0, 1))
	}
	for _, request := range []TURNRequest{
		turnRequest(40, 0, 1),
		{ProtocolSessionID: [16]byte{2}, PeerPathID: [16]byte{40}, ReplacesPeerPathID: [16]byte{1},
			Service: turnRequest(1, 0, 1).Service, Sockets: 1},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { lease, err := owner.Acquire(ctx, request); lease.Close(); result <- err }()
		awaitTURNQueue(t, capacity, 1)
		cancel()
		if err := await(t, result); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		awaitTURNQueue(t, capacity, 0)
	}
	wrongService := turnRequest(40, 1, 1)
	wrongService.Service.Profile = "other"
	if _, err := owner.Acquire(context.Background(), wrongService); !errors.Is(err, ErrTURNReservation) {
		t.Fatalf("unrelated service borrowed renewal capacity: %v", err)
	}
	reserveTURN(t, owner, turnRequest(40, 1, 1))
	if capacity.connections != MaximumTURNConnections || capacity.replacements != 1 {
		t.Fatal("cancelled queue leaked capacity")
	}
}

func TestTURNCapacitySeparatesLocalOwnersOfTheSameSession(t *testing.T) {
	capacity := NewTURNCapacity()
	left, right := capacity.NewOwner(), capacity.NewOwner()
	old := reserveTURN(t, left, turnRequest(1, 0, 1))
	other := reserveTURN(t, right, turnRequest(1, 0, 1))
	next := reserveTURN(t, left, turnRequest(2, 1, 1))
	old.Close()
	if next.predecessor != nil || other.closed || other.successor != nil || capacity.connections != 2 {
		t.Fatal("handoff crossed local endpoint ownership")
	}
}

func TestTURNCapacityHandoffAdmitsBothLocalEndpointsAtFullCapacity(t *testing.T) {
	capacity := NewTURNCapacity()
	left, right := capacity.NewOwner(), capacity.NewOwner()
	leftOld := reserveTURN(t, left, turnRequest(1, 0, 4))
	rightOld := reserveTURN(t, right, turnRequest(1, 0, 4))
	for i := range MaximumTURNConnections - 2 {
		reserveTURN(t, left, turnRequest(byte(i+2), 0, 4))
	}
	leftNext := reserveTURN(t, left, turnRequest(40, 1, 4))
	rightNext := reserveTURN(t, right, turnRequest(40, 1, 4))
	if capacity.connections != MaximumTURNConnections || capacity.sockets != MaximumTURNSockets || capacity.replacements != 2 {
		t.Fatal("colocated endpoints cannot complete the same handoff")
	}
	ctx := t.Context()
	pending := make(chan *TURNReservation, 1)
	failures := make(chan error, 1)
	go func() { lease, err := left.Acquire(ctx, turnRequest(41, 2, 4)); pending <- lease; failures <- err }()
	awaitTURNQueue(t, capacity, 1)
	leftOld.Close()
	awaitTURNQueue(t, capacity, 1)
	if leftNext.predecessor != nil || rightNext.predecessor != rightOld || capacity.replacements != 1 {
		t.Fatal("partial endpoint settlement released the shared handoff")
	}
	rightOld.Close()
	next := await(t, pending)
	if err := await(t, failures); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(next.Close)
	if capacity.connections != MaximumTURNConnections || capacity.sockets != MaximumTURNSockets || capacity.replacements != 1 {
		t.Fatal("completed handoff did not wake the next renewal")
	}
}

func TestTURNCapacityHandoffReservesSocketGrowthAndRollsBack(t *testing.T) {
	capacity := NewTURNCapacity()
	owner := capacity.NewOwner()
	old := reserveTURN(t, owner, turnRequest(1, 0, 2))
	next := reserveTURN(t, owner, turnRequest(2, 1, 5))
	if capacity.sockets != 5 {
		t.Fatal("new gathering envelope not funded")
	}
	next.Close()
	if capacity.sockets != 2 || capacity.replacements != 0 {
		t.Fatal("failed growth not refunded")
	}
	next = reserveTURN(t, owner, turnRequest(3, 1, 1))
	old.Close()
	if capacity.sockets != 1 || next.predecessor != nil || capacity.replacements != 0 {
		t.Fatal("handoff not settled")
	}
	next.Close()
	if capacity.sockets != 0 || capacity.connections != 0 {
		t.Fatal("last owner not released")
	}
}

func TestTURNNetworkShutdownLeavesProtocolWritableAndBoundsStalledCleanup(t *testing.T) {
	for _, stalled := range []bool{false, true} {
		t.Run(map[bool]string{false: "protocol", true: "forced"}[stalled], func(t *testing.T) {
			reservation := reserveTURN(t, NewTURNCapacity().NewOwner(), turnRequest(1, 0, 1))
			network, err := newTURNNetwork(reservation)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(network.close)
			socket, err := network.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			err, forced := network.shutdown(func() error {
				if _, err := network.ListenPacket("udp4", "127.0.0.1:0"); err == nil {
					t.Fatal("shutdown admitted a new socket")
				}
				if !stalled {
					_, err := socket.WriteTo([]byte("release"), socket.LocalAddr())
					return err
				}
				_, _, err := socket.ReadFrom(make([]byte, 32))
				return err
			})
			if forced != stalled || (!stalled && err != nil) {
				t.Fatalf("shutdown: forced=%v err=%v", forced, err)
			}
			if network.snapshot().ActiveSockets != 0 || !reservation.closed {
				t.Fatal("shutdown retained resources")
			}
		})
	}
}
