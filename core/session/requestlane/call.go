package requestlane

import (
	"sync"
	"time"
)

// Call owns at most one reservation across proven-unsent admission retries.
// Its lifetime is independent of trace collection and application response sinks.
type Call struct {
	mu          sync.Mutex
	reservation *Reservation
	estimate    Estimate
	closed      bool
}

func (call *Call) Reserve(reservation *Reservation, estimate Estimate) bool {
	call.mu.Lock()
	defer call.mu.Unlock()
	if call.closed {
		reservation.Abandon()
		return false
	}
	call.reservation.Abandon()
	call.reservation = reservation
	call.estimate = estimate
	return true
}

func (call *Call) Complete(now time.Time) {
	call.mu.Lock()
	defer call.mu.Unlock()
	call.reservation.Complete(now)
}

func (call *Call) Close() {
	call.mu.Lock()
	defer call.mu.Unlock()
	call.closed = true
	call.reservation.Abandon()
	call.reservation = nil
}

func (call *Call) Estimate() Estimate {
	call.mu.Lock()
	defer call.mu.Unlock()
	return call.estimate
}
