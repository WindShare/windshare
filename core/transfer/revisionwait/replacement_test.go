package revisionwait

import (
	"context"
	"testing"
	"time"
)

type replacementFence struct {
	current GenerationToken
	changes chan GenerationChange
}

func (f *replacementFence) Current() GenerationToken { return f.current }
func (f *replacementFence) WaitForChange(ctx context.Context, _ GenerationToken) (GenerationChange, error) {
	select {
	case <-ctx.Done():
		return GenerationChange{}, ctx.Err()
	case change := <-f.changes:
		return change, nil
	}
}

func TestAuthenticatedReplacementReopensRevisionWithoutResettingWaitBudget(t *testing.T) {
	first, second := waitTestToken(t, 11), waitTestToken(t, 12)
	fence := &replacementFence{current: first, changes: make(chan GenerationChange, 1)}
	clock := &fakeClock{now: time.Unix(1, 0)}
	timers := &manualTimerFactory{created: make(chan *fakeTimer, 1)}
	coordinator := waitTestCoordinator(t, clock, timers, fence, 10*time.Second, 0)
	operation, err := coordinator.NewOperation(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan WaitOutcome, 1)
	go func() {
		outcome, err := operation.Wait(context.Background(), waitTestSignal(t, first, time.Second))
		if err != nil {
			t.Error(err)
		}
		done <- outcome
	}()
	<-timers.created
	clock.advance(3 * time.Second)
	change, _ := NewGenerationReplacement(first, second, nil)
	fence.changes <- change
	if outcome := <-done; outcome != WaitRetry {
		t.Fatal(outcome)
	}
	if got := coordinator.Snapshot().AccumulatedWait; got != 3*time.Second {
		t.Fatalf("spent=%v", got)
	}
	go func() {
		outcome, err := operation.Wait(context.Background(), waitTestSignal(t, second, time.Second))
		if err == nil {
			t.Error("ended lifetime was allowed to retry")
		}
		done <- outcome
	}()
	<-timers.created
	clock.advance(2 * time.Second)
	end, _ := NewGenerationEnd(second, nil)
	fence.changes <- end
	if outcome := <-done; outcome != WaitGenerationEnded {
		t.Fatal(outcome)
	}
	if got := coordinator.Snapshot().AccumulatedWait; got != 5*time.Second {
		t.Fatalf("spent=%v", got)
	}
}
