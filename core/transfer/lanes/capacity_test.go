package lanes

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/transfer/fault"
)

type testCapacityPressure struct {
	mu        sync.Mutex
	remaining time.Duration
	cleared   bool
	clearance chan struct{}
}

func (p *testCapacityPressure) setPressure(remaining time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.remaining = remaining
	p.cleared = false
}

func (p *testCapacityPressure) clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.remaining = 0
	p.cleared = true
	if p.clearance != nil {
		close(p.clearance)
		p.clearance = nil
	}
}

func (p *testCapacityPressure) CapacityWait() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.remaining
}

func (p *testCapacityPressure) CapacityCleared() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cleared
}

func (p *testCapacityPressure) CapacityClearance() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cleared {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	if p.clearance == nil {
		p.clearance = make(chan struct{})
	}
	return p.clearance
}

func congestedBlock(retryAfter time.Duration, pressure framechannel.CapacityPressure) error {
	value, _ := fault.NewSession(fault.ScopeOutputPause, fault.SessionTransport)
	return NewDemandReassignableAfterRetirement(fault.Wrap(value, &framechannel.BandwidthWaitError{
		Cause: errors.New("fragment inactivity"), RetryAfter: retryAfter, Pressure: pressure,
	}))
}

func TestCongestionWaitRespectsServerRetryAfterAndMinimumFloor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		set := newBoundaryLaneSet(t, 136)
		descriptor := transferDescriptor(t, 1)
		demand := validDemand(t, descriptor, 0)
		var calls atomic.Int32
		_ = set.Add(Identity{ID: 1}, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
			if calls.Add(1) == 1 {
				// Sub-floor server hints clamp to minimumCapacityRetry rather than spinning.
				return records.BlockRecord{}, congestedBlock(500*time.Millisecond, nil)
			}
			// Larger server hints are honored directly without inflating them to 30s.
			return records.BlockRecord{}, congestedBlock(15*time.Second, nil)
		}))
		done := make(chan error, 1)
		go func() {
			_, err := set.Fetch(t.Context(), Request{Demand: demand, Validate: validateTransferRecord(demand)})
			done <- err
		}()
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("calls=%d, want 1", calls.Load())
		}
		time.Sleep(minimumCapacityRetry - time.Millisecond)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatal("congestion bypassed minimumCapacityRetry floor")
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatalf("calls=%d, want 2 after minimumCapacityRetry", calls.Load())
		}
		for range MaxDemandAttempts {
			before := calls.Load()
			time.Sleep(15*time.Second - time.Millisecond)
			synctest.Wait()
			if calls.Load() != before {
				t.Fatal("congestion ignored server RetryAfter hint")
			}
			time.Sleep(time.Millisecond)
			synctest.Wait()
			if calls.Load() != before+1 {
				t.Fatalf("calls=%d, want %d after server RetryAfter", calls.Load(), before+1)
			}
		}
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

func TestCongestionWaitWakesOnExplicitClearanceAndCooldownsAfterFailedProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		set := newBoundaryLaneSet(t, 138)
		descriptor := transferDescriptor(t, 1)
		demand := validDemand(t, descriptor, 0)
		pressure := &testCapacityPressure{}
		var calls atomic.Int32
		_ = set.Add(Identity{ID: 1}, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
			switch calls.Add(1) {
			case 1:
				pressure.setPressure(15 * time.Second)
				return records.BlockRecord{}, congestedBlock(15*time.Second, pressure)
			case 2:
				// The early probe fails under renewed congestion, spending early clearance.
				pressure.setPressure(15 * time.Second)
				return records.BlockRecord{}, congestedBlock(15*time.Second, pressure)
			default:
				return transferRecord(t, descriptor, 0), nil
			}
		}))
		done := make(chan error, 1)
		go func() {
			_, err := set.Fetch(t.Context(), Request{Demand: demand, Validate: validateTransferRecord(demand)})
			done <- err
		}()
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("calls=%d, want 1", calls.Load())
		}
		// Explicit clearance immediately admits one probe without waiting 15s.
		pressure.clear()
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatalf("calls=%d, want 2 immediately after clearance", calls.Load())
		}
		// A second clearance after a failed probe does not bypass the fallback cooldown.
		pressure.clear()
		time.Sleep(15*time.Second - time.Millisecond)
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatal("failed probe did not hold cooldown until fallback deadline")
		}
		time.Sleep(time.Millisecond)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 3 {
			t.Fatalf("calls=%d, want 3", calls.Load())
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
			return records.BlockRecord{}, congestedBlock(15*time.Second, nil)
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
		set.Close()
	})
}
