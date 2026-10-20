package provider

import (
	"cmp"
	"net/netip"
	"slices"
	"sync"

	"github.com/pion/ice/v4"
	"github.com/windshare/windshare/connectivity/iceconfig"
)

const maximumTURNRouteHistory = 64

// TURNRoutePlanner retains only proven control-transport exits for the current
// network. History orders optional attempts; it never delays the OS route.
type TURNRoutePlanner struct {
	mu         sync.Mutex
	generation uint64
	successful map[string]netip.Addr
}

func (p *TURNRoutePlanner) Plan(generation uint64, relay iceconfig.Relay, addresses []netip.Addr) (*TURNPlan, error) {
	plan, err := NewTURNPlan(relay, addresses)
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
		rank := func(route ice.TURNRoute) int {
			if p.successful[route.Server.String()] == route.LocalAddress {
				return 0
			}
			return 1
		}
		slices.SortStableFunc(plan.backup, func(a, b ice.TURNRoute) int { return cmp.Compare(rank(a), rank(b)) })
	}
	plan.succeeded = func(route ice.TURNRoute) { p.remember(generation, route) }
	return plan, nil
}

func (p *TURNRoutePlanner) remember(generation uint64, route ice.TURNRoute) {
	if !route.LocalAddress.IsValid() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if generation != p.generation {
		return
	}
	key := route.Server.String()
	if _, known := p.successful[key]; known || len(p.successful) < maximumTURNRouteHistory {
		p.successful[key] = route.LocalAddress
	}
}
