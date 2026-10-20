// Package turnaccount owns per-account authority and local budgets shared by all
// shares and receivers in one Engine. Provider billing is a separate observation.
package turnaccount

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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
	ErrBudget        = errors.New("TURN account local usage budget exhausted")
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

// QueryUsage is an explicit operator operation. Transfer and budget decisions
// never depend on provider analytics availability, sampling or rate limits.
func (m *Manager) QueryUsage(ctx context.Context, accountID string, first, last time.Time) (turnprovider.Usage, error) {
	m.mu.Lock()
	account := m.accounts[accountID]
	if m.closed || account == nil || account.config.Usage == nil {
		m.mu.Unlock()
		return turnprovider.Usage{}, ErrConfiguration
	}
	reporter := account.config.Usage
	m.work.Add(1)
	m.mu.Unlock()
	defer m.work.Done()
	return reporter.Query(ctx, first, last)
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
type accountState struct {
	config      Account
	usage       turnpath.Usage
	charged     uint64
	active      int
	exhausted   bool
	lastFailure string
	lost        chan struct{}
}
type Manager struct {
	mu       sync.Mutex
	config   Config
	accounts map[string]*accountState
	profiles map[string]Profile
	closed   bool
	work     sync.WaitGroup
}

func New(config Config) (*Manager, error) {
	if len(config.Accounts) > MaximumAccounts || len(config.Profiles) > MaximumProfiles {
		return nil, ErrConfiguration
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	config.Accounts = slices.Clone(config.Accounts)
	config.Profiles = slices.Clone(config.Profiles)
	manager := &Manager{config: config, accounts: make(map[string]*accountState), profiles: make(map[string]Profile)}
	for _, account := range config.Accounts {
		id := iceconfig.Identity{Provider: account.Provider, Account: account.ID, Profile: "validation"}
		if id.Validate() != nil || account.Source == nil || manager.accounts[account.ID] != nil {
			return nil, ErrConfiguration
		}
		manager.accounts[account.ID] = &accountState{config: account, lost: make(chan struct{})}
	}
	for _, profile := range config.Profiles {
		account := manager.accounts[profile.AccountID]
		if account == nil {
			return nil, ErrConfiguration
		}
		if _, exists := manager.profiles[profile.ID]; exists {
			return nil, ErrConfiguration
		}
		id := iceconfig.Identity{Provider: account.config.Provider, Account: profile.AccountID, Profile: profile.ID}
		if id.Validate() != nil || len(profile.URLs) > iceconfig.MaximumURLs {
			return nil, ErrConfiguration
		}
		seen := map[string]bool{}
		for _, raw := range profile.URLs {
			if iceconfig.ValidateRelayURL(raw) != nil || seen[raw] {
				return nil, ErrConfiguration
			}
			seen[raw] = true
		}
		profile.URLs = slices.Clone(profile.URLs)
		manager.profiles[profile.ID] = profile
	}
	return manager, nil
}

func (m *Manager) Profiles() []turnpath.Profile {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]turnpath.Profile, 0, len(m.profiles))
	for _, configured := range m.config.Profiles {
		account := m.accounts[configured.AccountID]
		result = append(result, turnpath.Profile{Identity: iceconfig.Identity{
			Provider: account.config.Provider, Account: configured.AccountID, Profile: configured.ID}})
	}
	return result
}

func (m *Manager) Acquire(ctx context.Context, profileID string, subject turnpath.Subject) (*turnpath.Lease, error) {
	return m.acquire(ctx, profileID, subject, nil)
}
func (m *Manager) acquire(ctx context.Context, profileID string, subject turnpath.Subject, previous *turnprovider.Issued) (*turnpath.Lease, error) {
	m.mu.Lock()
	profile, ok := m.profiles[profileID]
	account := m.accounts[profile.AccountID]
	if m.closed || !ok || account.exhausted || account.active >= MaximumActiveLeases {
		err := ErrConfiguration
		switch {
		case m.closed:
			err = ErrClosed
		case ok && account.exhausted:
			err = ErrBudget
		case ok && account.active >= MaximumActiveLeases:
			err = ErrCapacity
		}
		m.mu.Unlock()
		return nil, err
	}
	account.active++
	m.work.Add(1)
	identity := iceconfig.Identity{Provider: account.config.Provider, Account: profile.AccountID, Profile: profile.ID}
	m.mu.Unlock()
	defer m.work.Done()
	m.emit(Event{Identity: identity, Subject: subject, Milestone: "issuance_started"})
	request, cancel := context.WithTimeout(ctx, IssuanceTimeout)
	defer cancel()
	providerSubject := turnprovider.Subject{SessionID: subject.SessionID, PathID: subject.PathID}
	var issued turnprovider.Issued
	var err error
	if previous != nil {
		source, ok := account.config.Source.(renewable)
		if !ok {
			err = ErrConfiguration
		} else {
			issued, err = source.Renew(request, providerSubject, *previous)
		}
	} else {
		issued, err = account.config.Source.Issue(request, providerSubject)
	}
	if err == nil && !issued.Credentials.ValidAt(m.config.Now()) {
		err = turnprovider.ErrExpired
	}
	urls := profile.URLs
	if len(urls) == 0 {
		urls = issued.URLs
	}
	var relay iceconfig.Relay
	if err == nil {
		relay, err = iceconfig.NewRelay(identity, urls, issued.Credentials)
	}
	m.mu.Lock()
	if err == nil && (m.closed || account.exhausted || ctx.Err() != nil) {
		err = errors.Join(ErrClosed, ctx.Err())
	}
	if err != nil {
		account.active--
		account.lastFailure = err.Error()
		m.mu.Unlock()
		m.emit(Event{Identity: identity, Subject: subject, Milestone: "issuance_failed", Reason: err.Error()})
		if issued.Credentials.Kind() != 0 {
			m.revoke(account, issued, identity, subject)
		}
		return nil, err
	}
	account.lastFailure = ""
	// Keep a work reference throughout the lease lifetime. Close can then join
	// revocation without racing a worker added after its wait has begun.
	m.work.Add(1)
	m.mu.Unlock()
	state := &leaseState{manager: m, account: account, identity: identity, subject: subject, issued: issued}
	leaseConfig := turnpath.LeaseConfig{Relay: relay, Lost: account.lost, Charge: state.charge, Release: state.release}
	if _, ok := account.config.Source.(renewable); ok {
		leaseConfig.Renew = func(ctx context.Context, next turnpath.Subject) (*turnpath.Lease, error) {
			return m.acquire(ctx, profileID, next, &issued)
		}
	}
	lease := turnpath.NewLease(leaseConfig)
	m.emit(Event{Identity: identity, Subject: subject, Milestone: "credentials_issued"})
	return lease, nil
}

func (m *Manager) Snapshot() []Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]Snapshot, 0, len(m.config.Accounts))
	for _, configured := range m.config.Accounts {
		state := m.accounts[configured.ID]
		result = append(result, Snapshot{AccountID: configured.ID, Provider: configured.Provider,
			LocalBudgetBytes: configured.LocalBudgetBytes, Usage: state.usage, ChargedBytes: state.charged,
			ActiveLeases: state.active, Exhausted: state.exhausted, LastFailure: state.lastFailure})
	}
	return result
}
func (m *Manager) Close() error {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		for _, account := range m.accounts {
			if !account.exhausted {
				close(account.lost)
			}
		}
	}
	m.mu.Unlock()
	m.work.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, account := range m.accounts {
		if account.active != 0 {
			return fmt.Errorf("TURN account %s retained %d leases", account.config.ID, account.active)
		}
	}
	return nil
}
func (m *Manager) emit(event Event) {
	slog.Debug("TURN account", "provider", event.Identity.Provider, "account", event.Identity.Account, "profile", event.Identity.Profile,
		"session_id", fmt.Sprintf("%x", event.Subject.SessionID), "path_id", fmt.Sprintf("%x", event.Subject.PathID),
		"milestone", event.Milestone, "reason", event.Reason, "useful_bytes", event.Usage.Useful,
		"transport_sent", event.Usage.TransportSent, "transport_received", event.Usage.TransportReceived,
		"provider_revocation_confirmed", event.ProviderRevocationConfirmed)
	if m.config.Observe != nil {
		event.At = m.config.Now()
		m.config.Observe(event)
	}
}
