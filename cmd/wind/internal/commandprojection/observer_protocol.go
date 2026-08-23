package commandprojection

import (
	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

func ProjectProtocolOperation(
	command clievent.Command,
	value sessionruntime.ProtocolOperationTrace,
) (clievent.ProtocolOperationObserved, error) {
	role, ok := projectProtocolRole(value.Role)
	if !ok {
		return clievent.ProtocolOperationObserved{}, ErrInvalidProjection
	}
	stage, ok := projectProtocolOperationStage(value.Stage)
	if !ok {
		return clievent.ProtocolOperationObserved{}, ErrInvalidProjection
	}
	sessionID, err := ProtocolSessionID(value.ProtocolSessionID)
	if err != nil {
		return clievent.ProtocolOperationObserved{}, ErrInvalidProjection
	}
	operationID, err := ProtocolOperationID(value.OperationID)
	if err != nil {
		return clievent.ProtocolOperationObserved{}, ErrInvalidProjection
	}
	requestKind, ok := projectProtocolMessageKind(value.RequestKind)
	if !ok {
		return clievent.ProtocolOperationObserved{}, ErrInvalidProjection
	}
	var responseKind clievent.ProtocolMessageKind
	if value.HasResponse {
		responseKind, ok = projectProtocolMessageKind(value.ResponseKind)
		if !ok {
			return clievent.ProtocolOperationObserved{}, ErrInvalidProjection
		}
	}
	sendOutcome, ok := projectProtocolSendOutcome(value.SendOutcome)
	if !ok {
		return clievent.ProtocolOperationObserved{}, ErrInvalidProjection
	}
	cause, ok := projectProtocolOperationCause(value.Cause)
	if !ok {
		return clievent.ProtocolOperationObserved{}, ErrInvalidProjection
	}
	var lane clievent.LaneIdentity
	if value.HasLane {
		lane, err = LaneIdentity(value.Lane)
		if err != nil {
			return clievent.ProtocolOperationObserved{}, ErrInvalidProjection
		}
	}
	protocolFailure, err := projectProtocolFailure(value.Failure)
	if err != nil {
		return clievent.ProtocolOperationObserved{}, err
	}
	if value.ContentDecision != (contentflow.SenderDecisionTrace{}) &&
		(value.ContentDecision.OperationID != value.OperationID || value.ContentDecision.RequestKind != value.RequestKind) {
		return clievent.ProtocolOperationObserved{}, ErrInvalidProjection
	}
	contentDecision, err := projectSenderContentDecision(value.ContentDecision)
	if err != nil {
		return clievent.ProtocolOperationObserved{}, err
	}
	event, err := clievent.NewProtocolOperationObserved(clievent.ProtocolOperationSpec{
		Command: command, Role: role, Stage: stage,
		ProtocolSession: sessionID, ProtocolOperation: operationID,
		RequestKind: requestKind, ResponseKind: responseKind, HasResponse: value.HasResponse,
		Lane: lane, HasLane: value.HasLane,
		HasSend: value.HasSend, SendSettled: value.SendSettled,
		SendAdmitted: value.SendAdmitted, SendOutcome: sendOutcome,
		ResponseCount:           value.ResponseCount,
		DeadlineRemainingMillis: value.DeadlineRemainingMillis, HasDeadline: value.HasDeadline,
		OperationElapsedMillis:  value.OperationElapsedMillis,
		UsableLanesAtSelection:  value.UsableLanesAtSelection,
		UsableLanesAtSettlement: value.UsableLanesAtSettlement,
		Failure:                 protocolFailure,
		Cause:                   cause,
		ContentDecision:         contentDecision,
	})
	if err != nil {
		return clievent.ProtocolOperationObserved{}, ErrInvalidProjection
	}
	return event, nil
}

func projectSenderContentDecision(
	value contentflow.SenderDecisionTrace,
) (clievent.SenderContentDecision, error) {
	if value == (contentflow.SenderDecisionTrace{}) {
		return clievent.SenderContentDecision{}, nil
	}
	switch value.Stage {
	case contentflow.SenderDecisionCapacityBusy:
		if value.CapacityDecisionID == "" || !value.LeaseID.IsZero() {
			return clievent.SenderContentDecision{}, ErrInvalidProjection
		}
		decisionID, err := clievent.NewCapacityDecisionID(string(value.CapacityDecisionID))
		if err != nil {
			return clievent.SenderContentDecision{}, ErrInvalidProjection
		}
		return clievent.NewSenderCapacityDecision(decisionID)
	case contentflow.SenderDecisionLeaseRelinquished,
		contentflow.SenderDecisionLeaseUndelivered,
		contentflow.SenderDecisionLeaseDetached:
		if value.CapacityDecisionID != "" || value.LeaseID.IsZero() {
			return clievent.SenderContentDecision{}, ErrInvalidProjection
		}
		kind := clievent.SenderContentLeaseRelinquished
		switch value.Stage {
		case contentflow.SenderDecisionLeaseUndelivered:
			kind = clievent.SenderContentLeaseUndelivered
		case contentflow.SenderDecisionLeaseDetached:
			kind = clievent.SenderContentLeaseDetached
		}
		leaseID, err := clievent.NewRevisionLeaseID(value.LeaseID.Bytes())
		if err != nil {
			return clievent.SenderContentDecision{}, ErrInvalidProjection
		}
		return clievent.NewSenderLeaseDecision(kind, leaseID)
	default:
		return clievent.SenderContentDecision{}, ErrInvalidProjection
	}
}

func projectProtocolFailure(value sessionruntime.ProtocolFailure) (clievent.ProtocolFailure, error) {
	if value.IsZero() {
		return clievent.ProtocolFailure{}, nil
	}
	requestKind, ok := projectProtocolMessageKind(value.RequestKind())
	if !ok {
		return clievent.ProtocolFailure{}, invalidProjection(ProjectionUnknownEnum)
	}
	wireScope, ok := projectProtocolFailureScope(value.WireScope())
	if !ok {
		return clievent.ProtocolFailure{}, invalidProjection(ProjectionUnknownEnum)
	}
	session, err := ProtocolSessionID(value.ProtocolSessionID())
	if err != nil {
		return clievent.ProtocolFailure{}, err
	}
	operation, err := ProtocolOperationID(value.ProtocolOperationID())
	if err != nil {
		return clievent.ProtocolFailure{}, err
	}
	retryAfterMillis, hasRetryAfter := value.RetryAfterMillis()
	spec := clievent.ProtocolFailureSpec{
		RequestKind: requestKind, WireScope: wireScope, WireCode: value.WireCode(),
		Retryable: value.Retryable(), RetryAfterMillis: retryAfterMillis,
		HasRetryAfter: hasRetryAfter, ProtocolSession: session, ProtocolOperation: operation,
	}
	if sourceLane, hasLane := value.Lane(); hasLane {
		spec.Lane, err = LaneIdentity(sourceLane)
		if err != nil {
			return clievent.ProtocolFailure{}, err
		}
		spec.HasLane = true
	}
	settlement := value.Settlement()
	var projected clievent.ProtocolFailure
	switch settlement.Kind() {
	case sessionruntime.ProtocolFailureSettlementReceivedAuthenticated:
		projected, err = clievent.NewReceivedAuthenticatedProtocolFailure(spec)
	case sessionruntime.ProtocolFailureSettlementResponseSend:
		response, present := settlement.ResponseSend()
		if !present {
			return clievent.ProtocolFailure{}, invalidProjection(ProjectionInvalidStageFields)
		}
		outcome, outcomeOK := projectProtocolSendOutcome(response.Outcome)
		if !outcomeOK {
			return clievent.ProtocolFailure{}, invalidProjection(ProjectionUnknownEnum)
		}
		projected, err = clievent.NewResponseSendProtocolFailure(
			spec,
			clievent.ProtocolFailureResponseSendSettlement{
				Admitted: response.Admitted,
				Settled:  response.Settled,
				Outcome:  outcome,
			},
		)
	default:
		return clievent.ProtocolFailure{}, invalidProjection(ProjectionUnknownEnum)
	}
	if err != nil {
		return clievent.ProtocolFailure{}, invalidProjection(ProjectionEventContract)
	}
	return projected, nil
}
