package share

import (
	"context"
	"slices"
	"sync"

	"github.com/windshare/windshare/connectivity/relayset"
)

// Controller retains the first service-ready result independently of diagnostic
// delivery and caller presentation. Closing readyDone publishes immutable state.
type Controller struct {
	mu               sync.Mutex
	relayStatus      func() relayset.SenderStatus
	finalRelayStatus relayset.SenderStatus
	hasRelayStatus   bool
	ready            Ready
	readyErr         error
	readyDone        chan struct{}
	readyOnce        sync.Once
}

func NewController() *Controller {
	return &Controller{readyDone: make(chan struct{})}
}

func (c *Controller) Ready(ctx context.Context) (Ready, error) {
	select {
	case <-ctx.Done():
		return Ready{}, context.Cause(ctx)
	case <-c.readyDone:
		return cloneReady(c.ready), c.readyErr
	}
}

func cloneReady(ready Ready) Ready {
	// Ready remains queryable after publication. Neither the preparation
	// provider nor a consumer may mutate the retained capability authority.
	ready.Capability.ReadSecret = slices.Clone(ready.Capability.ReadSecret)
	ready.Capability.PKHash = slices.Clone(ready.Capability.PKHash)
	ready.Capability.Relays = slices.Clone(ready.Capability.Relays)
	ready.Capability.NodeAliases = slices.Clone(ready.Capability.NodeAliases)
	ready.Registrations.Admitted = slices.Clone(ready.Registrations.Admitted)
	ready.Registrations.Rejected = slices.Clone(ready.Registrations.Rejected)
	return ready
}

func (c *Controller) publish(ready Ready) bool {
	published := false
	c.readyOnce.Do(func() {
		published = true
		c.ready = cloneReady(ready)
		close(c.readyDone)
	})
	return published
}

// CancelBeforeReady arbitrates a replacement against the first publication.
// The old task can still be joined normally after its startup is withdrawn.
func (c *Controller) CancelBeforeReady(cancel func()) bool {
	cancelled := false
	c.readyOnce.Do(func() {
		cancelled = true
		cancel()
		c.readyErr = context.Canceled
		close(c.readyDone)
	})
	return cancelled
}

func (c *Controller) finish(err error) {
	c.mu.Lock()
	if c.relayStatus != nil {
		c.finalRelayStatus = c.relayStatus()
		c.relayStatus = nil
	}
	c.mu.Unlock()
	c.readyOnce.Do(func() {
		c.readyErr = err
		close(c.readyDone)
	})
}
