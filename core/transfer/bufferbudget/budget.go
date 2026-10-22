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

// The shared LRU can reclaim entries from accounts with different key types.
// Implementations only mutate budget-owned state while the budget lock is held.
type evictionCandidate interface {
	evictLocked()
}

type Account[K comparable] struct {
	budget      *Budget
	limit, used uint64
	cache       list.List
	evicted     []*Cached[K]
}

type Cached[K comparable] struct {
	account       *Account[K]
	key           K
	data          []byte
	global, local *list.Element
}

func New(limit uint64) (*Budget, error) {
	if limit == 0 {
		return nil, errors.New("receiver plaintext budget must be positive")
	}
	return &Budget{limit: limit, changed: make(chan struct{})}, nil
}

func NewAccount[K comparable](budget *Budget, limit uint64) *Account[K] {
	return &Account[K]{budget: budget, limit: min(limit, budget.limit)}
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
func (a *Account[K]) Used() uint64         { a.budget.mu.Lock(); defer a.budget.mu.Unlock(); return a.used }

func (a *Account[K]) Reserve(bytes uint64) bool {
	a.budget.mu.Lock()
	defer a.budget.mu.Unlock()
	return a.reserveLocked(bytes)
}

func (a *Account[K]) reserveLocked(bytes uint64) bool {
	b := a.budget
	if bytes == 0 || bytes > a.limit {
		return false
	}
	for bytes > a.limit-a.used {
		oldest := a.cache.Back()
		if oldest == nil {
			return false
		}
		oldest.Value.(evictionCandidate).evictLocked()
	}
	for bytes > b.limit-b.used {
		oldest := b.cache.Back()
		if oldest == nil {
			return false
		}
		oldest.Value.(evictionCandidate).evictLocked()
	}
	a.used += bytes
	b.used += bytes
	return true
}

func (a *Account[K]) Release(bytes uint64) {
	b := a.budget
	b.mu.Lock()
	a.used -= bytes
	b.used -= bytes
	b.wakeLocked()
	b.mu.Unlock()
}

// Store transfers an existing reservation, so a completed shared load is charged
// once when its last subscriber leaves and it becomes reclaimable cache.
func (a *Account[K]) Store(key K, data []byte) *Cached[K] {
	b := a.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	cached := &Cached[K]{account: a, key: key, data: data}
	cached.global = b.cache.PushFront(cached)
	cached.local = a.cache.PushFront(cached)
	b.wakeLocked()
	return cached
}

// TakeEvicted transfers pressure-eviction records to the account owner, so index
// cleanup follows actual evictions instead of scanning live cache entries.
// A record identifies the evicted instance, not a later replacement with its key.
func (a *Account[K]) TakeEvicted() []*Cached[K] {
	a.budget.mu.Lock()
	defer a.budget.mu.Unlock()
	evicted := a.evicted
	a.evicted = nil
	return evicted
}

func (c *Cached[K]) Key() K { return c.key }

func (c *Cached[K]) Live() bool {
	c.account.budget.mu.Lock()
	defer c.account.budget.mu.Unlock()
	return c.data != nil
}

// Copy reserves an independent consumer buffer atomically with pinning the
// cache source. A nil copy with live=true means capacity, not content, is absent.
func (c *Cached[K]) Copy() (data []byte, live bool) {
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

func (c *Cached[K]) Drop() {
	b := c.account.budget
	b.mu.Lock()
	c.dropLocked()
	b.mu.Unlock()
}

func (c *Cached[K]) evictLocked() {
	// Explicit Drop already belongs to the index owner and needs no notification.
	if c.dropLocked() {
		c.account.evicted = append(c.account.evicted, c)
	}
}

func (c *Cached[K]) dropLocked() bool {
	if c.data == nil {
		return false
	}
	a, b := c.account, c.account.budget
	bytes := uint64(len(c.data))
	a.used -= bytes
	b.used -= bytes
	a.cache.Remove(c.local)
	b.cache.Remove(c.global)
	c.global, c.local = nil, nil
	c.data = nil
	b.wakeLocked()
	return true
}
