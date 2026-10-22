package bufferbudget

import (
	"container/list"
	"errors"
	"slices"
	"sync"
)

// Budget owns both retained copies and immediately reclaimable cache across
// receivers. Cache eviction never calls another receiver while holding a lock.
type Budget struct {
	mu          sync.Mutex
	limit, used uint64
	changed     chan struct{}
	cache       list.List
}

type Account struct {
	budget      *Budget
	limit, used uint64
	cache       list.List
}

type Cached struct {
	account       *Account
	data          []byte
	global, local *list.Element
}

func New(limit uint64) (*Budget, error) {
	if limit == 0 {
		return nil, errors.New("receiver plaintext budget must be positive")
	}
	return &Budget{limit: limit, changed: make(chan struct{})}, nil
}

func (b *Budget) Account(limit uint64) *Account {
	return &Account{budget: b, limit: min(limit, b.limit)}
}
func (b *Budget) Limit() uint64 { return b.limit }
func (b *Budget) Used() uint64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}
func (b *Budget) Changes() <-chan struct{} { b.mu.Lock(); defer b.mu.Unlock(); return b.changed }
func (b *Budget) Wake()                    { b.mu.Lock(); b.wakeLocked(); b.mu.Unlock() }
func (b *Budget) wakeLocked()              { close(b.changed); b.changed = make(chan struct{}) }
func (a *Account) Used() uint64            { a.budget.mu.Lock(); defer a.budget.mu.Unlock(); return a.used }

func (a *Account) Reserve(bytes uint64) bool {
	a.budget.mu.Lock()
	defer a.budget.mu.Unlock()
	return a.reserveLocked(bytes)
}

func (a *Account) reserveLocked(bytes uint64) bool {
	b := a.budget
	if bytes == 0 || bytes > a.limit {
		return false
	}
	for bytes > a.limit-a.used {
		oldest := a.cache.Back()
		if oldest == nil {
			return false
		}
		oldest.Value.(*Cached).dropLocked()
	}
	for bytes > b.limit-b.used {
		oldest := b.cache.Back()
		if oldest == nil {
			return false
		}
		oldest.Value.(*Cached).dropLocked()
	}
	a.used += bytes
	b.used += bytes
	return true
}

func (a *Account) Release(bytes uint64) {
	b := a.budget
	b.mu.Lock()
	a.used -= bytes
	b.used -= bytes
	b.wakeLocked()
	b.mu.Unlock()
}

// Store transfers an existing reservation, so a completed shared load is charged
// once when its last subscriber leaves and it becomes reclaimable cache.
func (a *Account) Store(data []byte) *Cached {
	b := a.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	cached := &Cached{account: a, data: data}
	cached.global = b.cache.PushFront(cached)
	cached.local = a.cache.PushFront(cached)
	b.wakeLocked()
	return cached
}

func (c *Cached) Live() bool {
	c.account.budget.mu.Lock()
	defer c.account.budget.mu.Unlock()
	return c.data != nil
}

// Copy reserves an independent consumer buffer atomically with pinning the
// cache source. A nil copy with live=true means capacity, not content, is absent.
func (c *Cached) Copy() (data []byte, live bool) {
	b, a := c.account.budget, c.account
	b.mu.Lock()
	defer b.mu.Unlock()
	if c.data == nil {
		return nil, false
	}
	b.cache.Remove(c.global)
	a.cache.Remove(c.local)
	reserved := a.reserveLocked(uint64(len(c.data)))
	c.global = b.cache.PushFront(c)
	c.local = a.cache.PushFront(c)
	if !reserved {
		return nil, true
	}
	return slices.Clone(c.data), true
}

func (c *Cached) Drop() {
	b := c.account.budget
	b.mu.Lock()
	c.dropLocked()
	b.mu.Unlock()
}

func (c *Cached) dropLocked() {
	if c.data == nil {
		return
	}
	a, b := c.account, c.account.budget
	bytes := uint64(len(c.data))
	a.used -= bytes
	b.used -= bytes
	a.cache.Remove(c.local)
	b.cache.Remove(c.global)
	c.data = nil
	b.wakeLocked()
}
