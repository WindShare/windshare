package bufferbudget

import (
	"errors"
	"fmt"
	"sync"

	"github.com/windshare/windshare/core/capacityledger"
)

var ErrReservation = errors.New("buffer does not own a matching live reservation")

// Budget owns both retained copies and immediately reclaimable cache across
// receivers. Cache eviction never calls another receiver while holding a lock.
type Budget struct {
	mu      sync.Mutex
	ledger  *capacityledger.Ledger
	changed chan struct{}
	cache   cacheList
}

// Implementations only mutate budget-owned state while the budget lock is held.
type evictionCandidate interface{ evictLocked() }

type Account[K comparable] struct {
	budget  *Budget
	ledger  *capacityledger.Ledger
	cache   cacheList
	evicted []*Cached[K]
}

func New(limit uint64) (*Budget, error) {
	if limit == 0 {
		return nil, errors.New("receiver plaintext budget must be positive")
	}
	return &Budget{ledger: capacityledger.New(limit)}, nil
}

func NewAccount[K comparable](budget *Budget, limit uint64) *Account[K] {
	return &Account[K]{budget: budget, ledger: budget.ledger.Child(limit)}
}

func (b *Budget) Limit() uint64 { return b.ledger.Limit() }
func (b *Budget) Used() uint64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ledger.Snapshot().Used
}

// Changes must be observed before checking capacity so a release between the
// check and the wait cannot be missed.
func (b *Budget) Changes() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.changed == nil {
		b.changed = make(chan struct{})
	}
	return b.changed
}

func (b *Budget) Wake() { b.mu.Lock(); b.wakeLocked(); b.mu.Unlock() }

func (b *Budget) wakeLocked() {
	// Only observers need a generation channel. Repeated changes with no new
	// observer carry no extra information and must not allocate per buffer.
	if b.changed != nil {
		close(b.changed)
		b.changed = nil
	}
}

func (b *Budget) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	err := b.ledger.Close()
	b.wakeLocked()
	if err != nil {
		return fmt.Errorf("receiver plaintext process: %w", err)
	}
	return nil
}

func (a *Account[K]) Used() uint64 {
	a.budget.mu.Lock()
	defer a.budget.mu.Unlock()
	return a.ledger.Snapshot().Used
}

func (a *Account[K]) Reserve(bytes uint64) (Reservation, bool) {
	a.budget.mu.Lock()
	defer a.budget.mu.Unlock()
	return a.reserveLocked(bytes)
}

// Available includes reclaimable cache, but never output or authentication
// holdings. Selection can inspect pressure without allocating and rolling back.
func (a *Account[K]) Available(bytes uint64) bool {
	b := a.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	local, global := a.ledger.Snapshot(), b.ledger.Snapshot()
	return bytes > 0 && !local.Closed && !global.Closed &&
		bytes <= a.ledger.Limit()-local.Used+a.cache.bytes &&
		bytes <= b.ledger.Limit()-global.Used+b.cache.bytes
}

func (a *Account[K]) reserveLocked(bytes uint64) (Reservation, bool) {
	b := a.budget
	if bytes == 0 || bytes > a.ledger.Limit() || a.ledger.Snapshot().Closed || b.ledger.Snapshot().Closed {
		return Reservation{}, false
	}
	for bytes > a.ledger.Limit()-a.ledger.Snapshot().Used {
		oldest := a.cache.last
		if oldest == nil {
			return Reservation{}, false
		}
		oldest.entry.evictLocked()
	}
	for bytes > b.ledger.Available() {
		oldest := b.cache.last
		if oldest == nil {
			return Reservation{}, false
		}
		oldest.entry.evictLocked()
	}
	hold, ok := a.ledger.Reserve(bytes)
	return Reservation{budget: b, hold: hold}, ok
}

// Close retires the account's cache after its workers have joined. Independently
// delivered buffers stay charged and are reported instead of being revoked.
func (a *Account[K]) Close() error {
	b := a.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	for oldest := a.cache.last; oldest != nil; oldest = a.cache.last {
		oldest.entry.(*Cached[K]).dropLocked()
	}
	a.evicted = nil
	err := a.ledger.Close()
	b.wakeLocked()
	if err != nil {
		return fmt.Errorf("receiver plaintext account: %w", err)
	}
	return nil
}

// TakeEvicted transfers pressure-eviction records to the account owner, so index
// cleanup follows actual evictions instead of scanning live cache entries.
func (a *Account[K]) TakeEvicted() []*Cached[K] {
	a.budget.mu.Lock()
	defer a.budget.mu.Unlock()
	evicted := a.evicted
	a.evicted = nil
	return evicted
}
