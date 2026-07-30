package browsermatrixbroker

import (
	"context"
	"errors"
	"time"
)

func (handler *Handler) CloseAndWait(ctx context.Context) error {
	return handler.closeAndWait(ctx, false)
}

func (handler *Handler) ForceCloseAndWait(ctx context.Context) error {
	return handler.closeAndWait(ctx, true)
}

func (handler *Handler) closeAndWait(ctx context.Context, force bool) error {
	if handler == nil || ctx == nil {
		return errors.New("credential broker close authority is incomplete")
	}
	handler.mu.Lock()
	handler.accepting = false
	if force {
		handler.cancelLifecycle()
	}
	if handler.settled {
		handler.mu.Unlock()
		return nil
	}
	activeZero := handler.activeZero
	handler.mu.Unlock()
	select {
	case <-activeZero:
	case <-ctx.Done():
		return errors.New("credential broker active request settlement exceeded its authority")
	}
	select {
	case <-handler.settlementGate:
		defer func() { handler.settlementGate <- struct{}{} }()
	case <-ctx.Done():
		return errors.New("credential broker settlement ownership wait exceeded its authority")
	}
	handler.mu.Lock()
	if handler.settled {
		handler.mu.Unlock()
		return nil
	}
	active := make([]*compositeLease, 0, len(handler.leases))
	for _, lease := range handler.leases {
		if lease.ready && !lease.terminal {
			active = append(active, lease)
		}
	}
	failed := make([]*acquisitionClaim, 0, len(handler.acquisitions))
	for _, claim := range handler.acquisitions {
		if claim.failed && (claim.controlOwned || claim.providerOwned) {
			failed = append(failed, claim)
		}
	}
	handler.mu.Unlock()
	var failures []error
	for _, claim := range failed {
		claim.operation.Lock()
		settlementErr := handler.settleFailedAcquisition(claim)
		claim.operation.Unlock()
		if settlementErr != nil {
			failures = append(failures, settlementErr)
		}
	}
	for _, lease := range active {
		scope := lease.scope
		scope.Operation = "revoke-and-wait"
		scope.RequestID = lease.scope.RevokeRequestID
		scope.LeaseID = lease.leaseID
		retirementContext, cancel := context.WithTimeout(context.Background(), handler.retirementTimeout)
		_, _, err := handler.executeRetirement(retirementContext, scope)
		cancel()
		if err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) != 0 {
		return errors.Join(failures...)
	}
	handler.mu.Lock()
	handler.settled = true
	erase(handler.signer)
	handler.mu.Unlock()
	return nil
}

func (handler *Handler) admit() bool {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if !handler.accepting {
		return false
	}
	if handler.activeRequests == 0 {
		handler.activeZero = make(chan struct{})
	}
	handler.activeRequests++
	return true
}

func (handler *Handler) finishRequest() {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if handler.activeRequests < 1 {
		panic("credential broker lost an admitted request")
	}
	handler.activeRequests--
	if handler.activeRequests == 0 {
		close(handler.activeZero)
	}
}

func (handler *Handler) pruneLocked(now time.Time) {
	for requestID, claim := range handler.acquisitions {
		if claim.inFlight == 0 && !claim.controlOwned && !claim.providerOwned &&
			!claim.expiresAt.IsZero() && !now.Before(claim.expiresAt) {
			delete(handler.acquisitions, requestID)
			delete(handler.requestOwners, claim.scope.RequestID)
			delete(handler.requestOwners, claim.scope.ReleaseRequestID)
			delete(handler.requestOwners, claim.scope.RevokeRequestID)
		}
	}
	for leaseID, lease := range handler.leases {
		if lease.terminal && !now.Before(lease.expiresAt) {
			delete(handler.leases, leaseID)
		}
	}
	for requestID, claim := range handler.retirementClaims {
		if claim.completed && !now.Before(claim.expiresAt) {
			delete(handler.retirementClaims, requestID)
		}
	}
}

func (handler *Handler) emit(scope requestScope, outcome string) {
	if handler.trace == nil {
		return
	}
	handler.trace(TraceEvent{
		Milestone: "credential-broker-operation", Operation: scope.Operation,
		RequestID: scope.RequestID, LeaseID: scope.LeaseID, Outcome: outcome,
	})
}
