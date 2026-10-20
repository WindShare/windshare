package provider

import (
	"context"
	"net/netip"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/stun/v4"
	"github.com/windshare/windshare/connectivity/iceconfig"
)

const (
	TURNRouteStagger        = time.Second
	MaximumTURNBackupRoutes = 2
)

// TURNPlan is one immutable network-generation snapshot used for both admission
// and gathering. Direct candidate filters do not alter relay route semantics.
type TURNPlan struct {
	initial, backup []ice.TURNRoute
	succeeded       func(ice.TURNRoute)
}

type TURNRouteEvent struct {
	Server       string
	LocalAddress netip.Addr
	Outcome      string
}

func (c *Connection) observeTURNRoute(route TURNRouteEvent) {
	if c.request.Observe != nil {
		event := c.event("turn_route", route.Outcome)
		event.TURNRoute = &route
		c.request.Observe(event)
	}
}

func (c *Connection) configureTURN(config *ice.ProviderConfig) error {
	if c.turn == nil {
		return nil
	}
	plan := c.request.TURNPlan
	if plan == nil {
		relay, _ := c.request.ICE.Relay()
		var err error
		plan, err = NewTURNPlan(relay, nil)
		if err != nil {
			return err
		}
	}
	if plan.InitialSockets() != c.request.TURNReservation.socketLimit() {
		return ErrTURNReservation
	}
	c.turnGathering = &turnGathering{plan: plan, capacity: c.request.TURNReservation,
		selected: make(chan struct{}), observe: c.observeTURNRoute}
	config.GatherTURN = c.turnGathering.run
	config.OnTURNBinding = c.observeTURNBinding
	return nil
}

func NewTURNPlan(relay iceconfig.Relay, addresses []netip.Addr) (*TURNPlan, error) {
	plan := &TURNPlan{}
	for _, raw := range relay.URLs() {
		server, err := stun.ParseURI(raw)
		if err != nil {
			return nil, err
		}
		server.Username, server.Password = relay.Credentials().Username(), relay.Credentials().Password()
		plan.initial = append(plan.initial, ice.TURNRoute{Server: *server})
	}
	seen := make(map[netip.Addr]bool)
	var eligible []netip.Addr
	for _, address := range addresses {
		address = address.Unmap()
		if !address.Is4() || address.IsUnspecified() || address.IsLoopback() || address.IsLinkLocalUnicast() || seen[address] {
			continue
		}
		seen[address] = true
		eligible = append(eligible, address)
	}
	// Explore different exits before spending the small backup budget on
	// another protocol through the same potentially broken interface.
	for round := range eligible {
		for i, route := range plan.initial {
			route.LocalAddress = eligible[(round+i)%len(eligible)]
			plan.backup = append(plan.backup, route)
		}
	}
	return plan, nil
}

func (p *TURNPlan) InitialSockets() int { return len(p.initial) }

type turnGathering struct {
	plan            *TURNPlan
	capacity        *TURNReservation
	selected        chan struct{}
	selectOnce      sync.Once
	observe         func(TURNRouteEvent)
	mu              sync.Mutex
	selectedAddress string
	allocations     []*ice.TURNAllocation
}

func (g *turnGathering) selectPath(address string) {
	g.mu.Lock()
	g.selectedAddress = address
	allocations := g.allocations
	g.allocations = nil
	g.mu.Unlock()
	g.selectOnce.Do(func() { close(g.selected) })
	for _, allocation := range allocations {
		if allocation.Address.String() != address {
			allocation.Discard()
		}
	}
}

func (g *turnGathering) register(allocation *ice.TURNAllocation) {
	if allocation == nil {
		return
	}
	g.mu.Lock()
	selected := g.selectedAddress
	if selected == "" {
		g.allocations = append(g.allocations, allocation)
	}
	g.mu.Unlock()
	if selected != "" && selected != allocation.Address.String() {
		allocation.Discard()
	}
}

func (g *turnGathering) run(ctx context.Context, attempt ice.TURNRouteAttempt) {
	ticker := time.NewTicker(TURNRouteStagger)
	defer ticker.Stop()
	g.schedule(ctx, attempt, ticker.C)
}

func (g *turnGathering) schedule(ctx context.Context, attempt ice.TURNRouteAttempt, ticks <-chan time.Time) {
	pendingCtx, cancel := context.WithCancel(ctx)
	var work sync.WaitGroup
	defer func() { cancel(); work.Wait() }()
	results := make(chan bool, len(g.plan.initial)+MaximumTURNBackupRoutes)
	pending := 0
	start := func(route ice.TURNRoute, initial bool) {
		observe := func(outcome string) {
			g.observe(TURNRouteEvent{Server: route.Server.String(), LocalAddress: route.LocalAddress, Outcome: outcome})
		}
		release, ok := g.capacity.startRoute(initial)
		if !ok {
			observe("capacity")
			return
		}
		pending++
		observe("started")
		work.Go(func() {
			allocation := attempt(pendingCtx, route, release)
			g.register(allocation)
			if allocation != nil && g.plan.succeeded != nil {
				g.plan.succeeded(route)
			}
			switch {
			case allocation != nil:
				observe("allocated")
			case pendingCtx.Err() != nil:
				observe("canceled")
			default:
				observe("failed")
			}
			results <- allocation != nil
		})
	}
	for _, route := range g.plan.initial {
		start(route, true)
	}
	next, limit := 0, min(len(g.plan.backup), MaximumTURNBackupRoutes)
	succeeded := false
	for pending > 0 || (!succeeded && next < limit) {
		select {
		case <-ctx.Done():
			return
		case <-g.selected:
			return
		case success := <-results:
			pending--
			succeeded = succeeded || success
			if !succeeded && !success && next < limit {
				start(g.plan.backup[next], false)
				next++
			}
		case <-ticks:
			if !succeeded && next < limit {
				start(g.plan.backup[next], false)
				next++
			}
		}
	}
}
