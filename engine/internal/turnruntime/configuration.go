package turnruntime

import (
	"log/slog"
	"slices"
	"strings"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/connectivity/turnpath"
)

// Configuration is immutable after publication. Readers retain a value snapshot
// while calling an issuer. Its revision orders publications, not credential
// validity; each profile retains its own issuance revision across unrelated edits.
type configuration struct {
	revision                   uint64
	accounts                   map[string]account
	profiles                   map[string]profile
	services                   map[string]managedService
	accountOrder, profileOrder []string
}

func prepare(config accountConfig) (configuration, error) {
	for _, account := range config.Accounts {
		if account.Source == nil || account.Source.source == nil {
			return configuration{}, ErrConfiguration
		}
	}
	return prepareDefinitions(config)
}

func prepareDefinitions(config accountConfig) (configuration, error) {
	result := configuration{accounts: make(map[string]account), profiles: make(map[string]profile)}
	if len(config.Accounts) > MaximumAccounts || len(config.Profiles) > MaximumProfiles {
		return result, ErrConfiguration
	}
	for _, account := range config.Accounts {
		id := iceconfig.Identity{Provider: account.Provider, Account: account.ID, Profile: "validation"}
		if id.Validate() != nil || strings.HasPrefix(account.ID, managedPrefix) {
			return result, ErrConfiguration
		}
		if _, exists := result.accounts[account.ID]; exists {
			return result, ErrConfiguration
		}
		result.accounts[account.ID] = account
		result.accountOrder = append(result.accountOrder, account.ID)
	}
	for _, profile := range config.Profiles {
		account, ok := result.accounts[profile.AccountID]
		if !ok {
			return result, ErrConfiguration
		}
		if _, exists := result.profiles[profile.ID]; exists {
			return result, ErrConfiguration
		}
		id := iceconfig.Identity{Provider: account.Provider, Account: profile.AccountID, Profile: profile.ID}
		if id.Validate() != nil || len(profile.URLs) > iceconfig.MaximumURLs || strings.HasPrefix(profile.ID, managedPrefix) {
			return result, ErrConfiguration
		}
		seen := map[string]bool{}
		for _, raw := range profile.URLs {
			if iceconfig.ValidateRelayURL(raw) != nil || seen[raw] {
				return result, ErrConfiguration
			}
			seen[raw] = true
		}
		profile.URLs = slices.Clone(profile.URLs)
		result.profiles[profile.ID] = profile
		result.profileOrder = append(result.profileOrder, profile.ID)
	}
	if err := prepareServices(&result, config.Services); err != nil {
		return configuration{}, err
	}
	return result, nil
}

// Reconfigure publishes accounts and profiles atomically. Clock and observer
// dependencies remain owned by the accountManager constructed in newAccountManager.
func (m *accountManager) Reconfigure(config accountConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	config.Services = m.settings.Services
	return m.applyLocked(config)
}

func (m *accountManager) applyLocked(config accountConfig) error {
	if m.closed {
		return ErrClosed
	}
	next, err := prepare(config)
	if err != nil {
		return err
	}
	m.settings = cloneConfig(config)
	next.revision = m.current.revision + 1
	for id := range next.accounts {
		if m.ledgers[id] == nil {
			m.ledgers[id] = &accountLedger{}
		}
	}
	next.inheritProfileRevisions(m.current, m.ledgers)
	m.current = next
	canceled := 0
	for request := range m.requests {
		if cause := m.issuanceUnavailableLocked(request); cause != nil {
			request.cancel(cause)
			canceled++
		}
	}
	revoked := 0
	for lease := range m.leases {
		if cause := m.leaseUnavailableLocked(lease); cause != nil {
			lease.cancel(cause)
			revoked++
		}
	}
	close(m.changed)
	m.changed = make(chan struct{})
	slog.Debug("TURN configuration applied", "config_revision", next.revision,
		"accounts", len(next.accounts), "profiles", len(next.profiles), "canceled_requests", canceled, "revoked_leases", revoked)
	return nil
}

func (m *accountManager) readConfiguration() configuration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

func (next *configuration) inheritProfileRevisions(previous configuration, ledgers map[string]*accountLedger) {
	for id, profile := range next.profiles {
		old, exists := previous.profiles[id]
		account, prior := next.accounts[profile.AccountID], previous.accounts[old.AccountID]
		profile.revision = next.revision
		if exists && old.AccountID == profile.AccountID && prior.Provider == account.Provider &&
			prior.Source == account.Source && slices.Equal(old.URLs, profile.URLs) &&
			exhausted(prior, ledgers[profile.AccountID]) == exhausted(account, ledgers[profile.AccountID]) {
			profile.revision = old.revision
		}
		next.profiles[id] = profile
	}
}

func (m *accountManager) issuanceUnavailableLocked(request *issuance) error {
	profile, ok := m.current.profiles[request.profileID]
	if !ok || profile.revision != request.profileRevision {
		return turnpath.ErrSuperseded
	}
	if exhausted(m.current.accounts[profile.AccountID], request.account) {
		return ErrBudget
	}
	return nil
}

func (m *accountManager) leaseUnavailableLocked(lease *leaseState) error {
	profile, ok := m.current.profiles[lease.identity.Profile]
	account, enabled := m.current.accounts[lease.identity.Account]
	if !ok || !enabled || profile.AccountID != lease.identity.Account || account.Provider != lease.identity.Provider {
		return ErrConfiguration
	}
	if exhausted(account, lease.account) {
		return ErrBudget
	}
	return nil
}
