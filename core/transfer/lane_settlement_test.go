package transfer

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type laneSettlementCollector struct {
	mu        sync.Mutex
	summaries []LaneSettlementSummary
}

func (collector *laneSettlementCollector) TraceLaneSettlement(summary LaneSettlementSummary) {
	collector.mu.Lock()
	collector.summaries = append(collector.summaries, summary)
	collector.mu.Unlock()
}

func (collector *laneSettlementCollector) snapshot() []LaneSettlementSummary {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	return append([]LaneSettlementSummary(nil), collector.summaries...)
}

func settlementByLane(t *testing.T, summaries []LaneSettlementSummary) map[LaneIdentity]LaneSettlementSummary {
	t.Helper()
	result := make(map[LaneIdentity]LaneSettlementSummary, len(summaries))
	for _, summary := range summaries {
		if _, duplicate := result[summary.Lane]; duplicate {
			t.Fatalf("lane %v settled more than once", summary.Lane)
		}
		result[summary.Lane] = summary
	}
	return result
}

func TestLaneSettlementAttributesAuthenticatedWinnersByRoute(t *testing.T) {
	descriptor := transferDescriptor(t, 2)
	sessionID := transferID[protocolsession.ProtocolSessionID](81)
	collector := &laneSettlementCollector{}
	lanes, err := NewLaneSet(LaneSetConfig{
		ProtocolSessionID: sessionID,
		RaceWidth:         1,
		SettlementTracer:  collector,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := lanes.Add(LaneIdentity{ID: 1, Epoch: 1}, LaneRouteRelay, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		return transferRecord(t, descriptor, 0), nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := lanes.Add(LaneIdentity{ID: 2, Epoch: 3}, LaneRouteDirect, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		return transferRecord(t, descriptor, 1), nil
	})); err != nil {
		t.Fatal(err)
	}
	for index := range uint64(2) {
		demand := validDemand(t, descriptor, index)
		if _, err := lanes.fetch(context.Background(), demand, validateTransferRecord(demand)); err != nil {
			t.Fatal(err)
		}
	}
	lanes.Close()

	settled := settlementByLane(t, collector.snapshot())
	if len(settled) != 2 {
		t.Fatalf("settlements = %+v", settled)
	}
	for identity, route := range map[LaneIdentity]LaneRoute{
		{ID: 1, Epoch: 1}: LaneRouteRelay,
		{ID: 2, Epoch: 3}: LaneRouteDirect,
	} {
		summary := settled[identity]
		if summary.ProtocolSessionID != sessionID || summary.Route != route || summary.DeliveredBlocks != 1 ||
			summary.DeliveredBytes != uint64(catalog.MinChunkSize) || summary.FailedBlockAttempts != 0 ||
			summary.ReassignedBlocks != 0 || summary.Incomplete {
			t.Fatalf("lane %v summary = %+v", identity, summary)
		}
	}
}

func TestLaneSettlementCreditsReassignmentOnlyAfterNextRoundAdmission(t *testing.T) {
	descriptor := transferDescriptor(t, 1)
	demand := validDemand(t, descriptor, 0)
	collector := &laneSettlementCollector{}
	lanes, err := NewLaneSet(LaneSetConfig{
		ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](82),
		RaceWidth:         1,
		SettlementTracer:  collector,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := lanes.Add(LaneIdentity{ID: 1, Epoch: 1}, LaneRouteRelay, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		return records.BlockRecord{}, NewDemandNotAdmitted(errors.New("relay unavailable"))
	})); err != nil {
		t.Fatal(err)
	}
	if err := lanes.Add(LaneIdentity{ID: 2, Epoch: 1}, LaneRouteDirect, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		return transferRecord(t, descriptor, 0), nil
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := lanes.fetch(context.Background(), demand, validateTransferRecord(demand)); err != nil {
		t.Fatal(err)
	}
	lanes.Close()
	settled := settlementByLane(t, collector.snapshot())
	if origin := settled[LaneIdentity{ID: 1, Epoch: 1}]; origin.FailedBlockAttempts != 1 || origin.ReassignedBlocks != 1 || origin.DeliveredBlocks != 0 {
		t.Fatalf("reassigned origin = %+v", origin)
	}
	if winner := settled[LaneIdentity{ID: 2, Epoch: 1}]; winner.DeliveredBlocks != 1 || winner.FailedBlockAttempts != 0 || winner.ReassignedBlocks != 0 {
		t.Fatalf("retry winner = %+v", winner)
	}

	exhaustedCollector := &laneSettlementCollector{}
	exhausted, err := NewLaneSet(LaneSetConfig{
		ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](83),
		SettlementTracer:  exhaustedCollector,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := exhausted.Add(LaneIdentity{ID: 1, Epoch: 1}, LaneRouteRelay, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		return records.BlockRecord{}, NewDemandNotAdmitted(errors.New("no alternate lane"))
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := exhausted.fetch(context.Background(), demand, validateTransferRecord(demand)); err == nil {
		t.Fatal("exhausted fetch unexpectedly succeeded")
	}
	exhausted.Close()
	terminal := settlementByLane(t, exhaustedCollector.snapshot())[LaneIdentity{ID: 1, Epoch: 1}]
	if terminal.FailedBlockAttempts != 1 || terminal.ReassignedBlocks != 0 {
		t.Fatalf("exhausted origin = %+v", terminal)
	}
}

func TestLaneSettlementRaceCreditsOnlyWinnerAndIgnoresCancellation(t *testing.T) {
	descriptor := transferDescriptor(t, 1)
	demand := validDemand(t, descriptor, 0)
	collector := &laneSettlementCollector{}
	lanes, err := NewLaneSet(LaneSetConfig{
		ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](84),
		RaceWidth:         2,
		SettlementTracer:  collector,
	})
	if err != nil {
		t.Fatal(err)
	}
	slowStarted := make(chan struct{})
	if err := lanes.Add(LaneIdentity{ID: 1, Epoch: 1}, LaneRouteRelay, laneFunction(func(ctx context.Context, _ BlockDemand) (records.BlockRecord, error) {
		close(slowStarted)
		<-ctx.Done()
		return records.BlockRecord{}, ctx.Err()
	})); err != nil {
		t.Fatal(err)
	}
	if err := lanes.Add(LaneIdentity{ID: 2, Epoch: 1}, LaneRouteDirect, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		<-slowStarted
		return transferRecord(t, descriptor, 0), nil
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := lanes.fetch(context.Background(), demand, validateTransferRecord(demand)); err != nil {
		t.Fatal(err)
	}
	lanes.Close()
	settled := settlementByLane(t, collector.snapshot())
	if loser := settled[LaneIdentity{ID: 1, Epoch: 1}]; loser.DeliveredBlocks != 0 || loser.FailedBlockAttempts != 0 || loser.ReassignedBlocks != 0 {
		t.Fatalf("canceled hedge = %+v", loser)
	}
	if winner := settled[LaneIdentity{ID: 2, Epoch: 1}]; winner.DeliveredBlocks != 1 || winner.FailedBlockAttempts != 0 {
		t.Fatalf("selected winner = %+v", winner)
	}

	canceledCollector := &laneSettlementCollector{}
	canceled, err := NewLaneSet(LaneSetConfig{
		ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](85),
		SettlementTracer:  canceledCollector,
	})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	if err := canceled.Add(LaneIdentity{ID: 3, Epoch: 1}, LaneRouteRelay, laneFunction(func(ctx context.Context, _ BlockDemand) (records.BlockRecord, error) {
		close(started)
		<-ctx.Done()
		return records.BlockRecord{}, ctx.Err()
	})); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, fetchErr := canceled.fetch(ctx, demand, validateTransferRecord(demand))
		result <- fetchErr
	}()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled fetch error = %v", err)
	}
	canceled.Close()
	summary := settlementByLane(t, canceledCollector.snapshot())[LaneIdentity{ID: 3, Epoch: 1}]
	if summary.DeliveredBlocks != 0 || summary.FailedBlockAttempts != 0 || summary.ReassignedBlocks != 0 {
		t.Fatalf("caller cancellation = %+v", summary)
	}

	wrappedCollector := &laneSettlementCollector{}
	wrapped, err := NewLaneSet(LaneSetConfig{
		ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](90),
		SettlementTracer:  wrappedCollector,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := wrapped.Add(LaneIdentity{ID: 4, Epoch: 1}, LaneRouteRelay, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		return records.BlockRecord{}, NewDemandNotAdmitted(context.Canceled)
	})); err != nil {
		t.Fatal(err)
	}
	var alternateCalls uint64
	if err := wrapped.Add(LaneIdentity{ID: 5, Epoch: 1}, LaneRouteDirect, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		alternateCalls++
		return transferRecord(t, descriptor, 0), nil
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := wrapped.fetch(context.Background(), demand, validateTransferRecord(demand)); !errors.Is(err, context.Canceled) {
		t.Fatalf("wrapped cancellation error = %v", err)
	}
	wrapped.Close()
	wrappedSettled := settlementByLane(t, wrappedCollector.snapshot())
	wrappedOrigin := wrappedSettled[LaneIdentity{ID: 4, Epoch: 1}]
	if alternateCalls != 0 || wrappedOrigin.FailedBlockAttempts != 0 || wrappedOrigin.ReassignedBlocks != 0 {
		t.Fatalf("wrapped cancellation retried: alternate=%d origin=%+v", alternateCalls, wrappedOrigin)
	}
}

func TestLaneSettlementRetiresEachIncarnationAfterInflightWork(t *testing.T) {
	descriptor := transferDescriptor(t, 1)
	demand := validDemand(t, descriptor, 0)
	collector := &laneSettlementCollector{}
	lanes, err := NewLaneSet(LaneSetConfig{
		ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](86),
		SettlementTracer:  collector,
	})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	initial := LaneIdentity{ID: 1, Epoch: 1}
	if err := lanes.Add(initial, LaneRouteRelay, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		close(started)
		<-release
		return transferRecord(t, descriptor, 0), nil
	})); err != nil {
		t.Fatal(err)
	}
	fetched := make(chan error, 1)
	go func() {
		_, fetchErr := lanes.fetch(context.Background(), demand, validateTransferRecord(demand))
		fetched <- fetchErr
	}()
	<-started
	replacement := LaneIdentity{ID: 1, Epoch: 2}
	if err := lanes.Add(replacement, LaneRouteDirect, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		return transferRecord(t, descriptor, 0), nil
	})); err != nil {
		t.Fatal(err)
	}
	if len(collector.snapshot()) != 0 {
		t.Fatal("inflight incarnation settled before its authenticated result")
	}
	close(release)
	if err := <-fetched; err != nil {
		t.Fatal(err)
	}
	if !lanes.Remove(replacement) {
		t.Fatal("replacement removal failed")
	}
	removedStarted := make(chan struct{})
	removedRelease := make(chan struct{})
	removed := LaneIdentity{ID: 2, Epoch: 1}
	if err := lanes.Add(removed, LaneRouteDirect, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		close(removedStarted)
		<-removedRelease
		return transferRecord(t, descriptor, 0), nil
	})); err != nil {
		t.Fatal(err)
	}
	removedFetch := make(chan error, 1)
	go func() {
		_, fetchErr := lanes.fetch(context.Background(), demand, validateTransferRecord(demand))
		removedFetch <- fetchErr
	}()
	<-removedStarted
	if !lanes.Remove(removed) {
		t.Fatal("inflight lane removal failed")
	}
	for _, summary := range collector.snapshot() {
		if summary.Lane == removed {
			t.Fatal("removed lane settled before its inflight result")
		}
	}
	close(removedRelease)
	if err := <-removedFetch; err != nil {
		t.Fatal(err)
	}

	final := LaneIdentity{ID: 3, Epoch: 1}
	if err := lanes.Add(final, LaneRouteRelay, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		return transferRecord(t, descriptor, 0), nil
	})); err != nil {
		t.Fatal(err)
	}
	lanes.Close()
	settled := settlementByLane(t, collector.snapshot())
	if len(settled) != 4 {
		t.Fatalf("settlements = %+v", settled)
	}
	if settled[initial].DeliveredBlocks != 1 || settled[initial].Route != LaneRouteRelay {
		t.Fatalf("initial incarnation = %+v", settled[initial])
	}
	if settled[replacement].DeliveredBlocks != 0 || settled[replacement].Route != LaneRouteDirect {
		t.Fatalf("replacement incarnation = %+v", settled[replacement])
	}
	if settled[removed].DeliveredBlocks != 1 || settled[removed].Route != LaneRouteDirect {
		t.Fatalf("removed incarnation = %+v", settled[removed])
	}
	if settled[final].DeliveredBlocks != 0 || settled[final].Route != LaneRouteRelay {
		t.Fatalf("final incarnation = %+v", settled[final])
	}
}

func TestLaneSettlementConcurrentRemovalAndCloseEmitsOnce(t *testing.T) {
	for iteration := range 32 {
		collector := &laneSettlementCollector{}
		lanes, err := NewLaneSet(LaneSetConfig{
			ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](byte(120 + iteration)),
			SettlementTracer:  collector,
		})
		if err != nil {
			t.Fatal(err)
		}
		identity := LaneIdentity{ID: 1, Epoch: uint32(iteration + 1)}
		if err := lanes.Add(identity, LaneRouteRelay, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
			return records.BlockRecord{}, nil
		})); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		removed := make(chan bool, 1)
		closed := make(chan struct{})
		go func() {
			<-start
			removed <- lanes.Remove(identity)
		}()
		go func() {
			<-start
			lanes.Close()
			close(closed)
		}()
		close(start)
		<-removed
		<-closed
		summaries := collector.snapshot()
		if len(summaries) != 1 || summaries[0].Lane != identity {
			t.Fatalf("iteration %d settlements = %+v", iteration, summaries)
		}
	}
}

func TestLaneSettlementSaturatesAndTracerCannotBlockClose(t *testing.T) {
	counters := &laneSettlementCounters{
		deliveredBlocks:     math.MaxUint64,
		deliveredBytes:      math.MaxUint64 - 1,
		failedBlockAttempts: math.MaxUint64,
		reassignedBlocks:    math.MaxUint64,
	}
	counters.addDelivered(2)
	counters.addFailure()
	counters.addReassignment()
	if counters.deliveredBlocks != math.MaxUint64 || counters.deliveredBytes != math.MaxUint64 ||
		counters.failedBlockAttempts != math.MaxUint64 || counters.reassignedBlocks != math.MaxUint64 || !counters.incomplete {
		t.Fatalf("saturated counters = %+v", counters)
	}
	dispatcher := &laneSettlementDispatcher{}
	firstDropped := LaneSettlementSummary{Lane: LaneIdentity{ID: 1, Epoch: 1}, DeliveredBlocks: 2}
	dispatcher.coalesceLocked(firstDropped)
	dispatcher.coalesceLocked(LaneSettlementSummary{Lane: LaneIdentity{ID: 2, Epoch: 1}, DeliveredBlocks: 7})
	coalesced, ok := dispatcher.takeOverflow()
	if !ok || coalesced.Lane != firstDropped.Lane || coalesced.DeliveredBlocks != firstDropped.DeliveredBlocks || !coalesced.Incomplete {
		t.Fatalf("coalesced dispatcher loss = %+v, %t", coalesced, ok)
	}
	if dispatcher.loss.QueueOverflow != 1 {
		t.Fatalf("coalesced omitted count = %d, want 1", dispatcher.loss.QueueOverflow)
	}

	callbackExited := make(chan struct{})
	var lateCommits atomic.Uint64
	lanes, err := NewLaneSet(LaneSetConfig{
		ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](87),
		SettlementTracer: LaneSettlementContextTraceFunc(func(ctx context.Context, _ LaneSettlementSummary) {
			<-ctx.Done()
			if ctx.Err() == nil {
				lateCommits.Add(1)
			}
			close(callbackExited)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := lanes.Add(LaneIdentity{ID: 1, Epoch: 1}, LaneRouteRelay, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		return records.BlockRecord{}, nil
	})); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() {
		lanes.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("blocked lane tracer prevented close")
	}
	blockedCompletion := lanes.CompleteObservations(context.Background())
	if blockedCompletion.Drained || blockedCompletion.Loss.CallbackTimeout != 1 ||
		blockedCompletion.Loss.Undrained != 0 {
		t.Fatalf("blocked completion = %+v", blockedCompletion)
	}
	select {
	case <-callbackExited:
	case <-time.After(time.Second):
		t.Fatal("revoked settlement callback did not exit")
	}
	if lateCommits.Load() != 0 {
		t.Fatalf("revoked settlement callback committed %d late fact(s)", lateCommits.Load())
	}

	panicking, err := NewLaneSet(LaneSetConfig{
		ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](88),
		SettlementTracer: LaneSettlementTraceFunc(func(LaneSettlementSummary) {
			panic("observer defect")
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := panicking.Add(LaneIdentity{ID: 1, Epoch: 1}, LaneRouteRelay, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		return records.BlockRecord{}, nil
	})); err != nil {
		t.Fatal(err)
	}
	panicking.Close()
	panicCompletion := panicking.CompleteObservations(context.Background())
	if panicCompletion.Drained || panicCompletion.Loss.ObserverPanic != 1 ||
		panicCompletion.Loss.Undrained != 0 {
		t.Fatalf("panic completion = %+v", panicCompletion)
	}
}

func TestLaneSettlementCallbackFailureAccountsQueuedSummaries(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Uint64
	dispatcher := newLaneSettlementDispatcher(LaneSettlementTraceFunc(func(LaneSettlementSummary) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			panic("observer defect")
		}
	}))
	dispatcher.callbackLimit = time.Second
	dispatcher.publish(LaneSettlementSummary{Lane: LaneIdentity{ID: 1, Epoch: 1}})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("settlement callback did not begin")
	}
	dispatcher.publish(LaneSettlementSummary{Lane: LaneIdentity{ID: 2, Epoch: 1}})
	dispatcher.publish(LaneSettlementSummary{Lane: LaneIdentity{ID: 3, Epoch: 1}})
	close(release)
	completion := dispatcher.complete(context.Background())
	if completion.Drained || completion.Delivered != 0 ||
		completion.Loss.ObserverPanic != 1 || completion.Loss.Undrained != 2 {
		t.Fatalf("settlement completion = %+v", completion)
	}
	if calls.Load() != 1 {
		t.Fatalf("callbacks begun after observer failure = %d", calls.Load())
	}
}

func TestLaneSettlementNilTracerAllocatesNoObservationState(t *testing.T) {
	lanes, err := NewLaneSet(LaneSetConfig{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](89)})
	if err != nil {
		t.Fatal(err)
	}
	identity := LaneIdentity{ID: 1, Epoch: 1}
	if err := lanes.Add(identity, 0, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		return records.BlockRecord{}, nil
	})); !errors.Is(err, ErrInvalidLane) {
		t.Fatalf("open lane route error = %v", err)
	}
	if err := lanes.Add(identity, LaneRouteRelay, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		return records.BlockRecord{}, nil
	})); err != nil {
		t.Fatal(err)
	}
	lanes.mu.Lock()
	state := lanes.lanes[identity.ID]
	if lanes.settlements != nil || state == nil || state.settlement != nil {
		lanes.mu.Unlock()
		t.Fatalf("nil tracer allocated settlement state: dispatcher=%p state=%+v", lanes.settlements, state)
	}
	lanes.mu.Unlock()
	lanes.Close()
}
