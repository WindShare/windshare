package relayset

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/windshare/windshare/core/serviceauthority"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

const (
	senderRelayRecoveryWindow = 55 * time.Second
	senderRelayRetryInitial   = 100 * time.Millisecond
	senderRelayRetryMaximum   = time.Second
	senderRelaySlowRetry      = 30 * time.Second
	senderRelayAttemptTimeout = 10 * time.Second
)

var errParked = errors.New("network registration parked")

type Clock interface {
	Now() time.Time
	// A wake signal ends local backoff. A nil wake leaves only elapsed time or cancellation.
	Wait(context.Context, time.Duration, <-chan struct{}) error
}

type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

func (SystemClock) Wait(ctx context.Context, delay time.Duration, wake <-chan struct{}) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-wake:
	case <-timer.C:
	}
	return nil
}

func (lifecycle *SenderLifecycle) recover(ctx context.Context) error {
	lifecycle.recoveryMu.Lock()
	defer lifecycle.recoveryMu.Unlock()
	return lifecycle.recoverOwned(ctx)
}

func (lifecycle *SenderLifecycle) recoverOwned(callerContext context.Context) error {
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
	return lifecycle.recoverRegistration(callerContext, recoveryContext)
}

func (lifecycle *SenderLifecycle) recoverRegistration(callerContext, recoveryContext context.Context) error {
	deadline := lifecycle.config.Clock.Now().Add(senderRelayRecoveryWindow)
	delay := senderRelayRetryInitial
	attempt := uint32(1)
	slow := false
	waitingAnnounced := false
	for {
		if err := lifecycle.waitForServerRetry(callerContext, recoveryContext); err != nil {
			return err
		}
		resume := lifecycle.resumeNext
		permit, authorization, err := lifecycle.acquireRegistrationAttempt(recoveryContext, resume, attempt)
		if err != nil {
			return lifecycle.attemptCause(callerContext, recoveryContext, err)
		}
		started := lifecycle.config.Clock.Now()
		event := Attempt{ShareInstance: lifecycle.config.Fresh.ShareInstance, Number: attempt, State: AttemptStarted, Generation: lifecycle.generation, Slow: slow, Resume: resume}
		lifecycle.observeRecoveryAttempt(event)
		dialErr := lifecycle.connectAttempt(callerContext, recoveryContext, resume, permit, &event, authorization)
		if permit != nil {
			permit.Release()
		}
		event.Duration = lifecycle.config.Clock.Now().Sub(started)
		if cause := lifecycle.recoveryCause(callerContext, recoveryContext); cause != nil {
			return cause
		}
		if dialErr == nil {
			lifecycle.serverRetryAt = time.Time{}
			event.State, event.Generation = AttemptSucceeded, lifecycle.generation
			lifecycle.observeRecoveryAttempt(event)
			return nil
		}
		if errors.Is(dialErr, errParked) {
			event.State = AttemptParked
			lifecycle.observeRecoveryAttempt(event)
			if attempt != ^uint32(0) {
				attempt++
			}
			continue
		}
		if lifecycle.registrationFailed(&event, dialErr) {
			return dialErr
		}
		lifecycle.mu.Lock()
		if lifecycle.generation == 0 {
			lifecycle.setRegistrationLocked(RegistrationRetrying)
		}
		lifecycle.mu.Unlock()
		now := lifecycle.config.Clock.Now()
		slow = slow || !now.Add(delay).Before(deadline)
		wait := max(lifecycle.retryDelay(delay, slow), event.ServerRetryAt.Sub(now))
		// Announce the actual wait crossing the fast window without delaying this retry further.
		slow = slow || !now.Add(wait).Before(deadline)
		event.NextDelay = wait
		lifecycle.observeRecoveryAttempt(event)
		if slow && !waitingAnnounced {
			event.State, event.Failure, event.Slow, event.NextDelay = AttemptWaiting, nil, true, wait
			lifecycle.observeRecoveryAttempt(event)
			waitingAnnounced = true
		}
		if err := lifecycle.config.Clock.Wait(recoveryContext, wait, lifecycle.wake); err != nil {
			return lifecycle.attemptCause(callerContext, recoveryContext, err)
		}
		delay = min(delay*2, senderRelayRetryMaximum)
		if attempt != ^uint32(0) {
			attempt++
		}
	}
}

func (lifecycle *SenderLifecycle) beginAttempt(ctx context.Context) (AttemptPermit, error) {
	if lifecycle.config.Attempts == nil {
		return nil, nil
	}
	return lifecycle.config.Attempts.Begin(ctx)
}

func (lifecycle *SenderLifecycle) registrationFailed(event *Attempt, failure error) bool {
	event.State, event.Failure = AttemptFailed, failure
	lifecycle.resumeNext, event.Terminal = classifyRegistrationFailure(event.Resume, failure)
	// A previously admitted route can return 404 during a proxy restart. Only
	// an initial, unsent registration can retire this ambiguous candidate.
	if !lifecycle.mayHaveRegistered {
		_, missingRoute := errors.AsType[*relayv2.WebSocketRouteError](failure)
		event.Terminal = event.Terminal || missingRoute
	}
	if event.Terminal {
		lifecycle.observeRecoveryAttempt(*event)
		lifecycle.mu.Lock()
		lifecycle.terminalErr = failure
		lifecycle.mu.Unlock()
	}
	return event.Terminal
}

// A server deadline belongs to the registration, so it survives a local wake
// and cancellation of the caller that first received the refusal.
func (lifecycle *SenderLifecycle) waitForServerRetry(caller, recovery context.Context) error {
	for {
		if err := lifecycle.recoveryCause(caller, recovery); err != nil {
			return err
		}
		remaining := lifecycle.serverRetryAt.Sub(lifecycle.config.Clock.Now())
		if remaining <= 0 {
			return nil
		}
		if err := lifecycle.config.Clock.Wait(recovery, remaining, nil); err != nil {
			return lifecycle.attemptCause(caller, recovery, err)
		}
	}
}

func (lifecycle *SenderLifecycle) attemptCause(caller, recovery context.Context, err error) error {
	if cause := lifecycle.recoveryCause(caller, recovery); cause != nil {
		return cause
	}
	return err
}

// connectAttempt contains ownership arbitration around one bounded handshake.
// A success observed after cancellation is retired before it can advertise readiness.
func (lifecycle *SenderLifecycle) connectAttempt(caller, recovery context.Context, resume bool, permit AttemptPermit, attempt *Attempt, authorization Authorization) error {
	init := lifecycle.config.Fresh
	var descriptor []byte
	if resume {
		init = lifecycle.resume
	} else {
		descriptor = lifecycle.config.Descriptor
	}
	dialContext, cancel := context.WithTimeout(recovery, senderRelayAttemptTimeout)
	dialOptions := registrationDialOptions(lifecycle.config.LifecycleObservationCapacity, resume, authorization)
	stageAttempt := *attempt
	dialOptions.ObserveStage = func(stage relayv2.DialStage) {
		event := stageAttempt
		event.State, event.Stage, event.Duration, event.Failure = AttemptStage, stage.Stage, stage.Duration, stage.Failure
		lifecycle.observeRecoveryAttempt(event)
	}
	started := lifecycle.config.Clock.Now()
	connection, err := lifecycle.config.Dialer.Dial(dialContext, relayv2.SenderConfig{
		RelayBaseURL: lifecycle.config.Candidate.PublicURL, Init: init, SenderPrivateKey: lifecycle.config.PrivateKey,
		ResumeToken: lifecycle.config.ResumeToken, Descriptor: descriptor,
		Dial: dialOptions,
	})
	finished := lifecycle.config.Clock.Now()
	attempt.HandshakeDuration = finished.Sub(started)
	if delay := min(relayv2.RetryAfter(err), 2*senderRelaySlowRetry); delay > 0 {
		if until := finished.Add(delay); until.After(lifecycle.serverRetryAt) {
			lifecycle.serverRetryAt = until
		}
		attempt.ServerRetryAt = lifecycle.serverRetryAt
	}
	// Delivery uncertainty survives later failures: a lost acknowledgement can
	// leave a registration that must be resumed and permanently revoked on stop.
	delivery := serviceauthority.RegistrationNotSent
	if _, notSent := errors.AsType[*relayv2.RegistrationNotSentError](err); !notSent {
		delivery = serviceauthority.RegistrationMayHaveBeenSent
		lifecycle.mayHaveRegistered = true
		lifecycle.resumeNext = true
	}
	if authorization.Reservation != nil && delivery == serviceauthority.RegistrationNotSent {
		slog.DebugContext(recovery, "share_attribution_attempt_unsent",
			"share_instance_id", attempt.ShareInstance, "attempt", attempt.Number, "anonymous", authorization.Bearer == "")
	}
	authorization.Reservation.Finish(delivery)
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
	if err == nil && permit != nil && !permit.Admit() {
		err = errParked
	}
	if err == nil {
		err = lifecycle.installRecovered(connection, caller, recovery)
		if err != nil && lifecycle.config.Attempts != nil {
			lifecycle.config.Attempts.Unavailable()
		}
	}
	if err != nil {
		_ = lifecycle.retireConnection(connection)
	}
	return err
}

func classifyRegistrationFailure(resume bool, failure error) (resumeNext, terminal bool) {
	if _, notSent := errors.AsType[*relayv2.RegistrationNotSentError](failure); notSent {
		// No new registration uncertainty was introduced. Keep the mode selected
		// by earlier attempts, including an authenticated absent RESUME.
		return resume, errors.Is(failure, relayv2.ErrProtocol)
	}
	var relayError *relayv2.RelayError
	if !errors.As(failure, &relayError) {
		return true, errors.Is(failure, relayv2.ErrProtocol)
	}
	switch relayError.Code {
	case v2.ErrorNotFound:
		// Only an authenticated absent RESUME authorizes reuse of the fresh publication.
		return !resume, false
	case v2.ErrorAccessUnavailable:
		return resume, false
	case v2.ErrorAccessDenied, v2.ErrorMalformed, v2.ErrorUnsupportedMode, v2.ErrorShareIDCollision,
		v2.ErrorInvalidProof, v2.ErrorDescriptorInvalid, v2.ErrorStopped:
		return true, true
	default:
		// This includes uncertain REGISTER acknowledgements / AlreadyRegistered,
		// admission limits, Starting, expired challenges and competing resume claims.
		return true, false
	}
}

func (lifecycle *SenderLifecycle) retryDelay(fast time.Duration, slow bool) time.Duration {
	if !slow {
		return fast
	}
	// Bound injected jitter so scheduler defects cannot cause a hot loop.
	return max(senderRelaySlowRetry/2, min(lifecycle.config.Jitter(senderRelaySlowRetry), senderRelaySlowRetry*3/2))
}
