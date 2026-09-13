package senderrelay

import (
	"context"
	"errors"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
	"time"
)

const (
	senderRelayRecoveryWindow = 55 * time.Second
	senderRelayRetryInitial   = 100 * time.Millisecond
	senderRelayRetryMaximum   = time.Second
	senderRelaySlowRetry      = 30 * time.Second
	senderRelayAttemptTimeout = 10 * time.Second
)

type Clock interface {
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

func (lifecycle *Lifecycle) recover(ctx context.Context) error {
	lifecycle.recoveryMu.Lock()
	defer lifecycle.recoveryMu.Unlock()
	return lifecycle.recoverOwned(ctx)
}

func (lifecycle *Lifecycle) recoverOwned(callerContext context.Context) error {
	if err := lifecycle.recoveryCause(callerContext, lifecycle.recoveryContext); err != nil {
		return err
	}
	lifecycle.mu.Lock()
	terminal := lifecycle.terminalErr
	lifecycle.mu.Unlock()
	if terminal != nil {
		return terminal
	}
	old, err := lifecycle.detachForRecovery()
	if err != nil {
		return err
	}
	_ = lifecycle.retireConnection(old)
	recoveryContext, cancel := context.WithCancel(lifecycle.recoveryContext)
	stopCallerCancellation := context.AfterFunc(callerContext, cancel)
	defer func() { stopCallerCancellation(); cancel() }()
	deadline := lifecycle.config.Clock.Now().Add(senderRelayRecoveryWindow)
	delay := senderRelayRetryInitial
	attempt := uint32(1)
	slow := false
	waitingAnnounced := false
	for {
		if err := lifecycle.recoveryCause(callerContext, recoveryContext); err != nil {
			return err
		}
		resume := lifecycle.resumeNext
		event := Attempt{ShareInstance: lifecycle.config.Fresh.ShareInstance, Number: attempt, State: AttemptStarted, Generation: lifecycle.generation, Slow: slow, Resume: resume}
		lifecycle.observeRecoveryAttempt(event)
		dialErr := lifecycle.connectAttempt(callerContext, recoveryContext, resume)
		if cause := lifecycle.recoveryCause(callerContext, recoveryContext); cause != nil {
			return cause
		}
		if dialErr == nil {
			event.State, event.Generation = AttemptSucceeded, lifecycle.generation
			lifecycle.observeRecoveryAttempt(event)
			return nil
		}
		event.State, event.Failure = AttemptFailed, dialErr
		lifecycle.resumeNext, event.Terminal = classifyRegistrationFailure(resume, dialErr)
		lifecycle.observeRecoveryAttempt(event)
		if event.Terminal {
			lifecycle.mu.Lock()
			lifecycle.terminalErr = dialErr
			lifecycle.mu.Unlock()
			return dialErr
		}
		if !lifecycle.config.Clock.Now().Add(delay).Before(deadline) {
			slow = true
		}
		wait := lifecycle.retryDelay(delay, slow, dialErr)
		if slow && !waitingAnnounced {
			event.State, event.Failure, event.Slow, event.NextDelay = AttemptWaiting, nil, true, wait
			lifecycle.observeRecoveryAttempt(event)
			waitingAnnounced = true
		}
		if err := lifecycle.wait(recoveryContext, wait); err != nil {
			if cause := lifecycle.recoveryCause(callerContext, recoveryContext); cause != nil {
				return cause
			}
			return err
		}
		delay = min(delay*2, senderRelayRetryMaximum)
		if attempt != ^uint32(0) {
			attempt++
		}
	}
}

func (lifecycle *Lifecycle) wait(ctx context.Context, delay time.Duration) error {
	waitContext, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-lifecycle.wake:
			cancel()
		case <-waitContext.Done():
		}
	}()
	err := lifecycle.config.Clock.Wait(waitContext, delay)
	cancel()
	<-done
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// connectAttempt contains ownership arbitration around one bounded handshake.
// A success observed after cancellation is retired before it can advertise readiness.
func (lifecycle *Lifecycle) connectAttempt(caller, recovery context.Context, resume bool) error {
	init := lifecycle.config.Fresh
	var descriptor []byte
	if resume {
		init = lifecycle.resume
	} else {
		descriptor = lifecycle.config.Descriptor
	}
	dialContext, cancel := context.WithTimeout(recovery, senderRelayAttemptTimeout)
	lifecycle.resumeNext, lifecycle.attempted = true, true
	connection, err := lifecycle.config.Dialer.Dial(dialContext, relayv2.SenderConfig{
		RelayBaseURL: lifecycle.config.RelayURL, Init: init, SenderPrivateKey: lifecycle.config.PrivateKey,
		ResumeToken: lifecycle.config.ResumeToken, Descriptor: descriptor,
		Dial: relayv2.DialOptions{LifecycleObservationCapacity: lifecycle.config.LifecycleObservationCapacity},
	})
	if err == nil {
		err = dialContext.Err()
	}
	cancel()
	connection = lifecycle.trackObservationConnection(connection)
	if cause := lifecycle.recoveryCause(caller, recovery); cause != nil {
		err = cause
	}
	if err == nil && !connection.valid() {
		err = relayv2.ErrProtocol
	}
	if err == nil {
		err = lifecycle.installRecovered(connection, caller, recovery)
	}
	if err != nil {
		_ = lifecycle.retireConnection(connection)
	}
	return err
}

func classifyRegistrationFailure(resume bool, failure error) (resumeNext, terminal bool) {
	var relayError *relayv2.RelayError
	if !errors.As(failure, &relayError) {
		return true, errors.Is(failure, relayv2.ErrProtocol)
	}
	switch relayError.Code {
	case v2.ErrorNotFound:
		// Only an authenticated absent RESUME authorizes reuse of the fresh publication.
		return !resume, false
	case v2.ErrorMalformed, v2.ErrorUnsupportedMode, v2.ErrorShareIDCollision,
		v2.ErrorInvalidProof, v2.ErrorDescriptorInvalid, v2.ErrorStopped:
		return true, true
	default:
		// This includes uncertain REGISTER acknowledgements / AlreadyRegistered,
		// admission limits, Starting, expired challenges and competing resume claims.
		return true, false
	}
}

func (lifecycle *Lifecycle) retryDelay(fast time.Duration, slow bool, failure error) time.Duration {
	delay := fast
	if slow {
		delay = lifecycle.config.Jitter(senderRelaySlowRetry)
		// Bound injected jitter as well, so scheduler defects cannot cause a hot loop.
		delay = max(senderRelaySlowRetry/2, min(delay, senderRelaySlowRetry*3/2))
	}
	var relayError *relayv2.RelayError
	if errors.As(failure, &relayError) && relayError.RetryAfter > delay {
		delay = min(relayError.RetryAfter, senderRelaySlowRetry*2)
	}
	return delay
}
