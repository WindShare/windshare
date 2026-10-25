package lanes

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/transfer/lanescheduling"
)

func TestFrontierSelectionDoesNotPromoteUnmeasuredBootstrap(t *testing.T) {
	for _, route := range []Route{RouteDirect, RouteTURN, RouteRelay} {
		t.Run(strconv.Itoa(int(route)), func(t *testing.T) {
			set := newBoundaryLaneSet(t, 141)
			now := time.Unix(1, 0)
			set.now = func() time.Time { return now }
			demand := validDemand(t, transferDescriptor(t, 2), 0)
			bytes := blockDemandBytes(demand)
			source := laneFunction(func(context.Context, Demand) (records.BlockRecord, error) { return records.BlockRecord{}, nil })
			_ = set.Add(Identity{ID: 1}, route, source)
			_ = set.Add(Identity{ID: 2}, RouteDirect, source)
			measured := set.lanes[1]
			seedDelivery(measured, bytes, 200*time.Millisecond, now)
			pending := measured.receive.TryReserve(bytes, now)
			defer pending.Close()
			chosen := []*laneState{set.lanes[2], measured}
			set.orderLanesLocked(chosen, bytes)
			if chosen[0] != measured {
				t.Fatal("unknown bootstrap displaced current delivery evidence")
			}
		})
	}
}

func TestNormalDeliveryDoesNotAllocateRescueIntents(t *testing.T) {
	set := newBoundaryLaneSet(t, 144)
	now := time.Unix(10, 0)
	set.now = func() time.Time { return now }
	demand := validDemand(t, transferDescriptor(t, 1), 0)
	source := laneFunction(func(context.Context, Demand) (records.BlockRecord, error) { return records.BlockRecord{}, nil })
	_ = set.Add(Identity{ID: 1}, RouteDirect, source)
	state := set.lanes[1]
	original := &laneAdmission{laneState: state, permit: state.receive.TryReserve(blockDemandBytes(demand), now),
		expected: lanescheduling.DeliveryEstimate{Evidence: lanescheduling.DeliveryUnknown, Duration: lanescheduling.InitialBlockTime}}
	attempted := map[Identity]struct{}{state.identity: {}}
	now = now.Add(time.Second)
	if set.queueRescue(t.Context(), Request{Demand: demand}, attempted, original) != nil {
		t.Fatal("single-path transfer allocated a useless rescue")
	}
	original.permit.Close()
	original.permit = state.receive.TryReserve(blockDemandBytes(demand), now)
	defer original.permit.Close()
	_ = set.Add(Identity{ID: 2}, RouteTURN, source)
	if set.queueRescue(t.Context(), Request{Demand: demand}, attempted, original) != nil || len(set.pending) != 0 {
		t.Fatal("healthy startup allocated speculative work")
	}
}

func TestRescueFollowsTheConsumerFrontier(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		set := newBoundaryLaneSet(t, 142)
		descriptor := transferDescriptor(t, 2)
		demand := validDemand(t, descriptor, 0)
		var distance atomic.Uint64
		distance.Store(1)
		started := make(chan struct{})
		done := make(chan error, 1)
		_ = set.Add(Identity{ID: 1}, RouteDirect, laneFunction(func(ctx context.Context, _ Demand) (records.BlockRecord, error) {
			close(started)
			<-ctx.Done()
			return records.BlockRecord{}, ctx.Err()
		}))
		go func() {
			_, err := set.Fetch(t.Context(), Request{Demand: demand, Validate: validateTransferRecord(demand),
				Order: func() WorkOrder { return WorkOrder{Distance: distance.Load()} }})
			done <- err
		}()
		<-started
		var rescued atomic.Bool
		_ = set.Add(Identity{ID: 2}, RouteTURN, laneFunction(func(_ context.Context, input Demand) (records.BlockRecord, error) {
			rescued.Store(true)
			return transferRecord(t, descriptor, input.Index), nil
		}))
		set.mu.Lock()
		seedDelivery(set.lanes[2], blockDemandBytes(demand), time.Millisecond, set.now())
		set.mu.Unlock()
		time.Sleep(200 * time.Millisecond)
		if rescued.Load() {
			t.Fatal("read-ahead consumed a rescue budget before blocking output")
		}
		distance.Store(0)
		time.Sleep(200 * time.Millisecond)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if !rescued.Load() {
			t.Fatal("new frontier was not rescued")
		}
		set.Close()
	})
}

func TestQueuedRescueReceivesCreditBeforeReadAheadAndWithdrawsUnusedAdmission(t *testing.T) {
	set := newBoundaryLaneSet(t, 143)
	now := time.Unix(10, 0)
	set.now = func() time.Time { return now }
	descriptor := transferDescriptor(t, 4)
	demand := validDemand(t, descriptor, 0)
	bytes := blockDemandBytes(demand)
	for id, route := range []Route{RouteDirect, RouteTURN, RouteRelay} {
		_ = set.Add(Identity{ID: uint32(id + 1)}, route, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
			t.Fatal("allocation must not dispatch")
			return records.BlockRecord{}, nil
		}))
	}
	primary, fast := set.lanes[1], set.lanes[2]
	seedDelivery(fast, bytes, 100*time.Millisecond, now)
	backlog := fast.receive.TryReserve(4*bytes, now)
	defer backlog.Close()
	original := &laneAdmission{laneState: primary,
		permit:   primary.receive.TryReserve(bytes, now.Add(-time.Hour)),
		expected: lanescheduling.DeliveryEstimate{Evidence: lanescheduling.DeliveryRecent, Duration: time.Second}}
	defer original.permit.Close()
	var claimed, released int
	rescue := set.queueRescue(t.Context(), Request{Demand: demand, Claim: func() (func(), <-chan struct{}, error) {
		claimed++
		return func() { released++ }, nil, nil
	}}, map[Identity]struct{}{primary.identity: {}}, original)
	defer set.withdrawRescue(rescue)
	readAhead := &pendingAdmission{ctx: t.Context(), queuedAt: now, sequence: 0,
		request:   Request{Demand: validDemand(t, descriptor, 1), Order: func() WorkOrder { return WorkOrder{Distance: 1} }},
		attempted: map[Identity]struct{}{primary.identity: {}, set.lanes[3].identity: {}}}
	set.mu.Lock()
	set.pending = append(set.pending, readAhead)
	set.allocatePendingLocked()
	if rescue.done || claimed != 0 || set.lanes[3].inflight != 0 {
		set.mu.Unlock()
		t.Fatal("waiting rescue consumed capacity or chose an unmeasured idle lane")
	}
	now = now.Add(500 * time.Millisecond)
	backlog.Receive(bytes*5/2, now)
	set.allocatePendingLocked()
	if !rescue.done || rescue.selected[0].identity != fast.identity || readAhead.done || claimed != 1 {
		set.mu.Unlock()
		t.Fatal("read-ahead stole the frontier's next useful credit")
	}
	set.mu.Unlock()
	// The primary can finish before its owner takes the reserved rescue.
	set.withdrawRescue(rescue)
	if released != 1 || fast.inflight != 0 || !set.exploration.Available(lanescheduling.Rescue, now) {
		t.Fatal("undispatched rescue leaked ownership")
	}
}
