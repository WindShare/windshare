package provider

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync"

	"github.com/windshare/windshare/connectivity/iceconfig"
)

const (
	MaximumTURNConnections      = 16
	MaximumTURNSockets          = 64
	MaximumTURNPathSockets      = 8
	MaximumTURNHandoffEndpoints = 2
	MaximumTURNQueuedRequests   = 64
)

var (
	ErrTURNCapacity    = errors.New("TURN capacity exhausted")
	ErrTURNReservation = errors.New("invalid TURN reservation")
)

// TURNRequest reserves the whole gathering envelope before signaling starts.
// Replaces identifies actual predecessor ownership, never an unbounded bypass.
type TURNRequest struct {
	ProtocolSessionID  [16]byte
	PeerPathID         [16]byte
	ReplacesPeerPathID [16]byte
	Service            iceconfig.Identity
	Sockets            int
}

type turnPathKey struct {
	owner         *TURNResourceOwner
	session, path [16]byte
}

func (r TURNRequest) key(owner *TURNResourceOwner) turnPathKey {
	return turnPathKey{owner, r.ProtocolSessionID, r.PeerPathID}
}

// TURNResourceOwner distinguishes local endpoints of the same protocol session,
// including a sender and receiver hosted by different Engines in one process.
type TURNResourceOwner struct {
	capacity *TURNCapacity
	id       uint64
}

func (c *TURNCapacity) NewOwner() *TURNResourceOwner {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextOwner++
	return &TURNResourceOwner{capacity: c, id: c.nextOwner}
}

func (o *TURNResourceOwner) Acquire(ctx context.Context, request TURNRequest) (*TURNReservation, error) {
	return o.capacity.acquire(ctx, request, o)
}

// TURNCapacity is shared by the process; an injected instance isolates tests.
// Ordinary capacity is preserved during one bounded make-before-break handoff,
// including both endpoints when the sender and receiver share a process.
type TURNCapacity struct {
	mu                                 sync.Mutex
	paths                              map[turnPathKey]*TURNReservation
	queue                              []*turnWaiter
	connections, sockets, replacements int
	nextOwner                          uint64
}

type turnWaiter struct {
	owner   *TURNResourceOwner
	request TURNRequest
	ready   chan struct{}
	lease   *TURNReservation
	err     error
}

// TURNReservation owns capacity through preparation, gathering and final close.
// Its predecessor/successor relationship transfers ordinary capacity atomically.
type TURNReservation struct {
	owner                  *TURNCapacity
	scope                  *TURNResourceOwner
	request                TURNRequest
	predecessor, successor *TURNReservation
	claimed, closed        bool
}

var processTURNCapacity = NewTURNCapacity()

func NewTURNCapacity() *TURNCapacity {
	return &TURNCapacity{paths: make(map[turnPathKey]*TURNReservation)}
}

func ProcessTURNCapacity() *TURNCapacity { return processTURNCapacity }

func (c *TURNCapacity) acquire(ctx context.Context, request TURNRequest, owner *TURNResourceOwner) (*TURNReservation, error) {
	if ctx == nil || request.ProtocolSessionID == ([16]byte{}) || request.PeerPathID == ([16]byte{}) ||
		request.PeerPathID == request.ReplacesPeerPathID || request.Service.Validate() != nil ||
		request.Sockets < 1 || request.Sockets > MaximumTURNPathSockets {
		return nil, ErrTURNReservation
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if len(c.queue) >= MaximumTURNQueuedRequests {
		c.mu.Unlock()
		return nil, ErrTURNCapacity
	}
	waiter := &turnWaiter{owner: owner, request: request, ready: make(chan struct{})}
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
		return nil, errors.Join(ErrTURNCapacity, ctx.Err())
	}
}

func (c *TURNCapacity) drainLocked() {
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

func (c *TURNCapacity) tryAcquireLocked(owner *TURNResourceOwner, request TURNRequest) (*TURNReservation, error) {
	if c.paths[request.key(owner)] != nil {
		return nil, ErrTURNReservation
	}
	var predecessor *TURNReservation
	if request.ReplacesPeerPathID != ([16]byte{}) {
		predecessor = c.paths[turnPathKey{owner, request.ProtocolSessionID, request.ReplacesPeerPathID}]
	}
	lease := &TURNReservation{owner: c, scope: owner, request: request}
	if predecessor != nil {
		if admitted, err := c.reserveReplacementLocked(lease, predecessor); !admitted {
			return nil, err
		}
	} else {
		if c.connections >= MaximumTURNConnections || c.sockets+request.Sockets > MaximumTURNSockets {
			return nil, nil
		}
		c.connections++
		c.sockets += request.Sockets
	}
	c.paths[request.key(owner)] = lease
	c.traceLocked("granted", owner, request)
	return lease, nil
}

func (c *TURNCapacity) reserveReplacementLocked(lease, predecessor *TURNReservation) (bool, error) {
	if predecessor.request.Service != lease.request.Service {
		return false, ErrTURNReservation
	}
	// A pending handoff cannot recursively borrow another handoff. A failed
	// predecessor that already closed instead takes the ordinary admission path.
	if !predecessor.claimed || predecessor.predecessor != nil || predecessor.successor != nil ||
		!c.handoffAvailableLocked(lease.request) {
		return false, nil
	}
	growth := max(0, lease.request.Sockets-predecessor.request.Sockets)
	if c.sockets+growth > MaximumTURNSockets {
		return false, nil
	}
	c.sockets += growth
	c.replacements++
	lease.predecessor = predecessor
	predecessor.successor = lease
	return true, nil
}

func (c *TURNCapacity) handoffAvailableLocked(request TURNRequest) bool {
	if c.replacements == 0 {
		return true
	}
	if c.replacements >= MaximumTURNHandoffEndpoints {
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

func (r *TURNReservation) claim(session, path [16]byte, service iceconfig.Identity) error {
	if r == nil {
		return ErrTURNReservation
	}
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	if r.closed || r.claimed || r.request.ProtocolSessionID != session ||
		r.request.PeerPathID != path || r.request.Service != service {
		return ErrTURNReservation
	}
	r.claimed = true
	r.owner.drainLocked()
	return nil
}

func (r *TURNReservation) Close() {
	if r == nil {
		return
	}
	c := r.owner
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
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

func (c *TURNCapacity) traceLocked(transition string, owner *TURNResourceOwner, request TURNRequest) {
	slog.Debug("TURN capacity transition", "transition", transition, "owner_id", owner.id,
		"protocol_session_id", hex.EncodeToString(request.ProtocolSessionID[:]),
		"peer_path_id", hex.EncodeToString(request.PeerPathID[:]),
		"replaces_peer_path_id", hex.EncodeToString(request.ReplacesPeerPathID[:]),
		"service", request.Service, "requested_sockets", request.Sockets,
		"ordinary_connections", c.connections, "ordinary_socket_reservations", c.sockets,
		"replacement_connections", c.replacements, "queued", len(c.queue))
}
