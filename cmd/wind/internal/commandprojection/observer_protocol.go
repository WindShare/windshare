package commandprojection

import (
	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"strconv"
)

func ProjectProtocolOperation(command clievent.Command, value sessionruntime.ProtocolOperationTrace) (clievent.ProtocolOperationObserved, error) {
	event, err := projectProtocolOperation(command, value)
	if err == nil {
		return event, nil
	}
	context := clievent.ObservationRejection{Stage: "unknown_" + strconv.FormatUint(uint64(value.Stage), 10)}
	if stage, ok := projectProtocolOperationStage(value.Stage); ok {
		context.Stage, _ = stage.Name()
	}
	context.Session, _ = ProtocolSessionID(value.ProtocolSessionID)
	if context.Session.Valid() {
		context.Operation, _ = ProtocolOperationID(value.OperationID)
	}
	return event, withRejectionContext(err, context)
}

func projectProtocolOperation(
	command clievent.Command,
	value sessionruntime.ProtocolOperationTrace,
) (clievent.ProtocolOperationObserved, error) {
	role, ok := projectProtocolRole(value.Role)
	if !ok {
		return clievent.ProtocolOperationObserved{}, rejectedProjection(ProjectionUnknownEnum, "role", "known_enum")
	}
	stage, ok := projectProtocolOperationStage(value.Stage)
	if !ok {
		return clievent.ProtocolOperationObserved{}, rejectedProjection(ProjectionUnknownEnum, "stage", "known_enum")
	}
	sessionID, err := ProtocolSessionID(value.ProtocolSessionID)
	if err != nil {
		return clievent.ProtocolOperationObserved{}, rejectedProjection(ProjectionInvalidIdentity, "protocol_session_id", "nonzero_16_bytes")
	}
	operationID, err := ProtocolOperationID(value.OperationID)
	if err != nil {
		return clievent.ProtocolOperationObserved{}, rejectedProjection(ProjectionInvalidIdentity, "protocol_operation_id", "nonzero_16_bytes")
	}
	requestKind, ok := projectProtocolMessageKind(value.RequestKind)
	if !ok {
		return clievent.ProtocolOperationObserved{}, rejectedProjection(ProjectionUnknownEnum, "request_kind", "known_enum")
	}
	var responseKind clievent.ProtocolMessageKind
	if value.HasResponse {
		responseKind, ok = projectProtocolMessageKind(value.ResponseKind)
		if !ok {
			return clievent.ProtocolOperationObserved{}, rejectedProjection(ProjectionUnknownEnum, "response_kind", "known_enum")
		}
	}
	sendOutcome, ok := projectProtocolSendOutcome(value.SendOutcome)
	if !ok {
		return clievent.ProtocolOperationObserved{}, rejectedProjection(ProjectionUnknownEnum, "send_outcome", "known_enum")
	}
	cause, ok := projectProtocolOperationCause(value.Cause)
	if !ok {
		return clievent.ProtocolOperationObserved{}, rejectedProjection(ProjectionUnknownEnum, "cause", "known_enum")
	}
	var lane clievent.LaneIdentity
	if value.HasLane {
		lane, err = LaneIdentity(value.Lane)
		if err != nil {
			return clievent.ProtocolOperationObserved{}, rejectedProjection(ProjectionInvalidIdentity, "lane", "valid_lane_identity")
		}
	}
	protocolFailure, err := projectProtocolFailure(value.Failure)
	if err != nil {
		return clievent.ProtocolOperationObserved{}, err
	}
	if value.ContentDecision != (contentflow.SenderDecisionTrace{}) &&
		(value.ContentDecision.OperationID != value.OperationID || value.ContentDecision.RequestKind != value.RequestKind) {
		return clievent.ProtocolOperationObserved{}, rejectedProjection(ProjectionInvalidStageFields, "content_decision", "matching_operation_and_request")
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
		return clievent.ProtocolOperationObserved{}, err
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
		if value.CapacityDecisionID == "" {
			return clievent.SenderContentDecision{}, rejectedProjection(ProjectionInvalidIdentity, "content_decision.capacity_decision_id", "required_for_capacity_decision")
		}
		if !value.LeaseID.IsZero() {
			return clievent.SenderContentDecision{}, rejectedProjection(ProjectionInvalidStageFields, "content_decision.lease_id", "absent_for_capacity_decision")
		}
		decisionID, err := clievent.NewCapacityDecisionID(string(value.CapacityDecisionID))
		if err != nil {
			return clievent.SenderContentDecision{}, rejectedProjection(ProjectionInvalidIdentity, "content_decision.capacity_decision_id", "valid_capacity_identity")
		}
		return clievent.NewSenderCapacityDecision(decisionID)
	case contentflow.SenderDecisionLeaseRelinquished,
		contentflow.SenderDecisionLeaseUndelivered,
		contentflow.SenderDecisionLeaseDetached,
		contentflow.SenderDecisionBlockLeaseReleased,
		contentflow.SenderDecisionBlockLeaseNotOwned,
		contentflow.SenderDecisionBlockLeaseExpired,
		contentflow.SenderDecisionBlockLeaseInvalid:
		if value.CapacityDecisionID != "" {
			return clievent.SenderContentDecision{}, rejectedProjection(ProjectionInvalidStageFields, "content_decision.capacity_decision_id", "absent_for_lease_decision")
		}
		if value.LeaseID.IsZero() {
			return clievent.SenderContentDecision{}, rejectedProjection(ProjectionInvalidIdentity, "content_decision.lease_id", "nonzero_16_bytes")
		}
		kind := clievent.SenderContentLeaseRelinquished
		switch value.Stage {
		case contentflow.SenderDecisionLeaseUndelivered:
			kind = clievent.SenderContentLeaseUndelivered
		case contentflow.SenderDecisionLeaseDetached:
			kind = clievent.SenderContentLeaseDetached
		case contentflow.SenderDecisionBlockLeaseReleased:
			kind = clievent.SenderContentBlockLeaseReleased
		case contentflow.SenderDecisionBlockLeaseNotOwned:
			kind = clievent.SenderContentBlockLeaseNotOwned
		case contentflow.SenderDecisionBlockLeaseExpired:
			kind = clievent.SenderContentBlockLeaseExpired
		case contentflow.SenderDecisionBlockLeaseInvalid:
			kind = clievent.SenderContentBlockLeaseInvalid
		}
		leaseID, err := clievent.NewRevisionLeaseID(value.LeaseID.Bytes())
		if err != nil {
			return clievent.SenderContentDecision{}, rejectedProjection(ProjectionInvalidIdentity, "content_decision.lease_id", "valid_lease_identity")
		}
		return clievent.NewSenderLeaseDecision(kind, leaseID)
	default:
		return clievent.SenderContentDecision{}, rejectedProjection(ProjectionUnknownEnum, "content_decision.stage", "known_enum")
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
