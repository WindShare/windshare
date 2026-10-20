// Package turnservice shares demand-driven TURN discovery within one Engine.
package turnservice

import (
	"context"
	"errors"
	"log/slog"
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

type Cache interface {
	Load(senderrelay.Candidate) (*turnprovider.Capability, error)
	Save(context.Context, senderrelay.Candidate, turnprovider.Capability, time.Time) error
}
type service struct {
	refresh    sync.Mutex
	candidate  senderrelay.Candidate
	capability *turnprovider.Capability
	dirty      bool
	generation uint64
	cancel     context.CancelFunc
	done       chan struct{}
}
type publishedAuthority struct {
	address, accessKey, serviceID string
	budget                        uint64
}
type Registry struct {
	mu           sync.Mutex
	accounts     *turnaccount.Manager
	dependencies turnconfig.Dependencies
	cache        Cache
	entries      map[string]*service
	budgets      map[string]uint64
	demand       map[string]int
	workers      sync.WaitGroup
	closed       bool
	published    []publishedAuthority
}

func New(accounts *turnaccount.Manager, dependencies turnconfig.Dependencies, cache Cache) *Registry {
	if dependencies.Now == nil {
		dependencies.Now = time.Now
	}
	return &Registry{accounts: accounts, dependencies: dependencies, cache: cache, entries: make(map[string]*service), budgets: make(map[string]uint64), demand: make(map[string]int)}
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
	_, err := turnprovider.ManagedEndpoint(candidate.PublicURL)
	return err == nil
}
func (registry *Registry) newService(candidate senderrelay.Candidate) *service {
	current := &service{candidate: candidate}
	if registry.cache != nil {
		capability, err := registry.cache.Load(candidate)
		if err != nil {
			registry.trace(candidate, "", "cache_unavailable", err)
		}
		if capability != nil && turnprovider.ValidServiceID(capability.ServiceID) {
			current.capability = capability
		}
	}
	return current
}
func (registry *Registry) Configure(config relayconfig.Config) error {
	if config.Profiles == nil {
		config.Profiles = []senderrelay.Candidate{}
	}
	if err := config.Validate(); err != nil {
		return err
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.closed {
		return errors.New("TURN discovery is closed")
	}
	entries := make(map[string]*service)
	budgets := make(map[string]uint64)
	for _, policy := range config.ManagedTURNPolicies {
		budgets[addressKey(policy.RelayURL)] = policy.LocalBudgetBytes
	}
	seen := make(map[string]bool)
	for _, candidate := range config.Profiles {
		if !eligible(candidate) {
			continue
		}
		address := addressKey(candidate.PublicURL)
		current := registry.entries[address]
		if current == nil || current.candidate.AccessKey != candidate.AccessKey {
			next := registry.newService(candidate)
			if current != nil {
				registry.stopLocked(current)
				// Retain the request serializer while changing authority generations.
				current.candidate, current.capability, current.dirty = next.candidate, next.capability, false
			} else {
				current = next
			}
		}
		if current.capability != nil {
			if seen[current.capability.ServiceID] {
				current.capability = nil
			} else {
				seen[current.capability.ServiceID] = true
			}
		}
		entries[address] = current
	}
	for address, current := range registry.entries {
		if entries[address] != current {
			registry.stopLocked(current)
		}
	}
	registry.entries, registry.budgets = entries, budgets
	if err := registry.publish(); err != nil {
		return err
	}
	for address, current := range registry.entries {
		if registry.demand[address] > 0 && current.cancel == nil {
			registry.startLocked(current)
		}
	}
	return nil
}

func (registry *Registry) publish() error {
	addresses := make([]string, 0, len(registry.entries))
	for address := range registry.entries {
		addresses = append(addresses, address)
	}
	slices.Sort(addresses)
	var services []turnaccount.ManagedService
	var published []publishedAuthority
	for _, address := range addresses {
		current := registry.entries[address]
		if current.capability == nil || !current.capability.Enabled {
			continue
		}
		published = append(published, publishedAuthority{address: address, accessKey: current.candidate.AccessKey, serviceID: current.capability.ServiceID, budget: registry.budgets[address]})
		source, err := turnprovider.NewRemote(turnprovider.RemoteConfig{
			RelayURL: current.candidate.PublicURL, AccessKey: current.candidate.AccessKey,
			ServiceID: current.capability.ServiceID, Client: registry.dependencies.Client, Now: registry.dependencies.Now,
		})
		if err != nil {
			return err
		}
		services = append(services, turnaccount.ManagedService{ID: current.capability.ServiceID, RelayURL: current.candidate.PublicURL,
			LocalBudgetBytes: registry.budgets[address], Source: source})
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

func (registry *Registry) Close() {
	registry.mu.Lock()
	registry.closed = true
	for _, current := range registry.entries {
		registry.stopLocked(current)
	}
	registry.mu.Unlock()
	registry.workers.Wait()
}
func (registry *Registry) trace(candidate senderrelay.Candidate, serviceID, milestone string, err error) {
	slog.Debug("managed TURN discovery", "relay_url", addressKey(candidate.PublicURL), "service_id", serviceID,
		"milestone", milestone, "reason", err)
}
