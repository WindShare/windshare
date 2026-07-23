package v2peer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/windshare/windshare/connectivity/v2signal"
)

func (handler *senderHandler) retireRejectedOffer(
	operation peerOperation,
	binding v2signal.Binding,
) {
	if binding.Validate() != nil {
		return
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.expireRetiredLocked(handler.factory.now())
	if handler.attempts[operation] != nil {
		return
	}
	if _, exists := handler.bindings[binding]; exists {
		return
	}
	if _, exists := handler.retiredOperations[operation]; exists {
		return
	}
	if _, exists := handler.retiredBindings[binding]; exists {
		return
	}
	if len(handler.bindings)+len(handler.retiredBindings) >= handler.factory.maxRetiredBindings {
		blockedUntil := handler.factory.now().Add(handler.factory.retiredBindingTTL)
		if handler.replayBlockedUntil.Before(blockedUntil) {
			handler.replayBlockedUntil = blockedUntil
		}
		return
	}
	retired := retiredBinding{
		operation: operation, binding: binding,
		expiresAt: handler.factory.now().Add(handler.factory.retiredBindingTTL),
	}
	handler.retiredOperations[operation] = retired
	handler.retiredBindings[binding] = retired
}

func (handler *senderHandler) startAttempt(
	ctx context.Context,
	operation peerOperation,
	offer v2signal.Offer,
) error {
	handler.mu.Lock()
	defer handler.mu.Unlock()
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
		handler.factory.onError(fmt.Errorf("%w: %w", ErrNegotiation, result))
	}
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
	handler.closeInbox()
	handler.mu.Lock()
	handler.stopping = true
	attempts := make([]*peerAttempt, 0, len(handler.attempts))
	for _, attempt := range handler.attempts {
		attempts = append(attempts, attempt)
	}
	handler.mu.Unlock()
	for _, attempt := range attempts {
		attempt.stop(context.Canceled)
	}
	handler.work.Wait()
	handler.mu.Lock()
	clear(handler.attempts)
	clear(handler.bindings)
	clear(handler.retiredOperations)
	clear(handler.retiredBindings)
	handler.replayBlockedUntil = time.Time{}
	handler.mu.Unlock()
}

func (handler *senderHandler) closeInbox() {
	handler.inboxMu.Lock()
	defer handler.inboxMu.Unlock()
	if handler.closed {
		return
	}
	handler.closed = true
	for {
		select {
		case event := <-handler.events:
			if event.completed != nil {
				event.completed <- context.Canceled
			}
		default:
			return
		}
	}
}
