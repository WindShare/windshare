package browsermatrixpion

import (
	"errors"
	"sync"
	"time"
)

func (authority *ControlCredentialAuthority) Authenticate(leaseID, credential string) bool {
	if !validOpaqueID(leaseID) {
		return false
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	now := authority.clock.Now().UTC()
	authority.pruneLocked(now)
	claim := authority.claimsByLeaseID[leaseID]
	return !authority.closed && authority.claimMatchesCredentialLocked(claim, credential) &&
		!claim.revoked && now.Before(claim.expiresAt)
}

// beginRequest transfers a temporary use lease to the HTTP handler. Retirement
// can then prove that no credential-authorized operation still owns execution.
func (authority *ControlCredentialAuthority) beginRequest(
	leaseID string,
	credential string,
) (func(), bool) {
	if !validOpaqueID(leaseID) {
		return nil, false
	}
	authority.mu.Lock()
	now := authority.clock.Now().UTC()
	authority.pruneLocked(now)
	claim := authority.claimsByLeaseID[leaseID]
	if authority.closed || !authority.claimMatchesCredentialLocked(claim, credential) ||
		claim.revoked || !now.Before(claim.expiresAt) {
		authority.mu.Unlock()
		return nil, false
	}
	claim.inFlight++
	authority.mu.Unlock()

	var once sync.Once
	return func() { once.Do(func() { authority.endRequest(leaseID) }) }, true
}

func (authority *ControlCredentialAuthority) endRequest(leaseID string) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	claim := authority.claimsByLeaseID[leaseID]
	if claim == nil || claim.inFlight < 1 {
		panic("control credential authority lost an in-flight request owner")
	}
	claim.inFlight--
	if claim.inFlight == 0 {
		now := authority.clock.Now().UTC()
		if !now.Before(claim.expiresAt) {
			authority.recordExpiredClaimLocked(leaseID, now)
		}
		authority.condition.Broadcast()
	}
}

func (authority *ControlCredentialAuthority) claimAttempt(
	leaseID string,
	requestID string,
) error {
	if !validOpaqueID(leaseID) || !validOpaqueID(requestID) {
		return errors.New("control credential attempt claim is invalid")
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	now := authority.clock.Now().UTC()
	authority.pruneLocked(now)
	claim := authority.claimsByLeaseID[leaseID]
	if claim == nil || claim.revoked || !now.Before(claim.expiresAt) {
		return errors.New("control credential lease cannot own an attempt")
	}
	if claim.attemptRequestID == "" {
		claim.attemptRequestID = requestID
		return nil
	}
	if claim.attemptRequestID != requestID {
		return errors.New("control credential lease already consumed its attempt")
	}
	return nil
}

func (authority *ControlCredentialAuthority) leaseActive(leaseID string) bool {
	if !validOpaqueID(leaseID) {
		return false
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	now := authority.clock.Now().UTC()
	authority.pruneLocked(now)
	claim := authority.claimsByLeaseID[leaseID]
	return claim != nil && !claim.revoked && now.Before(claim.expiresAt)
}

func (authority *ControlCredentialAuthority) activeLeaseMetadata(
	leaseID string,
) (ControlCredentialLease, bool) {
	if !validOpaqueID(leaseID) {
		return ControlCredentialLease{}, false
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	now := authority.clock.Now().UTC()
	authority.pruneLocked(now)
	claim := authority.claimsByLeaseID[leaseID]
	if claim == nil || claim.revoked || !now.Before(claim.expiresAt) {
		return ControlCredentialLease{}, false
	}
	return cloneControlCredentialLeaseMetadata(claim.lease), true
}

// consumeAuthorityProbe burns the one-shot probe claim before comparing its
// scope. A mismatched request therefore cannot turn the bearer into a nonce
// oracle or retry it against a different transaction.
func (authority *ControlCredentialAuthority) consumeAuthorityProbe(
	leaseID string,
	credential string,
	request AuthorityProbeRequest,
	expectedProfileID string,
) (AuthorityProbeResponse, string, time.Time, bool) {
	if !validOpaqueID(leaseID) {
		return AuthorityProbeResponse{}, "", time.Time{}, false
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	now := authority.clock.Now().UTC()
	authority.pruneLocked(now)
	claim := authority.claimsByLeaseID[leaseID]
	if authority.closed || !authority.claimMatchesCredentialLocked(claim, credential) ||
		claim.revoked || claim.authorityProbeAttempted {
		return AuthorityProbeResponse{}, "", time.Time{}, false
	}
	claim.authorityProbeAttempted = true

	response := claim.response
	document, err := CanonicalLiveAttestationDocument(response.Attestation)
	exactDigest := err == nil && sha256Hex(document) == response.AttestationSHA256
	sampleAuthority := request.ControlAuthority.SampleAuthority
	if claim.lease.RunID != sampleAuthority.RunID ||
		claim.lease.ProfileID != expectedProfileID ||
		claim.lease.ProbeNonce != request.Nonce ||
		claim.requestedLeaseMillis != request.RequestedLeaseMillis ||
		claim.lease.AttestationSHA256 != response.AttestationSHA256 ||
		claim.lease.AuthorityInstanceID != response.Attestation.Fixture.AuthorityInstanceID ||
		response.Attestation.RunID != sampleAuthority.RunID ||
		response.Attestation.Nonce != request.Nonce ||
		response.Attestation.ExpiresAt != claim.lease.ExpiresAt ||
		!exactDigest || !now.Before(claim.expiresAt) {
		return AuthorityProbeResponse{}, "", time.Time{}, false
	}
	return cloneAuthorityProbeResponse(response), claim.lease.LeaseID, claim.expiresAt, true
}

func (authority *ControlCredentialAuthority) signAttemptTerminalReceipt(
	receipt AttemptTerminalReceipt,
) (SignedAttemptTerminalReceipt, error) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.closed {
		return SignedAttemptTerminalReceipt{}, errors.New("control credential authority is closed")
	}
	return SignAttemptTerminalReceipt(receipt, authority.signer)
}
