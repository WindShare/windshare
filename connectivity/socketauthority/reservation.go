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
	return r.reserve(false)
}

// ReserveOptional leaves a complete mandatory socket opportunity for a new
// receiver. The reserve uses the frozen address cost, not a guessed path count.
func (r *Request) ReserveOptional() (*Reservation, error) { return r.reserve(true) }
func (r *Request) SamePool(other *Request) bool           { return r.authority == other.authority }
func (a *Authority) essentialReserveLocked() int {
	reserve := 0
	for _, path := range a.paths {
		if path.optional {
			reserve = max(reserve, len(path.addresses))
		}
	}
	for _, reservation := range a.reservations {
		if reservation.optional {
			reserve = max(reserve, len(reservation.request.addresses))
		}
	}
	return reserve
}
func (r *Request) reserve(optional bool) (*Reservation, error) {
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
	reserve := 0
	if optional {
		reserve = max(len(r.addresses), a.essentialReserveLocked())
	}
	if a.reservations[r.key] != nil || a.socketCount+a.reservedCount+len(r.addresses)+reserve > a.config.Capacity {
		return nil, a.capacityLocked(len(r.addresses))
	}
	reservation := &Reservation{request: r, optional: optional}
	a.reservations[r.key] = reservation
	a.reservedCount += len(r.addresses)
	return reservation, nil
}

// Reservation owns either a future allocation or a retained existing lease.
// Activate transfers ownership once; abandonment returns only this reservation.
type Reservation struct {
	optional bool
	request  *Request
	lease    *Lease
	settled  bool
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
	if err == nil {
		lease.entry.optional = r.optional
	}
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
