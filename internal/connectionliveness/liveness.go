// Package connectionliveness judges a connection dead only after a full timeout
// without accepted inbound frames. One delayed reply, such as an acknowledgement
// queued behind content, is never evidence of loss, so liveness does not depend
// on any single round trip or on WebSocket Ping/Pong support by the host.
package connectionliveness

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

var ErrSilent = errors.New("connection liveness: no accepted inbound frame within timeout")

// Monitor tracks the newest accepted inbound frame of one connection. Readers
// observe while a separate owner evaluates silence, so the instant is atomic.
type Monitor struct {
	origin time.Time
	// Offset from origin keeps the monotonic clock reading that time.Time.Sub
	// uses, which a wall-clock UnixNano would lose across clock adjustments.
	last atomic.Int64
}

// NewMonitor starts the silence clock at now, so a connection that never
// delivers a frame still fails after one timeout.
func NewMonitor(now time.Time) *Monitor {
	return &Monitor{origin: now}
}

// Observe records an inbound frame the connection's protocol accepted. Frames
// that close the connection as violations are not evidence and need no call.
func (m *Monitor) Observe(now time.Time) {
	offset := int64(now.Sub(m.origin))
	for {
		previous := m.last.Load()
		// Concurrent or reordered observations cannot move evidence backwards.
		if offset <= previous || m.last.CompareAndSwap(previous, offset) {
			return
		}
	}
}

// Silence reports how long the connection has gone without accepted inbound frames.
func (m *Monitor) Silence(now time.Time) time.Duration {
	return now.Sub(m.origin) - time.Duration(m.last.Load())
}

// Watch blocks until the connection has been silent for timeout and returns
// that silence with ErrSilent, or returns the context's error first. Each wake
// re-reads the evidence, so traffic defers expiry without resetting a timer
// per frame.
func Watch(ctx context.Context, m *Monitor, timeout time.Duration) (time.Duration, error) {
	for {
		silence := m.Silence(time.Now())
		if silence >= timeout {
			return silence, ErrSilent
		}
		timer := time.NewTimer(timeout - silence)
		select {
		case <-ctx.Done():
			timer.Stop()
			return silence, ctx.Err()
		case <-timer.C:
		}
	}
}
