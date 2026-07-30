package browsermatrixpion

import (
	"errors"
	"time"
)

func (authority *ControlCredentialAuthority) beginRevocation(
	leaseID string,
) (ControlCredentialReceipt, bool, error) {
	if !validOpaqueID(leaseID) {
		return ControlCredentialReceipt{}, false, errors.New("control credential lease ID is invalid")
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	now := authority.clock.Now().UTC()
	authority.pruneLocked(now)
	if retirement, exists := authority.retirementsByLeaseID[leaseID]; exists {
		if retirement.completed {
			return retirement.receipt, true, nil
		}
		retirement.revoking = true
		authority.retirementsByLeaseID[leaseID] = retirement
		return ControlCredentialReceipt{}, false, nil
	}
	claim := authority.claimsByLeaseID[leaseID]
	if claim == nil {
		return ControlCredentialReceipt{}, false, errors.New("control credential lease is not active")
	}
	claim.revoked = true
	claim.revocationStarted = true
	return ControlCredentialReceipt{}, false, nil
}

func (authority *ControlCredentialAuthority) waitForRevocationRequests(leaseID string) error {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if retirement, exists := authority.retirementsByLeaseID[leaseID]; exists {
		if !retirement.completed && retirement.revoking {
			return nil
		}
		return errors.New("control credential lease is not revoking")
	}
	claim := authority.claimsByLeaseID[leaseID]
	if claim == nil || !claim.revoked {
		return errors.New("control credential lease is not revoking")
	}
	for claim.inFlight != 0 {
		authority.condition.Wait()
	}
	return nil
}

func (authority *ControlCredentialAuthority) finishRevocation(
	leaseID string,
) (ControlCredentialReceipt, error) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	now := authority.clock.Now().UTC()
	authority.pruneLocked(now)
	if retirement, exists := authority.retirementsByLeaseID[leaseID]; exists {
		if retirement.completed {
			return retirement.receipt, nil
		}
		if !retirement.revoking {
			return ControlCredentialReceipt{}, errors.New("control credential revocation is incomplete")
		}
		return authority.completeExpiredRetirementLocked(leaseID, retirement, now), nil
	}
	claim := authority.claimsByLeaseID[leaseID]
	if claim == nil || !claim.revoked || claim.inFlight != 0 {
		return ControlCredentialReceipt{}, errors.New("control credential revocation is incomplete")
	}
	return authority.recordRetirementLocked(leaseID, now), nil
}

func (authority *ControlCredentialAuthority) Release(
	leaseID string,
) (ControlCredentialReceipt, error) {
	if !validOpaqueID(leaseID) {
		return ControlCredentialReceipt{}, errors.New("control credential lease ID is invalid")
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	now := authority.clock.Now().UTC()
	authority.pruneLocked(now)
	if retirement, exists := authority.retirementsByLeaseID[leaseID]; exists {
		if retirement.completed {
			return retirement.receipt, nil
		}
		return ControlCredentialReceipt{}, errors.New("control credential lease is not active")
	}
	claim := authority.claimsByLeaseID[leaseID]
	if claim == nil || claim.revoked {
		return ControlCredentialReceipt{}, errors.New("control credential lease is not active")
	}
	if claim.inFlight != 0 {
		return ControlCredentialReceipt{}, errors.New("control credential lease still owns active requests")
	}
	claim.revoked = true
	return authority.recordRetirementLocked(leaseID, now), nil
}

func (authority *ControlCredentialAuthority) RevokeAndWait(
	leaseID string,
) (ControlCredentialReceipt, error) {
	receipt, completed, err := authority.beginRevocation(leaseID)
	if err != nil {
		return ControlCredentialReceipt{}, err
	}
	if completed {
		return receipt, nil
	}
	if err := authority.waitForRevocationRequests(leaseID); err != nil {
		return ControlCredentialReceipt{}, err
	}
	return authority.finishRevocation(leaseID)
}

func (authority *ControlCredentialAuthority) Close() {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.closed {
		return
	}
	authority.closed = true
	for _, claim := range authority.claimsByLeaseID {
		claim.revoked = true
	}
	for authority.hasInFlightRequestsLocked() {
		authority.condition.Wait()
	}
	for index := range authority.signer {
		authority.signer[index] = 0
	}
	for leaseID := range authority.claimsByLeaseID {
		authority.deleteClaimLocked(leaseID)
	}
	clear(authority.acquireReplaysByRequestID)
	clear(authority.retirementsByLeaseID)
	clear(authority.probeClaimExpiresAt)
	clear(authority.attestationClaimExpiresAt)
}

func (authority *ControlCredentialAuthority) pruneLocked(now time.Time) {
	for leaseID, claim := range authority.claimsByLeaseID {
		if now.Before(claim.expiresAt) {
			continue
		}
		claim.revoked = true
		if claim.inFlight == 0 {
			authority.recordExpiredClaimLocked(leaseID, now)
		}
	}
	for requestID, replay := range authority.acquireReplaysByRequestID {
		if !now.Before(replay.expiresAt) {
			delete(authority.acquireReplaysByRequestID, requestID)
		}
	}
	for probeKey, expiresAt := range authority.probeClaimExpiresAt {
		if !now.Before(expiresAt) {
			delete(authority.probeClaimExpiresAt, probeKey)
		}
	}
	for digest, expiresAt := range authority.attestationClaimExpiresAt {
		if !now.Before(expiresAt) {
			delete(authority.attestationClaimExpiresAt, digest)
		}
	}
	for leaseID, retirement := range authority.retirementsByLeaseID {
		if !now.Before(retirement.expiresAt) && (!retirement.revoking || retirement.completed) {
			delete(authority.retirementsByLeaseID, leaseID)
		}
	}
}

func (authority *ControlCredentialAuthority) hasInFlightRequestsLocked() bool {
	for _, claim := range authority.claimsByLeaseID {
		if claim.inFlight != 0 {
			return true
		}
	}
	return false
}

func (authority *ControlCredentialAuthority) deleteClaimLocked(leaseID string) {
	claim := authority.claimsByLeaseID[leaseID]
	if claim == nil {
		return
	}
	if claim.inFlight != 0 {
		panic("control credential authority deleted an in-flight request owner")
	}
	eraseCredentialBytes(claim.credential)
	delete(authority.claimsByLeaseID, leaseID)
}

func (authority *ControlCredentialAuthority) retirementState(
	leaseID string,
) (controlCredentialRetirementState, bool) {
	if !validOpaqueID(leaseID) {
		return controlCredentialRetirementState{}, false
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	now := authority.clock.Now().UTC()
	authority.pruneLocked(now)
	if retirement, exists := authority.retirementsByLeaseID[leaseID]; exists {
		return controlCredentialRetirementState{
			receipt: retirement.receipt, scopeExpiresAt: retirement.scopeExpiresAt,
			expiresAt: retirement.expiresAt, completed: retirement.completed,
		}, true
	}
	claim := authority.claimsByLeaseID[leaseID]
	if claim == nil {
		return controlCredentialRetirementState{}, false
	}
	return controlCredentialRetirementState{
		scopeExpiresAt: claim.expiresAt,
		expiresAt:      authority.retirementExpiresAtLocked(claim.expiresAt, now),
	}, true
}

func (authority *ControlCredentialAuthority) recordRetirementLocked(
	leaseID string,
	now time.Time,
) ControlCredentialReceipt {
	claim := authority.claimsByLeaseID[leaseID]
	if claim == nil || claim.inFlight != 0 {
		panic("control credential authority retired an invalid request owner")
	}
	receipt := ControlCredentialReceipt{LeaseID: claim.lease.LeaseID, Terminal: "revoked"}
	expiresAt := authority.retirementExpiresAtLocked(claim.expiresAt, now)
	authority.retirementsByLeaseID[claim.lease.LeaseID] = controlCredentialRetirement{
		receipt: receipt, acquireRequestID: claim.lease.RequestID,
		probeKey:          controlProbeClaimKey(claim.lease.RunID, claim.lease.ProbeNonce),
		attestationSHA256: claim.lease.AttestationSHA256, scopeExpiresAt: claim.expiresAt,
		expiresAt: expiresAt,
		completed: true,
	}
	authority.extendClaimTombstonesLocked(
		claim.lease.RequestID,
		controlProbeClaimKey(claim.lease.RunID, claim.lease.ProbeNonce),
		claim.lease.AttestationSHA256,
		expiresAt,
	)
	authority.deleteClaimLocked(leaseID)
	return receipt
}

func (authority *ControlCredentialAuthority) recordExpiredClaimLocked(
	leaseID string,
	now time.Time,
) {
	claim := authority.claimsByLeaseID[leaseID]
	if claim == nil || claim.inFlight != 0 || now.Before(claim.expiresAt) {
		panic("control credential authority expired an invalid request owner")
	}
	// Expiry only burns authorization; retention starts when the service observes
	// the burn because physical attempt containment may still be outstanding.
	expiresAt := authority.retirementExpiresAtLocked(claim.expiresAt, now)
	probeKey := controlProbeClaimKey(claim.lease.RunID, claim.lease.ProbeNonce)
	authority.retirementsByLeaseID[leaseID] = controlCredentialRetirement{
		acquireRequestID: claim.lease.RequestID, probeKey: probeKey,
		attestationSHA256: claim.lease.AttestationSHA256, scopeExpiresAt: claim.expiresAt,
		expiresAt: expiresAt, revoking: claim.revocationStarted,
	}
	authority.extendClaimTombstonesLocked(
		claim.lease.RequestID, probeKey, claim.lease.AttestationSHA256, expiresAt,
	)
	authority.deleteClaimLocked(leaseID)
}

func (authority *ControlCredentialAuthority) completeExpiredRetirementLocked(
	leaseID string,
	retirement controlCredentialRetirement,
	now time.Time,
) ControlCredentialReceipt {
	receipt := ControlCredentialReceipt{LeaseID: leaseID, Terminal: "revoked"}
	retirement.receipt = receipt
	retirement.completed = true
	retirement.expiresAt = authority.retirementExpiresAtLocked(retirement.scopeExpiresAt, now)
	authority.retirementsByLeaseID[leaseID] = retirement
	authority.extendClaimTombstonesLocked(
		retirement.acquireRequestID,
		retirement.probeKey,
		retirement.attestationSHA256,
		retirement.expiresAt,
	)
	return receipt
}

func (authority *ControlCredentialAuthority) extendClaimTombstonesLocked(
	requestID string,
	probeKey string,
	attestationSHA256 string,
	expiresAt time.Time,
) {
	if replay, exists := authority.acquireReplaysByRequestID[requestID]; exists &&
		expiresAt.After(replay.expiresAt) {
		replay.expiresAt = expiresAt
		authority.acquireReplaysByRequestID[requestID] = replay
	}
	if current, exists := authority.probeClaimExpiresAt[probeKey]; exists && expiresAt.After(current) {
		authority.probeClaimExpiresAt[probeKey] = expiresAt
	}
	if current, exists := authority.attestationClaimExpiresAt[attestationSHA256]; exists &&
		expiresAt.After(current) {
		authority.attestationClaimExpiresAt[attestationSHA256] = expiresAt
	}
}
