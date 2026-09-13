package relayset

import (
	"bytes"
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/transport/relayv2"
)

const (
	ReceiverInitialWait        = 10 * time.Second
	receiverFastRecoveryWindow = 55 * time.Second
	receiverMaximumFastDelay   = 5 * time.Second
	receiverSlowRetryDelay     = 30 * time.Second
	receiverMaximumRetryDelay  = time.Minute
)

var (
	ErrReceiverUnavailable  = errors.New("share is temporarily unavailable")
	ErrReceiverWaitExpired  = errors.New("receiver connection waiting deadline expired")
	ErrReceiverShareChanged = errors.New("receiver authenticated share descriptor changed")
)

type ReceiverRecoveryPhase string

const (
	ReceiverRecoveryConnecting ReceiverRecoveryPhase = "connecting"
	ReceiverRecoveryRetrying   ReceiverRecoveryPhase = "retrying"
	ReceiverRecoveryWaiting    ReceiverRecoveryPhase = "waiting"
	ReceiverRecoveryConnected  ReceiverRecoveryPhase = "connected"
	ReceiverRecoveryTerminal   ReceiverRecoveryPhase = "terminal"
)

// A joined share keeps endpoint decisions and retry pressure across protocol
// generations. Neither a new session nor a lane attachment resets this owner.
type ReceiverRecoveryOptions struct {
	Clock          ReceiverClock
	TimeoutContext func(context.Context, time.Duration) (context.Context, context.CancelFunc)
	InitialWait    time.Duration
	FastWindow     time.Duration
	WaitTimeout    time.Duration
	Jitter         func(time.Duration) time.Duration
	Observe        func(ReceiverRecoveryObservation)
}

type ReceiverRecoveryObservation struct {
	Endpoint             string
	Attempt              uint32
	Phase                ReceiverRecoveryPhase
	Delay                time.Duration
	ProtocolSessionID    [16]byte
	ConnectionGeneration uint64
	Err                  error
}

type ReceiverRecovery struct {
	options     ReceiverRecoveryOptions
	mu          sync.Mutex
	disabled    map[string]error
	descriptor  []byte
	terminal    error
	attempts    map[string]uint32
	generations map[string]uint64
}

func NewReceiverRecovery(options ReceiverRecoveryOptions) (*ReceiverRecovery, error) {
	if options.InitialWait < 0 || options.FastWindow < 0 || options.WaitTimeout < 0 {
		return nil, errors.New("receiver recovery durations must not be negative")
	}
	if options.Clock == nil {
		options.Clock = receiverClock{}
	}
	if options.TimeoutContext == nil {
		options.TimeoutContext = context.WithTimeout
	}
	if options.InitialWait == 0 {
		options.InitialWait = ReceiverInitialWait
	}
	if options.FastWindow == 0 {
		options.FastWindow = receiverFastRecoveryWindow
	}
	if options.Jitter == nil {
		options.Jitter = func(delay time.Duration) time.Duration {
			const spread = 5
			return delay - delay/spread + time.Duration(rand.Int64N(int64(2*delay/spread)+1))
		}
	}
	return &ReceiverRecovery{options: options, disabled: make(map[string]error), attempts: make(map[string]uint32), generations: make(map[string]uint64)}, nil
}

// Join has a finite default window. An explicit caller wait limit also selects
// how long the caller wants to wait before any usable share has been observed.
func (recovery *ReceiverRecovery) Join(ctx context.Context, config ReceiverConfig) (*Receiver, error) {
	window := recovery.options.InitialWait
	if recovery.options.WaitTimeout > 0 {
		window = recovery.options.WaitTimeout
	}
	return recovery.open(ctx, config, window, ErrReceiverUnavailable)
}

// Replace waits through temporary outages until success, terminal rejection, or
// caller cancellation. The unchanged continuation owns the transfer and output.
func (recovery *ReceiverRecovery) Replace(ctx context.Context, config ReceiverConfig) (*Receiver, error) {
	return recovery.open(ctx, config, recovery.options.WaitTimeout, ErrReceiverWaitExpired)
}

func (recovery *ReceiverRecovery) open(ctx context.Context, config ReceiverConfig, window time.Duration, expiry error) (*Receiver, error) {
	if ctx == nil {
		return nil, errors.New("receiver recovery requires caller lifetime")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateReceiverEndpoints(config.Receiver.Capability.Relays); err != nil {
		return nil, err
	}
	started := recovery.options.Clock.Now()
	waitCtx, cancel := context.WithCancel(ctx)
	if window > 0 {
		cancel()
		waitCtx, cancel = recovery.options.TimeoutContext(ctx, window)
	}
	defer cancel()
	config.recovery = recovery
	config.Clock = recovery.options.Clock
	var last error
	for attempt := uint32(0); ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if waitCtx.Err() != nil || (window > 0 && !recovery.options.Clock.Now().Before(started.Add(window))) {
			return nil, errors.Join(expiry, last)
		}
		set, err := recovery.joinOnce(ctx, waitCtx, config)
		if err == nil {
			return set, nil
		}
		last = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if _, joined := errors.AsType[*ReceiverJoinFailure](err); !joined {
			if waitCtx.Err() != nil {
				return nil, errors.Join(expiry, err)
			}
			return nil, err
		}
		eligible, err := recovery.endpoints(config.Receiver.Capability.Relays)
		if err != nil {
			return nil, err
		}
		delay, phase := recovery.delay(started, attempt, last)
		recovery.observeRetry(eligible, phase, delay, last)
		if window > 0 {
			delay = min(delay, max(0, started.Add(window).Sub(recovery.options.Clock.Now())))
		}
		if err = recovery.options.Clock.Wait(waitCtx, delay); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errors.Join(expiry, last)
		}
	}
}

func (recovery *ReceiverRecovery) joinOnce(ctx, waitCtx context.Context, config ReceiverConfig) (*Receiver, error) {
	var err error
	config.Receiver.Capability.Relays, err = recovery.endpoints(config.Receiver.Capability.Relays)
	if err != nil {
		return nil, err
	}
	// A successful session borrows the caller lifetime, never the waiting
	// deadline. Failed candidates are joined before another one is created.
	set, err := NewReceiver(ctx, config)
	if err != nil {
		return nil, err
	}
	if _, _, err = set.WaitReady(waitCtx); err != nil {
		set.Close()
		return nil, err
	}
	return set, nil
}

func (recovery *ReceiverRecovery) endpoints(endpoints []string) ([]string, error) {
	recovery.mu.Lock()
	defer recovery.mu.Unlock()
	if recovery.terminal != nil {
		return nil, recovery.terminal
	}
	eligible := make([]string, 0, len(endpoints))
	var rejected []error
	for _, endpoint := range endpoints {
		if err := recovery.disabled[endpoint]; err != nil {
			rejected = append(rejected, err)
		} else {
			eligible = append(eligible, endpoint)
		}
	}
	if len(eligible) == 0 {
		return nil, &ReceiverJoinFailure{causes: rejected}
	}
	return eligible, nil
}

func (recovery *ReceiverRecovery) validateDescriptor(object []byte) error {
	recovery.mu.Lock()
	defer recovery.mu.Unlock()
	if recovery.terminal != nil {
		return recovery.terminal
	}
	if recovery.descriptor == nil {
		recovery.descriptor = append([]byte(nil), object...)
		return nil
	}
	if !bytes.Equal(recovery.descriptor, object) {
		recovery.terminal = ErrReceiverShareChanged
		return recovery.terminal
	}
	return nil
}

func (recovery *ReceiverRecovery) rejected(endpoint string, err error) {
	if err == nil || receiverEndpointRetryable(err) {
		return
	}
	recovery.mu.Lock()
	recovery.disabled[endpoint] = err
	recovery.mu.Unlock()
}

func (recovery *ReceiverRecovery) begin(endpoint string) uint32 {
	recovery.mu.Lock()
	defer recovery.mu.Unlock()
	recovery.attempts[endpoint]++
	return recovery.attempts[endpoint]
}

func (recovery *ReceiverRecovery) observeRetry(endpoints []string, phase ReceiverRecoveryPhase, delay time.Duration, cause error) {
	for _, endpoint := range endpoints {
		recovery.mu.Lock()
		ordinal := recovery.attempts[endpoint]
		recovery.mu.Unlock()
		recovery.observe(endpoint, ordinal, phase, delay, nil, cause)
	}
}

func (recovery *ReceiverRecovery) observe(endpoint string, attempt uint32, phase ReceiverRecoveryPhase, delay time.Duration, runtime *sessionruntime.ReceiverRuntime, err error) {
	recovery.mu.Lock()
	if phase == ReceiverRecoveryConnected {
		recovery.generations[endpoint]++
	}
	generation := recovery.generations[endpoint]
	recovery.mu.Unlock()
	if recovery.options.Observe == nil {
		return
	}
	observation := ReceiverRecoveryObservation{Endpoint: endpoint, Attempt: attempt, Phase: phase, Delay: delay,
		ConnectionGeneration: generation, Err: err}
	if runtime != nil {
		observation.ProtocolSessionID = [16]byte(runtime.ProtocolSessionID())
	}
	recovery.options.Observe(observation)
}

func (recovery *ReceiverRecovery) delay(started time.Time, attempt uint32, cause error) (time.Duration, ReceiverRecoveryPhase) {
	phase := ReceiverRecoveryRetrying
	delay := min(receiverRetryDelay*time.Duration(uint64(1)<<min(attempt, 5)), receiverMaximumFastDelay)
	if !recovery.options.Clock.Now().Before(started.Add(recovery.options.FastWindow)) {
		phase = ReceiverRecoveryWaiting
		delay = receiverSlowRetryDelay
	}
	if rejection, ok := errors.AsType[*relayv2.RelayError](cause); ok {
		delay = max(delay, rejection.RetryAfter)
	}
	delay = min(delay, receiverMaximumRetryDelay)
	// A supplied jitter source cannot introduce an immediate busy loop or
	// bypass the bounded retry interval.
	return min(max(recovery.options.Jitter(delay), delay/2), receiverMaximumRetryDelay), phase
}
