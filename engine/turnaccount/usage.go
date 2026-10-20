package turnaccount

import (
	"context"
	"math"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/connectivity/turnpath"
	"github.com/windshare/windshare/engine/turnprovider"
)

type leaseState struct {
	manager  *Manager
	account  *accountState
	identity iceconfig.Identity
	subject  turnpath.Subject
	issued   turnprovider.Issued
	usage    turnpath.Usage
	charged  uint64
	released bool
}

func (l *leaseState) charge(delta turnpath.Usage) bool {
	m := l.manager
	m.mu.Lock()
	if l.released {
		m.mu.Unlock()
		return false
	}
	addUsage(&l.usage, delta)
	addUsage(&l.account.usage, delta)
	// Payload measurements account for application writes. Socket
	// measurements add overhead without charging the same bytes a second time.
	charged := max(l.usage.Payload, saturatingAdd(l.usage.TransportSent, l.usage.TransportReceived))
	l.account.charged = saturatingAdd(l.account.charged, charged-l.charged)
	l.charged = charged
	exhausted := !l.account.exhausted && l.account.config.LocalBudgetBytes > 0 && l.account.charged >= l.account.config.LocalBudgetBytes
	if exhausted {
		l.account.exhausted = true
		close(l.account.lost)
	}
	usage := l.account.usage
	usable := !m.closed && !l.account.exhausted
	m.mu.Unlock()
	if exhausted {
		m.emit(Event{Identity: l.identity, Subject: l.subject, Milestone: "budget_exhausted", Usage: usage})
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
	l.account.active--
	m.mu.Unlock()
	m.emit(Event{Identity: l.identity, Subject: l.subject, Milestone: "local_path_stopped", Usage: l.usage})
	go func() {
		defer m.work.Done()
		m.revoke(l.account, l.issued, l.identity, l.subject)
	}()
}
func (m *Manager) revoke(account *accountState, issued turnprovider.Issued, identity iceconfig.Identity, subject turnpath.Subject) {
	provider, ok := account.config.Source.(revocable)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), IssuanceTimeout)
	defer cancel()
	event := Event{Identity: identity, Subject: subject, Milestone: "provider_revocation_confirmed"}
	if err := provider.Revoke(ctx, issued); err != nil {
		event.Milestone = "provider_revocation_failed"
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
