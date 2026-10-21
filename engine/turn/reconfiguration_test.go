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

type versionedSource struct {
	name            string
	revoked         chan string
	mu              sync.Mutex
	issued, renewed int
}

func newVersionedSource(name string) *versionedSource {
	return &versionedSource{name: name, revoked: make(chan string, MaximumOutstandingLeases)}
}
func (s *versionedSource) Issue(context.Context, provider.Subject) (provider.Acquisition, error) {
	s.mu.Lock()
	s.issued++
	s.mu.Unlock()
	credential, _ := iceconfig.Expiring(s.name, "password", time.Now().Add(time.Hour))
	return provider.Acquisition{Credentials: credential, Cleanup: &provider.Cleanup{
		ID: s.name, ExpiresAt: credential.ExpiresAt(),
		Revoke: func(context.Context) (provider.CleanupResult, error) {
			s.revoked <- s.name
			return provider.RevocationConfirmed, nil
		},
	}}, nil
}
func (s *versionedSource) Renew(ctx context.Context, subject provider.Subject, _ provider.Acquisition) (provider.Acquisition, error) {
	s.mu.Lock()
	s.renewed++
	s.mu.Unlock()
	return s.Issue(ctx, subject)
}
func configurationFor(source credentialSource, budget uint64) accountConfig {
	return accountConfig{Accounts: []account{{ID: "owner", Provider: "test", Source: source, LocalBudgetBytes: budget}},
		Profiles: []profile{{ID: "primary", AccountID: "owner", URLs: []string{"turn:localhost:3478"}}}}
}
func reconfigure(t *testing.T, m *accountManager, config accountConfig) {
	t.Helper()
	if err := m.Reconfigure(config); err != nil {
		t.Fatal(err)
	}
}
func newConfiguredManager(t *testing.T, config accountConfig) *accountManager {
	t.Helper()
	m, err := newAccountManager(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
	return m
}
func awaitRevocation(t *testing.T, source *versionedSource, want string) {
	t.Helper()
	select {
	case got := <-source.revoked:
		if got != want {
			t.Fatalf("revoked %q, want %q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("issuer revocation missing")
	}
}

func TestConfigurationRetainsUsageAndPinsLeaseIssuer(t *testing.T) {
	first, second := newVersionedSource("old"), newVersionedSource("new")
	m := newConfiguredManager(t, configurationFor(first, 100))
	old := acquire(t, m, "primary", 1)
	old.Charge(turnpath.Usage{TransportSent: 40})
	inventory := m.Inventory()
	next := configurationFor(second, 200)
	next.Profiles[0].URLs[0] = "turn:localhost:3479"
	reconfigure(t, m, next)
	select {
	case <-inventory.Changed:
	default:
		t.Fatal("configuration subscribers not notified")
	}
	if old.Loss() != nil || old.Relay().URLs()[0] != "turn:localhost:3478" {
		t.Fatal("settings mutation interrupted an existing lease")
	}
	replacement, err := old.Renew(t.Context(), turnpath.Subject{SessionID: [16]byte{1}, PathID: [16]byte{2}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(replacement.Close)
	if replacement.Relay().Credentials().Username() != "new" || replacement.Relay().URLs()[0] != "turn:localhost:3479" {
		t.Fatal("new acquisition ignored current settings")
	}
	second.mu.Lock()
	renewed := second.renewed
	second.mu.Unlock()
	if renewed != 0 {
		t.Fatal("old credentials were handed to a new issuer for renewal")
	}
	old.Charge(turnpath.Usage{TransportSent: 5})
	replacement.Charge(turnpath.Usage{TransportSent: 30})
	if status := m.Snapshot()[0]; status.ChargedBytes != 75 || status.ActiveLeases != 2 || status.LocalBudgetBytes != 200 {
		t.Fatalf("usage split across revisions: %+v", status)
	}
	old.Close()
	awaitRevocation(t, first, "old")
	replacement.Close()
	awaitRevocation(t, second, "new")
}

func TestBudgetChangeAndAccountReimportNeverResetLedgerOrReviveLeases(t *testing.T) {
	source := newVersionedSource("issuer")
	m := newConfiguredManager(t, configurationFor(source, 100))
	old := acquire(t, m, "primary", 1)
	old.Charge(turnpath.Usage{TransportSent: 80})
	reconfigure(t, m, configurationFor(source, 50))
	if !errors.Is(old.Loss(), ErrBudget) {
		t.Fatal("lower hard budget did not revoke active authority")
	}
	if _, err := m.Acquire(t.Context(), "primary", turnpath.Subject{}); !errors.Is(err, ErrBudget) {
		t.Fatal(err)
	}
	reconfigure(t, m, configurationFor(source, 200))
	if old.Charge(turnpath.Usage{TransportSent: 10}) {
		t.Fatal("budget increase revived old authority")
	}
	current := acquire(t, m, "primary", 2)
	reconfigure(t, m, accountConfig{})
	if current.Loss() == nil {
		t.Fatal("removed account retained active authority")
	}
	reconfigure(t, m, configurationFor(source, 95))
	fresh := acquire(t, m, "primary", 3)
	if fresh.Charge(turnpath.Usage{TransportSent: 5}) {
		t.Fatal("re-import bypassed previous usage")
	}
	if status := m.Snapshot()[0]; status.ChargedBytes != 95 || !status.Exhausted {
		t.Fatal(status)
	}
	if old.Loss() == nil || current.Loss() == nil {
		t.Fatal("re-import resurrected revoked leases")
	}
}

type canceledProvider struct{}

func (canceledProvider) Issue(context.Context, provider.Subject) (provider.Acquisition, error) {
	return provider.Acquisition{}, context.Canceled
}

func TestProviderCancellationDoesNotMasqueradeAsCallerCancellation(t *testing.T) {
	m := newConfiguredManager(t, configurationFor(canceledProvider{}, 100))
	if _, err := m.Acquire(t.Context(), "primary", turnpath.Subject{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if m.Snapshot()[0].LastFailure == "" {
		t.Fatal("provider fault was hidden as caller cancellation")
	}
}

type lateSource struct {
	*versionedSource
	started chan context.Context
	proceed chan struct{}
}

func (s *lateSource) Issue(ctx context.Context, subject provider.Subject) (provider.Acquisition, error) {
	s.started <- ctx
	<-s.proceed
	return s.versionedSource.Issue(ctx, subject)
}

func TestConfigurationSupersedesPendingIssuanceWithoutProviderFailure(t *testing.T) {
	old := &lateSource{versionedSource: newVersionedSource("old"), started: make(chan context.Context, 1), proceed: make(chan struct{})}
	var once sync.Once
	unblock := func() { once.Do(func() { close(old.proceed) }) }
	config := configurationFor(old, 100)
	var mu sync.Mutex
	var events []Event
	config.Observe = func(event Event) { mu.Lock(); events = append(events, event); mu.Unlock() }
	m := newConfiguredManager(t, config)
	t.Cleanup(unblock)
	done := make(chan error, 1)
	go func() {
		lease, err := m.Acquire(t.Context(), "primary", turnpath.Subject{})
		if lease != nil {
			lease.Close()
		}
		done <- err
	}()
	request := <-old.started
	reconfigure(t, m, configurationFor(newVersionedSource("new"), 200))
	if !errors.Is(context.Cause(request), turnpath.ErrSuperseded) {
		t.Fatal("pending request did not receive configuration cancellation")
	}
	unblock()
	if err := <-done; !errors.Is(err, turnpath.ErrSuperseded) {
		t.Fatal(err)
	}
	awaitRevocation(t, old.versionedSource, "old")
	lease := acquire(t, m, "primary", 2)
	if lease.Relay().Credentials().Username() != "new" {
		t.Fatal("late credentials replaced current source")
	}
	if m.Snapshot()[0].LastFailure != "" {
		t.Fatal("configuration cancellation fabricated a provider failure")
	}
	mu.Lock()
	defer mu.Unlock()
	superseded := false
	for _, event := range events {
		if event.Milestone == "issuance_failed" {
			t.Fatal("superseded request reported a service fault")
		}
		superseded = superseded || event.Milestone == "issuance_superseded"
	}
	if !superseded {
		t.Fatal("missing superseded observation")
	}
}

func TestInvalidConfigurationIsAtomicAndCloseCancelsIssuance(t *testing.T) {
	source := blockingSource{make(chan struct{})}
	config := configurationFor(source, 100)
	m := newConfiguredManager(t, config)
	inventory := m.Inventory()
	bad := configurationFor(source, 100)
	bad.Profiles[0].AccountID = "missing"
	if err := m.Reconfigure(bad); !errors.Is(err, ErrConfiguration) {
		t.Fatal(err)
	}
	if m.Inventory().Revision != inventory.Revision {
		t.Fatal("invalid configuration was published")
	}
	select {
	case <-inventory.Changed:
		t.Fatal("invalid settings invalidated current readers")
	default:
	}
	done := make(chan error, 1)
	go func() { _, err := m.Acquire(t.Context(), "primary", turnpath.Subject{}); done <- err }()
	<-source.started
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if err := m.Reconfigure(config); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}
