package peerset

import (
	"context"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/v2signal"
)

func TestDeferredMappingReservationReturnsBothBucketsOnlyIfUnused(t *testing.T) {
	now := time.Unix(100, 0)
	budget := NewBudget(now)
	reserve := budget.reserveDeferred(now)
	if reserve == nil {
		t.Fatal("missing late opportunity")
	}
	for range AttemptsPerWindow - 1 {
		refund, _ := budget.reserve(now)
		if refund == nil {
			t.Fatal("ordinary count changed")
		}
		refund(0)
	}
	if refund, _ := budget.reserve(now); refund != nil {
		t.Fatal("ordinary attempts consumed reserved late slot")
	}
	reserve.settle(false, 0)
	reserve.settle(false, 0)
	refund, _ := budget.reserve(now)
	if refund == nil {
		t.Fatal("unused reservation lost attempt")
	}
	refund(0)
	if refund, _ := budget.reserve(now); refund != nil {
		t.Fatal("double refund created attempt")
	}
}
func TestAttemptCapCancelsSynchronousProviderStartup(t *testing.T) {
	clock := newClock()
	owner, _ := New(Config{Clock: clock})
	entered := make(chan struct{})
	path, err := owner.Open(context.Background(), PathConfig{Demand: ContentDemand, StopAfterWave: true, Start: func(ctx context.Context, _ v2signal.Binding) (Attempt, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	receive(t, entered)
	timer := nextTimer(t, clock, AttemptBudget)
	clock.advance(AttemptBudget)
	timer.fire(clock.Now())
	backoff := nextTimer(t, clock, time.Second)
	clock.advance(time.Second)
	backoff.fire(clock.Now())
	receive(t, path.Done())
	if path.Result().Stopped {
		t.Fatal("attempt timeout canceled parent intent")
	}
}
func TestLateFreshAttemptKeepsFortySecondICEWindowUnderNonresettingWaveCap(t *testing.T) {
	clock := newClock()
	owner, _ := New(Config{Clock: clock})
	attempts := make(chan *fakeAttempt, 3)
	path, err := owner.Open(context.Background(), PathConfig{Demand: ContentDemand, StopAfterWave: true, Start: func(context.Context, v2signal.Binding) (Attempt, error) {
		a := newAttempt()
		attempts <- a
		return a, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	first := receive(t, attempts)
	nextTimer(t, clock, AttemptBudget)
	clock.advance(50 * time.Second)
	first.Close()
	backoff := nextTimer(t, clock, time.Second)
	clock.advance(time.Second)
	backoff.fire(clock.Now())
	receive(t, attempts)
	cap := nextTimer(t, clock, 69*time.Second)
	if cap.duration < MinimumAttemptOpportunity {
		t.Fatal("checking shortened")
	}
	clock.advance(69 * time.Second)
	cap.fire(clock.Now())
	backoff = nextTimer(t, clock, 2*time.Second)
	clock.advance(2 * time.Second)
	backoff.fire(clock.Now())
	receive(t, path.Done())
	if path.Err() != ErrWaveExhausted {
		t.Fatal(path.Err())
	}
}
