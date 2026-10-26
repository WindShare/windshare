package lanescheduling

import (
	"testing"
	"time"
)

func TestFrontierRescuePolicy(t *testing.T) {
	cold := DeliveryEstimate{Evidence: DeliveryUnknown, Duration: InitialBlockTime}
	recent := func(duration time.Duration) DeliveryEstimate {
		return DeliveryEstimate{Evidence: DeliveryRecent, Duration: duration}
	}
	stale := func(duration time.Duration) DeliveryEstimate {
		return DeliveryEstimate{Evidence: DeliveryStale, Duration: duration}
	}
	tests := []struct {
		name        string
		progress    DeliveryProgress
		initial     DeliveryEstimate
		alternative DeliveryEstimate
		reason      RescueReason
	}{
		{
			name: "minimum delay protects transient startup",
			progress: DeliveryProgress{RemainingBytes: 1000, Elapsed: MinimumRescueDelay - time.Millisecond,
				Silent: MinimumRescueDelay - time.Millisecond, Remaining: cold},
			initial: cold, alternative: recent(time.Millisecond),
		},
		{
			name: "measured alternative helps before bootstrap expires",
			progress: DeliveryProgress{RemainingBytes: 1000, Elapsed: MinimumRescueDelay,
				Silent: MinimumRescueDelay, Remaining: cold},
			initial: cold, alternative: recent(time.Millisecond), reason: RescueFasterDelivery,
		},
		{
			name: "ongoing receipt can outlive the initial forecast",
			progress: DeliveryProgress{RemainingBytes: 10, Elapsed: 10 * time.Second,
				Silent: 10 * time.Millisecond, Remaining: recent(20 * time.Millisecond)},
			initial: recent(100 * time.Millisecond), alternative: recent(100 * time.Millisecond),
		},
		{
			name:     "fully received block needs no network rescue",
			progress: DeliveryProgress{Elapsed: 5 * time.Second, Silent: time.Second, Remaining: recent(time.Millisecond)},
			initial:  cold, alternative: recent(20 * time.Millisecond),
		},
		{
			name: "silent frontier does not wait for twice an old forecast",
			progress: DeliveryProgress{RemainingBytes: 131072, Elapsed: 8824 * time.Millisecond,
				Silent: 8824 * time.Millisecond, Remaining: stale(250 * time.Millisecond)},
			initial: recent(5056 * time.Millisecond), alternative: recent(439 * time.Millisecond), reason: RescueNoProgress,
		},
		{
			name: "unknown alternative cannot win by bootstrap score",
			progress: DeliveryProgress{RemainingBytes: 1000, Elapsed: HedgeDelayFactor*InitialBlockTime - time.Millisecond,
				Silent: HedgeDelayFactor*InitialBlockTime - time.Millisecond, Remaining: cold},
			initial: cold, alternative: cold,
		},
		{
			name: "unknown alternative retains bounded cold-start liveness",
			progress: DeliveryProgress{RemainingBytes: 1000, Elapsed: HedgeDelayFactor * InitialBlockTime,
				Silent: HedgeDelayFactor * InitialBlockTime, Remaining: cold},
			initial: cold, alternative: cold, reason: RescueBootstrapOverdue,
		},
		{
			name: "fresh progress prevents an unmeasured rescue",
			progress: DeliveryProgress{RemainingBytes: 100, Elapsed: 300 * time.Millisecond,
				Remaining: recent(100 * time.Millisecond)},
			initial: recent(100 * time.Millisecond), alternative: cold,
		},
		{
			name: "startup gets one alternative delivery interval",
			progress: DeliveryProgress{RemainingBytes: 1000, Elapsed: 99 * time.Millisecond,
				Silent: 99 * time.Millisecond, Remaining: recent(time.Second)},
			initial: recent(time.Second), alternative: recent(100 * time.Millisecond),
		},
		{
			name: "old fast evidence can revalidate a slow frontier",
			progress: DeliveryProgress{RemainingBytes: 1000, Elapsed: 400 * time.Millisecond,
				Silent: 5 * time.Millisecond, Remaining: recent(time.Second)},
			initial: recent(time.Second), alternative: stale(100 * time.Millisecond), reason: RescueFasterDelivery,
		},
		{
			name: "stale speed is not counted at face value",
			progress: DeliveryProgress{RemainingBytes: 1000, Elapsed: 400 * time.Millisecond,
				Silent: 5 * time.Millisecond, Remaining: recent(300 * time.Millisecond)},
			initial: recent(time.Second), alternative: stale(100 * time.Millisecond),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RescueDecision(tt.progress, tt.initial, tt.alternative); got != tt.reason {
				t.Fatalf("rescue: %q, want %q", got, tt.reason)
			}
		})
	}
}
