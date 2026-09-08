package socketauthority

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"time"
)

type EventKind string

const (
	STUNRefreshFinished   EventKind = "stun_refresh_finished"
	SocketHandoffStarted  EventKind = "socket_handoff_started"
	SocketHandoffFinished EventKind = "socket_handoff_finished"
)

// Socket work belongs to a path, not an ICE attempt that may already have ended.
type Event struct {
	ProtocolSessionID   [16]byte
	PeerPathID          [16]byte
	NetworkGenerationID uint64
	Kind                EventKind
	At                  time.Time
	Duration            time.Duration
	Local, Server       netip.AddrPort
	Result              string
}

func (l *Lease) observe(event Event) {
	if observe := l.authority.config.Observe; observe != nil {
		event.ProtocolSessionID = l.entry.key.session
		event.PeerPathID = l.entry.key.path
		event.NetworkGenerationID = l.entry.key.generation
		event.At = time.Now()
		observe(event)
	}
}

func socketResult(err error) string {
	var timeout net.Error
	switch {
	case err == nil:
		return "completed"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
		return "deadline"
	case errors.Is(err, ErrRetired):
		return "retired"
	case errors.Is(err, ErrClosed):
		return "closed"
	default:
		return "failed"
	}
}
