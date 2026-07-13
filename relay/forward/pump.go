package forward

import (
	"context"
	"errors"
	"sync"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/relay/protocol"
)

const NominalBlockBytes = 1024 * 1024

const DefaultSessionQueueFrames = session.InFlightWindow * (NominalBlockBytes / session.MaxFrameSize)

const (
	DefaultConnectionQueueMessages = 256
	DefaultSessionControlMessages  = 32
)

type Writer interface {
	WriteText(data []byte) error
	WriteBinary(data []byte) error
}

type EnqueueResult int

const (
	Enqueued EnqueueResult = iota
	Overflow
	UnknownSession
	SessionTerminated
	PumpClosed
	ContextDone
)

type Options struct {
	ConnectionQueueMessages int
	SessionControlMessages  int
	SessionQueueFrames      int
}

type delivery struct {
	once sync.Once
	done chan error
}

func newDelivery() *delivery {
	return &delivery{done: make(chan error, 1)}
}

func (d *delivery) finish(err error) {
	d.once.Do(func() {
		d.done <- err
		close(d.done)
	})
}

type item struct {
	binary   bool
	data     []byte
	session  protocol.SessionID
	terminal bool
	borrowed bool
	delivery *delivery
}

type sessionQueue struct {
	control  []item
	data     [][]byte
	terminal bool
}

// Pump owns one connection writer. Connection-scoped messages have their own
// bounded lane. Every session has separate control and data queues; scheduling
// is round-robin by session, with that session's control always preceding data.
//
// A terminal is a state transition, not an ordinary high-priority item. Once
// accepted it atomically discards queued signals/data, rejects later traffic,
// and removes the queue only after the writer reports the terminal delivered.
type Pump struct {
	w    Writer
	opts Options

	mu         sync.Mutex
	wake       *sync.Cond
	connection []item
	sessions   map[protocol.SessionID]*sessionQueue
	rr         []protocol.SessionID
	rrNext     int
	writing    *item
	closed     bool
	err        error

	done chan struct{}
}

var (
	ErrPumpClosed    = errors.New("forward: pump closed")
	ErrSessionClosed = errors.New("forward: session closed before terminal delivery")
)

func NewPump(w Writer, opts Options) *Pump {
	p := newPump(w, opts)
	go p.run()
	return p
}

// newPump builds a pump without starting its writer loop, so tests can place a
// dequeue at an exact point relative to a concurrently parked producer.
func newPump(w Writer, opts Options) *Pump {
	if opts.ConnectionQueueMessages <= 0 {
		opts.ConnectionQueueMessages = DefaultConnectionQueueMessages
	}
	if opts.SessionControlMessages <= 0 {
		opts.SessionControlMessages = DefaultSessionControlMessages
	}
	if opts.SessionQueueFrames <= 0 {
		opts.SessionQueueFrames = DefaultSessionQueueFrames
	}
	p := &Pump{
		w:        w,
		opts:     opts,
		sessions: make(map[protocol.SessionID]*sessionQueue),
		done:     make(chan struct{}),
	}
	p.wake = sync.NewCond(&p.mu)
	return p
}

func (p *Pump) EnqueueConnection(binary bool, data []byte) EnqueueResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return PumpClosed
	}
	if len(p.connection) >= p.opts.ConnectionQueueMessages {
		return Overflow
	}
	p.connection = append(p.connection, item{binary: binary, data: snapshot(data)})
	p.wake.Broadcast()
	return Enqueued
}

// EnqueueConnectionBorrowed transfers an immutable payload into the connection
// lane without copying it. The caller must retain the payload's owner until the
// receipt completes; non-Enqueued results do not transfer ownership.
func (p *Pump) EnqueueConnectionBorrowed(binary bool, data []byte) (EnqueueResult, <-chan error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return PumpClosed, nil
	}
	if len(p.connection) >= p.opts.ConnectionQueueMessages {
		return Overflow, nil
	}
	receipt := newDelivery()
	p.connection = append(p.connection, item{
		binary:   binary,
		data:     data,
		borrowed: true,
		delivery: receipt,
	})
	p.wake.Broadcast()
	return Enqueued, receipt.done
}

func (p *Pump) OpenSession(id protocol.SessionID) EnqueueResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return PumpClosed
	}
	if q, ok := p.sessions[id]; ok {
		if q.terminal {
			return SessionTerminated
		}
		return Enqueued
	}
	p.sessions[id] = &sessionQueue{}
	p.rr = append(p.rr, id)
	p.wake.Broadcast()
	return Enqueued
}

func (p *Pump) CloseSession(id protocol.SessionID) EnqueueResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return PumpClosed
	}
	q, ok := p.sessions[id]
	if !ok {
		return UnknownSession
	}
	if q.terminal {
		// An accepted terminal owns session removal. Close must not revoke it or
		// allow the same ID to reopen while its write is still in flight.
		return SessionTerminated
	}
	for i := range q.control {
		if q.control[i].delivery != nil {
			q.control[i].delivery.finish(ErrSessionClosed)
		}
	}
	p.removeSessionLocked(id)
	p.wake.Broadcast()
	return Enqueued
}

func (p *Pump) EnqueueSessionControl(id protocol.SessionID, binary bool, data []byte) EnqueueResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return PumpClosed
	}
	q, ok := p.sessions[id]
	if !ok {
		return UnknownSession
	}
	if q.terminal {
		return SessionTerminated
	}
	if len(q.control) >= p.opts.SessionControlMessages {
		return Overflow
	}
	q.control = append(q.control, item{binary: binary, data: snapshot(data), session: id})
	p.wake.Broadcast()
	return Enqueued
}

func (p *Pump) EnqueueForward(id protocol.SessionID, frame []byte) EnqueueResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.enqueueForwardLocked(id, frame)
}

// EnqueueForwardContext applies producer backpressure until the session has
// capacity. Relay-server callers use EnqueueForward to isolate a slow session;
// local transport producers use this method because queue saturation is normal
// flow control, not a session failure.
func (p *Pump) EnqueueForwardContext(ctx context.Context, id protocol.SessionID, frame []byte) EnqueueResult {
	stop := context.AfterFunc(ctx, func() {
		p.mu.Lock()
		p.wake.Broadcast()
		p.mu.Unlock()
	})
	defer stop()

	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		if ctx.Err() != nil {
			return ContextDone
		}
		result := p.enqueueForwardLocked(id, frame)
		if result != Overflow {
			return result
		}
		p.wake.Wait()
	}
}

func (p *Pump) enqueueForwardLocked(id protocol.SessionID, frame []byte) EnqueueResult {
	if p.closed {
		return PumpClosed
	}
	q, ok := p.sessions[id]
	if !ok {
		return UnknownSession
	}
	if q.terminal {
		return SessionTerminated
	}
	if len(q.data) >= p.opts.SessionQueueFrames {
		return Overflow
	}
	q.data = append(q.data, snapshot(frame))
	p.wake.Broadcast()
	return Enqueued
}

// EnqueueSessionTerminal returns a receipt closed with the writer result. The
// terminal owns a reserved slot: even a signal flood cannot prevent the relay
// from explaining why that session was terminated.
func (p *Pump) EnqueueSessionTerminal(id protocol.SessionID, binary bool, data []byte) (EnqueueResult, <-chan error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return PumpClosed, nil
	}
	q, ok := p.sessions[id]
	if !ok {
		return UnknownSession, nil
	}
	if q.terminal {
		return SessionTerminated, nil
	}
	q.terminal = true
	q.control = nil
	q.data = nil
	receipt := newDelivery()
	q.control = append(q.control, item{
		binary:   binary,
		data:     snapshot(data),
		session:  id,
		terminal: true,
		delivery: receipt,
	})
	p.wake.Broadcast()
	return Enqueued, receipt.done
}

func snapshot(data []byte) []byte {
	return append([]byte(nil), data...)
}

func (p *Pump) Close() {
	p.mu.Lock()
	p.closeLocked(ErrPumpClosed)
	p.wake.Broadcast()
	p.mu.Unlock()
}

func (p *Pump) Done() <-chan struct{} { return p.done }

func (p *Pump) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *Pump) WaitIdle(ctx context.Context) bool {
	stop := context.AfterFunc(ctx, func() {
		p.mu.Lock()
		p.wake.Broadcast()
		p.mu.Unlock()
	})
	defer stop()
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		if p.closed {
			return false
		}
		if p.writing == nil && len(p.connection) == 0 && p.sessionBacklogEmptyLocked() {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		p.wake.Wait()
	}
}

func (p *Pump) sessionBacklogEmptyLocked() bool {
	for _, q := range p.sessions {
		if len(q.control) != 0 || len(q.data) != 0 {
			return false
		}
	}
	return true
}

func (p *Pump) next() (item, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		if p.closed {
			return item{}, false
		}
		if it, ok := p.dequeueLocked(); ok {
			p.writing = &it
			// Capacity frees at the dequeue, not when the write completes: the
			// post-write broadcast in run fires while the queue is still full.
			// Without this wakeup a producer parked in EnqueueForwardContext that
			// re-checked between that broadcast and this dequeue would stall until
			// the next write completes even though a slot just opened.
			p.wake.Broadcast()
			return it, true
		}
		p.wake.Wait()
	}
}

func (p *Pump) dequeueLocked() (item, bool) {
	if len(p.connection) > 0 {
		it := p.connection[0]
		p.connection = p.connection[1:]
		return it, true
	}
	return p.nextSessionLocked()
}

func (p *Pump) nextSessionLocked() (item, bool) {
	n := len(p.rr)
	for i := range n {
		idx := (p.rrNext + i) % n
		id := p.rr[idx]
		q := p.sessions[id]
		if q == nil {
			continue
		}
		var it item
		switch {
		case len(q.control) > 0:
			it = q.control[0]
			q.control = q.control[1:]
		case len(q.data) > 0 && !q.terminal:
			it = item{binary: true, data: q.data[0], session: id}
			q.data = q.data[1:]
		default:
			continue
		}
		p.rrNext = (idx + 1) % n
		return it, true
	}
	return item{}, false
}

func (p *Pump) run() {
	defer close(p.done)
	for {
		it, ok := p.next()
		if !ok {
			return
		}
		var err error
		if it.binary {
			err = p.w.WriteBinary(it.data)
		} else {
			err = p.w.WriteText(it.data)
		}

		p.mu.Lock()
		p.writing = nil
		if err != nil {
			p.failWriteLocked(&it, err)
		} else {
			p.completeWriteLocked(&it)
		}
		p.wake.Broadcast()
		p.mu.Unlock()
		if err != nil {
			return
		}
	}
}

// failWriteLocked settles the item whose write failed before the pump closes
// with that error: the item left the queues at dequeue and run has already
// cleared p.writing, so closeLocked cannot reach its receipt.
func (p *Pump) failWriteLocked(it *item, err error) {
	if it.borrowed {
		it.data = nil
	}
	if it.delivery != nil {
		it.delivery.finish(err)
	}
	p.closeLocked(err)
}

// completeWriteLocked settles a delivered item: a terminal completes its
// session removal only now, so the session ID cannot reopen while the
// terminal write is still in flight.
func (p *Pump) completeWriteLocked(it *item) {
	if it.terminal {
		p.removeSessionLocked(it.session)
	}
	if it.delivery == nil {
		return
	}
	if it.borrowed {
		it.data = nil
	}
	it.delivery.finish(nil)
}

func (p *Pump) closeLocked(err error) {
	if p.closed {
		return
	}
	p.closed = true
	p.err = err
	if p.writing != nil && p.writing.delivery != nil && !p.writing.borrowed {
		p.writing.delivery.finish(err)
	}
	for i := range p.connection {
		delivery := p.connection[i].delivery
		p.connection[i].data = nil
		if delivery != nil {
			delivery.finish(err)
		}
	}
	p.connection = nil
	for _, q := range p.sessions {
		for i := range q.control {
			if q.control[i].delivery != nil {
				q.control[i].delivery.finish(err)
			}
		}
	}
	// A closed pump can remain reachable through a completed connection. Drop
	// queued payload references immediately instead of retaining every session's
	// bounded backlog until that connection object is garbage-collected.
	p.sessions = nil
	p.rr = nil
	p.rrNext = 0
}

func (p *Pump) removeSessionLocked(id protocol.SessionID) {
	delete(p.sessions, id)
	for i, candidate := range p.rr {
		if candidate != id {
			continue
		}
		p.rr = append(p.rr[:i], p.rr[i+1:]...)
		if len(p.rr) == 0 {
			p.rrNext = 0
		} else {
			if p.rrNext > i {
				p.rrNext--
			}
			p.rrNext %= len(p.rr)
		}
		return
	}
}
