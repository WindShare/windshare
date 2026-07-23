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
		if frame, ok := peer.takeForward(); ok {
			if err := s.write(ctx, peer.socket, frame); err != nil {
				peer.requestClose()
				return err
			}
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

func (peer *connection) enqueueControl(data []byte) bool {
	if peer == nil || peer.closed.Load() {
		return false
	}
	item := controlWrite{data: bytes.Clone(data), done: make(chan error, 1)}
	select {
	case peer.control <- item:
		return true
	default:
		return false
	}
}

func (peer *connection) enqueueForward(sessionID v2.RelaySessionID, encoded []byte) bool {
	peer.sessionMu.Lock()
	defer peer.sessionMu.Unlock()
	if peer.closed.Load() {
		return false
	}
	if _, active := peer.sessions[sessionID]; !active {
		return false
	}
	peer.forwardMu.Lock()
	defer peer.forwardMu.Unlock()
	queue := peer.forward[sessionID]
	if queue == nil {
		queue = &forwardQueue{}
		peer.forward[sessionID] = queue
		peer.forwardOrder = append(peer.forwardOrder, sessionID)
	}
	if len(queue.frames) >= MaximumSessionQueueFrames || queue.bytes+len(encoded) > MaximumSessionQueueBytes ||
		peer.forwardFrames >= MaximumForwardQueueFrames || peer.forwardBytes+len(encoded) > MaximumForwardQueueBytes {
		return false
	}
	queue.frames = append(queue.frames, bytes.Clone(encoded))
	queue.bytes += len(encoded)
	peer.forwardFrames++
	peer.forwardBytes += len(encoded)
	select {
	case peer.wake <- struct{}{}:
	default:
	}
	return true
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
		return frame, true
	}
	return nil, false
}
