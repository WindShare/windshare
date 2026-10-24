package content

import (
	"errors"
	"fmt"
	"sync"

	"github.com/windshare/windshare/core/capacityledger"
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
	mu     sync.Mutex
	ledger *capacityledger.Ledger
}

func NewRevisionMetadataBudget(capacity uint64) (*RevisionMetadataBudget, error) {
	if capacity == 0 {
		return nil, errors.New("revision metadata budget requires positive capacity")
	}
	return &RevisionMetadataBudget{ledger: capacityledger.New(capacity)}, nil
}

func (b *RevisionMetadataBudget) Snapshot() RevisionMetadataSnapshot {
	if b == nil {
		return RevisionMetadataSnapshot{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	snapshot := b.ledger.Snapshot()
	return RevisionMetadataSnapshot{Capacity: snapshot.Limit, Used: snapshot.Used}
}

func (b *RevisionMetadataBudget) reserveInvalidation() (*revisionMetadataReservation, error) {
	if b == nil {
		return nil, errors.New("revision metadata budget is nil")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	hold, ok := b.ledger.Reserve(1)
	if !ok {
		return nil, fmt.Errorf("%w: revision invalidation metadata", ErrQuotaExceeded)
	}
	return &revisionMetadataReservation{budget: b, hold: hold}, nil
}

type revisionMetadataReservation struct {
	budget *RevisionMetadataBudget
	hold   capacityledger.Hold
}

func (r *revisionMetadataReservation) release() {
	if r == nil {
		return
	}
	r.budget.mu.Lock()
	defer r.budget.mu.Unlock()
	r.hold.Release()
}
