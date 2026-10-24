package share

import (
	"context"
	"slices"
	"sync"
)

// Controller retains the first service-ready result independently of diagnostic
// delivery and caller presentation. Closing readyDone publishes immutable state.
type Controller struct {
	ready     Ready
	readyErr  error
	readyDone chan struct{}
	readyOnce sync.Once
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
	return ready
}

func (c *Controller) publish(ready Ready) {
	c.readyOnce.Do(func() {
		c.ready = cloneReady(ready)
		close(c.readyDone)
	})
}

func (c *Controller) finish(err error) {
	c.readyOnce.Do(func() {
		c.readyErr = err
		close(c.readyDone)
	})
}
