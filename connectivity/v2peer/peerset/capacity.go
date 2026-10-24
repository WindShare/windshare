package peerset

import (
	"context"
	"sync"

	"github.com/windshare/windshare/core/capacityledger"
)

// Capacity is injected by the application and is FIFO across paths and receivers.
// An admitted lane releases its attempt slot while retaining its separate
// provider/socket ownership.
type Capacity struct {
	mu     sync.Mutex
	ledger *capacityledger.Ledger
	queue  []*capacityWaiter
}

type capacityWaiter struct {
	ready chan struct{}
	hold  capacityledger.Hold
}

func NewCapacity(limit int) (*Capacity, error) {
	if limit <= 0 {
		return nil, ErrConfig
	}
	return &Capacity{ledger: capacityledger.New(uint64(limit))}, nil
}

func (c *Capacity) acquire(ctx context.Context) (func(), error) {
	waiter := &capacityWaiter{ready: make(chan struct{})}
	c.mu.Lock()
	c.queue = append(c.queue, waiter)
	c.dispatchLocked()
	c.mu.Unlock()
	select {
	case <-waiter.ready:
	case <-ctx.Done():
		c.mu.Lock()
		if waiter.hold.Release() {
			// Cancellation can win after dispatch has already charged the slot.
			c.dispatchLocked()
		} else {
			for i, item := range c.queue {
				if item == waiter {
					c.queue = append(c.queue[:i], c.queue[i+1:]...)
					break
				}
			}
		}
		c.mu.Unlock()
		return nil, context.Cause(ctx)
	}
	return func() { c.release(waiter.hold) }, nil
}

func (c *Capacity) tryExplore() func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Exploration must leave one slot for ordinary demand and cannot jump a queue.
	if len(c.queue) > 0 || c.ledger.Available() <= 1 {
		return nil
	}
	hold, _ := c.ledger.Reserve(1)
	return func() { c.release(hold) }
}

func (c *Capacity) release(hold capacityledger.Hold) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if hold.Release() {
		c.dispatchLocked()
	}
}

func (c *Capacity) dispatchLocked() {
	for len(c.queue) > 0 {
		hold, ok := c.ledger.Reserve(1)
		if !ok {
			return
		}
		waiter := c.queue[0]
		c.queue = c.queue[1:]
		waiter.hold = hold
		close(waiter.ready)
	}
}
