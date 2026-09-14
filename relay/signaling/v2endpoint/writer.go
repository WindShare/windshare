package v2endpoint

import (
	"bytes"
	"context"

	"github.com/coder/websocket"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

type controlWrite struct {
	data []byte
	done chan error
}

type forwardQueue struct {
	frames [][]byte
	bytes  int
}

func (s *Server) writeLoop(ctx context.Context, peer *connection) error {
	for {
		select {
		case item := <-peer.control:
			err := s.write(ctx, peer.socket, item.data)
			item.done <- err
			if err != nil {
				peer.requestClose()
				return err
			}
		default:
		}
		wroteSessionTraffic, err := s.writeSessionTraffic(ctx, peer)
		if err != nil {
			peer.requestClose()
			return err
		}
		if wroteSessionTraffic {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case item := <-peer.control:
			err := s.write(ctx, peer.socket, item.data)
			item.done <- err
			if err != nil {
				peer.requestClose()
				return err
			}
		case <-peer.wake:
		}
	}
}

func (s *Server) writeSessionTraffic(ctx context.Context, peer *connection) (bool, error) {
	// Atomic route publication can admit receivers before REGISTERED is
	// written. Keep their traffic queued until the handshake response is on
	// the wire; otherwise a valid session frame becomes a malformed greeting.
	if !peer.handshakeComplete.Load() {
		return false, nil
	}
	wroteControl, err := s.writeSessionControls(ctx, peer)
	if err != nil {
		return false, err
	}
	if frame, trace, ok := peer.takeForwardWithTrace(); ok {
		if trace.Stage != "" {
			if route, err := s.registry.ResolveSession(trace.SessionID, peer.ref); err == nil {
				trace.Source = route.Destination
			}
			s.traceForward(trace, trace.Stage)
		}
		if err := s.writeSessionData(ctx, peer, frame); err != nil {
			return false, err
		}
		s.completeForward(peer, frame)
		return true, nil
	}
	return wroteControl, nil
}

func (s *Server) writeSessionControls(ctx context.Context, peer *connection) (bool, error) {
	wrote := false
	if retired, ok := peer.takeSessionRetirement(); ok {
		encoded, err := retired.MarshalBinary()
		if err != nil {
			return false, err
		}
		if err := s.writeSessionData(ctx, peer, encoded); err != nil {
			return false, err
		}
		wrote = true
	}
	credit, trace, ok := peer.takeForwardCredit()
	if !ok {
		return wrote, nil
	}
	encoded, err := credit.MarshalBinary()
	if err != nil {
		return wrote, err
	}
	if err := s.writeSessionData(ctx, peer, encoded); err != nil {
		return wrote, err
	}
	if trace.Stage != "" {
		if route, err := s.registry.ResolveSession(credit.RelaySessionID, peer.ref); err == nil {
			trace.Destination = route.Destination
			s.traceForward(trace, trace.Stage)
		}
	}
	return true, nil
}

func (s *Server) writeSessionData(ctx context.Context, peer *connection, data []byte) error {
	// A session cycle may contain retirement, credit, and content writes. Probe
	// responses must wait for at most the current physical write, not that batch.
	select {
	case item := <-peer.control:
		err := s.write(ctx, peer.socket, item.data)
		item.done <- err
		if err != nil {
			return err
		}
	default:
	}
	return s.write(ctx, peer.socket, data)
}

func (s *Server) write(parent context.Context, socket BinaryConnection, data []byte) error {
	ctx := parent
	cancel := func() {}
	if s.writeTimeout > 0 {
		ctx, cancel = context.WithTimeout(parent, s.writeTimeout)
	}
	defer cancel()
	return socket.Write(ctx, websocket.MessageBinary, data)
}

func (peer *connection) sendControl(ctx context.Context, data []byte) error {
	item := controlWrite{data: bytes.Clone(data), done: make(chan error, 1)}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case peer.control <- item:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-item.done:
		return err
	}
}

func (peer *connection) tryForward(sessionID v2.RelaySessionID, encoded []byte) (bool, ForwardTrace) {
	peer.sessionMu.Lock()
	defer peer.sessionMu.Unlock()
	if peer.closed.Load() {
		return false, ForwardTrace{}
	}
	if _, active := peer.sessions[sessionID]; !active {
		return false, ForwardTrace{}
	}
	peer.forwardMu.Lock()
	defer peer.forwardMu.Unlock()
	queue := peer.forward[sessionID]
	trace := ForwardTrace{ConnectionFrames: peer.forwardFrames, ConnectionBytes: peer.forwardBytes}
	if queue != nil {
		trace.SessionFrames, trace.SessionBytes = len(queue.frames), queue.bytes
	}
	if trace.SessionFrames >= MaximumSessionQueueFrames || trace.SessionBytes+len(encoded) > MaximumSessionQueueBytes ||
		peer.forwardFrames >= MaximumForwardQueueFrames || peer.forwardBytes+len(encoded) > MaximumForwardQueueBytes {
		return false, trace
	}
	if queue == nil {
		queue = &forwardQueue{}
		peer.forward[sessionID] = queue
		peer.forwardOrder = append(peer.forwardOrder, sessionID)
	}
	queue.frames = append(queue.frames, bytes.Clone(encoded))
	queue.bytes += len(encoded)
	peer.forwardFrames++
	peer.forwardBytes += len(encoded)
	select {
	case peer.wake <- struct{}{}:
	default:
	}
	return true, trace
}

func (peer *connection) takeForwardWithTrace() ([]byte, ForwardTrace, bool) {
	peer.forwardMu.Lock()
	defer peer.forwardMu.Unlock()
	if len(peer.forwardOrder) == 0 {
		return nil, ForwardTrace{}, false
	}
	for range len(peer.forwardOrder) {
		if peer.forwardCursor >= len(peer.forwardOrder) {
			peer.forwardCursor = 0
		}
		id := peer.forwardOrder[peer.forwardCursor]
		peer.forwardCursor++
		queue := peer.forward[id]
		if queue == nil || len(queue.frames) == 0 {
			continue
		}
		frame := queue.frames[0]
		trace := ForwardTrace{}
		if window := peer.receiveWindows[id]; window != nil {
			if window.frames == 0 || window.bytes < len(frame) {
				continue
			}
			wasConstrained := window.frames == 0 || window.bytes < MaximumV2WebSocketMessageSize
			window.frames--
			window.bytes -= len(frame)
			if !wasConstrained && (window.frames == 0 || window.bytes < MaximumV2WebSocketMessageSize) {
				trace = ForwardTrace{Stage: ForwardReceiveWindowConstrained, Destination: peer.ref,
					SessionID: id, AvailableFrames: window.frames, AvailableBytes: window.bytes}
			}
		}
		queue.frames[0] = nil
		queue.frames = queue.frames[1:]
		queue.bytes -= len(frame)
		peer.forwardFrames--
		peer.forwardBytes -= len(frame)
		return frame, trace, true
	}
	return nil, ForwardTrace{}, false
}
