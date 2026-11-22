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
	observer func(RegistrationState)
}

func (e *waitingRegistration) WaitReady(ctx context.Context) error {
	select {
	case <-e.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (e *waitingRegistration) ObserveRegistration(observer func(RegistrationState)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.observer = observer
	observer(RegistrationPending)
	close(e.observed)
}
func (e *waitingRegistration) publish(state RegistrationState) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.observer(state)
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
	changes := make(chan SenderStatus, 16)
	set.ObserveStatus(func(state SenderStatus) { changes <- state })
	if got := waitValue(t, changes); got.Availability.Available != 0 || got.Availability.EverReady || got.Availability.Total != 2 {
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
	first.publish(RegistrationNeedsUserAuthorization)
	if got := waitValue(t, changes); got.AuthorizationRequired {
		t.Fatalf("another candidate can still publish: %+v", got)
	}
	second.publish(RegistrationNeedsUserAuthorization)
	if got := waitValue(t, changes); !got.AuthorizationRequired {
		t.Fatalf("share should require authorization: %+v", got)
	}
	first.publish(RegistrationAvailable)
	close(first.ready)
	if err := waitValue(t, ready); err != nil {
		t.Fatal(err)
	}
	if got := waitValue(t, changes); got.Availability.Available != 1 || !got.Availability.EverReady || got.AuthorizationRequired {
		t.Fatalf("first ready=%+v", got)
	}
	second.publish(RegistrationAvailable)
	close(second.ready)
	if got := waitValue(t, changes); got.Availability.Available != 2 {
		t.Fatalf("both=%+v", got)
	}
	first.publish(RegistrationPending)
	if got := waitValue(t, changes); got.Availability.Available != 1 {
		t.Fatalf("one lost=%+v", got)
	}
	second.publish(RegistrationPending)
	if got := waitValue(t, changes); got.Availability.Available != 0 || !got.Availability.EverReady {
		t.Fatalf("all lost=%+v", got)
	}
	first.publish(RegistrationAvailable)
	if got := waitValue(t, changes); got.Availability.Available != 1 {
		t.Fatalf("recovered=%+v", got)
	}
	set.StopRecovery()
	first.publish(RegistrationPending)
	select {
	case state := <-changes:
		t.Fatalf("explicit stop emitted recovery state=%+v", state)
	default:
	}
}
