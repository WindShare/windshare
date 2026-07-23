package v2endpoint

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/relay/signaling/v2route"
)

type connectionRole uint8

const (
	roleUnknown connectionRole = iota
	roleSender
	roleReceiver
	roleStopper
)

type connection struct {
	ref        v2route.ConnectionRef
	socket     BinaryConnection
	cancel     context.CancelFunc
	cancelOnce sync.Once

	roleMu sync.Mutex
	role   connectionRole
	share  v2.ShareID

	control chan controlWrite
	wake    chan struct{}

	forwardMu     sync.Mutex
	forward       map[v2.RelaySessionID]*forwardQueue
	forwardOrder  []v2.RelaySessionID
	forwardCursor int
	forwardFrames int
	forwardBytes  int

	sessionMu sync.Mutex
	sessions  map[v2.RelaySessionID]struct{}
	closed    atomic.Bool
}

func newConnection(ref v2route.ConnectionRef, socket BinaryConnection, cancel context.CancelFunc) *connection {
	return &connection{
		ref: ref, socket: socket, cancel: cancel,
		control: make(chan controlWrite, MaximumControlQueueFrames), wake: make(chan struct{}, 1),
		forward: make(map[v2.RelaySessionID]*forwardQueue), sessions: make(map[v2.RelaySessionID]struct{}),
	}
}

func (s *Server) cleanup(peer *connection) {
	peer.closed.Store(true)
	// Detach before Registry/apply work so a replacement may reuse the wire ID;
	// every delayed result below remains bound to peer.ref and cannot target it.
	s.connections.detach(peer.ref)
	role := peer.roleValue()
	if role == roleSender {
		retirement, transitioned := s.registry.UnexpectedDisconnect(peer.shareValue(), peer.ref)
		if transitioned {
			s.applyRouteRetirement(retirement, RetirementSourceDisconnect)
		}
	} else {
		for _, sessionID := range peer.sessionIDs() {
			s.endSession(sessionID, peer.ref)
		}
	}
}

func (s *Server) endSession(
	sessionID v2.RelaySessionID,
	source v2route.ConnectionRef,
) (v2route.SessionRetirement, bool) {
	retirement, ended := s.registry.EndSession(sessionID, source)
	if ended {
		s.applySessionRetirement(retirement)
	}
	return retirement, ended
}

func (s *Server) applySessionRetirement(retirement v2route.SessionRetirement) {
	sender, _, _ := s.connections.resolve(retirement.Sender)
	receiver, _, _ := s.connections.resolve(retirement.Receiver)
	if sender != nil && sender.removeSession(retirement.RelaySessionID) {
		s.notifySessionRetired(sender, retirement.RelaySessionID)
	}
	if receiver != nil && receiver.removeSession(retirement.RelaySessionID) {
		s.notifySessionRetired(receiver, retirement.RelaySessionID)
	}
}

func (s *Server) applyRouteRetirement(retirement v2route.RouteRetirement, source RetirementSource) {
	for _, session := range retirement.Sessions {
		if sender, result, currentGeneration := s.connections.resolve(session.Sender); sender != nil {
			applied := sender.removeSession(session.RelaySessionID)
			s.traceRetirement(session.Sender, source, RetirementTargetSessionSender, result, currentGeneration, applied)
		} else {
			s.traceRetirement(session.Sender, source, RetirementTargetSessionSender, result, currentGeneration, false)
		}
		if receiver, result, currentGeneration := s.connections.resolve(session.Receiver); receiver != nil {
			removed := receiver.removeSession(session.RelaySessionID)
			applied := receiver.requestClose()
			s.traceRetirement(session.Receiver, source, RetirementTargetSessionReceiver, result, currentGeneration, removed || applied)
		} else {
			s.traceRetirement(session.Receiver, source, RetirementTargetSessionReceiver, result, currentGeneration, false)
		}
	}
	if !retirement.Owner.Valid() {
		return
	}
	if owner, result, currentGeneration := s.connections.resolve(retirement.Owner); owner != nil {
		applied := owner.requestClose()
		s.traceRetirement(retirement.Owner, source, RetirementTargetOwner, result, currentGeneration, applied)
	} else {
		s.traceRetirement(retirement.Owner, source, RetirementTargetOwner, result, currentGeneration, false)
	}
}

func (s *Server) notifySessionRetired(peer *connection, sessionID v2.RelaySessionID) {
	if peer == nil {
		return
	}
	encoded, err := (v2.SessionRetired{RelaySessionID: sessionID}).MarshalBinary()
	if err != nil || !peer.enqueueControl(encoded) {
		// Without an exact retirement delivery, the client could retain an
		// unbounded runtime past the relay tombstone. Closing the link is the only
		// bounded fail-safe and lets its existing teardown close every channel.
		peer.requestClose()
	}
}

func (peer *connection) requestClose() bool {
	if peer == nil || peer.cancel == nil {
		return false
	}
	applied := false
	peer.cancelOnce.Do(func() {
		applied = true
		peer.cancel()
	})
	return applied
}

func (peer *connection) setRole(role connectionRole, share v2.ShareID) {
	peer.roleMu.Lock()
	peer.role, peer.share = role, share
	peer.roleMu.Unlock()
}

func (peer *connection) roleValue() connectionRole {
	peer.roleMu.Lock()
	defer peer.roleMu.Unlock()
	return peer.role
}

func (peer *connection) shareValue() v2.ShareID {
	peer.roleMu.Lock()
	defer peer.roleMu.Unlock()
	return peer.share
}

func (peer *connection) addSession(id v2.RelaySessionID) bool {
	peer.sessionMu.Lock()
	defer peer.sessionMu.Unlock()
	if peer.closed.Load() {
		return false
	}
	peer.sessions[id] = struct{}{}
	return true
}

func (peer *connection) removeSession(id v2.RelaySessionID) bool {
	peer.sessionMu.Lock()
	_, existed := peer.sessions[id]
	delete(peer.sessions, id)
	peer.forwardMu.Lock()
	if queue := peer.forward[id]; queue != nil {
		peer.forwardFrames -= len(queue.frames)
		peer.forwardBytes -= queue.bytes
		delete(peer.forward, id)
		for index, candidate := range peer.forwardOrder {
			if candidate == id {
				peer.forwardOrder = append(peer.forwardOrder[:index], peer.forwardOrder[index+1:]...)
				if peer.forwardCursor > index {
					peer.forwardCursor--
				}
				break
			}
		}
	}
	peer.forwardMu.Unlock()
	peer.sessionMu.Unlock()
	return existed
}

func (peer *connection) sessionIDs() []v2.RelaySessionID {
	peer.sessionMu.Lock()
	defer peer.sessionMu.Unlock()
	result := make([]v2.RelaySessionID, 0, len(peer.sessions))
	for id := range peer.sessions {
		result = append(result, id)
	}
	return result
}

func readBinary(ctx context.Context, socket BinaryConnection) ([]byte, error) {
	messageType, encoded, err := socket.Read(ctx)
	if err != nil {
		return nil, err
	}
	if messageType != websocket.MessageBinary || len(encoded) == 0 || len(encoded) > MaximumV2WebSocketMessageSize {
		return nil, ErrProtocol
	}
	return encoded, nil
}

func randomConnectionID() (v2route.ConnectionID, error) {
	var value [16]byte
	if _, err := io.ReadFull(rand.Reader, value[:]); err != nil {
		return "", err
	}
	return v2route.ConnectionID(fmt.Sprintf("%x", value)), nil
}

func normalClose(err error) bool {
	return err == nil || errors.Is(err, context.Canceled) ||
		errors.Is(err, io.ErrClosedPipe) ||
		websocket.CloseStatus(err) == websocket.StatusNormalClosure ||
		websocket.CloseStatus(err) == websocket.StatusGoingAway
}

func nonzero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return true
		}
	}
	return false
}
