package senderrelay

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

func TestRegistrationDeliveryPreservesRecoveryMode(t *testing.T) {
	notSent := fmt.Errorf("dial wrapper: %w", &relayv2.RegistrationNotSentError{Cause: errors.New("upgrade refused")})
	uncertain := errors.New("register acknowledgement lost")
	absent := &relayv2.RelayError{Code: v2.ErrorNotFound}
	for _, test := range []struct {
		name     string
		failures []error
		modes    []v2.RegistrationMode
	}{
		{"initial outage", []error{notSent}, []v2.RegistrationMode{v2.RegistrationFresh, v2.RegistrationFresh}},
		{"uncertain registration then outage", []error{uncertain, notSent}, []v2.RegistrationMode{v2.RegistrationFresh, v2.RegistrationResume, v2.RegistrationResume}},
		{"absent registration then outage", []error{uncertain, absent, notSent}, []v2.RegistrationMode{v2.RegistrationFresh, v2.RegistrationResume, v2.RegistrationFresh, v2.RegistrationFresh}},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempts := 0
			dialer := &senderRelayTestDialer{dial: func(context.Context, relayv2.SenderConfig) (Connection, error) {
				attempts++
				if attempts <= len(test.failures) {
					return Connection{}, test.failures[attempts-1]
				}
				return NewConnection(&senderRelayTestEndpoint{}), nil
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

func TestCleanupUsesDeliveryResultIndependentlyOfObservations(t *testing.T) {
	for _, notSent := range []bool{true, false} {
		t.Run(fmt.Sprintf("notSent=%t", notSent), func(t *testing.T) {
			ctx, stop := context.WithCancel(t.Context())
			defer stop()
			clock := newSenderRelayTestClock()
			clock.wait = func(context.Context, time.Duration) error { stop(); return context.Canceled }
			dialer := &senderRelayTestDialer{dial: func(_ context.Context, config relayv2.SenderConfig) (Connection, error) {
				failure := errors.New("connection lost")
				if notSent {
					// Optional diagnostic events are deliberately absent.
					return Connection{}, &relayv2.RegistrationNotSentError{Cause: failure}
				}
				// A wrapper may report a failed internal dial before another attempt
				// leaves registration delivery uncertain. Its final result owns policy.
				config.Dial.ObserveStage(relayv2.DialStage{Stage: relayv2.DialStageWebSocket, Failure: failure})
				return Connection{}, failure
			}}
			lifecycle := newSenderRelayTestLifecycle(t, nil, dialer, clock)
			if err := lifecycle.WaitReady(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("startup wait = %v", err)
			}
			err := lifecycle.Cleanup(ctx)
			if notSent && err != nil || !notSent && !errors.Is(err, context.Canceled) {
				t.Fatalf("cleanup = %v, notSent=%t", err, notSent)
			}
		})
	}
}
