package senderrelay

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

func TestInitialPublicationUncertaintyKeepsOriginalCredentialsAndDescriptor(t *testing.T) {
	for _, firstFailure := range []error{errors.New("register acknowledgement lost"), &relayv2.RelayError{Code: v2.ErrorAlreadyRegistered}} {
		t.Run(firstFailure.Error(), func(t *testing.T) {
			endpoint := &senderRelayTestEndpoint{accept: func(context.Context) (*relayv2.Channel, error) { return nil, nil }}
			calls := 0
			dialer := &senderRelayTestDialer{dial: func(context.Context, relayv2.SenderConfig) (Connection, error) {
				calls++
				switch calls {
				case 1:
					return Connection{}, firstFailure
				case 2:
					return Connection{}, &relayv2.RelayError{Code: v2.ErrorNotFound}
				default:
					return NewConnection(endpoint), nil
				}
			}}
			config := newSenderRelayTestConfig(t, nil, dialer, newSenderRelayTestClock())
			descriptor := bytes.Clone(config.Descriptor)
			lifecycle, err := New(config)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(lifecycle.StopRecovery)
			config.Descriptor[0] ^= 0xff
			if calls != 0 {
				t.Fatal("constructor started dialing before owning registration")
			}
			if err := lifecycle.WaitReady(t.Context()); err != nil {
				t.Fatal(err)
			}
			configs, _ := dialer.Snapshot()
			if len(configs) != 3 {
				t.Fatalf("dials=%d", len(configs))
			}
			if configs[0].Init != config.Fresh || configs[1].Init != lifecycle.resume || configs[2].Init != config.Fresh {
				t.Fatal("uncertain publication did not resume before republishing")
			}
			if !bytes.Equal(configs[0].Descriptor, descriptor) || !bytes.Equal(configs[2].Descriptor, descriptor) || len(configs[1].Descriptor) != 0 {
				t.Fatal("republish changed descriptor bytes")
			}
			for _, attempt := range configs {
				if attempt.ResumeToken != config.ResumeToken || !bytes.Equal(attempt.SenderPrivateKey, config.PrivateKey) {
					t.Fatal("recovery authority changed")
				}
			}
		})
	}
}

func TestAuthenticatedRejectionsStopWithoutRepublishing(t *testing.T) {
	for _, code := range []v2.ErrorCode{v2.ErrorInvalidProof, v2.ErrorShareIDCollision, v2.ErrorDescriptorInvalid, v2.ErrorStopped, v2.ErrorMalformed, v2.ErrorUnsupportedMode} {
		t.Run((&relayv2.RelayError{Code: code}).Error(), func(t *testing.T) {
			failure := &relayv2.RelayError{Code: code}
			dialer := &senderRelayTestDialer{dial: func(context.Context, relayv2.SenderConfig) (Connection, error) { return Connection{}, failure }}
			lifecycle, err := New(newSenderRelayTestConfig(t, nil, dialer, newSenderRelayTestClock()))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(lifecycle.StopRecovery)
			for range 2 {
				if err := lifecycle.WaitReady(t.Context()); !errors.Is(err, failure) {
					t.Fatalf("terminal=%v", err)
				}
			}
			configs, _ := dialer.Snapshot()
			if len(configs) != 1 {
				t.Fatalf("terminal rejection caused %d attempts", len(configs))
			}
		})
	}
}

func TestLifecycleRejectsChangedPublicationMaterialBeforeDial(t *testing.T) {
	config := newSenderRelayTestConfig(t, nil, nil, nil)
	for _, corrupt := range []func(*Config){
		func(config *Config) { config.Descriptor = nil },
		func(config *Config) { config.ResumeToken[0] ^= 0xff },
		func(config *Config) { config.PrivateKey = nil },
		func(config *Config) { config.Fresh.Mode = v2.RegistrationResume },
	} {
		changed := config
		corrupt(&changed)
		if _, err := New(changed); !errors.Is(err, relayv2.ErrProtocol) {
			t.Fatalf("changed material accepted: %v", err)
		}
	}
	lifecycle, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := lifecycle.Cleanup(canceled); err != nil {
		t.Fatalf("unused registration attempted network cleanup: %v", err)
	}
}

func TestNetworkWakeUsesSingleRecoveryOwner(t *testing.T) {
	waiting, dialing, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	clock := newSenderRelayTestClock()
	clock.wait = func(ctx context.Context, delay time.Duration) error { close(waiting); <-ctx.Done(); return ctx.Err() }
	endpoint := &senderRelayTestEndpoint{accept: func(context.Context) (*relayv2.Channel, error) { return nil, nil }}
	calls := 0
	dialer := &senderRelayTestDialer{dial: func(context.Context, relayv2.SenderConfig) (Connection, error) {
		calls++
		if calls == 1 {
			return Connection{}, errors.New("offline")
		}
		close(dialing)
		<-release
		return NewConnection(endpoint), nil
	}}
	lifecycle, err := New(newSenderRelayTestConfig(t, nil, dialer, clock))
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.StopRecovery()
	results := make(chan error, 2)
	go func() { results <- lifecycle.WaitReady(t.Context()) }()
	senderRelayAwaitSignal(t, waiting, "retry wait")
	lifecycle.Wake()
	senderRelayAwaitSignal(t, dialing, "network wake dial")
	go func() { results <- lifecycle.WaitReady(t.Context()) }()
	lifecycle.Wake()
	close(release)
	for range 2 {
		if err := senderRelayAwaitError(t, results); err != nil {
			t.Fatal(err)
		}
	}
	configs, _ := dialer.Snapshot()
	if len(configs) != 2 {
		t.Fatalf("concurrent owners caused %d dials", len(configs))
	}
}

func TestCurrentAvailabilityClearsDuringRecoveryAndReturnsOnAcknowledgement(t *testing.T) {
	initial := &senderRelayTestEndpoint{accept: func(context.Context) (*relayv2.Channel, error) { return nil, relayv2.ErrClosed }}
	dialStarted, release := make(chan struct{}), make(chan struct{})
	endpoint := &senderRelayTestEndpoint{accept: func(context.Context) (*relayv2.Channel, error) { return new(relayv2.Channel), nil }}
	lifecycle := newSenderRelayTestLifecycle(t, initial, &senderRelayTestDialer{dial: func(context.Context, relayv2.SenderConfig) (Connection, error) {
		close(dialStarted)
		<-release
		return NewConnection(endpoint), nil
	}}, newSenderRelayTestClock())
	var mu sync.Mutex
	var states []bool
	lifecycle.SetAvailabilityObserver(func(available bool) { mu.Lock(); states = append(states, available); mu.Unlock() })
	done := make(chan error, 1)
	go func() { _, err := lifecycle.Accept(t.Context()); done <- err }()
	senderRelayAwaitSignal(t, dialStarted, "replacement dial")
	mu.Lock()
	if len(states) != 2 || !states[0] || states[1] {
		t.Fatalf("lost availability=%v", states)
	}
	mu.Unlock()
	close(release)
	if err := senderRelayAwaitError(t, done); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(states) != 3 || !states[2] {
		t.Fatalf("restored availability=%v", states)
	}
}
