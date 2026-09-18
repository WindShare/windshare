package task

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/observationstream"
)

type detail struct{ Value int }

func (detail) EngineEvent() {}

func testConfig(run func(context.Context, Control) Completion[int]) Config[int] {
	return Config[int]{
		ID: "operation", Now: func() time.Time { return time.Unix(100, 0) },
		Random: bytes.NewReader([]byte{1}), CleanupTimeout: time.Minute,
		ObservationCapacity: 4, Run: run,
	}
}

func TestWaitAndObservationDetachmentDoNotCancelExecution(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	current, err := Start(context.Background(), testConfig(func(ctx context.Context, control Control) Completion[int] {
		close(entered)
		<-release
		if ctx.Err() != nil {
			return Completion[int]{Settlement: Settlement{Outcome: OutcomeCancelled, Err: ctx.Err()}}
		}
		return Completion[int]{Value: 42, Settlement: Settlement{Outcome: OutcomeSuccess}}
	}))
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := current.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error = %v", err)
	}
	if current.State() != Running || current.ID() != "operation" {
		t.Fatal("detaching changed execution")
	}
	close(release)
	result, err := current.Wait(context.Background())
	if err != nil || result.Value != 42 || result.Err != nil || result.StopReason != NoStop {
		t.Fatalf("result = %+v, %v", result, err)
	}
	current.Cancel()
	if current.State() != Finished {
		t.Fatal("completed task was cancelled")
	}
	if again, err := current.Wait(ctx); err != nil || again.Value != 42 {
		t.Fatalf("durable result = %+v, %v", again, err)
	}
}

func TestStopCausesAndCleanupAreIndependent(t *testing.T) {
	for _, reason := range []StopReason{Cancelled, ShareStopped, ApplicationClosed} {
		t.Run(reason.cause().Error(), func(t *testing.T) {
			observed := make(chan StopReason, 1)
			cleaning := make(chan bool, 1)
			release := make(chan struct{})
			cleanupErr := errors.New("cleanup failed")
			current, err := Start(context.Background(), testConfig(func(ctx context.Context, control Control) Completion[int] {
				<-ctx.Done()
				observed <- Reason(ctx)
				cleanup, cancel := control.CleanupContext()
				defer cancel()
				_, deadline := cleanup.Deadline()
				cleaning <- cleanup.Err() == nil && deadline
				<-release
				return Completion[int]{Settlement: Settlement{Outcome: OutcomeFailed, FailureClass: FailureLocal, Err: errors.Join(context.Cause(ctx), cleanupErr), CleanupError: cleanupErr}}
			}))
			if err != nil {
				t.Fatal(err)
			}
			current.Stop(NoStop)
			if current.State() != Running {
				t.Fatal("invalid stop changed state")
			}
			current.Stop(reason)
			current.Cancel()
			if got := <-observed; got != reason {
				t.Fatalf("reason = %v", got)
			}
			if !<-cleaning {
				t.Fatal("cleanup inherited cancellation or lacked deadline")
			}
			select {
			case <-current.Done():
				t.Fatal("finished before cleanup")
			default:
			}
			close(release)
			result, err := current.Wait(context.Background())
			if err != nil || result.StopReason != reason || !errors.Is(result.Err, reason.cause()) || !errors.Is(result.CleanupError, cleanupErr) {
				t.Fatalf("result = %+v, %v", result, err)
			}
			var states []State
			for item := range current.Observations() {
				if item.TaskID != current.ID() || !item.At.Equal(time.Unix(100, 0)) {
					t.Fatalf("bad envelope: %+v", item)
				}
				if lifecycle, ok := item.Event.(Lifecycle); ok {
					states = append(states, lifecycle.State)
				}
			}
			if len(states) != 3 || states[0] != Running || states[1] != Stopping || states[2] != Finished {
				t.Fatalf("states = %v", states)
			}
		})
	}
}

func TestParentCancellationCarriesValuesButCleanupOutlivesParent(t *testing.T) {
	type key struct{}
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "trace"))
	current, err := Start(parent, testConfig(func(ctx context.Context, control Control) Completion[int] {
		<-ctx.Done()
		cleanup, cleanupCancel := control.CleanupContext()
		defer cleanupCancel()
		if ctx.Value(key{}) != "trace" || cleanup.Value(key{}) != "trace" || cleanup.Err() != nil {
			return Completion[int]{Settlement: Settlement{Outcome: OutcomeFailed, FailureClass: FailureLocal, Err: errors.New("lost trace values or inherited cancellation")}}
		}
		return Completion[int]{Value: int(Reason(ctx)), Settlement: Settlement{Outcome: OutcomeCancelled}}
	}))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	result, err := current.Wait(context.Background())
	if err != nil || result.Err != nil || result.Value != int(Cancelled) || result.StopReason != Cancelled {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if Reason(context.Background()) != NoStop {
		t.Fatal("live context has stop reason")
	}
	if Reason(parent) != Cancelled {
		t.Fatal("foreign cancellation has no cancellation reason")
	}
}

func TestUndrainedObservationsRemainBoundedAndFinalResultIsDurable(t *testing.T) {
	config := testConfig(func(_ context.Context, control Control) Completion[int] {
		if control.Emit(nil) {
			return Completion[int]{Settlement: Settlement{Outcome: OutcomeFailed, FailureClass: FailureLocal, Err: errors.New("accepted nil event")}}
		}
		for i := range 1000 {
			control.Emit(detail{Value: i})
		}
		return Completion[int]{Value: 7, Settlement: Settlement{Outcome: OutcomeSuccess}}
	})
	config.ObservationCapacity = 2
	completed := make(chan error, 1)
	config.Completed = func(err error) { completed <- err }
	current, err := Start(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	result, err := current.Wait(context.Background())
	if err != nil || result.Value != 7 || result.Err != nil {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if result.Observations != (observationstream.Completion{Enqueued: 2, CapacityDropped: 1000}) {
		t.Fatalf("queue completion = %+v", result.Observations)
	}
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
	count := 0
	for range current.Observations() {
		count++
	}
	if count != 2 {
		t.Fatalf("retained = %d", count)
	}
}

func TestStartRejectsInvalidConfiguration(t *testing.T) {
	run := func(context.Context, Control) Completion[int] { return Completion[int]{} }
	for _, change := range []func(*Config[int]){
		func(config *Config[int]) { config.ID = "" },
		func(config *Config[int]) { config.Now = nil },
		func(config *Config[int]) { config.Random = nil },
		func(config *Config[int]) { config.CleanupTimeout = 0 },
		func(config *Config[int]) { config.Run = nil },
		func(config *Config[int]) { config.ObservationCapacity = 0 },
	} {
		config := testConfig(run)
		change(&config)
		if _, err := Start(context.Background(), config); err == nil {
			t.Fatal("accepted invalid config")
		}
	}
	var nilContext context.Context
	if _, err := Start(nilContext, testConfig(run)); err == nil {
		t.Fatal("accepted nil context")
	}
}

func TestSettlementIsIdenticalForWaitAndLifecycleObservers(t *testing.T) {
	cause, cleanupErr := errors.New("workflow failed"), errors.New("cleanup failed")
	for _, settlement := range []Settlement{
		{Outcome: OutcomeSuccess},
		{Outcome: OutcomePartial, FailureClass: FailureLocal, Err: cause},
		{Outcome: OutcomePaused, FailureClass: FailureNetwork, Err: cause},
		{Outcome: OutcomeCancelled},
		{Outcome: OutcomeCancelled, Err: context.Canceled},
		{Outcome: OutcomeStopped},
		{Outcome: OutcomeFailed, FailureClass: FailureUsage, Err: cause},
		{Outcome: OutcomePartial, FailureClass: FailureSourceDrift, Err: cause},
		{Outcome: OutcomeFailed, FailureClass: FailureLocal, Err: errors.Join(cause, cleanupErr), CleanupError: cleanupErr},
	} {
		current, err := Start(context.Background(), testConfig(func(context.Context, Control) Completion[int] {
			return Completion[int]{Value: 42, Settlement: settlement}
		}))
		if err != nil {
			t.Fatal(err)
		}
		result, err := current.Wait(context.Background())
		if err != nil || result.Settlement != settlement || result.Value != 42 || !result.Settlement.Valid() {
			t.Fatalf("settlement changed: want=%+v result=%+v wait=%v", settlement, result, err)
		}
		finished := 0
		for observation := range current.Observations() {
			lifecycle := observation.Event.(Lifecycle)
			if lifecycle.State == Finished {
				finished++
				if lifecycle.Settlement != result.Settlement {
					t.Fatalf("observers disagree: lifecycle=%+v result=%+v", lifecycle, result)
				}
			} else if lifecycle.Settlement != (Settlement{}) {
				t.Fatal("running task advertised a terminal result")
			}
		}
		if finished != 1 {
			t.Fatalf("finished observations = %d", finished)
		}
	}
}

func TestInvalidSettlementCannotLookSuccessful(t *testing.T) {
	cause := errors.New("retained diagnostic")
	cleanup := errors.New("release failed")
	for _, settlement := range []Settlement{
		{},
		{Outcome: Outcome(255)},
		{Outcome: OutcomeFailed, FailureClass: FailureClass(255), Err: cause},
		{Outcome: OutcomeSuccess, Err: cause},
		{Outcome: OutcomeSuccess, FailureClass: FailureLocal},
		{Outcome: OutcomeStopped, Err: cause},
		{Outcome: OutcomeCancelled, FailureClass: FailureLocal},
		{Outcome: OutcomePartial, FailureClass: FailureLocal},
		{Outcome: OutcomeFailed, FailureClass: FailureNone, Err: cause},
		{Outcome: OutcomeFailed, FailureClass: FailureUsage},
		{Outcome: OutcomePaused, FailureClass: FailureLocal, Err: cause, CleanupError: cleanup},
		{Outcome: OutcomeCancelled, CleanupError: cleanup},
		{Outcome: OutcomeSuccess, CleanupError: cleanup},
	} {
		if settlement.Valid() {
			t.Fatalf("invalid settlement accepted: %+v", settlement)
		}
		current, err := Start(context.Background(), testConfig(func(context.Context, Control) Completion[int] {
			return Completion[int]{Settlement: settlement}
		}))
		if err != nil {
			t.Fatal(err)
		}
		result, err := current.Wait(context.Background())
		if err != nil || !errors.Is(result.Err, ErrInvalidSettlement) || result.Outcome != OutcomeFailed || result.FailureClass != FailureLocal || !result.Settlement.Valid() {
			t.Fatalf("invalid settlement was not surfaced: %+v, %v", result, err)
		}
		if settlement.Err != nil && !errors.Is(result.Err, cause) || result.CleanupError != settlement.CleanupError {
			t.Fatal("invalid settlement lost its original diagnostic or cleanup error")
		}
		for observation := range current.Observations() {
			if lifecycle := observation.Event.(Lifecycle); lifecycle.State == Finished && lifecycle.Settlement != result.Settlement {
				t.Fatal("invalid settlement produced contradictory final views")
			}
		}
	}
}
