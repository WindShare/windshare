package lanes

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/lanescheduling"
)

func TestLaneAdmissionFreezesEstimateBeforeReservation(t *testing.T) {
	now := time.Unix(100, 0)
	set := &Set{now: func() time.Time { return now }}
	state := &laneState{}
	demand := validDemand(t, transferDescriptor(t, 1), 0)
	blockBytes := blockDemandBytes(demand)
	first, _, err := set.admitLaneLocked(state, demand, nil)
	if err != nil || first == nil {
		t.Fatalf("first admission: %v", err)
	}
	defer first.permit.Close()
	started := now
	now = now.Add(10 * time.Millisecond)
	// Receipt opens capacity before authentication finishes the first sample.
	first.permit.Receive(blockBytes, now)
	second, _, err := set.admitLaneLocked(state, demand, nil)
	if err != nil || second == nil {
		t.Fatalf("second admission: %v", err)
	}
	defer second.permit.Close()
	if first.started != started || first.expected.Duration != 250*time.Millisecond || second.expected.Duration != 10*time.Millisecond {
		t.Fatalf("admission estimates were computed after reservation: first=%v second=%v", first.expected, second.expected)
	}
	state.performance.Complete(now, blockBytes, true)
	state.performance.Complete(now, blockBytes, false)
	if first.expected.Duration != 250*time.Millisecond || second.expected.Duration != 10*time.Millisecond {
		t.Fatal("settlement changed an admitted attempt's original estimate")
	}
}

func TestLaneSchedulingKeepsUnmeasuredRelayOutOfFastContent(t *testing.T) {
	descriptor := transferDescriptor(t, 16)
	var tick atomic.Int64
	now := func() time.Time { return time.Unix(100, tick.Load()) }
	lanes, err := New(Config{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](120), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	defer lanes.Close()
	var directCalls, relayCalls, cancellations atomic.Int32
	_ = lanes.Add(Identity{ID: 1}, RouteDirect, laneFunction(func(_ context.Context, input Demand) (records.BlockRecord, error) {
		directCalls.Add(1)
		tick.Add(int64(time.Millisecond))
		return transferRecord(t, descriptor, input.Index), nil
	}))
	first := validDemand(t, descriptor, 0)
	if _, err := lanes.Fetch(context.Background(), Request{Demand: first, Validate: validateTransferRecord(first)}); err != nil {
		t.Fatal(err)
	}
	lanes.attempts.Wait()
	_ = lanes.Add(Identity{ID: 2}, RouteRelay, laneFunction(func(ctx context.Context, _ Demand) (records.BlockRecord, error) {
		relayCalls.Add(1)
		<-ctx.Done()
		cancellations.Add(1)
		return records.BlockRecord{}, ctx.Err()
	}))
	for index := uint64(1); index < 16; index++ {
		input := validDemand(t, descriptor, index)
		if _, err := lanes.Fetch(context.Background(), Request{Demand: input, Validate: validateTransferRecord(input)}); err != nil {
			t.Fatal(err)
		}
		lanes.attempts.Wait()
	}
	if directCalls.Load() != 16 || relayCalls.Load() != 0 || cancellations.Load() != 0 {
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
	_ = lanes.Add(Identity{ID: 1}, RouteRelay, laneFunction(func(ctx context.Context, _ Demand) (records.BlockRecord, error) {
		close(relayStarted)
		<-ctx.Done()
		close(relayCancelled)
		return records.BlockRecord{}, ctx.Err()
	}))
	input := validDemand(t, descriptor, 0)
	result := make(chan error, 1)
	go func() {
		_, err := lanes.Fetch(context.Background(), Request{Demand: input, Validate: validateTransferRecord(input)})
		result <- err
	}()
	<-relayStarted
	// Suspension fences only new content; the old prefix remains on the relay.
	suspension, err := lanes.SuspendContent(Identity{ID: 1})
	if err != nil {
		t.Fatal(err)
	}
	_ = lanes.Add(Identity{ID: 2}, RouteDirect, laneFunction(func(_ context.Context, input Demand) (records.BlockRecord, error) {
		return transferRecord(t, descriptor, input.Index), nil
	}))
	warm := validDemand(t, descriptor, 1)
	if _, err := lanes.Fetch(context.Background(), Request{Demand: warm, Validate: validateTransferRecord(warm)}); err != nil {
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
		route := RouteDirect
		if id == 2 {
			route = RouteRelay
		}
		_ = lanes.Add(Identity{ID: id}, route, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) { return records.BlockRecord{}, nil }))
	}
	now := lanes.now()
	seedDelivery(lanes.lanes[1], bytes, 10*time.Millisecond, now)
	seedDelivery(lanes.lanes[2], bytes, 100*time.Millisecond, now)
	chosen := []*laneState{lanes.lanes[1], lanes.lanes[2]}
	lanes.orderLanesLocked(chosen, bytes)
	if chosen[0].identity.ID != 1 {
		t.Fatal("slow relay won over fast direct")
	}
	backlog := lanes.lanes[1].receive.TryReserve(bytes*20, now)
	defer backlog.Close()
	lanes.orderLanesLocked(chosen, bytes)
	if chosen[0].identity.ID != 2 {
		t.Fatal("useful parallel relay capacity was ignored")
	}
	backlog.Close()
	lanes.lanes[2].receive = lanescheduling.ReceiveWindow{}
	seedDelivery(lanes.lanes[2], bytes, time.Millisecond, now)
	lanes.orderLanesLocked(chosen, bytes)
	if chosen[0].identity.ID != 2 {
		t.Fatal("route label overrode measured completion")
	}
}

func TestLaneSchedulingRescueRespectsRouteAndSuspension(t *testing.T) {
	descriptor := transferDescriptor(t, 1)
	input := validDemand(t, descriptor, 0)
	lanes := newBoundaryLaneSet(t, 123)
	for _, id := range []uint32{1, 2, 3} {
		route := RouteRelay
		if id == 1 {
			route = RouteDirect
		}
		_ = lanes.Add(Identity{ID: id}, route, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) { return records.BlockRecord{}, nil }))
	}
	primary := lanes.lanes[1]
	primary.performance.HasSuccessfulSample = true
	primary.performance.BytesPerSecond = float64(blockDemandBytes(input)) * 100
	lanes.lanes[2].performance.LastAttempt = time.Now().Add(-time.Hour)
	attempted := map[Identity]struct{}{{ID: 1}: {}}
	original := &laneAdmission{laneState: primary,
		permit:   primary.receive.TryReserve(blockDemandBytes(input), lanes.now().Add(-time.Hour)),
		expected: lanescheduling.DeliveryEstimate{Evidence: lanescheduling.DeliveryRecent, Duration: time.Second}}
	defer original.permit.Close()
	pending := lanes.queueRescue(t.Context(), Request{Demand: input}, attempted, original)
	defer lanes.withdrawRescue(pending)
	lanes.contentRoutePolicy = ContentRouteDirectOnly
	if selected, _, _ := lanes.takeRescue(pending); selected != nil {
		t.Fatal("route permission bypassed")
	}
	lanes.contentRoutePolicy = ContentRouteAll
	suspension, err := lanes.SuspendContent(Identity{ID: 3})
	if err != nil {
		t.Fatal(err)
	}
	if err := suspension.Resume(); err != nil {
		t.Fatal(err)
	}
	selected, _, _ := lanes.takeRescue(pending)
	if selected == nil || selected.identity.ID != 2 {
		t.Fatal("eligible rescue path was not selected")
	}
	selected.permit.Close()
	selected.performance.Complete(lanes.now(), blockDemandBytes(input), false)
	selected.inflight--
	lanes.exploration.Release(lanescheduling.Rescue)
	lanes.attempts.Done()
}

func TestLaneMeasurementRequiresAnIdleSampleAndUsefulAlternative(t *testing.T) {
	now := time.Unix(100, 0)
	lanes, err := New(Config{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](124), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer lanes.Close()
	for _, id := range []uint32{1, 2} {
		if err := lanes.Add(Identity{ID: id}, RouteTURN, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
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

// Real receipt evidence avoids manufacturing scheduling capacity from authentication counters.
func seedDelivery(state *laneState, bytes uint64, elapsed time.Duration, now time.Time) {
	permit := state.receive.TryReserve(bytes, now.Add(-elapsed))
	permit.Receive(bytes, now)
	permit.Close()
}
