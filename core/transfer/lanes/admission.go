package lanes

import (
	"sync"
	"time"

	"github.com/windshare/windshare/core/transfer/lanescheduling"
)

type laneAdmission struct {
	*laneState
	source   BlockSource
	permit   *lanescheduling.ReceivePermit
	release  func()
	started  time.Time
	expected time.Duration
	purpose  lanescheduling.Purpose
}

func (s *Set) admitLaneLocked(state *laneState, demand Demand, claim CapacityClaim) (*laneAdmission, <-chan struct{}, error) {
	if !state.receive.Available() {
		return nil, nil, nil
	}
	bytes := blockDemandBytes(demand)
	source := state.lane
	var releaseSource func()
	if capacity, ok := source.(BlockCapacitySource); ok {
		reserved, changed, err := capacity.TryReserveBlock(demand)
		if reserved.Source == nil {
			return nil, changed, err
		}
		source, releaseSource = reserved.Source, reserved.Release
	}
	var release func()
	if claim != nil {
		var changed <-chan struct{}
		var err error
		release, changed, err = claim()
		if release == nil {
			if releaseSource != nil {
				releaseSource()
			}
			return nil, changed, err
		}
	}
	if releaseSource != nil {
		releaseClaim := release
		release = sync.OnceFunc(func() {
			if releaseClaim != nil {
				releaseClaim()
			}
			releaseSource()
		})
	}
	permit := state.receive.TryReserve(max(1, bytes), s.now())
	if permit == nil {
		if release != nil {
			release()
		}
		return nil, nil, nil
	}
	started := s.now()
	expected := state.performance.Begin(started, bytes)
	state.inflight++
	return &laneAdmission{laneState: state, source: source, permit: permit, release: release, started: started, expected: expected, purpose: lanescheduling.Content}, nil, nil
}

func laneCompletionCost(state *laneState, bytes uint64) float64 {
	factor := 1.0
	if state.route == RouteTURN {
		factor = lanescheduling.UserRelayCostFactor
	}
	if state.route == RouteRelay {
		factor = lanescheduling.RelayCostFactor
	}
	return lanescheduling.Cost(state.performance.EstimateCompletion(bytes), factor)
}

func blockDemandBytes(demand Demand) uint64 {
	bytes, _ := demand.Descriptor.Geometry().BlockPlainLength(demand.Index)
	return uint64(bytes)
}

// Supplemental reads race the same immutable block. They never take exclusive
// ownership of a new prefix block and cannot bypass content-route authority.
func (s *Set) rescue(
	demand Demand, attempted map[Identity]struct{},
	elapsed, estimate time.Duration, claim CapacityClaim,
) *laneAdmission {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || len(attempted) >= MaxDemandAttempts {
		return nil
	}
	now, bytes := s.now(), blockDemandBytes(demand)
	best := s.rescueCandidateLocked(attempted, bytes)
	if best == nil {
		return nil
	}
	if !lanescheduling.RescueDue(elapsed, estimate, best.performance.EstimateCompletion(bytes)) {
		return nil
	}
	if !s.exploration.Acquire(lanescheduling.Rescue, now) {
		return nil
	}
	admission, _, _ := s.admitLaneLocked(best, demand, claim)
	if admission == nil {
		s.exploration.Release(lanescheduling.Rescue)
		return nil
	}
	attempted[best.identity] = struct{}{}
	s.attempts.Add(1)
	return admission
}

func (s *Set) rescueCandidateLocked(attempted map[Identity]struct{}, bytes uint64) *laneState {
	var best *laneState
	for _, state := range s.lanes {
		if _, tried := attempted[state.identity]; tried {
			continue
		}
		if _, suspended := s.contentSuspensions[state.identity.ID]; suspended || !s.contentRoutePolicy.Allows(state.route) {
			continue
		}
		if !state.receive.Available() {
			continue
		}
		if best == nil {
			best = state
			continue
		}
		if laneCompletionCost(state, bytes) < laneCompletionCost(best, bytes) ||
			(laneCompletionCost(state, bytes) == laneCompletionCost(best, bytes) && state.identity.ID < best.identity.ID) {
			best = state
		}
	}
	return best
}
