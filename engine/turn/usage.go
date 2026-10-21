package turn

import (
	"context"
	"math"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/connectivity/turnpath"
	"github.com/windshare/windshare/engine/turn/internal/provider"
)

type leaseState struct {
	manager         *accountManager
	account         *accountLedger
	identity        iceconfig.Identity
	subject         turnpath.Subject
	revision        uint64
	profileRevision uint64
	issuer          *credentialIssuer
	issued          provider.Acquisition
	authority       context.Context
	cancel          context.CancelCauseFunc
	usage           turnpath.Usage
	charged         uint64
	released        bool
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
		m.emit(Event{Identity: l.identity, Subject: l.subject, Revision: l.revision, ProfileRevision: l.profileRevision, Milestone: "budget_exhausted", Usage: usage})
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
	event := Event{Identity: l.identity, Subject: l.subject, Revision: l.revision, ProfileRevision: l.profileRevision,
		Milestone: "local_path_stopped", Usage: l.usage, ResourceID: cleanupID(l.issued.Cleanup)}
	cleanup := m.retainCleanupLocked(l.account, l.issued.Cleanup, event)
	m.mu.Unlock()
	m.emit(event)
	if cleanup != nil {
		go m.clean(cleanup)
	}
	m.work.Done()
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
