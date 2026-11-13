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
	mu                sync.Mutex
	availability      func() relayset.SenderAvailability
	finalAvailability relayset.SenderAvailability
	hasAvailability   bool
	ready             Ready
	readyErr          error
	readyDone         chan struct{}
	readyOnce         sync.Once
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
	ready.Admissions = slices.Clone(ready.Admissions)
	return ready
}

func (c *Controller) publish(ready Ready) {
	c.readyOnce.Do(func() {
		c.ready = cloneReady(ready)
		close(c.readyDone)
	})
}

func (c *Controller) finish(err error) {
	c.mu.Lock()
	if c.availability != nil {
		c.finalAvailability = c.availability()
		c.availability = nil
	}
	c.mu.Unlock()
	c.readyOnce.Do(func() {
		c.readyErr = err
		close(c.readyDone)
	})
}
