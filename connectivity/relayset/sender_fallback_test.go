package relayset

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/transport/relayv2"
)

func TestCanaryStartsAfterEveryActiveFailsAndKeepsPendingRoutes(t *testing.T) {
	var slogOutput bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&slogOutput, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	primary := &waitingRegistration{endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})}
	reject := make(chan struct{})
	plan := relayplan.Plan{Network: []relayplan.Candidate{{URL: "retrying"}, {URL: "rejected"}}, Standby: []relayplan.Candidate{{URL: "standby", Canary: true}}}
	standby := make(chan struct{})
	var owner atomic.Pointer[Sender]
	fallbacks := make(chan Fallback, 2)
	set, err := NewPlannedSender(t.Context(), plan, func(ctx context.Context, url string) (SenderEndpoint, error) {
		switch url {
		case "retrying":
			return primary, nil
		case "rejected":
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-reject:
				return nil, errors.New("registration denied")
			}
		default:
			close(standby)
			return newEndpoint(), nil
		}
	}, func(event Fallback) {
		// Reentering Sender proves s.mu was released before notifying observers.
		if current := owner.Load(); current != nil {
			_ = current.Status()
			_ = current.Registrations()
		}
		fallbacks <- event
	})
	if err != nil {
		t.Fatal(err)
	}
	owner.Store(set)
	defer cleanupSet(t, set)
	waitValue(t, primary.observed)
	primary.publish(RegistrationRetrying)
	if got := set.Registrations().Candidates; !slices.Equal(got, []string{"rejected", "retrying"}) {
		t.Fatalf("premature fallback: %v", got)
	}
	close(reject)
	waitValue(t, standby)
	event := waitValue(t, fallbacks)
	if !slices.Equal(event.Active, []string{"retrying", "rejected"}) || !slices.Equal(event.Next, []string{"standby"}) {
		t.Fatalf("unexpected fallback event: %+v", event)
	}
	if err := set.WaitReady(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := set.Registrations()
	if set.ReadyRelayURL() != "standby" || !slices.Equal(got.Candidates, []string{"retrying", "standby"}) || !slices.Equal(got.Rejected, []string{"rejected"}) {
		t.Fatal(set.ReadyRelayURL(), got)
	}
	if slogOutput.Len() != 0 {
		t.Fatalf("fallback bypassed observer via slog: %s", slogOutput.String())
	}
}

func TestCanaryCannotStartAfterPublication(t *testing.T) {
	active := &waitingRegistration{endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})}
	plan := relayplan.Plan{Network: []relayplan.Candidate{{URL: "active"}}, Standby: []relayplan.Candidate{{URL: "dormant", Canary: true}}}
	set, err := NewPlannedSender(t.Context(), plan, func(_ context.Context, url string) (SenderEndpoint, error) {
		if url != "active" {
			t.Errorf("dialed dormant relay %s", url)
		}
		return active, nil
	}, func(event Fallback) {
		t.Errorf("unexpected fallback after publication: %+v", event)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupSet(t, set)
	waitValue(t, active.observed)
	active.publish(RegistrationAvailable)
	close(active.ready)
	if err := set.WaitReady(t.Context()); err != nil {
		t.Fatal(err)
	}
	active.publish(RegistrationRetrying)
	set.Wake()
	if got := set.Registrations().Candidates; !slices.Equal(got, []string{"active"}) {
		t.Fatal(got)
	}
}

func TestCanaryReusesLastLinkSlotWithoutExceedingLimit(t *testing.T) {
	plan := relayplan.Plan{Network: []relayplan.Candidate{{URL: "active"}}, Standby: []relayplan.Candidate{{URL: "canary", Canary: true}}}
	for i := range MaximumEndpoints - 1 {
		plan.Configured = append(plan.Configured, relayplan.Candidate{URL: fmt.Sprint(i)})
	}
	set, err := NewPlannedSender(t.Context(), plan, func(ctx context.Context, url string) (SenderEndpoint, error) {
		switch url {
		case "active":
			return nil, errors.New("refused")
		case "canary":
			return newEndpoint(), nil
		default:
			<-ctx.Done()
			return nil, ctx.Err()
		}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupSet(t, set)
	if err := set.WaitReady(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := set.Registrations().Candidates
	if len(got) != MaximumEndpoints || slices.Contains(got, "active") || !slices.Contains(got, "canary") {
		t.Fatal(got)
	}
}

type racedRegistrationEndpoint struct {
	*SenderLifecycle
	onAvailable func()
	waitDone    chan struct{}
}

func (e *racedRegistrationEndpoint) ObserveRegistration(observer func(RegistrationState)) {
	e.SenderLifecycle.ObserveRegistration(func(state RegistrationState) {
		if state == RegistrationAvailable && e.onAvailable != nil {
			e.onAvailable()
		}
		observer(state)
	})
}

func (e *racedRegistrationEndpoint) WaitReady(ctx context.Context) error {
	defer close(e.waitDone)
	return e.SenderLifecycle.WaitReady(ctx)
}

func TestCanaryRetirementClosesActiveConnectionRacingRegistrationCallback(t *testing.T) {
	const (
		urlA1 = "https://a1.example"
		urlA2 = "https://a2.example"
		urlA3 = "https://a3.example"
		urlA4 = "https://a4.example"
		urlC1 = "https://c1.example"
		urlC2 = "https://c2.example"
	)
	plan := relayplan.Plan{
		Network: []relayplan.Candidate{
			{URL: urlA1, Identity: urlA1},
			{URL: urlA2, Identity: urlA2},
			{URL: urlA3, Identity: urlA3},
			{URL: urlA4, Identity: urlA4},
		},
		Standby: []relayplan.Candidate{
			{URL: urlC1, Identity: urlC1, Canary: true},
			{URL: urlC2, Identity: urlC2, Canary: true},
		},
	}
	clock := newSenderRelayTestClock()
	admissions := NewNetworkAdmissions(plan, clock, nil)

	a1 := &waitingRegistration{endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})}
	a2 := &waitingRegistration{endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})}
	a4 := &waitingRegistration{endpoint: newEndpoint(), ready: make(chan struct{}), observed: make(chan struct{})}

	a3Retrying := make(chan struct{})
	startA3Retry := make(chan struct{})
	clock.wait = func(ctx context.Context, _ time.Duration, _ <-chan struct{}) error {
		close(a3Retrying)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-startA3Retry:
			return nil
		}
	}

	connA3 := &senderRelayTestEndpoint{
		accept: func(context.Context) (*relayv2.Channel, error) {
			return nil, errors.New("retired connection served accept")
		},
	}
	var a3Attempts int
	configA3 := newSenderRelayTestConfig(t, nil, &senderRelayTestDialer{
		dial: func(context.Context, relayv2.SenderConfig) (SenderConnection, error) {
			a3Attempts++
			if a3Attempts == 1 {
				return SenderConnection{}, errors.New("transient startup failure")
			}
			return NewSenderConnection(connA3), nil
		},
	}, clock)
	configA3.Candidate.PublicURL = urlA3
	configA3.Attempts = admissions.Control(urlA3)
	lifecycleA3, err := NewSenderLifecycle(configA3)
	if err != nil {
		t.Fatal(err)
	}

	var retireOnce sync.Once
	racedA3 := &racedRegistrationEndpoint{
		SenderLifecycle: lifecycleA3,
		waitDone:        make(chan struct{}),
		onAvailable: func() {
			// Simulate A4 acquiring Sender.mu and retiring A3 right before A3's
			// RegistrationAvailable callback enters Sender.setRegistration.
			retireOnce.Do(func() { a4.publish(RegistrationRetrying) })
		},
	}

	canaryStarted := make(chan struct{})
	var canaryOnce sync.Once
	set, err := NewPlannedSender(t.Context(), plan, func(_ context.Context, url string) (SenderEndpoint, error) {
		switch url {
		case urlA1:
			return a1, nil
		case urlA2:
			return a2, nil
		case urlA3:
			return racedA3, nil
		case urlA4:
			return a4, nil
		case urlC1, urlC2:
			canaryOnce.Do(func() { close(canaryStarted) })
			return newEndpoint(), nil
		default:
			return nil, fmt.Errorf("unexpected relay %s", url)
		}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		set.StopRecovery()
		set.workers.Wait()
	}()

	waitValue(t, a1.observed)
	waitValue(t, a2.observed)
	waitValue(t, a4.observed)
	waitValue(t, a3Retrying)

	a1.publish(RegistrationRetrying)
	a2.publish(RegistrationRetrying)
	close(startA3Retry)

	waitValue(t, canaryStarted)
	waitValue(t, racedA3.waitDone)
	if err := set.WaitReady(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := connA3.closeCalls.Load(); got != 1 {
		t.Fatalf("retired active connection close calls = %d, want 1", got)
	}
	if got := connA3.acceptCalls.Load(); got != 0 {
		t.Fatalf("retired active connection accept calls = %d, want 0", got)
	}
	lifecycleA3.mu.Lock()
	installed, generation, reg := lifecycleA3.connection.valid(), lifecycleA3.generation, lifecycleA3.registration
	lifecycleA3.mu.Unlock()
	if installed || generation != 0 || reg != RegistrationPending {
		t.Fatalf("retired lifecycle retained state: installed=%v generation=%d registration=%v", installed, generation, reg)
	}
	admissions.mu.Lock()
	active, primary := admissions.active, admissions.primary
	admissions.mu.Unlock()
	if active != 0 || primary != -1 {
		t.Fatalf("retired lifecycle retained admission slot: active=%d primary=%d", active, primary)
	}
	got := set.Registrations().Candidates
	if slices.Contains(got, urlA3) || slices.Contains(got, urlA4) || !slices.Contains(got, urlC1) || !slices.Contains(got, urlC2) {
		t.Fatalf("unexpected surviving candidates: %v", got)
	}
}
