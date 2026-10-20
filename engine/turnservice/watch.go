package turnservice

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/windshare/windshare/connectivity/senderrelay"
)

// Watch registers demand. The Registry owns one worker per relay, so cancelling
// one share cannot cancel discovery still needed by another share.
func (registry *Registry) Watch(ctx context.Context, candidates []senderrelay.Candidate) func() {
	if ctx.Err() != nil {
		return func() {}
	}
	registry.mu.Lock()
	if registry.closed {
		registry.mu.Unlock()
		return func() {}
	}
	var acquired []string
	seen := make(map[string]bool)
	for _, candidate := range candidates {
		if !eligible(candidate) {
			continue
		}
		address := addressKey(candidate.PublicURL)
		if seen[address] {
			continue
		}
		seen[address] = true
		current := registry.entries[address]
		if current == nil {
			current = registry.newService(candidate)
			for _, other := range registry.entries {
				if current.capability != nil && other.capability != nil && current.capability.ServiceID == other.capability.ServiceID {
					current.capability = nil
					break
				}
			}
			registry.entries[address] = current
		}
		// A share request captured before reconfiguration cannot restore old authority.
		registry.demand[address]++
		acquired = append(acquired, address)
		if current.cancel == nil {
			registry.startLocked(current)
		}
	}
	if err := registry.publish(); err != nil {
		for _, address := range acquired {
			registry.trace(registry.entries[address].candidate, "", "publication_failed", err)
		}
	}
	registry.mu.Unlock()
	return registry.subscription(ctx, acquired)
}

func (registry *Registry) subscription(ctx context.Context, addresses []string) func() {
	var once sync.Once
	release := func() { once.Do(func() { registry.release(addresses) }) }
	stopContext := context.AfterFunc(ctx, release)
	return func() { stopContext(); release() }
}

func (registry *Registry) release(addresses []string) {
	var stopped []<-chan struct{}
	registry.mu.Lock()
	for _, address := range addresses {
		registry.demand[address]--
		if registry.demand[address] != 0 {
			continue
		}
		delete(registry.demand, address)
		if current := registry.entries[address]; current != nil {
			registry.stopLocked(current)
			if current.done != nil {
				stopped = append(stopped, current.done)
			}
		}
	}
	registry.mu.Unlock()
	for _, done := range stopped {
		<-done
	}
}

func (registry *Registry) stopLocked(current *service) {
	current.generation++
	if current.cancel != nil {
		current.cancel()
		current.cancel = nil
		slog.Debug("managed TURN watch", "relay_url", addressKey(current.candidate.PublicURL),
			"generation", current.generation, "subscribers", registry.demand[addressKey(current.candidate.PublicURL)], "milestone", "stopped")
	}
}
func (registry *Registry) startLocked(current *service) {
	ctx, cancel := context.WithCancel(context.Background())
	current.generation++
	current.cancel = cancel
	done := make(chan struct{})
	current.done = done
	candidate := current.candidate
	slog.Debug("managed TURN watch", "relay_url", addressKey(candidate.PublicURL),
		"generation", current.generation, "subscribers", registry.demand[addressKey(candidate.PublicURL)], "milestone", "started")
	registry.workers.Go(func() {
		defer close(done)
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
