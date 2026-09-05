package socketauthority

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"time"
)

const MaxIdleServers = 2
const MaxIdleHold = 2 * time.Minute

// StartIdle holds NAT discovery mappings only until the caller's already
// budgeted fresh attempt deadline. It does not retain a demand reference and
// therefore cannot keep an otherwise unused endpoint or generation alive.
// Repeated handoffs cancel and join the previous heartbeat before replacement.
func (l *Lease) StartIdle(ctx context.Context, servers []netip.AddrPort, deadline time.Time) error {
	if l == nil || ctx == nil || len(servers) == 0 || len(servers) > MaxIdleServers || !deadline.After(time.Now()) || time.Until(deadline) > MaxIdleHold {
		return ErrInvalid
	}
	servers = slices.Clone(servers)
	for _, server := range servers {
		if !server.IsValid() || server.Port() == 0 {
			return ErrInvalid
		}
	}
	a := l.authority
	a.mu.Lock()
	defer a.mu.Unlock()
	if l.released || a.closed {
		return ErrClosed
	}
	if l.entry.key.generation <= a.retiredThrough {
		return ErrRetired
	}
	if l.entry.active {
		return ErrActive
	}
	if l.entry.idleCancel != nil {
		l.entry.idleCancel()
		<-l.entry.idleDone
	}
	idleContext, cancel := context.WithDeadline(ctx, deadline)
	done := make(chan struct{})
	l.entry.idleCancel = cancel
	l.entry.idleDone = done
	go l.runIdle(idleContext, cancel, done, servers, deadline)
	return nil
}

func (l *Lease) runIdle(ctx context.Context, cancel context.CancelFunc, done chan<- struct{}, servers []netip.AddrPort, deadline time.Time) {
	defer close(done)
	defer cancel()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if !l.refreshIdle(ctx, servers, deadline) {
			return
		}
		timer.Reset(l.authority.config.IdleInterval)
	}
}

func (l *Lease) refreshIdle(ctx context.Context, servers []netip.AddrPort, deadline time.Time) bool {
	for _, server := range servers {
		for _, endpoint := range l.entry.mux.endpoints {
			if ctx.Err() != nil {
				return false
			}
			local := endpoint.GetListenAddresses()[0].(*net.UDPAddr).AddrPort()
			if local.Addr().Is4() != server.Addr().Is4() {
				continue
			}
			// Refresh bypasses the discovery cache: socket survival alone does not
			// prove a destination-specific NAT binding is still alive.
			timeout := l.authority.config.RefreshTimeout
			if remaining := time.Until(deadline); remaining < timeout {
				timeout = remaining
			}
			if timeout <= 0 {
				return false
			}
			_, _ = endpoint.RefreshXORMappedAddr(net.UDPAddrFromAddrPort(server), timeout)
		}
	}
	return true
}
