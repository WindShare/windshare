package lanes

import "slices"

// RouteSupply is the lifetime of an owner actively provisioning or recovering
// permitted content paths. It is not evidence that any existing lane is usable.
type RouteSupply struct {
	set *Set
}

// ExpectRoute lets queued demand survive a gap between connection generations.
// The connectivity owner must close the capability when its recovery work ends;
// a forbidden route can never acquire one, regardless of transport liveness.
func (s *Set) ExpectRoute(route Route) (*RouteSupply, error) {
	if !s.contentRoutePolicy.Allows(route) {
		return nil, ErrNoContentRoute
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if len(s.routeSupplies) >= MaxLogicalLanes {
		return nil, ErrBudget
	}
	supply := &RouteSupply{set: s}
	s.routeSupplies[supply] = struct{}{}
	s.notifyAdmissionChangedLocked()
	return supply, nil
}

func (supply *RouteSupply) Close() {
	if supply == nil || supply.set == nil {
		return
	}
	s := supply.set
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, live := s.routeSupplies[supply]; live {
		delete(s.routeSupplies, supply)
		s.notifyAdmissionChangedLocked()
	}
}

// RecoveryDemand identifies exact live generations that exhausted a queued
// immutable read. Healthy in-flight work and consumer backpressure do not grant
// recovery authority. The connection owner rechecks the identity before acting.
func (s *Set) RecoveryDemand() []Identity {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []Identity
	for _, pending := range s.pending {
		if pending.done || pending.ctx.Err() != nil || pending.blocked != CapacityRoute {
			continue
		}
		for identity := range pending.attempted {
			state := s.lanes[identity.ID]
			if state != nil && state.identity == identity && s.contentRoutePolicy.Allows(state.route) &&
				!slices.Contains(result, identity) {
				result = append(result, identity)
			}
		}
	}
	return result
}
