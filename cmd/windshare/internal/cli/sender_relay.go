package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"sync"
	"time"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

const (
	senderRelayRecoveryWindow = 55 * time.Second
	senderRelayRetryInitial   = 100 * time.Millisecond
	senderRelayRetryMaximum   = time.Second
)

type senderRelayRecoveryStoppedError struct{}

func (senderRelayRecoveryStoppedError) Error() string { return "sender relay recovery stopped" }

// shareServeStopCause lets the orchestration layer recognize its own monotonic
// stop transition without making lifecycle policy depend on provider error text.
func (senderRelayRecoveryStoppedError) shareServeStopCause() {}

var errSenderRelayRecoveryStopped = senderRelayRecoveryStoppedError{}

// senderRelayEndpoint is defined at the lifecycle that consumes it so recovery
// policy does not depend on the concrete WebSocket transport.
type senderRelayEndpoint interface {
	Accept(context.Context) (*relayv2.Channel, error)
	Close() error
}

// senderRelayConnection keeps factories concrete while containing the narrow
// transport interface at the consumer boundary.
type senderRelayConnection struct {
	endpoint senderRelayEndpoint
}

func newSenderRelayConnection(endpoint senderRelayEndpoint) senderRelayConnection {
	return senderRelayConnection{endpoint: endpoint}
}

func (connection senderRelayConnection) valid() bool {
	return connection.endpoint != nil
}

func (connection senderRelayConnection) Accept(ctx context.Context) (*relayv2.Channel, error) {
	return connection.endpoint.Accept(ctx)
}

func (connection senderRelayConnection) Close() error {
	if !connection.valid() {
		return nil
	}
	return connection.endpoint.Close()
}

type senderRelayDialer interface {
	Dial(context.Context, relayv2.SenderConfig) (senderRelayConnection, error)
}

type relayV2SenderDialer struct{}

func (relayV2SenderDialer) Dial(
	ctx context.Context,
	config relayv2.SenderConfig,
) (senderRelayConnection, error) {
	connection, err := relayv2.DialSender(ctx, config)
	if err != nil {
		return senderRelayConnection{}, err
	}
	return newSenderRelayConnection(connection), nil
}

type senderRelayRecoveryClock interface {
	Now() time.Time
	Wait(context.Context, time.Duration) error
}

type wallSenderRelayRecoveryClock struct{}

func (wallSenderRelayRecoveryClock) Now() time.Time { return time.Now() }

func (wallSenderRelayRecoveryClock) Wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type senderRelayLifecycleConfig struct {
	relayURL    string
	fresh       v2.RegisterInit
	resumeToken v2.ResumeToken
	privateKey  ed25519.PrivateKey
	initial     senderRelayEndpoint
	dialer      senderRelayDialer
	clock       senderRelayRecoveryClock
}

type senderRelayLifecycle struct {
	mu sync.Mutex

	config          senderRelayLifecycleConfig
	resume          v2.RegisterInit
	stopID          v2.StopID
	connection      senderRelayConnection
	recoveryContext context.Context
	cancelRecovery  context.CancelFunc
	stopping        bool
	cleanupOnce     sync.Once
	cleanupErr      error
}

func newSenderRelayLifecycle(config senderRelayLifecycleConfig) (*senderRelayLifecycle, error) {
	resume, err := relayv2.ResumeInit(config.fresh)
	if err != nil || config.initial == nil || config.relayURL == "" || len(config.privateKey) != ed25519.PrivateKeySize {
		return nil, errors.Join(relayv2.ErrProtocol, err)
	}
	// Recovery owns retry policy, while dialing and time are consumer-side
	// boundaries so stop races and budgets are testable without weakening limits.
	if config.dialer == nil {
		config.dialer = relayV2SenderDialer{}
	}
	if config.clock == nil {
		config.clock = wallSenderRelayRecoveryClock{}
	}
	var stopID v2.StopID
	if _, err := rand.Read(stopID[:]); err != nil {
		return nil, err
	}
	connection := newSenderRelayConnection(config.initial)
	// Bootstrap ownership moves into connection; clearing the source reference
	// lets a failed initial WebSocket become collectible immediately after detach.
	config.initial = nil
	recoveryContext, cancel := context.WithCancel(context.Background())
	return &senderRelayLifecycle{
		config:          config,
		resume:          resume,
		stopID:          stopID,
		connection:      connection,
		recoveryContext: recoveryContext,
		cancelRecovery:  cancel,
	}, nil
}

func (lifecycle *senderRelayLifecycle) Accept(ctx context.Context) (*relayv2.Channel, error) {
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

func (lifecycle *senderRelayLifecycle) current() (senderRelayConnection, error) {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.stopping {
		return senderRelayConnection{}, errSenderRelayRecoveryStopped
	}
	if !lifecycle.connection.valid() {
		return senderRelayConnection{}, relayv2.ErrClosed
	}
	return lifecycle.connection, nil
}

func (lifecycle *senderRelayLifecycle) recover(callerContext context.Context) error {
	old, err := lifecycle.detachForRecovery()
	if err != nil {
		return err
	}
	// Detaching before Close prevents retries and cleanup from repeatedly owning
	// the failed transport while a replacement is being established.
	_ = old.Close()

	recoveryContext, cancel := context.WithTimeout(
		lifecycle.recoveryContext,
		senderRelayRecoveryWindow,
	)
	stopCallerCancellation := context.AfterFunc(callerContext, cancel)
	defer func() {
		stopCallerCancellation()
		cancel()
	}()
	deadline := lifecycle.config.clock.Now().Add(senderRelayRecoveryWindow)
	delay := senderRelayRetryInitial

	for {
		if err := lifecycle.recoveryCause(callerContext, recoveryContext); err != nil {
			return err
		}
		connection, dialErr := lifecycle.config.dialer.Dial(recoveryContext, relayv2.SenderConfig{
			RelayBaseURL:     lifecycle.config.relayURL,
			Init:             lifecycle.resume,
			SenderPrivateKey: lifecycle.config.privateKey,
			ResumeToken:      lifecycle.config.resumeToken,
		})
		if dialErr == nil {
			if err := lifecycle.recoveryCause(callerContext, recoveryContext); err != nil {
				_ = connection.Close()
				return err
			}
			if !connection.valid() {
				return relayv2.ErrProtocol
			}
			if err := lifecycle.installRecovered(connection, callerContext, recoveryContext); err != nil {
				// A dial can win concurrently with cancellation or explicit stop.
				// Closing the uninstalled result keeps route ownership leak-free.
				_ = connection.Close()
				return err
			}
			return nil
		}
		if err := lifecycle.recoveryCause(callerContext, recoveryContext); err != nil {
			return errors.Join(dialErr, err)
		}
		// A retry must start strictly inside the fixed recovery budget; the
		// context deadline independently bounds a dial that blocks.
		if !lifecycle.config.clock.Now().Add(delay).Before(deadline) {
			return dialErr
		}
		if waitErr := lifecycle.config.clock.Wait(recoveryContext, delay); waitErr != nil {
			if err := lifecycle.recoveryCause(callerContext, recoveryContext); err != nil {
				return errors.Join(dialErr, err)
			}
			return errors.Join(dialErr, waitErr)
		}
		delay = min(delay*2, senderRelayRetryMaximum)
	}
}

func (lifecycle *senderRelayLifecycle) detachForRecovery() (senderRelayConnection, error) {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.stopping {
		return senderRelayConnection{}, errSenderRelayRecoveryStopped
	}
	if !lifecycle.connection.valid() {
		return senderRelayConnection{}, relayv2.ErrClosed
	}
	old := lifecycle.connection
	lifecycle.connection = senderRelayConnection{}
	return old, nil
}

func (lifecycle *senderRelayLifecycle) installRecovered(
	connection senderRelayConnection,
	callerContext context.Context,
	recoveryContext context.Context,
) error {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.stopping {
		return errSenderRelayRecoveryStopped
	}
	if err := callerContext.Err(); err != nil {
		return err
	}
	if err := recoveryContext.Err(); err != nil {
		return err
	}
	lifecycle.connection = connection
	return nil
}

func (lifecycle *senderRelayLifecycle) recoveryCause(
	callerContext context.Context,
	recoveryContext context.Context,
) error {
	lifecycle.mu.Lock()
	stopping := lifecycle.stopping
	lifecycle.mu.Unlock()
	if stopping {
		return errSenderRelayRecoveryStopped
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
		return errSenderRelayRecoveryStopped
	}
	if callerErr != nil {
		return callerErr
	}
	return recoveryErr
}

func (lifecycle *senderRelayLifecycle) StopRecovery() {
	lifecycle.mu.Lock()
	if !lifecycle.stopping {
		lifecycle.stopping = true
		lifecycle.cancelRecovery()
	}
	lifecycle.mu.Unlock()
}

func (lifecycle *senderRelayLifecycle) Cleanup(ctx context.Context) error {
	lifecycle.cleanupOnce.Do(func() {
		lifecycle.StopRecovery()
		stopErr := relayv2.Stop(ctx, relayv2.StopConfig{
			RelayBaseURL:     lifecycle.config.relayURL,
			ShareID:          lifecycle.config.fresh.ShareID,
			ShareInstance:    lifecycle.config.fresh.ShareInstance,
			PKHash:           lifecycle.config.fresh.PKHash,
			StopID:           lifecycle.stopID,
			SenderPrivateKey: lifecycle.config.privateKey,
		})
		lifecycle.mu.Lock()
		connection := lifecycle.connection
		lifecycle.connection = senderRelayConnection{}
		lifecycle.mu.Unlock()
		closeErr := connection.Close()
		lifecycle.cleanupErr = errors.Join(stopErr, closeErr)
	})
	return lifecycle.cleanupErr
}
