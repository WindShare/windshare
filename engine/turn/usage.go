package turn

import (
	"context"
	"errors"
	"math"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/connectivity/turnpath"
	"github.com/windshare/windshare/engine/turn/internal/provider"
)

type leaseState struct {
	manager   *accountManager
	account   *accountLedger
	source    credentialSource
	identity  iceconfig.Identity
	subject   turnpath.Subject
	revision  uint64
	issued    provider.Issued
	authority context.Context
	cancel    context.CancelCauseFunc
	usage     turnpath.Usage
	charged   uint64
	released  bool
}

func (l *leaseState) charge(delta turnpath.Usage) bool {
	m := l.manager
	m.mu.Lock()
	if l.released {
		m.mu.Unlock()
		return false
	}
	configured, enabled := m.current.accounts[l.identity.Account]
	wasExhausted := enabled && exhausted(configured, l.account)
	addUsage(&l.usage, delta)
	addUsage(&l.account.usage, delta)
	// Socket measurements include overhead without charging payload twice.
	charged := max(l.usage.Payload, saturatingAdd(l.usage.TransportSent, l.usage.TransportReceived))
	l.account.charged = saturatingAdd(l.account.charged, charged-l.charged)
	l.charged = charged
	newlyExhausted := enabled && !wasExhausted && exhausted(configured, l.account)
	if newlyExhausted {
		for request := range m.requests {
			if request.account == l.account {
				request.cancel(ErrBudget)
			}
		}
		for lease := range m.leases {
			if lease.account == l.account {
				lease.cancel(ErrBudget)
			}
		}
	}
	usage := l.account.usage
	usable := !m.closed && l.authority.Err() == nil
	m.mu.Unlock()
	if newlyExhausted {
		m.emit(Event{Identity: l.identity, Subject: l.subject, Revision: l.revision, Milestone: "budget_exhausted", Usage: usage})
	}
	return usable
}

func (l *leaseState) release() {
	m := l.manager
	m.mu.Lock()
	if l.released {
		m.mu.Unlock()
		return
	}
	l.released = true
	l.cancel(ErrClosed)
	l.account.active--
	delete(m.leases, l)
	usage := l.usage
	m.mu.Unlock()
	m.emit(Event{Identity: l.identity, Subject: l.subject, Revision: l.revision, Milestone: "local_path_stopped", Usage: usage})
	go func() {
		defer m.work.Done()
		m.revoke(l.source, l.issued, l.identity, l.subject, l.revision)
	}()
}

func (m *accountManager) revoke(source credentialSource, issued provider.Issued, identity iceconfig.Identity, subject turnpath.Subject, revision uint64) {
	issuer, ok := source.(revocable)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), IssuanceTimeout)
	defer cancel()
	event := Event{Identity: identity, Subject: subject, Revision: revision, Milestone: "provider_revocation_confirmed"}
	if err := issuer.Revoke(ctx, issued); err != nil {
		event.Milestone = "provider_revocation_failed"
		if errors.Is(err, provider.ErrRevocationPending) {
			event.Milestone = "provider_revocation_pending"
		}
		event.Reason = err.Error()
	} else {
		event.ProviderRevocationConfirmed = true
	}
	m.emit(event)
}

func saturatingAdd(a, b uint64) uint64 {
	if b > math.MaxUint64-a {
		return math.MaxUint64
	}
	return a + b
}
func addUsage(target *turnpath.Usage, delta turnpath.Usage) {
	target.Payload = saturatingAdd(target.Payload, delta.Payload)
	target.Useful = saturatingAdd(target.Useful, delta.Useful)
	target.TransportSent = saturatingAdd(target.TransportSent, delta.TransportSent)
	target.TransportReceived = saturatingAdd(target.TransportReceived, delta.TransportReceived)
}
