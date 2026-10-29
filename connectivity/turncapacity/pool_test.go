package turncapacity

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
)

func turnRequest(path, replaces byte, sockets int) Request {
	var old [16]byte
	if replaces != 0 {
		old = [16]byte{replaces}
	}
	return Request{ProtocolSessionID: [16]byte{1}, PeerPathID: [16]byte{path},
		ReplacesPeerPathID: old, Service: iceconfig.Identity{Provider: "test", Account: "a", Profile: "p"}, Sockets: sockets}
}

func reserveTURN(t *testing.T, owner *Owner, request Request) *Reservation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	reservation, err := owner.Acquire(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reservation.Close)
	if err := reservation.Claim(request.ProtocolSessionID, request.PeerPathID, request.Service); err != nil {
		t.Fatal(err)
	}
	return reservation
}

func assertTURNQueueLength(t *testing.T, capacity *Pool, count int) {
	t.Helper()
	synctest.Wait()
	capacity.mu.Lock()
	length := len(capacity.queue)
	capacity.mu.Unlock()
	if length != count {
		t.Fatalf("capacity queue length=%d, want %d", length, count)
	}
}

func TestTURNCapacityFullProcessReplacesWithoutTakingOrdinaryCapacity(t *testing.T) {
	synctest.Test(t, testTURNCapacityFullProcessReplacesWithoutTakingOrdinaryCapacity)
}

func testTURNCapacityFullProcessReplacesWithoutTakingOrdinaryCapacity(t *testing.T) {
	capacity := NewPool()
	owner := capacity.NewOwner()
	var ordinary []*Reservation
	for i := range MaximumConnections {
		ordinary = append(ordinary, reserveTURN(t, owner, turnRequest(byte(i+1), 0, MaximumSockets/MaximumConnections)))
	}
	replacement := reserveTURN(t, owner, turnRequest(40, 1, ordinary[0].request.Sockets))
	if capacity.connections != MaximumConnections || capacity.sockets != MaximumSockets ||
		capacity.replacements != 1 || len(capacity.paths) != MaximumConnections+1 {
		t.Fatalf("capacity lost or unbounded: %+v", capacity)
	}
	ctx, cancel := context.WithCancel(context.Background())
	pending := make(chan *Reservation, 1)
	failures := make(chan error, 1)
	go func() { lease, err := owner.Acquire(ctx, turnRequest(41, 2, 4)); pending <- lease; failures <- err }()
	assertTURNQueueLength(t, capacity, 1)
	ordinary[0].Close()
	next := await(t, pending)
	if err := await(t, failures); err != nil {
		t.Fatal(err)
	}
	cancel()
	t.Cleanup(next.Close)
	if replacement.predecessor != nil || next.predecessor != ordinary[1] {
		t.Fatal("handoff did not promote its successor and release the shared overlap")
	}
	if capacity.connections != MaximumConnections || capacity.sockets != MaximumSockets || capacity.replacements != 1 {
		t.Fatal("promotion changed steady capacity")
	}
	next.Close()
	if capacity.replacements != 0 || ordinary[1].successor != nil {
		t.Fatal("failed replacement retained capacity")
	}
	for _, reservation := range append(ordinary, replacement) {
		reservation.Close()
	}
	if capacity.connections != 0 || capacity.sockets != 0 || capacity.replacements != 0 || len(capacity.paths) != 0 {
		t.Fatal("capacity retained after cleanup")
	}
}

func TestTURNCapacityQueuedCancellationAndPredecessorValidation(t *testing.T) {
	synctest.Test(t, testTURNCapacityQueuedCancellationAndPredecessorValidation)
}

func testTURNCapacityQueuedCancellationAndPredecessorValidation(t *testing.T) {
	capacity := NewPool()
	owner := capacity.NewOwner()
	for i := range MaximumConnections {
		reserveTURN(t, owner, turnRequest(byte(i+1), 0, 1))
	}
	for _, request := range []Request{
		turnRequest(40, 0, 1),
		{ProtocolSessionID: [16]byte{2}, PeerPathID: [16]byte{40}, ReplacesPeerPathID: [16]byte{1},
			Service: turnRequest(1, 0, 1).Service, Sockets: 1},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { lease, err := owner.Acquire(ctx, request); lease.Close(); result <- err }()
		assertTURNQueueLength(t, capacity, 1)
		cancel()
		if err := await(t, result); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		assertTURNQueueLength(t, capacity, 0)
	}
	wrongService := turnRequest(40, 1, 1)
	wrongService.Service.Profile = "other"
	if _, err := owner.Acquire(context.Background(), wrongService); !errors.Is(err, ErrReservation) {
		t.Fatalf("unrelated service borrowed renewal capacity: %v", err)
	}
	reserveTURN(t, owner, turnRequest(40, 1, 1))
	if capacity.connections != MaximumConnections || capacity.replacements != 1 {
		t.Fatal("cancelled queue leaked capacity")
	}
}

func TestTURNCapacitySeparatesLocalOwnersOfTheSameSession(t *testing.T) {
	capacity := NewPool()
	left, right := capacity.NewOwner(), capacity.NewOwner()
	old := reserveTURN(t, left, turnRequest(1, 0, 1))
	other := reserveTURN(t, right, turnRequest(1, 0, 1))
	next := reserveTURN(t, left, turnRequest(2, 1, 1))
	old.Close()
	if next.predecessor != nil || other.state == reservationClosed || other.successor != nil || capacity.connections != 2 {
		t.Fatal("handoff crossed local endpoint ownership")
	}
}

func TestTURNCapacityHandoffAdmitsBothLocalEndpointsAtFullCapacity(t *testing.T) {
	synctest.Test(t, testTURNCapacityHandoffAdmitsBothLocalEndpointsAtFullCapacity)
}

func testTURNCapacityHandoffAdmitsBothLocalEndpointsAtFullCapacity(t *testing.T) {
	capacity := NewPool()
	left, right := capacity.NewOwner(), capacity.NewOwner()
	leftOld := reserveTURN(t, left, turnRequest(1, 0, 4))
	rightOld := reserveTURN(t, right, turnRequest(1, 0, 4))
	for i := range MaximumConnections - 2 {
		reserveTURN(t, left, turnRequest(byte(i+2), 0, 4))
	}
	leftNext := reserveTURN(t, left, turnRequest(40, 1, 4))
	rightNext := reserveTURN(t, right, turnRequest(40, 1, 4))
	if capacity.connections != MaximumConnections || capacity.sockets != MaximumSockets || capacity.replacements != 2 {
		t.Fatal("colocated endpoints cannot complete the same handoff")
	}
	ctx := t.Context()
	pending := make(chan *Reservation, 1)
	failures := make(chan error, 1)
	go func() { lease, err := left.Acquire(ctx, turnRequest(41, 2, 4)); pending <- lease; failures <- err }()
	assertTURNQueueLength(t, capacity, 1)
	leftOld.Close()
	assertTURNQueueLength(t, capacity, 1)
	if leftNext.predecessor != nil || rightNext.predecessor != rightOld || capacity.replacements != 1 {
		t.Fatal("partial endpoint settlement released the shared handoff")
	}
	rightOld.Close()
	next := await(t, pending)
	if err := await(t, failures); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(next.Close)
	if capacity.connections != MaximumConnections || capacity.sockets != MaximumSockets || capacity.replacements != 1 {
		t.Fatal("completed handoff did not wake the next renewal")
	}
}

func TestTURNCapacityHandoffReservesSocketGrowthAndRollsBack(t *testing.T) {
	capacity := NewPool()
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

func TestTURNExplorationCannotExceedSharedCapacity(t *testing.T) {
	capacity := NewPool()
	reservation := reserveTURN(t, capacity.NewOwner(), turnRequest(1, 0, 1))
	release, ok := reservation.StartInitialRoute()
	if !ok {
		t.Fatal("initial reservation unavailable")
	}
	capacity.mu.Lock()
	capacity.sockets = MaximumSockets
	capacity.mu.Unlock()
	if _, ok = reservation.TryBackupRoute(); ok {
		t.Fatal("optional exploration exceeded process capacity")
	}
	capacity.mu.Lock()
	capacity.sockets = 1
	capacity.mu.Unlock()
	release()
	release()
	if capacity.sockets != 0 {
		t.Fatal("route release is not idempotent")
	}
}

func TestTURNRouteCapacityTracksHandoffEnvelope(t *testing.T) {
	capacity := NewPool()
	owner := capacity.NewOwner()
	old := reserveTURN(t, owner, turnRequest(1, 0, 2))
	oldFirst, _ := old.StartInitialRoute()
	oldSecond, _ := old.StartInitialRoute()
	next := reserveTURN(t, owner, turnRequest(2, 1, 1))
	nextInitial, _ := next.StartInitialRoute()
	nextBackup, ok := next.TryBackupRoute()
	if !ok || capacity.sockets != 2 {
		t.Fatal("backup double-counted the handoff envelope")
	}
	oldFirst()
	oldSecond()
	if capacity.sockets != 2 {
		t.Fatal("predecessor release stole successor capacity")
	}
	old.Close()
	nextBackup()
	if capacity.sockets != 1 || capacity.replacements != 0 {
		t.Fatal("handoff did not transfer dynamic socket ownership")
	}
	nextInitial()
	next.Close()
	if capacity.sockets != 0 || capacity.connections != 0 {
		t.Fatal("handoff leaked capacity")
	}
}

func await[T any](t *testing.T, values <-chan T) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(time.Second):
		t.Fatal("capacity transition timed out")
		var zero T
		return zero
	}
}
