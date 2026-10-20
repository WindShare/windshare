package provider

import (
	"context"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/windshare/windshare/connectivity/iceconfig"
)

func routeFixture(t *testing.T) (*TURNPlan, *TURNReservation, *TURNCapacity) {
	t.Helper()
	credential, _ := iceconfig.Fixed("u", "p")
	relay, _ := iceconfig.NewRelay(iceconfig.Identity{Provider: "test", Account: "a", Profile: "p"}, []string{"turn:localhost:3478"}, credential)
	plan, err := NewTURNPlan(relay, []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")})
	if err != nil {
		t.Fatal(err)
	}
	capacity := NewTURNCapacity()
	request := TURNRequest{ProtocolSessionID: [16]byte{1}, PeerPathID: [16]byte{2}, Service: relay.Identity(), Sockets: plan.InitialSockets()}
	reservation, err := capacity.NewOwner().Acquire(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reservation.Close)
	if err = reservation.claim(request.ProtocolSessionID, request.PeerPathID, request.Service); err != nil {
		t.Fatal(err)
	}
	return plan, reservation, capacity
}

func TestTURNPlanHealthyRouteDoesNotWaitForExploration(t *testing.T) {
	plan, reservation, capacity := routeFixture(t)
	if plan.InitialSockets() != 1 || capacity.sockets != 1 {
		t.Fatal("interfaces multiplied initial admission")
	}
	g := &turnGathering{plan: plan, capacity: reservation, selected: make(chan struct{}), observe: func(TURNRouteEvent) {}}
	calls := 0
	g.schedule(t.Context(), func(_ context.Context, route ice.TURNRoute, release func()) *ice.TURNAllocation {
		calls++
		if route.LocalAddress.IsValid() {
			t.Fatal("healthy route explored backup")
		}
		return &ice.TURNAllocation{Address: netip.MustParseAddrPort("127.0.0.1:1"), Discard: release}
	}, nil)
	if calls != 1 {
		t.Fatal("healthy path waited for a timer or explored an interface")
	}
	g.selectPath("127.0.0.1:1")
}

func TestTURNPlanFailureExploresWithCapacityAndReleasesLosers(t *testing.T) {
	plan, reservation, capacity := routeFixture(t)
	g := &turnGathering{plan: plan, capacity: reservation, selected: make(chan struct{}), observe: func(TURNRouteEvent) {}}
	var calls atomic.Int32
	g.schedule(t.Context(), func(_ context.Context, route ice.TURNRoute, release func()) *ice.TURNAllocation {
		calls.Add(1)
		if !route.LocalAddress.IsValid() {
			release()
			return nil
		}
		return &ice.TURNAllocation{Address: netip.MustParseAddrPort("127.0.0.1:2"), Discard: release}
	}, nil)
	if calls.Load() != 2 || capacity.sockets != 1 {
		t.Fatalf("fallback admission: calls=%d sockets=%d", calls.Load(), capacity.sockets)
	}
	g.selectPath("127.0.0.1:3")
	if capacity.sockets != 0 {
		t.Fatal("unselected allocation retained capacity")
	}
}

func TestTURNPlanStallExploresAndSelectionCancelsPending(t *testing.T) {
	plan, reservation, capacity := routeFixture(t)
	g := &turnGathering{plan: plan, capacity: reservation, selected: make(chan struct{}), observe: func(TURNRouteEvent) {}}
	started := make(chan bool, 3)
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		g.schedule(t.Context(), func(ctx context.Context, route ice.TURNRoute, release func()) *ice.TURNAllocation {
			started <- route.LocalAddress.IsValid()
			<-ctx.Done()
			release()
			return nil
		}, ticks)
		close(done)
	}()
	if <-started {
		t.Fatal("backup started before primary")
	}
	ticks <- time.Now()
	if !<-started {
		t.Fatal("stalled primary prevented fallback")
	}
	g.selectPath("127.0.0.1:1")
	await(t, done)
	if capacity.sockets != 0 {
		t.Fatal("cancelled attempts retained reservation")
	}
}

func TestTURNExplorationCannotExceedSharedCapacity(t *testing.T) {
	_, reservation, capacity := routeFixture(t)
	release, ok := reservation.startRoute(true)
	if !ok {
		t.Fatal("initial reservation unavailable")
	}
	capacity.mu.Lock()
	capacity.sockets = MaximumTURNSockets
	capacity.mu.Unlock()
	if _, ok = reservation.startRoute(false); ok {
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

func TestTURNPlanDiverseExitsAndGenerationScopedHistory(t *testing.T) {
	credential, _ := iceconfig.Fixed("u", "p")
	relay, _ := iceconfig.NewRelay(iceconfig.Identity{Provider: "test", Account: "a", Profile: "p"},
		[]string{"turn:localhost:3478", "turn:localhost:3478?transport=tcp"}, credential)
	addresses := []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")}
	var planner TURNRoutePlanner
	plan, err := planner.Plan(1, relay, addresses)
	if err != nil {
		t.Fatal(err)
	}
	if plan.InitialSockets() != 2 || plan.backup[0].LocalAddress == plan.backup[1].LocalAddress {
		t.Fatal("initial admission or backup exit diversity is wrong")
	}
	success := plan.backup[2]
	plan.succeeded(success)
	sameNetwork, _ := planner.Plan(1, relay, addresses)
	if sameNetwork.backup[0] != success {
		t.Fatal("proven egress was not preferred")
	}
	nextNetwork, _ := planner.Plan(2, relay, addresses)
	plan.succeeded(success)
	newAttempt, _ := planner.Plan(2, relay, addresses)
	if nextNetwork.backup[0] != plan.backup[0] || newAttempt.backup[0] != plan.backup[0] {
		t.Fatal("obsolete network evidence contaminated fresh routing")
	}
}

func TestTURNRouteCapacityTracksHandoffEnvelope(t *testing.T) {
	capacity := NewTURNCapacity()
	owner := capacity.NewOwner()
	old := reserveTURN(t, owner, turnRequest(1, 0, 2))
	oldFirst, _ := old.startRoute(true)
	oldSecond, _ := old.startRoute(true)
	next := reserveTURN(t, owner, turnRequest(2, 1, 1))
	nextInitial, _ := next.startRoute(true)
	nextBackup, ok := next.startRoute(false)
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
