package transfer

import (
	"math"

	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/core/session/protocolsession"
)

// LaneSettlementObservationCapacity is the exact number of summaries retained
// while the owner-selected consumer is not receiving. Zero disables settlement
// accounting and stream allocation.
type LaneSettlementObservationCapacity int

const DefaultLaneSettlementObservationCapacity = LaneSettlementObservationCapacity(MaxLogicalLanes)

// LaneRoute is the authenticated content route admitted for one lane
// incarnation. It is captured at admission because lane numbers carry no
// transport meaning and may be reused across epochs.
type LaneRoute uint8

const (
	LaneRouteRelay LaneRoute = iota + 1
	LaneRouteDirect
)

func (route LaneRoute) valid() bool {
	return route == LaneRouteRelay || route == LaneRouteDirect
}

// LaneSettlementSummary is deliberately aggregate and text-free. The source
// contract excludes file and block identities so diagnostics cannot become a
// second content-access path.
type LaneSettlementSummary struct {
	ProtocolSessionID   protocolsession.ProtocolSessionID
	Route               LaneRoute
	Lane                LaneIdentity
	DeliveredBlocks     uint64
	DeliveredBytes      uint64
	FailedBlockAttempts uint64
	ReassignedBlocks    uint64
	Incomplete          bool
}

// LaneSettlementObservationLoss names every producer-owned omission without
// merging lane identities into a synthetic summary.
type LaneSettlementObservationLoss struct {
	CapacityDropped uint64
}

func (loss LaneSettlementObservationLoss) Total() uint64 {
	return loss.CapacityDropped
}

type LaneSettlementObservationCompletion struct {
	Enqueued uint64
	Loss     LaneSettlementObservationLoss
}

type laneSettlementCounters struct {
	deliveredBlocks     uint64
	deliveredBytes      uint64
	failedBlockAttempts uint64
	reassignedBlocks    uint64
	incomplete          bool
}

func (counters *laneSettlementCounters) addDelivered(bytes uint64) {
	if counters == nil {
		return
	}
	saturatingLaneCounter(&counters.deliveredBlocks, 1, &counters.incomplete)
	saturatingLaneCounter(&counters.deliveredBytes, bytes, &counters.incomplete)
}

func (counters *laneSettlementCounters) addFailure() {
	if counters != nil {
		saturatingLaneCounter(&counters.failedBlockAttempts, 1, &counters.incomplete)
	}
}

func (counters *laneSettlementCounters) addReassignment() {
	if counters != nil {
		saturatingLaneCounter(&counters.reassignedBlocks, 1, &counters.incomplete)
	}
}

func saturatingLaneCounter(counter *uint64, delta uint64, incomplete *bool) {
	if delta > math.MaxUint64-*counter {
		*counter = math.MaxUint64
		*incomplete = true
		return
	}
	*counter += delta
}

func laneSettlementCompletion(completion observationstream.Completion) LaneSettlementObservationCompletion {
	return LaneSettlementObservationCompletion{
		Enqueued: completion.Enqueued,
		Loss: LaneSettlementObservationLoss{
			CapacityDropped: completion.CapacityDropped,
		},
	}
}

func (s *LaneSet) holdLaneReassignments(results []laneResult) {
	if !s.settlementObservationsEnabled() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, result := range results {
		result.state.settlementHolds++
	}
}

func (s *LaneSet) resolveLaneReassignments(results []laneResult, admitted bool) {
	if !s.settlementObservationsEnabled() || len(results) == 0 {
		return
	}
	settlements := make([]LaneSettlementSummary, 0, len(results))
	s.mu.Lock()
	for _, result := range results {
		state := result.state
		if state.settlementHolds == 0 {
			continue
		}
		if admitted {
			state.settlement.addReassignment()
		}
		state.settlementHolds--
		if summary := s.settleLaneLocked(state); summary != nil {
			settlements = append(settlements, *summary)
		}
	}
	s.mu.Unlock()
	for index := range settlements {
		s.publishLaneSettlement(&settlements[index])
	}
}

func (s *LaneSet) retireLaneLocked(state *laneState) *LaneSettlementSummary {
	if state == nil || state.retired {
		return nil
	}
	state.retired = true
	return s.settleLaneLocked(state)
}

func (s *LaneSet) settleLaneLocked(state *laneState) *LaneSettlementSummary {
	if state == nil || state.settlement == nil || !state.retired || state.settled ||
		state.inflight != 0 || state.settlementHolds != 0 {
		return nil
	}
	state.settled = true
	return &LaneSettlementSummary{
		ProtocolSessionID:   s.sessionID,
		Route:               state.route,
		Lane:                state.identity,
		DeliveredBlocks:     state.settlement.deliveredBlocks,
		DeliveredBytes:      state.settlement.deliveredBytes,
		FailedBlockAttempts: state.settlement.failedBlockAttempts,
		ReassignedBlocks:    state.settlement.reassignedBlocks,
		Incomplete:          state.settlement.incomplete,
	}
}

func (s *LaneSet) publishLaneSettlement(summary *LaneSettlementSummary) {
	if summary != nil {
		s.settlementProducer.TryPublish(*summary)
	}
}

func (s *LaneSet) publishRegisteredLaneSettlement(summary *LaneSettlementSummary) {
	if summary == nil {
		return
	}
	s.settlementProducer.TryPublish(*summary)
	s.publications.Done()
}

// Stop closes admission and cancels attempts without waiting. BlockLane
// callbacks use it when a lane-local failure must terminate the whole set.
func (s *LaneSet) Stop() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		for _, state := range s.lanes {
			if state.settlement != nil {
				// Stop is cancellation-only. Holding the final incarnation keeps
				// summaries behind Close's attempt join while allowing replacement
				// and explicit removal to settle independently.
				state.settlementHolds++
				s.finalSettlements = append(s.finalSettlements, state)
			}
			s.retireLaneLocked(state)
		}
		clear(s.lanes)
		clear(s.contentSuspensions)
		s.notifyAvailabilityLocked()
	}
	s.mu.Unlock()
	s.stop()
}

// Close is the external ownership boundary. Attempt callbacks call Stop because
// synchronously joining their own attempt would prevent its deferred completion.
func (s *LaneSet) Close() {
	s.mu.Lock()
	if s.closeStarted {
		done := s.closeDone
		s.mu.Unlock()
		<-done
		return
	}
	s.closeStarted = true
	s.mu.Unlock()

	s.Stop()
	s.fetches.Wait()
	// A winner may return without waiting for its hedges, but lane ownership
	// cannot end until cancellation has brought every admitted attempt home.
	s.attempts.Wait()
	s.publications.Wait()
	for _, settlement := range s.finalizeLaneSettlements() {
		s.settlementProducer.TryPublish(settlement)
	}
	s.settlementProducer.Complete()
	close(s.closeDone)
}

// CompleteObservations returns the producer-owned settlement cut. Close first
// joins lane work so no new summary can be admitted while this result is read.
func (s *LaneSet) CompleteObservations() LaneSettlementObservationCompletion {
	if s == nil {
		return LaneSettlementObservationCompletion{}
	}
	s.Close()
	return laneSettlementCompletion(s.settlementProducer.Complete())
}

func (s *LaneSet) finalizeLaneSettlements() []LaneSettlementSummary {
	if !s.settlementObservationsEnabled() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	settlements := make([]LaneSettlementSummary, 0, len(s.finalSettlements))
	for _, state := range s.finalSettlements {
		if state.settlementHolds == 0 {
			continue
		}
		state.settlementHolds--
		if summary := s.settleLaneLocked(state); summary != nil {
			settlements = append(settlements, *summary)
		}
	}
	s.finalSettlements = nil
	return settlements
}
