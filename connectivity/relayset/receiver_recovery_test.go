package relayset

import (
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/protocolsession"

	"github.com/windshare/windshare/core/session"
	receiversession "github.com/windshare/windshare/core/session/receiversession"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

func receiverUnavailableConfig(endpoints ...string) ReceiverConfig {
	raw := make([]byte, v2.ShareIDBytes)
	raw[0] = 1
	return ReceiverConfig{Receiver: liveshare.ReceiverConfig{
		Capability: link.Link{ShareID: base64.RawURLEncoding.EncodeToString(raw), Relays: endpoints},
	}, Dial: func(context.Context, relayv2.ReceiverConfig) (ReceiverConnection, error) {
		return nil, &relayv2.RelayError{Code: v2.ErrorNotFound}
	}}
}

func TestReceiverRecoveryLongOutagePreservesEndpointDecisions(t *testing.T) {
	const outageWindow = 3 * time.Minute
	clock := &receiverFakeClock{now: time.Unix(1, 0)}
	var stoppedCalls, availableCalls atomic.Int32
	var mu sync.Mutex
	var observations []ReceiverRecoveryObservation
	recovery, err := NewReceiverRecovery(ReceiverRecoveryOptions{
		Clock: clock, WaitTimeout: outageWindow,
		Jitter: func(delay time.Duration) time.Duration { return delay },
		Observe: func(value ReceiverRecoveryObservation) {
			mu.Lock()
			defer mu.Unlock()
			observations = append(observations, value)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	config := receiverUnavailableConfig("available", "stopped")
	config.Dial = func(_ context.Context, config relayv2.ReceiverConfig) (ReceiverConnection, error) {
		if config.RelayBaseURL == "stopped" {
			stoppedCalls.Add(1)
			return nil, &relayv2.RelayError{Code: v2.ErrorStopped}
		}
		availableCalls.Add(1)
		return nil, &relayv2.RelayError{Code: v2.ErrorNotFound}
	}
	// Expiring two caller windows exercises replacement decisions without a
	// successful network handshake: temporary failures retain retry authority.
	for range 2 {
		before := availableCalls.Load()
		started := clock.Now()
		set, err := recovery.Replace(t.Context(), config)
		if set != nil || !errors.Is(err, ErrReceiverWaitExpired) {
			t.Fatal(set, err)
		}
		if clock.Now().Sub(started) != outageWindow || availableCalls.Load()-before < 4 || stoppedCalls.Load() != 1 {
			t.Fatalf("elapsed=%s available=%d stopped=%d", clock.Now().Sub(started), availableCalls.Load(), stoppedCalls.Load())
		}
		eligible, err := recovery.endpoints(config.Receiver.Capability.Relays)
		if err != nil || len(eligible) != 1 || eligible[0] != "available" {
			t.Fatal(eligible, err)
		}
	}
	if !slices.Contains(clock.waits, receiverSlowRetryDelay) {
		t.Fatal("fast recovery never transitioned to slow waiting", clock.waits)
	}
	mu.Lock()
	defer mu.Unlock()
	var waiting bool
	var previousAttempt uint32
	for _, value := range observations {
		if value.Phase == ReceiverRecoveryWaiting && value.Endpoint == "available" {
			waiting = true
		}
		if value.Phase == ReceiverRecoveryConnecting && value.Endpoint == "available" {
			if value.Attempt != previousAttempt+1 {
				t.Fatalf("attempt history reset across caller windows: previous=%d next=%d", previousAttempt, value.Attempt)
			}
			previousAttempt = value.Attempt
		}
		if value.Phase == ReceiverRecoveryWaiting && value.Endpoint == "stopped" {
			t.Fatal("permanently stopped relay entered retry waiting")
		}
	}
	if !waiting {
		t.Fatal("no waiting observation")
	}
}

func TestReceiverRecoveryWaitLimitAndCallerScope(t *testing.T) {
	for _, first := range []bool{true, false} {
		t.Run(map[bool]string{true: "first", false: "replacement"}[first], func(t *testing.T) {
			clock := &receiverFakeClock{now: time.Unix(1, 0)}
			recovery, err := NewReceiverRecovery(ReceiverRecoveryOptions{
				Clock: clock, WaitTimeout: 3 * time.Second, Jitter: func(delay time.Duration) time.Duration { return delay },
			})
			if err != nil {
				t.Fatal(err)
			}
			config := receiverUnavailableConfig("one")
			open := recovery.Replace
			expected := ErrReceiverWaitExpired
			if first {
				open, expected = recovery.Join, ErrReceiverUnavailable
			}
			set, err := open(context.Background(), config)
			if set != nil || !errors.Is(err, expected) || errors.Is(err, context.Canceled) {
				t.Fatal(set, err)
			}
			if clock.Now() != time.Unix(4, 0) {
				t.Fatal(clock.Now())
			}
			// The configured duration is per outage, not time since the first invocation.
			_, err = open(context.Background(), config)
			if !errors.Is(err, expected) || clock.Now() != time.Unix(7, 0) {
				t.Fatal(err, clock.Now())
			}
		})
	}
	recovery, _ := NewReceiverRecovery(ReceiverRecoveryOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if set, err := recovery.Replace(ctx, receiverUnavailableConfig("one")); set != nil || !errors.Is(err, context.Canceled) {
		t.Fatal(set, err)
	}
}

func TestReceiverRecoveryTerminalErrorsNeverWait(t *testing.T) {
	protocolFault, _ := transferfault.NewSession(transferfault.ScopeSessionTerminal, transferfault.SessionProtocol)
	for _, rejected := range []error{
		ErrReceiverAuthentication, relayv2.ErrProtocol,
		protocolsession.ErrServerHelloSignature, protocolsession.ErrServerHelloMalformed,
		transferfault.Wrap(protocolFault, protocolsession.ErrUnsupportedVersion),
		transferfault.Wrap(protocolFault, protocolsession.ErrKeyAgreement),
		&relayv2.RelayError{Code: v2.ErrorStopped}, &relayv2.RelayError{Code: v2.ErrorInvalidProof},
	} {
		t.Run(rejected.Error(), func(t *testing.T) {
			clock := &receiverFakeClock{now: time.Unix(1, 0)}
			var terminalCount atomic.Int32
			recovery, _ := NewReceiverRecovery(ReceiverRecoveryOptions{Clock: clock, Observe: func(value ReceiverRecoveryObservation) {
				if value.Phase == ReceiverRecoveryTerminal && errors.Is(value.Err, rejected) {
					terminalCount.Add(1)
				}
			}})
			config := receiverUnavailableConfig("one")
			config.Dial = func(context.Context, relayv2.ReceiverConfig) (ReceiverConnection, error) {
				return nil, rejected
			}
			set, err := recovery.Replace(context.Background(), config)
			if set != nil || !errors.Is(err, rejected) || len(clock.waits) != 0 || terminalCount.Load() != 1 {
				t.Fatal(set, err, clock.waits, terminalCount.Load())
			}
		})
	}
}

func TestReceiverRecoveryAttemptDeadlineAndCancellationJoin(t *testing.T) {
	attemptDeadline := make(chan context.CancelFunc, 1)
	dialEntered := make(chan struct{})
	finished := make(chan struct{})
	clock := &receiverBlockedRetryClock{waiting: make(chan struct{})}
	recovery, _ := NewReceiverRecovery(ReceiverRecoveryOptions{
		Clock: clock,
		TimeoutContext: func(ctx context.Context, duration time.Duration) (context.Context, context.CancelFunc) {
			if duration != receiverAttemptTimeout {
				t.Errorf("attempt duration=%s", duration)
			}
			child, cancel := context.WithCancel(ctx)
			attemptDeadline <- cancel
			return child, cancel
		},
	})
	config := receiverUnavailableConfig("one")
	config.Dial = func(ctx context.Context, _ relayv2.ReceiverConfig) (ReceiverConnection, error) {
		defer close(finished)
		close(dialEntered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := recovery.Replace(ctx, config); result <- err }()
	expireAttempt := <-attemptDeadline
	// Context creation does not establish dial ownership: an attempt that
	// expires before Dial starts correctly skips it entirely.
	<-dialEntered
	expireAttempt()
	<-clock.waiting
	if ctx.Err() != nil {
		t.Fatal("attempt expiry canceled the caller", ctx.Err())
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("returned before dial released its ownership")
	}
}

type receiverBlockedRetryClock struct{ waiting chan struct{} }

func (*receiverBlockedRetryClock) Now() time.Time { return time.Unix(1, 0) }
func (clock *receiverBlockedRetryClock) Wait(ctx context.Context, _ time.Duration) error {
	close(clock.waiting)
	<-ctx.Done()
	return ctx.Err()
}

func TestReceiverRecoveryWaitingDeadlineClosesActiveAttempt(t *testing.T) {
	const waitLimit = time.Minute
	window := make(chan context.CancelFunc, 1)
	entered := make(chan struct{})
	finished := make(chan struct{})
	recovery, _ := NewReceiverRecovery(ReceiverRecoveryOptions{
		WaitTimeout: waitLimit,
		TimeoutContext: func(ctx context.Context, duration time.Duration) (context.Context, context.CancelFunc) {
			child, cancel := context.WithCancel(ctx)
			if duration == waitLimit {
				window <- cancel
			} else if duration != receiverAttemptTimeout {
				t.Errorf("unexpected deadline=%s", duration)
			}
			return child, cancel
		},
	})
	config := receiverUnavailableConfig("one")
	config.Dial = func(ctx context.Context, _ relayv2.ReceiverConfig) (ReceiverConnection, error) {
		close(entered)
		<-ctx.Done()
		close(finished)
		return nil, ctx.Err()
	}
	ctx := t.Context()
	result := make(chan error, 1)
	go func() { _, err := recovery.Replace(ctx, config); result <- err }()
	expireWindow := <-window
	<-entered
	expireWindow()
	if err := <-result; !errors.Is(err, ErrReceiverWaitExpired) {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatal("waiting deadline canceled caller", ctx.Err())
	}
	select {
	case <-finished:
	default:
		t.Fatal("waiting deadline returned before dial released ownership")
	}
}

func TestReceiverRecoveryRejectsInvalidEndpointSets(t *testing.T) {
	recovery, _ := NewReceiverRecovery(ReceiverRecoveryOptions{})
	for _, endpoints := range [][]string{nil, {"one", "one"}, make([]string, MaximumEndpoints+1)} {
		for _, open := range []func(context.Context, ReceiverConfig) (*Receiver, error){recovery.Join, recovery.Replace} {
			if set, err := open(context.Background(), receiverUnavailableConfig(endpoints...)); set != nil || err == nil || err.Error() == "" {
				t.Fatal(set, err)
			}
		}
	}
	if (&ReceiverJoinFailure{}).Error() == "" {
		t.Fatal("empty failure lacks diagnostic")
	}
}

func TestReceiverInitialPublicationRejectsConcurrentIdentityChange(t *testing.T) {
	recovery, _ := NewReceiverRecovery(ReceiverRecoveryOptions{})
	receiver := &Receiver{ctx: context.Background(), config: ReceiverConfig{recovery: recovery}, ready: make(chan struct{})}
	if err := recovery.validateDescriptor([]byte{1}); err != nil {
		t.Fatal(err)
	}
	// The candidate completed authentication before the competing descriptor
	// arrived. Publication must still consult the share's current authority.
	candidate := &receiversession.Runtime{}
	if err := recovery.validateDescriptor([]byte{2}); !errors.Is(err, ErrReceiverShareChanged) {
		t.Fatal(err)
	}
	var lane session.LaneIdentity
	if err := receiver.publishInitial(context.Background(), candidate, nil, nil, &lane); !errors.Is(err, ErrReceiverShareChanged) || receiver.current() != nil {
		t.Fatal(err)
	}
	select {
	case <-receiver.ready:
		t.Fatal("contradictory identity published as ready")
	default:
	}
}

func TestReceiverRecoveryDescriptorAndDelayBounds(t *testing.T) {
	clock := &receiverFakeClock{now: time.Unix(1, 0)}
	recovery, _ := NewReceiverRecovery(ReceiverRecoveryOptions{Clock: clock, Jitter: func(time.Duration) time.Duration { return -1 }})
	if err := recovery.validateDescriptor([]byte{1, 2}); err != nil {
		t.Fatal(err)
	}
	if err := recovery.validateDescriptor([]byte{1, 2}); err != nil {
		t.Fatal(err)
	}
	if err := recovery.validateDescriptor([]byte{1, 3}); !errors.Is(err, ErrReceiverShareChanged) {
		t.Fatal(err)
	}
	if _, err := recovery.endpoints([]string{"one"}); !errors.Is(err, ErrReceiverShareChanged) {
		t.Fatal(err)
	}
	if err := recovery.validateDescriptor([]byte{1, 2}); !errors.Is(err, ErrReceiverShareChanged) {
		t.Fatal("identity failure was reversible")
	}
	started := clock.Now()
	delay, phase := recovery.delay(started, 0, nil)
	if delay != receiverRetryDelay/2 || phase != ReceiverRecoveryRetrying {
		t.Fatal(delay, phase)
	}
	clock.Wait(context.Background(), time.Hour)
	delay, phase = recovery.delay(started, 100, &relayv2.RelayError{Code: v2.ErrorAdmission, RetryAfter: time.Hour})
	if delay != receiverMaximumRetryDelay/2 || phase != ReceiverRecoveryWaiting {
		t.Fatal(delay, phase)
	}
	for _, options := range []ReceiverRecoveryOptions{{InitialWait: -1}, {FastWindow: -1}, {WaitTimeout: -1}} {
		if _, err := NewReceiverRecovery(options); err == nil {
			t.Fatal("negative duration accepted")
		}
	}
}

func TestReceiverRecoveryRejectsInvalidCallerLifetimes(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, test := range []struct {
		name  string
		ctx   context.Context
		cause error
	}{
		{name: "missing"},
		{name: "canceled", ctx: canceled, cause: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			recovery, _ := NewReceiverRecovery(ReceiverRecoveryOptions{})
			config := receiverUnavailableConfig("one")
			config.Dial = func(context.Context, relayv2.ReceiverConfig) (ReceiverConnection, error) {
				t.Error("invalid caller lifetime reached dial")
				return nil, relayv2.ErrProtocol
			}
			for _, open := range []func(context.Context, ReceiverConfig) (*Receiver, error){recovery.Join, recovery.Replace} {
				set, err := open(test.ctx, config)
				if set != nil || err == nil || (test.cause != nil && !errors.Is(err, test.cause)) {
					t.Fatal(set, err)
				}
			}
		})
	}
}
