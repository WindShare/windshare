package v2peer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/windshare/windshare/connectivity/v2signal"
)

func (handler *senderHandler) claimRejectedOffer(
	operation peerOperation,
	binding v2signal.Binding,
) evidenceClaim {
	if binding.Validate() != nil {
		return evidenceClaim{}
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.expireRetiredLocked(handler.factory.now())
	_, activeBinding := handler.bindings[binding]
	claim := handler.claimEvidenceLocked(operation, binding)
	if activeBinding {
		// An active recorder owns this identity. Reserving a missing claim repairs
		// focused harness state without publishing a competing sideSequence=1.
		claim.acquired = false
		return claim
	}
	if handler.attempts[operation] != nil {
		// Reusing an operation with a new binding is still the first rejection of
		// that distinct evidence identity, even though it also stops the collision.
		return claim
	}
	if claim.sessionTerminal {
		return claim
	}
	_, retiredOperation := handler.retiredOperations[operation]
	_, retiredBindingExists := handler.retiredBindings[binding]
	if retiredOperation || retiredBindingExists {
		return claim
	}
	if handler.stopping {
		return claim
	}
	if len(handler.bindings)+len(handler.retiredBindings) >= handler.factory.maxRetiredBindings {
		blockedUntil := handler.factory.now().Add(handler.factory.retiredBindingTTL)
		if handler.replayBlockedUntil.Before(blockedUntil) {
			handler.replayBlockedUntil = blockedUntil
		}
		return claim
	}
	retired := retiredBinding{
		operation: operation, binding: binding,
		expiresAt: handler.factory.now().Add(handler.factory.retiredBindingTTL),
	}
	handler.retiredOperations[operation] = retired
	handler.retiredBindings[binding] = retired
	return claim
}

func (handler *senderHandler) startAttempt(
	ctx context.Context,
	operation peerOperation,
	offer v2signal.Offer,
) error {
	var capacityBoundary *v2signal.Binding
	handler.mu.Lock()
	defer func() {
		handler.mu.Unlock()
		if capacityBoundary != nil {
			handler.emitRejectedOfferTerminal(*capacityBoundary, senderEvidenceCapacityFailure())
		}
	}()
	if handler.stopping {
		return context.Canceled
	}
	handler.expireRetiredLocked(handler.factory.now())
	if handler.factory.now().Before(handler.replayBlockedUntil) {
		return ErrReplayCapacity
	}
	if _, exists := handler.attempts[operation]; exists {
		return errors.Join(ErrProtocol, errors.New("peer offer operation was repeated"))
	}
	if _, exists := handler.retiredOperations[operation]; exists {
		return errors.Join(ErrProtocol, errors.New("retired peer offer operation was repeated"))
	}
	if _, exists := handler.bindings[offer.Binding]; exists {
		return errors.Join(ErrProtocol, v2signal.ErrSignalBinding)
	}
	if _, exists := handler.retiredBindings[offer.Binding]; exists {
		return errors.Join(ErrProtocol, v2signal.ErrSignalBinding)
	}
	if handler.evidenceAuthority.terminal {
		return ErrEvidenceIdentityCapacity
	}
	if handler.evidenceAuthority.claimed(offer.Binding) {
		return errors.Join(ErrProtocol, v2signal.ErrSignalBinding)
	}
	if len(handler.attempts) >= handler.factory.maxActiveAttempts {
		return ErrAttemptCapacity
	}
	// Every admitted attempt must be able to leave a replay tombstone. Reserving
	// that budget up front avoids a security-sensitive eviction race at teardown.
	if len(handler.bindings)+len(handler.retiredBindings) >= handler.factory.maxRetiredBindings {
		return ErrReplayCapacity
	}
	attempt := newPeerAttempt(peerAttemptConfig{
		factory: handler.factory, session: handler.session,
		operation: operation.id, generation: operation.generation, offer: offer,
		onDone: handler.attemptDone,
	})
	claim := handler.claimEvidenceLocked(operation, offer.Binding)
	if claim.sessionTerminal {
		if claim.acquired {
			binding := offer.Binding
			capacityBoundary = &binding
		}
		return ErrEvidenceIdentityCapacity
	}
	if !claim.acquired {
		return errors.Join(ErrProtocol, v2signal.ErrSignalBinding)
	}
	handler.attempts[operation] = attempt
	handler.bindings[offer.Binding] = operation
	handler.work.Add(1)
	attempt.start(ctx, &handler.work)
	return nil
}

func (handler *senderHandler) acceptCandidate(
	operation peerOperation,
	candidate v2signal.Candidate,
) error {
	handler.mu.Lock()
	handler.expireRetiredLocked(handler.factory.now())
	attempt := handler.attempts[operation]
	retired, isRetired := handler.retiredOperations[operation]
	handler.mu.Unlock()
	if attempt != nil {
		if err := attempt.binding().RequireSame(candidate.Binding); err != nil {
			return errors.Join(ErrProtocol, err)
		}
		_, err := attempt.remoteCandidate(candidate)
		return err
	}
	if isRetired {
		if err := retired.binding.RequireSame(candidate.Binding); err != nil {
			return errors.Join(ErrProtocol, err)
		}
		return nil
	}
	return errors.Join(ErrProtocol, errors.New("peer candidate has no offer operation"))
}

func (handler *senderHandler) cancelAttempt(
	ctx context.Context,
	operation peerOperation,
) error {
	handler.mu.Lock()
	handler.expireRetiredLocked(handler.factory.now())
	attempt := handler.attempts[operation]
	handler.mu.Unlock()
	if attempt != nil {
		return attempt.cancelOperation(ctx)
	}
	return nil
}

func (handler *senderHandler) attemptDone(attempt *peerAttempt, result error) {
	operation := attempt.operation()
	binding := attempt.binding()
	handler.mu.Lock()
	if handler.attempts[operation] == attempt {
		delete(handler.attempts, operation)
		delete(handler.bindings, binding)
		if !handler.stopping {
			retired := retiredBinding{
				operation: operation,
				binding:   binding,
				expiresAt: handler.factory.now().Add(handler.factory.retiredBindingTTL),
			}
			handler.retiredOperations[operation] = retired
			handler.retiredBindings[binding] = retired
		}
	}
	handler.mu.Unlock()
	if result != nil && (!errors.Is(result, context.Canceled) ||
		errors.Is(result, errPeerShutdown) || errors.Is(result, errChannelDrain)) {
		handler.factory.reportError(fmt.Errorf("%w: %w", ErrNegotiation, result))
	}
}

func (handler *senderHandler) claimEvidenceLocked(
	operation peerOperation,
	binding v2signal.Binding,
) evidenceClaim {
	return handler.evidenceAuthority.claim(operation, binding)
}

func (handler *senderHandler) expireRetiredLocked(now time.Time) {
	for operation, retired := range handler.retiredOperations {
		if now.Before(retired.expiresAt) {
			continue
		}
		delete(handler.retiredOperations, operation)
		if current, exists := handler.retiredBindings[retired.binding]; exists && current.operation == operation {
			delete(handler.retiredBindings, retired.binding)
		}
	}
}

func (handler *senderHandler) stopAll() {
	handler.mu.Lock()
	handler.stopping = true
	attempts := make([]*peerAttempt, 0, len(handler.attempts))
	for _, attempt := range handler.attempts {
		attempts = append(attempts, attempt)
	}
	handler.mu.Unlock()
	handler.closeInbox()
	for _, attempt := range attempts {
		attempt.stop(context.Canceled)
	}
	handler.ingress.Wait()
	handler.work.Wait()
	handler.mu.Lock()
	clear(handler.attempts)
	clear(handler.bindings)
	handler.evidenceAuthority.reset()
	clear(handler.retiredOperations)
	clear(handler.retiredBindings)
	handler.replayBlockedUntil = time.Time{}
	handler.mu.Unlock()
}

func (handler *senderHandler) closeInbox() {
	handler.inboxMu.Lock()
	if handler.closed {
		handler.inboxMu.Unlock()
		return
	}
	handler.closed = true
	type abandonedOffer struct {
		event   handlerEvent
		failure SenderAttemptFailure
	}
	var abandoned []abandonedOffer
	for {
		select {
		case event := <-handler.events:
			if binding := offerBindingForEvent(event); binding != nil {
				claim := handler.claimRejectedOffer(event.operation, *binding)
				if claim.acquired {
					failure := senderRuntimeStoppedFailure()
					if claim.sessionTerminal {
						failure = senderEvidenceCapacityFailure()
					}
					abandoned = append(abandoned, abandonedOffer{event: event, failure: failure})
				}
			}
			if event.completed != nil {
				event.completed <- context.Canceled
			}
		default:
			handler.inboxMu.Unlock()
			for _, abandonedOffer := range abandoned {
				handler.emitUnstartedOfferTerminal(abandonedOffer.event, abandonedOffer.failure)
			}
			return
		}
	}
}
