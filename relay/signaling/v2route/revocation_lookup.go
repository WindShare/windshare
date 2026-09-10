package v2route

import (
	"context"
	"errors"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

// Retained only while lookups are in flight, never for historical shares.
// STOP can publish its result directly to overlapping readers of the same ID.
type revocationLookup struct {
	readers int
	stopped *Tombstone
}

// lockRoute returns with mu held, including on error. Active routes never need
// disk I/O. Absent routes are checked outside mu so a slow index cannot stall
// established shares. A concurrent STOP overrides stale absence for this ID;
// unrelated stops neither invalidate the lookup nor force another disk read.
func (r *Registry) lockRoute(ctx context.Context, shareID v2.ShareID) (*route, *Tombstone, error) {
	r.mu.Lock()
	r.expireRoutes(r.now())
	if current := r.routes[shareID]; current != nil {
		return current, nil, nil
	}
	lookup := r.revocationLookups[shareID]
	if lookup == nil {
		lookup = &revocationLookup{}
		r.revocationLookups[shareID] = lookup
	}
	lookup.readers++
	r.mu.Unlock()

	stopped, found, err := r.tombstones.Lookup(ctx, shareID)

	r.mu.Lock()
	r.expireRoutes(r.now())
	lookup.readers--
	if lookup.readers == 0 {
		delete(r.revocationLookups, shareID)
	}
	if current := r.routes[shareID]; current != nil {
		return current, nil, nil
	}
	if lookup.stopped != nil {
		return nil, lookup.stopped, nil
	}
	if err != nil {
		return nil, nil, errors.Join(ErrAdmission, err)
	}
	if found {
		if stopped.ShareID != shareID || !validTombstone(stopped) {
			return nil, nil, errors.Join(ErrAdmission, ErrConfig)
		}
		return nil, &stopped, nil
	}
	return nil, nil, nil
}
