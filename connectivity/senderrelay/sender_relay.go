// Package senderrelay owns one relay's registration for the lifetime of a share.
package senderrelay

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	mathrand "math/rand/v2"
	"slices"
	"sync"
	"time"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

type senderRelayRecoveryStoppedError struct{}

func (senderRelayRecoveryStoppedError) Error() string { return "sender relay recovery stopped" }

var ErrStopped = senderRelayRecoveryStoppedError{}

type Config struct {
	RelayURL                     string
	Fresh                        v2.RegisterInit
	ResumeToken                  v2.ResumeToken
	PrivateKey                   ed25519.PrivateKey
	Descriptor                   []byte
	Initial                      Endpoint
	Dialer                       Dialer
	Clock                        Clock
	LifecycleObservationCapacity int
	ObserveConnection            func(Connection) func()
	ObserveAttempt               func(Attempt)
	// Jitter is applied only in the slow phase, keeping short outages responsive.
	Jitter func(time.Duration) time.Duration
}

type AttemptState uint8

const (
	AttemptStarted AttemptState = iota + 1
	AttemptSucceeded
	AttemptFailed
	AttemptWaiting
)

type Attempt struct {
	ShareInstance v2.ShareInstance
	Number        uint32
	State         AttemptState
	Failure       error
	Generation    uint64
	Slow          bool
	Resume        bool
	NextDelay     time.Duration
	Terminal      bool
}

type Lifecycle struct {
	mu         sync.Mutex
	recoveryMu sync.Mutex
	acceptMu   sync.Mutex

	config            Config
	resume            v2.RegisterInit
	stopID            v2.StopID
	connection        Connection
	recoveryContext   context.Context
	cancelRecovery    context.CancelFunc
	stopping          bool
	cleanupOnce       sync.Once
	cleanupErr        error
	retiredCompletion relayv2.LifecycleObservationCompletion
	resumeNext        bool
	terminalErr       error
	generation        uint64
	wake              chan struct{}
	availability      func(bool)
	attempted         bool
}

func New(config Config) (*Lifecycle, error) {
	resume, err := relayv2.ResumeInit(config.Fresh)
	if err != nil || config.RelayURL == "" || len(config.PrivateKey) != ed25519.PrivateKeySize ||
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
		config.Clock = wallSenderRelayRecoveryClock{}
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
	connection := NewConnection(config.Initial)
	// Bootstrap ownership moves into connection; clearing the source reference
	// lets a failed initial WebSocket become collectible immediately after detach.
	config.Initial = nil
	recoveryContext, cancel := context.WithCancel(context.Background())
	lifecycle := &Lifecycle{
		config:          config,
		resume:          resume,
		stopID:          stopID,
		connection:      connection,
		recoveryContext: recoveryContext,
		cancelRecovery:  cancel,
		resumeNext:      connection.valid(),
		attempted:       connection.valid(),
		wake:            make(chan struct{}, 1),
	}
	lifecycle.connection = lifecycle.trackObservationConnection(connection)
	if connection.valid() {
		lifecycle.generation = 1
	}
	return lifecycle, nil
}

func (lifecycle *Lifecycle) Accept(ctx context.Context) (*relayv2.Channel, error) {
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

func (lifecycle *Lifecycle) current() (Connection, error) {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.stopping {
		return Connection{}, ErrStopped
	}
	if !lifecycle.connection.valid() {
		return Connection{}, relayv2.ErrClosed
	}
	return lifecycle.connection, nil
}

// WaitReady waits through temporary startup failures. Only authenticated readiness
// makes a link publishable; merely constructing a lifecycle does not.
func (lifecycle *Lifecycle) WaitReady(ctx context.Context) error {
	lifecycle.recoveryMu.Lock()
	defer lifecycle.recoveryMu.Unlock()
	if _, err := lifecycle.current(); err == nil {
		return nil
	}
	return lifecycle.recoverOwned(ctx)
}

// SetAvailabilityObserver installs the aggregate ingress observer before startup.
// It reports the current state immediately, so a fast initial connection is not lost.
func (lifecycle *Lifecycle) SetAvailabilityObserver(observer func(bool)) {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	lifecycle.availability = observer
	if observer != nil {
		observer(lifecycle.connection.valid() && !lifecycle.stopping)
	}
}

// Wake coalesces network/resume notifications into the single recovery owner.
func (lifecycle *Lifecycle) Wake() {
	select {
	case lifecycle.wake <- struct{}{}:
	default:
	}
}

func (lifecycle *Lifecycle) observeRecoveryAttempt(observation Attempt) {
	if lifecycle != nil && lifecycle.config.ObserveAttempt != nil {
		lifecycle.config.ObserveAttempt(observation)
	}
}

func (lifecycle *Lifecycle) detachForRecovery() (Connection, error) {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.stopping {
		return Connection{}, ErrStopped
	}
	old := lifecycle.connection
	lifecycle.connection = Connection{}
	if lifecycle.availability != nil {
		lifecycle.availability(false)
	}
	return old, nil
}

func (lifecycle *Lifecycle) installRecovered(
	connection Connection,
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
	if lifecycle.availability != nil {
		lifecycle.availability(true)
	}
	return nil
}

func (lifecycle *Lifecycle) recoveryCause(
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

func (lifecycle *Lifecycle) StopRecovery() {
	lifecycle.mu.Lock()
	if !lifecycle.stopping {
		lifecycle.stopping = true
		lifecycle.cancelRecovery()
		if lifecycle.availability != nil {
			lifecycle.availability(false)
		}
	}
	lifecycle.mu.Unlock()
}

func (lifecycle *Lifecycle) Cleanup(ctx context.Context) error {
	lifecycle.cleanupOnce.Do(func() {
		lifecycle.StopRecovery()
		lifecycle.recoveryMu.Lock()
		defer lifecycle.recoveryMu.Unlock()
		var stopErr error
		if lifecycle.attempted {
			stopErr = relayv2.Stop(ctx, relayv2.StopConfig{
				RelayBaseURL:     lifecycle.config.RelayURL,
				ShareID:          lifecycle.config.Fresh.ShareID,
				ShareInstance:    lifecycle.config.Fresh.ShareInstance,
				PKHash:           lifecycle.config.Fresh.PKHash,
				StopID:           lifecycle.stopID,
				SenderPrivateKey: lifecycle.config.PrivateKey,
			})
		}
		lifecycle.mu.Lock()
		connection := lifecycle.connection
		lifecycle.connection = Connection{}
		lifecycle.mu.Unlock()
		closeErr := lifecycle.retireConnection(connection)
		lifecycle.cleanupErr = errors.Join(stopErr, closeErr)
	})
	return lifecycle.cleanupErr
}
