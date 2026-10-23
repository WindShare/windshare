package turnruntime

import (
	"context"
	"errors"

	"github.com/windshare/windshare/engine/internal/turnruntime/internal/provider"
	"github.com/windshare/windshare/engine/relayprofile"
)

func (registry *discovery) Refresh(ctx context.Context, candidate relayprofile.Candidate) {
	address := addressKey(candidate.PublicURL)
	registry.mu.Lock()
	current := registry.entries[address]
	if registry.closed || current == nil || current.candidate.AccessKey != candidate.AccessKey {
		registry.mu.Unlock()
		return
	}
	generation := current.generation
	registry.mu.Unlock()
	// Manual refresh and the shared worker cannot race requests for one relay.
	current.refresh.Lock()
	defer current.refresh.Unlock()
	registry.mu.Lock()
	valid := registry.current(current, address, generation)
	registry.mu.Unlock()
	if !valid || ctx.Err() != nil {
		return
	}
	requestedAt := registry.dependencies.Now()
	registry.trace(candidate, "", "started", nil)
	observed, err := provider.Discover(ctx, registry.dependencies.Client, candidate.PublicURL, candidate.AccessKey)
	var capability *Capability
	if observed != nil {
		value := Capability(*observed)
		capability = &value
	}
	registry.mu.Lock()
	if !registry.current(current, address, generation) || ctx.Err() != nil {
		registry.mu.Unlock()
		registry.trace(candidate, "", "superseded", ctx.Err())
		return
	}
	switch {
	case errors.Is(err, provider.ErrAuthorityWithdrawn):
		if current.capability == nil {
			registry.mu.Unlock()
			return
		}
		withdrawn := *current.capability
		withdrawn.Enabled = false
		capability = &withdrawn
	case err != nil:
		registry.mu.Unlock()
		registry.trace(candidate, "", "retained", err)
		return
	case capability == nil:
		registry.mu.Unlock()
		registry.trace(candidate, "", "unavailable", nil)
		return
	}
	for _, other := range registry.entries {
		if other != current && other.capability != nil && other.capability.ServiceID == capability.ServiceID {
			registry.mu.Unlock()
			registry.trace(candidate, capability.ServiceID, "duplicate_service", provider.ErrConfiguration)
			return
		}
	}
	changed := current.capability == nil || *current.capability != *capability
	if changed {
		before := current.capability
		current.capability = capability
		if err := registry.publish(); err != nil {
			current.capability = before
			registry.mu.Unlock()
			registry.trace(candidate, capability.ServiceID, "publication_failed", err)
			return
		}
		current.dirty = true
	}
	persist := current.dirty && registry.cache != nil
	registry.mu.Unlock()
	// Runtime withdrawal precedes optional persistence. A failed cache write cannot
	// keep revoked authority alive or interrupt healthy paths when retried.
	if persist {
		err := registry.cache.Save(ctx, candidate, *capability, requestedAt)
		registry.mu.Lock()
		if err == nil && registry.current(current, address, generation) {
			current.dirty = false
		}
		registry.mu.Unlock()
		if err != nil {
			registry.trace(candidate, capability.ServiceID, "persistence_failed", err)
		}
	}
	milestone := "unchanged"
	if changed {
		milestone = "published"
	}
	registry.trace(candidate, capability.ServiceID, milestone, nil)
}

func (registry *discovery) current(value *service, address string, generation uint64) bool {
	return !registry.closed && registry.entries[address] == value && value.generation == generation
}
