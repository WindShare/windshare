package browsermatrixbroker

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"github.com/windshare/windshare/internal/browsermatrixpion"
)

func (handler *Handler) retire(ctx context.Context, scope requestScope) ([]byte, error) {
	claim, normalizedScope, owner, err := handler.retirementClaim(scope)
	if err != nil {
		return nil, err
	}
	if owner {
		go handler.completeRetirement(normalizedScope, claim)
	}
	select {
	case <-claim.done:
		if claim.err != nil {
			return nil, claim.err
		}
		return encodeReceiptFrame(handler.signer, claim.receipt)
	case <-ctx.Done():
		return nil, errors.New("credential broker retirement waiter was cancelled")
	}
}

func (handler *Handler) completeRetirement(scope requestScope, claim *retirementClaim) {
	retirementContext, cancel := context.WithTimeout(context.Background(), handler.retirementTimeout)
	receipt, expiresAt, err := handler.executeRetirement(retirementContext, scope)
	cancel()
	handler.mu.Lock()
	if claim.scope != scope || !claim.inProgress {
		handler.mu.Unlock()
		panic("credential broker retirement lost its exact registry owner")
	}
	claim.err = err
	claim.inProgress = false
	if err == nil {
		claim.receipt = receipt
		claim.expiresAt = expiresAt
		claim.completed = true
	}
	close(claim.done)
	handler.mu.Unlock()
}

func (handler *Handler) executeRetirement(
	ctx context.Context,
	scope requestScope,
) (ReceiptPayload, time.Time, error) {
	handler.mu.Lock()
	lease := handler.leases[scope.LeaseID]
	ready := lease != nil && lease.ready
	handler.mu.Unlock()
	if lease == nil || !exactRetirementScope(scope, lease) || !ready {
		return ReceiptPayload{}, time.Time{}, errOperationConflict
	}
	lease.operation.Lock()
	defer lease.operation.Unlock()

	if lease.terminal {
		if lease.terminalOperation != scope.Operation || lease.receipt.RequestID != scope.RequestID {
			return ReceiptPayload{}, time.Time{}, errOperationConflict
		}
		return lease.receipt, lease.expiresAt, nil
	}
	var adminErr, turnErr error
	if !lease.controlSettled {
		var adminReceipt browsermatrixpion.ControlCredentialReceipt
		if scope.Operation == "release" {
			adminReceipt, adminErr = handler.admin.ReleaseControlCredential(scope.LeaseID)
		} else {
			adminReceipt, adminErr = handler.admin.RevokeControlCredentialAndWait(scope.LeaseID)
		}
		if adminErr == nil && adminReceipt.LeaseID == scope.LeaseID && adminReceipt.Terminal == "revoked" {
			lease.controlSettled = true
		} else if adminErr == nil {
			adminErr = errors.New("control authority returned a nonterminal receipt")
		}
	}
	// The provider remains usable until the contained Pion authority proves that
	// every control-authenticated request and attempt is reaped. This ordering
	// prevents a rejected graceful release from destroying a still-owned attempt.
	if lease.controlSettled && !lease.turnSettled {
		turnErr = handler.revokeTURN(ctx, scope, lease)
		if turnErr == nil {
			lease.turnSettled = true
		}
	}
	if !lease.controlSettled || !lease.turnSettled {
		return ReceiptPayload{}, time.Time{}, errors.Join(errCapabilityUnavailable, adminErr, turnErr)
	}
	now := handler.clock.Now().UTC().Truncate(time.Millisecond)
	expiresAt := now.Add(handler.tombstoneRetention)
	if lease.controlExpires.After(expiresAt) {
		expiresAt = lease.controlExpires
	}
	receipt := ReceiptPayload{
		ProtocolVersion: RemotePionProtocolVersion, Operation: scope.Operation,
		RequestID: scope.RequestID, ReleaseRequestID: lease.scope.ReleaseRequestID,
		RevokeRequestID: lease.scope.RevokeRequestID, LeaseID: scope.LeaseID,
		RunID: lease.scope.RunID, ProfileID: lease.scope.ProfileID,
		ProbeNonce: lease.scope.ProbeNonce, AuthorityInstanceID: lease.authorityID,
		AttestationSHA256: lease.attestationSHA256, LeaseExpiresAt: lease.controlExpiresAt,
		ControlTerminal: "revoked", TURNProviderLeaseID: lease.providerLeaseID,
		TURNTerminal: turnTerminal(handler.turnProvider != nil),
		Terminal:     "revoked", RetiredAt: now.Format(canonicalTimestampLayout),
	}
	handler.mu.Lock()
	lease.terminal = true
	lease.terminalOperation = scope.Operation
	lease.expiresAt = expiresAt
	lease.receipt = receipt
	if acquisition := handler.acquisitions[lease.scope.RequestID]; acquisition != nil &&
		expiresAt.After(acquisition.expiresAt) {
		acquisition.expiresAt = expiresAt
	}
	if acquisition := handler.acquisitions[lease.scope.RequestID]; acquisition != nil {
		acquisition.controlOwned = false
		acquisition.providerOwned = false
	}
	handler.mu.Unlock()
	return receipt, expiresAt, nil
}

func (handler *Handler) retirementClaim(
	scope requestScope,
) (*retirementClaim, requestScope, bool, error) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.pruneLocked(handler.clock.Now().UTC())
	lease := handler.leases[scope.LeaseID]
	if lease == nil || !exactRetirementRequest(scope, lease) || !lease.ready {
		return nil, requestScope{}, false, errOperationConflict
	}
	scope.ReleaseRequestID = lease.scope.ReleaseRequestID
	scope.RevokeRequestID = lease.scope.RevokeRequestID
	if claim := handler.retirementClaims[scope.RequestID]; claim != nil {
		if claim.scope != scope {
			return nil, requestScope{}, false, errOperationConflict
		}
		if claim.completed || claim.inProgress {
			return claim, scope, false, nil
		}
		claim.done = make(chan struct{})
		claim.err = nil
		claim.inProgress = true
		return claim, scope, true, nil
	}
	if len(handler.retirementClaims) >= handler.maximumTombstones {
		return nil, requestScope{}, false, errCapacityExhausted
	}
	claim := &retirementClaim{scope: scope, done: make(chan struct{}), inProgress: true}
	handler.retirementClaims[scope.RequestID] = claim
	return claim, scope, true, nil
}

func (handler *Handler) revokeTURN(
	ctx context.Context,
	scope requestScope,
	lease *compositeLease,
) error {
	if handler.turnProvider == nil {
		return nil
	}
	return handler.revokeTURNProvider(ctx, TURNRetirementRequest{
		Operation:       scope.Operation,
		RequestID:       derivedRequestID(scope.RequestID, lease.providerLeaseID, scope.Operation),
		ProviderLeaseID: lease.providerLeaseID, ControlLeaseID: lease.leaseID,
		RunID: lease.scope.RunID, ProfileID: lease.scope.ProfileID,
		ProbeNonce: lease.scope.ProbeNonce, AttestationSHA256: lease.attestationSHA256,
	})
}

func (handler *Handler) revokeTURNClaim(
	ctx context.Context,
	claim *acquisitionClaim,
) error {
	if handler.turnProvider == nil {
		return nil
	}
	attestationSHA256 := ""
	handler.mu.Lock()
	providerOwned := claim.providerOwned
	lease := handler.leases[claim.leaseID]
	handler.mu.Unlock()
	if !providerOwned {
		return nil
	}
	if lease != nil {
		attestationSHA256 = lease.attestationSHA256
	}
	return handler.revokeTURNProvider(ctx, TURNRetirementRequest{
		Operation:       "revoke-and-wait",
		RequestID:       derivedRequestID(claim.scope.RevokeRequestID, claim.providerLeaseID, "acquisition-failure"),
		ProviderLeaseID: claim.providerLeaseID, ControlLeaseID: claim.leaseID,
		RunID: claim.scope.RunID, ProfileID: claim.scope.ProfileID,
		ProbeNonce: claim.scope.ProbeNonce, AttestationSHA256: attestationSHA256,
	})
}

func (handler *Handler) revokeTURNProvider(
	ctx context.Context,
	request TURNRetirementRequest,
) error {
	if !validOpaqueID(request.ProviderLeaseID) || !validOpaqueID(request.RequestID) {
		return errors.New("revocable TURN provider lease is invalid")
	}
	receipt, err := handler.turnProvider.RevokeAndWait(ctx, request)
	if err != nil || receipt.RequestID != request.RequestID ||
		receipt.ProviderLeaseID != request.ProviderLeaseID ||
		receipt.Terminal != "revoked" {
		return errors.New("revocable TURN provider did not prove early revocation")
	}
	return nil
}

func exactRetirementRequest(scope requestScope, lease *compositeLease) bool {
	if scope.ControllerOrigin != lease.scope.ControllerOrigin || scope.RunID != lease.scope.RunID ||
		scope.ProfileID != lease.scope.ProfileID || scope.ProbeNonce != lease.scope.ProbeNonce ||
		scope.Identity != lease.scope.Identity || scope.LeaseID != lease.leaseID {
		return false
	}
	if scope.Operation == "release" {
		return scope.RequestID == lease.scope.ReleaseRequestID
	}
	return scope.Operation == "revoke-and-wait" && scope.RequestID == lease.scope.RevokeRequestID
}

func exactRetirementScope(scope requestScope, lease *compositeLease) bool {
	return exactRetirementRequest(scope, lease) &&
		scope.ReleaseRequestID == lease.scope.ReleaseRequestID &&
		scope.RevokeRequestID == lease.scope.RevokeRequestID
}

func derivedRequestID(values ...string) string {
	digest := sha256.New()
	for _, value := range values {
		_, _ = digest.Write([]byte(value))
		_, _ = digest.Write([]byte{'\n'})
	}
	return base64.RawURLEncoding.EncodeToString(digest.Sum(nil))
}

func turnTerminal(required bool) string {
	if required {
		return "revoked"
	}
	return "not-required"
}
