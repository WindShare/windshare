package turnruntime

import (
	"context"
	"errors"
	"time"

	"github.com/windshare/windshare/engine/internal/turnruntime/internal/provider"
)

const (
	cleanupRetryDelay        = time.Second
	cleanupMaximumRetryDelay = 30 * time.Second
)

var errCleanupExpired = errors.New("TURN cleanup authority expired")

type cleanupState struct {
	resource *provider.Cleanup
	account  *accountLedger
	event    Event
}

func cleanupID(resource *provider.Cleanup) string {
	if resource == nil {
		return ""
	}
	return resource.ID
}

// Admission includes retained cleanup so an unavailable issuer cannot turn
// repeated failed acquisitions into an unbounded background queue.
func (m *accountManager) retainCleanupLocked(account *accountLedger, resource *provider.Cleanup, event Event) *cleanupState {
	if resource == nil {
		return nil
	}
	pending := &cleanupState{resource: resource, account: account, event: event}
	m.cleanups[pending] = struct{}{}
	account.pendingCleanup++
	m.work.Add(1)
	return pending
}

func (m *accountManager) clean(pending *cleanupState) {
	defer m.work.Done()
	resource := pending.resource
	ctx, cancel := context.WithTimeoutCause(m.cleanupContext, resource.ExpiresAt.Sub(m.now()), errCleanupExpired)
	defer cancel()
	delay := cleanupRetryDelay
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			m.endCleanup(pending, context.Cause(ctx))
			return
		}
		bounded, stop := context.WithTimeout(ctx, IssuanceTimeout)
		result, err := resource.Revoke(bounded)
		stop()
		event := pending.event
		event.Attempt, event.Reason = attempt, ""
		if err == nil {
			event.Milestone = cleanupMilestone(result)
			if event.Milestone == "" {
				err = provider.ErrConfiguration
			} else {
				event.ProviderRevocationConfirmed = result == provider.RevocationConfirmed
				m.finishCleanup(pending, event)
				return
			}
		}
		event.Milestone, event.Reason = "provider_revocation_failed", err.Error()
		m.emit(event)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
		delay = min(delay*2, cleanupMaximumRetryDelay)
	}
}

func cleanupMilestone(result provider.CleanupResult) string {
	switch result {
	case provider.RevocationConfirmed:
		return "provider_revocation_confirmed"
	case provider.RevocationQueued:
		return "provider_revocation_pending"
	case provider.IssuanceCanceled:
		return "issuance_cancellation_confirmed"
	case provider.AuthorityExpired:
		return "provider_authority_expired"
	default:
		return ""
	}
}

func (m *accountManager) finishCleanup(pending *cleanupState, event Event) {
	m.mu.Lock()
	delete(m.cleanups, pending)
	pending.account.pendingCleanup--
	m.mu.Unlock()
	m.emit(event)
}

func (m *accountManager) endCleanup(pending *cleanupState, cause error) {
	event := pending.event
	event.Reason = cause.Error()
	if errors.Is(cause, errCleanupExpired) {
		event.Milestone = "provider_authority_expired"
		m.finishCleanup(pending, event)
		return
	}
	// Shutdown has a finite join budget. Retain the identities and report the
	// unfinished responsibility instead of claiming that cancellation cleaned it.
	event.Milestone = "provider_cleanup_unresolved"
	m.emit(event)
}
