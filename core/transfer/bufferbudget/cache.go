package bufferbudget

import (
	"slices"

	"github.com/windshare/windshare/core/capacityledger"
)

type Cached[K comparable] struct {
	account       *Account[K]
	key           K
	data          []byte
	hold          capacityledger.Hold
	global, local cacheLink
}

// Store consumes a matching reservation without changing the charged total.
// Rejecting an invalid handoff leaves the caller's ownership untouched.
func (a *Account[K]) Store(key K, data []byte, reservation Reservation) (*Cached[K], error) {
	b := a.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if a.ledger.Snapshot().Closed || b.ledger.Snapshot().Closed || reservation.budget != b ||
		!a.ledger.Owns(reservation.hold) || reservation.hold.Units() != uint64(len(data)) {
		return nil, ErrReservation
	}
	hold, ok := reservation.hold.Transfer()
	if !ok {
		return nil, ErrReservation
	}
	cached := &Cached[K]{account: a, key: key, data: data, hold: hold}
	cached.global.entry, cached.local.entry = cached, cached
	b.cache.pushFront(&cached.global)
	a.cache.pushFront(&cached.local)
	b.wakeLocked()
	return cached, nil
}

func (c *Cached[K]) Key() K { return c.key }
func (c *Cached[K]) Live() bool {
	c.account.budget.mu.Lock()
	defer c.account.budget.mu.Unlock()
	return c.data != nil
}

// Copy pins its source while reserving an independent delivery. A nil copy with
// live=true means capacity, not content, is absent.
func (c *Cached[K]) Copy() (data []byte, reservation Reservation, live bool) {
	b, a := c.account.budget, c.account
	b.mu.Lock()
	defer b.mu.Unlock()
	if c.data == nil {
		return nil, Reservation{}, false
	}
	b.cache.remove(&c.global)
	a.cache.remove(&c.local)
	reservation, ok := a.reserveLocked(uint64(len(c.data)))
	b.cache.pushFront(&c.global)
	a.cache.pushFront(&c.local)
	if !ok {
		return nil, Reservation{}, true
	}
	return slices.Clone(c.data), reservation, true
}

func (c *Cached[K]) Drop() {
	b := c.account.budget
	b.mu.Lock()
	c.dropLocked()
	b.mu.Unlock()
}

func (c *Cached[K]) evictLocked() {
	if c.dropLocked() {
		c.account.evicted = append(c.account.evicted, c)
	}
}

func (c *Cached[K]) dropLocked() bool {
	if c.data == nil {
		return false
	}
	a, b := c.account, c.account.budget
	c.hold.Release()
	a.cache.remove(&c.local)
	b.cache.remove(&c.global)
	c.global.entry, c.local.entry = nil, nil
	c.data = nil
	b.wakeLocked()
	return true
}
