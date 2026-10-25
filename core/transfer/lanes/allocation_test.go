package lanes

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/transfer/lanescheduling"
)

func TestRemainingWorkMergesJobsWithoutClosingDiscovery(t *testing.T) {
	set := newBoundaryLaneSet(t, 135)
	complete := false
	closeFirst := set.ObserveRemainingWork(func() RemainingWork { return RemainingWork{Bytes: 500, Known: true} })
	closeSecond := set.ObserveRemainingWork(func() RemainingWork { return RemainingWork{Bytes: 100, Known: complete} })
	if got := set.SchedulingDemand(); got.RemainingBytes != 600 || got.RemainingKnown {
		t.Fatal(got)
	}
	complete = true
	if got := set.SchedulingDemand(); got.RemainingBytes != 600 || !got.RemainingKnown {
		t.Fatal(got)
	}
	closeFirst()
	if got := set.SchedulingDemand(); got.RemainingBytes != 100 || !got.RemainingKnown {
		t.Fatal(got)
	}
	closeSecond()
	if got := set.SchedulingDemand(); got.RemainingBytes != 0 || got.RemainingKnown {
		t.Fatal(got)
	}
}

func TestSharedAllocationExploresDistinctQueuedBlocks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		set := newBoundaryLaneSet(t, 131)
		descriptor := transferDescriptor(t, 2)
		primaryStarted, primaryRelease := make(chan struct{}), make(chan struct{})
		var probes atomic.Int32
		_ = set.Add(Identity{ID: 1}, RouteDirect, laneFunction(func(ctx context.Context, demand Demand) (records.BlockRecord, error) {
			close(primaryStarted)
			select {
			case <-primaryRelease:
				return transferRecord(t, descriptor, demand.Index), nil
			case <-ctx.Done():
				return records.BlockRecord{}, ctx.Err()
			}
		}))
		_ = set.Add(Identity{ID: 2}, RouteDirect, laneFunction(func(_ context.Context, demand Demand) (records.BlockRecord, error) {
			probes.Add(1)
			if demand.Index != 1 {
				t.Errorf("exploration duplicated active frontier %d", demand.Index)
			}
			return transferRecord(t, descriptor, demand.Index), nil
		}))
		bytes := blockDemandBytes(validDemand(t, descriptor, 0))
		seedDelivery(set.lanes[1], bytes, time.Millisecond, set.now())
		done := make(chan error, 1)
		go func() {
			demand := validDemand(t, descriptor, 0)
			_, err := set.Fetch(t.Context(), Request{Demand: demand, Validate: validateTransferRecord(demand)})
			done <- err
		}()
		<-primaryStarted
		demand := validDemand(t, descriptor, 1)
		if _, err := set.Fetch(t.Context(), Request{Demand: demand, Validate: validateTransferRecord(demand), Order: func() WorkOrder { return WorkOrder{Distance: 1} }}); err != nil {
			t.Fatal(err)
		}
		if probes.Load() != 1 {
			t.Fatalf("distinct probe count: %d", probes.Load())
		}
		close(primaryRelease)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		set.Close()
		if got := set.SchedulingDemand(); got.QueuedBlocks != 0 || got.InFlightBytes != 0 || got.UsefulBytes != 2*bytes {
			t.Fatalf("settlement: %+v", got)
		}
	})
}

func TestSharedAllocationCancellationWithdrawsCapacityBlockedWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		set := newBoundaryLaneSet(t, 132)
		descriptor := transferDescriptor(t, 1)
		var calls atomic.Int32
		_ = set.Add(Identity{ID: 1}, RouteDirect, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
			calls.Add(1)
			return records.BlockRecord{}, nil
		}))
		changed := make(chan struct{})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			demand := validDemand(t, descriptor, 0)
			_, err := set.Fetch(ctx, Request{Demand: demand, Validate: validateTransferRecord(demand), Ready: func() CapacityReadiness { return CapacityReadiness{Kind: CapacityConsumer, Changed: changed} }})
			done <- err
		}()
		synctest.Wait()
		if got := set.SchedulingDemand(); got.QueuedBlocks != 1 || got.NetworkWaitingBytes != 0 || got.CapacityWaitingBytes == 0 {
			t.Fatalf("pressure was misclassified: %+v", got)
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
		if calls.Load() != 0 || set.SchedulingDemand().QueuedBlocks != 0 {
			t.Fatal("canceled work retained a private backlog")
		}
	})
}

func TestSharedAllocationPrioritizesPreviewAndConsumerFrontiers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		set := newBoundaryLaneSet(t, 133)
		descriptor := transferDescriptor(t, 3)
		var ready atomic.Bool
		changed := make(chan struct{})
		started := make(chan uint64, 3)
		release := make(chan struct{})
		_ = set.Add(Identity{ID: 1}, RouteDirect, laneFunction(func(ctx context.Context, demand Demand) (records.BlockRecord, error) {
			started <- demand.Index
			select {
			case <-release:
				return transferRecord(t, descriptor, demand.Index), nil
			case <-ctx.Done():
				return records.BlockRecord{}, ctx.Err()
			}
		}))
		done := make(chan error, 3)
		for index, order := range []WorkOrder{{Distance: 2}, {Priority: Preview}, {LastServed: 1}} {
			go func() {
				demand := validDemand(t, descriptor, uint64(index))
				_, err := set.Fetch(t.Context(), Request{Demand: demand, Validate: validateTransferRecord(demand),
					Order: func() WorkOrder { return order }, Ready: func() CapacityReadiness {
						kind := CapacityConsumer
						if ready.Load() {
							kind = CapacityReady
						}
						return CapacityReadiness{Kind: kind, Changed: changed}
					}})
				done <- err
			}()
			synctest.Wait()
		}
		ready.Store(true)
		close(changed)
		if index := <-started; index != 1 {
			t.Fatalf("preview waited behind block %d", index)
		}
		close(release)
		for range 3 {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
		set.Close()
	})
}

func TestSharedAllocationWaitsForFastLaneBeforeUsingSlowerIdleLane(t *testing.T) {
	set := newBoundaryLaneSet(t, 134)
	descriptor := transferDescriptor(t, 1)
	demand := validDemand(t, descriptor, 0)
	bytes := blockDemandBytes(demand)
	for _, id := range []uint32{1, 2} {
		_ = set.Add(Identity{ID: id}, RouteDirect, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) { return records.BlockRecord{}, nil }))
	}
	fast, slow := set.lanes[1], set.lanes[2]
	now := set.now()
	seedDelivery(fast, bytes, 10*time.Millisecond, now)
	seedDelivery(slow, bytes, 10*time.Second, now)
	permit := fast.receive.TryReserve(bytes*100, now)
	defer permit.Close()
	pending := &pendingAdmission{request: Request{Demand: demand}, order: WorkOrder{}}
	ordered := []*laneState{slow, fast}
	set.orderLanesLocked(ordered, bytes)
	if chosen, _ := set.pendingCandidateLocked(pending, ordered, now); chosen != nil {
		t.Fatal("slow idle lane displaced a faster queued completion")
	}
	slow.receive = lanescheduling.ReceiveWindow{}
	seedDelivery(slow, bytes, time.Millisecond, now)
	set.orderLanesLocked(ordered, bytes)
	if chosen, _ := set.pendingCandidateLocked(pending, ordered, now); chosen != slow {
		t.Fatal("useful independent capacity was ignored")
	}
}
