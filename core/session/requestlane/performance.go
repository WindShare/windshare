// Package requestlane estimates control-response completion independently of
// content throughput, transport providers, and operation delivery authority.
package requestlane

import (
	"sync"
	"time"

	"github.com/windshare/windshare/core/session/protocolsession"
)

const (
	InitialResponse = 250 * time.Millisecond
	MinimumResponse = time.Millisecond
	MaximumEstimate = time.Hour
	sampleWeight    = 0.25
	sampleLifetime  = 30 * time.Second
)

// Managed excludes streams and negotiations: their lifetime is not a control RTT.
func Managed(kind protocolsession.MessageKind) bool {
	switch kind {
	case protocolsession.MessageListChildren, protocolsession.MessageOpenRevisions,
		protocolsession.MessageRenewLease, protocolsession.MessageReleaseLease:
		return true
	default:
		return false
	}
}

type sample struct {
	response time.Duration
	at       time.Time
}

// Estimate records the decision inputs before the new request is reserved.
type Estimate struct {
	Response      time.Duration
	QueuedContent time.Duration
	Expected      time.Duration
	Pending       uint32
}

// Lane belongs to one physical incarnation. Retired reservations may finish on
// this object but cannot update a replacement lane with the same logical ID.
type Lane struct {
	mu      sync.Mutex
	initial time.Duration
	samples map[protocolsession.MessageKind]sample
	pending map[*Reservation]struct{}
}

// New accepts measured latency, including a sub-clock-resolution zero sample.
// Callers without evidence supply InitialResponse explicitly.
func New(initial time.Duration) *Lane {
	return &Lane{
		initial: bounded(initial),
		samples: make(map[protocolsession.MessageKind]sample),
		pending: make(map[*Reservation]struct{}),
	}
}

func (lane *Lane) Estimate(kind protocolsession.MessageKind, now time.Time, queued time.Duration) Estimate {
	lane.mu.Lock()
	defer lane.mu.Unlock()
	response := lane.initial
	if measured, ok := lane.samples[kind]; ok && now.Sub(measured.at) < sampleLifetime {
		response = measured.response
	}
	// An unfinished request is already evidence of delay. Waiting for its final
	// sample would keep admitting work to a stalled path with an optimistic score.
	for reservation := range lane.pending {
		response = max(response, bounded(now.Sub(reservation.started)))
	}
	queued = min(max(queued, 0), MaximumEstimate)
	pending := uint32(len(lane.pending))
	expected := time.Duration(min(float64(response)*float64(pending+1)+float64(queued), float64(MaximumEstimate)))
	return Estimate{Response: response, QueuedContent: queued, Expected: expected, Pending: pending}
}

// Reservation is scheduling accounting only; it never authorizes sends or retries.
type Reservation struct {
	lane    *Lane
	kind    protocolsession.MessageKind
	started time.Time
}

func (lane *Lane) Reserve(kind protocolsession.MessageKind, now time.Time) *Reservation {
	reservation := &Reservation{lane: lane, kind: kind, started: now}
	lane.mu.Lock()
	lane.pending[reservation] = struct{}{}
	lane.mu.Unlock()
	return reservation
}

// Complete samples the authenticated final response, before application decoding,
// disk publication, or caller cleanup can inflate the path's response estimate.
func (reservation *Reservation) Complete(now time.Time) {
	if reservation == nil {
		return
	}
	lane := reservation.lane
	lane.mu.Lock()
	defer lane.mu.Unlock()
	if _, active := lane.pending[reservation]; !active {
		return
	}
	delete(lane.pending, reservation)
	elapsed := bounded(now.Sub(reservation.started))
	previous, ok := lane.samples[reservation.kind]
	if ok && elapsed < previous.response && now.Sub(previous.at) < sampleLifetime {
		elapsed = previous.response + time.Duration(sampleWeight*float64(elapsed-previous.response))
	}
	lane.samples[reservation.kind] = sample{response: elapsed, at: now}
}

// Abandon releases accounting without mistaking caller cancellation for a fast
// response or a transport failure. Repeated settlement is deliberately harmless.
func (reservation *Reservation) Abandon() {
	if reservation == nil {
		return
	}
	reservation.lane.mu.Lock()
	delete(reservation.lane.pending, reservation)
	reservation.lane.mu.Unlock()
}

func bounded(value time.Duration) time.Duration {
	return min(max(value, MinimumResponse), MaximumEstimate)
}
