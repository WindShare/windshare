package turnroute

import (
	"context"
	"net/netip"
	"sync"
	"time"
)

const (
	RouteStagger        = time.Second
	MaximumBackupRoutes = 2
)

// Capacity separates scheduling decisions from process-wide resource accounting.
type Capacity interface {
	StartInitialRoute() (func(), bool)
	TryBackupRoute() (func(), bool)
}

// Allocation transfers its reservation to the transport until discard or close.
type Allocation struct {
	Address netip.AddrPort
	Discard func()
}

// Attempt must release on failure; a successful allocation retains release until closed.
type Attempt func(context.Context, Route, func()) *Allocation

type Event struct {
	Server       string
	LocalAddress netip.Addr
	Outcome      string
}

func NewGathering(plan *Plan, capacity Capacity, observe func(Event)) *Gathering {
	if observe == nil {
		observe = func(Event) {}
	}
	return &Gathering{plan: plan, capacity: capacity, selected: make(chan struct{}), observe: observe}
}

type Gathering struct {
	plan            *Plan
	capacity        Capacity
	selected        chan struct{}
	selectOnce      sync.Once
	observe         func(Event)
	mu              sync.Mutex
	selectedAddress netip.AddrPort
	allocations     []*Allocation
}

func (g *Gathering) Select(address netip.AddrPort) {
	g.mu.Lock()
	g.selectedAddress = address
	allocations := g.allocations
	g.allocations = nil
	g.mu.Unlock()
	g.selectOnce.Do(func() { close(g.selected) })
	for _, allocation := range allocations {
		if allocation.Address != address {
			allocation.Discard()
		}
	}
}

func (g *Gathering) register(allocation *Allocation) {
	if allocation == nil {
		return
	}
	g.mu.Lock()
	selected := g.selectedAddress
	if !selected.IsValid() {
		g.allocations = append(g.allocations, allocation)
	}
	g.mu.Unlock()
	if selected.IsValid() && selected != allocation.Address {
		allocation.Discard()
	}
}

func (g *Gathering) Run(ctx context.Context, attempt Attempt) {
	ticker := time.NewTicker(RouteStagger)
	defer ticker.Stop()
	g.schedule(ctx, attempt, ticker.C)
}

func (g *Gathering) schedule(ctx context.Context, attempt Attempt, ticks <-chan time.Time) {
	pendingCtx, cancel := context.WithCancel(ctx)
	var work sync.WaitGroup
	defer func() { cancel(); work.Wait() }()
	results := make(chan bool, len(g.plan.initial)+MaximumBackupRoutes)
	pending := 0
	start := func(route Route, initial bool) {
		observe := func(outcome string) {
			g.observe(Event{Server: route.Server, LocalAddress: route.LocalAddress, Outcome: outcome})
		}
		var release func()
		var ok bool
		if initial {
			release, ok = g.capacity.StartInitialRoute()
		} else {
			release, ok = g.capacity.TryBackupRoute()
		}
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
	next, limit := 0, min(len(g.plan.backup), MaximumBackupRoutes)
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
