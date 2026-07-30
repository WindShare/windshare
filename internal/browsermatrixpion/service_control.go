package browsermatrixpion

import (
	"context"
	"errors"
	"time"
)

func (service *Service) AcquireControlCredential(
	request ControlCredentialAcquireRequest,
) (ControlCredentialLease, error) {
	if service.unavailable() {
		return ControlCredentialLease{}, errors.New("remote Pion service is unavailable")
	}
	service.mu.Lock()
	service.pruneControlRetirementsLocked(service.clock.Now().UTC())
	service.mu.Unlock()
	return service.controlCredentials.Acquire(request)
}

func (service *Service) ReleaseControlCredential(
	leaseID string,
) (ControlCredentialReceipt, error) {
	if !validOpaqueID(leaseID) {
		return ControlCredentialReceipt{}, errors.New("control credential lease ID is invalid")
	}
	service.mu.Lock()
	service.pruneControlRetirementsLocked(service.clock.Now().UTC())
	if current := service.controlRevocations[leaseID]; current != nil {
		service.mu.Unlock()
		<-current.done
		return current.receipt, current.err
	}
	if service.closed || len(service.containmentFailures) != 0 ||
		service.controlLeaseOwnsAttemptLocked(leaseID) {
		service.mu.Unlock()
		return ControlCredentialReceipt{}, errors.New("control credential lease still owns an attempt")
	}
	authorityState, exists := service.controlCredentials.retirementState(leaseID)
	if !exists {
		service.mu.Unlock()
		return ControlCredentialReceipt{}, errors.New("control credential lease is not active")
	}
	if len(service.controlRevocations) >= service.maximumTombstones {
		service.mu.Unlock()
		return ControlCredentialReceipt{}, errors.New("control credential retirement capacity is exhausted")
	}
	retirement := &controlLeaseRevocation{
		done: make(chan struct{}), expiresAt: authorityState.expiresAt,
	}
	service.controlRevocations[leaseID] = retirement
	service.retiringControlLeases[leaseID] = true
	if authorityState.completed {
		retirement.receipt = authorityState.receipt
		retirement.completed = true
		service.retireControlTURNCredentialLocked(leaseID)
		close(retirement.done)
		service.mu.Unlock()
		return retirement.receipt, nil
	}
	service.mu.Unlock()

	receipt, err := service.controlCredentials.Release(leaseID)
	service.completeControlLeaseRetirement(leaseID, retirement, receipt, err, false)
	return receipt, err
}

func (service *Service) RevokeControlCredentialAndWait(
	leaseID string,
) (ControlCredentialReceipt, error) {
	if !validOpaqueID(leaseID) {
		return ControlCredentialReceipt{}, errors.New("control credential lease ID is invalid")
	}
	service.mu.Lock()
	service.pruneControlRetirementsLocked(service.clock.Now().UTC())
	if current := service.controlRevocations[leaseID]; current != nil {
		done := current.done
		service.mu.Unlock()
		<-done
		return current.receipt, current.err
	}
	authorityState, exists := service.controlCredentials.retirementState(leaseID)
	if !exists {
		service.mu.Unlock()
		return ControlCredentialReceipt{}, errors.New("control credential lease is not active")
	}
	if len(service.controlRevocations) >= service.maximumTombstones {
		service.mu.Unlock()
		return ControlCredentialReceipt{}, errors.New("control credential retirement capacity is exhausted")
	}
	revocation := &controlLeaseRevocation{
		done: make(chan struct{}), expiresAt: authorityState.expiresAt,
	}
	service.controlRevocations[leaseID] = revocation
	service.retiringControlLeases[leaseID] = true
	if authorityState.completed {
		revocation.receipt = authorityState.receipt
		revocation.completed = true
		service.retireControlTURNCredentialLocked(leaseID)
		close(revocation.done)
		service.mu.Unlock()
		return revocation.receipt, nil
	}
	service.mu.Unlock()

	var receipt ControlCredentialReceipt
	replayed, completed, err := service.controlCredentials.beginRevocation(leaseID)
	if err == nil && completed {
		receipt = replayed
	}
	if err == nil && !completed {
		containmentErr := service.revokeControlLeaseAttemptsAndWait(leaseID)
		requestErr := service.controlCredentials.waitForRevocationRequests(leaseID)
		err = errors.Join(containmentErr, requestErr)
	}
	if err == nil && !completed {
		receipt, err = service.controlCredentials.finishRevocation(leaseID)
	}
	service.completeControlLeaseRetirement(leaseID, revocation, receipt, err, true)
	return receipt, err
}

func (service *Service) completeControlLeaseRetirement(
	leaseID string,
	retirement *controlLeaseRevocation,
	receipt ControlCredentialReceipt,
	err error,
	retainFailure bool,
) {
	expiresAt := retirement.expiresAt
	if authorityState, exists := service.controlCredentials.retirementState(leaseID); exists {
		expiresAt = authorityState.expiresAt
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.controlRevocations[leaseID] != retirement || retirement.completed {
		panic("control credential retirement lost its exact registry owner")
	}
	retirement.receipt = receipt
	retirement.err = err
	retirement.expiresAt = expiresAt
	retirement.completed = true
	if err == nil {
		service.retireControlTURNCredentialLocked(leaseID)
	}
	close(retirement.done)
	if err != nil && !retainFailure {
		delete(service.controlRevocations, leaseID)
		delete(service.retiringControlLeases, leaseID)
	}
}

func (service *Service) pruneControlRetirementsLocked(now time.Time) {
	for leaseID, retirement := range service.controlRevocations {
		if retirement.completed && !now.Before(retirement.expiresAt) {
			delete(service.controlRevocations, leaseID)
			delete(service.retiringControlLeases, leaseID)
		}
	}
}

func (service *Service) controlLeaseRetiringLocked(leaseID string) bool {
	service.pruneControlRetirementsLocked(service.clock.Now().UTC())
	return service.retiringControlLeases[leaseID]
}

func (service *Service) controlLeaseOwnsAttemptLocked(leaseID string) bool {
	for _, ownerLeaseID := range service.requestControlLeases {
		if ownerLeaseID == leaseID {
			return true
		}
	}
	return false
}

// revokeControlLeaseAttemptsAndWait closes the registry race in both directions:
// a startup sees the retirement marker before publication, while revocation waits
// for the request's exact startup/reap signal before it can return a receipt.
func (service *Service) revokeControlLeaseAttemptsAndWait(leaseID string) error {
	for {
		var cancellations []context.CancelFunc
		startups := make(map[<-chan struct{}]struct{})
		reaping := make(map[*leasedAttempt]bool)

		service.mu.Lock()
		owned := service.controlLeaseOwnsAttemptLocked(leaseID)
		for requestID, ownerLeaseID := range service.requestControlLeases {
			if ownerLeaseID != leaseID {
				continue
			}
			if cancel := service.requestCancels[requestID]; cancel != nil {
				cancellations = append(cancellations, cancel)
			}
			if startup := service.requestStarts[requestID]; startup != nil {
				startups[startup] = struct{}{}
			}
		}
		for attemptID, entry := range service.active {
			if entry.controlLeaseID != leaseID {
				continue
			}
			if transitioned, owner := service.transitionToRetiringLocked(attemptID, entry); transitioned != nil {
				reaping[transitioned] = owner
			}
		}
		for _, entry := range service.retiring {
			if entry.controlLeaseID == leaseID {
				if _, exists := reaping[entry]; !exists {
					reaping[entry] = false
				}
			}
		}
		service.mu.Unlock()

		for _, cancel := range cancellations {
			cancel()
		}
		for entry, owner := range reaping {
			if owner {
				go service.reap(entry)
			}
		}
		for startup := range startups {
			<-startup
		}
		for entry := range reaping {
			<-entry.reaped
		}
		if !owned {
			break
		}
	}

	service.mu.Lock()
	defer service.mu.Unlock()
	if service.controlLeaseOwnsAttemptLocked(leaseID) ||
		len(service.containmentFailures) != 0 {
		return errors.New("control credential attempt containment failed")
	}
	return nil
}
