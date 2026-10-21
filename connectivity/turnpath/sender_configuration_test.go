package turnpath

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session/turncontrol"
)

type mutableAccounts struct {
	accountFixture
	inventory Inventory
	started   chan context.Context
	proceed   chan struct{}
}

func (accounts *mutableAccounts) Inventory() Inventory { return accounts.inventory }
func (accounts *mutableAccounts) Acquire(ctx context.Context, _ string, subject Subject) (*Lease, error) {
	if accounts.started != nil {
		accounts.started <- ctx
		<-accounts.proceed
	}
	return accounts.issue(ctx, subject)
}

func configurationSender(t *testing.T) (*Sender, *mutableAccounts) {
	t.Helper()
	now := time.Unix(1700000000, 0)
	accounts := &mutableAccounts{accountFixture: accountFixture{now: func() time.Time { return now }}}
	accounts.inventory = Inventory{Revision: 1, Profiles: accounts.Profiles()}
	accounts.inventory.Profiles[0].Revision = 1
	sender := NewSender(SenderConfig{Accounts: accounts, SessionID: [16]byte{1}, Now: accounts.now,
		Send: func(context.Context, []byte) error { return nil }})
	t.Cleanup(sender.close)
	return sender, accounts
}

func TestInventoryPublicationPreservesUnchangedPendingIssuance(t *testing.T) {
	sender, accounts := configurationSender(t)
	accounts.started, accounts.proceed = make(chan context.Context, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(accounts.proceed) }) }
	t.Cleanup(unblock)
	sender.apply(t.Context(), turncontrol.Demand{Authority: fixtureHeader(sender, 1), HoldFor: time.Minute})
	var request context.Context
	select {
	case request = <-accounts.started:
	case <-time.After(time.Second):
		t.Fatal("issuer did not start")
	}
	accounts.inventory.Revision++
	sender.refreshConfiguration(t.Context())
	if request.Err() != nil {
		t.Fatal("unrelated inventory publication canceled the issuer")
	}
	unblock()
	issueNext(t, sender)
	if sender.services[0].pending == nil {
		t.Fatal("unchanged profile's result was discarded")
	}
	if calls, _ := accounts.counts(); calls != 1 {
		t.Fatal("unrelated publication retried issuance")
	}
}

func TestProfileChangeRejectsAlreadyCompletedIssuance(t *testing.T) {
	sender, accounts := configurationSender(t)
	sender.apply(t.Context(), turncontrol.Demand{Authority: fixtureHeader(sender, 1), HoldFor: time.Minute})
	var result issued
	select {
	case result = <-sender.completed:
	case <-time.After(time.Second):
		t.Fatal("issuer did not finish")
	}
	// The sender can miss intermediate publications, including remove/reimport.
	// A profile incarnation must still reject results completed before that gap.
	accounts.inventory.Revision += 2
	accounts.inventory.Profiles[0].Revision += 2
	sender.install(t.Context(), result)
	if sender.services[0].pending != nil {
		t.Fatal("completed result from an earlier profile incarnation was installed")
	}
	issueNext(t, sender)
	if calls, closed := accounts.counts(); calls != 2 || closed != 1 || sender.services[0].pending == nil {
		t.Fatalf("changed profile did not retry cleanly: calls=%d closed=%d", calls, closed)
	}
}

func TestUnrelatedPublicationPreservesProviderCooldown(t *testing.T) {
	sender, accounts := configurationSender(t)
	accounts.fail = true
	sender.apply(t.Context(), turncontrol.Demand{Authority: fixtureHeader(sender, 1), HoldFor: time.Minute})
	issueNext(t, sender)
	retryAt := sender.services[0].retryAt
	accounts.inventory.Revision++
	sender.maintain(t.Context())
	if sender.services[0].retryAt != retryAt {
		t.Fatal("unrelated configuration bypassed provider cooldown")
	}
	if calls, _ := accounts.counts(); calls != 1 {
		t.Fatal("provider retried because another configuration changed")
	}
	accounts.inventory.Revision++
	accounts.inventory.Profiles[0].Revision++
	sender.maintain(t.Context())
	issueNext(t, sender)
	if calls, _ := accounts.counts(); calls != 2 {
		t.Fatal("affected profile failed to reconsider current authority")
	}
}
