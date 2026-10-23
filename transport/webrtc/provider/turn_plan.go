package provider

import (
	"context"
	"errors"
	"net/netip"

	"github.com/pion/ice/v4"
	"github.com/pion/stun/v4"
	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/connectivity/turnroute"
)

var errTURNConfiguration = errors.New("TURN plan and reservation must match the connection")

// TURNReservation is the capability an admitted attempt needs. The provider
// cannot change queue policy, mint capacity or inspect other paths' reservations.
type TURNReservation interface {
	turnroute.Capacity
	Claim([16]byte, [16]byte, iceconfig.Identity) error
	SocketLimit() int
	Close()
}

// TURNConfig keeps the admitted plan and its capacity together through handoff.
type TURNConfig struct {
	Plan        *turnroute.Plan
	Reservation TURNReservation
}

// The adapter consumes scheduling behavior without accessing its policy state.
type turnGatherer interface {
	Run(context.Context, turnroute.Attempt)
	Select(netip.AddrPort)
}

func (c *Connection) observeTURNRoute(route turnroute.Event) {
	if c.request.Observe != nil {
		event := c.event("turn_route", route.Outcome)
		event.TURNRoute = &route
		c.request.Observe(event)
	}
}

func (c *Connection) configureTURN(config *ice.ProviderConfig) {
	if c.turn == nil {
		return
	}
	request := c.request.TURN
	relay, _ := c.request.ICE.Relay()
	c.turnGathering = turnroute.NewGathering(request.Plan, request.Reservation, c.observeTURNRoute)
	config.GatherTURN = func(ctx context.Context, attempt ice.TURNRouteAttempt) {
		c.turnGathering.Run(ctx, func(ctx context.Context, route turnroute.Route, release func()) *turnroute.Allocation {
			server, err := stun.ParseURI(route.Server)
			if err != nil {
				release()
				return nil
			}
			server.Username, server.Password = relay.Credentials().Username(), relay.Credentials().Password()
			allocation := attempt(ctx, ice.TURNRoute{Server: *server, LocalAddress: route.LocalAddress}, release)
			if allocation == nil {
				return nil
			}
			return &turnroute.Allocation{Address: allocation.Address, Discard: allocation.Discard}
		})
	}
	config.OnTURNBinding = c.observeTURNBinding
}
