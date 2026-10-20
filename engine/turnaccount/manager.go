// Package turnaccount owns per-account authority and local budgets shared by all
// shares and receivers in one Engine. Provider billing is a separate observation.
package turnaccount

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/connectivity/turnpath"
	"github.com/windshare/windshare/engine/turnprovider"
)

const (
	MaximumProfiles     = 8
	MaximumAccounts     = 8
	MaximumActiveLeases = 32
	IssuanceTimeout     = 15 * time.Second
)

var (
	ErrConfiguration = errors.New("invalid TURN account configuration")
	ErrBudget        = turnpath.ErrBudget
	ErrCapacity      = errors.New("TURN account lease capacity exhausted")
	ErrClosed        = errors.New("TURN account owner closed")
)

type Source interface {
	Issue(context.Context, turnprovider.Subject) (turnprovider.Issued, error)
}
type renewable interface {
	Renew(context.Context, turnprovider.Subject, turnprovider.Issued) (turnprovider.Issued, error)
}
type revocable interface {
	Revoke(context.Context, turnprovider.Issued) error
}
type UsageReporter interface {
	Query(context.Context, time.Time, time.Time) (turnprovider.Usage, error)
}

type Account struct {
	ID               string
	Provider         string
	LocalBudgetBytes uint64
	Source           Source
	Usage            UsageReporter
}
type Profile struct {
	ID, AccountID string
	URLs          []string
}
type Config struct {
	Accounts []Account
	Profiles []Profile
	Now      func() time.Time
	Observe  func(Event)
}
type Event struct {
	At                          time.Time
	Identity                    iceconfig.Identity
	Subject                     turnpath.Subject
	Revision                    uint64
	Milestone                   string
	Reason                      string
	Usage                       turnpath.Usage
	ProviderRevocationConfirmed bool
}
type Snapshot struct {
	AccountID, Provider string
	LocalBudgetBytes    uint64
	Usage               turnpath.Usage
	ChargedBytes        uint64
	ActiveLeases        int
	Exhausted           bool
	LastFailure         string
}

// A ledger outlives configuration removal/re-import. A lease owns its issuer and
// revocation signal separately, so re-enabling an account never revives old grants.
type accountLedger struct {
	usage       turnpath.Usage
	charged     uint64
	active      int
	lastFailure string
}
type Manager struct {
	mu       sync.Mutex
	now      func() time.Time
	observe  func(Event)
	current  configuration
	changed  chan struct{}
	ledgers  map[string]*accountLedger
	leases   map[*leaseState]struct{}
	requests map[*issuance]struct{}
	closed   bool
	work     sync.WaitGroup
}

func New(config Config) (*Manager, error) {
	prepared, err := prepare(config)
	if err != nil {
		return nil, err
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	m := &Manager{now: config.Now, observe: config.Observe, current: prepared,
		changed: make(chan struct{}), ledgers: make(map[string]*accountLedger),
		leases: make(map[*leaseState]struct{}), requests: make(map[*issuance]struct{})}
	m.current.revision = 1
	for id := range prepared.accounts {
		m.ledgers[id] = &accountLedger{}
	}
	return m, nil
}

// QueryUsage is an explicit operator operation. Transfer and budget decisions
// never depend on provider analytics availability, sampling or rate limits.
func (m *Manager) QueryUsage(ctx context.Context, accountID string, first, last time.Time) (turnprovider.Usage, error) {
	m.mu.Lock()
	account, ok := m.current.accounts[accountID]
	if m.closed || !ok || account.Usage == nil {
		m.mu.Unlock()
		return turnprovider.Usage{}, ErrConfiguration
	}
	reporter := account.Usage
	m.work.Add(1)
	m.mu.Unlock()
	defer m.work.Done()
	return reporter.Query(ctx, first, last)
}

func (m *Manager) Inventory() turnpath.Inventory {
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

func (m *Manager) Snapshot() []Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]Snapshot, 0, len(m.current.accountOrder))
	for _, id := range m.current.accountOrder {
		configured := m.current.accounts[id]
		state := m.ledgers[id]
		result = append(result, Snapshot{AccountID: id, Provider: configured.Provider,
			LocalBudgetBytes: configured.LocalBudgetBytes, Usage: state.usage, ChargedBytes: state.charged,
			ActiveLeases: state.active, Exhausted: exhausted(configured, state), LastFailure: state.lastFailure})
	}
	return result
}

func exhausted(account Account, ledger *accountLedger) bool {
	return account.LocalBudgetBytes > 0 && ledger.charged >= account.LocalBudgetBytes
}

func (m *Manager) Close() error {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
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
	for id, ledger := range m.ledgers {
		if ledger.active != 0 {
			return fmt.Errorf("TURN account %s retained %d leases", id, ledger.active)
		}
	}
	return nil
}

func (m *Manager) emit(event Event) {
	slog.Debug("TURN account", "provider", event.Identity.Provider, "account", event.Identity.Account, "profile", event.Identity.Profile,
		"session_id", fmt.Sprintf("%x", event.Subject.SessionID), "path_id", fmt.Sprintf("%x", event.Subject.PathID),
		"config_revision", event.Revision, "milestone", event.Milestone, "reason", event.Reason, "useful_bytes", event.Usage.Useful,
		"transport_sent", event.Usage.TransportSent, "transport_received", event.Usage.TransportReceived,
		"provider_revocation_confirmed", event.ProviderRevocationConfirmed)
	if m.observe != nil {
		event.At = m.now()
		m.observe(event)
	}
}
