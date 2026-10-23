package lanes

import (
	"math"

	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/core/session/protocolsession"
)

// SettlementObservations returns nil when settlement observation was disabled.
// The receive-only capability leaves publication and completion with Set.
func (s *Set) SettlementObservations() observationstream.Consumer[SettlementSummary] {
	if s == nil {
		return nil
	}
	return s.settlementConsumer
}

func (s *Set) settlementObservationsEnabled() bool {
	return s != nil && s.settlementConsumer != nil
}

// SettlementObservationCapacity is the exact number of summaries retained
// while the owner-selected consumer is not receiving. Zero disables settlement
// accounting and stream allocation.
type SettlementObservationCapacity int

const DefaultSettlementObservationCapacity = SettlementObservationCapacity(MaxLogicalLanes)

// SettlementSummary is deliberately aggregate and text-free. The source
// contract excludes file and block identities so diagnostics cannot become a
// second content-access path.
type SettlementSummary struct {
	ProtocolSessionID   protocolsession.ProtocolSessionID
	Route               Route
	Lane                Identity
	DeliveredBlocks     uint64
	DeliveredBytes      uint64
	FailedBlockAttempts uint64
	ReassignedBlocks    uint64
	Incomplete          bool
}

// SettlementObservationLoss names every producer-owned omission without
// merging lane identities into a synthetic summary.
type SettlementObservationLoss struct {
	CapacityDropped uint64
}

func (loss SettlementObservationLoss) Total() uint64 {
	return loss.CapacityDropped
}

type SettlementObservationCompletion struct {
	Enqueued uint64
	Loss     SettlementObservationLoss
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

func laneSettlementCompletion(completion observationstream.Completion) SettlementObservationCompletion {
	return SettlementObservationCompletion{
		Enqueued: completion.Enqueued,
		Loss: SettlementObservationLoss{
			CapacityDropped: completion.CapacityDropped,
		},
	}
}

func (s *Set) holdLaneReassignments(results []laneResult) {
	if !s.settlementObservationsEnabled() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, result := range results {
		result.state.settlementHolds++
	}
}

func (s *Set) resolveLaneReassignments(results []laneResult, admitted bool) {
	if !s.settlementObservationsEnabled() || len(results) == 0 {
		return
	}
	settlements := make([]SettlementSummary, 0, len(results))
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

func (s *Set) retireLaneLocked(state *laneState) *SettlementSummary {
	if state == nil || state.retired {
		return nil
	}
	state.retired = true
	return s.settleLaneLocked(state)
}

func (s *Set) settleLaneLocked(state *laneState) *SettlementSummary {
	if state == nil || state.settlement == nil || !state.retired || state.settled ||
		state.inflight != 0 || state.settlementHolds != 0 {
		return nil
	}
	state.settled = true
	return &SettlementSummary{
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

func (s *Set) publishLaneSettlement(summary *SettlementSummary) {
	if summary != nil {
		s.settlementProducer.TryPublish(*summary)
	}
}

func (s *Set) publishRegisteredLaneSettlement(summary *SettlementSummary) {
	if summary == nil {
		return
	}
	s.settlementProducer.TryPublish(*summary)
	s.publications.Done()
}

// CompleteObservations returns the producer-owned settlement cut. Close first
// joins lane work so no new summary can be admitted while this result is read.
func (s *Set) CompleteObservations() SettlementObservationCompletion {
	if s == nil {
		return SettlementObservationCompletion{}
	}
	s.Close()
	return laneSettlementCompletion(s.settlementProducer.Complete())
}

func (s *Set) finalizeLaneSettlements() []SettlementSummary {
	if !s.settlementObservationsEnabled() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	settlements := make([]SettlementSummary, 0, len(s.finalSettlements))
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
