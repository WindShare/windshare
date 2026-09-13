package relayset

import (
	"context"
	"sync"
	"testing"
)

type waitingRegistration struct {
	*endpoint
	ready    chan struct{}
	observed chan struct{}
	mu       sync.Mutex
	observer func(bool)
}

func (e *waitingRegistration) WaitReady(ctx context.Context) error {
	select {
	case <-e.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (e *waitingRegistration) SetAvailabilityObserver(observer func(bool)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.observer = observer
	observer(false)
	close(e.observed)
}
func (e *waitingRegistration) publish(available bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.observer(available)
}

func TestSenderStartupWaitsForConfirmationAndAggregatesCurrentAvailability(t *testing.T) {
	created := make(chan *waitingRegistration, 2)
	set, err := NewSender(t.Context(), []string{"one", "two"}, func(context.Context, string) (SenderEndpoint, error) {
		endpoint := &waitingRegistration{endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})}
		created <- endpoint
		return endpoint, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupSet(t, set)
	first, second := waitValue(t, created), waitValue(t, created)
	changes := make(chan SenderAvailability, 16)
	set.ObserveAvailability(func(state SenderAvailability) { changes <- state })
	if got := waitValue(t, changes); got.Available != 0 || got.EverReady || got.Total != 2 {
		t.Fatalf("initial=%+v", got)
	}
	ready := make(chan error, 1)
	go func() { ready <- set.WaitReady(t.Context()) }()
	select {
	case err := <-ready:
		t.Fatalf("lifecycle construction published readiness: %v", err)
	default:
	}
	// Ensure each registration worker has installed its observer before publishing.
	for _, endpoint := range []*waitingRegistration{first, second} {
		waitValue(t, endpoint.observed)
	}
	first.publish(true)
	close(first.ready)
	if err := waitValue(t, ready); err != nil {
		t.Fatal(err)
	}
	if got := waitValue(t, changes); got.Available != 1 || !got.EverReady {
		t.Fatalf("first ready=%+v", got)
	}
	second.publish(true)
	close(second.ready)
	if got := waitValue(t, changes); got.Available != 2 {
		t.Fatalf("both=%+v", got)
	}
	first.publish(false)
	if got := waitValue(t, changes); got.Available != 1 {
		t.Fatalf("one lost=%+v", got)
	}
	second.publish(false)
	if got := waitValue(t, changes); got.Available != 0 || !got.EverReady {
		t.Fatalf("all lost=%+v", got)
	}
	first.publish(true)
	if got := waitValue(t, changes); got.Available != 1 {
		t.Fatalf("recovered=%+v", got)
	}
	set.StopRecovery()
	first.publish(false)
	select {
	case state := <-changes:
		t.Fatalf("explicit stop emitted recovery state=%+v", state)
	default:
	}
}
