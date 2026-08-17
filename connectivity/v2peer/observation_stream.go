package v2peer

import (
	"sync"

	"github.com/windshare/windshare/core/observationstream"
)

type observationPublishResult uint8

const (
	observationPublishEnqueued observationPublishResult = iota + 1
	observationPublishCapacityDropped
	observationPublishAfterCompletion
)

// observationSource adds domain lifecycle ownership to the generic stream. The
// extra lock distinguishes bounded loss from a publication attempted after the
// owner's completion cut without executing consumer work.
type observationSource[T any] struct {
	mu       sync.Mutex
	producer observationstream.Producer[T]
	consumer observationstream.Consumer[T]
	complete bool
	cut      ObservationCompletion
}

func newObservationSource[T any](capacity int) (*observationSource[T], error) {
	if capacity == 0 {
		return nil, nil
	}
	producer, consumer, err := observationstream.New[T](observationstream.Capacity(capacity))
	if err != nil {
		return nil, err
	}
	return &observationSource[T]{producer: producer, consumer: consumer}, nil
}

func (source *observationSource[T]) observations() <-chan T {
	if source == nil {
		return nil
	}
	return source.consumer
}

func (source *observationSource[T]) publish(value T) observationPublishResult {
	if source == nil {
		return observationPublishAfterCompletion
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.complete {
		return observationPublishAfterCompletion
	}
	if source.producer.TryPublish(value) {
		return observationPublishEnqueued
	}
	return observationPublishCapacityDropped
}

func (source *observationSource[T]) completeObservations() ObservationCompletion {
	if source == nil {
		return ObservationCompletion{}
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.complete {
		return source.cut
	}
	source.complete = true
	cut := source.producer.Complete()
	source.cut = ObservationCompletion{
		Enqueued: cut.Enqueued,
		Loss:     ObservationLoss{CapacityDropped: cut.CapacityDropped},
	}
	return source.cut
}
