package relayset

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	mathrand "math/rand/v2"
	"slices"
	"strconv"
	"sync"
	"time"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

type senderRelayRecoveryStoppedError struct{}

func (senderRelayRecoveryStoppedError) Error() string { return "sender relay recovery stopped" }

var ErrStopped = senderRelayRecoveryStoppedError{}

type SenderLifecycleConfig struct {
	Candidate                    Candidate
	Fresh                        v2.RegisterInit
	ResumeToken                  v2.ResumeToken
	PrivateKey                   ed25519.PrivateKey
	Descriptor                   []byte
	Initial                      SenderTransport
	Dialer                       SenderDialer
	Clock                        Clock
	LifecycleObservationCapacity int
	ObserveConnection            func(SenderConnection) func()
	ObserveAttempt               func(Attempt)
	Attempts                     AttemptControl
	// Jitter is applied only in the slow phase, keeping short outages responsive.
	Jitter func(time.Duration) time.Duration
}

type AttemptState uint8

const (
	AttemptStarted AttemptState = iota + 1
	AttemptSucceeded
	AttemptFailed
	AttemptWaiting
	AttemptParked
	AttemptStage
	AttemptAuthorization
)

// DiagnosticName belongs to the source vocabulary; recorders accept new values.
func (value AttemptState) DiagnosticName() string {
	switch value {
	case AttemptStarted:
		return "started"
	case AttemptSucceeded:
		return "succeeded"
	case AttemptFailed:
		return "failed"
	case AttemptWaiting:
		return "waiting"
	case AttemptParked:
		return "parked"
	case AttemptStage:
		return "stage"
	case AttemptAuthorization:
		return "authorization"
	default:
		return "unknown_" + strconv.FormatUint(uint64(value), 10)
	}
}

type Attempt struct {
	ShareInstance     v2.ShareInstance
	Number            uint32
	State             AttemptState
	Failure           error
	Generation        uint64
	Slow              bool
	Resume            bool
	NextDelay         time.Duration
	ServerRetryAt     time.Time
	HandshakeDuration time.Duration
	Terminal          bool
	Duration          time.Duration
	Stage             string
	Authorization     AuthorizationState
}

type RegistrationState uint8

const (
	RegistrationPending RegistrationState = iota
	RegistrationNeedsUserAuthorization
	RegistrationRetrying
	RegistrationAvailable
)

// SenderLifecycle owns one relay's registration for the lifetime of a share.
type SenderLifecycle struct {
	mu         sync.Mutex
	recoveryMu sync.Mutex
	acceptMu   sync.Mutex

	config              SenderLifecycleConfig
	resume              v2.RegisterInit
	stopID              v2.StopID
	connection          SenderConnection
	recoveryContext     context.Context
	cancelRecovery      context.CancelFunc
	stopping            bool
	cleanupOnce         sync.Once
	cleanupErr          error
	retiredCompletion   relayv2.LifecycleObservationCompletion
	serverRetryAt       time.Time // owned by recoveryMu across callers
	resumeNext          bool
	terminalErr         error
	generation          uint64
	wake                chan struct{}
	registration        RegistrationState
	observeRegistration func(RegistrationState)
	mayHaveRegistered   bool
}

func NewSenderLifecycle(config SenderLifecycleConfig) (*SenderLifecycle, error) {
	resume, err := relayv2.ResumeInit(config.Fresh)
	if err != nil || config.Candidate.Validate() != nil || len(config.PrivateKey) != ed25519.PrivateKeySize ||
		len(config.Descriptor) == 0 || len(config.Descriptor) > v2.MaxDescriptorBytes ||
		sha256.Sum256(config.Descriptor) != config.Fresh.DescriptorDigest || sha256.Sum256(config.ResumeToken[:]) != config.Fresh.ResumeTokenHash {
		return nil, errors.Join(relayv2.ErrProtocol, err)
	}
	// Recovery owns retry policy, while dialing and time are consumer-side
	// boundaries so stop races and budgets are testable without weakening limits.
	if config.Dialer == nil {
		config.Dialer = relayV2SenderDialer{}
	}
	if config.Clock == nil {
		config.Clock = SystemClock{}
	}
	if config.Jitter == nil {
		config.Jitter = func(delay time.Duration) time.Duration {
			return delay*3/4 + time.Duration(mathrand.Int64N(int64(delay/2)))
		}
	}
	// Registration uncertainty must never regenerate a descriptor or recovery token.
	config.Descriptor = slices.Clone(config.Descriptor)
	config.PrivateKey = slices.Clone(config.PrivateKey)
	var stopID v2.StopID
	if _, err := rand.Read(stopID[:]); err != nil {
		return nil, err
	}
	connection := NewSenderConnection(config.Initial)
	// Bootstrap ownership moves into connection; clearing the source reference
	// lets a failed initial WebSocket become collectible immediately after detach.
	config.Initial = nil
	recoveryContext, cancel := context.WithCancel(context.Background())
	lifecycle := &SenderLifecycle{
		config:            config,
		resume:            resume,
		stopID:            stopID,
		connection:        connection,
		recoveryContext:   recoveryContext,
		cancelRecovery:    cancel,
		resumeNext:        connection.valid(),
		mayHaveRegistered: connection.valid(),
		wake:              make(chan struct{}, 1),
	}
	lifecycle.connection = lifecycle.trackObservationConnection(connection)
	if connection.valid() {
		lifecycle.generation = 1
		lifecycle.registration = RegistrationAvailable
	}
	return lifecycle, nil
}

func (lifecycle *SenderLifecycle) Accept(ctx context.Context) (*relayv2.Channel, error) {
	lifecycle.acceptMu.Lock()
	defer lifecycle.acceptMu.Unlock()
	if err := lifecycle.WaitReady(ctx); err != nil {
		return nil, err
	}
	for {
		connection, err := lifecycle.current()
		if err != nil {
			return nil, err
		}
		channel, err := connection.Accept(ctx)
		if err == nil {
			return channel, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err := lifecycle.recover(ctx); err != nil {
			return nil, err
		}
	}
}

func (lifecycle *SenderLifecycle) current() (SenderConnection, error) {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.stopping {
		return SenderConnection{}, ErrStopped
	}
	if !lifecycle.connection.valid() {
		return SenderConnection{}, relayv2.ErrClosed
	}
	return lifecycle.connection, nil
}

// WaitReady waits through temporary startup failures. Only authenticated readiness
// makes a link publishable; merely constructing a lifecycle does not.
func (lifecycle *SenderLifecycle) WaitReady(ctx context.Context) error {
	lifecycle.recoveryMu.Lock()
	defer lifecycle.recoveryMu.Unlock()
	if _, err := lifecycle.current(); err == nil {
		return nil
	}
	return lifecycle.recoverOwned(ctx)
}

// ObserveRegistration provides the current route state even when diagnostic
// observations are disabled or dropped. The callback must not reenter SenderLifecycle.
func (lifecycle *SenderLifecycle) ObserveRegistration(observer func(RegistrationState)) {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	lifecycle.observeRegistration = observer
	if observer != nil {
		observer(lifecycle.registration)
	}
}

func (lifecycle *SenderLifecycle) setRegistrationLocked(state RegistrationState) {
	if lifecycle.registration == state {
		return
	}
	lifecycle.registration = state
	if lifecycle.observeRegistration != nil {
		lifecycle.observeRegistration(state)
	}
}

func (lifecycle *SenderLifecycle) showAuthorizationWait(required bool) {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.stopping {
		return
	}
	state := RegistrationPending
	if required {
		state = RegistrationNeedsUserAuthorization
	}
	lifecycle.setRegistrationLocked(state)
}

// Wake coalesces network/resume notifications into the single recovery owner.
func (lifecycle *SenderLifecycle) Wake() {
	select {
	case lifecycle.wake <- struct{}{}:
	default:
	}
}

func (lifecycle *SenderLifecycle) observeRecoveryAttempt(observation Attempt) {
	if lifecycle != nil && lifecycle.config.ObserveAttempt != nil {
		lifecycle.config.ObserveAttempt(observation)
	}
}

func (lifecycle *SenderLifecycle) detachForRecovery() (SenderConnection, error) {
	if lifecycle.config.Attempts != nil {
		lifecycle.config.Attempts.Unavailable()
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.stopping {
		return SenderConnection{}, ErrStopped
	}
	old := lifecycle.connection
	lifecycle.connection = SenderConnection{}
	lifecycle.setRegistrationLocked(RegistrationPending)
	return old, nil
}

func (lifecycle *SenderLifecycle) installRecovered(
	connection SenderConnection,
	callerContext context.Context,
	recoveryContext context.Context,
) error {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.stopping {
		return ErrStopped
	}
	if err := callerContext.Err(); err != nil {
		return err
	}
	if err := recoveryContext.Err(); err != nil {
		return err
	}
	lifecycle.connection = connection
	lifecycle.generation++
	lifecycle.setRegistrationLocked(RegistrationAvailable)
	return nil
}

func (lifecycle *SenderLifecycle) recoveryCause(
	callerContext context.Context,
	recoveryContext context.Context,
) error {
	lifecycle.mu.Lock()
	stopping := lifecycle.stopping
	lifecycle.mu.Unlock()
	if stopping {
		return ErrStopped
	}
	callerErr := callerContext.Err()
	recoveryErr := recoveryContext.Err()
	if callerErr == nil && recoveryErr == nil {
		return nil
	}
	// Stop cancels the recovery context too. Rechecking after observing a cause
	// gives explicit lifecycle shutdown precedence over its cancellation echo.
	lifecycle.mu.Lock()
	stopping = lifecycle.stopping
	lifecycle.mu.Unlock()
	if stopping {
		return ErrStopped
	}
	if callerErr != nil {
		return callerErr
	}
	return recoveryErr
}

func (lifecycle *SenderLifecycle) StopRecovery() {
	lifecycle.mu.Lock()
	if !lifecycle.stopping {
		lifecycle.stopping = true
		lifecycle.cancelRecovery()
		lifecycle.setRegistrationLocked(RegistrationPending)
	}
	lifecycle.mu.Unlock()
	if lifecycle.config.Attempts != nil {
		lifecycle.config.Attempts.Unavailable()
	}
}

func (lifecycle *SenderLifecycle) Cleanup(ctx context.Context) error {
	lifecycle.cleanupOnce.Do(func() {
		lifecycle.StopRecovery()
		lifecycle.recoveryMu.Lock()
		defer lifecycle.recoveryMu.Unlock()
		var stopErr error
		if lifecycle.mayHaveRegistered {
			stopErr = relayv2.Stop(ctx, relayv2.StopConfig{
				RelayBaseURL:     lifecycle.config.Candidate.PublicURL,
				ShareID:          lifecycle.config.Fresh.ShareID,
				ShareInstance:    lifecycle.config.Fresh.ShareInstance,
				PKHash:           lifecycle.config.Fresh.PKHash,
				StopID:           lifecycle.stopID,
				SenderPrivateKey: lifecycle.config.PrivateKey,
			})
		}
		lifecycle.mu.Lock()
		connection := lifecycle.connection
		lifecycle.connection = SenderConnection{}
		lifecycle.mu.Unlock()
		closeErr := lifecycle.retireConnection(connection)
		lifecycle.cleanupErr = errors.Join(stopErr, closeErr)
	})
	return lifecycle.cleanupErr
}

func (lifecycle *SenderLifecycle) trackObservationConnection(connection SenderConnection) SenderConnection {
	if lifecycle != nil && connection.valid() && lifecycle.config.ObserveConnection != nil {
		connection.finishObservations = lifecycle.config.ObserveConnection(connection)
	}
	return connection
}

func (lifecycle *SenderLifecycle) retireConnection(connection SenderConnection) error {
	closeErr := connection.Close()
	// Close can encounter a transport already retiring on another goroutine.
	// Its Done follows the terminal observation, so never cut diagnostics early.
	if terminal, ok := connection.endpoint.(interface{ Done() <-chan struct{} }); ok {
		<-terminal.Done()
	}
	completion := connection.CompleteObservations()
	if connection.finishObservations != nil {
		connection.finishObservations()
	}
	lifecycle.mu.Lock()
	mergeRelayCompletion(&lifecycle.retiredCompletion, completion)
	lifecycle.mu.Unlock()
	return closeErr
}

func (lifecycle *SenderLifecycle) CompleteObservations() relayv2.LifecycleObservationCompletion {
	if lifecycle == nil {
		return relayv2.LifecycleObservationCompletion{}
	}
	// The command joins recovery and cleanup before its final observation cut.
	// Retired transports have already contributed immutable counters and no
	// longer need to remain reachable from this lifecycle.
	lifecycle.mu.Lock()
	completion, connection := lifecycle.retiredCompletion, lifecycle.connection
	lifecycle.mu.Unlock()
	mergeRelayCompletion(&completion, connection.CompleteObservations())
	return completion
}

func mergeRelayCompletion(total *relayv2.LifecycleObservationCompletion, next relayv2.LifecycleObservationCompletion) {
	total.Enqueued = saturatingAdd(total.Enqueued, next.Enqueued)
	total.Loss.CapacityDropped = saturatingAdd(total.Loss.CapacityDropped, next.Loss.CapacityDropped)
}

func saturatingAdd(a, b uint64) uint64 {
	if ^uint64(0)-a < b {
		return ^uint64(0)
	}
	return a + b
}
