package v2peer

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestObservationDispatcherCompletionAccountsCapacityAndIsIdempotent(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var first sync.Once
	dispatcher := newObservationDispatcher(
		2,
		func(int) {
			first.Do(func() {
				close(entered)
				<-release
			})
		},
		nil,
		nil,
	)
	dispatcher.publish(1)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("observation callback did not begin")
	}
	for value := 2; value <= 5; value++ {
		dispatcher.publish(value)
	}
	close(release)
	completion := dispatcher.complete(context.Background())
	if !completion.Drained || completion.Delivered != 3 || completion.Loss.Capacity != 2 ||
		completion.Loss.Total() != 2 {
		t.Fatalf("completion = %+v", completion)
	}
	if repeated := dispatcher.complete(context.Background()); !reflect.DeepEqual(repeated, completion) {
		t.Fatalf("repeated completion = %+v, want %+v", repeated, completion)
	}
}

func TestObservationDispatcherTimeoutAccountsQueuedCut(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Uint64
	dispatcher := newObservationDispatcher(
		4,
		func(int) {
			if calls.Add(1) == 1 {
				close(entered)
				<-release
			}
		},
		nil,
		nil,
	)
	dispatcher.callbackLimit = 10 * time.Millisecond
	dispatcher.publish(1)
	dispatcher.publish(2)
	dispatcher.publish(3)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("observation callback did not begin")
	}
	completion := dispatcher.complete(context.Background())
	if completion.Drained || completion.Delivered != 0 ||
		completion.Loss.CallbackTimeout != 1 || completion.Loss.Undrained != 2 {
		t.Fatalf("completion = %+v", completion)
	}
	close(release)
	if calls.Load() != 1 {
		t.Fatalf("callbacks begun after completion = %d", calls.Load())
	}
}

func TestObservationDispatcherDeadlineCountsActiveAndQueuedCut(t *testing.T) {
	entered := make(chan struct{})
	exited := make(chan struct{})
	var calls atomic.Uint64
	var committed atomic.Uint64
	dispatcher := newObservationDispatcher(
		4,
		func(int) {},
		nil,
		nil,
	)
	dispatcher.observeContext = func(ctx context.Context, _ int) {
		if calls.Add(1) == 1 {
			close(entered)
			<-ctx.Done()
			if ctx.Err() == nil {
				committed.Add(1)
			}
			close(exited)
		}
	}
	dispatcher.callbackLimit = time.Second
	dispatcher.publish(1)
	dispatcher.publish(2)
	dispatcher.publish(3)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("observation callback did not begin")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	completion := dispatcher.complete(ctx)
	if completion.Drained || completion.Delivered != 0 ||
		completion.Loss.CallbackTimeout != 0 || completion.Loss.Undrained != 3 {
		t.Fatalf("deadline completion = %+v", completion)
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("revoked callback did not exit")
	}
	if calls.Load() != 1 {
		t.Fatalf("callbacks begun after deadline completion = %d", calls.Load())
	}
	if committed.Load() != 0 {
		t.Fatalf("revoked callback committed %d late fact(s)", committed.Load())
	}
}

func TestPeerFactoryCompletionDrainsLossDiagnostics(t *testing.T) {
	diagnostics := make(chan PeerDiagnosticObservation, 2)
	factory, err := NewFactory(Config{
		Observer: SenderAttemptContextObserverFunc(func(context.Context, SenderAttemptObservation) {
			panic("observer defect")
		}),
		DiagnosticObserver: PeerDiagnosticContextObserverFunc(func(_ context.Context, observation PeerDiagnosticObservation) {
			diagnostics <- observation
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	factory.observeSenderAttempt(SenderAttemptObservation{})
	completion := factory.CompleteObservations(context.Background())
	if !completion.Attempts.Drained || completion.Attempts.Loss.ObserverPanic != 1 ||
		!completion.Diagnostics.Drained || completion.Diagnostics.Delivered != 1 {
		t.Fatalf("factory completion = %+v", completion)
	}
	select {
	case diagnostic := <-diagnostics:
		if diagnostic.Category != PeerDiagnosticSenderAttempt ||
			diagnostic.Reason != PeerDiagnosticObserverPanic || diagnostic.Count != 1 {
			t.Fatalf("diagnostic = %+v", diagnostic)
		}
	default:
		t.Fatal("panic diagnostic was not delivered before completion")
	}
}

func TestReceiverFactoryCompletionDrainsTerminationAndDiagnostics(t *testing.T) {
	diagnostics := make(chan PeerDiagnosticObservation, 2)
	factory, err := NewReceiverFactory(ReceiverFactoryConfig{
		OnTerminationContext: func(context.Context, ReceiverTerminationTrace) { panic("observer defect") },
		DiagnosticObserver: PeerDiagnosticContextObserverFunc(func(_ context.Context, observation PeerDiagnosticObservation) {
			diagnostics <- observation
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	factory.terminations.publish(ReceiverTerminationTrace{})
	completion := factory.CompleteObservations(context.Background())
	if !completion.Terminations.Drained || completion.Terminations.Loss.ObserverPanic != 1 ||
		!completion.Diagnostics.Drained || completion.Diagnostics.Delivered != 1 {
		t.Fatalf("factory completion = %+v", completion)
	}
	select {
	case diagnostic := <-diagnostics:
		if diagnostic.Category != PeerDiagnosticReceiverTermination ||
			diagnostic.Reason != PeerDiagnosticObserverPanic || diagnostic.Count != 1 {
			t.Fatalf("diagnostic = %+v", diagnostic)
		}
	default:
		t.Fatal("termination diagnostic was not delivered before completion")
	}
}
