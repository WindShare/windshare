package contentflow

import (
	"errors"
	"fmt"
	"sync"

	"github.com/windshare/windshare/core/capacityledger"
)

type ProcessCacheBudget struct {
	mu     sync.Mutex
	ledger *capacityledger.Ledger
}

func NewProcessCacheBudget(limit uint64) (*ProcessCacheBudget, error) {
	if limit == 0 {
		return nil, errors.New("process cache budget must be positive")
	}
	return &ProcessCacheBudget{ledger: capacityledger.New(limit)}, nil
}

func (b *ProcessCacheBudget) Used() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ledger.Snapshot().Used
}

func (b *ProcessCacheBudget) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ledger.Close(); err != nil {
		return fmt.Errorf("sealed block cache process: %w", err)
	}
	return nil
}

// One cache aggregates its retained entries into one hold. Entry insertion and
// eviction already share the cache lock, so per-entry ownership objects add no
// protection and would penalize transfers containing many small files.
type cacheReservation struct {
	budget *ProcessCacheBudget
	hold   capacityledger.Hold
}

func (r *cacheReservation) grow(bytes uint64) bool {
	r.budget.mu.Lock()
	defer r.budget.mu.Unlock()
	if r.hold.Units() != 0 {
		return r.hold.Grow(bytes)
	}
	hold, ok := r.budget.ledger.Reserve(bytes)
	if ok {
		r.hold = hold
	}
	return ok
}

func (r *cacheReservation) shrink(bytes uint64) {
	r.budget.mu.Lock()
	defer r.budget.mu.Unlock()
	if !r.hold.Shrink(bytes) {
		// Entries are removed under their cache's lock; a mismatch is a broken
		// internal accounting invariant, never a capacity or network failure.
		panic("sealed cache eviction exceeds its retained reservation")
	}
}
