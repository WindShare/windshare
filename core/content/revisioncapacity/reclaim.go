package revisioncapacity

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"
)

// ReclaimTarget retains store lifecycle authority. It must return Declined if
// the claim became stale before logical detachment. Once detached, it must finish
// Close and return Completed or OwnershipUncertain even if ctx is cancelled.
type ReclaimTarget interface {
	ReclaimIdle(context.Context, ReclaimClaim) ReclaimResult
}

type IdleCandidate struct {
	Token               CandidateToken
	RevisionID          RevisionID
	RecoveryUntil       time.Time
	LifecycleGeneration uint64
	StableHandle        StableHandleCharge
}

type candidateKey struct {
	storeID StoreID
	token   CandidateToken
}

type candidateState struct {
	key                 candidateKey
	store               *storeState
	token               CandidateToken
	revisionID          RevisionID
	recoveryUntil       time.Time
	lifecycleGeneration uint64
	charge              *chargeState
	claim               *reclaimState
	withdrawn           bool
}

func (s *StoreRegistration) PublishIdle(candidate IdleCandidate) error {
	if s == nil || s.coordinator == nil || s.state == nil {
		return errors.New("revision capacity idle publication requires a store registration")
	}
	if candidate.Token == "" || candidate.RevisionID == "" || candidate.RecoveryUntil.IsZero() ||
		candidate.LifecycleGeneration == 0 || candidate.StableHandle.state == nil {
		return errors.New("revision capacity idle candidate is incomplete")
	}
	c := s.coordinator
	c.mu.Lock()
	var trace *TraceEvent
	defer func() {
		c.mu.Unlock()
		if trace != nil {
			c.trace(*trace)
		}
	}()
	store := s.state
	if c.closed {
		return ErrCoordinatorClosed
	}
	if store.closing || store.closed {
		return ErrRegistrationClosing
	}
	charge := candidate.StableHandle.state
	if charge.coordinator != c || charge.store != store || charge.kind != chargeStableHandle || charge.status != chargeActive {
		return errors.New("revision capacity idle candidate does not own an active stable handle for this store")
	}
	key := candidateKey{storeID: store.storeID, token: candidate.Token}
	if existing := c.candidates[key]; existing != nil {
		if existing.charge == charge && existing.revisionID == candidate.RevisionID &&
			existing.recoveryUntil.Equal(candidate.RecoveryUntil) &&
			existing.lifecycleGeneration == candidate.LifecycleGeneration && !existing.withdrawn {
			return nil
		}
		return errors.New("revision capacity idle candidate token was reused for different state")
	}
	if c.byCharge[charge] != nil {
		return errors.New("revision capacity stable handle already has an idle candidate")
	}
	state := &candidateState{
		key: key, store: store, token: candidate.Token, revisionID: candidate.RevisionID,
		recoveryUntil: candidate.RecoveryUntil, lifecycleGeneration: candidate.LifecycleGeneration,
		charge: charge,
	}
	c.candidates[key] = state
	c.byCharge[charge] = state
	c.reclaimable++
	store.reclaimable++
	event := TraceEvent{
		stage: TraceIdlePublished, storeID: store.storeID, shareID: store.shareID,
		revision: candidate.RevisionID, candidate: candidate.Token, snapshot: c.decisionSnapshotLocked(store, nil),
	}
	trace = &event
	return nil
}

func (s *StoreRegistration) WithdrawIdle(token CandidateToken) bool {
	if s == nil || s.coordinator == nil || s.state == nil || token == "" {
		return false
	}
	c := s.coordinator
	c.mu.Lock()
	key := candidateKey{storeID: s.state.storeID, token: token}
	candidate := c.candidates[key]
	if candidate == nil {
		c.mu.Unlock()
		return false
	}
	if candidate.claim != nil {
		// The callback owns resolution until it proves either staleness or
		// terminal Close; withdrawal prevents reuse but cannot steal that proof.
		candidate.withdrawn = true
		event := TraceEvent{
			stage: TraceIdleWithdrawn, storeID: candidate.store.storeID, shareID: candidate.store.shareID,
			revision: candidate.revisionID, candidate: candidate.token, snapshot: c.decisionSnapshotLocked(candidate.store, nil),
		}
		c.mu.Unlock()
		c.trace(event)
		return true
	}
	c.removeAvailableCandidateLocked(candidate)
	event := TraceEvent{
		stage: TraceIdleWithdrawn, storeID: candidate.store.storeID, shareID: candidate.store.shareID,
		revision: candidate.revisionID, candidate: candidate.token, snapshot: c.decisionSnapshotLocked(candidate.store, nil),
	}
	c.mu.Unlock()
	c.trace(event)
	return true
}

func (c *Coordinator) removeAvailableCandidateLocked(candidate *candidateState) {
	if candidate == nil {
		return
	}
	delete(c.candidates, candidate.key)
	delete(c.byCharge, candidate.charge)
	if candidate.claim == nil && !candidate.withdrawn && candidate.charge.status == chargeActive {
		c.reclaimable--
		candidate.store.reclaimable--
	}
}

type reclaimState struct {
	id         ReclaimClaimID
	decisionID CapacityDecisionID
	candidate  *candidateState
	requester  *storeState
}

type ReclaimClaim struct {
	state *reclaimState
}

func (c ReclaimClaim) ClaimID() ReclaimClaimID {
	if c.state == nil {
		return ""
	}
	return c.state.id
}

func (c ReclaimClaim) DecisionID() CapacityDecisionID {
	if c.state == nil {
		return ""
	}
	return c.state.decisionID
}

func (c ReclaimClaim) CandidateToken() CandidateToken {
	if c.state == nil || c.state.candidate == nil {
		return ""
	}
	return c.state.candidate.token
}

func (c ReclaimClaim) RevisionID() RevisionID {
	if c.state == nil || c.state.candidate == nil {
		return ""
	}
	return c.state.candidate.revisionID
}

func (c ReclaimClaim) RecoveryUntil() time.Time {
	if c.state == nil || c.state.candidate == nil {
		return time.Time{}
	}
	return c.state.candidate.recoveryUntil
}

func (c ReclaimClaim) LifecycleGeneration() uint64 {
	if c.state == nil || c.state.candidate == nil {
		return 0
	}
	return c.state.candidate.lifecycleGeneration
}

type reclaimResultKind uint8

const (
	reclaimResultDeclined reclaimResultKind = iota + 1
	reclaimResultCompleted
	reclaimResultUncertain
)

type ReclaimResult struct {
	claim      *reclaimState
	kind       reclaimResultKind
	diagnostic error
}

// ReclaimDeclined asserts that the target retained ownership because the store
// generation no longer matched the claim.
func ReclaimDeclined(claim ReclaimClaim) ReclaimResult {
	return ReclaimResult{claim: claim.state, kind: reclaimResultDeclined}
}

// ReclaimCompleted asserts that StableFile.Close returned and ownership is
// terminal. A diagnostic Close error does not weaken that ownership proof.
func ReclaimCompleted(claim ReclaimClaim, diagnostic error) ReclaimResult {
	return ReclaimResult{claim: claim.state, kind: reclaimResultCompleted, diagnostic: diagnostic}
}

// ReclaimOwnershipUncertain reports that the target cannot prove whether stable
// ownership became terminal. The coordinator quarantines rather than grants it.
func ReclaimOwnershipUncertain(claim ReclaimClaim, cause error) ReclaimResult {
	if cause == nil {
		cause = ErrInvalidReclaimResult
	}
	return ReclaimResult{claim: claim.state, kind: reclaimResultUncertain, diagnostic: cause}
}

func (r ReclaimResult) Diagnostic() error { return r.diagnostic }

func invokeReclaimTarget(ctx context.Context, target ReclaimTarget, claim ReclaimClaim) (result ReclaimResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = ReclaimOwnershipUncertain(
				claim,
				fmt.Errorf("reclaim target panicked: %v\n%s", recovered, debug.Stack()),
			)
		}
	}()
	return target.ReclaimIdle(ctx, claim)
}

func (c *Coordinator) oldestCandidateLocked(requester *storeState, shareBlocked bool) *candidateState {
	var oldest *candidateState
	for _, candidate := range c.candidates {
		if candidate.claim != nil || candidate.withdrawn || candidate.store.closing || candidate.store.closed ||
			candidate.charge.status != chargeActive {
			if candidate.claim == nil {
				c.removeAvailableCandidateLocked(candidate)
			}
			continue
		}
		if shareBlocked && candidate.store.shareID != requester.shareID {
			continue
		}
		if oldest == nil || candidateLess(candidate, oldest) {
			oldest = candidate
		}
	}
	return oldest
}

func candidateLess(left, right *candidateState) bool {
	if !left.recoveryUntil.Equal(right.recoveryUntil) {
		return left.recoveryUntil.Before(right.recoveryUntil)
	}
	if left.store.storeID != right.store.storeID {
		return left.store.storeID < right.store.storeID
	}
	if left.revisionID != right.revisionID {
		return left.revisionID < right.revisionID
	}
	return left.token < right.token
}

func (c *Coordinator) claimCandidateLocked(decision CapacityDecisionID, requester *storeState, candidate *candidateState) *reclaimState {
	c.nextClaim++
	claim := &reclaimState{
		id: ReclaimClaimID(fmt.Sprintf("%s-claim-%d", decision, c.nextClaim)), decisionID: decision,
		candidate: candidate, requester: requester,
	}
	candidate.claim = claim
	c.reclaimable--
	candidate.store.reclaimable--
	candidate.store.reclaims++
	c.activeReclaims++
	return claim
}

func (c *Coordinator) abandonClaimLocked(candidate *candidateState, claim *reclaimState, crossStoreShareReservation bool) {
	if candidate.claim != claim {
		return
	}
	candidate.claim = nil
	candidate.store.reclaims--
	c.activeReclaims--
	if crossStoreShareReservation {
		claim.requester.used.StableHandles--
	}
	if candidate.withdrawn || candidate.store.closing {
		delete(c.candidates, candidate.key)
		delete(c.byCharge, candidate.charge)
	} else {
		c.reclaimable++
		candidate.store.reclaimable++
	}
	c.cond.Broadcast()
}

type reclaimFinishKind uint8

const (
	reclaimFinishDeclined reclaimFinishKind = iota + 1
	reclaimFinishCompleted
	reclaimFinishUncertain
)

type reclaimFinish struct {
	kind  reclaimFinishKind
	cause error
}

func (c *Coordinator) finishClaimLocked(candidate *candidateState, claim *reclaimState, result ReclaimResult, state *admissionState, crossStoreShareReservation bool) reclaimFinish {
	if candidate.claim != claim {
		return reclaimFinish{kind: reclaimFinishUncertain, cause: ErrInvalidReclaimResult}
	}
	candidate.claim = nil
	candidate.store.reclaims--
	c.activeReclaims--
	delete(c.candidates, candidate.key)
	delete(c.byCharge, candidate.charge)
	c.cond.Broadcast()
	if result.claim != claim {
		return reclaimFinish{kind: reclaimFinishUncertain, cause: ErrInvalidReclaimResult}
	}
	switch result.kind {
	case reclaimResultDeclined:
		if crossStoreShareReservation {
			state.store.used.StableHandles--
		}
		return reclaimFinish{kind: reclaimFinishDeclined}
	case reclaimResultCompleted:
		return reclaimFinish{kind: reclaimFinishCompleted}
	case reclaimResultUncertain:
		cause := result.diagnostic
		if cause == nil {
			cause = ErrInvalidReclaimResult
		}
		return reclaimFinish{kind: reclaimFinishUncertain, cause: cause}
	default:
		return reclaimFinish{kind: reclaimFinishUncertain, cause: ErrInvalidReclaimResult}
	}
}

func (c *Coordinator) transferTerminalVictimLocked(candidate *candidateState, state *admissionState, crossStoreShareReservation bool) {
	victim := candidate.charge
	victim.status = chargeTransferred
	victim.store.liveCharges--
	if crossStoreShareReservation {
		victim.store.used.StableHandles--
	}
	// Process usage is deliberately unchanged: the claimed unit moves directly
	// from victim to requester without becoming globally available.
	state.stableReserved = true
	c.cond.Broadcast()
}

func (c *Coordinator) releaseTerminalVictimLocked(candidate *candidateState, state *admissionState, crossStoreShareReservation bool) {
	victim := candidate.charge
	victim.status = chargeReleased
	victim.store.liveCharges--
	victim.store.used.StableHandles--
	c.used.StableHandles--
	if crossStoreShareReservation {
		state.store.used.StableHandles--
	}
	c.cond.Broadcast()
}

func (c *Coordinator) quarantineVictimLocked(candidate *candidateState, state *admissionState, crossStoreShareReservation bool) {
	victim := candidate.charge
	victim.status = chargeQuarantined
	victim.store.liveCharges--
	victim.store.quarantined++
	c.quarantined++
	if crossStoreShareReservation {
		state.store.used.StableHandles--
	}
	c.cond.Broadcast()
}
