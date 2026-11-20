package relayset

import (
	"bytes"
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/windshare/windshare/core/framechannel"

	receiversession "github.com/windshare/windshare/core/session/receiversession"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
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
	ErrReceiverShareStopped = errors.New("all relay routes for the share are stopped")
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
	options    ReceiverRecoveryOptions
	mu         sync.Mutex
	routes     map[string]*endpointRecovery
	descriptor []byte
	terminal   error
}

type endpointRecovery struct {
	attempts   uint32
	generation uint64
	disabled   error
	backoff    endpointBackoff
}

type endpointBackoff struct {
	until time.Time
	cause error
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
	return &ReceiverRecovery{options: options, routes: make(map[string]*endpointRecovery)}, nil
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
	if err := validateReceiverCandidates(config); err != nil {
		return nil, err
	}
	started := recovery.options.Clock.Now()
	waitCtx, cancel := recovery.joinWaitContext(ctx, window)
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
		set, err := recovery.joinOnce(ctx, waitCtx, &config)
		if err == nil {
			return set, nil
		}
		last = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrReceiverShareStopped) {
			return nil, err
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
		delay, phase := recovery.localRetryDelay(started, attempt, last)
		delay = recovery.scheduleWave(eligible, phase, delay, last)
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

func (recovery *ReceiverRecovery) joinWaitContext(ctx context.Context, window time.Duration) (context.Context, context.CancelFunc) {
	if window > 0 {
		return recovery.options.TimeoutContext(ctx, window)
	}
	return context.WithCancel(ctx)
}

func (recovery *ReceiverRecovery) joinOnce(ctx, waitCtx context.Context, config *ReceiverConfig) (*Receiver, error) {
	// A successful session borrows the caller lifetime, never the waiting
	// deadline. Failed candidates are joined before another one is created.
	set, err := NewReceiver(ctx, *config)
	if err != nil {
		return nil, err
	}
	if _, _, err = set.WaitReady(waitCtx); err != nil {
		set.Close()
		if set.AllRelaysStopped() {
			return nil, errors.Join(ErrReceiverShareStopped, err)
		}
		// Keep the first bounded expansion across recovery waves. Its omissions
		// still matter when deciding whether every route rejected the share.
		config.Receiver.Capability.Relays, config.incompleteCandidates = set.candidateSnapshot()
		config.ResolveEndpoints = nil
		return nil, err
	}
	return set, nil
}

func (recovery *ReceiverRecovery) routeLocked(endpoint string) *endpointRecovery {
	route := recovery.routes[endpoint]
	if route == nil {
		route = &endpointRecovery{}
		recovery.routes[endpoint] = route
	}
	return route
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
		if err := recovery.routeLocked(endpoint).disabled; err != nil {
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

func (recovery *ReceiverRecovery) allRelaysStopped(candidates []string) bool {
	recovery.mu.Lock()
	defer recovery.mu.Unlock()
	for _, endpoint := range candidates {
		rejection, ok := errors.AsType[*relayv2.RelayError](recovery.routeLocked(endpoint).disabled)
		if !ok || rejection == nil || rejection.Code != v2.ErrorStopped {
			return false
		}
	}
	return len(candidates) != 0 && recovery.terminal == nil
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

func (recovery *ReceiverRecovery) recordFailure(endpoint string, err error) {
	if err == nil {
		return
	}
	recovery.mu.Lock()
	defer recovery.mu.Unlock()
	route := recovery.routeLocked(endpoint)
	if !receiverEndpointRetryable(err) {
		route.disabled = err
		return
	}
	if delay := min(relayv2.RetryAfter(err), receiverMaximumRetryDelay); delay > 0 {
		if until := recovery.options.Clock.Now().Add(delay); until.After(route.backoff.until) {
			route.backoff = endpointBackoff{until: until, cause: err}
		}
	}
}

func (recovery *ReceiverRecovery) begin(endpoint string) (uint32, error) {
	recovery.mu.Lock()
	defer recovery.mu.Unlock()
	route := recovery.routeLocked(endpoint)
	if route.disabled != nil {
		return route.attempts, route.disabled
	}
	if route.backoff.cause != nil {
		if route.backoff.until.After(recovery.options.Clock.Now()) {
			return route.attempts, route.backoff.cause
		}
		route.backoff = endpointBackoff{}
	}
	route.attempts++
	return route.attempts, nil
}

// A wave can retry as soon as any eligible route can dial. Its siblings keep
// their own absolute deadlines across initial races and session replacement.
func (recovery *ReceiverRecovery) scheduleWave(endpoints []string, phase ReceiverRecoveryPhase, delay time.Duration, cause error) time.Duration {
	earliest := receiverMaximumRetryDelay
	for _, endpoint := range endpoints {
		wait := recovery.scheduleEndpointRetry(endpoint, phase, delay, nil, cause)
		earliest = min(earliest, wait)
	}
	return earliest
}

func (recovery *ReceiverRecovery) scheduleEndpointRetry(endpoint string, phase ReceiverRecoveryPhase, delay time.Duration, runtime *receiversession.Runtime, cause error) time.Duration {
	recovery.mu.Lock()
	route := recovery.routeLocked(endpoint)
	ordinal := route.attempts
	if backoff := route.backoff; backoff.cause != nil {
		if remaining := backoff.until.Sub(recovery.options.Clock.Now()); remaining > 0 {
			delay, cause = max(delay, remaining), backoff.cause
		}
	}
	recovery.mu.Unlock()
	recovery.observe(endpoint, ordinal, phase, delay, runtime, cause)
	return delay
}

func (recovery *ReceiverRecovery) observe(endpoint string, attempt uint32, phase ReceiverRecoveryPhase, delay time.Duration, runtime *receiversession.Runtime, err error) {
	recovery.mu.Lock()
	route := recovery.routeLocked(endpoint)
	if phase == ReceiverRecoveryConnected {
		route.generation++
	}
	generation := route.generation
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

func (recovery *ReceiverRecovery) localRetryDelay(started time.Time, attempt uint32, cause error) (time.Duration, ReceiverRecoveryPhase) {
	phase := ReceiverRecoveryRetrying
	delay := min(receiverRetryDelay*time.Duration(uint64(1)<<min(attempt, 5)), receiverMaximumFastDelay)
	if !recovery.options.Clock.Now().Before(started.Add(recovery.options.FastWindow)) {
		phase = ReceiverRecoveryWaiting
		delay = receiverSlowRetryDelay
	}
	if pressure, ok := errors.AsType[*framechannel.BandwidthWaitError](cause); ok {
		phase = ReceiverRecoveryWaiting
		delay = max(receiverSlowRetryDelay, pressure.RetryAfter)
	}
	delay = min(delay, receiverMaximumRetryDelay)
	// A supplied jitter source cannot introduce an immediate busy loop or
	// bypass the bounded retry interval.
	return min(max(recovery.options.Jitter(delay), delay/2), receiverMaximumRetryDelay), phase
}
