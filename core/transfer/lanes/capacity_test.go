package lanes

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/transfer/fault"
)

func congestedBlock() error {
	value, _ := fault.NewSession(fault.ScopeOutputPause, fault.SessionTransport)
	return NewDemandReassignableAfterRetirement(fault.Wrap(value, &framechannel.BandwidthWaitError{Cause: errors.New("fragment inactivity"), RetryAfter: time.Second}))
}

func TestCongestionWaitDoesNotSpendFailureBudgetOrHoldOtherPaths(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		set := newBoundaryLaneSet(t, 136)
		descriptor := transferDescriptor(t, 1)
		demand := validDemand(t, descriptor, 0)
		var calls atomic.Int32
		_ = set.Add(Identity{ID: 1}, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
			calls.Add(1)
			return records.BlockRecord{}, congestedBlock()
		}))
		done := make(chan error, 1)
		go func() {
			_, err := set.Fetch(t.Context(), Request{Demand: demand, Validate: validateTransferRecord(demand)})
			done <- err
		}()
		for range MaxDemandAttempts + 1 {
			synctest.Wait()
			before := calls.Load()
			time.Sleep(29 * time.Second)
			synctest.Wait()
			if calls.Load() != before {
				t.Fatal("congestion caused an immediate retry")
			}
			time.Sleep(time.Second)
		}
		synctest.Wait()
		if set.lanes[1].failures != 0 {
			t.Fatal("congestion consumed failure budget")
		}
		_ = set.Add(Identity{ID: 2}, RouteDirect, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
			return transferRecord(t, descriptor, 0), nil
		}))
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		set.Close()
	})
}

func TestCongestionWaitRemainsCancelable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		set := newBoundaryLaneSet(t, 137)
		descriptor := transferDescriptor(t, 1)
		demand := validDemand(t, descriptor, 0)
		_ = set.Add(Identity{ID: 1}, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
			return records.BlockRecord{}, congestedBlock()
		}))
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, err := set.Fetch(ctx, Request{Demand: demand, Validate: validateTransferRecord(demand)})
			done <- err
		}()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}
