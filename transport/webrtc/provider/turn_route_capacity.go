package provider

import "sync"

// Initial attempts consume their prepaid slots. Optional egress exploration
// never waits behind another path or borrows that path's reserved capacity.
func (r *TURNReservation) startRoute(initial bool) (func(), bool) {
	c := r.owner
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.closed || !r.claimed {
		return nil, false
	}
	if initial {
		if r.unstarted == 0 {
			return nil, false
		}
		r.unstarted--
	} else {
		if len(c.queue) != 0 || r.request.Sockets >= MaximumTURNPathSockets ||
			c.sockets+r.socketDeltaLocked(1) > MaximumTURNSockets {
			return nil, false
		}
		r.resizeSocketsLocked(1)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if !r.closed {
				r.resizeSocketsLocked(-1)
				c.drainLocked()
			}
		})
	}, true
}

func (r *TURNReservation) socketDeltaLocked(delta int) int {
	other := r.predecessor
	if other == nil {
		other = r.successor
	}
	if other == nil {
		return delta
	}
	return max(r.request.Sockets+delta, other.request.Sockets) - max(r.request.Sockets, other.request.Sockets)
}

func (r *TURNReservation) resizeSocketsLocked(delta int) {
	r.owner.sockets += r.socketDeltaLocked(delta)
	r.request.Sockets += delta
	r.owner.traceLocked("route_capacity_changed", r.scope, r.request)
}

func (r *TURNReservation) socketLimit() int {
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	if r.closed {
		return 0
	}
	return r.request.Sockets
}
