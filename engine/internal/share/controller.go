package share

import (
	"context"
	"slices"
	"sync"
)

// Controller retains readiness independently of diagnostic delivery. Publishing
// a capability and acknowledging it is the caller's explicit activation boundary.
type Controller struct {
	mu                  sync.Mutex
	ready               Ready
	readyErr            error
	readyDone           chan struct{}
	activated           chan struct{}
	activatedErr        error
	acknowledgement     chan error
	readyOnce           sync.Once
	activatedOnce       sync.Once
	acknowledgementOnce sync.Once
}

func NewController() *Controller {
	return &Controller{readyDone: make(chan struct{}), activated: make(chan struct{}), acknowledgement: make(chan error, 1)}
}

func (c *Controller) Ready(ctx context.Context) (Ready, error) {
	select {
	case <-ctx.Done():
		return Ready{}, context.Cause(ctx)
	case <-c.readyDone:
		c.mu.Lock()
		defer c.mu.Unlock()
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

func (c *Controller) Acknowledge(publicationErr error) {
	c.acknowledgementOnce.Do(func() { c.acknowledgement <- publicationErr })
}

func (c *Controller) Activated(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-c.activated:
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.activatedErr
	}
}

func (c *Controller) publish(ready Ready) {
	c.readyOnce.Do(func() {
		c.mu.Lock()
		c.ready = cloneReady(ready)
		c.mu.Unlock()
		close(c.readyDone)
	})
}

func (c *Controller) activate(ctx context.Context, prefetch func()) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case err := <-c.acknowledgement:
		if err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	prefetch()
	c.activationComplete(nil)
	return nil
}

func (c *Controller) activationComplete(err error) {
	c.activatedOnce.Do(func() {
		c.mu.Lock()
		c.activatedErr = err
		c.mu.Unlock()
		close(c.activated)
	})
}

func (c *Controller) finish(err error) {
	c.readyOnce.Do(func() {
		c.mu.Lock()
		c.readyErr = err
		c.mu.Unlock()
		close(c.readyDone)
	})
	c.activationComplete(err)
}
