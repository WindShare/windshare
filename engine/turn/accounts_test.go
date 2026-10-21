package turn

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/connectivity/turnpath"
	"github.com/windshare/windshare/engine/turn/internal/provider"
)

func fixture(t *testing.T, budget uint64) *accountManager {
	t.Helper()
	credential, _ := iceconfig.Fixed("user", "password")
	source, _ := provider.NewStatic(credential, nil)
	issuer := &credentialIssuer{source: source}
	manager, err := newAccountManager(accountConfig{Accounts: []account{{ID: "one", Provider: "standard", Source: issuer, LocalBudgetBytes: budget}, {ID: "two", Provider: "standard", Source: issuer, LocalBudgetBytes: budget}},
		Profiles: []profile{{ID: "a", AccountID: "one", URLs: []string{"turn:localhost:3478"}}, {ID: "b", AccountID: "one", URLs: []string{"turn:localhost:3479"}}, {ID: "c", AccountID: "two", URLs: []string{"turn:localhost:3480"}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Error(err)
		}
	})
	return manager
}
func acquire(t *testing.T, m *accountManager, profile string, id byte) *turnpath.Lease {
	t.Helper()
	lease, err := m.Acquire(context.Background(), profile, turnpath.Subject{SessionID: [16]byte{id}, PathID: [16]byte{id}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.Close)
	return lease
}
func TestAccountBudgetSharedAcrossSharesAndIndependentProfiles(t *testing.T) {
	m := fixture(t, 100)
	a := acquire(t, m, "a", 1)
	b := acquire(t, m, "b", 2)
	c := acquire(t, m, "c", 3)
	a.Charge(turnpath.Usage{Payload: 30, Useful: 25})
	a.Charge(turnpath.Usage{TransportSent: 40, TransportReceived: 10})
	if m.Snapshot()[0].ChargedBytes != 50 {
		t.Fatal("payload counted twice")
	}
	if b.Charge(turnpath.Usage{TransportSent: 50}) {
		t.Fatal("account budget not shared")
	}
	for _, lease := range []*turnpath.Lease{a, b} {
		select {
		case <-lease.Lost():
		default:
			t.Fatal("affected allocation retained")
		}
	}
	select {
	case <-c.Lost():
		t.Fatal("other account retired")
	default:
	}
	if _, err := m.Acquire(context.Background(), "a", turnpath.Subject{}); !errors.Is(err, ErrBudget) {
		t.Fatal(err)
	}
	a.Charge(turnpath.Usage{TransportSent: 7})
	if m.Snapshot()[0].ChargedBytes != 107 {
		t.Fatal("lost final shutdown traffic")
	}
	if !c.Charge(turnpath.Usage{Useful: 60}) || m.Snapshot()[1].ChargedBytes != 0 {
		t.Fatal("receiver report became billing")
	}
}

type blockingSource struct{ started chan struct{} }

func (s blockingSource) Issue(ctx context.Context, _ provider.Subject) (provider.Acquisition, error) {
	close(s.started)
	<-ctx.Done()
	return provider.Acquisition{}, ctx.Err()
}
func TestCanceledIssuanceReturnsCapacity(t *testing.T) {
	source := blockingSource{make(chan struct{})}
	m, err := newAccountManager(accountConfig{Accounts: []account{{ID: "a", Provider: "provider", Source: &credentialIssuer{source: source}}}, Profiles: []profile{{ID: "p", AccountID: "a", URLs: []string{"turn:localhost"}}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := m.Acquire(ctx, "p", turnpath.Subject{SessionID: [16]byte{1}, PathID: [16]byte{2}})
		done <- err
	}()
	<-source.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if m.Snapshot()[0].ActiveLeases != 0 || m.Snapshot()[0].LastFailure != "" {
		t.Fatalf("cancellation leaked capacity or fabricated provider failure: %+v", m.Snapshot())
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestConcurrentBudgetHasOneExhaustion(t *testing.T) {
	m := fixture(t, 64)
	lease := acquire(t, m, "a", 1)
	var group sync.WaitGroup
	for range 64 {
		group.Go(func() { lease.Charge(turnpath.Usage{TransportSent: 1}) })
	}
	group.Wait()
	if m.Snapshot()[0].ChargedBytes != 64 {
		t.Fatal(m.Snapshot())
	}
}
func TestCloseJoinsLiveLeaseRevocation(t *testing.T) {
	m := fixture(t, 0)
	lease := acquire(t, m, "a", 1)
	done := make(chan error, 1)
	go func() { done <- m.Close() }()
	select {
	case <-lease.Lost():
	case <-time.After(time.Second):
		t.Fatal("close did not revoke local authority")
	}
	lease.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not join")
	}
}

type revocationFixture struct {
	issued           provider.Acquisition
	started, proceed chan struct{}
}

func (source *revocationFixture) Issue(context.Context, provider.Subject) (provider.Acquisition, error) {
	issued := source.issued
	issued.Cleanup = &provider.Cleanup{ID: "fixture", ExpiresAt: time.Now().Add(time.Hour), Revoke: source.revoke}
	return issued, nil
}
func (source *revocationFixture) revoke(ctx context.Context) (provider.CleanupResult, error) {
	close(source.started)
	select {
	case <-source.proceed:
		return provider.RevocationConfirmed, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func TestLocalShutdownDoesNotClaimProviderRevocation(t *testing.T) {
	credentials, _ := iceconfig.Fixed("user", "password")
	source := &revocationFixture{issued: provider.Acquisition{Credentials: credentials}, started: make(chan struct{}), proceed: make(chan struct{})}
	var mu sync.Mutex
	var events []Event
	m, err := newAccountManager(accountConfig{
		Accounts: []account{{ID: "owner", Provider: "test", Source: &credentialIssuer{source: source}}},
		Profiles: []profile{{ID: "primary", AccountID: "owner", URLs: []string{"turn:localhost"}}},
		Observe:  func(event Event) { mu.Lock(); events = append(events, event); mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	lease := acquire(t, m, "primary", 1)
	lease.Close()
	<-source.started
	mu.Lock()
	local := events[len(events)-1]
	mu.Unlock()
	close(source.proceed)
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	if local.Milestone != "local_path_stopped" || local.ProviderRevocationConfirmed {
		t.Fatalf("local stop claimed remote revocation: %+v", local)
	}
	mu.Lock()
	confirmed := events[len(events)-1]
	mu.Unlock()
	if confirmed.Milestone != "provider_revocation_confirmed" || !confirmed.ProviderRevocationConfirmed {
		t.Fatalf("provider confirmation lost: %+v", confirmed)
	}
}
