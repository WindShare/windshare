package lanes

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/windshare/windshare/core/transfer/lanescheduling"
)

type laneAdmission struct {
	*laneState
	permit  *lanescheduling.ReceivePermit
	release func()
}

type laneCandidate struct {
	state *laneState
	order uint64
}

func (s *Set) candidates(
	ctx context.Context,
	attempted map[Identity]struct{},
	bytes uint64,
	claim CapacityClaim,
) ([]*laneAdmission, bool, error) {
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil, false, ErrClosed
		}
		remaining := MaxDemandAttempts - len(attempted)
		if remaining <= 0 {
			s.mu.Unlock()
			return nil, true, nil
		}
		ordered := make([]*laneState, 0, len(s.lanes))
		untriedSuspended := false
		for _, state := range s.lanes {
			if _, alreadyAttempted := attempted[state.identity]; alreadyAttempted {
				continue
			}
			if _, suspended := s.contentSuspensions[state.identity.ID]; suspended || !s.contentRoutePolicy.Allows(state.route) {
				untriedSuspended = true
				continue
			}
			ordered = append(ordered, state)
		}
		var capacityChanged <-chan struct{}
		if len(ordered) != 0 {
			selected, changed, err := s.selectCandidatesLocked(ordered, remaining, bytes, claim)
			capacityChanged = changed
			if err != nil {
				s.mu.Unlock()
				return nil, false, err
			}
			// Registration remains inside the closed-state lock so Close cannot
			// observe a zero group while an admitted hedge is about to start.
			if len(selected) > 0 {
				s.attempts.Add(len(selected))
				s.mu.Unlock()
				return selected, false, nil
			}
		}
		waitForFirstLane := len(s.lanes) == 0 && len(attempted) == 0
		if !waitForFirstLane && !untriedSuspended && len(ordered) == 0 {
			s.mu.Unlock()
			return nil, true, nil
		}
		changed := s.admissionChanged
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-s.lifecycle.Done():
			return nil, false, ErrClosed
		case <-changed:
		case <-capacityChanged:
		}
	}
}

func (s *Set) selectCandidatesLocked(ordered []*laneState, remaining int, bytes uint64, claim CapacityClaim) ([]*laneAdmission, <-chan struct{}, error) {
	slices.SortFunc(ordered, func(left, right *laneState) int {
		return cmp.Compare(left.identity.ID, right.identity.ID)
	})
	start := int(s.cursor % uint64(len(ordered)))
	s.cursor++
	candidates := make([]laneCandidate, 0, len(ordered))
	for rank := range ordered {
		state := ordered[(start+rank)%len(ordered)]
		candidates = append(candidates, laneCandidate{state: state, order: uint64(rank)})
	}
	slices.SortFunc(candidates, func(left, right laneCandidate) int {
		if left.state.failures != right.state.failures {
			return cmp.Compare(left.state.failures, right.state.failures)
		}
		// A canceled probe only bounds speed; it cannot prove that a standby can
		// finish content before the output window fills.
		if left.state.performance.HasSuccessfulSample != right.state.performance.HasSuccessfulSample {
			if left.state.performance.HasSuccessfulSample {
				return -1
			}
			return 1
		}
		if compared := cmp.Compare(laneCompletionCost(left.state, bytes), laneCompletionCost(right.state, bytes)); compared != 0 {
			return compared
		}
		return cmp.Compare(left.order, right.order)
	})
	limit := min(s.raceWidth, len(candidates), remaining)
	selected := make([]*laneAdmission, 0, limit)
	for _, candidate := range candidates {
		if len(selected) == limit {
			break
		}
		admission, changed, err := s.admitLaneLocked(candidate.state, bytes, claim)
		if admission != nil {
			selected = append(selected, admission)
		} else if changed == nil && err == nil {
			// A busy preferred path is not evidence that a slower standby should
			// own the output frontier. Bounded probes evaluate that alternative.
			break
		}
		if changed != nil || err != nil {
			if len(selected) > 0 {
				return selected, nil, nil
			}
			return nil, changed, err
		}
	}
	return selected, nil, nil
}

func (s *Set) admitLaneLocked(state *laneState, bytes uint64, claim CapacityClaim) (*laneAdmission, <-chan struct{}, error) {
	if !state.receive.Available() {
		return nil, nil, nil
	}
	var release func()
	if claim != nil {
		var changed <-chan struct{}
		var err error
		release, changed, err = claim()
		if release == nil {
			return nil, changed, err
		}
	}
	permit := state.receive.TryReserve(max(1, bytes), s.now())
	if permit == nil {
		if release != nil {
			release()
		}
		return nil, nil, nil
	}
	state.inflight++
	state.performance.Begin(s.now(), bytes)
	return &laneAdmission{laneState: state, permit: permit, release: release}, nil, nil
}

func laneCompletionCost(state *laneState, bytes uint64) float64 {
	factor := 1.0
	if state.route == RouteTURN {
		factor = lanescheduling.UserRelayCostFactor
	}
	if state.route == RouteRelay {
		factor = lanescheduling.RelayCostFactor
	}
	return lanescheduling.Cost(state.performance.Estimate(bytes), factor)
}

func blockDemandBytes(demand Demand) uint64 {
	bytes, _ := demand.Descriptor.Geometry().BlockPlainLength(demand.Index)
	return uint64(bytes)
}

// Supplemental reads race the same immutable block. They never take exclusive
// ownership of a new prefix block and cannot bypass content-route authority.
func (s *Set) supplement(
	demand Demand, attempted map[Identity]struct{}, primary *laneState,
	elapsed, estimate time.Duration, purpose lanescheduling.Purpose, claim CapacityClaim,
) *laneAdmission {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || len(attempted) >= MaxDemandAttempts {
		return nil
	}
	now, bytes := s.now(), blockDemandBytes(demand)
	if purpose == lanescheduling.Probe && !primary.performance.HasSuccessfulSample {
		return nil
	}
	best := s.supplementCandidateLocked(attempted, bytes, purpose, now)
	if best == nil {
		return nil
	}
	if purpose == lanescheduling.Rescue && !lanescheduling.RescueDue(elapsed, estimate, best.performance.Estimate(bytes)) {
		return nil
	}
	if !s.exploration.Acquire(purpose, now) {
		return nil
	}
	admission, _, _ := s.admitLaneLocked(best, bytes, claim)
	if admission == nil {
		s.exploration.Release(purpose)
		return nil
	}
	attempted[best.identity] = struct{}{}
	s.attempts.Add(1)
	return admission
}

func (s *Set) supplementCandidateLocked(attempted map[Identity]struct{}, bytes uint64,
	purpose lanescheduling.Purpose, now time.Time) *laneState {
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
		if purpose == lanescheduling.Probe && !lanescheduling.ProbeDue(&state.performance, now) {
			continue
		}
		if best == nil {
			best = state
			continue
		}
		if purpose == lanescheduling.Probe {
			// Oldest evidence first prevents a previously sampled fallback from
			// monopolizing the exploration budget when a new path appears.
			if state.performance.LastAttempt.Before(best.performance.LastAttempt) ||
				(state.performance.LastAttempt.Equal(best.performance.LastAttempt) && state.identity.ID < best.identity.ID) {
				best = state
			}
		} else if laneCompletionCost(state, bytes) < laneCompletionCost(best, bytes) ||
			(laneCompletionCost(state, bytes) == laneCompletionCost(best, bytes) && state.identity.ID < best.identity.ID) {
			best = state
		}
	}
	return best
}
