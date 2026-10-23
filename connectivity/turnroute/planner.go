package turnroute

import (
	"cmp"
	"net/netip"
	"slices"
	"sync"

	"github.com/windshare/windshare/connectivity/iceconfig"
)

const maximumRouteHistory = 64

// Planner retains only proven control-transport exits for the current
// network. History orders optional attempts; it never delays the OS route.
type Planner struct {
	mu         sync.Mutex
	generation uint64
	successful map[string]netip.Addr
}

func (p *Planner) Plan(generation uint64, relay iceconfig.Relay, addresses []netip.Addr) (*Plan, error) {
	plan, err := NewPlan(relay, addresses)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if generation > p.generation || p.successful == nil {
		p.generation = generation
		p.successful = make(map[string]netip.Addr)
	}
	if generation == p.generation {
		rank := func(route Route) int {
			if p.successful[route.Server] == route.LocalAddress {
				return 0
			}
			return 1
		}
		slices.SortStableFunc(plan.backup, func(a, b Route) int { return cmp.Compare(rank(a), rank(b)) })
	}
	plan.succeeded = func(route Route) { p.remember(generation, route) }
	return plan, nil
}

func (p *Planner) remember(generation uint64, route Route) {
	if !route.LocalAddress.IsValid() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if generation != p.generation {
		return
	}
	key := route.Server
	if _, known := p.successful[key]; known || len(p.successful) < maximumRouteHistory {
		p.successful[key] = route.LocalAddress
	}
}
