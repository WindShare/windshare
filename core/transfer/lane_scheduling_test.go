package transfer

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/lanescheduling"
)

func TestLaneSchedulingKeepsUnmeasuredRelayOutOfFastContent(t *testing.T) {
	descriptor := transferDescriptor(t, 16)
	var tick atomic.Int64
	now := func() time.Time { return time.Unix(100, tick.Load()) }
	lanes, err := NewLaneSet(LaneSetConfig{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](120), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	defer lanes.Close()
	var directCalls, relayCalls, cancellations atomic.Int32
	_ = lanes.Add(LaneIdentity{ID: 1}, LaneRouteDirect, laneFunction(func(_ context.Context, input BlockDemand) (records.BlockRecord, error) {
		directCalls.Add(1)
		tick.Add(int64(time.Millisecond))
		return transferRecord(t, descriptor, input.Index), nil
	}))
	first := validDemand(t, descriptor, 0)
	if _, err := lanes.fetch(context.Background(), blockRequest{demand: first, validate: validateTransferRecord(first)}); err != nil {
		t.Fatal(err)
	}
	lanes.attempts.Wait()
	_ = lanes.Add(LaneIdentity{ID: 2}, LaneRouteRelay, laneFunction(func(ctx context.Context, _ BlockDemand) (records.BlockRecord, error) {
		relayCalls.Add(1)
		<-ctx.Done()
		cancellations.Add(1)
		return records.BlockRecord{}, ctx.Err()
	}))
	for index := uint64(1); index < 16; index++ {
		input := validDemand(t, descriptor, index)
		if _, err := lanes.fetch(context.Background(), blockRequest{demand: input, validate: validateTransferRecord(input)}); err != nil {
			t.Fatal(err)
		}
		lanes.attempts.Wait()
	}
	if directCalls.Load() != 16 || relayCalls.Load() != 1 || cancellations.Load() != 1 {
		t.Fatalf("content=%d probe=%d canceled=%d", directCalls.Load(), relayCalls.Load(), cancellations.Load())
	}
	if lanes.Len() != 2 {
		t.Fatal("standby transport was removed")
	}
}

func TestLaneSchedulingRescuesAlreadyDispatchedPrefix(t *testing.T) {
	descriptor := transferDescriptor(t, 2)
	lanes := newBoundaryLaneSet(t, 121)
	relayStarted := make(chan struct{})
	relayCancelled := make(chan struct{})
	_ = lanes.Add(LaneIdentity{ID: 1}, LaneRouteRelay, laneFunction(func(ctx context.Context, _ BlockDemand) (records.BlockRecord, error) {
		close(relayStarted)
		<-ctx.Done()
		close(relayCancelled)
		return records.BlockRecord{}, ctx.Err()
	}))
	input := validDemand(t, descriptor, 0)
	result := make(chan error, 1)
	go func() {
		_, err := lanes.fetch(context.Background(), blockRequest{demand: input, validate: validateTransferRecord(input)})
		result <- err
	}()
	<-relayStarted
	// Suspension fences only new content; the old prefix remains on the relay.
	suspension, err := lanes.SuspendContent(LaneIdentity{ID: 1})
	if err != nil {
		t.Fatal(err)
	}
	_ = lanes.Add(LaneIdentity{ID: 2}, LaneRouteDirect, laneFunction(func(_ context.Context, input BlockDemand) (records.BlockRecord, error) {
		return transferRecord(t, descriptor, input.Index), nil
	}))
	warm := validDemand(t, descriptor, 1)
	if _, err := lanes.fetch(context.Background(), blockRequest{demand: warm, validate: validateTransferRecord(warm)}); err != nil {
		t.Fatal(err)
	}
	if err := suspension.Resume(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow prefix was not rescued by the newly available direct path")
	}
	lanes.Close()
	select {
	case <-relayCancelled:
	default:
		t.Fatal("rescued attempt was not canceled")
	}
}

func TestLaneSchedulingUsesMeasuredCompletionAndMarginalRelayCost(t *testing.T) {
	lanes := newBoundaryLaneSet(t, 122)
	descriptor := transferDescriptor(t, 1)
	input := validDemand(t, descriptor, 0)
	bytes := blockDemandBytes(input)
	for _, id := range []uint32{1, 2} {
		route := LaneRouteDirect
		if id == 2 {
			route = LaneRouteRelay
		}
		_ = lanes.Add(LaneIdentity{ID: id}, route, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) { return records.BlockRecord{}, nil }))
	}
	// Selection tests use measured capacity without adding wall-clock delays.
	lanes.lanes[1].performance = lanescheduling.Performance{HasSuccessfulSample: true, BytesPerSecond: float64(bytes) * 100}
	lanes.lanes[2].performance = lanescheduling.Performance{HasSuccessfulSample: true, BytesPerSecond: float64(bytes) * 10}
	chosen, _, _ := lanes.selectCandidatesLocked([]*laneState{lanes.lanes[1], lanes.lanes[2]}, 1, bytes, nil)
	if chosen[0].identity.ID != 1 {
		t.Fatal("slow relay won over fast direct")
	}
	chosen[0].permit.Close()
	lanes.lanes[1].performance.PendingBytes = bytes * 20
	chosen, _, _ = lanes.selectCandidatesLocked([]*laneState{lanes.lanes[1], lanes.lanes[2]}, 1, bytes, nil)
	if chosen[0].identity.ID != 2 {
		t.Fatal("useful parallel relay capacity was ignored")
	}
	chosen[0].permit.Close()
	lanes.lanes[1].performance.PendingBytes = 0
	lanes.lanes[2].performance.PendingBytes = 0
	lanes.lanes[2].performance.BytesPerSecond = float64(bytes) * 1000
	chosen, _, _ = lanes.selectCandidatesLocked([]*laneState{lanes.lanes[1], lanes.lanes[2]}, 1, bytes, nil)
	if chosen[0].identity.ID != 2 {
		t.Fatal("route label overrode measured completion")
	}
}

func TestLaneSchedulingSupplementRespectsAuthorityAndOldestProbe(t *testing.T) {
	descriptor := transferDescriptor(t, 1)
	input := validDemand(t, descriptor, 0)
	lanes := newBoundaryLaneSet(t, 123)
	for _, id := range []uint32{1, 2, 3} {
		route := LaneRouteRelay
		if id == 1 {
			route = LaneRouteDirect
		}
		_ = lanes.Add(LaneIdentity{ID: id}, route, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) { return records.BlockRecord{}, nil }))
	}
	primary := lanes.lanes[1]
	primary.performance.HasSuccessfulSample = true
	primary.performance.BytesPerSecond = float64(blockDemandBytes(input)) * 100
	lanes.lanes[2].performance.LastAttempt = time.Now().Add(-time.Hour)
	attempted := map[LaneIdentity]struct{}{{ID: 1}: {}}
	lanes.contentRoutePolicy = ContentRouteDirectOnly
	if lanes.supplement(input, attempted, primary, time.Hour, time.Second, lanescheduling.Probe, nil) != nil ||
		lanes.supplement(input, attempted, primary, time.Hour, time.Second, lanescheduling.Rescue, nil) != nil {
		t.Fatal("route permission bypassed")
	}
	lanes.contentRoutePolicy = ContentRouteAll
	suspension, err := lanes.SuspendContent(LaneIdentity{ID: 3})
	if err != nil {
		t.Fatal(err)
	}
	if err := suspension.Resume(); err != nil {
		t.Fatal(err)
	}
	selected := lanes.supplement(input, attempted, primary, 0, time.Second, lanescheduling.Probe, nil)
	if selected == nil || selected.identity.ID != 3 {
		t.Fatal("cold path was starved by previously sampled fallback")
	}
	lanes.exploration.Release(lanescheduling.Probe)
	lanes.attempts.Done()
}

func TestLaneMeasurementRequiresAnIdleSampleAndUsefulAlternative(t *testing.T) {
	now := time.Unix(100, 0)
	lanes, err := NewLaneSet(LaneSetConfig{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](124), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer lanes.Close()
	for _, id := range []uint32{1, 2} {
		if err := lanes.Add(LaneIdentity{ID: id}, LaneRouteTURN, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
			return records.BlockRecord{}, nil
		})); err != nil {
			t.Fatal(err)
		}
	}
	slow, fast := lanes.lanes[1], lanes.lanes[2]
	slow.performance = lanescheduling.Performance{HasSuccessfulSample: true, BytesPerSecond: 100}
	fast.performance = lanescheduling.Performance{HasSuccessfulSample: true, BytesPerSecond: 200}
	fast.lastUsefulAt = now
	if lanes.MeasureLane(slow.identity).Unhelpful {
		t.Fatal("new route did not receive an exploration window")
	}
	now = now.Add(UnhelpfulLaneWindow)
	if lanes.MeasureLane(slow.identity).Unhelpful {
		t.Fatal("idle alternative justified retirement")
	}
	fast.lastUsefulAt = now
	if !lanes.MeasureLane(slow.identity).Unhelpful {
		t.Fatal("sampled route that never won retained resources indefinitely")
	}
	slow.inflight = 1
	if lanes.MeasureLane(slow.identity).Unhelpful {
		t.Fatal("active request was retired")
	}
	slow.inflight = 0
	slow.lastUsefulAt, slow.usefulBytes = now, 123
	if measurement := lanes.MeasureLane(slow.identity); measurement.Unhelpful || measurement.UsefulBytes != 123 {
		t.Fatalf("useful route was retired: %+v", measurement)
	}
}
