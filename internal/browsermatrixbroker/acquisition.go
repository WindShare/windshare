package browsermatrixbroker

import (
	"context"
	"crypto/subtle"
	"errors"
	"time"

	"github.com/windshare/windshare/internal/browsermatrixpion"
)

func (handler *Handler) acquire(ctx context.Context, scope requestScope) ([]byte, error) {
	claim, err := handler.acquireClaim(scope)
	if err != nil {
		return nil, err
	}
	defer handler.releaseAcquisitionClaim(claim)
	claim.operation.Lock()
	defer claim.operation.Unlock()
	if claim.failed {
		return nil, errors.Join(errOperationConflict, handler.settleFailedAcquisition(claim))
	}
	if claim.ready {
		return handler.replayReadyAcquisition(claim)
	}

	var reservation TURNReservation
	var turnDeclaration *browsermatrixpion.ControlTURNDeclaration
	if handler.turnProvider != nil {
		reservation, err = handler.acquireTURNReservation(ctx, claim)
		if err != nil {
			return nil, handler.failAcquisition(claim, err)
		}
		defer erase(reservation.Credential)
		turnDeclaration = &browsermatrixpion.ControlTURNDeclaration{
			CredentialID: reservation.CredentialID,
			Username:     reservation.Username,
			ExpiresAt:    reservation.ExpiresAt,
		}
	}
	lease, acquireErr := handler.admin.AcquireControlCredential(browsermatrixpion.ControlCredentialAcquireRequest{
		RequestID: scope.RequestID, RunID: scope.RunID, ProfileID: scope.ProfileID,
		ProbeNonce: scope.ProbeNonce, RequestedLeaseMillis: handler.leaseDuration.Milliseconds(),
		TURN: turnDeclaration,
	})
	defer erase(lease.Credential)
	registrationErr := handler.recordControlMaterialization(claim, lease, reservation)
	issuedAt, expiresAt, validationErr := validateControlLease(
		scope, lease, handler.clock.Now().UTC(), turnDeclaration,
	)
	if acquireErr != nil || registrationErr != nil || validationErr != nil {
		return nil, handler.failAcquisition(
			claim,
			errors.Join(errOperationConflict, acquireErr, registrationErr, validationErr),
		)
	}
	if handler.turnProvider != nil {
		bindRequest := TURNBindRequest{
			ProviderLeaseID: reservation.ProviderLeaseID, RequestID: scope.RequestID,
			RunID: scope.RunID, ProfileID: scope.ProfileID, ProbeNonce: scope.ProbeNonce,
			ControlLeaseID: lease.LeaseID, AttestationSHA256: lease.AttestationSHA256,
			CredentialID: reservation.CredentialID, Username: reservation.Username,
			ExpiresAt: reservation.ExpiresAt, MaxAttempts: 1,
		}
		bound, bindErr := handler.turnProvider.BindAndWait(ctx, bindRequest)
		if bindErr != nil || !exactTURNBoundLease(bindRequest, bound) ||
			subtle.ConstantTimeCompare(reservation.Credential, lease.Credential) == 1 ||
			handler.admin.BindControlTURNCredential(browsermatrixpion.ControlTURNCredentialLease{
				RequestID: scope.RequestID, RunID: scope.RunID, ProfileID: scope.ProfileID,
				ProbeNonce: scope.ProbeNonce, ControlLeaseID: lease.LeaseID,
				AttestationSHA256: lease.AttestationSHA256, CredentialID: reservation.CredentialID,
				Username: reservation.Username, ExpiresAt: reservation.ExpiresAt,
				MaxAttempts: 1, Credential: reservation.Credential,
			}) != nil {
			return nil, handler.failAcquisition(claim, errors.Join(errCapabilityUnavailable, bindErr))
		}
	}
	if err := handler.markCompositeReady(claim); err != nil {
		return nil, handler.failAcquisition(claim, err)
	}
	turnCapability := "not-required"
	if handler.turnProvider != nil {
		turnCapability = "bound"
	}
	payload := LeasePayload{
		ProtocolVersion: RemotePionProtocolVersion, RequestID: lease.RequestID,
		ReleaseRequestID: scope.ReleaseRequestID, RevokeRequestID: scope.RevokeRequestID,
		LeaseID: lease.LeaseID, RunID: lease.RunID, ProfileID: lease.ProfileID,
		ProbeNonce: lease.ProbeNonce, AuthorityInstanceID: lease.AuthorityInstanceID,
		AttestationSHA256: lease.AttestationSHA256,
		IssuedAt:          issuedAt.Format(canonicalTimestampLayout), ExpiresAt: expiresAt.Format(canonicalTimestampLayout),
		MaxAttempts: 1, CredentialByteLength: len(lease.Credential),
		TURNCapability: turnCapability, TURNProviderLeaseID: reservation.ProviderLeaseID,
		TURNCredentialID: reservation.CredentialID, TURNUsername: reservation.Username,
		TURNExpiresAt: reservation.ExpiresAt,
	}
	return encodeLeaseFrame(handler.signer, payload, lease.Credential)
}

func (handler *Handler) replayReadyAcquisition(claim *acquisitionClaim) ([]byte, error) {
	var turnDeclaration *browsermatrixpion.ControlTURNDeclaration
	if handler.turnProvider != nil {
		turnDeclaration = &browsermatrixpion.ControlTURNDeclaration{
			CredentialID: claim.turnCredentialID, Username: claim.turnUsername,
			ExpiresAt: claim.turnExpiresAt,
		}
	}
	lease, err := handler.admin.AcquireControlCredential(browsermatrixpion.ControlCredentialAcquireRequest{
		RequestID: claim.scope.RequestID, RunID: claim.scope.RunID, ProfileID: claim.scope.ProfileID,
		ProbeNonce: claim.scope.ProbeNonce, RequestedLeaseMillis: handler.leaseDuration.Milliseconds(),
		TURN: turnDeclaration,
	})
	defer erase(lease.Credential)
	issuedAt, expiresAt, validationErr := validateControlLease(
		claim.scope, lease, handler.clock.Now().UTC(), turnDeclaration,
	)
	handler.mu.Lock()
	composite := handler.leases[claim.leaseID]
	validComposite := composite != nil && composite.owner == claim && composite.ready && !composite.terminal &&
		composite.leaseID == lease.LeaseID && composite.authorityID == lease.AuthorityInstanceID &&
		composite.attestationSHA256 == lease.AttestationSHA256 && composite.controlExpiresAt == lease.ExpiresAt
	handler.mu.Unlock()
	if err != nil || validationErr != nil || !validComposite {
		return nil, errors.Join(errOperationConflict, err, validationErr)
	}
	turnCapability := "not-required"
	if handler.turnProvider != nil {
		turnCapability = "bound"
	}
	payload := LeasePayload{
		ProtocolVersion: RemotePionProtocolVersion, RequestID: lease.RequestID,
		ReleaseRequestID: claim.scope.ReleaseRequestID, RevokeRequestID: claim.scope.RevokeRequestID,
		LeaseID: lease.LeaseID, RunID: lease.RunID, ProfileID: lease.ProfileID,
		ProbeNonce: lease.ProbeNonce, AuthorityInstanceID: lease.AuthorityInstanceID,
		AttestationSHA256: lease.AttestationSHA256,
		IssuedAt:          issuedAt.Format(canonicalTimestampLayout), ExpiresAt: expiresAt.Format(canonicalTimestampLayout),
		MaxAttempts: 1, CredentialByteLength: len(lease.Credential), TURNCapability: turnCapability,
		TURNProviderLeaseID: claim.providerLeaseID, TURNCredentialID: claim.turnCredentialID,
		TURNUsername: claim.turnUsername, TURNExpiresAt: claim.turnExpiresAt,
	}
	return encodeLeaseFrame(handler.signer, payload, lease.Credential)
}

func (handler *Handler) acquireTURNReservation(
	ctx context.Context,
	claim *acquisitionClaim,
) (TURNReservation, error) {
	if claim.turnExpiresAt == "" {
		claim.turnExpiresAt = handler.clock.Now().UTC().Truncate(time.Millisecond).
			Add(handler.leaseDuration).Format(canonicalTimestampLayout)
	}
	request := TURNAcquireRequest{
		RequestID: claim.scope.RequestID, RunID: claim.scope.RunID,
		ProfileID: claim.scope.ProfileID, ProbeNonce: claim.scope.ProbeNonce,
		ExpiresAt: claim.turnExpiresAt, MaxAttempts: 1,
	}
	reservation, err := handler.turnProvider.Acquire(ctx, request)
	recordErr := handler.recordTURNMaterialization(claim, reservation)
	if err != nil || recordErr != nil || !exactTURNReservation(request, reservation) ||
		!validCredentialBytes(reservation.Credential) {
		return reservation, errors.Join(
			errors.New("revocable TURN provider rejected the reservation"), err, recordErr,
		)
	}
	return reservation, nil
}

func (handler *Handler) acquireClaim(scope requestScope) (*acquisitionClaim, error) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.pruneLocked(handler.clock.Now().UTC())
	if claim := handler.acquisitions[scope.RequestID]; claim != nil {
		if claim.scope != scope || handler.requestOwners[scope.ReleaseRequestID] != claim ||
			handler.requestOwners[scope.RevokeRequestID] != claim {
			return nil, errOperationConflict
		}
		claim.inFlight++
		return claim, nil
	}
	if len(handler.acquisitions) >= handler.maximumTombstones ||
		handler.requestOwners[scope.RequestID] != nil ||
		handler.requestOwners[scope.ReleaseRequestID] != nil ||
		handler.requestOwners[scope.RevokeRequestID] != nil {
		return nil, errCapacityExhausted
	}
	claim := &acquisitionClaim{
		scope:     scope,
		inFlight:  1,
		expiresAt: handler.clock.Now().UTC().Add(handler.tombstoneRetention),
	}
	handler.acquisitions[scope.RequestID] = claim
	handler.requestOwners[scope.RequestID] = claim
	handler.requestOwners[scope.ReleaseRequestID] = claim
	handler.requestOwners[scope.RevokeRequestID] = claim
	return claim, nil
}

func (handler *Handler) releaseAcquisitionClaim(claim *acquisitionClaim) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if claim.inFlight < 1 {
		panic("credential broker acquisition lost its exact registry owner")
	}
	claim.inFlight--
}

func (handler *Handler) recordTURNMaterialization(
	claim *acquisitionClaim,
	reservation TURNReservation,
) error {
	if !validOpaqueID(reservation.ProviderLeaseID) {
		return errors.New("revocable TURN provider lease ID is invalid")
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if claim.providerLeaseID != "" {
		if claim.providerLeaseID != reservation.ProviderLeaseID ||
			claim.turnCredentialID != reservation.CredentialID ||
			claim.turnUsername != reservation.Username || claim.turnExpiresAt != reservation.ExpiresAt {
			return errOperationConflict
		}
		return nil
	}
	claim.providerLeaseID = reservation.ProviderLeaseID
	claim.turnCredentialID = reservation.CredentialID
	claim.turnUsername = reservation.Username
	claim.turnExpiresAt = reservation.ExpiresAt
	claim.providerOwned = true
	return nil
}

func (handler *Handler) recordControlMaterialization(
	claim *acquisitionClaim,
	control browsermatrixpion.ControlCredentialLease,
	reservation TURNReservation,
) error {
	if !validOpaqueID(control.LeaseID) {
		return errors.New("control authority lease ID is invalid")
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if existing := handler.leases[control.LeaseID]; existing != nil {
		expectedScope := claim.scope
		expectedScope.LeaseID = control.LeaseID
		if existing.owner != claim || existing.scope != expectedScope ||
			existing.providerLeaseID != reservation.ProviderLeaseID ||
			existing.authorityID != control.AuthorityInstanceID ||
			existing.attestationSHA256 != control.AttestationSHA256 ||
			existing.controlExpiresAt != control.ExpiresAt {
			return errOperationConflict
		}
		claim.controlOwned = true
		return nil
	}
	if len(handler.leases) >= handler.maximumTombstones || claim.leaseID != "" {
		return errCapacityExhausted
	}
	controlExpires, _ := parseTimestamp(control.ExpiresAt)
	ownedScope := claim.scope
	ownedScope.LeaseID = control.LeaseID
	lease := &compositeLease{
		owner: claim, scope: ownedScope, leaseID: control.LeaseID,
		authorityID: control.AuthorityInstanceID, attestationSHA256: control.AttestationSHA256,
		controlExpiresAt: control.ExpiresAt, controlExpires: controlExpires,
		providerLeaseID: reservation.ProviderLeaseID, turnCredentialID: reservation.CredentialID,
		turnUsername: reservation.Username, turnExpiresAt: reservation.ExpiresAt,
		turnSettled: handler.turnProvider == nil, expiresAt: controlExpires,
	}
	handler.leases[control.LeaseID] = lease
	claim.leaseID = control.LeaseID
	claim.controlOwned = true
	claim.expiresAt = controlExpires
	return nil
}

func (handler *Handler) markCompositeReady(claim *acquisitionClaim) error {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	lease := handler.leases[claim.leaseID]
	if lease == nil || lease.owner != claim || lease.terminal || claim.failed {
		return errOperationConflict
	}
	lease.ready = true
	claim.ready = true
	return nil
}

func (handler *Handler) failAcquisition(claim *acquisitionClaim, cause error) error {
	handler.mu.Lock()
	claim.failed = true
	handler.mu.Unlock()
	return errors.Join(cause, handler.settleFailedAcquisition(claim))
}

func (handler *Handler) settleFailedAcquisition(claim *acquisitionClaim) error {
	retirementContext, cancel := context.WithTimeout(context.Background(), handler.retirementTimeout)
	defer cancel()
	handler.mu.Lock()
	providerOwned := claim.providerOwned
	controlOwned := claim.controlOwned
	lease := handler.leases[claim.leaseID]
	handler.mu.Unlock()
	var turnErr, controlErr error
	if controlOwned {
		receipt, err := handler.admin.RevokeControlCredentialAndWait(claim.leaseID)
		if err != nil || receipt.LeaseID != claim.leaseID || receipt.Terminal != "revoked" {
			controlErr = errors.Join(err, errors.New("control authority did not prove failed acquisition retirement"))
		} else {
			handler.mu.Lock()
			claim.controlOwned = false
			if lease != nil {
				lease.controlSettled = true
			}
			handler.mu.Unlock()
		}
	}
	if providerOwned && controlErr == nil {
		turnErr = handler.revokeTURNClaim(retirementContext, claim)
		if turnErr == nil {
			handler.mu.Lock()
			claim.providerOwned = false
			if lease != nil {
				lease.turnSettled = true
			}
			handler.mu.Unlock()
		}
	}
	if turnErr == nil && controlErr == nil {
		handler.mu.Lock()
		now := handler.clock.Now().UTC().Truncate(time.Millisecond)
		expiresAt := now.Add(handler.tombstoneRetention)
		if lease != nil {
			lease.terminal = true
			lease.expiresAt = expiresAt
		}
		claim.expiresAt = expiresAt
		handler.mu.Unlock()
	}
	return errors.Join(turnErr, controlErr)
}
