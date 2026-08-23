package revisioncapacity

import (
	"errors"
	"fmt"
)

type AdmissionGrant struct {
	state *admissionState
}

func (g AdmissionGrant) DecisionID() CapacityDecisionID {
	if g.state == nil {
		return ""
	}
	return g.state.decisionID
}

func (g AdmissionGrant) ReclaimDiagnostic() error {
	if g.state == nil {
		return nil
	}
	return g.state.reclaimDiagnostic
}

func (g AdmissionGrant) Commit() (AdmissionCharges, error) {
	if g.state == nil || g.state.coordinator == nil {
		return AdmissionCharges{}, errors.New("revision capacity admission grant is empty")
	}
	state := g.state
	c := state.coordinator
	c.mu.Lock()
	defer c.mu.Unlock()
	if state.settled {
		return AdmissionCharges{}, ErrAdmissionGrantSettled
	}
	if state.requirements.stableHandle && !state.stableReserved {
		return AdmissionCharges{}, errors.New("revision capacity grant lost its stable-handle reservation")
	}
	state.settled = true
	state.store.pending--
	state.session.pending--
	c.pending--
	charges := AdmissionCharges{}
	if state.requirements.stableHandle {
		charge := c.newChargeLocked(state.store, nil, chargeStableHandle)
		charges.stable = StableHandleCharge{state: charge}
	}
	if state.requirements.activeLease {
		charge := c.newChargeLocked(state.store, state.session, chargeActiveLease)
		charges.lease = ActiveLeaseCharge{state: charge}
	}
	if state.requirements.sessionHandle {
		charge := c.newChargeLocked(state.store, state.session, chargeSessionHandle)
		charges.sessionHandle = SessionHandleCharge{state: charge}
	}
	c.cond.Broadcast()
	return charges, nil
}

func (g AdmissionGrant) Abort() error {
	if g.state == nil || g.state.coordinator == nil {
		return errors.New("revision capacity admission grant is empty")
	}
	state := g.state
	c := state.coordinator
	c.mu.Lock()
	defer c.mu.Unlock()
	if state.settled {
		return ErrAdmissionGrantSettled
	}
	c.rollbackAdmissionLocked(state)
	return nil
}

// QuarantineStableHandle resolves a provisional new-revision admission after a
// backend acquired a stable source but panicked before terminal Close could be
// proved. Active/session dimensions are rolled back; the physical unit remains
// unavailable and observable as quarantined.
func (g AdmissionGrant) QuarantineStableHandle(cause error) error {
	if g.state == nil || g.state.coordinator == nil {
		return errors.New("revision capacity admission grant is empty")
	}
	state := g.state
	c := state.coordinator
	c.mu.Lock()
	var trace *TraceEvent
	defer func() {
		c.mu.Unlock()
		if trace != nil {
			c.trace(*trace)
		}
	}()
	if state.settled {
		return ErrAdmissionGrantSettled
	}
	if !state.requirements.stableHandle || !state.stableReserved {
		return errors.New("revision capacity admission has no stable handle to quarantine")
	}
	if cause == nil {
		cause = ErrInvalidReclaimResult
	}
	// The stable process/share reservation deliberately remains in used counts.
	state.stableReserved = false
	c.quarantined++
	state.store.quarantined++
	if state.requirements.activeLease {
		c.used.ActiveLeases--
		state.store.used.ActiveLeases--
		state.session.used.ActiveLeases--
	}
	if state.requirements.sessionHandle {
		state.session.used.StableHandles--
	}
	state.settled = true
	state.store.pending--
	state.session.pending--
	c.pending--
	c.cond.Broadcast()
	event := TraceEvent{
		stage: TraceOwnershipQuarantined, decision: state.decisionID,
		storeID: state.store.storeID, shareID: state.store.shareID, sessionID: state.session.sessionID,
		revision: state.revisionID, snapshot: c.decisionSnapshotLocked(state.store, state.session), diagnostic: cause,
	}
	trace = &event
	return nil
}

type chargeKind uint8

const (
	chargeStableHandle chargeKind = iota + 1
	chargeActiveLease
	chargeSessionHandle
)

type chargeStatus uint8

const (
	chargeActive chargeStatus = iota + 1
	chargeReleased
	chargeTransferred
	chargeQuarantined
)

type chargeState struct {
	coordinator *Coordinator
	store       *storeState
	session     *sessionState
	kind        chargeKind
	status      chargeStatus
}

func (c *Coordinator) newChargeLocked(store *storeState, session *sessionState, kind chargeKind) *chargeState {
	charge := &chargeState{coordinator: c, store: store, session: session, kind: kind, status: chargeActive}
	store.liveCharges++
	if session != nil {
		session.liveCharges++
	}
	return charge
}

type StableHandleCharge struct{ state *chargeState }
type ActiveLeaseCharge struct{ state *chargeState }
type SessionHandleCharge struct{ state *chargeState }

func (c StableHandleCharge) Valid() bool  { return c.state != nil }
func (c ActiveLeaseCharge) Valid() bool   { return c.state != nil }
func (c SessionHandleCharge) Valid() bool { return c.state != nil }

func (c StableHandleCharge) Release() error  { return releaseCharge(c.state, chargeStableHandle) }
func (c ActiveLeaseCharge) Release() error   { return releaseCharge(c.state, chargeActiveLease) }
func (c SessionHandleCharge) Release() error { return releaseCharge(c.state, chargeSessionHandle) }

type AdmissionCharges struct {
	stable        StableHandleCharge
	lease         ActiveLeaseCharge
	sessionHandle SessionHandleCharge
}

func (c AdmissionCharges) StableHandle() (StableHandleCharge, bool) {
	return c.stable, c.stable.Valid()
}

func (c AdmissionCharges) ActiveLease() (ActiveLeaseCharge, bool) {
	return c.lease, c.lease.Valid()
}

func (c AdmissionCharges) SessionHandle() (SessionHandleCharge, bool) {
	return c.sessionHandle, c.sessionHandle.Valid()
}

func (c AdmissionCharges) Release() error {
	var joined error
	if c.lease.Valid() {
		joined = errors.Join(joined, c.lease.Release())
	}
	if c.sessionHandle.Valid() {
		joined = errors.Join(joined, c.sessionHandle.Release())
	}
	if c.stable.Valid() {
		joined = errors.Join(joined, c.stable.Release())
	}
	return joined
}

func releaseCharge(state *chargeState, expected chargeKind) error {
	if state == nil || state.coordinator == nil {
		return errors.New("revision capacity charge is empty")
	}
	c := state.coordinator
	c.mu.Lock()
	defer c.mu.Unlock()
	if state.kind != expected {
		return errors.New("revision capacity charge kind mismatch")
	}
	switch state.status {
	case chargeReleased, chargeTransferred:
		return ErrOwnershipResolved
	case chargeQuarantined:
		return ErrOwnershipQuarantined
	case chargeActive:
	default:
		return errors.New("revision capacity charge has an invalid state")
	}
	if candidate := c.byCharge[state]; candidate != nil {
		if candidate.claim != nil {
			return ErrOwnershipClaimed
		}
		c.removeAvailableCandidateLocked(candidate)
	}
	switch state.kind {
	case chargeStableHandle:
		c.used.StableHandles--
		state.store.used.StableHandles--
	case chargeActiveLease:
		c.used.ActiveLeases--
		state.store.used.ActiveLeases--
		state.session.used.ActiveLeases--
	case chargeSessionHandle:
		state.session.used.StableHandles--
	default:
		return fmt.Errorf("revision capacity charge kind %d is invalid", state.kind)
	}
	state.status = chargeReleased
	state.store.liveCharges--
	if state.session != nil {
		state.session.liveCharges--
	}
	c.cond.Broadcast()
	return nil
}

// Quarantine records a stable handle whose terminal ownership cannot be
// proved. The unit remains unavailable at process/share scope, while the charge
// token is resolved so registration teardown cannot wait forever on a backend
// contract failure.
func (c StableHandleCharge) Quarantine(cause error) error {
	if c.state == nil || c.state.coordinator == nil {
		return errors.New("revision capacity stable-handle charge is empty")
	}
	state := c.state
	coordinator := state.coordinator
	coordinator.mu.Lock()
	var trace *TraceEvent
	defer func() {
		coordinator.mu.Unlock()
		if trace != nil {
			coordinator.trace(*trace)
		}
	}()
	if state.kind != chargeStableHandle {
		return errors.New("revision capacity charge kind mismatch")
	}
	if cause == nil {
		cause = ErrInvalidReclaimResult
	}
	switch state.status {
	case chargeReleased, chargeTransferred:
		return ErrOwnershipResolved
	case chargeQuarantined:
		return ErrOwnershipQuarantined
	case chargeActive:
	default:
		return errors.New("revision capacity charge has an invalid state")
	}
	if candidate := coordinator.byCharge[state]; candidate != nil {
		if candidate.claim != nil {
			return ErrOwnershipClaimed
		}
		coordinator.removeAvailableCandidateLocked(candidate)
	}
	state.status = chargeQuarantined
	state.store.liveCharges--
	state.store.quarantined++
	coordinator.quarantined++
	coordinator.cond.Broadcast()
	event := TraceEvent{
		stage: TraceOwnershipQuarantined, storeID: state.store.storeID, shareID: state.store.shareID,
		snapshot: coordinator.decisionSnapshotLocked(state.store, nil), diagnostic: cause,
	}
	trace = &event
	return nil
}
