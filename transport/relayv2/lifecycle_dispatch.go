package relayv2

import (
	"context"
	"sync"
	"time"
)

const (
	relayLifecycleQueueCapacity = 256
	relayLifecycleCallbackLimit = 25 * time.Millisecond
)

type lifecycleCallbackOutcome uint8

const (
	lifecycleCallbackDelivered lifecycleCallbackOutcome = iota + 1
	lifecycleCallbackPanicked
	lifecycleCallbackTimedOut
	lifecycleCallbackAbandoned
)

type lifecycleDispatcher struct {
	linkID uint64
	tracer LifecycleTracer

	mu            sync.Mutex
	wake          *sync.Cond
	queue         []LifecycleTrace
	closing       bool
	detached      bool
	callbackLive  bool
	drained       bool
	delivered     uint64
	loss          LifecycleObservationLoss
	summarized    uint64
	callbackLimit time.Duration
	detach        chan struct{}
	detachOnce    sync.Once
	done          chan struct{}
}

func newLifecycleDispatcher(linkID uint64, tracer LifecycleTracer) *lifecycleDispatcher {
	if tracer == nil {
		return nil
	}
	dispatcher := &lifecycleDispatcher{
		linkID: linkID, tracer: tracer,
		queue:         make([]LifecycleTrace, 0, relayLifecycleQueueCapacity),
		drained:       true,
		callbackLimit: relayLifecycleCallbackLimit,
		detach:        make(chan struct{}),
		done:          make(chan struct{}),
	}
	dispatcher.wake = sync.NewCond(&dispatcher.mu)
	go dispatcher.run()
	return dispatcher
}

func (dispatcher *lifecycleDispatcher) emit(event LifecycleTrace) {
	if dispatcher == nil {
		return
	}
	dispatcher.mu.Lock()
	defer dispatcher.mu.Unlock()
	if dispatcher.closing || dispatcher.detached {
		dispatcher.loss.Undrained = saturatingLifecycleCount(dispatcher.loss.Undrained, 1)
		return
	}
	dispatcher.flushDropSummaryLocked()
	if len(dispatcher.queue) < relayLifecycleQueueCapacity {
		dispatcher.queue = append(dispatcher.queue, event)
	} else {
		dispatcher.loss.QueueOverflow = saturatingLifecycleCount(dispatcher.loss.QueueOverflow, 1)
	}
	dispatcher.wake.Signal()
}

func (dispatcher *lifecycleDispatcher) shutdown() {
	if dispatcher == nil {
		return
	}
	dispatcher.mu.Lock()
	if !dispatcher.closing {
		dispatcher.closing = true
		dispatcher.flushDropSummaryLocked()
		dispatcher.wake.Broadcast()
	}
	dispatcher.mu.Unlock()
}

func (dispatcher *lifecycleDispatcher) complete(ctx context.Context) LifecycleObservationCompletion {
	if dispatcher == nil {
		return LifecycleObservationCompletion{Drained: true}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	dispatcher.shutdown()
	select {
	case <-dispatcher.done:
	default:
		select {
		case <-dispatcher.done:
		case <-ctx.Done():
			dispatcher.mu.Lock()
			dispatcher.detachAtDeadlineLocked()
			dispatcher.mu.Unlock()
			<-dispatcher.done
		}
	}
	dispatcher.mu.Lock()
	completion := LifecycleObservationCompletion{
		Delivered: dispatcher.delivered,
		Loss:      dispatcher.loss,
		Drained:   dispatcher.drained,
	}
	dispatcher.mu.Unlock()
	return completion
}

func (dispatcher *lifecycleDispatcher) detachAtDeadlineLocked() {
	if dispatcher.detached {
		return
	}
	undrained := uint64(len(dispatcher.queue))
	if dispatcher.callbackLive {
		undrained = saturatingLifecycleCount(undrained, 1)
	}
	dispatcher.loss.Undrained = saturatingLifecycleCount(dispatcher.loss.Undrained, undrained)
	clear(dispatcher.queue)
	dispatcher.queue = nil
	dispatcher.detached = true
	dispatcher.drained = false
	dispatcher.detachOnce.Do(func() { close(dispatcher.detach) })
	dispatcher.wake.Broadcast()
}

func (dispatcher *lifecycleDispatcher) flushDropSummaryLocked() {
	// The lifecycle summary is safe evidence for queue omission only. Callback
	// defects retain their distinct cause in the typed completion vector; folding
	// them into Dropped would make consumers count the same defect twice.
	dropped := dispatcher.loss.QueueOverflow
	if dropped == 0 || dropped == dispatcher.summarized ||
		len(dispatcher.queue) >= relayLifecycleQueueCapacity || dispatcher.detached {
		return
	}
	dispatcher.queue = append(dispatcher.queue, traceDroppedSummary(dispatcher.linkID, dropped))
	dispatcher.summarized = dropped
}

func (dispatcher *lifecycleDispatcher) run() {
	defer close(dispatcher.done)
	for {
		dispatcher.mu.Lock()
		dispatcher.flushDropSummaryLocked()
		for len(dispatcher.queue) == 0 && !dispatcher.closing && !dispatcher.detached {
			dispatcher.wake.Wait()
			dispatcher.flushDropSummaryLocked()
		}
		if dispatcher.detached || (len(dispatcher.queue) == 0 && dispatcher.closing) {
			dispatcher.mu.Unlock()
			return
		}
		event := dispatcher.queue[0]
		dispatcher.queue[0] = LifecycleTrace{}
		dispatcher.queue = dispatcher.queue[1:]
		dispatcher.callbackLive = true
		dispatcher.mu.Unlock()

		outcome := dispatcher.invoke(event)
		dispatcher.mu.Lock()
		dispatcher.callbackLive = false
		if dispatcher.detached || outcome == lifecycleCallbackAbandoned {
			dispatcher.mu.Unlock()
			return
		}
		switch outcome {
		case lifecycleCallbackDelivered:
			dispatcher.delivered = saturatingLifecycleCount(dispatcher.delivered, 1)
		case lifecycleCallbackPanicked:
			dispatcher.loss.ObserverPanic = saturatingLifecycleCount(dispatcher.loss.ObserverPanic, 1)
		case lifecycleCallbackTimedOut:
			dispatcher.loss.CallbackTimeout = saturatingLifecycleCount(dispatcher.loss.CallbackTimeout, 1)
			dispatcher.loss.Undrained = saturatingLifecycleCount(
				dispatcher.loss.Undrained, uint64(len(dispatcher.queue)),
			)
			clear(dispatcher.queue)
			dispatcher.queue = nil
			dispatcher.detached = true
			dispatcher.drained = false
			dispatcher.detachOnce.Do(func() { close(dispatcher.detach) })
		}
		dispatcher.flushDropSummaryLocked()
		dispatcher.mu.Unlock()
		if outcome == lifecycleCallbackTimedOut {
			return
		}
	}
}

func (dispatcher *lifecycleDispatcher) invoke(event LifecycleTrace) lifecycleCallbackOutcome {
	result := make(chan lifecycleCallbackOutcome, 1)
	callbackContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		outcome := lifecycleCallbackDelivered
		defer func() {
			if recover() != nil {
				outcome = lifecycleCallbackPanicked
			}
			result <- outcome
		}()
		if contextual, ok := dispatcher.tracer.(LifecycleContextTracer); ok {
			contextual.TraceRelayLifecycleContext(callbackContext, event)
			return
		}
		dispatcher.tracer.TraceRelayLifecycle(event)
	}()
	timer := time.NewTimer(dispatcher.callbackLimit)
	defer timer.Stop()
	select {
	case outcome := <-result:
		return outcome
	case <-timer.C:
		cancel()
		return lifecycleCallbackTimedOut
	case <-dispatcher.detach:
		cancel()
		return lifecycleCallbackAbandoned
	}
}
