package relayset

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/liveshare"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

type receiverRetryBarrierClock struct {
	waiting chan struct{}
	release chan struct{}
}

func (clock *receiverRetryBarrierClock) Now() time.Time { return time.Unix(1, 0) }

func (clock *receiverRetryBarrierClock) Wait(ctx context.Context, _ time.Duration) error {
	close(clock.waiting)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-clock.release:
		return nil
	}
}

func TestReceiverDoesNotRedialSessionThatStoppedDuringBackoff(t *testing.T) {
	server := testReceiverRelay(t)
	sender := receiverTestShare(t, []string{server.URL})
	const unavailableEndpoint = "unavailable"
	capability := sender.Capability()
	capability.Relays = append(capability.Relays, unavailableEndpoint)
	clock := &receiverRetryBarrierClock{waiting: make(chan struct{}), release: make(chan struct{})}
	var unavailableAttempts atomic.Int32
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	set, err := NewReceiver(ctx, ReceiverConfig{
		Clock: clock, Receiver: liveshare.ReceiverConfig{Capability: capability},
		Dial: func(ctx context.Context, config relayv2.ReceiverConfig) (*relayv2.ReceiverConnection, error) {
			if config.RelayBaseURL != unavailableEndpoint {
				return relayv2.DialReceiver(ctx, config)
			}
			if unavailableAttempts.Add(1) > 1 {
				// A broken retry owner still terminates promptly so the assertion
				// diagnoses an extra attempt without another wait or a timeout.
				return nil, &relayv2.RelayError{Code: v2.ErrorStopped}
			}
			return nil, errors.New("endpoint temporarily unavailable")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()
	runtime, _, err := set.WaitReady(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-clock.waiting:
	case <-ctx.Done():
		t.Fatal("unavailable endpoint did not enter backoff")
	}
	// Keep the relay-set owner alive while its session ends, exactly as it can
	// while the continuation is still joining the old transfer generation.
	runtime.Close()
	close(clock.release)
	joined := make(chan struct{})
	go func() { set.workers.Wait(); close(joined) }()
	select {
	case <-joined:
	case <-ctx.Done():
		t.Fatal("old session endpoint workers did not stop")
	}
	if attempts := unavailableAttempts.Load(); attempts != 1 {
		t.Fatalf("retired session redialed after backoff: attempts=%d", attempts)
	}
}

func TestReceiverStopsInFlightDialAndDisposesLateConnection(t *testing.T) {
	server := testReceiverRelay(t)
	sender := receiverTestShare(t, []string{server.URL})
	const lateEndpoint = "late"
	capability := sender.Capability()
	capability.Relays = append(capability.Relays, lateEndpoint)
	late := make(chan *relayv2.ReceiverConnection, 1)
	dialCanceled := make(chan struct{})
	var connected atomic.Int32
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	set, err := NewReceiver(ctx, ReceiverConfig{
		Receiver: liveshare.ReceiverConfig{Capability: capability},
		Dial: func(ctx context.Context, config relayv2.ReceiverConfig) (*relayv2.ReceiverConnection, error) {
			if config.RelayBaseURL != lateEndpoint {
				return relayv2.DialReceiver(ctx, config)
			}
			config.RelayBaseURL = server.URL
			candidate, err := relayv2.DialReceiver(ctx, config)
			if err != nil {
				return nil, err
			}
			late <- candidate
			<-ctx.Done()
			close(dialCanceled)
			// A provider can win connection establishment while cancellation is
			// being delivered. Returning the owner transfers cleanup responsibility.
			return candidate, nil
		},
		Connected: func(*relayv2.ReceiverConnection) { connected.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()
	runtime, _, err := set.WaitReady(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var candidate *relayv2.ReceiverConnection
	select {
	case candidate = <-late:
	case <-ctx.Done():
		t.Fatal("secondary dial did not reach its result barrier")
	}
	runtime.BeginClose()
	select {
	case <-dialCanceled:
	case <-ctx.Done():
		t.Fatal("session stopping did not cancel its in-flight relay dial")
	}
	select {
	case <-candidate.Done():
	case <-ctx.Done():
		t.Fatal("late relay connection was not disposed")
	}
	set.Close()
	if connected.Load() != 1 || candidate.Channel().State() != framechannel.Closed {
		t.Fatalf("late candidate admitted or retained: connections=%d state=%v", connected.Load(), candidate.Channel().State())
	}
}
