package browsermatrixpion

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"time"
)

func (service *Service) reserveAttempt(
	requestID string,
	lease time.Duration,
	binding AttemptBinding,
	controlLeaseID string,
) (attemptReservation, <-chan struct{}, int) {
	service.mu.Lock()
	service.pruneTombstonesLocked(service.clock.Now())
	if service.closed || len(service.containmentFailures) != 0 ||
		service.controlLeaseRetiringLocked(controlLeaseID) {
		service.mu.Unlock()
		return attemptReservation{}, nil, http.StatusServiceUnavailable
	}
	if _, exists := service.requestOwners[requestID]; exists {
		if service.requestLeases[requestID] == lease &&
			service.requestBindings[requestID] == binding &&
			service.requestControlLeases[requestID] == controlLeaseID {
			startup := service.requestStarts[requestID]
			service.mu.Unlock()
			return attemptReservation{}, startup, 0
		}
		service.mu.Unlock()
		return attemptReservation{}, nil, http.StatusConflict
	}
	if _, exists := service.requestTombstones[requestID]; exists {
		service.mu.Unlock()
		return attemptReservation{}, nil, http.StatusConflict
	}
	if service.occupied >= service.maximumActive ||
		len(service.tombstones)+service.occupied >= service.maximumTombstones {
		service.mu.Unlock()
		return attemptReservation{}, nil, http.StatusTooManyRequests
	}
	service.occupied++
	service.starting++
	// The request claim precedes entropy and factory work so duplicate calls and
	// capacity storms cannot manufacture unowned Pion state.
	service.requestOwners[requestID] = ""
	service.requestLeases[requestID] = lease
	service.requestBindings[requestID] = binding
	service.requestControlLeases[requestID] = controlLeaseID
	service.requestStarts[requestID] = make(chan struct{})
	service.mu.Unlock()

	attemptID, err := service.newAttemptID()
	if err != nil {
		service.releaseUnidentifiedReservation(requestID)
		return attemptReservation{}, nil, http.StatusInternalServerError
	}
	service.mu.Lock()
	if service.closed || len(service.containmentFailures) != 0 ||
		service.controlLeaseRetiringLocked(controlLeaseID) {
		service.releaseUnidentifiedReservationLocked(requestID)
		service.mu.Unlock()
		return attemptReservation{}, nil, http.StatusServiceUnavailable
	}
	_, collidesWithRequest := service.requestOwners[attemptID]
	_, collidesWithRetiredRequest := service.requestTombstones[attemptID]
	if attemptID == requestID || collidesWithRequest || collidesWithRetiredRequest ||
		service.attemptReservations[attemptID] != "" || service.active[attemptID] != nil || service.retiring[attemptID] != nil ||
		service.tombstones[attemptID].attemptID != "" {
		service.releaseUnidentifiedReservationLocked(requestID)
		service.mu.Unlock()
		return attemptReservation{}, nil, http.StatusInternalServerError
	}
	service.requestOwners[requestID] = attemptID
	service.attemptReservations[attemptID] = requestID
	service.mu.Unlock()

	challenge, err := service.newAttemptChallenge()
	if err != nil {
		service.releaseIdentifiedReservation(requestID, attemptID)
		return attemptReservation{}, nil, http.StatusInternalServerError
	}
	service.mu.Lock()
	if service.closed || len(service.containmentFailures) != 0 ||
		service.controlLeaseRetiringLocked(controlLeaseID) {
		service.releaseIdentifiedReservationLocked(requestID, attemptID)
		service.mu.Unlock()
		return attemptReservation{}, nil, http.StatusServiceUnavailable
	}
	service.mu.Unlock()
	emitTrace(service.trace, TraceEvent{
		Milestone: traceAttemptReserved, InstanceID: service.instanceID,
		RunID:             binding.ControlAuthority.SampleAuthority.RunID,
		AttestationSHA256: binding.FixtureBinding.AttestationSHA256,
		RequestID:         requestID, AttemptID: attemptID,
	})
	return attemptReservation{
		attemptID: attemptID, requestID: requestID, challenge: challenge, binding: binding,
		controlLeaseID: controlLeaseID,
	}, nil, 0
}

func (service *Service) registerStartupCancellation(
	reservation attemptReservation,
	cancel context.CancelFunc,
) bool {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.closed || len(service.containmentFailures) != 0 ||
		service.controlLeaseRetiringLocked(reservation.controlLeaseID) ||
		service.requestOwners[reservation.requestID] != reservation.attemptID {
		return false
	}
	service.requestCancels[reservation.requestID] = cancel
	return true
}

func (service *Service) unregisterStartupCancellation(reservation attemptReservation) {
	service.mu.Lock()
	delete(service.requestCancels, reservation.requestID)
	service.mu.Unlock()
}

func (service *Service) releaseUnidentifiedReservation(requestID string) {
	service.mu.Lock()
	service.releaseUnidentifiedReservationLocked(requestID)
	service.mu.Unlock()
}

func (service *Service) releaseUnidentifiedReservationLocked(requestID string) {
	service.starting--
	service.occupied--
	delete(service.requestOwners, requestID)
	delete(service.requestLeases, requestID)
	delete(service.requestBindings, requestID)
	delete(service.requestControlLeases, requestID)
	delete(service.requestCancels, requestID)
	service.signalAndDeleteStartupLocked(requestID)
	service.condition.Broadcast()
}

func (service *Service) releaseIdentifiedReservation(requestID, attemptID string) {
	service.mu.Lock()
	service.releaseIdentifiedReservationLocked(requestID, attemptID)
	service.mu.Unlock()
}

func (service *Service) releaseIdentifiedReservationLocked(requestID, attemptID string) {
	delete(service.attemptReservations, attemptID)
	service.releaseUnidentifiedReservationLocked(requestID)
}

func (service *Service) releaseFailedReservation(
	reservation attemptReservation,
	cleanupErr error,
) {
	service.mu.Lock()
	service.starting--
	service.occupied--
	delete(service.attemptReservations, reservation.attemptID)
	delete(service.requestOwners, reservation.requestID)
	delete(service.requestLeases, reservation.requestID)
	delete(service.requestBindings, reservation.requestID)
	delete(service.requestControlLeases, reservation.requestID)
	delete(service.requestCancels, reservation.requestID)
	service.signalAndDeleteStartupLocked(reservation.requestID)
	var containmentOwners []*leasedAttempt
	if cleanupErr != nil {
		service.containmentFailures = append(
			service.containmentFailures,
			errors.New("remote Pion attempt containment failed"),
		)
		service.cancelLifecycle()
		containmentOwners = service.containAllActiveLocked()
	}
	service.condition.Broadcast()
	service.mu.Unlock()
	if cleanupErr != nil {
		emitTrace(service.trace, TraceEvent{
			Milestone: traceContainmentFailed, InstanceID: service.instanceID,
			RunID:             reservation.binding.ControlAuthority.SampleAuthority.RunID,
			AttestationSHA256: reservation.binding.FixtureBinding.AttestationSHA256,
			RequestID:         reservation.requestID, AttemptID: reservation.attemptID,
			Outcome: "startup-cleanup-failed",
		})
	}
	for _, entry := range containmentOwners {
		go service.reap(entry)
	}
}

func (service *Service) retireRejectedAttempt(attemptID string) {
	entry, owner, _ := service.transitionOrObserve(attemptID)
	if entry == nil {
		return
	}
	if owner {
		service.reap(entry)
	} else {
		<-entry.reaped
	}
}

func (service *Service) expireAttempt(expected *leasedAttempt) {
	service.mu.Lock()
	entry, owner := service.transitionToRetiringLocked(expected.attemptID, expected)
	service.mu.Unlock()
	if owner {
		service.reap(entry)
	}
}

func (service *Service) transitionOrObserve(
	attemptID string,
) (*leasedAttempt, bool, *attemptTombstone) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.pruneTombstonesLocked(service.clock.Now())
	if tombstone, found := service.tombstones[attemptID]; found {
		return nil, false, &tombstone
	}
	if entry := service.retiring[attemptID]; entry != nil {
		return entry, false, nil
	}
	entry, owner := service.transitionToRetiringLocked(attemptID, nil)
	return entry, owner, nil
}

func (service *Service) transitionOrObserveAuthorized(
	attemptID string,
	authorization requestAuthorization,
) (*leasedAttempt, bool, *attemptTombstone) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.pruneTombstonesLocked(service.clock.Now())
	if tombstone, found := service.tombstones[attemptID]; found {
		if !authorization.owns(tombstone.controlLeaseID) {
			return nil, false, nil
		}
		return nil, false, &tombstone
	}
	if entry := service.retiring[attemptID]; entry != nil {
		if !authorization.owns(entry.controlLeaseID) {
			return nil, false, nil
		}
		return entry, false, nil
	}
	entry := service.active[attemptID]
	if entry == nil || !authorization.owns(entry.controlLeaseID) {
		return nil, false, nil
	}
	transitioned, owner := service.transitionToRetiringLocked(attemptID, entry)
	return transitioned, owner, nil
}

func (service *Service) transitionToRetiringLocked(
	attemptID string,
	expected *leasedAttempt,
) (*leasedAttempt, bool) {
	entry := service.active[attemptID]
	if entry == nil || expected != nil && entry != expected {
		if retiring := service.retiring[attemptID]; retiring != nil {
			return retiring, false
		}
		return nil, false
	}
	delete(service.active, attemptID)
	entry.state = attemptRetiring
	entry.leaseCancel()
	if entry.timer != nil {
		entry.timer.Stop()
	}
	service.retiring[attemptID] = entry
	return entry, true
}

func (service *Service) reap(entry *leasedAttempt) {
	emitTrace(service.trace, TraceEvent{
		Milestone: traceAttemptRetiring, InstanceID: service.instanceID,
		RunID:             entry.binding.ControlAuthority.SampleAuthority.RunID,
		AttestationSHA256: entry.binding.FixtureBinding.AttestationSHA256,
		RequestID:         entry.requestID, AttemptID: entry.attemptID,
	})
	entry.operation.Lock()
	var closeErr error
	if !entry.preclosed {
		closeErr = entry.attempt.Close()
	}
	var terminalReceipt *SignedAttemptTerminalReceipt
	if entry.terminalReceipt != nil {
		cloned := cloneSignedAttemptTerminalReceipt(*entry.terminalReceipt)
		terminalReceipt = &cloned
	}
	entry.operation.Unlock()
	entry.retireErr = errors.Join(entry.precloseErr, closeErr)

	service.mu.Lock()
	if service.retiring[entry.attemptID] != entry {
		service.mu.Unlock()
		panic("remote Pion retirement lost its exact registry owner")
	}
	delete(service.retiring, entry.attemptID)
	entry.state = attemptReaped
	service.occupied--
	delete(service.attemptReservations, entry.attemptID)
	delete(service.requestOwners, entry.requestID)
	delete(service.requestLeases, entry.requestID)
	delete(service.requestBindings, entry.requestID)
	delete(service.requestControlLeases, entry.requestID)
	delete(service.requestCancels, entry.requestID)
	service.signalAndDeleteStartupLocked(entry.requestID)
	now := service.clock.Now().UTC()
	tombstone := attemptTombstone{
		attemptID: entry.attemptID, requestID: entry.requestID, challenge: entry.challenge,
		controlLeaseID: entry.controlLeaseID,
		expiresAt:      service.attemptTombstoneExpiresAt(entry, now), terminal: "reaped",
		err: entry.retireErr, binding: entry.binding, terminalReceipt: terminalReceipt,
	}
	service.addTombstoneLocked(tombstone)
	var containmentOwners []*leasedAttempt
	if entry.retireErr != nil {
		service.containmentFailures = append(
			service.containmentFailures,
			errors.New("remote Pion attempt containment failed"),
		)
		service.cancelLifecycle()
		containmentOwners = service.containAllActiveLocked()
	}
	close(entry.reaped)
	service.condition.Broadcast()
	service.mu.Unlock()
	outcome := "clean"
	if entry.retireErr != nil {
		outcome = "containment-failed"
	}
	emitTrace(service.trace, TraceEvent{
		Milestone: traceAttemptReaped, InstanceID: service.instanceID,
		RunID:             entry.binding.ControlAuthority.SampleAuthority.RunID,
		AttestationSHA256: entry.binding.FixtureBinding.AttestationSHA256,
		RequestID:         entry.requestID, AttemptID: entry.attemptID, Outcome: outcome,
	})
	if entry.retireErr != nil {
		emitTrace(service.trace, TraceEvent{
			Milestone: traceContainmentFailed, InstanceID: service.instanceID,
			RunID:             entry.binding.ControlAuthority.SampleAuthority.RunID,
			AttestationSHA256: entry.binding.FixtureBinding.AttestationSHA256,
			RequestID:         entry.requestID, AttemptID: entry.attemptID,
			Outcome: "retirement-failed",
		})
	}
	for _, owner := range containmentOwners {
		go service.reap(owner)
	}
}

func (service *Service) containAllActiveLocked() []*leasedAttempt {
	owners := make([]*leasedAttempt, 0, len(service.active))
	for attemptID, entry := range service.active {
		if transitioned, owner := service.transitionToRetiringLocked(attemptID, entry); owner {
			owners = append(owners, transitioned)
		}
	}
	return owners
}

func (service *Service) addTombstoneLocked(tombstone attemptTombstone) {
	service.tombstones[tombstone.attemptID] = tombstone
	service.requestTombstones[tombstone.requestID] = tombstone.attemptID
	service.tombstoneOrder = append(service.tombstoneOrder, tombstone.attemptID)
}

func (service *Service) attemptTombstoneExpiresAt(
	entry *leasedAttempt,
	now time.Time,
) time.Time {
	expiresAt := now.Add(service.tombstoneRetention)
	for _, governingExpiry := range []time.Time{
		entry.expiresAt,
		entry.authorityExpiresAt,
		entry.controlExpiresAt,
	} {
		if governingExpiry.After(expiresAt) {
			expiresAt = governingExpiry
		}
	}
	return expiresAt
}

func (service *Service) pruneTombstonesLocked(now time.Time) {
	kept := service.tombstoneOrder[:0]
	for _, attemptID := range service.tombstoneOrder {
		tombstone, found := service.tombstones[attemptID]
		if !found {
			continue
		}
		if !now.Before(tombstone.expiresAt) {
			delete(service.tombstones, attemptID)
			delete(service.requestTombstones, tombstone.requestID)
			continue
		}
		kept = append(kept, attemptID)
	}
	service.tombstoneOrder = kept
}

func (service *Service) activeAttempt(attemptID string) *leasedAttempt {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.active[attemptID]
}

func (service *Service) attemptForRead(
	attemptID string,
	authorization requestAuthorization,
) (*leasedAttempt, *attemptTombstone) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.pruneTombstonesLocked(service.clock.Now().UTC())
	if tombstone, exists := service.tombstones[attemptID]; exists {
		if !authorization.owns(tombstone.controlLeaseID) {
			return nil, nil
		}
		return nil, &tombstone
	}
	entry := service.active[attemptID]
	if entry == nil || !authorization.owns(entry.controlLeaseID) {
		return nil, nil
	}
	return entry, nil
}

func (service *Service) signalStartupLocked(requestID string) {
	startup, exists := service.requestStarts[requestID]
	if exists && startup != nil {
		close(startup)
		service.requestStarts[requestID] = nil
	}
}

func (service *Service) signalAndDeleteStartupLocked(requestID string) {
	service.signalStartupLocked(requestID)
	delete(service.requestStarts, requestID)
}

func (service *Service) newAttemptID() (string, error) {
	service.entropyMu.Lock()
	defer service.entropyMu.Unlock()
	identifier := make([]byte, attemptIdentifierBytes)
	if _, err := io.ReadFull(service.attemptIDSource, identifier); err != nil {
		return "", errors.New("remote Pion attempt ID source failed")
	}
	value := base64.RawURLEncoding.EncodeToString(identifier)
	if !validOpaqueID(value) {
		return "", errors.New("remote Pion attempt ID source is invalid")
	}
	return value, nil
}

func (service *Service) newAttemptChallenge() (string, error) {
	service.entropyMu.Lock()
	defer service.entropyMu.Unlock()
	challenge := make([]byte, attemptChallengeBytes)
	if _, err := io.ReadFull(service.challengeSource, challenge); err != nil {
		return "", errors.New("remote Pion challenge source failed")
	}
	challengeValue := base64.RawURLEncoding.EncodeToString(challenge)
	if !validOpaqueID(challengeValue) {
		return "", errors.New("remote Pion challenge source is invalid")
	}
	return challengeValue, nil
}
