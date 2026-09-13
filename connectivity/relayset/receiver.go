package relayset

import (
	"context"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

const receiverAttemptTimeout = 10 * time.Second
const receiverRetryDelay = 250 * time.Millisecond

var ErrReceiverAuthentication = errors.New("receiver relay authentication failed")

// ReceiverJoinFailure retains endpoint authority boundaries. One permanently
// rejected endpoint cannot veto recovery through an independently failed relay.
type ReceiverJoinFailure struct {
	causes         []error
	retryEndpoints []string
}

func (failure *ReceiverJoinFailure) Error() string {
	if cause := errors.Join(failure.causes...); cause != nil {
		return cause.Error()
	}
	return "receiver could not join any relay endpoint"
}
func (failure *ReceiverJoinFailure) Unwrap() []error { return append([]error(nil), failure.causes...) }
func (failure *ReceiverJoinFailure) RetryEndpoints() []string {
	return append([]string(nil), failure.retryEndpoints...)
}

func receiverEndpointRetryable(err error) bool {
	if boundary, ok := errors.AsType[*transferfault.BoundaryError](err); ok {
		if code, ok := boundary.Fault().SessionCode(); ok && code == transferfault.SessionProtocol {
			return false
		}
	}
	for _, terminal := range []error{ErrReceiverAuthentication, ErrReceiverShareChanged, relayv2.ErrProtocol,
		protocolsession.ErrServerHelloMalformed, protocolsession.ErrServerHelloSignature,
		protocolsession.ErrLaneSignature, protocolsession.ErrEnvelopeAuthentication,
		protocolsession.ErrInboundAuthentication, protocolsession.ErrControlSignature} {
		if errors.Is(err, terminal) {
			return false
		}
	}
	var rejection *relayv2.RelayError
	if !errors.As(err, &rejection) {
		return true
	}
	switch rejection.Code {
	case v2.ErrorNotFound, v2.ErrorStarting, v2.ErrorAdmission:
		return true
	default:
		return false
	}
}

type ReceiverClock interface {
	Now() time.Time
	Wait(context.Context, time.Duration) error
}
type receiverClock struct{}

func (receiverClock) Now() time.Time { return time.Now() }
func (receiverClock) Wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type ReceiverConfig struct {
	recovery    *ReceiverRecovery
	Clock       ReceiverClock
	Receiver    liveshare.ReceiverConfig
	Dial        func(context.Context, relayv2.ReceiverConfig) (*relayv2.ReceiverConnection, error)
	DialOptions relayv2.DialOptions
	Connected   func(*relayv2.ReceiverConnection)
}

// Receiver owns relay connections for one cryptographic session. Competing
// initial handshakes race to publish the first usable session; losers release
// their authority and join the winner using a fresh authenticated lane grant.
type Receiver struct {
	ctx              context.Context
	cancel           context.CancelFunc
	config           ReceiverConfig
	shareID          v2.ShareID
	mu               sync.Mutex
	runtime          *sessionruntime.ReceiverRuntime
	prepared         *liveshare.PreparedReceiver
	first            *relayv2.ReceiverConnection
	remaining        int
	failure          error
	ready            chan struct{}
	workers          sync.WaitGroup
	closeOnce        sync.Once
	stopRuntimeWatch func() bool
}

func validateReceiverEndpoints(endpoints []string) error {
	if len(endpoints) == 0 || len(endpoints) > MaximumEndpoints {
		return errors.New("receiver relay set requires bounded endpoints")
	}
	seen := make(map[string]bool, len(endpoints))
	for _, endpoint := range endpoints {
		if seen[endpoint] {
			return errors.New("receiver relay endpoints must be unique")
		}
		seen[endpoint] = true
	}
	return nil
}

func NewReceiver(ctx context.Context, config ReceiverConfig) (*Receiver, error) {
	if ctx == nil {
		return nil, errors.New("receiver relay set requires caller lifetime")
	}
	if err := validateReceiverEndpoints(config.Receiver.Capability.Relays); err != nil {
		return nil, err
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(config.Receiver.Capability.ShareID)
	if err != nil {
		return nil, err
	}
	share, err := v2.ShareIDFromBytes(raw)
	if err != nil {
		return nil, err
	}
	if config.Clock == nil {
		config.Clock = receiverClock{}
	}
	if config.Dial == nil {
		config.Dial = relayv2.DialReceiver
	}
	if config.recovery == nil {
		config.recovery, _ = NewReceiverRecovery(ReceiverRecoveryOptions{Clock: config.Clock})
	}
	lifetime, cancel := context.WithCancel(ctx)
	receiver := &Receiver{ctx: lifetime, cancel: cancel, config: config, shareID: share, remaining: len(config.Receiver.Capability.Relays), ready: make(chan struct{})}
	for _, url := range config.Receiver.Capability.Relays {
		receiver.workers.Add(1)
		go receiver.run(url)
	}
	return receiver, nil
}

func (receiver *Receiver) WaitReady(ctx context.Context) (*sessionruntime.ReceiverRuntime, *relayv2.ReceiverConnection, error) {
	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case <-receiver.ready:
	}
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	return receiver.runtime, receiver.first, receiver.failure
}

func (receiver *Receiver) Close() {
	if receiver == nil {
		return
	}
	receiver.closeOnce.Do(func() {
		receiver.cancel()
		receiver.mu.Lock()
		runtime := receiver.runtime
		stopRuntimeWatch := receiver.stopRuntimeWatch
		receiver.mu.Unlock()
		if stopRuntimeWatch != nil {
			stopRuntimeWatch()
		}
		if runtime != nil {
			runtime.BeginClose()
		}
		receiver.workers.Wait()
		if runtime != nil {
			runtime.WaitClosed()
		}
		receiver.mu.Lock()
		prepared := receiver.prepared
		receiver.mu.Unlock()
		if prepared != nil {
			prepared.Close()
		}
	})
}

func (receiver *Receiver) run(url string) {
	defer receiver.workers.Done()
	initial := true
	var lane sessionruntime.LaneIdentity
	recovery := receiver.config.recovery
	started := receiver.config.Clock.Now()
	for failures := uint32(0); receiver.attemptError(receiver.ctx) == nil; failures++ {
		attempt := recovery.begin(url)
		recovery.observe(url, attempt, ReceiverRecoveryConnecting, 0, receiver.current(), nil)
		ctx, cancel := recovery.options.TimeoutContext(receiver.ctx, receiverAttemptTimeout)
		connection, err := receiver.connectAttempt(ctx, url, &lane)
		cancel()
		recovery.rejected(url, err)
		if !receiverEndpointRetryable(err) {
			// Publish the terminal reason before ready releases the join owner,
			// whose cleanup may cancel this worker immediately.
			recovery.observe(url, attempt, ReceiverRecoveryTerminal, 0, receiver.current(), err)
		}
		if initial {
			receiver.finishInitial(url, err)
			initial = false
		}
		if err == nil {
			recovery.observe(url, attempt, ReceiverRecoveryConnected, 0, receiver.current(), nil)
			receiver.waitLane(connection, lane)
			err = connection.Err()
			recovery.rejected(url, err)
			if !receiverEndpointRetryable(err) {
				recovery.observe(url, attempt, ReceiverRecoveryTerminal, 0, receiver.current(), err)
			}
			started = receiver.config.Clock.Now()
			failures = 0
		}
		if connection != nil {
			_ = connection.Close()
		}
		// Initial failure is coordinated by the share's first-join owner. Once
		// another relay wins, this endpoint can attach without replacing it.
		select {
		case <-receiver.ctx.Done():
			return
		case <-receiver.ready:
		}
		runtime := receiver.current()
		if receiver.ctx.Err() != nil {
			return
		}
		if !receiverEndpointRetryable(err) {
			return
		}
		if runtime == nil {
			return
		}
		if runtime.Stopping() {
			return
		}
		delay, phase := recovery.delay(started, failures, err)
		recovery.observe(url, attempt, phase, delay, runtime, err)
		if err := receiver.config.Clock.Wait(receiver.ctx, delay); err != nil {
			return
		}
	}
}

func (receiver *Receiver) current() *sessionruntime.ReceiverRuntime {
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	return receiver.runtime
}

var closedReceiverLane = func() <-chan struct{} { done := make(chan struct{}); close(done); return done }()

func (receiver *Receiver) admit(ctx context.Context, connection *relayv2.ReceiverConnection, lane *sessionruntime.LaneIdentity) error {
	config := receiver.config.Receiver
	config.DescriptorObject = connection.Descriptor()
	prepared, err := liveshare.PrepareReceiver(config)
	if err != nil {
		return errors.Join(ErrReceiverAuthentication, err)
	}
	if err = receiver.config.recovery.validateDescriptor(config.DescriptorObject); err != nil {
		prepared.Close()
		if runtime := receiver.current(); runtime != nil {
			runtime.RejectShareIdentity(err)
		}
		return err
	}
	receiver.mu.Lock()
	runtime := receiver.runtime
	receiver.mu.Unlock()
	if runtime == nil {
		return receiver.connectInitial(ctx, prepared, connection, lane)
	}
	defer prepared.Close()
	if prepared.Descriptor().ShareInstance() != runtime.Descriptor().ShareInstance() || prepared.Descriptor().SyntheticRoot() != runtime.Descriptor().SyntheticRoot() {
		return ErrReceiverAuthentication
	}
	grant, err := runtime.RequestLane(ctx, lane.ID)
	if err != nil {
		return err
	}
	_, err = runtime.AttachLane(ctx, grant, connection.Channel(), transfer.LaneRouteRelay)
	if err == nil {
		lane.ID = grant.LaneID
		lane.Epoch = grant.LaneEpoch
	}
	if err == nil && receiver.config.Connected != nil {
		receiver.config.Connected(connection)
	}
	return err
}

func (receiver *Receiver) attemptError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if runtime := receiver.current(); runtime != nil && runtime.Stopping() {
		return sessionruntime.ErrRuntimeClosed
	}
	return nil
}

func (receiver *Receiver) connectAttempt(ctx context.Context, url string, lane *sessionruntime.LaneIdentity) (*relayv2.ReceiverConnection, error) {
	if err := receiver.attemptError(ctx); err != nil {
		return nil, err
	}
	connection, err := receiver.dial(ctx, url)
	if err == nil && connection == nil {
		err = errors.New("receiver dial returned no connection")
	}
	if err == nil {
		// Dial implementations may finish after cancellation. Such a candidate
		// cannot authenticate a new descriptor or enter the retired generation.
		err = receiver.attemptError(ctx)
	}
	if err == nil {
		err = receiver.admit(ctx, connection, lane)
	}
	return connection, err
}

func (receiver *Receiver) dial(ctx context.Context, url string) (*relayv2.ReceiverConnection, error) {
	return receiver.config.Dial(ctx, relayv2.ReceiverConfig{RelayBaseURL: url, ShareID: receiver.shareID, Dial: receiver.config.DialOptions})
}

func (receiver *Receiver) connectInitial(ctx context.Context, prepared *liveshare.PreparedReceiver, connection *relayv2.ReceiverConnection, lane *sessionruntime.LaneIdentity) error {
	candidate, connectErr := prepared.Connect(ctx, connection.Channel(), transfer.LaneRouteRelay)
	if connectErr != nil {
		prepared.Close()
		return connectErr
	}
	if err := receiver.publishInitial(ctx, candidate, prepared, connection, lane); err != nil {
		candidate.Close()
		prepared.Close()
		return err
	}
	if receiver.config.Connected != nil {
		receiver.config.Connected(connection)
	}
	return nil
}

func (receiver *Receiver) publishInitial(ctx context.Context, candidate *sessionruntime.ReceiverRuntime, prepared *liveshare.PreparedReceiver, connection *relayv2.ReceiverConnection, lane *sessionruntime.LaneIdentity) error {
	// An authenticated contradiction can race a still-running handshake. Hold
	// identity authority through publication so rejection either prevents this
	// generation or observes and terminates the published runtime.
	recovery := receiver.config.recovery
	recovery.mu.Lock()
	defer recovery.mu.Unlock()
	if recovery.terminal != nil {
		return recovery.terminal
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	if err := receiver.ctx.Err(); err != nil {
		return err
	}
	if receiver.runtime != nil {
		// A completed loser owns a distinct transcript. Its channel cannot be
		// reassigned to the winner, so the endpoint redials before attachment.
		return errors.New("initial relay handshake superseded")
	}
	if candidate.Stopping() {
		return errors.Join(sessionruntime.ErrRuntimeClosed, candidate.Err())
	}
	// Relay waits and dials belong to this session's live authority. Done is a
	// later cleanup boundary and cannot authorize retries during finalization.
	receiver.stopRuntimeWatch = context.AfterFunc(candidate.Lifetime(), receiver.cancel)
	receiver.runtime = candidate
	receiver.prepared = prepared
	receiver.first = connection
	lane.ID, lane.Epoch = candidate.LaneIdentity()
	receiver.failure = nil
	close(receiver.ready)
	return nil
}
func (receiver *Receiver) finishInitial(url string, err error) {
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	receiver.remaining--
	if receiver.runtime != nil {
		return
	}
	var failure *ReceiverJoinFailure
	if !errors.As(receiver.failure, &failure) {
		failure = &ReceiverJoinFailure{}
		receiver.failure = failure
	}
	failure.causes = append(failure.causes, err)
	if receiverEndpointRetryable(err) {
		failure.retryEndpoints = append(failure.retryEndpoints, url)
	}
	if receiver.remaining == 0 {
		close(receiver.ready)
	}
}
func (receiver *Receiver) waitLane(connection *relayv2.ReceiverConnection, lane sessionruntime.LaneIdentity) {
	receiver.mu.Lock()
	runtime := receiver.runtime
	receiver.mu.Unlock()
	done, live := runtime.LaneDone(lane)
	if !live {
		done = closedReceiverLane
	}
	select {
	case <-receiver.ctx.Done():
	case <-runtime.Done():
	case <-connection.Done():
	case <-done:
	}
}
