package revisioncapacity

import (
	"errors"
	"fmt"
)

type StoreRegistration struct {
	coordinator *Coordinator
	state       *storeState
}

func (s *StoreRegistration) StoreID() StoreID {
	if s == nil || s.state == nil {
		return ""
	}
	return s.state.storeID
}

func (s *StoreRegistration) ShareID() ShareID {
	if s == nil || s.state == nil {
		return ""
	}
	return s.state.shareID
}

func (s *StoreRegistration) RegisterSession(config SessionConfig) (*SessionRegistration, error) {
	if s == nil || s.coordinator == nil || s.state == nil {
		return nil, errors.New("revision capacity session registration requires a store registration")
	}
	if config.SessionID == "" {
		return nil, errors.New("revision capacity session registration requires a session identity")
	}
	if err := validateLimits(config.Limits); err != nil {
		return nil, err
	}
	c := s.coordinator
	c.mu.Lock()
	defer c.mu.Unlock()
	store := s.state
	if c.closed {
		return nil, ErrCoordinatorClosed
	}
	if store.closing || store.closed {
		return nil, ErrRegistrationClosing
	}
	if _, exists := store.sessions[config.SessionID]; exists {
		return nil, fmt.Errorf("revision capacity session identity %q is already registered in store %q", config.SessionID, store.storeID)
	}
	state := &sessionState{store: store, sessionID: config.SessionID, limits: config.Limits}
	registration := &SessionRegistration{coordinator: c, state: state}
	store.sessions[state.sessionID] = state
	return registration, nil
}

func (s *StoreRegistration) Snapshot() CapacitySnapshot {
	if s == nil || s.coordinator == nil || s.state == nil {
		return CapacitySnapshot{}
	}
	s.coordinator.mu.Lock()
	defer s.coordinator.mu.Unlock()
	return s.coordinator.snapshotLocked(s.state)
}

// WaitForReclaims joins callbacks after the caller has fenced local lifecycle
// state and withdrawn its candidates. This lets a store prove that no callback
// can still own a claimed stable handle before ordinary terminal cleanup.
func (s *StoreRegistration) WaitForReclaims() error {
	if s == nil || s.coordinator == nil || s.state == nil {
		return errors.New("revision capacity reclaim wait requires a store registration")
	}
	c := s.coordinator
	c.mu.Lock()
	defer c.mu.Unlock()
	for s.state.reclaims != 0 {
		c.cond.Wait()
	}
	return nil
}

func (s *StoreRegistration) Close() error {
	if s == nil || s.coordinator == nil || s.state == nil {
		return nil
	}
	c := s.coordinator
	c.mu.Lock()
	store := s.state
	if store.closed {
		c.mu.Unlock()
		return nil
	}
	store.closing = true
	c.withdrawStoreCandidatesLocked(store)
	for store.pending != 0 || store.reclaims != 0 {
		c.cond.Wait()
	}
	if len(store.sessions) != 0 {
		err := &LiveSessionRegistrationsError{storeID: store.storeID, count: len(store.sessions)}
		c.mu.Unlock()
		return err
	}
	if store.liveCharges != 0 {
		err := &LiveCapacityOwnershipError{
			scope: CapacityScopeShare, identity: string(store.shareID), usage: store.used,
		}
		c.mu.Unlock()
		return err
	}
	store.closed = true
	delete(c.stores, store.storeID)
	delete(c.shares, store.shareID)
	c.cond.Broadcast()
	c.mu.Unlock()
	return nil
}

type SessionRegistration struct {
	coordinator *Coordinator
	state       *sessionState
}

func (s *SessionRegistration) SessionID() SessionID {
	if s == nil || s.state == nil {
		return ""
	}
	return s.state.sessionID
}

func (s *SessionRegistration) Snapshot() ScopeSnapshot {
	if s == nil || s.coordinator == nil || s.state == nil {
		return ScopeSnapshot{}
	}
	c := s.coordinator
	c.mu.Lock()
	defer c.mu.Unlock()
	state := s.state
	return ScopeSnapshot{
		scope: CapacityScopeSession, identity: string(state.sessionID), limits: state.limits,
		used: state.used, pending: state.pending,
	}
}

func (s *SessionRegistration) Close() error {
	if s == nil || s.coordinator == nil || s.state == nil {
		return nil
	}
	c := s.coordinator
	c.mu.Lock()
	session := s.state
	if session.closed {
		c.mu.Unlock()
		return nil
	}
	session.closing = true
	for session.pending != 0 {
		c.cond.Wait()
	}
	if session.liveCharges != 0 {
		err := &LiveCapacityOwnershipError{
			scope: CapacityScopeSession, identity: string(session.sessionID), usage: session.used,
		}
		c.mu.Unlock()
		return err
	}
	session.closed = true
	delete(session.store.sessions, session.sessionID)
	c.cond.Broadcast()
	c.mu.Unlock()
	return nil
}

func (c *Coordinator) withdrawStoreCandidatesLocked(store *storeState) {
	for _, candidate := range c.candidates {
		if candidate.store != store {
			continue
		}
		if candidate.claim != nil {
			candidate.withdrawn = true
			continue
		}
		c.removeAvailableCandidateLocked(candidate)
	}
}
