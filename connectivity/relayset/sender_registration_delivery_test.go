package relayset

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/windshare/windshare/core/serviceauthority"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

func TestRegistrationDeliveryPreservesRecoveryMode(t *testing.T) {
	notSent := fmt.Errorf("dial wrapper: %w", &relayv2.RegistrationNotSentError{Cause: errors.New("upgrade refused")})
	missingRoute := &relayv2.RegistrationNotSentError{Cause: &relayv2.WebSocketRouteError{}}
	uncertain := errors.New("register acknowledgement lost")
	absent := &relayv2.RelayError{Code: v2.ErrorNotFound}
	for _, test := range []struct {
		name     string
		failures []error
		modes    []v2.RegistrationMode
	}{
		{"route missing after uncertain delivery", []error{uncertain, missingRoute}, []v2.RegistrationMode{v2.RegistrationFresh, v2.RegistrationResume, v2.RegistrationResume}},
		{"initial outage", []error{notSent}, []v2.RegistrationMode{v2.RegistrationFresh, v2.RegistrationFresh}},
		{"uncertain registration then outage", []error{uncertain, notSent}, []v2.RegistrationMode{v2.RegistrationFresh, v2.RegistrationResume, v2.RegistrationResume}},
		{"absent registration then outage", []error{uncertain, absent, notSent}, []v2.RegistrationMode{v2.RegistrationFresh, v2.RegistrationResume, v2.RegistrationFresh, v2.RegistrationFresh}},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempts := 0
			dialer := &senderRelayTestDialer{dial: func(context.Context, relayv2.SenderConfig) (SenderConnection, error) {
				attempts++
				if attempts <= len(test.failures) {
					return SenderConnection{}, test.failures[attempts-1]
				}
				return NewSenderConnection(&senderRelayTestEndpoint{}), nil
			}}
			lifecycle := newSenderRelayTestLifecycle(t, nil, dialer, newSenderRelayTestClock())
			t.Cleanup(lifecycle.StopRecovery)
			if err := lifecycle.WaitReady(t.Context()); err != nil {
				t.Fatal(err)
			}
			configs, _ := dialer.Snapshot()
			if len(configs) != len(test.modes) {
				t.Fatalf("dials = %d, want %d", len(configs), len(test.modes))
			}
			for index, config := range configs {
				if config.Init.Mode != test.modes[index] {
					t.Fatalf("attempt %d mode = %v, want %v", index, config.Init.Mode, test.modes[index])
				}
			}
		})
	}
}

func TestInitialMissingWebSocketRouteRetiresOnlyThisLifecycle(t *testing.T) {
	failure := &relayv2.RegistrationNotSentError{Cause: &relayv2.WebSocketRouteError{}}
	dialer := &senderRelayTestDialer{dial: func(context.Context, relayv2.SenderConfig) (SenderConnection, error) {
		return SenderConnection{}, failure
	}}
	clock := newSenderRelayTestClock()
	clock.wait = func(context.Context, time.Duration, <-chan struct{}) error {
		return errors.New("missing initial route retried")
	}
	lifecycle := newSenderRelayTestLifecycle(t, nil, dialer, clock)
	t.Cleanup(lifecycle.StopRecovery)
	if err := lifecycle.WaitReady(t.Context()); !errors.Is(err, failure) {
		t.Fatal(err)
	}
}

func TestAttributionAndCleanupUseDeliveryResultIndependentlyOfObservations(t *testing.T) {
	for _, notSent := range []bool{true, false} {
		t.Run(fmt.Sprintf("notSent=%t", notSent), func(t *testing.T) {
			ctx, stop := context.WithCancel(t.Context())
			defer stop()
			dialer := &senderRelayTestDialer{dial: func(_ context.Context, config relayv2.SenderConfig) (SenderConnection, error) {
				// Cancellation can replace the returned error, but the raw delivery
				// result still owns attribution and cleanup.
				stop()
				failure := errors.New("connection lost")
				if notSent {
					// Optional diagnostic events are deliberately absent.
					return SenderConnection{}, &relayv2.RegistrationNotSentError{Cause: failure}
				}
				// A wrapper may report a failed internal dial before another attempt
				// leaves registration delivery uncertain. Its final result owns policy.
				config.Dial.ObserveStage(relayv2.DialStage{Stage: relayv2.DialStageWebSocket, Failure: failure})
				return SenderConnection{}, failure
			}}
			var deliveries []serviceauthority.RegistrationDelivery
			config := newSenderRelayTestConfig(t, nil, dialer, nil)
			config.Candidate.Authorization = authorizationFunc(func() Authorization {
				return Authorization{State: AuthorizationReady, Reservation: func(delivery serviceauthority.RegistrationDelivery) {
					deliveries = append(deliveries, delivery)
				}}
			})
			lifecycle, err := NewSenderLifecycle(config)
			if err != nil {
				t.Fatal(err)
			}
			defer lifecycle.StopRecovery()
			if err := lifecycle.WaitReady(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("startup wait = %v", err)
			}
			if len(deliveries) != 1 || (deliveries[0] == serviceauthority.RegistrationNotSent) != notSent {
				t.Fatalf("registration delivery = %v, want unsent=%t", deliveries, notSent)
			}
			err = lifecycle.Cleanup(ctx)
			if notSent && err != nil || !notSent && !errors.Is(err, context.Canceled) {
				t.Fatalf("cleanup = %v, notSent=%t", err, notSent)
			}
		})
	}
}
