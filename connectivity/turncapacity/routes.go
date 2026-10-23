package turncapacity

import "sync"

// Initial attempts consume their prepaid slots. Optional egress exploration
// never waits behind another path or borrows that path's reserved capacity.
func (r *Reservation) startRoute(initial bool) (func(), bool) {
	c := r.owner
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.state != reservationClaimed {
		return nil, false
	}
	if initial {
		if r.unstarted == 0 {
			return nil, false
		}
		r.unstarted--
	} else {
		if len(c.queue) != 0 || r.request.Sockets >= MaximumPathSockets ||
			c.sockets+r.socketDeltaLocked(1) > MaximumSockets {
			return nil, false
		}
		r.resizeSocketsLocked(1)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if r.state != reservationClosed {
				r.resizeSocketsLocked(-1)
				c.drainLocked()
			}
		})
	}, true
}

func (r *Reservation) socketDeltaLocked(delta int) int {
	other := r.predecessor
	if other == nil {
		other = r.successor
	}
	if other == nil {
		return delta
	}
	return max(r.request.Sockets+delta, other.request.Sockets) - max(r.request.Sockets, other.request.Sockets)
}

func (r *Reservation) resizeSocketsLocked(delta int) {
	r.owner.sockets += r.socketDeltaLocked(delta)
	r.request.Sockets += delta
	r.owner.traceLocked("route_capacity_changed", r.scope, r.request)
}

func (r *Reservation) SocketLimit() int {
	if r == nil {
		return 0
	}
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	if r.state == reservationClosed {
		return 0
	}
	return r.request.Sockets
}

// StartInitialRoute consumes a prepaid slot without queueing behind another path.
func (r *Reservation) StartInitialRoute() (func(), bool) { return r.startRoute(true) }

// TryBackupRoute yields to queued ordinary work and never borrows its capacity.
func (r *Reservation) TryBackupRoute() (func(), bool) { return r.startRoute(false) }
