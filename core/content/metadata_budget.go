package content

import (
	"errors"
	"fmt"
	"sync"
)

const DefaultRevisionInvalidationEntries = 4_096

type RevisionMetadataSnapshot struct {
	Capacity uint64
	Used     uint64
}

// RevisionMetadataBudget bounds the only share-lifetime revision metadata:
// positively invalidated (FileID, FileRevision) tuples. Reservations are never
// evicted because forgetting one would restore authority to known-stale data.
type RevisionMetadataBudget struct {
	mu       sync.Mutex
	capacity uint64
	used     uint64
}

func NewRevisionMetadataBudget(capacity uint64) (*RevisionMetadataBudget, error) {
	if capacity == 0 {
		return nil, errors.New("revision metadata budget requires positive capacity")
	}
	return &RevisionMetadataBudget{capacity: capacity}, nil
}

func (b *RevisionMetadataBudget) Snapshot() RevisionMetadataSnapshot {
	if b == nil {
		return RevisionMetadataSnapshot{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return RevisionMetadataSnapshot{Capacity: b.capacity, Used: b.used}
}

func (b *RevisionMetadataBudget) reserveInvalidation() (*revisionMetadataReservation, error) {
	if b == nil {
		return nil, errors.New("revision metadata budget is nil")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used == b.capacity {
		return nil, fmt.Errorf("%w: revision invalidation metadata", ErrQuotaExceeded)
	}
	b.used++
	return &revisionMetadataReservation{budget: b}, nil
}

type revisionMetadataReservation struct {
	once   sync.Once
	budget *RevisionMetadataBudget
}

func (r *revisionMetadataReservation) release() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		r.budget.mu.Lock()
		r.budget.used--
		r.budget.mu.Unlock()
	})
}
