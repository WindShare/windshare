package turnruntime

import (
	"context"
	"slices"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/connectivity/turnpath"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

const (
	MaximumManagedServices = 64
	managedPrefix          = "managed."
)

type managedService struct {
	ID               string
	RelayURL         string
	LocalBudgetBytes uint64
	Source           *credentialIssuer
}

func managedIdentity(id string) string { return managedPrefix + id }
func prepareServices(result *configuration, services []managedService) error {
	if len(services) > MaximumManagedServices {
		return ErrConfiguration
	}
	result.services = make(map[string]managedService, len(services))
	for _, service := range services {
		id := managedIdentity(service.ID)
		identity := iceconfig.Identity{Provider: "cloudflare", Account: id, Profile: id}
		if identity.Validate() != nil || service.Source == nil || service.Source.source == nil {
			return ErrConfiguration
		}
		endpoint, err := v2.NormalizeRelayEndpoint(service.RelayURL)
		if err != nil {
			return ErrConfiguration
		}
		if _, exists := result.accounts[id]; exists {
			return ErrConfiguration
		}
		service.RelayURL = endpoint.DialURL
		result.services[id] = service
		result.accounts[id] = account{ID: id, Provider: identity.Provider, Source: service.Source, LocalBudgetBytes: service.LocalBudgetBytes}
		result.profiles[id] = profile{ID: id, AccountID: id}
		result.accountOrder = append(result.accountOrder, id)
		result.profileOrder = append(result.profileOrder, id)
	}
	return nil
}
func cloneConfig(config accountConfig) accountConfig {
	config.Accounts = slices.Clone(config.Accounts)
	config.Profiles = slices.Clone(config.Profiles)
	for i := range config.Profiles {
		config.Profiles[i].URLs = slices.Clone(config.Profiles[i].URLs)
	}
	config.Services = slices.Clone(config.Services)
	return config
}

// SetManagedServices replaces discovered authority without rewriting the manual
// account configuration or resetting either kind of Engine-lifetime budget.
func (m *accountManager) SetManagedServices(services []managedService) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	config := m.settings
	config.Services = services
	return m.applyLocked(config)
}

type Selection struct {
	manager *accountManager
	relays  map[string]bool
}

func (m *accountManager) Select(relays []string) *Selection {
	selection := &Selection{manager: m, relays: make(map[string]bool)}
	for _, address := range relays {
		if endpoint, err := v2.NormalizeRelayEndpoint(address); err == nil {
			selection.relays[endpoint.DialURL] = true
		}
	}
	return selection
}
func (selection *Selection) Inventory() turnpath.Inventory {
	m := selection.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	inventory := turnpath.Inventory{Revision: m.current.revision, Changed: m.changed}
	var managed, manual []turnpath.Profile
	for _, id := range m.current.profileOrder {
		profile := m.current.profiles[id]
		account := m.current.accounts[profile.AccountID]
		value := turnpath.Profile{Revision: profile.revision, Identity: iceconfig.Identity{Provider: account.Provider, Account: account.ID, Profile: id}}
		if service, ok := m.current.services[id]; ok {
			if selection.relays[service.RelayURL] {
				managed = append(managed, value)
			}
		} else {
			manual = append(manual, value)
		}
	}
	// Inventory retention and session admission are different limits. Interleave
	// independent manual and deployment services so a full manual list cannot
	// prevent a discovered deployment from participating.
	for i := 0; len(inventory.Profiles) < MaximumProfiles && (i < len(managed) || i < len(manual)); i++ {
		if i < len(managed) {
			inventory.Profiles = append(inventory.Profiles, managed[i])
		}
		if i < len(manual) && len(inventory.Profiles) < MaximumProfiles {
			inventory.Profiles = append(inventory.Profiles, manual[i])
		}
	}
	return inventory
}
func (selection *Selection) Acquire(ctx context.Context, id string, subject turnpath.Subject) (*turnpath.Lease, error) {
	for _, profile := range selection.Inventory().Profiles {
		if profile.Identity.Profile == id {
			return selection.manager.Acquire(ctx, id, subject)
		}
	}
	return nil, ErrConfiguration
}
