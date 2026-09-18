package task

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/windshare/windshare/core/observationstream"
)

type Config[T any] struct {
	ID                  ID
	Now                 func() time.Time
	Random              io.Reader
	CleanupTimeout      time.Duration
	ObservationCapacity observationstream.Capacity
	Run                 func(context.Context, Control) Completion[T]
	Completed           func(error)
}

type Task[T any] struct {
	mu           sync.Mutex
	id           ID
	state        State
	reason       StopReason
	cancel       context.CancelCauseFunc
	done         chan struct{}
	producer     observationstream.Producer[Observation]
	observations observationstream.Consumer[Observation]
	now          func() time.Time
	result       Result[T]
}

func Start[T any](parent context.Context, config Config[T]) (*Task[T], error) {
	if parent == nil || config.ID == "" || config.Now == nil || config.Random == nil ||
		config.CleanupTimeout <= 0 || config.Run == nil {
		return nil, errors.New("task requires identity, context, dependencies and runner")
	}
	producer, observations, err := observationstream.New[Observation](config.ObservationCapacity)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancelCause(context.WithoutCancel(parent))
	started := config.Now()
	current := &Task[T]{
		id: config.ID, state: Running, cancel: cancel, done: make(chan struct{}),
		producer: producer, observations: observations, now: config.Now,
		result: Result[T]{StartedAt: started},
	}
	current.emit(Lifecycle{State: Running})
	stopParent := context.AfterFunc(parent, func() { current.Stop(Cancelled) })
	control := Control{
		ID: config.ID, Now: config.Now, Random: config.Random, Emit: current.emit,
		CleanupContext: func() (context.Context, context.CancelFunc) {
			// Cleanup keeps caller values for tracing but cannot inherit a deadline
			// or cancellation that caused the task to stop.
			return context.WithTimeout(context.WithoutCancel(parent), config.CleanupTimeout)
		},
	}
	go func() {
		completion := config.Run(ctx, control)
		completion.Settlement = completion.checked()
		stopParent()
		current.mu.Lock()
		current.state = Finished
		current.result.Completion = completion
		current.result.StopReason = current.reason
		current.result.FinishedAt = config.Now()
		current.emit(Lifecycle{State: Finished, StopReason: current.reason, Settlement: completion.Settlement})
		current.result.Observations = current.producer.Complete()
		current.mu.Unlock()
		cancel(nil)
		if config.Completed != nil {
			config.Completed(completion.CleanupError)
		}
		close(current.done)
	}()
	return current, nil
}

func (current *Task[T]) ID() ID { return current.id }

func (current *Task[T]) State() State {
	current.mu.Lock()
	defer current.mu.Unlock()
	return current.state
}

func (current *Task[T]) Done() <-chan struct{} { return current.done }

func (current *Task[T]) Observations() <-chan Observation { return current.observations }

// Cancel ends execution. Canceling a Wait context only detaches that waiter.
func (current *Task[T]) Cancel() { current.Stop(Cancelled) }

// Stop is kept inside the engine boundary so callers can only explicitly stop a
// share or cancel a task, while the application owns the close reason.
func (current *Task[T]) Stop(reason StopReason) {
	if reason.cause() == nil {
		return
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	if current.state != Running {
		return
	}
	current.state = Stopping
	current.reason = reason
	current.emit(Lifecycle{State: Stopping, StopReason: reason})
	current.cancel(reason.cause())
}

func (current *Task[T]) Wait(ctx context.Context) (Result[T], error) {
	// A completed result wins even when the caller canceled while it was being
	// published; this makes repeated queries for a settled task deterministic.
	select {
	case <-current.done:
		return current.finalResult(), nil
	default:
	}
	select {
	case <-current.done:
		return current.finalResult(), nil
	case <-ctx.Done():
		return Result[T]{}, ctx.Err()
	}
}

func (current *Task[T]) finalResult() Result[T] {
	current.mu.Lock()
	defer current.mu.Unlock()
	return current.result
}

func (current *Task[T]) emit(event Event) bool {
	if event == nil {
		return false
	}
	return current.producer.TryPublish(Observation{TaskID: current.id, At: current.now(), Event: event})
}
