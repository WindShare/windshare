// Package observationstream provides a bounded, producer-owned observation
// queue without executing consumer code. Consumer panic, timeout, or detachment
// therefore cannot block producer completion; consumer cancellation and joining
// remain the responsibility of the layer that owns consumer execution.
package observationstream

import (
	"errors"
	"sync"
)

// Capacity names the maximum number of observations retained while the
// consumer is not receiving.
type Capacity int

// ErrInvalidCapacity is returned when a stream cannot retain an observation.
var ErrInvalidCapacity = errors.New("observation stream capacity must be positive")

// Completion is the producer-proven snapshot at the stream's admission cut.
// Enqueued does not imply that a consumer received or processed an observation.
type Completion struct {
	Enqueued        uint64
	CapacityDropped uint64
}

// Consumer is the receive-only capability for a stream. Its direction prevents
// consumers from publishing observations or closing producer-owned admission.
type Consumer[T any] <-chan T

// Producer owns publication and completion for one stream.
//
// Producer values may be copied; every copy shares the same lifecycle. The zero
// value is inactive: publication is rejected and completion returns a zero cut.
type Producer[T any] struct {
	state *state[T]
}

type state[T any] struct {
	mu                 sync.Mutex
	queue              chan T
	completed          bool
	enqueued           uint64
	capacityDropped    uint64
	completionSnapshot Completion
}

// New creates separate producer and consumer capabilities. Capacity must be
// positive so every active stream has an explicit bounded retention budget.
func New[T any](capacity Capacity) (Producer[T], Consumer[T], error) {
	if capacity <= 0 {
		return Producer[T]{}, nil, ErrInvalidCapacity
	}

	queue := make(chan T, int(capacity))
	return Producer[T]{state: &state[T]{queue: queue}}, Consumer[T](queue), nil
}

// TryPublish retains observation when admission is open and capacity remains.
// It never waits for the consumer. A false result means either the bounded
// capacity was exhausted before completion or publication began after the cut.
func (producer Producer[T]) TryPublish(observation T) bool {
	if producer.state == nil {
		return false
	}

	producer.state.mu.Lock()
	defer producer.state.mu.Unlock()

	if producer.state.completed {
		return false
	}

	select {
	case producer.state.queue <- observation:
		producer.state.enqueued = saturatingIncrement(producer.state.enqueued)
		return true
	default:
		producer.state.capacityDropped = saturatingIncrement(producer.state.capacityDropped)
		return false
	}
}

// Complete closes admission and the consumer channel at one serialized cut.
// Every call returns the same producer-proven snapshot. Owners must quiesce all
// possible publishers before treating that cut as their final lifecycle state.
func (producer Producer[T]) Complete() Completion {
	if producer.state == nil {
		return Completion{}
	}

	producer.state.mu.Lock()
	defer producer.state.mu.Unlock()

	if producer.state.completed {
		return producer.state.completionSnapshot
	}

	producer.state.completed = true
	producer.state.completionSnapshot = Completion{
		Enqueued:        producer.state.enqueued,
		CapacityDropped: producer.state.capacityDropped,
	}
	close(producer.state.queue)
	return producer.state.completionSnapshot
}

func saturatingIncrement(value uint64) uint64 {
	const maximumUint64 = ^uint64(0)
	if value == maximumUint64 {
		return maximumUint64
	}
	return value + 1
}
