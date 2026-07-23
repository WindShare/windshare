package transfer

import (
	"errors"
	"sync"
)

const (
	DefaultSessionPlaintextBytes = uint64(128) << 20
	DefaultProcessPlaintextBytes = uint64(1) << 30
)

var ErrPlaintextBudget = errors.New("receiver plaintext budget exceeded")

// PlaintextBudget is process-scoped. Individual brokers enforce the session
// ceiling while this account prevents many receivers from multiplying it.
type PlaintextBudget struct {
	mu    sync.Mutex
	limit uint64
	used  uint64
}

func NewPlaintextBudget(limit uint64) (*PlaintextBudget, error) {
	if limit == 0 {
		return nil, errors.New("receiver plaintext budget must be positive")
	}
	return &PlaintextBudget{limit: limit}, nil
}

func (b *PlaintextBudget) tryReserve(bytes uint64) bool {
	if b == nil || bytes == 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if bytes > b.limit-b.used {
		return false
	}
	b.used += bytes
	return true
}

func (b *PlaintextBudget) release(bytes uint64) {
	b.mu.Lock()
	b.used -= bytes
	b.mu.Unlock()
}

func (b *PlaintextBudget) Used() uint64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}
