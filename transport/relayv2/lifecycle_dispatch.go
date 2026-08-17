package relayv2

import (
	"sync"

	"github.com/windshare/windshare/core/observationstream"
)

// DefaultLifecycleObservationCapacity retains a bounded prefix until the owner
// attaches a consumer. Callers must opt in by assigning it explicitly.
const DefaultLifecycleObservationCapacity = 256

type lifecycleSource struct {
	linkID uint64

	mu              sync.Mutex
	producer        observationstream.Producer[LifecycleTrace]
	consumer        observationstream.Consumer[LifecycleTrace]
	capacityDropped uint64
	summarized      uint64
	completed       bool
	completion      LifecycleObservationCompletion
}

func newLifecycleSource(linkID uint64, capacity int) *lifecycleSource {
	if capacity == 0 {
		return nil
	}
	producer, consumer, err := observationstream.New[LifecycleTrace](
		observationstream.Capacity(capacity),
	)
	if err != nil {
		panic("relayv2: invalid internal lifecycle observation capacity")
	}
	return &lifecycleSource{linkID: linkID, producer: producer, consumer: consumer}
}

func (source *lifecycleSource) stream() <-chan LifecycleTrace {
	if source == nil {
		return nil
	}
	return source.consumer
}

func (source *lifecycleSource) emit(event LifecycleTrace) bool {
	if source == nil {
		return false
	}

	source.mu.Lock()
	defer source.mu.Unlock()
	if source.completed {
		return false
	}

	source.flushDropSummaryLocked()
	if !source.producer.TryPublish(event) {
		source.capacityDropped = saturatingLifecycleCount(source.capacityDropped, 1)
		return false
	}
	return true
}

func (source *lifecycleSource) flushDropSummaryLocked() {
	if source.capacityDropped == 0 || source.capacityDropped == source.summarized {
		return
	}

	// Synthetic evidence must not consume the accounting path used for omitted
	// domain facts. The source lock excludes another producer, while a consumer
	// can only create more room, so this capacity check makes TryPublish infallible.
	if len(source.consumer) >= cap(source.consumer) {
		return
	}
	dropped := source.capacityDropped
	if source.producer.TryPublish(traceDroppedSummary(source.linkID, dropped)) {
		source.summarized = dropped
	}
}

func (source *lifecycleSource) complete() LifecycleObservationCompletion {
	if source == nil {
		return LifecycleObservationCompletion{}
	}

	source.mu.Lock()
	defer source.mu.Unlock()
	if source.completed {
		return source.completion
	}

	// Completion gets one final bounded summary attempt. The producer cut remains
	// authoritative when the summary itself cannot be retained.
	source.flushDropSummaryLocked()
	return source.completeLocked()
}

func (source *lifecycleSource) completeWithFinal(event LifecycleTrace) LifecycleObservationCompletion {
	if source == nil {
		return LifecycleObservationCompletion{}
	}

	source.mu.Lock()
	defer source.mu.Unlock()
	if source.completed {
		return source.completion
	}

	// A pending gap precedes the final fact. If only one slot remains, retaining
	// the gap and accounting the omitted final fact preserves trace chronology.
	source.flushDropSummaryLocked()
	if !source.producer.TryPublish(event) {
		source.capacityDropped = saturatingLifecycleCount(source.capacityDropped, 1)
	}
	return source.completeLocked()
}

func (source *lifecycleSource) completeLocked() LifecycleObservationCompletion {
	cut := source.producer.Complete()
	source.completed = true
	source.completion = LifecycleObservationCompletion{
		Enqueued: cut.Enqueued,
		Loss: LifecycleObservationLoss{
			CapacityDropped: cut.CapacityDropped,
		},
	}
	return source.completion
}
