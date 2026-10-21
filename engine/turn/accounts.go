package turn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/connectivity/turnpath"
	"github.com/windshare/windshare/engine/turn/internal/provider"
)

const (
	MaximumProfiles          = 8
	MaximumAccounts          = 8
	MaximumOutstandingLeases = 32
	IssuanceTimeout          = 15 * time.Second
)

var (
	ErrConfiguration = errors.New("invalid TURN settings")
	ErrBudget        = turnpath.ErrBudget
	ErrCapacity      = errors.New("TURN account lease capacity exhausted")
	ErrClosed        = errors.New("TURN runtime closed")
)

type credentialSource interface {
	Issue(context.Context, provider.Subject) (provider.Acquisition, error)
}
type renewable interface {
	Renew(context.Context, provider.Subject, provider.Acquisition) (provider.Acquisition, error)
}
type usageReporter interface {
	Query(context.Context, time.Time, time.Time) (provider.Usage, error)
}

type account struct {
	ID               string
	Provider         string
	LocalBudgetBytes uint64
	Source           credentialSource
	Usage            usageReporter
}
type profile struct {
	ID, AccountID string
	URLs          []string
}
type accountConfig struct {
	Services []managedService
	Accounts []account
	Profiles []profile
	Now      func() time.Time
	Observe  func(Event)
}
type Event struct {
	At                          time.Time
	Identity                    iceconfig.Identity
	Subject                     turnpath.Subject
	Revision                    uint64
	Milestone                   string
	ResourceID                  string
	Attempt                     int
	Reason                      string
	Usage                       turnpath.Usage
	ProviderRevocationConfirmed bool
}
type Status struct {
	AccountID, Provider string
	LocalBudgetBytes    uint64
	Usage               turnpath.Usage
	ChargedBytes        uint64
	ActiveLeases        int
	PendingCleanup      int
	Exhausted           bool
	LastFailure         string
}

// A ledger outlives configuration removal/re-import. A lease owns its issuer and
// revocation signal separately, so re-enabling an account never revives old grants.
type accountLedger struct {
	usage          turnpath.Usage
	charged        uint64
	active         int
	pendingCleanup int
	lastFailure    string
}
type accountManager struct {
	mu              sync.Mutex
	now             func() time.Time
	observe         func(Event)
	settings        accountConfig
	current         configuration
	changed         chan struct{}
	ledgers         map[string]*accountLedger
	leases          map[*leaseState]struct{}
	requests        map[*issuance]struct{}
	cleanups        map[*cleanupState]struct{}
	closed          bool
	work            sync.WaitGroup
	cleanupContext  context.Context
	stopCleanup     context.CancelFunc
	cleanupShutdown *time.Timer
}

func newAccountManager(config accountConfig) (*accountManager, error) {
	prepared, err := prepare(config)
	if err != nil {
		return nil, err
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	m := &accountManager{now: config.Now, observe: config.Observe, current: prepared, settings: cloneConfig(config),
		changed: make(chan struct{}), ledgers: make(map[string]*accountLedger),
		leases: make(map[*leaseState]struct{}), requests: make(map[*issuance]struct{}), cleanups: make(map[*cleanupState]struct{})}
	m.cleanupContext, m.stopCleanup = context.WithCancel(context.Background())
	m.current.revision = 1
	for id := range prepared.accounts {
		m.ledgers[id] = &accountLedger{}
	}
	return m, nil
}

// QueryUsage is an explicit operator operation. Transfer and budget decisions
// never depend on provider analytics availability, sampling or rate limits.
func (m *accountManager) QueryUsage(ctx context.Context, accountID string, first, last time.Time) (Usage, error) {
	m.mu.Lock()
	account, ok := m.current.accounts[accountID]
	if m.closed || !ok || account.Usage == nil {
		m.mu.Unlock()
		return Usage{}, ErrConfiguration
	}
	reporter := account.Usage
	m.work.Add(1)
	m.mu.Unlock()
	defer m.work.Done()
	usage, err := reporter.Query(ctx, first, last)
	return Usage(usage), err
}

func (m *accountManager) Inventory() turnpath.Inventory {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := turnpath.Inventory{Revision: m.current.revision, Changed: m.changed}
	for _, id := range m.current.profileOrder {
		profile := m.current.profiles[id]
		account := m.current.accounts[profile.AccountID]
		result.Profiles = append(result.Profiles, turnpath.Profile{Identity: iceconfig.Identity{
			Provider: account.Provider, Account: profile.AccountID, Profile: profile.ID}})
	}
	return result
}

func (m *accountManager) Snapshot() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]Status, 0, len(m.current.accountOrder))
	for _, id := range m.current.accountOrder {
		configured := m.current.accounts[id]
		state := m.ledgers[id]
		result = append(result, Status{AccountID: id, Provider: configured.Provider,
			LocalBudgetBytes: configured.LocalBudgetBytes, Usage: state.usage, ChargedBytes: state.charged,
			ActiveLeases: state.active, PendingCleanup: state.pendingCleanup,
			Exhausted: exhausted(configured, state), LastFailure: state.lastFailure})
	}
	return result
}

func exhausted(account account, ledger *accountLedger) bool {
	return account.LocalBudgetBytes > 0 && ledger.charged >= account.LocalBudgetBytes
}

func (m *accountManager) Close() error {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		m.cleanupShutdown = time.AfterFunc(IssuanceTimeout, m.stopCleanup)
		for request := range m.requests {
			request.cancel(ErrClosed)
		}
		for lease := range m.leases {
			lease.cancel(ErrClosed)
		}
	}
	m.mu.Unlock()
	m.work.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupShutdown.Stop()
	m.stopCleanup()
	var failures []error
	for id, ledger := range m.ledgers {
		if ledger.active != 0 || ledger.pendingCleanup != 0 {
			failures = append(failures, fmt.Errorf("TURN account %s retained %d leases and %d unresolved cleanups", id, ledger.active, ledger.pendingCleanup))
		}
	}
	return errors.Join(failures...)
}

func (m *accountManager) emit(event Event) {
	slog.Debug("TURN account", "provider", event.Identity.Provider, "account", event.Identity.Account, "profile", event.Identity.Profile,
		"session_id", fmt.Sprintf("%x", event.Subject.SessionID), "path_id", fmt.Sprintf("%x", event.Subject.PathID),
		"config_revision", event.Revision, "milestone", event.Milestone, "resource_id", event.ResourceID,
		"attempt", event.Attempt, "reason", event.Reason, "useful_bytes", event.Usage.Useful,
		"transport_sent", event.Usage.TransportSent, "transport_received", event.Usage.TransportReceived,
		"provider_revocation_confirmed", event.ProviderRevocationConfirmed)
	if m.observe != nil {
		event.At = m.now()
		m.observe(event)
	}
}
