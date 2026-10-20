package turnpath

import (
	"context"
	"errors"
	"time"
)

var ErrAccountUnavailable = errors.New("TURN account unavailable")

// WatchTransport charges actual socket deltas, including allocation traffic and
// retransmission. The owner joins this watcher after closing transport, before
// releasing the account lease, so the final sample includes shutdown traffic.
func WatchTransport(parent context.Context, lease *Lease, sample func() Usage, stop func(error)) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	var previous Usage
	charge := func() {
		current := sample()
		lease.Charge(Usage{
			TransportSent:     current.TransportSent - min(previous.TransportSent, current.TransportSent),
			TransportReceived: current.TransportReceived - min(previous.TransportReceived, current.TransportReceived),
		})
		previous = current
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(MaintenanceInterval)
		defer ticker.Stop()
		defer charge()
		for {
			select {
			case <-ctx.Done():
				return
			case <-lease.Lost():
				stop(ErrAccountUnavailable)
				return
			case <-ticker.C:
				charge()
				if !lease.Relay().Credentials().ValidAt(time.Now()) {
					stop(ErrAccountUnavailable)
					return
				}
			}
		}
	}()
	return func() { cancel(); <-done; charge() }
}
