package v2endpoint

import (
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/relay/signaling/v2route"
)

// Storage in this window belongs to the receiving application, independently
// of the relay's forwarding queue. A completed socket write cannot replenish it.
type receiveWindow struct {
	frames, bytes int
}

func (s *Server) grantReceiveCredit(peer *connection, encoded []byte) error {
	credit, err := v2.ParseReceiveCredit(encoded)
	if err != nil || peer.roleValue() != roleReceiver {
		return ErrProtocol
	}
	resolution, err := s.registry.ResolveSession(credit.RelaySessionID, peer.ref)
	if err != nil {
		return ErrProtocol
	}
	if resolution.Disposition == v2route.SessionRetired {
		return nil
	}
	if resolution.Disposition != v2route.SessionForward {
		return ErrProtocol
	}
	peer.forwardMu.Lock()
	window := peer.receiveWindows[credit.RelaySessionID]
	if window == nil || peer.closed.Load() {
		peer.forwardMu.Unlock()
		return nil // Retirement may have won after route resolution.
	}
	if int(credit.Frames) > v2.ReceiveWindowFrames-window.frames ||
		int(credit.Bytes) > v2.ReceiveWindowBytes-window.bytes {
		peer.forwardMu.Unlock()
		return ErrProtocol
	}
	constrained := window.frames == 0 || window.bytes < MaximumV2WebSocketMessageSize
	window.frames += int(credit.Frames)
	window.bytes += int(credit.Bytes)
	trace := ForwardTrace{
		Source: resolution.Destination, Destination: peer.ref, SessionID: credit.RelaySessionID,
		AvailableFrames: window.frames, AvailableBytes: window.bytes,
	}
	available := window.frames > 0 && window.bytes >= MaximumV2WebSocketMessageSize
	peer.forwardMu.Unlock()
	if constrained && available {
		s.traceForward(trace, ForwardReceiveWindowAvailable)
	}
	select {
	case peer.wake <- struct{}{}:
	default:
	}
	return nil
}
