package browsermatrixpion

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

type attemptAdmission struct {
	binding                AttemptBinding
	requestID              string
	lease                  time.Duration
	authority              authorityLease
	leaseIssuedAt          time.Time
	requestedLeaseDeadline time.Time
}

type startedAttempt struct {
	entry                 *leasedAttempt
	authority             AttemptAuthority
	publicationObservedAt time.Time
}

type attemptCreationFailure struct {
	status int
	code   string
}

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
	admission, authorized := service.authorizeAttemptAdmission(input, lease, authorization)
	if !authorized {
		writeProtocolError(w, http.StatusConflict, "authority-binding-mismatch")
		return
	}
	if authorization.dynamic {
		if err := service.controlCredentials.claimAttempt(
			admission.authority.controlLeaseID,
			admission.requestID,
		); err != nil {
			writeProtocolError(w, http.StatusConflict, "attempt-authority-consumed")
			return
		}
	}
	reservation, startup, status := service.reserveAttempt(
		admission.requestID,
		admission.lease,
		admission.binding,
		admission.authority.controlLeaseID,
	)
	if status != 0 {
		writeProtocolError(w, status, "attempt-admission-rejected")
		return
	}
	if reservation.attemptID == "" {
		service.replayCreateAttempt(
			w,
			request,
			admission.requestID,
			admission.lease,
			admission.binding,
			admission.authority.controlLeaseID,
			startup,
		)
		return
	}
	started, failure := service.startReservedAttempt(request.Context(), reservation, admission)
	if failure.status != 0 {
		writeProtocolError(w, failure.status, failure.code)
		return
	}
	if failure = service.activateStartedAttempt(started); failure.status != 0 {
		writeProtocolError(w, failure.status, failure.code)
		return
	}
	writeJSON(w, http.StatusCreated, CreateAttemptResponse{
		ProtocolVersion: ProtocolVersion, AttemptAuthority: started.authority,
		LeaseIssuedAt:  started.entry.leaseIssuedAt.Format(canonicalTimestampLayout),
		LeaseExpiresAt: started.entry.expiresAt.Format(canonicalTimestampLayout),
		LeaseMillis:    started.entry.leaseLength.Milliseconds(),
	})
}

func (service *Service) authorizeAttemptAdmission(
	input CreateAttemptRequest,
	lease time.Duration,
	authorization requestAuthorization,
) (attemptAdmission, bool) {
	requestAuthority := input.RequestAuthority
	binding := AttemptBinding{
		ControlAuthority: requestAuthority.ControlAuthority,
		FixtureBinding:   requestAuthority.FixtureBinding,
	}
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
			RequestID:         requestAuthority.RequestID, Outcome: "authority-lease-rejected",
		})
		return attemptAdmission{}, false
	}
	return attemptAdmission{
		binding: binding, requestID: requestAuthority.RequestID, lease: lease,
		authority: authority, leaseIssuedAt: leaseIssuedAt,
		requestedLeaseDeadline: requestedLeaseDeadline,
	}, true
}

func (service *Service) startReservedAttempt(
	requestContext context.Context,
	reservation attemptReservation,
	admission attemptAdmission,
) (startedAttempt, attemptCreationFailure) {
	emitTrace(service.trace, TraceEvent{
		Milestone: traceAttemptStarting, InstanceID: service.instanceID,
		RunID:             reservation.binding.ControlAuthority.SampleAuthority.RunID,
		AttestationSHA256: reservation.binding.FixtureBinding.AttestationSHA256,
		RequestID:         reservation.requestID, AttemptID: reservation.attemptID,
	})
	startupDeadline := service.clock.Now().UTC().Add(service.attemptStartTimeout)
	if admission.authority.expiresAt.Before(startupDeadline) {
		startupDeadline = admission.authority.expiresAt
	}
	if admission.requestedLeaseDeadline.Before(startupDeadline) {
		startupDeadline = admission.requestedLeaseDeadline
	}
	ctx, cancel := context.WithDeadline(requestContext, startupDeadline)
	stop := context.AfterFunc(service.lifecycleContext, cancel)
	if !service.registerStartupCancellation(reservation, cancel) {
		stop()
		cancel()
		service.releaseFailedReservation(reservation, nil)
		return startedAttempt{}, attemptCreationFailure{
			status: http.StatusConflict,
			code:   "attempt-rejected",
		}
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
		return startedAttempt{}, attemptCreationFailure{
			status: http.StatusInternalServerError,
			code:   "attempt-start-failed",
		}
	}
	now := service.clock.Now().UTC()
	expiresAt := admission.requestedLeaseDeadline
	if admission.authority.expiresAt.Before(expiresAt) {
		expiresAt = admission.authority.expiresAt
	}
	leaseContext, leaseCancel := context.WithDeadline(service.lifecycleContext, expiresAt)
	entry := &leasedAttempt{
		attempt: attempt, attemptID: reservation.attemptID, requestID: reservation.requestID,
		challenge: reservation.challenge, binding: reservation.binding,
		controlLeaseID: reservation.controlLeaseID, leaseLength: admission.lease,
		leaseIssuedAt: admission.leaseIssuedAt, expiresAt: expiresAt,
		authorityIssuedAt: admission.authority.issuedAt, authorityExpiresAt: admission.authority.expiresAt,
		controlExpiresAt: admission.authority.controlExpiresAt,
		lease:            leaseContext, leaseCancel: leaseCancel,
		state: attemptActive, reaped: make(chan struct{}),
	}
	return startedAttempt{
		entry: entry, authority: attemptAuthority, publicationObservedAt: now,
	}, attemptCreationFailure{}
}

func (service *Service) activateStartedAttempt(started startedAttempt) attemptCreationFailure {
	entry := started.entry
	service.mu.Lock()
	service.starting--
	delete(service.attemptReservations, entry.attemptID)
	if service.closed || len(service.containmentFailures) != 0 ||
		service.controlLeaseRetiringLocked(entry.controlLeaseID) ||
		!started.publicationObservedAt.Before(entry.expiresAt) {
		service.retiring[entry.attemptID] = entry
		entry.state = attemptRetiring
		service.condition.Broadcast()
		service.mu.Unlock()
		service.reap(entry)
		return attemptCreationFailure{status: http.StatusConflict, code: "attempt-rejected"}
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

	timer := service.clock.AfterFunc(
		entry.expiresAt.Sub(service.clock.Now().UTC()),
		func() { service.expireAttempt(entry) },
	)
	service.mu.Lock()
	if timer == nil {
		transitioned, owner := service.transitionToRetiringLocked(entry.attemptID, entry)
		service.mu.Unlock()
		if owner {
			service.reap(transitioned)
		}
		return attemptCreationFailure{
			status: http.StatusInternalServerError,
			code:   "attempt-lease-failed",
		}
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
			return attemptCreationFailure{
				status: http.StatusInternalServerError,
				code:   "attempt-containment-failed",
			}
		}
		return attemptCreationFailure{status: http.StatusConflict, code: "attempt-rejected"}
	}
	return attemptCreationFailure{}
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
		result, receiptIssued, resultErr := service.resolveAttemptResultLocked(entry, now)
		entry.operation.Unlock()
		if resultErr != nil {
			service.rejectInvalidAttemptResult(entry)
			writeProtocolError(w, http.StatusInternalServerError, "attempt-result-invalid")
			return
		}
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

func (service *Service) resolveAttemptResultLocked(
	entry *leasedAttempt,
	now time.Time,
) (AttemptResult, bool, error) {
	result := entry.attempt.Result()
	result.ProtocolVersion = ProtocolVersion
	result.AttemptAuthority = attemptAuthorityFromParts(
		entry.binding, entry.requestID, entry.attemptID, entry.challenge,
	)
	result.ChallengeBindingSHA256 = challengeBindingSHA256(result.AttemptAuthority)
	result.TerminalReceipt = nil
	switch {
	case entry.terminalReceipt != nil:
		applyAttemptTerminalReceipt(&result, *entry.terminalReceipt)
		return result, false, nil
	case result.State == attemptStateEstablished || result.State == attemptStateFailed:
		signed, err := service.signAttemptTerminalResult(entry, result, now)
		if err != nil {
			return result, false, err
		}
		entry.terminalReceipt = &signed
		applyAttemptTerminalReceipt(&result, signed)
		return result, true, nil
	default:
		return result, false, validatePendingAttemptResult(result)
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
