package transfer

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/windshare/windshare/core/session/protocolsession"
)

const (
	laneSettlementQueueCapacity = MaxLogicalLanes
	laneSettlementCallbackLimit = 25 * time.Millisecond
)

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

type LaneSettlementTracer interface {
	TraceLaneSettlement(LaneSettlementSummary)
}

type LaneSettlementContextTracer interface {
	LaneSettlementTracer
	TraceLaneSettlementContext(context.Context, LaneSettlementSummary)
}

type LaneSettlementTraceFunc func(LaneSettlementSummary)

func (function LaneSettlementTraceFunc) TraceLaneSettlement(summary LaneSettlementSummary) {
	if function != nil {
		function(summary)
	}
}

type LaneSettlementContextTraceFunc func(context.Context, LaneSettlementSummary)

func (function LaneSettlementContextTraceFunc) TraceLaneSettlement(summary LaneSettlementSummary) {
	function.TraceLaneSettlementContext(context.Background(), summary)
}

func (function LaneSettlementContextTraceFunc) TraceLaneSettlementContext(
	ctx context.Context,
	summary LaneSettlementSummary,
) {
	if function != nil {
		function(ctx, summary)
	}
}

// LaneSettlementObservationLoss names every producer-owned omission without
// merging lane identities into a synthetic summary.
type LaneSettlementObservationLoss struct {
	QueueOverflow   uint64
	ObserverPanic   uint64
	CallbackTimeout uint64
	Undrained       uint64
}

func (loss LaneSettlementObservationLoss) Total() uint64 {
	total := saturatingSettlementCount(loss.QueueOverflow, loss.ObserverPanic)
	total = saturatingSettlementCount(total, loss.CallbackTimeout)
	return saturatingSettlementCount(total, loss.Undrained)
}

type LaneSettlementObservationCompletion struct {
	Delivered uint64
	Loss      LaneSettlementObservationLoss
	Drained   bool
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

func saturatingSettlementCount(current, increment uint64) uint64 {
	if math.MaxUint64-current < increment {
		return math.MaxUint64
	}
	return current + increment
}

type laneSettlementCallbackOutcome uint8

const (
	laneSettlementCallbackDelivered laneSettlementCallbackOutcome = iota + 1
	laneSettlementCallbackPanicked
	laneSettlementCallbackTimedOut
	laneSettlementCallbackAbandoned
)

type laneSettlementDispatcher struct {
	tracer LaneSettlementTracer

	mu            sync.Mutex
	wake          *sync.Cond
	queue         []LaneSettlementSummary
	overflow      *LaneSettlementSummary
	closing       bool
	detached      bool
	callbackLive  bool
	drained       bool
	delivered     uint64
	loss          LaneSettlementObservationLoss
	callbackLimit time.Duration
	detach        chan struct{}
	detachOnce    sync.Once
	done          chan struct{}
}

func newLaneSettlementDispatcher(tracer LaneSettlementTracer) *laneSettlementDispatcher {
	if tracer == nil {
		return nil
	}
	dispatcher := &laneSettlementDispatcher{
		tracer:  tracer,
		queue:   make([]LaneSettlementSummary, 0, laneSettlementQueueCapacity),
		drained: true, callbackLimit: laneSettlementCallbackLimit,
		detach: make(chan struct{}), done: make(chan struct{}),
	}
	dispatcher.wake = sync.NewCond(&dispatcher.mu)
	go dispatcher.run()
	return dispatcher
}

func (dispatcher *laneSettlementDispatcher) publish(summary LaneSettlementSummary) {
	if dispatcher == nil {
		return
	}
	dispatcher.mu.Lock()
	defer dispatcher.mu.Unlock()
	if dispatcher.closing || dispatcher.detached {
		dispatcher.loss.Undrained = saturatingSettlementCount(dispatcher.loss.Undrained, 1)
		return
	}
	if len(dispatcher.queue) < laneSettlementQueueCapacity {
		dispatcher.queue = append(dispatcher.queue, summary)
	} else {
		dispatcher.coalesceLocked(summary)
	}
	dispatcher.wake.Signal()
}

func (dispatcher *laneSettlementDispatcher) coalesceLocked(summary LaneSettlementSummary) {
	if dispatcher.overflow == nil {
		dispatcher.overflow = &summary
		return
	}
	// The retained summary keeps its exact lane identity. Only later summaries
	// are omitted, and the completion result supplies their exact count.
	dispatcher.overflow.Incomplete = true
	dispatcher.loss.QueueOverflow = saturatingSettlementCount(dispatcher.loss.QueueOverflow, 1)
}

func (dispatcher *laneSettlementDispatcher) takeOverflow() (LaneSettlementSummary, bool) {
	dispatcher.mu.Lock()
	defer dispatcher.mu.Unlock()
	return dispatcher.takeOverflowLocked()
}

func (dispatcher *laneSettlementDispatcher) takeOverflowLocked() (LaneSettlementSummary, bool) {
	if dispatcher.overflow == nil {
		return LaneSettlementSummary{}, false
	}
	summary := *dispatcher.overflow
	dispatcher.overflow = nil
	return summary, true
}

func (dispatcher *laneSettlementDispatcher) run() {
	defer close(dispatcher.done)
	for {
		dispatcher.mu.Lock()
		for len(dispatcher.queue) == 0 && dispatcher.overflow == nil &&
			!dispatcher.closing && !dispatcher.detached {
			dispatcher.wake.Wait()
		}
		if dispatcher.detached ||
			(len(dispatcher.queue) == 0 && dispatcher.overflow == nil && dispatcher.closing) {
			dispatcher.mu.Unlock()
			return
		}
		var summary LaneSettlementSummary
		if len(dispatcher.queue) != 0 {
			summary = dispatcher.queue[0]
			dispatcher.queue[0] = LaneSettlementSummary{}
			dispatcher.queue = dispatcher.queue[1:]
		} else {
			summary, _ = dispatcher.takeOverflowLocked()
		}
		dispatcher.callbackLive = true
		dispatcher.mu.Unlock()

		outcome := dispatcher.invoke(summary)
		dispatcher.mu.Lock()
		dispatcher.callbackLive = false
		if dispatcher.detached || outcome == laneSettlementCallbackAbandoned {
			dispatcher.mu.Unlock()
			return
		}
		switch outcome {
		case laneSettlementCallbackDelivered:
			dispatcher.delivered = saturatingSettlementCount(dispatcher.delivered, 1)
		case laneSettlementCallbackPanicked:
			dispatcher.loss.ObserverPanic = saturatingSettlementCount(dispatcher.loss.ObserverPanic, 1)
			dispatcher.detachAfterCallbackFailureLocked()
		case laneSettlementCallbackTimedOut:
			dispatcher.loss.CallbackTimeout = saturatingSettlementCount(dispatcher.loss.CallbackTimeout, 1)
			dispatcher.detachAfterCallbackFailureLocked()
		}
		dispatcher.mu.Unlock()
		if outcome != laneSettlementCallbackDelivered {
			return
		}
	}
}

func (dispatcher *laneSettlementDispatcher) detachAfterCallbackFailureLocked() {
	undrained := uint64(len(dispatcher.queue))
	if dispatcher.overflow != nil {
		undrained = saturatingSettlementCount(undrained, 1)
	}
	dispatcher.loss.Undrained = saturatingSettlementCount(dispatcher.loss.Undrained, undrained)
	clear(dispatcher.queue)
	dispatcher.queue = nil
	dispatcher.overflow = nil
	dispatcher.detached = true
	dispatcher.drained = false
	dispatcher.detachOnce.Do(func() { close(dispatcher.detach) })
}

func (dispatcher *laneSettlementDispatcher) invoke(summary LaneSettlementSummary) laneSettlementCallbackOutcome {
	result := make(chan laneSettlementCallbackOutcome, 1)
	callbackContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		outcome := laneSettlementCallbackDelivered
		defer func() {
			if recover() != nil {
				outcome = laneSettlementCallbackPanicked
			}
			result <- outcome
		}()
		if contextual, ok := dispatcher.tracer.(LaneSettlementContextTracer); ok {
			contextual.TraceLaneSettlementContext(callbackContext, summary)
			return
		}
		dispatcher.tracer.TraceLaneSettlement(summary)
	}()
	timer := time.NewTimer(dispatcher.callbackLimit)
	defer timer.Stop()
	select {
	case outcome := <-result:
		return outcome
	case <-timer.C:
		cancel()
		return laneSettlementCallbackTimedOut
	case <-dispatcher.detach:
		cancel()
		return laneSettlementCallbackAbandoned
	}
}

func (dispatcher *laneSettlementDispatcher) complete(ctx context.Context) LaneSettlementObservationCompletion {
	if dispatcher == nil {
		return LaneSettlementObservationCompletion{Drained: true}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	dispatcher.mu.Lock()
	if !dispatcher.closing {
		dispatcher.closing = true
		dispatcher.wake.Broadcast()
	}
	dispatcher.mu.Unlock()
	select {
	case <-dispatcher.done:
	default:
		select {
		case <-dispatcher.done:
		case <-ctx.Done():
			dispatcher.mu.Lock()
			if !dispatcher.detached {
				undrained := uint64(len(dispatcher.queue))
				if dispatcher.overflow != nil {
					undrained = saturatingSettlementCount(undrained, 1)
				}
				if dispatcher.callbackLive {
					undrained = saturatingSettlementCount(undrained, 1)
				}
				dispatcher.loss.Undrained = saturatingSettlementCount(dispatcher.loss.Undrained, undrained)
				clear(dispatcher.queue)
				dispatcher.queue = nil
				dispatcher.overflow = nil
				dispatcher.detached = true
				dispatcher.drained = false
				dispatcher.detachOnce.Do(func() { close(dispatcher.detach) })
				dispatcher.wake.Broadcast()
			}
			dispatcher.mu.Unlock()
			<-dispatcher.done
		}
	}
	dispatcher.mu.Lock()
	completion := LaneSettlementObservationCompletion{
		Delivered: dispatcher.delivered, Loss: dispatcher.loss, Drained: dispatcher.drained,
	}
	dispatcher.mu.Unlock()
	return completion
}

func (dispatcher *laneSettlementDispatcher) close() LaneSettlementObservationCompletion {
	return dispatcher.complete(context.Background())
}

func (s *LaneSet) holdLaneReassignments(results []laneResult) {
	if s.settlements == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, result := range results {
		result.state.settlementHolds++
	}
}

func (s *LaneSet) resolveLaneReassignments(results []laneResult, admitted bool) {
	if s.settlements == nil || len(results) == 0 {
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
		s.settlements.publish(*summary)
	}
}

func (s *LaneSet) publishRegisteredLaneSettlement(summary *LaneSettlementSummary) {
	if summary == nil {
		return
	}
	s.settlements.publish(*summary)
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
		s.settlements.publish(settlement)
	}
	s.settlements.close()
	close(s.closeDone)
}

// CompleteObservations returns the producer-owned settlement cut. Close first
// joins lane work so no new summary can be admitted while this result is read.
func (s *LaneSet) CompleteObservations(ctx context.Context) LaneSettlementObservationCompletion {
	if s == nil {
		return LaneSettlementObservationCompletion{Drained: true}
	}
	s.Close()
	return s.settlements.complete(ctx)
}

func (s *LaneSet) finalizeLaneSettlements() []LaneSettlementSummary {
	if s.settlements == nil {
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
