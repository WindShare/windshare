// Package peerset owns demand-driven peer path recovery. Transport providers own
// one attempt; they never replenish budgets or restart a failed connection.
package peerset

import (
	"context"
	"errors"
	"github.com/windshare/windshare/connectivity/nativepeer"
	"sync"
	"time"
)

const (
	AttemptsPerWindow         = 8
	ActiveTimePerWindow       = 360 * time.Second
	RefillWindow              = 600 * time.Second
	AttemptsPerWave           = 3
	WaveBudget                = 120 * time.Second
	AttemptBudget             = nativepeer.ProcessAttemptBudget
	MinimumAttemptOpportunity = nativepeer.MinimumPreparationOpportunity
	PrewarmRetention          = 30 * time.Second
	MaximumPaths              = 8
	ProcessAttemptCapacity    = 8
)

var (
	ErrConfig        = errors.New("invalid peer set configuration")
	ErrWaveExhausted = errors.New("peer recovery wave exhausted")
	ErrPathTerminal  = errors.New("peer path is terminal")
)

// Budget belongs to a receive intent, not a protocol or network generation.
// Reserving the complete attempt before launch prevents a depleted token bucket
// from shortening the ICE opportunity halfway through an attempt.
type Budget struct {
	mu       sync.Mutex
	updated  time.Time
	attempts float64
	active   float64
}

func NewBudget(now time.Time) *Budget {
	return &Budget{updated: now, attempts: AttemptsPerWindow, active: float64(ActiveTimePerWindow)}
}

func (b *Budget) reserve(now time.Time) (func(time.Duration), time.Duration) {
	b.mu.Lock()
	elapsed := max(now.Sub(b.updated), 0)
	b.attempts = min(AttemptsPerWindow, b.attempts+float64(elapsed)*AttemptsPerWindow/float64(RefillWindow))
	b.active = min(float64(ActiveTimePerWindow), b.active+float64(elapsed)*float64(ActiveTimePerWindow)/float64(RefillWindow))
	if now.After(b.updated) {
		b.updated = now
	}
	waitCount := (1 - b.attempts) * float64(RefillWindow) / AttemptsPerWindow
	waitTime := (float64(AttemptBudget) - b.active) * float64(RefillWindow) / float64(ActiveTimePerWindow)
	wait := time.Duration(max(waitCount, waitTime, 0))
	if b.attempts < 1 || b.active < float64(AttemptBudget) {
		b.mu.Unlock()
		return nil, max(wait, time.Nanosecond)
	}
	b.attempts--
	b.active -= float64(AttemptBudget)
	b.mu.Unlock()
	var once sync.Once
	return func(used time.Duration) {
		once.Do(func() {
			b.mu.Lock()
			b.active = min(float64(ActiveTimePerWindow), b.active+float64(AttemptBudget-min(max(used, 0), AttemptBudget)))
			b.mu.Unlock()
		})
	}, 0
}

// Capacity is FIFO across paths and receivers. An admitted lane releases its
// attempt slot while retaining its separate provider/socket ownership.
type Capacity struct {
	mu            sync.Mutex
	limit, active int
	queue         []*capacityWaiter
}
type capacityWaiter struct {
	ready   chan struct{}
	granted bool
}

func NewCapacity(limit int) (*Capacity, error) {
	if limit <= 0 {
		return nil, ErrConfig
	}
	return &Capacity{limit: limit}, nil
}

var processCapacity = &Capacity{limit: ProcessAttemptCapacity}

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
		if !waiter.granted {
			for i, item := range c.queue {
				if item == waiter {
					c.queue = append(c.queue[:i], c.queue[i+1:]...)
					break
				}
			}
		} else {
			c.active--
			c.dispatchLocked()
		}
		c.mu.Unlock()
		return nil, context.Cause(ctx)
	}
	var once sync.Once
	return func() { once.Do(func() { c.mu.Lock(); c.active--; c.dispatchLocked(); c.mu.Unlock() }) }, nil
}
func (c *Capacity) dispatchLocked() {
	for c.active < c.limit && len(c.queue) > 0 {
		waiter := c.queue[0]
		c.queue = c.queue[1:]
		c.active++
		waiter.granted = true
		close(waiter.ready)
	}
}

// A reserved late opportunity charges both buckets immediately, but returns
// both charges if no PeerConnection is started. It cannot multiply on wakeups.
type reservedAttempt struct {
	budget *Budget
	refund func(time.Duration)
	once   sync.Once
}

func (b *Budget) reserveDeferred(now time.Time) *reservedAttempt {
	refund, _ := b.reserve(now)
	if refund == nil {
		return nil
	}
	return &reservedAttempt{budget: b, refund: refund}
}
func (r *reservedAttempt) settle(started bool, used time.Duration) {
	if r == nil {
		return
	}
	r.once.Do(func() {
		r.refund(used)
		if !started {
			r.budget.mu.Lock()
			r.budget.attempts = min(AttemptsPerWindow, r.budget.attempts+1)
			r.budget.mu.Unlock()
		}
	})
}
