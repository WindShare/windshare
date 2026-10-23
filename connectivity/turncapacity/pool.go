// Package turncapacity owns shared TURN admission and bounded handoff capacity.
// It accounts for reservations, independently of socket or protocol implementations.
package turncapacity

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync"

	"github.com/windshare/windshare/connectivity/iceconfig"
)

const (
	MaximumConnections      = 16
	MaximumSockets          = 64
	MaximumPathSockets      = 8
	MaximumHandoffEndpoints = 2
	MaximumQueuedRequests   = 64
)

var (
	ErrCapacity    = errors.New("TURN capacity exhausted")
	ErrReservation = errors.New("invalid TURN reservation")
)

// Request reserves initial routes before signaling. Backup routes acquire
// additional slots without queueing; each closed route returns its own slot.
// Replaces identifies actual predecessor ownership, never an unbounded bypass.
type Request struct {
	ProtocolSessionID  [16]byte
	PeerPathID         [16]byte
	ReplacesPeerPathID [16]byte
	Service            iceconfig.Identity
	Sockets            int
}

type pathKey struct {
	owner         *Owner
	session, path [16]byte
}

func (r Request) key(owner *Owner) pathKey {
	return pathKey{owner, r.ProtocolSessionID, r.PeerPathID}
}

// Owner distinguishes local endpoints of the same protocol session sharing a pool.
type Owner struct {
	capacity *Pool
	id       uint64
}

func (c *Pool) NewOwner() *Owner {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextOwner++
	return &Owner{capacity: c, id: c.nextOwner}
}

func (o *Owner) Acquire(ctx context.Context, request Request) (*Reservation, error) {
	return o.capacity.acquire(ctx, request, o)
}

// Pool belongs to the application that injects it into its connectivity owners.
// Ordinary capacity is preserved during one bounded make-before-break handoff,
// including both endpoints when the sender and receiver share this pool.
type Pool struct {
	mu                                 sync.Mutex
	paths                              map[pathKey]*Reservation
	queue                              []*waiter
	connections, sockets, replacements int
	nextOwner                          uint64
}

type waiter struct {
	owner   *Owner
	request Request
	ready   chan struct{}
	lease   *Reservation
	err     error
}

// Reservation owns capacity through preparation, gathering and final close.
// Its predecessor/successor relationship transfers ordinary capacity atomically.
type Reservation struct {
	owner                  *Pool
	scope                  *Owner
	request                Request
	predecessor, successor *Reservation
	state                  reservationState
	unstarted              int
}

type reservationState uint8

const (
	reservationReserved reservationState = iota
	reservationClaimed
	reservationClosed
)

func NewPool() *Pool {
	return &Pool{paths: make(map[pathKey]*Reservation)}
}

func (c *Pool) acquire(ctx context.Context, request Request, owner *Owner) (*Reservation, error) {
	if ctx == nil || request.ProtocolSessionID == ([16]byte{}) || request.PeerPathID == ([16]byte{}) ||
		request.PeerPathID == request.ReplacesPeerPathID || request.Service.Validate() != nil ||
		request.Sockets < 1 || request.Sockets > MaximumPathSockets {
		return nil, ErrReservation
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if len(c.queue) >= MaximumQueuedRequests {
		c.mu.Unlock()
		return nil, ErrCapacity
	}
	waiter := &waiter{owner: owner, request: request, ready: make(chan struct{})}
	c.queue = append(c.queue, waiter)
	c.traceLocked("queued", owner, request)
	c.drainLocked()
	c.mu.Unlock()
	select {
	case <-waiter.ready:
		if err := ctx.Err(); err != nil {
			waiter.lease.Close()
			return nil, err
		}
		return waiter.lease, waiter.err
	case <-ctx.Done():
		c.mu.Lock()
		for i, pending := range c.queue {
			if pending == waiter {
				c.queue = append(c.queue[:i], c.queue[i+1:]...)
				break
			}
		}
		lease := waiter.lease
		c.traceLocked("cancelled", owner, request)
		c.mu.Unlock()
		lease.Close()
		return nil, errors.Join(ErrCapacity, ctx.Err())
	}
}

func (c *Pool) drainLocked() {
	for i := 0; i < len(c.queue); {
		waiter := c.queue[i]
		lease, err := c.tryAcquireLocked(waiter.owner, waiter.request)
		if lease == nil && err == nil {
			i++
			continue
		}
		c.queue = append(c.queue[:i], c.queue[i+1:]...)
		waiter.lease, waiter.err = lease, err
		close(waiter.ready)
	}
}

func (c *Pool) tryAcquireLocked(owner *Owner, request Request) (*Reservation, error) {
	if c.paths[request.key(owner)] != nil {
		return nil, ErrReservation
	}
	var predecessor *Reservation
	if request.ReplacesPeerPathID != ([16]byte{}) {
		predecessor = c.paths[pathKey{owner, request.ProtocolSessionID, request.ReplacesPeerPathID}]
	}
	lease := &Reservation{owner: c, scope: owner, request: request, unstarted: request.Sockets}
	if predecessor != nil {
		if admitted, err := c.reserveReplacementLocked(lease, predecessor); !admitted {
			return nil, err
		}
	} else {
		if c.connections >= MaximumConnections || c.sockets+request.Sockets > MaximumSockets {
			return nil, nil
		}
		c.connections++
		c.sockets += request.Sockets
	}
	c.paths[request.key(owner)] = lease
	c.traceLocked("granted", owner, request)
	return lease, nil
}

func (c *Pool) reserveReplacementLocked(lease, predecessor *Reservation) (bool, error) {
	if predecessor.request.Service != lease.request.Service {
		return false, ErrReservation
	}
	// A pending handoff cannot recursively borrow another handoff. A failed
	// predecessor that already closed instead takes the ordinary admission path.
	if predecessor.state != reservationClaimed || predecessor.predecessor != nil || predecessor.successor != nil ||
		!c.handoffAvailableLocked(lease.request) {
		return false, nil
	}
	growth := max(0, lease.request.Sockets-predecessor.request.Sockets)
	if c.sockets+growth > MaximumSockets {
		return false, nil
	}
	c.sockets += growth
	c.replacements++
	lease.predecessor = predecessor
	predecessor.successor = lease
	return true, nil
}

func (c *Pool) handoffAvailableLocked(request Request) bool {
	if c.replacements == 0 {
		return true
	}
	if c.replacements >= MaximumHandoffEndpoints {
		return false
	}
	// Both local endpoints must gather before either can authenticate the new
	// lane. Allow the counterpart of this exact handoff, not another renewal.
	for _, active := range c.paths {
		if active.predecessor != nil {
			return active.request.ProtocolSessionID == request.ProtocolSessionID &&
				active.request.PeerPathID == request.PeerPathID &&
				active.request.ReplacesPeerPathID == request.ReplacesPeerPathID &&
				active.request.Service == request.Service
		}
	}
	return false
}

func (r *Reservation) Claim(session, path [16]byte, service iceconfig.Identity) error {
	if r == nil {
		return ErrReservation
	}
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	if r.state != reservationReserved || r.request.ProtocolSessionID != session ||
		r.request.PeerPathID != path || r.request.Service != service {
		return ErrReservation
	}
	r.state = reservationClaimed
	r.owner.traceLocked("claimed", r.scope, r.request)
	r.owner.drainLocked()
	return nil
}

func (r *Reservation) Close() {
	if r == nil {
		return
	}
	c := r.owner
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.state == reservationClosed {
		return
	}
	r.state = reservationClosed
	delete(c.paths, r.request.key(r.scope))
	switch {
	case r.predecessor != nil:
		old := r.predecessor
		c.sockets -= max(0, r.request.Sockets-old.request.Sockets)
		c.replacements--
		old.successor = nil
		r.predecessor = nil
	case r.successor != nil:
		next := r.successor
		c.sockets -= max(0, r.request.Sockets-next.request.Sockets)
		c.replacements--
		next.predecessor = nil
		r.successor = nil
	default:
		c.connections--
		c.sockets -= r.request.Sockets
	}
	c.traceLocked("released", r.scope, r.request)
	c.drainLocked()
}

func (c *Pool) traceLocked(transition string, owner *Owner, request Request) {
	slog.Debug("TURN capacity transition", "transition", transition, "owner_id", owner.id,
		"protocol_session_id", hex.EncodeToString(request.ProtocolSessionID[:]),
		"peer_path_id", hex.EncodeToString(request.PeerPathID[:]),
		"replaces_peer_path_id", hex.EncodeToString(request.ReplacesPeerPathID[:]),
		"service", request.Service, "requested_sockets", request.Sockets,
		"ordinary_connections", c.connections, "ordinary_socket_reservations", c.sockets,
		"replacement_connections", c.replacements, "queued", len(c.queue))
}
