package lanes

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/windshare/windshare/core/transfer/lanescheduling"
)

type laneAdmission struct {
	*laneState
	source       BlockSource
	permit       *lanescheduling.ReceivePermit
	release      func()
	started      time.Time
	expected     lanescheduling.DeliveryEstimate
	rescueReason lanescheduling.RescueReason
	purpose      lanescheduling.Purpose
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
	started := s.now()
	expected := state.receive.EstimateCompletion(bytes, started)
	permit := state.receive.TryReserve(max(1, bytes), started)
	if permit == nil {
		if release != nil {
			release()
		}
		return nil, nil, nil
	}
	state.performance.Begin(started, bytes)
	state.inflight++
	return &laneAdmission{laneState: state, source: source, permit: permit, release: release, started: started, expected: expected, purpose: lanescheduling.Content}, nil, nil
}

func laneCompletionCost(state *laneState, bytes uint64, now time.Time) float64 {
	factor := 1.0
	if state.route == RouteTURN {
		factor = lanescheduling.UserRelayCostFactor
	}
	if state.route == RouteRelay {
		factor = lanescheduling.RelayCostFactor
	}
	return lanescheduling.Cost(lanescheduling.ConservativeDuration(state.receive.EstimateCompletion(bytes, now)), factor)
}

func blockDemandBytes(demand Demand) uint64 {
	bytes, _ := demand.Descriptor.Geometry().BlockPlainLength(demand.Index)
	return uint64(bytes)
}

// A rescue is another intent in the shared allocation queue, not a lane-local
// backlog. Its owner still controls whether any admitted attempt is dispatched.
func (s *Set) queueRescue(ctx context.Context, request Request, attempted map[Identity]struct{}, primary *laneAdmission) *pendingAdmission {
	s.mu.Lock()
	defer s.mu.Unlock()
	if request.Order != nil && request.Order().Distance > 0 {
		return nil
	}
	now, bytes := s.now(), blockDemandBytes(request.Demand)
	var best *laneState
	for _, candidate := range s.lanes {
		if _, tried := attempted[candidate.identity]; tried {
			continue
		}
		if s.contentSuspensions[candidate.identity.ID] != nil || !s.contentRoutePolicy.Allows(candidate.route) {
			continue
		}
		if best == nil || compareLaneDelivery(candidate, best, bytes, now) < 0 {
			best = candidate
		}
	}
	// Healthy transfers and single-path sessions need no queued rescue object.
	// Once due, allocation rechecks this evidence when physical credit arrives.
	if best == nil || lanescheduling.RescueDecision(primary.permit.Progress(now), primary.expected, best.receive.EstimateCompletion(bytes, now)) == "" {
		return nil
	}
	s.allocationSequence++
	pending := &pendingAdmission{ctx: ctx, request: request, attempted: maps.Clone(attempted),
		rescueOf: primary, resolved: make(chan struct{}), queuedAt: s.now(), sequence: s.allocationSequence}
	s.pending = append(s.pending, pending)
	return pending
}

func (s *Set) takeRescue(pending *pendingAdmission) (*laneAdmission, <-chan struct{}, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allocatePendingLocked()
	if !pending.done {
		return nil, pending.resolved, false
	}
	s.pending = slices.DeleteFunc(s.pending, func(item *pendingAdmission) bool { return item == pending })
	if len(pending.selected) == 0 {
		return nil, nil, true
	}
	selected := pending.selected[0]
	pending.selected = nil
	return selected, nil, true
}

func (s *Set) withdrawRescue(pending *pendingAdmission) {
	if pending == nil {
		return
	}
	s.mu.Lock()
	s.pending = slices.DeleteFunc(s.pending, func(item *pendingAdmission) bool { return item == pending })
	var unused *laneAdmission
	var settlement *SettlementSummary
	if len(pending.selected) != 0 {
		unused = pending.selected[0]
		pending.selected = nil
		// A primary can win after another allocator reserves the rescue credit but
		// before this round observes it. Settle that ownership without dispatch.
		unused.performance.Complete(s.now(), blockDemandBytes(pending.request.Demand), false)
		unused.permit.Close()
		unused.inflight--
		settlement = s.settleLaneLocked(unused.laneState)
		s.exploration.Release(lanescheduling.Rescue)
		s.notifyAdmissionChangedLocked()
	}
	s.mu.Unlock()
	if unused != nil {
		s.publishLaneSettlement(settlement)
		unused.releaseCapacity()
		s.attempts.Done()
	}
}

func (s *Set) allocateRescueLocked(pending *pendingAdmission, now time.Time) {
	pending.blocked, pending.capacityChanged = CapacityNetwork, nil
	if pending.order.Distance > 0 || len(pending.attempted) >= MaxDemandAttempts {
		return
	}
	ordered, _ := s.eligiblePendingLanesLocked(pending)
	if len(ordered) == 0 {
		return
	}
	bytes := blockDemandBytes(pending.request.Demand)
	s.orderLanesLocked(ordered, bytes)
	best := ordered[0]
	reason := lanescheduling.RescueDecision(pending.rescueOf.permit.Progress(now), pending.rescueOf.expected, best.receive.EstimateCompletion(bytes, now))
	if reason == "" || !best.receive.Available() {
		return
	}
	if !s.exploration.Acquire(lanescheduling.Rescue, now) {
		return
	}
	admission, wake, err := s.admitLaneLocked(best, pending.request.Demand, pending.request.Claim)
	pending.capacityChanged = wake
	if admission == nil {
		s.exploration.Release(lanescheduling.Rescue)
		if err != nil {
			pending.err, pending.done = err, true
		}
		return
	}
	admission.purpose, admission.rescueReason = lanescheduling.Rescue, reason
	s.attempts.Add(1)
	pending.selected, pending.done = []*laneAdmission{admission}, true
}
