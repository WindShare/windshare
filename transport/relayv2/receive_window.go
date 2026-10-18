package relayv2

import (
	"bytes"
	"log/slog"
	"sync"

	"github.com/windshare/windshare/core/framechannel"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2/internal/receiveledger"
)

type receivedFrame struct {
	payload framechannel.Frame
	receipt receiveledger.Receipt
}

// A flow outlives its application channel: in-flight envelopes still need to be
// accounted for after local cancellation, until the relay freezes the prefix.
type receiveFlow struct {
	id                        v2.RelaySessionID
	session                   receiveledger.Session
	queue                     []receivedFrame
	recv                      chan framechannel.Frame
	wake                      chan struct{}
	abort                     chan struct{}
	done                      chan struct{}
	discard, sealed, disposed bool
}

// One mutex owns the connection budget and every queue transition. Application
// handoff and socket writes happen outside it, so an idle consumer cannot block
// siblings, control ingress, or the ordered feedback writer.
type receiveBuffer struct {
	mu         sync.Mutex
	ledger     *receiveledger.Ledger
	flows      map[v2.RelaySessionID]*receiveFlow
	closed     bool
	wakeWriter func()
	fail       func(error)
}

func newReceiveBuffer(id uint64, capacity v2.ReceiveCapacity, wake func(), fail func(error)) (*receiveBuffer, error) {
	r := &receiveBuffer{flows: make(map[v2.RelaySessionID]*receiveFlow), wakeWriter: wake, fail: fail}
	ledger, err := receiveledger.New(receiveledger.Config{
		ConnectionID: id, Capacity: capacity, MaxSessions: maxReceiveSessions, Observer: r,
	})
	if err != nil {
		return nil, err
	}
	r.ledger = ledger
	return r, nil
}

func (r *receiveBuffer) ObserveReceive(event receiveledger.Event) {
	slog.Debug("relay receive ledger", "link_id", event.ConnectionID, "session_id", event.SessionID,
		"action", event.Action, "progress", event.Progress, "usage", event.Usage, "error", event.Failure)
}

func (r *receiveBuffer) open(id v2.RelaySessionID) (*receiveFlow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, err := r.ledger.Open(id)
	if err != nil {
		return nil, err
	}
	f := &receiveFlow{id: id, session: session, recv: make(chan framechannel.Frame),
		wake: make(chan struct{}, 1), abort: make(chan struct{}), done: make(chan struct{})}
	r.flows[id] = f
	go r.run(f)
	return f, nil
}

func wakeReceive(f *receiveFlow) {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (r *receiveBuffer) receive(f *receiveFlow, payload []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	receipt, err := r.ledger.Receive(f.session, uint64(len(payload)+v2.OpaqueRouteHeaderBytes))
	if err != nil {
		return err
	}
	if f.disposed {
		// The worker has released the entire earlier prefix. No payload copy is
		// needed for late data on a locally cancelled, not-yet-frozen flow.
		return r.ledger.Discard(receipt)
	}
	f.queue = append(f.queue, receivedFrame{payload: bytes.Clone(payload), receipt: receipt})
	wakeReceive(f)
	return nil
}

func (r *receiveBuffer) retire(f *receiveFlow, discard bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed && r.flows[f.id] == f {
		_ = r.ledger.Retire(f.session)
	}
	// A graceful remote retirement preserves already accepted terminal frames.
	// Local owner release aborts handoff and actually drops their storage.
	f.sealed = true
	if discard && !f.discard {
		f.discard = true
		close(f.abort)
	}
	wakeReceive(f)
}

func (r *receiveBuffer) notice(id v2.RelaySessionID, notice v2.SettlementNotice) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.flows[id]
	if f == nil {
		// A duplicate after ordered acknowledgement must not recreate metadata.
		return nil
	}
	if err := r.ledger.Notice(f.session, notice); err != nil {
		return err
	}
	f.sealed = true
	wakeReceive(f)
	r.wakeWriter()
	return nil
}

func (r *receiveBuffer) run(f *receiveFlow) {
	defer r.wakeWriter()
	defer close(f.done)
	defer close(f.recv)
	for {
		r.mu.Lock()
		if len(f.queue) == 0 {
			if f.sealed {
				f.disposed = true
				r.mu.Unlock()
				return
			}
			r.mu.Unlock()
			<-f.wake
			continue
		}
		frame := f.queue[0]
		f.queue[0] = receivedFrame{}
		f.queue = f.queue[1:]
		if len(f.queue) == 0 {
			f.queue = nil
		}
		discard := f.discard
		r.mu.Unlock()
		if !discard {
			select {
			case <-f.abort:
				discard = true
			case f.recv <- frame.payload:
			}
		}
		// Release our reference before reporting consumption/discard. The receipt
		// remains bound to this exact flow even if the application has closed it.
		frame.payload = nil
		r.mu.Lock()
		var err error
		if !r.closed {
			if discard {
				err = r.ledger.Discard(frame.receipt)
			} else {
				err = r.ledger.Consume(frame.receipt)
			}
		}
		r.mu.Unlock()
		r.wakeWriter()
		if err != nil {
			// Failure ends feedback authority before the worker drops any
			// remaining payloads; it must not leave an abandoned queue behind.
			r.fail(err)
			r.retire(f, true)
		}
	}
}

func (r *receiveBuffer) close(discard bool) {
	r.mu.Lock()
	r.closed = true
	r.ledger.Close()
	var waiters []<-chan struct{}
	for _, f := range r.flows {
		f.sealed = true
		if discard && !f.discard {
			f.discard = true
			close(f.abort)
		}
		wakeReceive(f)
		if discard {
			waiters = append(waiters, f.done)
		}
	}
	r.mu.Unlock()
	for _, done := range waiters {
		<-done
	}
}

// take is called exclusively by the physical connection's ordered writer. A
// failed write closes that generation; issued feedback is never retried elsewhere.
func (r *receiveBuffer) take() ([]byte, v2.RelaySessionID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, v2.RelaySessionID{}, nil
	}
	for id, f := range r.flows {
		if !f.disposed {
			continue
		}
		select {
		case <-f.done:
		default:
			continue
		}
		if ack, ok := r.ledger.TakeSettlement(f.session); ok {
			encoded, err := ack.MarshalBinary()
			if err != nil {
				return nil, id, err
			}
			if err := r.ledger.Forget(f.session); err != nil {
				return nil, id, err
			}
			delete(r.flows, id)
			return encoded, id, nil
		}
	}
	if consumed, ok := r.ledger.TakeConsumed(); ok {
		encoded, err := consumed.MarshalBinary()
		return encoded, v2.RelaySessionID{}, err
	}
	return nil, v2.RelaySessionID{}, nil
}

var _ receiveledger.Observer = (*receiveBuffer)(nil)

func (c *Channel) stopReceiving() {
	if c.receive != nil {
		c.link.receives.retire(c.receive, true)
	}
}

func (c *Channel) deliver(frame []byte) error {
	return c.link.receives.receive(c.receive, frame)
}
