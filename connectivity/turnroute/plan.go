// Package turnroute plans and schedules TURN egress independently of an ICE implementation.
package turnroute

import (
	"net/netip"
	"slices"

	"github.com/windshare/windshare/connectivity/iceconfig"
)

// Route names one server and an optional source-bound exit. The zero address
// leaves egress to the OS, preserving the fast path before backup exploration.
type Route struct {
	Server       string
	LocalAddress netip.Addr
}

// Plan is the immutable snapshot shared by admission and gathering.
type Plan struct {
	relay           iceconfig.Relay
	initial, backup []Route
	succeeded       func(Route)
}

// Matches prevents a reservation and gatherer from silently using different authority.
func (p *Plan) Matches(relay iceconfig.Relay) bool {
	return p != nil && p.relay.Identity() == relay.Identity() &&
		p.relay.Credentials() == relay.Credentials() && slices.Equal(p.relay.URLs(), relay.URLs())
}

func NewPlan(relay iceconfig.Relay, addresses []netip.Addr) (*Plan, error) {
	if _, err := iceconfig.NewRelay(relay.Identity(), relay.URLs(), relay.Credentials()); err != nil {
		return nil, err
	}
	plan := &Plan{relay: relay}
	for _, server := range relay.URLs() {
		plan.initial = append(plan.initial, Route{Server: server})
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

func (p *Plan) InitialSockets() int { return len(p.initial) }
