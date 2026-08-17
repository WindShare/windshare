package v2peer

import (
	"context"
	"sync"
	"time"
)

const peerObservationCallbackLimit = 25 * time.Millisecond

type observationCallbackOutcome uint8

const (
	observationCallbackDelivered observationCallbackOutcome = iota + 1
	observationCallbackPanicked
	observationCallbackTimedOut
	observationCallbackAbandoned
)

type observationDispatcher[T any] struct {
	mu             sync.Mutex
	wake           *sync.Cond
	queue          []T
	capacity       int
	observe        func(T)
	observeContext func(context.Context, T)
	onPanic        func()
	onCapacity     func()
	closing        bool
	detached       bool
	callbackLive   bool
	drained        bool
	delivered      uint64
	loss           ObservationLoss
	callbackLimit  time.Duration
	detach         chan struct{}
	detachOnce     sync.Once
	done           chan struct{}
}

func newObservationDispatcher[T any](
	capacity int,
	observe func(T),
	onPanic func(),
	onCapacity func(),
) *observationDispatcher[T] {
	if capacity <= 0 || observe == nil {
		return nil
	}
	dispatcher := &observationDispatcher[T]{
		queue: make([]T, 0, capacity), capacity: capacity, observe: observe,
		onPanic: onPanic, onCapacity: onCapacity,
		drained: true, callbackLimit: peerObservationCallbackLimit,
		detach: make(chan struct{}), done: make(chan struct{}),
	}
	dispatcher.wake = sync.NewCond(&dispatcher.mu)
	go dispatcher.run()
	return dispatcher
}

func (dispatcher *observationDispatcher[T]) publish(value T) {
	if dispatcher == nil {
		return
	}
	dispatcher.mu.Lock()
	if dispatcher.closing || dispatcher.detached {
		dispatcher.loss.Undrained = saturatingObservationCount(dispatcher.loss.Undrained, 1)
		dispatcher.mu.Unlock()
		return
	}
	if len(dispatcher.queue) == dispatcher.capacity {
		dispatcher.loss.Capacity = saturatingObservationCount(dispatcher.loss.Capacity, 1)
		dispatcher.mu.Unlock()
		if dispatcher.onCapacity != nil {
			dispatcher.onCapacity()
		}
		return
	}
	dispatcher.queue = append(dispatcher.queue, value)
	dispatcher.wake.Signal()
	dispatcher.mu.Unlock()
}

func (dispatcher *observationDispatcher[T]) run() {
	defer close(dispatcher.done)
	for {
		dispatcher.mu.Lock()
		for len(dispatcher.queue) == 0 && !dispatcher.closing && !dispatcher.detached {
			dispatcher.wake.Wait()
		}
		if dispatcher.detached || (len(dispatcher.queue) == 0 && dispatcher.closing) {
			dispatcher.mu.Unlock()
			return
		}
		value := dispatcher.queue[0]
		var zero T
		dispatcher.queue[0] = zero
		dispatcher.queue = dispatcher.queue[1:]
		dispatcher.callbackLive = true
		dispatcher.mu.Unlock()

		outcome := dispatcher.invoke(value)
		dispatcher.mu.Lock()
		dispatcher.callbackLive = false
		if dispatcher.detached || outcome == observationCallbackAbandoned {
			dispatcher.mu.Unlock()
			return
		}
		panicCallback := false
		switch outcome {
		case observationCallbackDelivered:
			dispatcher.delivered = saturatingObservationCount(dispatcher.delivered, 1)
		case observationCallbackPanicked:
			dispatcher.loss.ObserverPanic = saturatingObservationCount(dispatcher.loss.ObserverPanic, 1)
			panicCallback = true
		case observationCallbackTimedOut:
			dispatcher.loss.CallbackTimeout = saturatingObservationCount(dispatcher.loss.CallbackTimeout, 1)
			dispatcher.detachRemainingLocked()
		}
		dispatcher.mu.Unlock()
		if panicCallback && dispatcher.onPanic != nil {
			dispatcher.onPanic()
		}
		if outcome == observationCallbackTimedOut {
			return
		}
	}
}

func (dispatcher *observationDispatcher[T]) invoke(value T) observationCallbackOutcome {
	result := make(chan observationCallbackOutcome, 1)
	callbackContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		outcome := observationCallbackDelivered
		defer func() {
			if recover() != nil {
				outcome = observationCallbackPanicked
			}
			result <- outcome
		}()
		if dispatcher.observeContext != nil {
			dispatcher.observeContext(callbackContext, value)
			return
		}
		dispatcher.observe(value)
	}()
	timer := time.NewTimer(dispatcher.callbackLimit)
	defer timer.Stop()
	select {
	case outcome := <-result:
		return outcome
	case <-timer.C:
		cancel()
		return observationCallbackTimedOut
	case <-dispatcher.detach:
		cancel()
		return observationCallbackAbandoned
	}
}

func (dispatcher *observationDispatcher[T]) detachRemainingLocked() {
	dispatcher.loss.Undrained = saturatingObservationCount(
		dispatcher.loss.Undrained, uint64(len(dispatcher.queue)),
	)
	clear(dispatcher.queue)
	dispatcher.queue = nil
	dispatcher.detached = true
	dispatcher.drained = false
	dispatcher.detachOnce.Do(func() { close(dispatcher.detach) })
}

func (dispatcher *observationDispatcher[T]) complete(ctx context.Context) ObservationCompletion {
	if dispatcher == nil {
		return ObservationCompletion{Drained: true}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	dispatcher.mu.Lock()
	if !dispatcher.closing {
		dispatcher.closing = true
		dispatcher.wake.Broadcast()
	}
	dispatcher.mu.Unlock()
	select {
	case <-dispatcher.done:
	default:
		select {
		case <-dispatcher.done:
		case <-ctx.Done():
			dispatcher.mu.Lock()
			if !dispatcher.detached {
				undrained := uint64(len(dispatcher.queue))
				if dispatcher.callbackLive {
					undrained = saturatingObservationCount(undrained, 1)
				}
				dispatcher.loss.Undrained = saturatingObservationCount(dispatcher.loss.Undrained, undrained)
				clear(dispatcher.queue)
				dispatcher.queue = nil
				dispatcher.detached = true
				dispatcher.drained = false
				dispatcher.detachOnce.Do(func() { close(dispatcher.detach) })
				dispatcher.wake.Broadcast()
			}
			dispatcher.mu.Unlock()
			<-dispatcher.done
		}
	}
	dispatcher.mu.Lock()
	completion := ObservationCompletion{
		Delivered: dispatcher.delivered, Loss: dispatcher.loss, Drained: dispatcher.drained,
	}
	dispatcher.mu.Unlock()
	return completion
}
