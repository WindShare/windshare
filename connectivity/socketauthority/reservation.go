package socketauthority

import (
	"fmt"
	"net/netip"
	"slices"
)

// CapacityError preserves the accounting decision across admission and tracing.
type CapacityError struct {
	Used, Reserved, Requested, Limit int
}

func (e *CapacityError) Error() string {
	return fmt.Sprintf("%s: used=%d reserved=%d requested=%d limit=%d", ErrCapacity, e.Used, e.Reserved, e.Requested, e.Limit)
}
func (*CapacityError) Unwrap() error { return ErrCapacity }

func (a *Authority) capacityLocked(requested int) *CapacityError {
	return &CapacityError{Used: a.socketCount, Reserved: a.reservedCount, Requested: requested, Limit: a.config.Capacity}
}
func (a *Authority) notifyLocked() { close(a.changed); a.changed = make(chan struct{}) }

// Request is immutable and can be retried by the single process admission queue.
type Request struct {
	authority *Authority
	key       pathKey
	addresses []netip.Addr
}

// Changes must be sampled before Reserve so a concurrent release cannot be lost.
func (r *Request) Changes() <-chan struct{} {
	r.authority.mu.Lock()
	defer r.authority.mu.Unlock()
	return r.authority.changed
}

// Reserve performs no network I/O. Optional TCP listeners share this ledger but
// cannot consume capacity already promised to another path's mandatory UDP set.
func (r *Request) Reserve() (*Reservation, error) {
	a := r.authority
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closing != nil {
		return nil, ErrClosed
	}
	if r.key.generation <= a.retiredThrough {
		return nil, ErrRetired
	}
	if entry := a.paths[r.key]; entry != nil {
		if entry.closing != nil {
			return nil, a.capacityLocked(len(r.addresses))
		}
		if !slices.Equal(entry.addresses, r.addresses) {
			return nil, ErrInvalid
		}
		entry.refs++
		return &Reservation{request: r, lease: &Lease{authority: a, entry: entry}}, nil
	}
	if a.reservations[r.key] != nil || a.socketCount+a.reservedCount+len(r.addresses) > a.config.Capacity {
		return nil, a.capacityLocked(len(r.addresses))
	}
	reservation := &Reservation{request: r}
	a.reservations[r.key] = reservation
	a.reservedCount += len(r.addresses)
	return reservation, nil
}

// Reservation owns either a future allocation or a retained existing lease.
// Activate transfers ownership once; abandonment returns only this reservation.
type Reservation struct {
	request *Request
	lease   *Lease
	settled bool
}

func (r *Reservation) Activate() (*Lease, error) {
	a := r.request.authority
	a.mu.Lock()
	if r.settled {
		a.mu.Unlock()
		return nil, ErrInvalid
	}
	r.settled = true
	if r.lease != nil {
		err := r.lease.unavailableLocked()
		a.mu.Unlock()
		if err != nil {
			_ = r.lease.Close()
			return nil, err
		}
		return r.lease, nil
	}
	delete(a.reservations, r.request.key)
	a.reservedCount -= len(r.request.addresses)
	lease, err := a.acquireLocked(r.request.key, r.request.addresses)
	a.notifyLocked()
	a.mu.Unlock()
	return lease, err
}

func (r *Reservation) Close() {
	if r == nil {
		return
	}
	a := r.request.authority
	a.mu.Lock()
	if r.settled {
		a.mu.Unlock()
		return
	}
	r.settled = true
	if r.lease == nil {
		delete(a.reservations, r.request.key)
		a.reservedCount -= len(r.request.addresses)
		a.notifyLocked()
	}
	a.mu.Unlock()
	if r.lease != nil {
		_ = r.lease.Close()
	}
}
