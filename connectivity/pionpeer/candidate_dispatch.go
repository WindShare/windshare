package pionpeer

import (
	"context"
	"sync"

	pion "github.com/pion/webrtc/v4"
)

// The connection owns the Pion subscription. Replacing an application callback
// retires only its delivery and discovery work, so a Pion callback racing the
// handoff still posts to the same ordered stream.
type candidateDispatch struct {
	mu               sync.Mutex
	current          *candidateSubscription
	queue            []candidateEvent
	draining         bool
	pionEnded        bool
	observing        bool
	afterObservation func()
}

type candidateSubscription struct {
	connection *Connection
	callback   func(*pion.ICECandidate)
	ctx        context.Context
	cancel     context.CancelFunc
	probes     sync.WaitGroup
	ready      bool

	// The single drain owns gathering state, including when callbacks replace
	// themselves while handling a host or its mapped candidate.
	seen      map[MappedEndpoint]bool
	pending   int
	pionEnded bool
}

type candidateEvent struct {
	candidate *pion.ICECandidate
	late      bool
	probe     *hostProbeResult
}

func (c *Connection) OnICECandidate(callback func(*pion.ICECandidate)) {
	c.candidateMu.Lock()
	defer c.candidateMu.Unlock()
	if c.closed {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.candidates.replace(&candidateSubscription{
		connection: c, callback: callback, ctx: ctx, cancel: cancel, seen: make(map[MappedEndpoint]bool),
	})
	c.PeerConnection.OnICECandidate(c.candidates.receive)
}

// Callers serialize handoffs. Pion may enqueue for the successor during old
// socket cleanup, but its probes cannot start until the predecessor has left.
func (d *candidateDispatch) replace(next *candidateSubscription) {
	d.mu.Lock()
	old := d.current
	d.current = next
	d.queue = nil
	d.mu.Unlock()
	if old != nil {
		old.cancel()
		old.probes.Wait()
	}
	if next != nil {
		d.mu.Lock()
		next.ready = true
		d.wakeLocked()
		d.mu.Unlock()
	}
}

func (d *candidateDispatch) receive(candidate *pion.ICECandidate) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.current == nil || (candidate == nil && d.pionEnded) {
		return
	}
	event := candidateEvent{candidate: candidate, late: d.pionEnded}
	if candidate == nil {
		d.pionEnded = true
	}
	d.enqueueLocked(event)
}

func (d *candidateDispatch) enqueueLocked(event candidateEvent) {
	d.queue = append(d.queue, event)
	d.wakeLocked()
}

func (d *candidateDispatch) wakeLocked() {
	if !d.draining && d.current.ready && len(d.queue) != 0 {
		d.draining = true
		go d.drain()
	}
}

func (d *candidateDispatch) drain() {
	d.mu.Lock()
	for d.current != nil && d.current.ready && len(d.queue) != 0 {
		sub, event := d.current, d.queue[0]
		d.queue[0] = candidateEvent{}
		d.queue = d.queue[1:]
		d.mu.Unlock()
		sub.handle(event)
		d.mu.Lock()
	}
	if len(d.queue) == 0 {
		d.queue = nil
	}
	d.draining = false
	d.mu.Unlock()
}

// Only the drain admits observations and application callbacks. A handoff can
// revoke the rest of an event even from inside its observer. An application
// callback already in progress may finish independently of socket ownership.
func (s *candidateSubscription) observe(publish func()) bool {
	d := &s.connection.candidates
	d.mu.Lock()
	if d.current != s {
		d.mu.Unlock()
		return false
	}
	d.observing = true
	d.mu.Unlock()
	publish()
	d.mu.Lock()
	d.observing = false
	active := d.current == s
	after := d.afterObservation
	d.afterObservation = nil
	d.mu.Unlock()
	if after != nil {
		after()
	}
	return active
}

// An observer can itself call Close. Preserve the terminal observation order
// without holding the lease or waiting for an application callback to return.
func (d *candidateDispatch) observeClosed(publish func()) {
	d.mu.Lock()
	if d.observing {
		d.afterObservation = publish
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()
	publish()
}
