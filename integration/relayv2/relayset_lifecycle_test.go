package relayv2_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/internal/testrun"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

type relaysetRetryBarrierClock struct {
	waiting chan context.Context
	release chan struct{}
}

func (*relaysetRetryBarrierClock) Now() time.Time { return time.Unix(1, 0) }

func (clock *relaysetRetryBarrierClock) Wait(ctx context.Context, _ time.Duration) error {
	select {
	case clock.waiting <- ctx:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-clock.release:
		return nil
	}
}

func TestReceiverDoesNotRedialSessionThatStoppedDuringBackoff(t *testing.T) {
	ctx, trace, urls := startRelaysetScenario(t, 1)
	share := prepareRelaysetShare(t, ctx, trace, urls)
	const unavailableEndpoint = "unavailable"
	capability := share.Capability()
	capability.Relays = append(capability.Relays, unavailableEndpoint)
	clock := &relaysetRetryBarrierClock{waiting: make(chan context.Context, 1), release: make(chan struct{})}
	var unavailableAttempts atomic.Int32
	set, err := relayset.NewReceiver(ctx, relayset.ReceiverConfig{
		Clock: clock, Receiver: liveshare.ReceiverConfig{Capacity: testReceiveCapacity(t), Capability: capability},
		Dial: func(ctx context.Context, config relayv2.ReceiverConfig) (relayset.ReceiverConnection, error) {
			if config.RelayBaseURL != unavailableEndpoint {
				return relayset.DialReceiver(ctx, config)
			}
			if unavailableAttempts.Add(1) > 1 {
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
	waitContext := waitRelaysetEvent(t, ctx, clock.waiting)
	// Ending the authenticated session must revoke retry authority even though
	// the caller has not closed the relay set or its own lifetime.
	runtime.Close()
	waitRelaysetEvent(t, ctx, waitContext.Done())
	if ctx.Err() != nil {
		t.Fatal("session shutdown consumed the caller lifetime")
	}
	close(clock.release)
	set.Close()
	if attempts := unavailableAttempts.Load(); attempts != 1 {
		t.Fatalf("retired session redialed after backoff: attempts=%d", attempts)
	}
	trace.RequireRecord(t, "session_backoff_retired", testrun.OutcomeSucceeded, nil)
	trace.RequireSuccess(t)
}

func TestReceiverStopsInFlightDialAndDisposesLateConnection(t *testing.T) {
	ctx, trace, urls := startRelaysetScenario(t, 1)
	share := prepareRelaysetShare(t, ctx, trace, urls)
	const lateEndpoint = "late"
	capability := share.Capability()
	capability.Relays = append(capability.Relays, lateEndpoint)
	late := make(chan relayset.ReceiverConnection, 1)
	dialCanceled := make(chan struct{})
	var connected atomic.Int32
	set, err := relayset.NewReceiver(ctx, relayset.ReceiverConfig{
		Receiver: liveshare.ReceiverConfig{Capacity: testReceiveCapacity(t), Capability: capability},
		Dial: func(ctx context.Context, config relayv2.ReceiverConfig) (relayset.ReceiverConnection, error) {
			if config.RelayBaseURL != lateEndpoint {
				return relayset.DialReceiver(ctx, config)
			}
			config.RelayBaseURL = urls[0]
			candidate, err := relayset.DialReceiver(ctx, config)
			if err != nil {
				return nil, err
			}
			late <- candidate
			<-ctx.Done()
			close(dialCanceled)
			// Establishment can win concurrently with cancellation; returning
			// the owner still transfers cleanup responsibility to the relay set.
			return candidate, nil
		},
		Connected: func(relayset.ReceiverConnection) { connected.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()
	runtime, _, err := set.WaitReady(ctx)
	if err != nil {
		t.Fatal(err)
	}
	candidate := waitRelaysetEvent(t, ctx, late)
	runtime.BeginClose()
	waitRelaysetEvent(t, ctx, dialCanceled)
	waitRelaysetEvent(t, ctx, candidate.Done())
	set.Close()
	if connected.Load() != 1 || candidate.Channel().State() != framechannel.Closed {
		t.Fatalf("late candidate admitted or retained: connections=%d state=%v", connected.Load(), candidate.Channel().State())
	}
	trace.RequireRecord(t, "late_candidate_disposed", testrun.OutcomeSucceeded, nil)
	trace.RequireSuccess(t)
}
