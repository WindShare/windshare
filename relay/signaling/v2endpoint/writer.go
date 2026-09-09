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
		wroteSessionControl, err := s.writeSessionControls(ctx, peer)
		if err != nil {
			peer.requestClose()
			return err
		}
		if frame, ok := peer.takeForward(); ok {
			if err := s.write(ctx, peer.socket, frame); err != nil {
				peer.requestClose()
				return err
			}
			s.completeForward(peer, frame)
			continue
		}
		if wroteSessionControl {
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

func (s *Server) writeSessionControls(ctx context.Context, peer *connection) (bool, error) {
	wrote := false
	if retired, ok := peer.takeSessionRetirement(); ok {
		encoded, err := retired.MarshalBinary()
		if err != nil {
			return false, err
		}
		if err := s.write(ctx, peer.socket, encoded); err != nil {
			return false, err
		}
		wrote = true
	}
	credit, trace, ok := peer.takeSenderCredit()
	if !ok {
		return wrote, nil
	}
	encoded, err := credit.MarshalBinary()
	if err != nil {
		return wrote, err
	}
	if err := s.write(ctx, peer.socket, encoded); err != nil {
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

func (peer *connection) tryForward(sessionID v2.RelaySessionID, encoded []byte) (bool, <-chan struct{}, ForwardTrace) {
	peer.sessionMu.Lock()
	defer peer.sessionMu.Unlock()
	if peer.closed.Load() {
		return false, nil, ForwardTrace{}
	}
	if _, active := peer.sessions[sessionID]; !active {
		return false, nil, ForwardTrace{}
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
		if peer.forwardChanged == nil {
			peer.forwardChanged = make(chan struct{})
		}
		return false, peer.forwardChanged, trace
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
	return true, nil, trace
}

func (peer *connection) forwardCapacityChangedLocked() {
	if peer.forwardChanged != nil {
		close(peer.forwardChanged)
		peer.forwardChanged = nil
	}
}

func (peer *connection) takeForward() ([]byte, bool) {
	peer.forwardMu.Lock()
	defer peer.forwardMu.Unlock()
	if len(peer.forwardOrder) == 0 {
		return nil, false
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
		queue.frames[0] = nil
		queue.frames = queue.frames[1:]
		queue.bytes -= len(frame)
		peer.forwardFrames--
		peer.forwardBytes -= len(frame)
		peer.forwardCapacityChangedLocked()
		return frame, true
	}
	return nil, false
}
