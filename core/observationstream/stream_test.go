package observationstream

import (
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

func TestNewRejectsNonPositiveCapacity(t *testing.T) {
	t.Parallel()

	for _, capacity := range []Capacity{0, -1} {
		t.Run(capacityName(capacity), func(t *testing.T) {
			t.Parallel()

			producer, consumer, err := New[int](capacity)
			if !errors.Is(err, ErrInvalidCapacity) {
				t.Fatalf("New(%d) error = %v, want %v", capacity, err, ErrInvalidCapacity)
			}
			if consumer != nil {
				t.Fatal("invalid construction returned a consumer channel")
			}
			if producer.TryPublish(1) {
				t.Fatal("invalid construction returned an active producer")
			}
			if cut := producer.Complete(); cut != (Completion{}) {
				t.Fatalf("inactive producer completion = %+v, want zero", cut)
			}
		})
	}
}

func TestTryPublishUsesExactCapacity(t *testing.T) {
	t.Parallel()

	producer, consumer := mustNew[int](t, 3)
	for observation := 1; observation <= 3; observation++ {
		if !producer.TryPublish(observation) {
			t.Fatalf("TryPublish(%d) = false before capacity was exhausted", observation)
		}
	}
	if producer.TryPublish(4) {
		t.Fatal("TryPublish accepted an observation beyond capacity")
	}

	if cut := producer.Complete(); cut != (Completion{Enqueued: 3, CapacityDropped: 1}) {
		t.Fatalf("Complete() = %+v, want 3 enqueued and 1 capacity drop", cut)
	}
	if got := drain(consumer); !reflect.DeepEqual(got, []int{1, 2, 3}) {
		t.Fatalf("consumer observations = %v, want [1 2 3]", got)
	}
}

func TestTryPublishDoesNotWaitForReaderWhenSaturated(t *testing.T) {
	t.Parallel()

	producer, _ := mustNew[int](t, 1)
	if !producer.TryPublish(1) {
		t.Fatal("first observation was not retained")
	}
	for observation := 2; observation <= 1_000; observation++ {
		if producer.TryPublish(observation) {
			t.Fatalf("saturated stream retained observation %d", observation)
		}
	}

	if cut := producer.Complete(); cut != (Completion{Enqueued: 1, CapacityDropped: 999}) {
		t.Fatalf("Complete() = %+v, want 1 enqueued and 999 capacity drops", cut)
	}
}

func TestConsumerReceivesFIFO(t *testing.T) {
	t.Parallel()

	producer, consumer := mustNew[string](t, 3)
	want := []string{"first", "second", "third"}
	for _, observation := range want {
		if !producer.TryPublish(observation) {
			t.Fatalf("TryPublish(%q) = false", observation)
		}
	}
	producer.Complete()

	if got := drain(consumer); !reflect.DeepEqual(got, want) {
		t.Fatalf("consumer observations = %v, want %v", got, want)
	}
}

func TestCompleteIsIdempotent(t *testing.T) {
	t.Parallel()

	producer, consumer := mustNew[int](t, 1)
	producer.TryPublish(1)
	first := producer.Complete()

	const callers = 16
	cuts := make(chan Completion, callers)
	var calls sync.WaitGroup
	calls.Add(callers)
	for range callers {
		go func() {
			defer calls.Done()
			cuts <- producer.Complete()
		}()
	}
	calls.Wait()
	close(cuts)

	for cut := range cuts {
		if cut != first {
			t.Fatalf("repeated Complete() = %+v, want cached %+v", cut, first)
		}
	}
	if got := drain(consumer); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("consumer observations = %v, want [1]", got)
	}
}

func TestTryPublishAfterCompleteIsRejectedWithoutChangingCut(t *testing.T) {
	t.Parallel()

	producer, _ := mustNew[int](t, 1)
	before := producer.Complete()
	if producer.TryPublish(1) {
		t.Fatal("TryPublish accepted an observation after completion")
	}
	if after := producer.Complete(); after != before {
		t.Fatalf("completion changed after rejected publication: before=%+v after=%+v", before, after)
	}
}

func TestConcurrentPublishersAndCompleteShareOneCut(t *testing.T) {
	t.Parallel()

	const (
		rounds     = 100
		publishers = 32
	)
	for round := range rounds {
		producer, consumer := mustNew[int](t, publishers)
		start := make(chan struct{})
		var enqueued atomic.Uint64
		var publishes sync.WaitGroup
		publishes.Add(publishers)
		for observation := range publishers {
			go func() {
				defer publishes.Done()
				<-start
				if producer.TryPublish(observation) {
					enqueued.Add(1)
				}
			}()
		}

		completed := make(chan Completion, 1)
		go func() {
			<-start
			completed <- producer.Complete()
		}()
		close(start)
		publishes.Wait()
		cut := <-completed

		if cut.Enqueued != enqueued.Load() {
			t.Fatalf("round %d completion enqueued = %d, successful publications = %d", round, cut.Enqueued, enqueued.Load())
		}
		if cut.CapacityDropped != 0 {
			t.Fatalf("round %d capacity drops = %d, want 0 with capacity for every publisher", round, cut.CapacityDropped)
		}
		if got := uint64(len(drain(consumer))); got != cut.Enqueued {
			t.Fatalf("round %d consumer retained = %d, completion enqueued = %d", round, got, cut.Enqueued)
		}
	}
}

func TestCompleteClosesConsumerChannel(t *testing.T) {
	t.Parallel()

	producer, consumer := mustNew[int](t, 1)
	producer.Complete()
	if observation, open := <-consumer; open {
		t.Fatalf("consumer remained open and returned %d", observation)
	}
}

func mustNew[T any](t *testing.T, capacity Capacity) (Producer[T], Consumer[T]) {
	t.Helper()
	producer, consumer, err := New[T](capacity)
	if err != nil {
		t.Fatalf("New(%d): %v", capacity, err)
	}
	return producer, consumer
}

func drain[T any](consumer Consumer[T]) []T {
	var observations []T
	for observation := range consumer {
		observations = append(observations, observation)
	}
	return observations
}

func capacityName(capacity Capacity) string {
	if capacity == 0 {
		return "zero"
	}
	return "negative"
}
