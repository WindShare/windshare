package v2route

import (
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"time"
)

type SessionPhase string

const (
	SessionAwaitingReceiver SessionPhase = "awaiting_receiver"
	SessionAwaitingSender   SessionPhase = "awaiting_sender"
	SessionActive           SessionPhase = "active"
)

// ObserveReceiverFrame advances only the phase, never the deadline. Arbitrary
// traffic cannot renew provisional resource ownership.
func (r *Registry) ObserveReceiverFrame(id v2.RelaySessionID, receiver ConnectionRef) error {
	if r == nil || !receiver.Valid() {
		return ErrOwner
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	session, exists := r.sessions[id]
	if !exists || session.receiver != receiver {
		return ErrOwner
	}
	if session.phase == SessionActive {
		return nil
	}
	if !r.now().Before(session.admissionDeadline) {
		return ErrAdmissionExpired
	}
	session.phase = SessionAwaitingSender
	r.sessions[id] = session
	return nil
}

func (r *Registry) AdmitSession(id v2.RelaySessionID, sender ConnectionRef) error {
	if r == nil || !sender.Valid() {
		return ErrOwner
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	session, exists := r.sessions[id]
	if !exists {
		if ended, ok := r.sessionTombstones[id]; ok && ended.session.sender == sender && r.now().Before(ended.expiresAt) {
			return ErrSessionEnded
		}
		return ErrOwner
	}
	if session.sender != sender {
		return ErrOwner
	}
	if session.phase == SessionActive {
		return nil
	}
	if !r.now().Before(session.admissionDeadline) {
		return ErrAdmissionExpired
	}
	if session.phase != SessionAwaitingSender {
		return ErrSession
	}
	session.phase = SessionActive
	session.admissionDeadline = time.Time{}
	// Active sessions have no traffic-idle timeout: browsing and direct P2P
	// transfer can legitimately leave this relay channel silent.
	r.sessions[id] = session
	return nil
}

// ExpireAdmission competes with AdmitSession under the same authority lock.
// A stale timer cannot retire a different connection lifetime or an active peer.
func (r *Registry) ExpireAdmission(id v2.RelaySessionID, receiver ConnectionRef) (SessionRetirement, SessionPhase, bool) {
	if r == nil || !receiver.Valid() {
		return SessionRetirement{}, "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	session, exists := r.sessions[id]
	if !exists || session.receiver != receiver || session.phase == SessionActive ||
		r.now().Before(session.admissionDeadline) {
		return SessionRetirement{}, "", false
	}
	return r.retireSession(id, session), session.phase, true
}
