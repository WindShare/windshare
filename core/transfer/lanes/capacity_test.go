package lanes

import (
	"context"
	"errors"
	"slices"
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

func TestCongestionBackoffExposesNetworkWaitingDemandWhilePreservingCooldown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		set := newBoundaryLaneSet(t, 139)
		descriptor := transferDescriptor(t, 1)
		demand := validDemand(t, descriptor, 0)
		wantBytes := blockDemandBytes(demand)
		var primaryCalls atomic.Int32
		_ = set.Add(Identity{ID: 1, Epoch: 1}, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
			primaryCalls.Add(1)
			return records.BlockRecord{}, congestedBlock(15*time.Second, nil)
		}))
		done := make(chan error, 1)
		go func() {
			_, err := set.Fetch(t.Context(), Request{Demand: demand, Validate: validateTransferRecord(demand)})
			done <- err
		}()
		synctest.Wait()
		if primaryCalls.Load() != 1 {
			t.Fatalf("primary calls=%d, want 1", primaryCalls.Load())
		}
		got := set.SchedulingDemand()
		if got.QueuedBlocks != 1 || got.InFlightBytes != 0 || got.CapacityWaitingBytes != 0 || got.NetworkWaitingBytes != wantBytes {
			t.Fatalf("cooldown demand=%+v, want queued=1 inflight=0 capacity=0 network=%d", got, wantBytes)
		}
		if recovery := set.RecoveryDemand(); len(recovery) != 0 {
			t.Fatalf("cooldown falsely requested path recovery: %v", recovery)
		}
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if primaryCalls.Load() != 1 {
			t.Fatalf("primary bypassed cooldown: calls=%d", primaryCalls.Load())
		}
		_ = set.Add(Identity{ID: 2, Epoch: 1}, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
			return transferRecord(t, descriptor, 0), nil
		}))
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if primaryCalls.Load() != 1 {
			t.Fatalf("backup takeover disturbed primary cooldown: calls=%d", primaryCalls.Load())
		}
		if after := set.SchedulingDemand(); after.QueuedBlocks != 0 || after.NetworkWaitingBytes != 0 {
			t.Fatalf("settled demand=%+v", after)
		}
		set.Close()
	})
}

func TestCongestionBackoffPreservesBackpressureSuspensionAndFailedRouteRecovery(t *testing.T) {
	t.Run("consumer backpressure wins over cooldown", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			set := newBoundaryLaneSet(t, 140)
			descriptor := transferDescriptor(t, 1)
			demand := validDemand(t, descriptor, 0)
			wantBytes := blockDemandBytes(demand)
			var blocked atomic.Bool
			changed := make(chan struct{})
			_ = set.Add(Identity{ID: 1, Epoch: 1}, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
				blocked.Store(true)
				return records.BlockRecord{}, congestedBlock(15*time.Second, nil)
			}))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := set.Fetch(ctx, Request{Demand: demand, Validate: validateTransferRecord(demand), Ready: func() CapacityReadiness {
					if blocked.Load() {
						return CapacityReadiness{Kind: CapacityConsumer, Changed: changed}
					}
					return CapacityReadiness{Kind: CapacityReady}
				}})
				done <- err
			}()
			synctest.Wait()
			got := set.SchedulingDemand()
			if got.NetworkWaitingBytes != 0 || got.CapacityWaitingBytes != wantBytes {
				t.Fatalf("backpressure demand=%+v, want network=0 capacity=%d", got, wantBytes)
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	})

	t.Run("suspended relay stays route-blocked", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			set := newBoundaryLaneSet(t, 141)
			descriptor := transferDescriptor(t, 1)
			demand := validDemand(t, descriptor, 0)
			identity := Identity{ID: 1, Epoch: 1}
			_ = set.Add(identity, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
				return records.BlockRecord{}, congestedBlock(15*time.Second, nil)
			}))
			if _, err := set.SuspendContent(identity); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := set.Fetch(ctx, Request{Demand: demand, Validate: validateTransferRecord(demand)})
				done <- err
			}()
			synctest.Wait()
			if got := set.SchedulingDemand(); got.QueuedBlocks != 1 || got.NetworkWaitingBytes != 0 {
				t.Fatalf("suspended demand=%+v, want queued=1 network=0", got)
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	})

	t.Run("failed direct generation retains recovery demand during relay cooldown", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			set := newBoundaryLaneSet(t, 142)
			supply, err := set.ExpectRoute(RouteDirect)
			if err != nil {
				t.Fatal(err)
			}
			defer supply.Close()
			descriptor := transferDescriptor(t, 1)
			demand := validDemand(t, descriptor, 0)
			wantBytes := blockDemandBytes(demand)
			direct := Identity{ID: 2, Epoch: 1}
			relay := Identity{ID: 1, Epoch: 1}
			_ = set.Add(direct, RouteDirect, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
				return records.BlockRecord{}, retiredBlockFailure()
			}))
			_ = set.Add(relay, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
				return records.BlockRecord{}, congestedBlock(15*time.Second, nil)
			}))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := set.Fetch(ctx, Request{Demand: demand, Validate: validateTransferRecord(demand)})
				done <- err
			}()
			synctest.Wait()
			if got := set.SchedulingDemand(); got.NetworkWaitingBytes != wantBytes {
				t.Fatalf("mixed demand=%+v, want network=%d", got, wantBytes)
			}
			if got := set.RecoveryDemand(); !slices.Equal(got, []Identity{direct}) {
				t.Fatalf("recovery demand=%v, want [%v]", got, direct)
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	})
}
