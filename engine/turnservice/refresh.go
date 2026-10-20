package turnservice

import (
	"context"
	"errors"
	"slices"

	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/turnconfig"
	"github.com/windshare/windshare/engine/turnprovider"
)

var errSuperseded = errors.New("managed TURN discovery superseded by settings")

func referenceAt(values []turnconfig.ManagedService, address string) (turnconfig.ManagedService, bool) {
	for _, value := range values {
		if addressKey(value.RelayURL) == address {
			return value, true
		}
	}
	return turnconfig.ManagedService{}, false
}
func replaceReference(values []turnconfig.ManagedService, next turnconfig.ManagedService) []turnconfig.ManagedService {
	values = slices.Clone(values)
	for i, value := range values {
		if addressKey(value.RelayURL) == addressKey(next.RelayURL) {
			values[i] = next
			return values
		}
	}
	return append(values, next)
}

func (registry *Registry) Refresh(ctx context.Context, candidate senderrelay.Candidate) {
	address := addressKey(candidate.PublicURL)
	registry.mu.Lock()
	before, existed := referenceAt(registry.references, address)
	persisted, wasSaved := referenceAt(registry.persisted, address)
	configured := registry.profiles[address]
	registry.mu.Unlock()
	if configured.AccessKey != candidate.AccessKey {
		return
	}
	registry.trace(candidate, before.ServiceID, "started", nil)
	capability, err := turnprovider.Discover(ctx, registry.dependencies.Client, candidate.PublicURL, candidate.AccessKey)
	next := before
	switch {
	case errors.Is(err, turnprovider.ErrAuthorityWithdrawn):
		if !existed {
			return
		}
		next.Enabled = false
	case err != nil:
		registry.trace(candidate, before.ServiceID, "retained", err)
		return
	case capability == nil:
		// Missing provisioning is not an explicit withdrawal of existing leases.
		registry.trace(candidate, before.ServiceID, "unavailable", nil)
		return
	default:
		next.ServiceID, next.Enabled, next.RelayURL = capability.ServiceID, capability.Enabled, candidate.PublicURL
	}
	if next.Validate() != nil {
		registry.trace(candidate, before.ServiceID, "invalid", turnconfig.ErrConfiguration)
		return
	}
	if existed && before == next && (registry.store == nil || wasSaved && persisted == next) {
		registry.trace(candidate, next.ServiceID, "unchanged", nil)
		return
	}
	if ctx.Err() != nil {
		return
	}
	savedNext := false
	if registry.store != nil {
		_, saveErr := registry.store.Update(ctx, func(current *relayconfig.Config) error {
			matches := slices.ContainsFunc(current.Profiles, func(profile senderrelay.Candidate) bool {
				return addressKey(profile.PublicURL) == address && profile.AccessKey == candidate.AccessKey
			})
			saved, present := referenceAt(current.ManagedTURN, address)
			// A local budget edit must not obstruct an authority withdrawal or be
			// overwritten by a discovery request that started before that edit.
			baseline := persisted
			baseline.LocalBudgetBytes = saved.LocalBudgetBytes
			next.LocalBudgetBytes = saved.LocalBudgetBytes
			if !matches || (saved != next && (present != wasSaved || saved != baseline)) {
				return errSuperseded
			}
			current.ManagedTURN = replaceReference(current.ManagedTURN, next)
			return nil
		})
		if errors.Is(saveErr, errSuperseded) {
			registry.trace(candidate, next.ServiceID, "superseded", saveErr)
			return
		}
		if saveErr != nil {
			registry.trace(candidate, next.ServiceID, "persistence_failed", saveErr)
		} else {
			savedNext = true
		}
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	current, present := referenceAt(registry.references, address)
	if registry.profiles[address].AccessKey != candidate.AccessKey || present != existed || current != before {
		return
	}
	if savedNext {
		registry.persisted = replaceReference(registry.persisted, next)
	}
	if existed && before == next {
		return
	}
	references := replaceReference(registry.references, next)
	if err := registry.publish(references, registry.profiles); err != nil {
		registry.trace(candidate, next.ServiceID, "publication_failed", err)
		return
	}
	registry.references = references
	registry.trace(candidate, next.ServiceID, "published", nil)
}
