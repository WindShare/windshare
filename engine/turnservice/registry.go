// Package turnservice discovers deployment-owned TURN authority independently
// of application-relay connectivity. It never issues credentials during discovery.
package turnservice

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/turnaccount"
	"github.com/windshare/windshare/engine/turnconfig"
	"github.com/windshare/windshare/engine/turnprovider"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

const RefreshInterval = time.Minute

type Store interface {
	Update(context.Context, func(*relayconfig.Config) error) (relayconfig.Config, error)
}
type Registry struct {
	mu           sync.Mutex
	accounts     *turnaccount.Manager
	dependencies turnconfig.Dependencies
	store        Store
	references   []turnconfig.ManagedService
	persisted    []turnconfig.ManagedService
	profiles     map[string]senderrelay.Candidate
}

func New(accounts *turnaccount.Manager, dependencies turnconfig.Dependencies, store Store) *Registry {
	return &Registry{accounts: accounts, dependencies: dependencies, store: store, profiles: make(map[string]senderrelay.Candidate)}
}
func addressKey(address string) string {
	endpoint, err := v2.NormalizeRelayEndpoint(address)
	if err != nil {
		return ""
	}
	return endpoint.DialURL
}
func (registry *Registry) Configure(config relayconfig.Config) error {
	if len(config.ManagedTURN) > relayconfig.MaximumProfiles {
		return relayconfig.ErrConfig
	}
	profiles := make(map[string]senderrelay.Candidate, len(config.Profiles))
	for _, profile := range config.Profiles {
		if profile.Validate() != nil {
			return relayconfig.ErrConfig
		}
		profiles[addressKey(profile.PublicURL)] = profile
	}
	seen, addresses := map[string]bool{}, map[string]bool{}
	for _, reference := range config.ManagedTURN {
		if reference.Validate() != nil || seen[reference.ServiceID] || addresses[addressKey(reference.RelayURL)] ||
			profiles[addressKey(reference.RelayURL)].AccessKey == "" {
			return relayconfig.ErrConfig
		}
		seen[reference.ServiceID] = true
		addresses[addressKey(reference.RelayURL)] = true
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if err := registry.publish(config.ManagedTURN, profiles); err != nil {
		return err
	}
	registry.references = slices.Clone(config.ManagedTURN)
	registry.persisted = slices.Clone(config.ManagedTURN)
	registry.profiles = profiles
	return nil
}
func (registry *Registry) publish(references []turnconfig.ManagedService, profiles map[string]senderrelay.Candidate) error {
	var services []turnaccount.ManagedService
	for _, reference := range references {
		if !reference.Enabled {
			continue
		}
		profile := profiles[addressKey(reference.RelayURL)]
		if profile.AccessKey == "" {
			continue
		}
		source, err := turnprovider.NewRemote(turnprovider.RemoteConfig{RelayURL: reference.RelayURL,
			AccessKey: profile.AccessKey, ServiceID: reference.ServiceID,
			Client: registry.dependencies.Client, Now: registry.dependencies.Now})
		if err != nil {
			return err
		}
		services = append(services, turnaccount.ManagedService{ID: reference.ServiceID, RelayURL: reference.RelayURL,
			LocalBudgetBytes: reference.LocalBudgetBytes, Source: source})
	}
	return registry.accounts.SetManagedServices(services)
}

// Watch follows the configured candidates, including disconnected relays. Its
// lifetime belongs to the share task; neither link publication nor content waits.
func (registry *Registry) Watch(ctx context.Context, candidates []senderrelay.Candidate) func() {
	ctx, cancel := context.WithCancel(ctx)
	var work sync.WaitGroup
	registry.mu.Lock()
	profiles := maps.Clone(registry.profiles)
	changed := false
	for _, candidate := range candidates {
		if candidate.AccessKey != "" {
			address := addressKey(candidate.PublicURL)
			changed = changed || profiles[address].AccessKey != candidate.AccessKey
			profiles[address] = candidate
		}
	}
	if changed {
		if err := registry.publish(registry.references, profiles); err != nil {
			slog.Debug("managed TURN authority update failed", "reason", err)
		} else {
			registry.profiles = profiles
		}
	}
	registry.mu.Unlock()
	for _, candidate := range candidates {
		if candidate.AccessKey == "" {
			continue
		}
		if _, err := turnprovider.ManagedEndpoint(candidate.PublicURL); err != nil {
			continue
		}
		work.Go(func() {
			ticker := time.NewTicker(RefreshInterval)
			defer ticker.Stop()
			for {
				registry.Refresh(ctx, candidate)
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		})
	}
	return func() { cancel(); work.Wait() }
}
func (registry *Registry) trace(candidate senderrelay.Candidate, serviceID, milestone string, err error) {
	slog.Debug("managed TURN discovery", "relay_url", candidate.PublicURL, "service_id", serviceID,
		"milestone", milestone, "reason", err)
}
