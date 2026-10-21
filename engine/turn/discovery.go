package turn

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/engine/turn/internal/provider"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

const refreshInterval = time.Minute

type service struct {
	refresh    *sync.Mutex
	candidate  senderrelay.Candidate
	capability *Capability
	dirty      bool
	generation uint64
	cancel     context.CancelFunc
	done       chan struct{}
}
type remoteAuthority struct {
	address, accessKey, serviceID string
}
type publishedAuthority struct {
	remoteAuthority
	budget uint64
}
type discovery struct {
	mu           sync.Mutex
	accounts     *accountManager
	dependencies Dependencies
	cache        Cache
	entries      map[string]*service
	budgets      map[string]uint64
	demand       map[string]int
	workers      sync.WaitGroup
	closed       bool
	published    []publishedAuthority
}

func newDiscovery(accounts *accountManager, dependencies Dependencies) *discovery {
	return &discovery{accounts: accounts, dependencies: dependencies, cache: dependencies.Cache,
		entries: make(map[string]*service), budgets: make(map[string]uint64), demand: make(map[string]int)}
}
func addressKey(address string) string {
	endpoint, err := v2.NormalizeRelayEndpoint(address)
	if err != nil {
		return ""
	}
	return endpoint.DialURL
}
func eligible(candidate senderrelay.Candidate) bool {
	if candidate.AccessKey == "" || candidate.Validate() != nil {
		return false
	}
	_, err := provider.ManagedEndpoint(candidate.PublicURL)
	return err == nil
}
func (registry *discovery) newService(candidate senderrelay.Candidate) *service {
	current := &service{candidate: candidate, refresh: &sync.Mutex{}}
	if registry.cache != nil {
		capability, err := registry.cache.Load(candidate)
		if err == nil && capability != nil {
			err = capability.Validate()
			if err == nil {
				value := *capability
				current.capability = &value
			}
		}
		if err != nil {
			registry.trace(candidate, "", "cache_unavailable", err)
		}
	}
	return current
}

// configure stages discovery entries before replacing the full account snapshot.
// Neither bad settings nor a rejected publication can partially install authority.
func (registry *discovery) configure(manual accountConfig, settings ManagedSettings) error {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.closed {
		return ErrClosed
	}
	entries := make(map[string]*service)
	budgets := make(map[string]uint64)
	for _, policy := range settings.Policies {
		budgets[addressKey(policy.RelayURL)] = policy.LocalBudgetBytes
	}
	seen := make(map[string]bool)
	for _, candidate := range settings.Relays {
		if !eligible(candidate) {
			continue
		}
		address := addressKey(candidate.PublicURL)
		current := registry.entries[address]
		if current == nil || current.candidate.AccessKey != candidate.AccessKey {
			next := registry.newService(candidate)
			if current != nil {
				// Old and new authority serialize HTTP requests, but stale results
				// retain the old entry identity and cannot publish into this one.
				next.refresh = current.refresh
			}
			current = next
		}
		if current.capability != nil {
			if seen[current.capability.ServiceID] {
				current = &service{candidate: candidate, refresh: current.refresh}
			} else {
				seen[current.capability.ServiceID] = true
			}
		}
		entries[address] = current
	}
	services, published, err := registry.services(entries, budgets)
	if err != nil {
		return err
	}
	manual.Services = services
	registry.accounts.mu.Lock()
	err = registry.accounts.applyLocked(manual)
	registry.accounts.mu.Unlock()
	if err != nil {
		return err
	}
	for address, current := range registry.entries {
		if entries[address] != current {
			registry.stopLocked(current)
		}
	}
	registry.entries, registry.budgets, registry.published = entries, budgets, published
	for address, current := range entries {
		if registry.demand[address] > 0 && current.cancel == nil {
			registry.startLocked(current)
		}
	}
	return nil
}

func (registry *discovery) services(entries map[string]*service, budgets map[string]uint64) ([]managedService, []publishedAuthority, error) {
	previous := registry.accounts.readConfiguration()
	addresses := make([]string, 0, len(entries))
	for address := range entries {
		addresses = append(addresses, address)
	}
	slices.Sort(addresses)
	var services []managedService
	var published []publishedAuthority
	for _, address := range addresses {
		current := entries[address]
		if current.capability == nil || !current.capability.Enabled {
			continue
		}
		identity := remoteAuthority{address: address, accessKey: current.candidate.AccessKey, serviceID: current.capability.ServiceID}
		factory := planIssuer(identity, func(dependencies Dependencies) (credentialSource, error) {
			return provider.NewRemote(provider.RemoteConfig{
				RelayURL: current.candidate.PublicURL, AccessKey: identity.accessKey, ServiceID: identity.serviceID,
				Client: dependencies.Client, Now: dependencies.Now,
			})
		})
		source, err := factory(registry.dependencies, previous.accounts[managedIdentity(identity.serviceID)].Source)
		if err != nil {
			return nil, nil, err
		}
		published = append(published, publishedAuthority{remoteAuthority: identity, budget: budgets[address]})
		services = append(services, managedService{ID: current.capability.ServiceID, RelayURL: current.candidate.PublicURL,
			LocalBudgetBytes: budgets[address], Source: source})
	}
	return services, published, nil
}

func (registry *discovery) publish() error {
	services, published, err := registry.services(registry.entries, registry.budgets)
	if err != nil {
		return err
	}
	if slices.Equal(registry.published, published) {
		return nil
	}
	if err := registry.accounts.SetManagedServices(services); err != nil {
		return err
	}
	registry.published = published
	return nil
}

func (registry *discovery) Close() {
	registry.mu.Lock()
	registry.closed = true
	for _, current := range registry.entries {
		registry.stopLocked(current)
	}
	registry.mu.Unlock()
	registry.workers.Wait()
}
func (registry *discovery) trace(candidate senderrelay.Candidate, serviceID, milestone string, err error) {
	slog.Debug("managed TURN discovery", "relay_url", addressKey(candidate.PublicURL), "service_id", serviceID,
		"milestone", milestone, "reason", err)
}
