package runtrace

import "github.com/windshare/windshare/cmd/wind/internal/clievent"

func (visitor *encodeVisitorV4) VisitRelayLifecycleObserved(event clievent.RelayLifecycleObserved) error {
	stage, err := nameOf(event.Stage())
	if err != nil {
		return err
	}
	retirement, err := nameOf(event.RetirementSource())
	if err != nil {
		return err
	}
	cause, err := nameOf(event.Cause())
	if err != nil {
		return err
	}
	drain, err := nameOf(event.DrainCause())
	if err != nil {
		return err
	}
	payload := relayLifecyclePayloadV4{
		LinkID:           decimal(event.LinkID()),
		Stage:            stage,
		Terminal:         event.Terminal(),
		RetirementSource: retirement,
		Cause:            cause,
		DrainCause:       drain,
	}
	if session, ok := event.RelaySessionID(); ok {
		encoded := encodeTypedIdentity(session.Bytes())
		payload.RelaySessionID = &encoded
	}
	if operation := event.SendOperationID(); operation != 0 {
		payload.SendOperationID = decimalPointer(operation)
	}
	if disposition, ok := event.Disposition(); ok {
		payload.Disposition, err = namedPointer(disposition)
		if err != nil {
			return err
		}
	}
	if dropped := event.Dropped(); dropped != 0 {
		payload.Dropped = decimalPointer(dropped)
	}
	visitor.set("relay_lifecycle", nil, payload)
	return nil
}

func (visitor *encodeVisitorV4) VisitWebRTCLifecycleObserved(event clievent.WebRTCLifecycleObserved) error {
	operation, err := nameOf(event.Operation())
	if err != nil {
		return err
	}
	transition, err := nameOf(event.Transition())
	if err != nil {
		return err
	}
	state, err := nameOf(event.State())
	if err != nil {
		return err
	}
	terminal, err := nameOf(event.Terminal())
	if err != nil {
		return err
	}
	cause, err := nameOf(event.Cause())
	if err != nil {
		return err
	}
	payload := webRTCLifecyclePayloadV4{
		ChannelID:     decimal(event.ChannelID()),
		Operation:     operation,
		Transition:    transition,
		State:         state,
		TerminalState: terminal,
		Cause:         cause,
	}
	if sendOperation := event.SendOperationID(); sendOperation != 0 {
		payload.SendOperationID = decimalPointer(sendOperation)
	}
	if disposition, ok := event.Disposition(); ok {
		payload.Disposition, err = namedPointer(disposition)
		if err != nil {
			return err
		}
	}
	if dropped := event.Dropped(); dropped != 0 {
		payload.Dropped = decimalPointer(dropped)
	}
	visitor.set("webrtc_lifecycle", nil, payload)
	return nil
}

func (visitor *encodeVisitorV4) VisitPeerAttemptObserved(event clievent.PeerAttemptObserved) error {
	stage, err := nameOf(event.Stage())
	if err != nil {
		return err
	}
	lane, hasLane := event.Lane()
	correlation, err := projectEventCorrelation(
		event.ProtocolSessionID(), clievent.ProtocolOperationID{},
		event.PeerPathID(), event.PeerAttemptID(), lane, hasLane,
	)
	if err != nil {
		return err
	}
	payload := peerAttemptPayloadV4{
		AttemptSequence:  decimal(event.Sequence()),
		AttemptElapsedMS: decimal(event.ElapsedMillis()),
		Stage:            stage,
	}
	if operation, ok := event.OfferOperationID(); ok {
		encoded := encodeTypedIdentity(operation.Bytes())
		payload.OfferOperationID = &encoded
	}
	if phase, deadline, ok := event.PhaseDeadline(); ok {
		phaseName, nameErr := nameOf(phase)
		if nameErr != nil {
			return nameErr
		}
		payload.PhaseDeadline = &peerPhaseDeadlineV4{
			Phase: phaseName, DeadlineMS: decimal(deadline),
		}
	}
	if candidates, ok := event.Candidates(); ok {
		payload.Candidates = &peerCandidateCountsV4{
			LocalEmitted:   candidates.LocalEmitted,
			RemoteAccepted: candidates.RemoteAccepted,
		}
	}
	if operation, ok := event.GrantOperationID(); ok {
		encoded := encodeTypedIdentity(operation.Bytes())
		payload.GrantOperationID = &encoded
	}
	if disposition, delivery, ok := event.Admission(); ok {
		dispositionName, nameErr := nameOf(disposition)
		if nameErr != nil {
			return nameErr
		}
		deliveryName, nameErr := nameOf(delivery)
		if nameErr != nil {
			return nameErr
		}
		payload.Admission = &peerAdmissionV4{
			Disposition: dispositionName, ResponseDelivery: deliveryName,
		}
	}
	if rejection, retryAfter, ok := event.Rejection(); ok {
		rejectionName, nameErr := nameOf(rejection)
		if nameErr != nil {
			return nameErr
		}
		payload.Rejection = &peerRejectionV4{Code: rejectionName}
		if retryAfter != 0 {
			payload.Rejection.RetryAfterMS = decimalPointer(retryAfter)
		}
	}
	if err := encodePeerAttemptFailure(event, &payload); err != nil {
		return err
	}
	visitor.set("peer_attempt", correlation, payload)
	return nil
}

func encodePeerAttemptFailure(event clievent.PeerAttemptObserved, payload *peerAttemptPayloadV4) error {
	scope, failure, ok := event.Failure()
	if !ok {
		return nil
	}
	failedAt, present := event.FailedAtStage()
	if !present {
		return errInvalidSchemaEvent
	}
	failedAtName, nameErr := nameOf(failedAt)
	if nameErr != nil {
		return nameErr
	}
	scopeName, nameErr := nameOf(scope)
	if nameErr != nil {
		return nameErr
	}
	projectedFailure, projectErr := projectFailure(failure)
	if projectErr != nil {
		return projectErr
	}
	payload.Failure = &peerFailureV4{
		FailedAtStage: failedAtName, Scope: scopeName, Failure: projectedFailure,
	}
	if summary, ok := event.FailureSummary(); ok {
		last, err := nameOf(summary.LastCompletedStage)
		if err != nil {
			return err
		}
		initiator, _ := summary.Initiator.Name()
		cause, _ := summary.Cause.Name()
		payload.Failure.Summary = &peerFailureSummaryV4{
			LastCompletedStage: last, StageElapsedMillis: decimal(summary.StageElapsedMillis),
			DeadlineExpired: summary.DeadlineExpired, Initiator: initiator, Cause: cause,
		}
	}
	return nil
}
