package senderrelay

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/transport/relayv2"
)

type authorizationFunc func() Authorization

func (source authorizationFunc) ReserveRegistrationAuthorization() Authorization { return source() }

func TestCandidateValidationDoesNotSelectDynamicAuthorization(t *testing.T) {
	config := newSenderRelayTestConfig(t, nil, nil, nil)
	config.Candidate.Authorization = authorizationFunc(func() Authorization {
		t.Fatal("candidate validation selected dynamic authorization")
		return Authorization{}
	})
	if _, err := PublicURLs([]Candidate{config.Candidate}); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle.StopRecovery()
}

type attemptControlFunc func(context.Context) (AttemptPermit, error)

func (control attemptControlFunc) Begin(ctx context.Context) (AttemptPermit, error) {
	return control(ctx)
}
func (attemptControlFunc) Unavailable() {}

func TestFreshRegistrationReadsTicketAfterWaitingForAnAttemptSlot(t *testing.T) {
	source := &changingAuthorization{
		value: Authorization{State: AuthorizationReady, Bearer: "old-ticket"}, changed: make(chan struct{}),
	}
	endpoint := &senderRelayTestEndpoint{accept: func(context.Context) (*relayv2.Channel, error) { return nil, nil }}
	dialer := &senderRelayTestDialer{dial: func(context.Context, relayv2.SenderConfig) (Connection, error) { return NewConnection(endpoint), nil }}
	config := newSenderRelayTestConfig(t, nil, dialer, nil)
	config.Candidate.Authorization = source
	queued, admit := make(chan struct{}), make(chan struct{})
	config.Attempts = attemptControlFunc(func(ctx context.Context) (AttemptPermit, error) {
		close(queued)
		select {
		case <-admit:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	lifecycle, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.StopRecovery()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- lifecycle.WaitReady(ctx) }()
	select {
	case <-queued:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	source.set(Authorization{State: AuthorizationReady, Bearer: "refreshed-ticket"})
	close(admit)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	calls, _ := dialer.Snapshot()
	if len(calls) != 1 || calls[0].Dial.Header.Get("Authorization") != "Bearer refreshed-ticket" {
		t.Fatal("first registration used a ticket from before the slot wait")
	}
}

type changingAuthorization struct {
	mu      sync.Mutex
	value   Authorization
	changed chan struct{}
}

func (source *changingAuthorization) ReserveRegistrationAuthorization() Authorization {
	source.mu.Lock()
	defer source.mu.Unlock()
	value := source.value
	value.Changed = source.changed
	return value
}
func (source *changingAuthorization) set(value Authorization) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.value = value
	close(source.changed)
	source.changed = make(chan struct{})
}

func TestAuthorizationCompletionWakesOriginalPublicationOutsideHandshake(t *testing.T) {
	for _, state := range []AuthorizationState{AuthorizationPendingSetup, AuthorizationExpired, AuthorizationTemporaryFailure, AuthorizationUserActionRequired} {
		source := &changingAuthorization{value: Authorization{State: state}, changed: make(chan struct{})}
		endpoint := &senderRelayTestEndpoint{accept: func(context.Context) (*relayv2.Channel, error) { return nil, nil }}
		dialer := &senderRelayTestDialer{dial: func(context.Context, relayv2.SenderConfig) (Connection, error) { return NewConnection(endpoint), nil }}
		config := newSenderRelayTestConfig(t, nil, dialer, nil)
		config.Candidate.Authorization = source
		waiting := make(chan Attempt, 1)
		config.ObserveAttempt = func(attempt Attempt) {
			if attempt.State == AttemptAuthorization {
				waiting <- attempt
			}
		}
		lifecycle, err := New(config)
		if err != nil {
			t.Fatal(err)
		}
		statuses := make(chan RegistrationState, 4)
		lifecycle.ObserveRegistration(func(state RegistrationState) { statuses <- state })
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		done := make(chan error, 1)
		go func() { done <- lifecycle.WaitReady(ctx) }()
		select {
		case attempt := <-waiting:
			if attempt.Authorization != state {
				t.Fatal(attempt)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if got := <-statuses; got != RegistrationPending {
			t.Fatal(got)
		}
		if state == AuthorizationUserActionRequired {
			select {
			case got := <-statuses:
				if got != RegistrationNeedsUserAuthorization {
					t.Fatal(got)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		calls, _ := dialer.Snapshot()
		if len(calls) != 0 {
			t.Fatal("pending credentials consumed a handshake")
		}
		source.set(Authorization{State: AuthorizationReady, Bearer: "cached-ticket"})
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		calls, _ = dialer.Snapshot()
		if len(calls) != 1 || calls[0].Init != config.Fresh ||
			!bytes.Equal(calls[0].Descriptor, config.Descriptor) ||
			calls[0].Dial.Header.Get("Authorization") != "Bearer cached-ticket" {
			t.Fatal("publication was recreated or lost authorization")
		}
		lifecycle.StopRecovery()
		cancel()
	}
}

func TestKnownAuthorizationDenialDoesNotDialOrFallBack(t *testing.T) {
	source := &changingAuthorization{value: Authorization{State: AuthorizationDenied, Cause: errors.New("account_blocked")}, changed: make(chan struct{})}
	dialer := &senderRelayTestDialer{dial: func(context.Context, relayv2.SenderConfig) (Connection, error) {
		t.Fatal("denied authorization dialed")
		return Connection{}, nil
	}}
	config := newSenderRelayTestConfig(t, nil, dialer, nil)
	config.Candidate.Authorization = source
	lifecycle, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.StopRecovery()
	var denied *AuthorizationError
	if err := lifecycle.WaitReady(t.Context()); !errors.As(err, &denied) || denied.State != AuthorizationDenied {
		t.Fatal(err)
	}
}
