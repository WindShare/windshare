package relayset

import (
	"context"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

const receiverAttemptTimeout = 10 * time.Second
const receiverRecoveryWindow = 55 * time.Second
const receiverRetryDelay = 250 * time.Millisecond

var ErrReceiverAuthentication = errors.New("receiver relay authentication failed")

// ReceiverJoinFailure retains endpoint authority boundaries. One permanently
// rejected endpoint cannot veto recovery through an independently failed relay.
type ReceiverJoinFailure struct {
	causes         []error
	retryEndpoints []string
}

func (failure *ReceiverJoinFailure) Error() string   { return errors.Join(failure.causes...).Error() }
func (failure *ReceiverJoinFailure) Unwrap() []error { return append([]error(nil), failure.causes...) }
func (failure *ReceiverJoinFailure) RetryEndpoints() []string {
	return append([]string(nil), failure.retryEndpoints...)
}

func receiverEndpointRetryable(err error) bool {
	var rejection *relayv2.RelayError
	return !errors.Is(err, ErrReceiverAuthentication) && !errors.Is(err, relayv2.ErrProtocol) &&
		(!errors.As(err, &rejection) || rejection.Code != v2.ErrorStopped)
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
	ctx       context.Context
	cancel    context.CancelFunc
	config    ReceiverConfig
	shareID   v2.ShareID
	mu        sync.Mutex
	runtime   *sessionruntime.ReceiverRuntime
	prepared  *liveshare.PreparedReceiver
	first     *relayv2.ReceiverConnection
	remaining int
	failure   error
	ready     chan struct{}
	workers   sync.WaitGroup
	closeOnce sync.Once
}

func NewReceiver(ctx context.Context, config ReceiverConfig) (*Receiver, error) {
	if ctx == nil || len(config.Receiver.Capability.Relays) == 0 || len(config.Receiver.Capability.Relays) > MaximumEndpoints {
		return nil, errors.New("receiver relay set requires bounded endpoints")
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
		receiver.mu.Unlock()
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
	deadline := receiver.config.Clock.Now().Add(receiverRecoveryWindow)
	for receiver.ctx.Err() == nil {
		ctx, cancel := context.WithTimeout(receiver.ctx, receiverAttemptTimeout)
		connection, err := receiver.dial(ctx, url)
		if err == nil && connection == nil {
			err = errors.New("receiver dial returned no connection")
		}
		if err == nil {
			err = receiver.admit(ctx, connection, &lane)
		}
		cancel()
		if initial {
			receiver.finishInitial(url, err)
			initial = false
		}
		if err == nil {
			receiver.waitLane(connection, lane)
			deadline = receiver.config.Clock.Now().Add(receiverRecoveryWindow)
		}
		if connection != nil {
			_ = connection.Close()
		}
		// A fast failed endpoint must not disappear merely because another
		// endpoint is still authenticating the first usable session.
		select {
		case <-receiver.ctx.Done():
			return
		case <-receiver.ready:
		}
		receiver.mu.Lock()
		runtime := receiver.runtime
		receiver.mu.Unlock()
		if runtime == nil || receiver.ctx.Err() != nil || !receiverEndpointRetryable(err) {
			return
		}
		select {
		case <-runtime.Done():
			return
		default:
		}
		if !receiver.config.Clock.Now().Before(deadline) {
			return
		}
		if err := receiver.config.Clock.Wait(receiver.ctx, receiverRetryDelay); err != nil {
			return
		}
	}
}

var closedReceiverLane = func() <-chan struct{} { done := make(chan struct{}); close(done); return done }()

func (receiver *Receiver) admit(ctx context.Context, connection *relayv2.ReceiverConnection, lane *sessionruntime.LaneIdentity) error {
	config := receiver.config.Receiver
	config.DescriptorObject = connection.Descriptor()
	prepared, err := liveshare.PrepareReceiver(config)
	if err != nil {
		return errors.Join(ErrReceiverAuthentication, err)
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

func (receiver *Receiver) dial(ctx context.Context, url string) (*relayv2.ReceiverConnection, error) {
	for {
		connection, err := receiver.config.Dial(ctx, relayv2.ReceiverConfig{RelayBaseURL: url, ShareID: receiver.shareID, Dial: receiver.config.DialOptions})
		var rejection *relayv2.RelayError
		if !errors.As(err, &rejection) || rejection.Code != v2.ErrorStarting {
			return connection, err
		}
		if connection != nil {
			_ = connection.Close()
		}
		delay := rejection.RetryAfter
		if delay <= 0 {
			delay = receiverRetryDelay
		}
		if err = receiver.config.Clock.Wait(ctx, delay); err != nil {
			return nil, err
		}
	}
}

func (receiver *Receiver) connectInitial(ctx context.Context, prepared *liveshare.PreparedReceiver, connection *relayv2.ReceiverConnection, lane *sessionruntime.LaneIdentity) error {
	candidate, connectErr := prepared.Connect(ctx, connection.Channel(), transfer.LaneRouteRelay)
	if connectErr != nil {
		prepared.Close()
		return connectErr
	}
	receiver.mu.Lock()
	if receiver.runtime == nil {
		if receiver.ctx.Err() != nil {
			receiver.mu.Unlock()
			candidate.Close()
			prepared.Close()
			return receiver.ctx.Err()
		}
		receiver.runtime = candidate
		receiver.prepared = prepared
		receiver.first = connection
		lane.ID, lane.Epoch = candidate.LaneIdentity()
		receiver.failure = nil
		close(receiver.ready)
		receiver.mu.Unlock()
		if receiver.config.Connected != nil {
			receiver.config.Connected(connection)
		}
		return nil
	}
	receiver.mu.Unlock()
	candidate.Close()
	prepared.Close()
	// A completed loser owns a distinct transcript. Its channel cannot be
	// reassigned to the winning transcript, so the endpoint redials before attach.
	return errors.New("initial relay handshake superseded")
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
