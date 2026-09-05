package transfer

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type laneSettlementCollector struct {
	observations <-chan LaneSettlementSummary
	summaries    []LaneSettlementSummary
}

func (collector *laneSettlementCollector) observe(lanes *LaneSet) {
	collector.observations = lanes.SettlementObservations()
}

func (collector *laneSettlementCollector) snapshot() []LaneSettlementSummary {
	for {
		select {
		case summary, open := <-collector.observations:
			if !open {
				return append([]LaneSettlementSummary(nil), collector.summaries...)
			}
			collector.summaries = append(collector.summaries, summary)
		default:
			return append([]LaneSettlementSummary(nil), collector.summaries...)
		}
	}
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

func TestUsefulContentActivityDoesNotRequireObservationQueue(t *testing.T) {
	now := time.Unix(70, 0)
	lanes, err := NewLaneSet(LaneSetConfig{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](83), Now: func() time.Time { return now }, RaceWidth: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer lanes.Close()
	descriptor := transferDescriptor(t, 1)
	identity := LaneIdentity{ID: 1, Epoch: 1}
	if err = lanes.Add(identity, LaneRouteDirect, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		return transferRecord(t, descriptor, 0), nil
	})); err != nil {
		t.Fatal(err)
	}
	initial := lanes.ContentActivity()
	if len(initial) != 1 || initial[0].UsefulBytes != 0 || !initial[0].LastUsefulAt.IsZero() || initial[0].AdmittedLanes != 1 {
		t.Fatal(initial)
	}
	demand := validDemand(t, descriptor, 0)
	if _, err = lanes.fetch(context.Background(), demand, validateTransferRecord(demand)); err != nil {
		t.Fatal(err)
	}
	lanes.attempts.Wait()
	activity := lanes.ContentActivity()
	if len(activity) != 1 || activity[0].UsefulBytes != uint64(catalog.MinChunkSize) || activity[0].LastUsefulAt != now {
		t.Fatal(activity)
	}
	lanes.Remove(identity)
	retired := lanes.ContentActivity()
	if len(retired) != 1 || retired[0].AdmittedLanes != 0 || retired[0].UsefulBytes != activity[0].UsefulBytes {
		t.Fatal(retired)
	}
}

func TestLaneSettlementAttributesAuthenticatedWinnersByRoute(t *testing.T) {
	descriptor := transferDescriptor(t, 2)
	sessionID := transferID[protocolsession.ProtocolSessionID](81)
	collector := &laneSettlementCollector{}
	lanes, err := NewLaneSet(LaneSetConfig{
		ProtocolSessionID:             sessionID,
		RaceWidth:                     1,
		SettlementObservationCapacity: DefaultLaneSettlementObservationCapacity,
	})
	if err != nil {
		t.Fatal(err)
	}
	collector.observe(lanes)
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
		ProtocolSessionID:             transferID[protocolsession.ProtocolSessionID](82),
		RaceWidth:                     1,
		SettlementObservationCapacity: DefaultLaneSettlementObservationCapacity,
	})
	if err != nil {
		t.Fatal(err)
	}
	collector.observe(lanes)
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
		ProtocolSessionID:             transferID[protocolsession.ProtocolSessionID](83),
		SettlementObservationCapacity: DefaultLaneSettlementObservationCapacity,
	})
	if err != nil {
		t.Fatal(err)
	}
	exhaustedCollector.observe(exhausted)
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
		ProtocolSessionID:             transferID[protocolsession.ProtocolSessionID](84),
		RaceWidth:                     2,
		SettlementObservationCapacity: DefaultLaneSettlementObservationCapacity,
	})
	if err != nil {
		t.Fatal(err)
	}
	collector.observe(lanes)
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
		ProtocolSessionID:             transferID[protocolsession.ProtocolSessionID](85),
		SettlementObservationCapacity: DefaultLaneSettlementObservationCapacity,
	})
	if err != nil {
		t.Fatal(err)
	}
	canceledCollector.observe(canceled)
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
		ProtocolSessionID:             transferID[protocolsession.ProtocolSessionID](90),
		SettlementObservationCapacity: DefaultLaneSettlementObservationCapacity,
	})
	if err != nil {
		t.Fatal(err)
	}
	wrappedCollector.observe(wrapped)
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
		ProtocolSessionID:             transferID[protocolsession.ProtocolSessionID](86),
		SettlementObservationCapacity: DefaultLaneSettlementObservationCapacity,
	})
	if err != nil {
		t.Fatal(err)
	}
	collector.observe(lanes)
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
			ProtocolSessionID:             transferID[protocolsession.ProtocolSessionID](byte(120 + iteration)),
			SettlementObservationCapacity: DefaultLaneSettlementObservationCapacity,
		})
		if err != nil {
			t.Fatal(err)
		}
		collector.observe(lanes)
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

func TestLaneSettlementSaturatesCounters(t *testing.T) {
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
}

func TestLaneSettlementNoConsumerSaturationCannotBlockClose(t *testing.T) {
	lanes, err := NewLaneSet(LaneSetConfig{
		ProtocolSessionID:             transferID[protocolsession.ProtocolSessionID](87),
		SettlementObservationCapacity: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	observations := lanes.SettlementObservations()
	for laneID := uint32(1); laneID <= 2; laneID++ {
		if err := lanes.Add(LaneIdentity{ID: laneID, Epoch: 1}, LaneRouteRelay, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
			return records.BlockRecord{}, nil
		})); err != nil {
			t.Fatal(err)
		}
	}
	closed := make(chan struct{})
	go func() {
		lanes.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("saturated settlement stream prevented close")
	}
	completion := lanes.CompleteObservations()
	if completion.Enqueued != 1 || completion.Loss.CapacityDropped != 1 || completion.Loss.Total() != 1 {
		t.Fatalf("saturated completion = %+v", completion)
	}
	if repeated := lanes.CompleteObservations(); repeated != completion {
		t.Fatalf("repeated completion = %+v, want %+v", repeated, completion)
	}
	retained, open := <-observations
	if !open || (retained.Lane.ID != 1 && retained.Lane.ID != 2) {
		t.Fatalf("retained exact-lane summary = %+v, open=%t", retained, open)
	}
	if _, open := <-observations; open {
		t.Fatal("settlement stream remained open after owner completion")
	}
}

func TestLaneSettlementFinalSummaryPrecedesStreamClose(t *testing.T) {
	lanes, err := NewLaneSet(LaneSetConfig{
		ProtocolSessionID:             transferID[protocolsession.ProtocolSessionID](88),
		SettlementObservationCapacity: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	identity := LaneIdentity{ID: 4, Epoch: 3}
	if err := lanes.Add(identity, LaneRouteDirect, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		return records.BlockRecord{}, nil
	})); err != nil {
		t.Fatal(err)
	}
	observations := lanes.SettlementObservations()
	lanes.Close()
	final, open := <-observations
	if !open || final.Lane != identity || final.ProtocolSessionID != transferID[protocolsession.ProtocolSessionID](88) ||
		final.Route != LaneRouteDirect {
		t.Fatalf("final summary = %+v, open=%t", final, open)
	}
	if _, open := <-observations; open {
		t.Fatal("stream closed before its final summary was observable")
	}
	if completion := lanes.CompleteObservations(); completion.Enqueued != 1 || completion.Loss.Total() != 0 {
		t.Fatalf("completion = %+v", completion)
	}
}

func TestLaneSettlementConcurrentReplacementAndCloseOwnsEveryPublication(t *testing.T) {
	for iteration := range 32 {
		lanes, err := NewLaneSet(LaneSetConfig{
			ProtocolSessionID:             transferID[protocolsession.ProtocolSessionID](byte(160 + iteration)),
			SettlementObservationCapacity: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		initial := LaneIdentity{ID: 1, Epoch: 1}
		replacement := LaneIdentity{ID: 1, Epoch: 2}
		lane := laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
			return records.BlockRecord{}, nil
		})
		if err := lanes.Add(initial, LaneRouteRelay, lane); err != nil {
			t.Fatal(err)
		}
		observations := lanes.SettlementObservations()
		start := make(chan struct{})
		added := make(chan error, 1)
		closed := make(chan struct{})
		go func() {
			<-start
			added <- lanes.Add(replacement, LaneRouteDirect, lane)
		}()
		go func() {
			<-start
			lanes.Close()
			close(closed)
		}()
		close(start)
		addErr := <-added
		<-closed

		summaries := make([]LaneSettlementSummary, 0, 2)
		for summary := range observations {
			summaries = append(summaries, summary)
		}
		settled := settlementByLane(t, summaries)
		if settled[initial].Lane != initial {
			t.Fatalf("iteration %d lost initial settlement: %+v", iteration, summaries)
		}
		switch {
		case addErr == nil:
			if len(settled) != 2 || settled[replacement].Lane != replacement {
				t.Fatalf("iteration %d replacement admitted without settlement: %+v", iteration, summaries)
			}
		case errors.Is(addErr, ErrLaneClosed):
			if len(settled) != 1 {
				t.Fatalf("iteration %d closed replacement produced settlement: %+v", iteration, summaries)
			}
		default:
			t.Fatalf("iteration %d replacement error = %v", iteration, addErr)
		}
	}
}

func TestLaneSettlementDisabledAllocatesNoObservationState(t *testing.T) {
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
	if lanes.SettlementObservations() != nil || state == nil || state.settlement != nil {
		lanes.mu.Unlock()
		t.Fatalf("disabled observations allocated settlement state: stream=%v state=%+v", lanes.SettlementObservations(), state)
	}
	lanes.mu.Unlock()
	lanes.Close()
	if completion := lanes.CompleteObservations(); completion != (LaneSettlementObservationCompletion{}) {
		t.Fatalf("disabled completion = %+v", completion)
	}
}

func TestLaneSettlementRejectsNegativeObservationCapacity(t *testing.T) {
	lanes, err := NewLaneSet(LaneSetConfig{
		ProtocolSessionID:             transferID[protocolsession.ProtocolSessionID](91),
		SettlementObservationCapacity: -1,
	})
	if lanes != nil || !errors.Is(err, observationstream.ErrInvalidCapacity) {
		t.Fatalf("NewLaneSet() = (%v, %v), want invalid capacity", lanes, err)
	}
}
