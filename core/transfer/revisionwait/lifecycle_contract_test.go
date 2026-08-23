package revisionwait

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDefaultPolicyAndDirectCancelCloseRetryAuthority(t *testing.T) {
	token := waitTestToken(t, 42)
	fence, err := NewLifetimeFence(token, context.Background())
	if err != nil {
		t.Fatalf("new lifetime fence: %v", err)
	}
	defaults, err := NewCoordinator(Config{GenerationFence: fence})
	if err != nil {
		t.Fatalf("new default coordinator: %v", err)
	}
	if defaults.budget != DefaultWaitBudget || defaults.jitterLimit != DefaultAdditiveJitterLimit ||
		defaults.visibilityThreshold != DefaultVisibilityThreshold || defaults.clock == nil || defaults.timers == nil ||
		defaults.jitter == nil || defaults.waitIDs == nil {
		t.Fatalf("default coordinator policy = %+v", defaults)
	}

	if snapshot := (*Coordinator)(nil).Snapshot(); snapshot != (Snapshot{}) {
		t.Fatalf("nil coordinator snapshot = %+v", snapshot)
	}
	if _, err := (*Coordinator)(nil).NewOperation(nil, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil coordinator operation = %v", err)
	}
	var nilOperation *Operation
	if !nilOperation.ID().IsZero() {
		t.Fatal("nil operation exposed an identity")
	}
	nilOperation.Succeed()
	nilOperation.Cancel(nil)
	nilOperation.Stop()

	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	timers := &instantTimerFactory{clock: clock}
	coordinator := waitTestCoordinator(t, clock, timers, fence, 10*time.Second, 0)
	var traces []Trace
	operation, err := coordinator.NewOperation(nil, TraceFunc(func(event Trace) {
		traces = append(traces, event)
	}))
	if err != nil {
		t.Fatalf("new operation: %v", err)
	}
	if outcome, err := operation.Wait(context.Background(), waitTestSignal(t, token, time.Second)); err != nil || outcome != WaitRetry {
		t.Fatalf("initial retry outcome = %v, err = %v", outcome, err)
	}
	active := coordinator.Snapshot()
	if active.ActiveDuration(clock.Now()) != time.Second {
		t.Fatalf("active duration = %v, want 1s", active.ActiveDuration(clock.Now()))
	}

	cause := errors.New("caller replaced retry authority")
	operation.Cancel(cause)
	closed := coordinator.Snapshot()
	if closed.ActiveWaiters != 0 || closed.ActiveDuration(clock.Now()) != 0 {
		t.Fatalf("cancelled retry snapshot = %+v", closed)
	}
	if len(traces) != 3 || traces[2].Stage != TraceWaitCanceled || !errors.Is(traces[2].Cause, cause) {
		t.Fatalf("cancel traces = %+v", traces)
	}
	// Cancellation is terminal and idempotent from the caller's perspective.
	operation.Cancel(nil)
	operation.Succeed()
}
