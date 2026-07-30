package browsermatrixpion

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

func (service *Service) handleAttempts(
	w http.ResponseWriter,
	request *http.Request,
	authorization requestAuthorization,
) {
	if request.Method != http.MethodPost {
		writeProtocolError(w, http.StatusMethodNotAllowed, "method-not-allowed")
		return
	}
	var input CreateAttemptRequest
	if service.decodeBody(w, request, &input) != nil {
		writeProtocolError(w, http.StatusBadRequest, "invalid-attempt")
		return
	}
	lease, err := validateCreateRequest(input, service.maximumLease)
	if err != nil {
		writeProtocolError(w, http.StatusBadRequest, "invalid-attempt")
		return
	}
	requestAuthority := input.RequestAuthority
	binding := AttemptBinding{
		ControlAuthority: requestAuthority.ControlAuthority,
		FixtureBinding:   requestAuthority.FixtureBinding,
	}
	requestID := requestAuthority.RequestID
	authority, authorized := service.authorityLeaseForBinding(binding)
	admittedAt := service.clock.Now().UTC()
	leaseIssuedAt := admittedAt.Truncate(time.Millisecond)
	requestedLeaseDeadline := leaseIssuedAt.Add(lease)
	controlLeaseID := authority.controlLeaseID
	if !authorized || controlLeaseID == "" || !authorization.owns(controlLeaseID) ||
		!service.controlCredentials.leaseActive(controlLeaseID) ||
		leaseIssuedAt.Before(authority.issuedAt) ||
		requestedLeaseDeadline.After(authority.expiresAt) || !requestedLeaseDeadline.After(admittedAt) {
		emitTrace(service.trace, TraceEvent{
			Milestone: traceAttemptStarting, InstanceID: service.instanceID,
			RunID:             requestAuthority.ControlAuthority.SampleAuthority.RunID,
			AttestationSHA256: requestAuthority.FixtureBinding.AttestationSHA256,
			RequestID:         requestID, Outcome: "authority-lease-rejected",
		})
		writeProtocolError(w, http.StatusConflict, "authority-binding-mismatch")
		return
	}
	if authorization.dynamic {
		if err := service.controlCredentials.claimAttempt(controlLeaseID, requestID); err != nil {
			writeProtocolError(w, http.StatusConflict, "attempt-authority-consumed")
			return
		}
	}
	reservation, startup, status := service.reserveAttempt(
		requestID,
		lease,
		binding,
		controlLeaseID,
	)
	if status != 0 {
		writeProtocolError(w, status, "attempt-admission-rejected")
		return
	}
	if reservation.attemptID == "" {
		service.replayCreateAttempt(
			w,
			request,
			requestID,
			lease,
			binding,
			controlLeaseID,
			startup,
		)
		return
	}
	emitTrace(service.trace, TraceEvent{
		Milestone: traceAttemptStarting, InstanceID: service.instanceID,
		RunID:             reservation.binding.ControlAuthority.SampleAuthority.RunID,
		AttestationSHA256: reservation.binding.FixtureBinding.AttestationSHA256,
		RequestID:         reservation.requestID, AttemptID: reservation.attemptID,
	})
	startupDeadline := service.clock.Now().UTC().Add(service.attemptStartTimeout)
	if authority.expiresAt.Before(startupDeadline) {
		startupDeadline = authority.expiresAt
	}
	if requestedLeaseDeadline.Before(startupDeadline) {
		startupDeadline = requestedLeaseDeadline
	}
	ctx, cancel := context.WithDeadline(request.Context(), startupDeadline)
	stop := context.AfterFunc(service.lifecycleContext, cancel)
	if !service.registerStartupCancellation(reservation, cancel) {
		stop()
		cancel()
		service.releaseFailedReservation(reservation, nil)
		writeProtocolError(w, http.StatusConflict, "attempt-rejected")
		return
	}
	attemptAuthority := attemptAuthorityFromParts(
		reservation.binding,
		reservation.requestID,
		reservation.attemptID,
		reservation.challenge,
	)
	attempt, createErr := service.attemptFactory.Create(ctx, attemptAuthority)
	createDeadlineErr := ctx.Err()
	service.unregisterStartupCancellation(reservation)
	stop()
	cancel()
	if createErr != nil || createDeadlineErr != nil || attempt == nil {
		var cleanupErr error
		if attempt != nil {
			cleanupErr = attempt.Close()
		}
		service.releaseFailedReservation(reservation, cleanupErr)
		writeProtocolError(w, http.StatusInternalServerError, "attempt-start-failed")
		return
	}
	now := service.clock.Now().UTC()
	expiresAt := requestedLeaseDeadline
	if authority.expiresAt.Before(expiresAt) {
		expiresAt = authority.expiresAt
	}
	leaseContext, leaseCancel := context.WithDeadline(service.lifecycleContext, expiresAt)
	entry := &leasedAttempt{
		attempt: attempt, attemptID: reservation.attemptID, requestID: reservation.requestID,
		challenge: reservation.challenge, binding: reservation.binding,
		controlLeaseID: reservation.controlLeaseID, leaseLength: lease,
		leaseIssuedAt: leaseIssuedAt, expiresAt: expiresAt,
		authorityIssuedAt: authority.issuedAt, authorityExpiresAt: authority.expiresAt,
		controlExpiresAt: authority.controlExpiresAt,
		lease:            leaseContext, leaseCancel: leaseCancel,
		state: attemptActive, reaped: make(chan struct{}),
	}
	service.mu.Lock()
	service.starting--
	delete(service.attemptReservations, entry.attemptID)
	if service.closed || len(service.containmentFailures) != 0 ||
		service.controlLeaseRetiringLocked(entry.controlLeaseID) || !now.Before(expiresAt) {
		service.retiring[entry.attemptID] = entry
		entry.state = attemptRetiring
		service.condition.Broadcast()
		service.mu.Unlock()
		service.reap(entry)
		writeProtocolError(w, http.StatusConflict, "attempt-rejected")
		return
	}
	service.active[entry.attemptID] = entry
	service.condition.Broadcast()
	service.mu.Unlock()
	emitTrace(service.trace, TraceEvent{
		Milestone: traceAttemptActive, InstanceID: service.instanceID,
		RunID:             entry.binding.ControlAuthority.SampleAuthority.RunID,
		AttestationSHA256: entry.binding.FixtureBinding.AttestationSHA256,
		RequestID:         entry.requestID, AttemptID: entry.attemptID,
	})

	timer := service.clock.AfterFunc(expiresAt.Sub(service.clock.Now().UTC()), func() { service.expireAttempt(entry) })
	service.mu.Lock()
	if timer == nil {
		transitioned, owner := service.transitionToRetiringLocked(entry.attemptID, entry)
		service.mu.Unlock()
		if owner {
			service.reap(transitioned)
		}
		writeProtocolError(w, http.StatusInternalServerError, "attempt-lease-failed")
		return
	}
	activeAfterTimer := entry.state == attemptActive
	if activeAfterTimer {
		entry.timer = timer
		service.signalStartupLocked(entry.requestID)
	} else {
		timer.Stop()
	}
	service.mu.Unlock()
	if !activeAfterTimer {
		<-entry.reaped
		if entry.retireErr != nil {
			writeProtocolError(w, http.StatusInternalServerError, "attempt-containment-failed")
		} else {
			writeProtocolError(w, http.StatusConflict, "attempt-rejected")
		}
		return
	}

	writeJSON(w, http.StatusCreated, CreateAttemptResponse{
		ProtocolVersion: ProtocolVersion, AttemptAuthority: attemptAuthority,
		LeaseIssuedAt:  entry.leaseIssuedAt.Format(canonicalTimestampLayout),
		LeaseExpiresAt: entry.expiresAt.Format(canonicalTimestampLayout),
		LeaseMillis:    entry.leaseLength.Milliseconds(),
	})
}

func (service *Service) replayCreateAttempt(
	w http.ResponseWriter,
	request *http.Request,
	requestID string,
	lease time.Duration,
	binding AttemptBinding,
	controlLeaseID string,
	startup <-chan struct{},
) {
	if startup != nil {
		select {
		case <-startup:
		case <-request.Context().Done():
			writeProtocolError(w, http.StatusRequestTimeout, "attempt-replay-canceled")
			return
		case <-service.lifecycleContext.Done():
			writeProtocolError(w, http.StatusConflict, "attempt-rejected")
			return
		}
	}
	service.mu.Lock()
	attemptID := service.requestOwners[requestID]
	entry := service.active[attemptID]
	if entry == nil || entry.requestID != requestID || entry.leaseLength != lease ||
		entry.binding != binding || entry.controlLeaseID != controlLeaseID ||
		service.controlLeaseRetiringLocked(controlLeaseID) ||
		!service.clock.Now().UTC().Before(entry.expiresAt) {
		service.mu.Unlock()
		writeProtocolError(w, http.StatusConflict, "attempt-admission-rejected")
		return
	}
	response := CreateAttemptResponse{
		ProtocolVersion: ProtocolVersion,
		AttemptAuthority: attemptAuthorityFromParts(
			entry.binding, entry.requestID, entry.attemptID, entry.challenge,
		),
		LeaseIssuedAt:  entry.leaseIssuedAt.Format(canonicalTimestampLayout),
		LeaseExpiresAt: entry.expiresAt.Format(canonicalTimestampLayout),
		LeaseMillis:    entry.leaseLength.Milliseconds(),
	}
	service.mu.Unlock()
	writeJSON(w, http.StatusOK, response)
}

func (service *Service) handleAttempt(
	w http.ResponseWriter,
	request *http.Request,
	authorization requestAuthorization,
) {
	remainder := strings.TrimPrefix(request.URL.Path, attemptsPath+"/")
	parts := strings.Split(remainder, "/")
	if len(parts) == 2 && parts[1] == "offer" {
		service.handleOffer(w, request, parts[0], authorization)
		return
	}
	if len(parts) != 1 || !validOpaqueID(parts[0]) {
		writeProtocolError(w, http.StatusNotFound, "not-found")
		return
	}
	switch request.Method {
	case http.MethodGet:
		entry, tombstone := service.attemptForRead(parts[0], authorization)
		if tombstone != nil {
			result, replayable := terminalAttemptResultFromTombstone(*tombstone)
			if !replayable {
				writeProtocolError(w, http.StatusNotFound, "attempt-not-found")
				return
			}
			writeJSON(w, http.StatusOK, result)
			return
		}
		if entry == nil {
			writeProtocolError(w, http.StatusNotFound, "attempt-not-found")
			return
		}
		if !service.clock.Now().UTC().Before(entry.expiresAt) {
			service.retireRejectedAttempt(parts[0])
			writeProtocolError(w, http.StatusNotFound, "attempt-not-found")
			return
		}
		entry.operation.Lock()
		now := service.clock.Now().UTC()
		if service.activeAttempt(parts[0]) != entry || !now.Before(entry.expiresAt) {
			entry.operation.Unlock()
			service.retireRejectedAttempt(parts[0])
			writeProtocolError(w, http.StatusNotFound, "attempt-not-found")
			return
		}
		result := entry.attempt.Result()
		result.ProtocolVersion = ProtocolVersion
		result.AttemptAuthority = attemptAuthorityFromParts(
			entry.binding, entry.requestID, entry.attemptID, entry.challenge,
		)
		result.ChallengeBindingSHA256 = challengeBindingSHA256(result.AttemptAuthority)
		result.TerminalReceipt = nil
		receiptIssued := false
		if entry.terminalReceipt != nil {
			applyAttemptTerminalReceipt(&result, *entry.terminalReceipt)
		} else if result.State == attemptStateEstablished || result.State == attemptStateFailed {
			signed, err := service.signAttemptTerminalResult(entry, result, now)
			if err != nil {
				entry.operation.Unlock()
				service.rejectInvalidAttemptResult(entry)
				writeProtocolError(w, http.StatusInternalServerError, "attempt-result-invalid")
				return
			}
			entry.terminalReceipt = &signed
			applyAttemptTerminalReceipt(&result, signed)
			receiptIssued = true
		} else if validatePendingAttemptResult(result) != nil {
			entry.operation.Unlock()
			service.rejectInvalidAttemptResult(entry)
			writeProtocolError(w, http.StatusInternalServerError, "attempt-result-invalid")
			return
		}
		entry.operation.Unlock()
		if receiptIssued {
			emitTrace(service.trace, TraceEvent{
				Milestone: traceTerminalReceipt, InstanceID: service.instanceID,
				RunID:             entry.binding.ControlAuthority.SampleAuthority.RunID,
				AttestationSHA256: entry.binding.FixtureBinding.AttestationSHA256,
				RequestID:         entry.requestID, AttemptID: entry.attemptID, Outcome: result.State,
			})
		}
		writeJSON(w, http.StatusOK, result)
	case http.MethodDelete:
		service.deleteAttempt(w, parts[0], authorization)
	default:
		writeProtocolError(w, http.StatusMethodNotAllowed, "method-not-allowed")
	}
}

func (service *Service) rejectInvalidAttemptResult(entry *leasedAttempt) {
	emitTrace(service.trace, TraceEvent{
		Milestone: traceTerminalReceipt, InstanceID: service.instanceID,
		RunID:             entry.binding.ControlAuthority.SampleAuthority.RunID,
		AttestationSHA256: entry.binding.FixtureBinding.AttestationSHA256,
		RequestID:         entry.requestID, AttemptID: entry.attemptID, Outcome: "rejected",
	})
	service.retireRejectedAttempt(entry.attemptID)
}

func validatePendingAttemptResult(result AttemptResult) error {
	if result.State != attemptStatePending || result.FailureCode != nil {
		return errors.New("pending attempt result is invalid")
	}
	if result.SelectedPair != nil {
		return validateSelectedPairEvidence(*result.SelectedPair)
	}
	return nil
}

func (service *Service) signAttemptTerminalResult(
	entry *leasedAttempt,
	result AttemptResult,
	now time.Time,
) (SignedAttemptTerminalReceipt, error) {
	terminalAt := now.UTC().Truncate(time.Millisecond)
	if terminalAt.Before(entry.authorityIssuedAt) || !terminalAt.Before(entry.authorityExpiresAt) {
		return SignedAttemptTerminalReceipt{}, errors.New("attempt terminal time crossed its attestation lease")
	}
	receipt := AttemptTerminalReceipt{
		ProtocolVersion: ProtocolVersion,
		AttemptAuthority: attemptAuthorityFromParts(
			entry.binding, entry.requestID, entry.attemptID, entry.challenge,
		),
		TerminalAt:             terminalAt.Format(canonicalTimestampLayout),
		AttemptLeaseIssuedAt:   entry.leaseIssuedAt.Format(canonicalTimestampLayout),
		AttemptLeaseExpiresAt:  entry.expiresAt.Format(canonicalTimestampLayout),
		AttemptLeaseMillis:     entry.expiresAt.Sub(entry.leaseIssuedAt).Milliseconds(),
		State:                  result.State,
		SelectedPair:           result.SelectedPair,
		ChallengeBindingSHA256: result.ChallengeBindingSHA256,
		FailureCode:            result.FailureCode,
	}
	return service.controlCredentials.signAttemptTerminalReceipt(receipt)
}

func applyAttemptTerminalReceipt(
	result *AttemptResult,
	signed SignedAttemptTerminalReceipt,
) {
	receipt := cloneAttemptTerminalReceipt(signed.Receipt)
	result.State = receipt.State
	result.SelectedPair = receipt.SelectedPair
	result.ChallengeBindingSHA256 = receipt.ChallengeBindingSHA256
	result.FailureCode = receipt.FailureCode
	signed = cloneSignedAttemptTerminalReceipt(signed)
	result.TerminalReceipt = &signed
}

func terminalAttemptResultFromTombstone(
	tombstone attemptTombstone,
) (AttemptResult, bool) {
	if tombstone.terminalReceipt == nil {
		return AttemptResult{}, false
	}
	result := AttemptResult{
		ProtocolVersion: ProtocolVersion,
		AttemptAuthority: attemptAuthorityFromParts(
			tombstone.binding, tombstone.requestID, tombstone.attemptID, tombstone.challenge,
		),
	}
	applyAttemptTerminalReceipt(&result, *tombstone.terminalReceipt)
	return result, true
}

func (service *Service) handleOffer(
	w http.ResponseWriter,
	request *http.Request,
	attemptID string,
	authorization requestAuthorization,
) {
	if request.Method != http.MethodPost || !validOpaqueID(attemptID) {
		writeProtocolError(w, http.StatusMethodNotAllowed, "method-not-allowed")
		return
	}
	entry := service.activeAttempt(attemptID)
	if entry == nil || !authorization.owns(entry.controlLeaseID) {
		writeProtocolError(w, http.StatusNotFound, "attempt-not-found")
		return
	}
	var input OfferRequest
	if service.decodeBody(w, request, &input) != nil {
		service.retireRejectedAttempt(attemptID)
		writeProtocolError(w, http.StatusBadRequest, "invalid-offer")
		return
	}
	attemptAuthority := attemptAuthorityFromParts(
		entry.binding, entry.requestID, entry.attemptID, entry.challenge,
	)
	if validateOfferRequest(input, attemptAuthority) != nil {
		service.retireRejectedAttempt(attemptID)
		writeProtocolError(w, http.StatusBadRequest, "invalid-offer")
		return
	}
	entry.operation.Lock()
	if service.activeAttempt(attemptID) != entry || !service.clock.Now().Before(entry.expiresAt) {
		entry.operation.Unlock()
		service.retireRejectedAttempt(attemptID)
		writeProtocolError(w, http.StatusNotFound, "attempt-not-found")
		return
	}
	deadline := service.clock.Now().Add(service.offerTimeout)
	if entry.expiresAt.Before(deadline) {
		deadline = entry.expiresAt
	}
	ctx, cancel := context.WithDeadline(request.Context(), deadline)
	stopLease := context.AfterFunc(entry.lease, cancel)
	answer, offerErr, closeErr, preclosed := offerWithForcedDeadline(ctx, entry.attempt, input.SDP)
	stopLease()
	cancel()
	if closeErr != nil {
		entry.precloseErr = errors.New("remote Pion forced offer containment failed")
	}
	entry.preclosed = preclosed
	entry.operation.Unlock()
	offerOutcome := "accepted"
	if offerErr != nil {
		offerOutcome = "rejected"
	}
	emitTrace(service.trace, TraceEvent{
		Milestone: traceOfferTerminal, InstanceID: service.instanceID,
		RunID:             entry.binding.ControlAuthority.SampleAuthority.RunID,
		AttestationSHA256: entry.binding.FixtureBinding.AttestationSHA256,
		RequestID:         entry.requestID, AttemptID: entry.attemptID, Outcome: offerOutcome,
	})
	if offerErr != nil {
		service.retireRejectedAttempt(attemptID)
		writeProtocolError(w, http.StatusUnprocessableEntity, "offer-rejected")
		return
	}
	writeJSON(w, http.StatusOK, OfferResponse{
		ProtocolVersion: ProtocolVersion, AttemptAuthority: attemptAuthority,
		Type: "answer", SDP: answer,
	})
}

func (service *Service) deleteAttempt(
	w http.ResponseWriter,
	attemptID string,
	authorization requestAuthorization,
) {
	entry, owner, tombstone := service.transitionOrObserveAuthorized(attemptID, authorization)
	if tombstone != nil {
		service.writeCleanupReceipt(w, *tombstone)
		return
	}
	if entry == nil {
		writeProtocolError(w, http.StatusNotFound, "attempt-not-found")
		return
	}
	if owner {
		service.reap(entry)
	} else {
		<-entry.reaped
	}
	service.writeCleanupReceipt(w, attemptTombstone{
		attemptID: attemptID, requestID: entry.requestID, challenge: entry.challenge,
		controlLeaseID: entry.controlLeaseID,
		terminal:       "reaped", err: entry.retireErr, binding: entry.binding,
	})
}

func (service *Service) writeCleanupReceipt(w http.ResponseWriter, tombstone attemptTombstone) {
	if tombstone.err != nil {
		writeProtocolError(w, http.StatusInternalServerError, "attempt-containment-failed")
		return
	}
	writeJSON(w, http.StatusOK, CleanupReceipt{
		ProtocolVersion: ProtocolVersion,
		AttemptAuthority: attemptAuthorityFromParts(
			tombstone.binding, tombstone.requestID, tombstone.attemptID, tombstone.challenge,
		),
		Terminal: "reaped",
	})
}

type offerResult struct {
	answer string
	err    error
}

func offerWithForcedDeadline(
	ctx context.Context,
	attempt Attempt,
	sdp string,
) (string, error, error, bool) {
	result := make(chan offerResult, 1)
	go func() {
		answer, err := attempt.Offer(ctx, sdp)
		result <- offerResult{answer: answer, err: err}
	}()
	select {
	case completed := <-result:
		return completed.answer, completed.err, nil, false
	case <-ctx.Done():
		closeErr := attempt.Close()
		<-result
		return "", errors.New("remote Pion offer deadline exceeded"), closeErr, true
	}
}
