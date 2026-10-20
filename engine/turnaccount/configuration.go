package turnaccount

import (
	"log/slog"
	"slices"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/connectivity/turnpath"
)

// Configuration is immutable after publication. Readers retain a value snapshot
// while calling an issuer; only the ledger is shared across revisions.
type configuration struct {
	revision                   uint64
	accounts                   map[string]Account
	profiles                   map[string]Profile
	accountOrder, profileOrder []string
}

func prepare(config Config) (configuration, error) {
	result := configuration{accounts: make(map[string]Account), profiles: make(map[string]Profile)}
	if len(config.Accounts) > MaximumAccounts || len(config.Profiles) > MaximumProfiles {
		return result, ErrConfiguration
	}
	for _, account := range config.Accounts {
		id := iceconfig.Identity{Provider: account.Provider, Account: account.ID, Profile: "validation"}
		if id.Validate() != nil || account.Source == nil {
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
		if id.Validate() != nil || len(profile.URLs) > iceconfig.MaximumURLs {
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
	return result, nil
}

// Reconfigure publishes accounts and profiles atomically. Clock and observer
// dependencies remain owned by the Manager constructed in New.
func (m *Manager) Reconfigure(config Config) error {
	next, err := prepare(config)
	if err != nil {
		return err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	next.revision = m.current.revision + 1
	for id := range next.accounts {
		if m.ledgers[id] == nil {
			m.ledgers[id] = &accountLedger{}
		}
	}
	m.current = next
	// A result issued under an old configuration must never install authority
	// after a settings change. Its captured source still owns any revocation.
	for request := range m.requests {
		request.cancel(turnpath.ErrSuperseded)
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
	m.mu.Unlock()
	slog.Debug("TURN configuration applied", "config_revision", next.revision,
		"accounts", len(next.accounts), "profiles", len(next.profiles), "revoked_leases", revoked)
	return nil
}

func (m *Manager) leaseUnavailableLocked(lease *leaseState) error {
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
