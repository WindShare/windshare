package revisioncapacity

import (
	"context"
	"errors"
)

type admissionRequirements struct {
	stableHandle  bool
	activeLease   bool
	sessionHandle bool
}

func requirementsFor(kind AdmissionKind) (admissionRequirements, error) {
	switch kind {
	case AdmissionNewRevision:
		return admissionRequirements{stableHandle: true, activeLease: true, sessionHandle: true}, nil
	case AdmissionFirstSessionLease:
		return admissionRequirements{activeLease: true, sessionHandle: true}, nil
	case AdmissionAdditionalSessionLease:
		return admissionRequirements{activeLease: true}, nil
	default:
		return admissionRequirements{}, errors.New("revision capacity admission kind is invalid")
	}
}

type admissionState struct {
	coordinator       *Coordinator
	store             *storeState
	session           *sessionState
	decisionID        CapacityDecisionID
	revisionID        RevisionID
	requirements      admissionRequirements
	stableReserved    bool
	settled           bool
	reclaimDiagnostic error
}

func (s *StoreRegistration) Admit(ctx context.Context, request AdmissionRequest) (AdmissionGrant, error) {
	if ctx == nil {
		return AdmissionGrant{}, errors.New("revision capacity admission requires a context")
	}
	if err := ctx.Err(); err != nil {
		return AdmissionGrant{}, err
	}
	if s == nil || s.coordinator == nil || s.state == nil {
		return AdmissionGrant{}, errors.New("revision capacity admission requires a store registration")
	}
	if request.RevisionID == "" || request.Session == nil || request.Session.state == nil {
		return AdmissionGrant{}, errors.New("revision capacity admission requires revision and session identities")
	}
	requirements, err := requirementsFor(request.Kind)
	if err != nil {
		return AdmissionGrant{}, err
	}
	run, err := s.beginAdmission(ctx, request, requirements)
	if err != nil {
		return AdmissionGrant{}, err
	}
	return run.admit()
}

type admissionRun struct {
	ctx   context.Context
	state *admissionState
}

type pendingReclaim struct {
	candidate                  *candidateState
	claim                      *reclaimState
	crossStoreShareReservation bool
}

func (s *StoreRegistration) beginAdmission(
	ctx context.Context,
	request AdmissionRequest,
	requirements admissionRequirements,
) (*admissionRun, error) {
	c := s.coordinator
	c.mu.Lock()
	store := s.state
	session := request.Session.state
	if request.Session.coordinator != c || session.store != store {
		c.mu.Unlock()
		return nil, errors.New("revision capacity admission session belongs to another store")
	}
	if c.closed {
		c.mu.Unlock()
		return nil, ErrCoordinatorClosed
	}
	if store.closing || store.closed || session.closing || session.closed {
		c.mu.Unlock()
		return nil, ErrRegistrationClosing
	}
	decisionID := c.nextDecisionIDLocked()
	if resource, scope, blocked := c.nonStableBoundaryLocked(store, session, requirements); blocked {
		busy := c.busyLocked(decisionID, resource, scope, store, session)
		event := c.traceForLocked(TraceAdmissionDenied, decisionID, nil, store, session, request.RevisionID, "", busy)
		c.mu.Unlock()
		c.trace(event)
		return nil, busy
	}
	store.pending++
	session.pending++
	c.pending++
	c.reserveNonStableLocked(store, session, requirements)
	state := &admissionState{
		coordinator: c, store: store, session: session, decisionID: decisionID,
		revisionID: request.RevisionID, requirements: requirements,
	}
	c.mu.Unlock()
	return &admissionRun{ctx: ctx, state: state}, nil
}

func (run *admissionRun) admit() (AdmissionGrant, error) {
	for {
		grant, pending, err := run.claimOrResolve()
		if pending == nil {
			return grant, err
		}
		if err := run.cancelBeforeReclaim(pending); err != nil {
			return AdmissionGrant{}, err
		}
		result := invokeReclaimTarget(
			run.ctx,
			pending.candidate.store.target,
			ReclaimClaim{state: pending.claim},
		)
		retry, grant, err := run.finishReclaim(pending, result)
		if retry {
			continue
		}
		return grant, err
	}
}

func (run *admissionRun) claimOrResolve() (AdmissionGrant, *pendingReclaim, error) {
	state := run.state
	c := state.coordinator
	c.mu.Lock()
	if stopErr := admissionStopError(run.ctx, state.store, state.session); stopErr != nil {
		c.rollbackAdmissionLocked(state)
		event := c.traceForLocked(
			TraceAdmissionCancelled, state.decisionID, nil, state.store, state.session,
			state.revisionID, "", stopErr,
		)
		c.mu.Unlock()
		c.trace(event)
		return AdmissionGrant{}, nil, stopErr
	}
	shareBlocked := state.requirements.stableHandle &&
		state.store.used.StableHandles >= state.store.limits.StableHandles
	processBlocked := state.requirements.stableHandle && c.used.StableHandles >= c.limits.StableHandles
	if !state.requirements.stableHandle || !shareBlocked && !processBlocked {
		return run.grantAvailableLocked(), nil, nil
	}
	candidate := c.oldestCandidateLocked(state.store, shareBlocked)
	if candidate == nil {
		return AdmissionGrant{}, nil, run.denyStableLocked(shareBlocked)
	}
	claim := c.claimCandidateLocked(state.decisionID, state.store, candidate)
	crossStoreShareReservation := candidate.store != state.store
	if crossStoreShareReservation {
		// The process unit stays charged to the victim; only the requester's
		// independent share dimension is provisional while Close runs.
		state.store.used.StableHandles++
	}
	event := c.traceForLocked(
		TraceReclaimClaimed, state.decisionID, claim, state.store, state.session,
		state.revisionID, candidate.token, nil,
	)
	c.mu.Unlock()
	c.trace(event)
	return AdmissionGrant{}, &pendingReclaim{
		candidate: candidate, claim: claim,
		crossStoreShareReservation: crossStoreShareReservation,
	}, nil
}

func (run *admissionRun) grantAvailableLocked() AdmissionGrant {
	state := run.state
	c := state.coordinator
	if state.requirements.stableHandle {
		c.used.StableHandles++
		state.store.used.StableHandles++
		state.stableReserved = true
	}
	grant := AdmissionGrant{state: state}
	event := c.traceForLocked(
		TraceAdmissionGranted, state.decisionID, nil, state.store, state.session,
		state.revisionID, "", nil,
	)
	c.mu.Unlock()
	c.trace(event)
	return grant
}

func (run *admissionRun) denyStableLocked(shareBlocked bool) error {
	state := run.state
	c := state.coordinator
	scope := CapacityScopeProcess
	if shareBlocked {
		scope = CapacityScopeShare
	}
	c.rollbackAdmissionLocked(state)
	busy := c.busyLocked(state.decisionID, CapacityResourceStableHandle, scope, state.store, state.session)
	event := c.traceForLocked(
		TraceAdmissionDenied, state.decisionID, nil, state.store, state.session,
		state.revisionID, "", busy,
	)
	c.mu.Unlock()
	c.trace(event)
	return busy
}

func (run *admissionRun) cancelBeforeReclaim(pending *pendingReclaim) error {
	state := run.state
	c := state.coordinator
	c.mu.Lock()
	stopErr := admissionStopError(run.ctx, state.store, state.session)
	if stopErr == nil {
		c.mu.Unlock()
		return nil
	}
	c.abandonClaimLocked(
		pending.candidate,
		pending.claim,
		pending.crossStoreShareReservation,
	)
	c.rollbackAdmissionLocked(state)
	event := c.traceForLocked(
		TraceAdmissionCancelled, state.decisionID, pending.claim, state.store, state.session,
		state.revisionID, pending.candidate.token, stopErr,
	)
	c.mu.Unlock()
	c.trace(event)
	return stopErr
}

func (run *admissionRun) finishReclaim(
	pending *pendingReclaim,
	result ReclaimResult,
) (bool, AdmissionGrant, error) {
	state := run.state
	c := state.coordinator
	c.mu.Lock()
	outcome := c.finishClaimLocked(
		pending.candidate,
		pending.claim,
		result,
		state,
		pending.crossStoreShareReservation,
	)
	switch outcome.kind {
	case reclaimFinishDeclined:
		return run.finishDeclinedLocked(pending, result)
	case reclaimFinishCompleted:
		return run.finishCompletedLocked(pending, result)
	case reclaimFinishUncertain:
		return run.finishUncertainLocked(pending, outcome.cause)
	default:
		panic("revision capacity reclaim produced an invalid finish outcome")
	}
}

func (run *admissionRun) finishDeclinedLocked(
	pending *pendingReclaim,
	result ReclaimResult,
) (bool, AdmissionGrant, error) {
	state := run.state
	c := state.coordinator
	declinedEvent := c.traceForLocked(
		TraceReclaimDeclined, state.decisionID, pending.claim, state.store, state.session,
		state.revisionID, pending.candidate.token, result.Diagnostic(),
	)
	if stopErr := admissionStopError(run.ctx, state.store, state.session); stopErr != nil {
		c.rollbackAdmissionLocked(state)
		cancelledEvent := c.traceForLocked(
			TraceAdmissionCancelled, state.decisionID, pending.claim, state.store, state.session,
			state.revisionID, pending.candidate.token, stopErr,
		)
		c.mu.Unlock()
		c.trace(declinedEvent)
		c.trace(cancelledEvent)
		return false, AdmissionGrant{}, stopErr
	}
	c.mu.Unlock()
	c.trace(declinedEvent)
	return true, AdmissionGrant{}, nil
}

func (run *admissionRun) finishCompletedLocked(
	pending *pendingReclaim,
	result ReclaimResult,
) (bool, AdmissionGrant, error) {
	state := run.state
	c := state.coordinator
	completedEvent := c.traceForLocked(
		TraceReclaimCompleted, state.decisionID, pending.claim, state.store, state.session,
		state.revisionID, pending.candidate.token, result.Diagnostic(),
	)
	if stopErr := admissionStopError(run.ctx, state.store, state.session); stopErr != nil {
		c.releaseTerminalVictimLocked(pending.candidate, state, pending.crossStoreShareReservation)
		c.rollbackAdmissionLocked(state)
		cancelledEvent := c.traceForLocked(
			TraceAdmissionCancelled, state.decisionID, pending.claim, state.store, state.session,
			state.revisionID, pending.candidate.token, stopErr,
		)
		c.mu.Unlock()
		c.trace(completedEvent)
		c.trace(cancelledEvent)
		return false, AdmissionGrant{}, stopErr
	}
	c.transferTerminalVictimLocked(pending.candidate, state, pending.crossStoreShareReservation)
	state.reclaimDiagnostic = result.Diagnostic()
	grant := AdmissionGrant{state: state}
	grantedEvent := c.traceForLocked(
		TraceAdmissionGranted, state.decisionID, pending.claim, state.store, state.session,
		state.revisionID, pending.candidate.token, result.Diagnostic(),
	)
	c.mu.Unlock()
	c.trace(completedEvent)
	c.trace(grantedEvent)
	return false, grant, nil
}

func (run *admissionRun) finishUncertainLocked(
	pending *pendingReclaim,
	cause error,
) (bool, AdmissionGrant, error) {
	state := run.state
	c := state.coordinator
	c.quarantineVictimLocked(pending.candidate, state, pending.crossStoreShareReservation)
	c.rollbackAdmissionLocked(state)
	ownershipErr := &ReclaimOwnershipError{
		decisionID: state.decisionID,
		claimID:    pending.claim.id,
		candidate:  pending.candidate.token,
		cause:      cause,
	}
	event := c.traceForLocked(
		TraceReclaimQuarantined, state.decisionID, pending.claim, state.store, state.session,
		state.revisionID, pending.candidate.token, ownershipErr,
	)
	c.mu.Unlock()
	c.trace(event)
	return false, AdmissionGrant{}, ownershipErr
}

func admissionStopError(ctx context.Context, store *storeState, session *sessionState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if store.closing || store.closed || session.closing || session.closed {
		return ErrRegistrationClosing
	}
	return nil
}

func (c *Coordinator) nonStableBoundaryLocked(store *storeState, session *sessionState, requirements admissionRequirements) (CapacityResource, CapacityScope, bool) {
	if requirements.activeLease {
		switch {
		case session.used.ActiveLeases >= session.limits.ActiveLeases:
			return CapacityResourceActiveLease, CapacityScopeSession, true
		case store.used.ActiveLeases >= store.limits.ActiveLeases:
			return CapacityResourceActiveLease, CapacityScopeShare, true
		case c.used.ActiveLeases >= c.limits.ActiveLeases:
			return CapacityResourceActiveLease, CapacityScopeProcess, true
		}
	}
	if requirements.sessionHandle && session.used.StableHandles >= session.limits.StableHandles {
		return CapacityResourceStableHandle, CapacityScopeSession, true
	}
	return 0, 0, false
}

func (c *Coordinator) reserveNonStableLocked(store *storeState, session *sessionState, requirements admissionRequirements) {
	if requirements.activeLease {
		c.used.ActiveLeases++
		store.used.ActiveLeases++
		session.used.ActiveLeases++
	}
	if requirements.sessionHandle {
		session.used.StableHandles++
	}
}

func (c *Coordinator) rollbackAdmissionLocked(state *admissionState) {
	if state == nil || state.settled {
		return
	}
	if state.stableReserved {
		c.used.StableHandles--
		state.store.used.StableHandles--
		state.stableReserved = false
	}
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
}

func (c *Coordinator) busyLocked(decision CapacityDecisionID, resource CapacityResource, scope CapacityScope, store *storeState, session *sessionState) *CapacityBusyError {
	return &CapacityBusyError{
		decisionID: decision, resource: resource, scope: scope, retryAfter: c.retryAfter,
		snapshot: c.decisionSnapshotLocked(store, session),
	}
}

func (c *Coordinator) traceForLocked(stage TraceStage, decision CapacityDecisionID, claim *reclaimState, store *storeState, session *sessionState, revision RevisionID, candidate CandidateToken, diagnostic error) TraceEvent {
	event := TraceEvent{
		stage: stage, decision: decision, storeID: store.storeID, shareID: store.shareID,
		sessionID: session.sessionID, revision: revision, candidate: candidate,
		snapshot: c.decisionSnapshotLocked(store, session), diagnostic: diagnostic,
	}
	if claim != nil {
		event.claim = claim.id
	}
	return event
}
