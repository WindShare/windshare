package relayset

import (
	"context"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	"github.com/windshare/windshare/core/framechannel"

	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/protocolsession"

	"github.com/windshare/windshare/core/session"
	receiversession "github.com/windshare/windshare/core/session/receiversession"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
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
		protocolsession.ErrLaneSignature, protocolsession.ErrEnvelopeAuthentication} {
		if errors.Is(err, terminal) {
			return false
		}
	}
	var rejection *relayv2.RelayError
	if !errors.As(err, &rejection) {
		return true
	}
	switch rejection.Code {
	case v2.ErrorNotFound, v2.ErrorStarting, v2.ErrorAdmission, v2.ErrorAccessUnavailable:
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
	recovery             *ReceiverRecovery
	incompleteCandidates bool
	Clock                ReceiverClock
	Receiver             liveshare.ReceiverConfig
	Dial                 func(context.Context, relayv2.ReceiverConfig) (ReceiverConnection, error)
	DialOptions          relayv2.DialOptions
	Connected            func(ReceiverConnection)
	ResolveEndpoints     func(context.Context) ([]string, error)
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
	runtime          *receiversession.Runtime
	prepared         *liveshare.PreparedReceiver
	first            ReceiverConnection
	remaining        int
	failure          error
	ready            chan struct{}
	workers          sync.WaitGroup
	closeOnce        sync.Once
	stopRuntimeWatch func() bool
	candidates       []string
	incomplete       bool
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
	if err := validateReceiverCandidates(config); err != nil {
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
		config.Dial = DialReceiver
	}
	if config.recovery == nil {
		config.recovery, _ = NewReceiverRecovery(ReceiverRecoveryOptions{Clock: config.Clock})
	}
	candidates := config.Receiver.Capability.Relays
	eligible, err := config.recovery.endpoints(candidates)
	if err != nil && config.ResolveEndpoints == nil {
		if !config.incompleteCandidates && config.recovery.allRelaysStopped(candidates) {
			err = errors.Join(ErrReceiverShareStopped, err)
		}
		return nil, err
	}
	lifetime, cancel := context.WithCancel(ctx)
	receiver := &Receiver{ctx: lifetime, cancel: cancel, config: config, shareID: share, remaining: len(eligible), ready: make(chan struct{})}
	receiver.candidates = append([]string(nil), candidates...)
	receiver.incomplete = config.ResolveEndpoints != nil || config.incompleteCandidates
	if config.ResolveEndpoints != nil {
		receiver.remaining++
		receiver.workers.Add(1)
		go receiver.expandCandidates()
	}
	for _, url := range eligible {
		receiver.workers.Add(1)
		go receiver.run(url)
	}
	return receiver, nil
}

func (receiver *Receiver) WaitReady(ctx context.Context) (*receiversession.Runtime, ReceiverConnection, error) {
	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case <-receiver.ready:
	}
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	return receiver.runtime, receiver.first, receiver.failure
}

// AllRelaysStopped requires an explicit stop from every route in the link's
// resolved candidate set. Missing directory results and temporary failures
// leave open the possibility of joining the same share again.
func (receiver *Receiver) AllRelaysStopped() bool {
	candidates, incomplete := receiver.candidateSnapshot()
	return !incomplete && receiver.config.recovery.allRelaysStopped(candidates)
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

type admittedLane struct {
	identity session.LaneIdentity
	done     <-chan struct{}
}

func (receiver *Receiver) run(url string) {
	defer receiver.workers.Done()
	initial := true
	var lane admittedLane
	recovery := receiver.config.recovery
	started := receiver.config.Clock.Now()
	for failures := uint32(0); receiver.attemptError(receiver.ctx) == nil; failures++ {
		if !initial {
			if err := receiver.waitForDemand(receiver.ctx); err != nil {
				return
			}
		}
		attempt, connection, err := receiver.tryEndpoint(url, &lane)
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
			receiver.waitLane(lane)
			err = retiredLaneError(connection)
			recovery.recordFailure(url, err)
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
		if runtimeEnding(runtime) {
			return
		}
		delay, phase := recovery.localRetryDelay(started, failures, err)
		delay = recovery.scheduleEndpointRetry(url, phase, delay, runtime, err)
		if err := receiver.config.Clock.Wait(receiver.ctx, delay); err != nil {
			return
		}
	}
}

func (receiver *Receiver) tryEndpoint(url string, lane *admittedLane) (uint32, ReceiverConnection, error) {
	if err := receiver.attemptError(receiver.ctx); err != nil {
		return 0, nil, err
	}
	recovery := receiver.config.recovery
	attempt, err := recovery.begin(url)
	if err != nil {
		// Completing this initial slot lets another endpoint retry while this one waits.
		return attempt, nil, err
	}
	recovery.observe(url, attempt, ReceiverRecoveryConnecting, 0, receiver.current(), nil)
	ctx, cancel := recovery.options.TimeoutContext(receiver.ctx, receiverAttemptTimeout)
	connection, err := receiver.connectAttempt(ctx, url, lane)
	cancel()
	recovery.recordFailure(url, err)
	return attempt, connection, err
}

func retiredLaneError(connection ReceiverConnection) error {
	if err := connection.Err(); err != nil {
		return err
	}
	if connection.Channel().State() == framechannel.Closed {
		return framechannel.WithCapacityWait(connection.Channel(), errors.New("relay lane retired during capacity wait"))
	}
	return nil
}

func (receiver *Receiver) current() *receiversession.Runtime {
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	return receiver.runtime
}

var closedReceiverLane = func() <-chan struct{} { done := make(chan struct{}); close(done); return done }()

func (receiver *Receiver) admit(ctx context.Context, connection ReceiverConnection, lane *admittedLane) error {
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
	// A lane number is a recyclable session slot, not this endpoint's identity.
	// Another route may have occupied the old slot while the relay was offline.
	grant, err := runtime.RequestLane(ctx, 0)
	if err != nil {
		return err
	}
	channel := connection.Channel()
	_, err = runtime.AttachLane(ctx, grant, channel, transferlanes.RouteRelay)
	err = framechannel.ClassifyDeadline(channel, err)
	if err == nil {
		lane.identity = session.LaneIdentity{ID: grant.LaneID, Epoch: grant.LaneEpoch}
		if done, live := runtime.LaneDone(lane.identity); live {
			lane.done = done
		} else {
			lane.done = closedReceiverLane
		}
	}
	if err == nil && receiver.config.Connected != nil {
		receiver.config.Connected(connection)
	}
	return err
}

// runtimeEnding closes the publication window between removing the last lane
// (which claims TerminationPathsExhausted under lanes.mu) and canceling
// runtime.Lifetime after the detach hook completes.
func runtimeEnding(runtime *receiversession.Runtime) bool {
	return runtime == nil || runtime.Stopping() || runtime.PathsExhausted()
}

func (receiver *Receiver) attemptError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if runtime := receiver.current(); runtime != nil && runtimeEnding(runtime) {
		return session.ErrRuntimeClosed
	}
	return nil
}

func (receiver *Receiver) connectAttempt(ctx context.Context, url string, lane *admittedLane) (ReceiverConnection, error) {
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

func (receiver *Receiver) dial(ctx context.Context, url string) (ReceiverConnection, error) {
	return receiver.config.Dial(ctx, relayv2.ReceiverConfig{RelayBaseURL: url, ShareID: receiver.shareID, Dial: receiver.config.DialOptions})
}

func (receiver *Receiver) connectInitial(ctx context.Context, prepared *liveshare.PreparedReceiver, connection ReceiverConnection, lane *admittedLane) error {
	channel := connection.Channel()
	candidate, connectErr := prepared.Connect(ctx, channel, transferlanes.RouteRelay)
	if connectErr != nil {
		prepared.Close()
		return framechannel.ClassifyDeadline(channel, connectErr)
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

func (receiver *Receiver) publishInitial(ctx context.Context, candidate *receiversession.Runtime, prepared *liveshare.PreparedReceiver, connection ReceiverConnection, lane *admittedLane) error {
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
	var identity session.LaneIdentity
	identity.ID, identity.Epoch = candidate.LaneIdentity()
	done, live := candidate.LaneDone(identity)
	if !live || runtimeEnding(candidate) {
		return errors.Join(session.ErrRuntimeClosed, candidate.Err())
	}
	// Relay waits and dials belong to this session's live authority. Done is a
	// later cleanup boundary and cannot authorize retries during finalization.
	receiver.stopRuntimeWatch = context.AfterFunc(candidate.Lifetime(), receiver.cancel)
	receiver.runtime = candidate
	receiver.prepared = prepared
	receiver.first = connection
	lane.identity = identity
	lane.done = done
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
	if err != nil {
		failure.causes = append(failure.causes, err)
	}
	if url != "" && receiverEndpointRetryable(err) {
		failure.retryEndpoints = append(failure.retryEndpoints, url)
	}
	if receiver.remaining == 0 {
		close(receiver.ready)
	}
}

// Wait for sessioncore to retire this lane rather than racing connection.Done.
// Transport shutdown always closes the lane channel, and retiring the last lane
// cancels runtime.Lifetime before closing lane.done; waking earlier on
// connection.Done can discard buffered terminal frames or schedule an
// in-session retry against a dying generation.
func (receiver *Receiver) waitLane(lane admittedLane) {
	runtime := receiver.current()
	done := lane.done
	if done == nil {
		done = closedReceiverLane
	}
	var lifetimeDone <-chan struct{}
	if lifetime := runtime.Lifetime(); lifetime != nil {
		lifetimeDone = lifetime.Done()
	}
	select {
	case <-receiver.ctx.Done():
	case <-lifetimeDone:
	case <-runtime.Done():
	case <-done:
	}
}
