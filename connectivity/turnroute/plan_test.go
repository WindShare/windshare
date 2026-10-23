package turnroute

import (
	"context"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
)

// The scheduling tests inject capacity; process admission has its own independent tests.
type routeCapacity struct {
	mu      sync.Mutex
	sockets int
	initial int
}

func (c *routeCapacity) start(initial bool) (func(), bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if initial {
		if c.initial == 0 {
			return nil, false
		}
		c.initial--
	} else {
		c.sockets++
	}
	var once sync.Once
	return func() { once.Do(func() { c.mu.Lock(); c.sockets--; c.mu.Unlock() }) }, true
}
func (c *routeCapacity) StartInitialRoute() (func(), bool) { return c.start(true) }
func (c *routeCapacity) TryBackupRoute() (func(), bool)    { return c.start(false) }
func routeFixture(t *testing.T) (*Plan, *routeCapacity) {
	t.Helper()
	credential, _ := iceconfig.Fixed("u", "p")
	relay, _ := iceconfig.NewRelay(iceconfig.Identity{Provider: "test", Account: "a", Profile: "p"}, []string{"turn:localhost:3478"}, credential)
	plan, err := NewPlan(relay, []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")})
	if err != nil {
		t.Fatal(err)
	}
	capacity := &routeCapacity{sockets: plan.InitialSockets(), initial: plan.InitialSockets()}
	return plan, capacity
}

func TestTURNPlanHealthyRouteDoesNotWaitForExploration(t *testing.T) {
	plan, capacity := routeFixture(t)
	if plan.InitialSockets() != 1 || capacity.sockets != 1 {
		t.Fatal("interfaces multiplied initial admission")
	}
	g := NewGathering(plan, capacity, nil)
	calls := 0
	g.schedule(t.Context(), func(_ context.Context, route Route, release func()) *Allocation {
		calls++
		if route.LocalAddress.IsValid() {
			t.Fatal("healthy route explored backup")
		}
		return &Allocation{Address: netip.MustParseAddrPort("127.0.0.1:1"), Discard: release}
	}, nil)
	if calls != 1 {
		t.Fatal("healthy path waited for a timer or explored an interface")
	}
	g.Select(netip.MustParseAddrPort("127.0.0.1:1"))
}

func TestTURNPlanFailureExploresWithCapacityAndReleasesLosers(t *testing.T) {
	plan, capacity := routeFixture(t)
	g := NewGathering(plan, capacity, nil)
	var calls atomic.Int32
	g.schedule(t.Context(), func(_ context.Context, route Route, release func()) *Allocation {
		calls.Add(1)
		if !route.LocalAddress.IsValid() {
			release()
			return nil
		}
		return &Allocation{Address: netip.MustParseAddrPort("127.0.0.1:2"), Discard: release}
	}, nil)
	if calls.Load() != 2 || capacity.sockets != 1 {
		t.Fatalf("fallback admission: calls=%d sockets=%d", calls.Load(), capacity.sockets)
	}
	g.Select(netip.MustParseAddrPort("127.0.0.1:3"))
	if capacity.sockets != 0 {
		t.Fatal("unselected allocation retained capacity")
	}
}

func TestTURNPlanStallExploresAndSelectionCancelsPending(t *testing.T) {
	plan, capacity := routeFixture(t)
	g := NewGathering(plan, capacity, nil)
	started := make(chan bool, 3)
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		g.schedule(t.Context(), func(ctx context.Context, route Route, release func()) *Allocation {
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
	g.Select(netip.MustParseAddrPort("127.0.0.1:1"))
	await(t, done)
	if capacity.sockets != 0 {
		t.Fatal("cancelled attempts retained reservation")
	}
}

func TestTURNPlanDiverseExitsAndGenerationScopedHistory(t *testing.T) {
	credential, _ := iceconfig.Fixed("u", "p")
	relay, _ := iceconfig.NewRelay(iceconfig.Identity{Provider: "test", Account: "a", Profile: "p"},
		[]string{"turn:localhost:3478", "turn:localhost:3478?transport=tcp"}, credential)
	addresses := []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")}
	var planner Planner
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

func await[T any](t *testing.T, values <-chan T) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(time.Second):
		t.Fatal("route scheduling timed out")
		var zero T
		return zero
	}
}
