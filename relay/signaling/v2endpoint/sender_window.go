package v2endpoint

import (
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/relay/signaling/v2route"
)

type senderWindow struct {
	frames, bytes               int
	pendingFrames, pendingBytes uint32
}

func (peer *connection) consumeSenderCredit(id v2.RelaySessionID, size int) (ForwardTrace, bool) {
	peer.sessionMu.Lock()
	defer peer.sessionMu.Unlock()
	window := peer.windows[id]
	trace := ForwardTrace{SessionID: id}
	if window != nil {
		trace.AvailableFrames, trace.AvailableBytes = window.frames, window.bytes
	}
	if peer.closed.Load() || window == nil || window.frames == 0 || window.bytes < size {
		return trace, false
	}
	wasConstrained := window.constrained()
	window.frames--
	window.bytes -= size
	trace.AvailableFrames, trace.AvailableBytes = window.frames, window.bytes
	if !wasConstrained && window.constrained() {
		trace.Stage = ForwardWindowConstrained
	}
	return trace, true
}

func (window *senderWindow) constrained() bool {
	return window.frames == 0 || window.bytes < MaximumV2WebSocketMessageSize
}

func (peer *connection) replenishSenderCredit(id v2.RelaySessionID, size int) {
	peer.sessionMu.Lock()
	defer peer.sessionMu.Unlock()
	window := peer.windows[id]
	if peer.closed.Load() || window == nil {
		return
	}
	window.pendingFrames++
	window.pendingBytes += uint32(size)
	// Coalescing by live session keeps credit notifications bounded even while
	// the sender is not reading; a burst must not overflow the control queue.
	select {
	case peer.wake <- struct{}{}:
	default:
	}
}

func (peer *connection) takeSenderCredit() (v2.SessionCredit, ForwardTrace, bool) {
	peer.sessionMu.Lock()
	defer peer.sessionMu.Unlock()
	for id, window := range peer.windows {
		if window.pendingFrames == 0 {
			continue
		}
		credit := v2.SessionCredit{
			RelaySessionID: id, Frames: window.pendingFrames, Bytes: window.pendingBytes,
		}
		wasConstrained := window.constrained()
		window.frames += int(window.pendingFrames)
		window.bytes += int(window.pendingBytes)
		window.pendingFrames, window.pendingBytes = 0, 0
		trace := ForwardTrace{Source: peer.ref, SessionID: id, AvailableFrames: window.frames, AvailableBytes: window.bytes}
		if wasConstrained && !window.constrained() {
			trace.Stage = ForwardWindowAvailable
		}
		return credit, trace, true
	}
	return v2.SessionCredit{}, ForwardTrace{}, false
}

func (s *Server) completeForward(destination *connection, encoded []byte) {
	if destination.roleValue() != roleReceiver {
		return
	}
	route, err := v2.ParseOpaqueRoute(encoded)
	if err != nil {
		return
	}
	resolution, err := s.registry.ResolveSession(route.RelaySessionID, destination.ref)
	if err != nil || resolution.Disposition != v2route.SessionForward {
		return
	}
	if sender, _, _ := s.connections.resolve(resolution.Destination); sender != nil {
		sender.replenishSenderCredit(route.RelaySessionID, len(encoded))
	}
}
