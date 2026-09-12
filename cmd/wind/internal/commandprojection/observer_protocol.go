package commandprojection

import (
	"encoding/hex"
	"errors"
	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"strconv"
	"strings"
)

func ProjectProtocolObservation(command clievent.Command, value sessionruntime.ProtocolObservation) (clievent.ProtocolObservationObserved, error) {
	event, err := projectProtocolObservation(command, value)
	if err == nil {
		return event, nil
	}
	return event, withRejectionContext(err, protocolRejection(command, value, err))
}
func projectProtocolObservation(command clievent.Command, value sessionruntime.ProtocolObservation) (clievent.ProtocolObservationObserved, error) {
	if value == nil {
		return clievent.ProtocolObservationObserved{}, rejectedProjection(ProjectionInvalidStageFields, "observation", "present_fact")
	}
	raw := value.Correlation()
	role, ok := projectProtocolRole(raw.Role)
	if !ok {
		return clievent.ProtocolObservationObserved{}, rejectedProjection(ProjectionUnknownEnum, "role", "known_enum")
	}
	sessionID, err := ProtocolSessionID(raw.ProtocolSessionID)
	if err != nil {
		return clievent.ProtocolObservationObserved{}, rejectedProjection(ProjectionInvalidIdentity, "protocol_session_id", "nonzero_16_bytes")
	}
	operationID, err := ProtocolOperationID(raw.OperationID)
	if err != nil {
		return clievent.ProtocolObservationObserved{}, rejectedProjection(ProjectionInvalidIdentity, "protocol_operation_id", "nonzero_16_bytes")
	}
	requestKind, ok := projectProtocolMessageKind(raw.RequestKind)
	_, notStarted := value.(sessionruntime.ResponseSendNotStarted)
	if !ok && (!notStarted || raw.RequestKind != 0) {
		return clievent.ProtocolObservationObserved{}, rejectedProjection(ProjectionUnknownEnum, "request_kind", "known_enum")
	}
	context := clievent.ProtocolObservationContext{Command: command, ObservedAt: value.ObservedAt(), Role: role, ProtocolSession: sessionID, ProtocolOperation: operationID, RequestKind: requestKind}
	switch fact := value.(type) {
	case sessionruntime.ProtocolOperationObservation:
		return projectProtocolOperation(context, fact)
	case sessionruntime.SenderContentDecision:
		decision, err := projectSenderContentDecision(fact.Decision())
		if err != nil {
			return clievent.ProtocolObservationObserved{}, err
		}
		if fact.Decision().OperationID != raw.OperationID || fact.Decision().RequestKind != raw.RequestKind {
			return clievent.ProtocolObservationObserved{}, rejectedProjection(ProjectionInvalidStageFields, "content_decision", "matching_operation_and_request")
		}
		lane, hasLane := fact.Lane()
		var projectedLane clievent.LaneIdentity
		if hasLane {
			projectedLane, err = LaneIdentity(lane)
			if err != nil {
				return clievent.ProtocolObservationObserved{}, err
			}
		}
		return clievent.NewSenderContentDecisionObserved(context, decision, projectedLane, hasLane)
	case sessionruntime.ResponseSendNotStarted:
		kind, content, result, err := projectResponseFields(fact.ResponseKind(), fact.Content(), fact.Result())
		if err != nil {
			return clievent.ProtocolObservationObserved{}, err
		}
		return clievent.NewResponseSendNotStartedObserved(context, fact.ResponseSequence(), kind, content, result)
	case sessionruntime.ResponseSendReturned:
		kind, content, result, err := projectResponseFields(fact.ResponseKind(), fact.Content(), fact.Result())
		if err != nil {
			return clievent.ProtocolObservationObserved{}, err
		}
		return clievent.NewResponseSendReturnedObserved(context, fact.ResponseSequence(), kind, content, result)
	case sessionruntime.SendAttemptSettled:
		kind, ok := projectProtocolMessageKind(fact.ResponseKind())
		if !ok {
			return clievent.ProtocolObservationObserved{}, rejectedProjection(ProjectionUnknownEnum, "response_kind", "known_enum")
		}
		attempt, err := projectSendAttempt(fact.Attempt())
		if err != nil {
			return clievent.ProtocolObservationObserved{}, err
		}
		return clievent.NewSendAttemptSettledObserved(context, fact.Attempt().Identity().ResponseSequence, kind, attempt)
	case sessionruntime.ReceivedProtocolError:
		content, err := projectProtocolErrorContent(fact.Content())
		if err != nil {
			return clievent.ProtocolObservationObserved{}, err
		}
		lane, err := LaneIdentity(fact.Lane())
		if err != nil {
			return clievent.ProtocolObservationObserved{}, err
		}
		return clievent.NewReceivedProtocolErrorObserved(context, content, lane)
	default:
		return clievent.ProtocolObservationObserved{}, rejectedProjection(ProjectionInvalidStageFields, "observation", "known_fact")
	}
}
func projectProtocolOperation(context clievent.ProtocolObservationContext, value sessionruntime.ProtocolOperationObservation) (clievent.ProtocolObservationObserved, error) {
	stage, ok := projectProtocolOperationStage(value.Stage)
	if !ok {
		return clievent.ProtocolObservationObserved{}, rejectedProjection(ProjectionUnknownEnum, "stage", "known_enum")
	}
	var responseKind clievent.ProtocolMessageKind
	if value.HasResponse {
		responseKind, ok = projectProtocolMessageKind(value.ResponseKind)
		if !ok {
			return clievent.ProtocolObservationObserved{}, rejectedProjection(ProjectionUnknownEnum, "response_kind", "known_enum")
		}
	}
	var outcome clievent.ProtocolSendOutcome
	if value.HasSend {
		outcome, ok = projectProtocolSendOutcome(value.SendOutcome)
		if !ok {
			return clievent.ProtocolObservationObserved{}, rejectedProjection(ProjectionUnknownEnum, "send_outcome", "known_enum")
		}
	} else if value.SendOutcome != protocolsession.SendOutcomeUninitialized {
		return clievent.ProtocolObservationObserved{}, rejectedProjection(ProjectionInvalidStageFields, "send_outcome", "requires_send")
	}
	cause, ok := projectProtocolOperationCause(value.Cause)
	if !ok {
		return clievent.ProtocolObservationObserved{}, rejectedProjection(ProjectionUnknownEnum, "cause", "known_enum")
	}
	var lane clievent.LaneIdentity
	if value.HasLane {
		var err error
		lane, err = LaneIdentity(value.Lane)
		if err != nil {
			return clievent.ProtocolObservationObserved{}, err
		}
	}
	return clievent.NewProtocolOperationObserved(clievent.ProtocolOperationSpec{Command: context.Command, ObservedAt: context.ObservedAt, Role: context.Role, ProtocolSession: context.ProtocolSession, ProtocolOperation: context.ProtocolOperation, RequestKind: context.RequestKind, Stage: stage, ResponseKind: responseKind, HasResponse: value.HasResponse, Lane: lane, HasLane: value.HasLane, HasSend: value.HasSend, SendSettled: value.SendSettled, SendAdmitted: value.SendAdmitted, SendOutcome: outcome, ResponseCount: value.ResponseCount, DeadlineRemainingMillis: value.DeadlineRemainingMillis, HasDeadline: value.HasDeadline, OperationElapsedMillis: value.OperationElapsedMillis, UsableLanesAtSelection: value.UsableLanesAtSelection, UsableLanesAtSettlement: value.UsableLanesAtSettlement, Cause: cause})
}
func projectProtocolErrorContent(value sessionruntime.ProtocolErrorContent) (clievent.ProtocolErrorContent, error) {
	if value.IsZero() {
		return clievent.ProtocolErrorContent{}, nil
	}
	scope, ok := projectProtocolErrorScope(value.WireScope())
	if !ok {
		return clievent.ProtocolErrorContent{}, rejectedProjection(ProjectionUnknownEnum, "protocol_error.scope", "known_enum")
	}
	retry, hasRetry := value.RetryAfterMillis()
	return clievent.NewProtocolErrorContent(clievent.ProtocolErrorContentSpec{WireScope: scope, WireCode: value.WireCode(), Retryable: value.Retryable(), RetryAfterMillis: retry, HasRetryAfter: hasRetry})
}
func projectSendAttempt(value protocolsession.SendAttemptSnapshot) (clievent.SendAttemptSnapshot, error) {
	projected, err := projectSendAttemptValue(value)
	if err == nil {
		return projected, nil
	}
	id := value.Identity()
	source := clievent.CaptureObservationRejection(clievent.ObservationRejection{Event: "protocol_send_attempt", Source: "commandprojection.ProjectProtocolObservation", Stage: "send_attempt", ResponseSequence: strconv.FormatUint(id.ResponseSequence, 10), AttemptSequence: strconv.FormatUint(uint64(id.AttemptSequence), 10)}, attemptRejectionFields(value)...)
	return projected, withRejectionContext(err, source)
}
func projectSendAttemptValue(value protocolsession.SendAttemptSnapshot) (clievent.SendAttemptSnapshot, error) {
	id := value.Identity()
	lane, err := clievent.NewLaneIdentity(id.LaneID, id.LaneEpoch)
	if err != nil {
		return clievent.SendAttemptSnapshot{}, rejectedProjection(ProjectionInvalidIdentity, "attempt.lane", "valid_lane_identity")
	}
	outcome, ok := projectProtocolSendOutcome(value.Outcome())
	if !ok {
		return clievent.SendAttemptSnapshot{}, rejectedProjection(ProjectionUnknownEnum, "attempt.outcome", "known_enum")
	}
	disposition, ok := projectOptionalDisposition(value.TransportDisposition())
	if !ok {
		return clievent.SendAttemptSnapshot{}, rejectedProjection(ProjectionUnknownEnum, "attempt.transport_disposition", "known_enum")
	}
	end, ok := sendAttemptEndProjections[value.End()]
	if !ok {
		return clievent.SendAttemptSnapshot{}, rejectedProjection(ProjectionUnknownEnum, "attempt.end", "known_enum")
	}
	causeKind, ok := sendAttemptCauseProjections[value.Cause().Kind()]
	if !ok {
		return clievent.SendAttemptSnapshot{}, rejectedProjection(ProjectionUnknownEnum, "attempt.cause.kind", "known_enum")
	}
	cause, err := clievent.NewSendAttemptCause(causeKind, value.Cause().Detail(), value.Cause().Truncated())
	if err != nil {
		return clievent.SendAttemptSnapshot{}, err
	}
	return clievent.NewSendAttemptSnapshot(clievent.SendAttemptSpec{Cause: cause, AttemptSequence: id.AttemptSequence, Lane: lane, PolicyAdmitted: value.PolicyAdmitted(), Settled: value.Settled(), Outcome: outcome, TransportDisposition: disposition, HasTransportDisposition: value.TransportDisposition() != 0, End: end})
}
func projectResponseFields(kind protocolsession.MessageKind, content sessionruntime.ProtocolErrorContent, result protocolsession.ResponseSendResult) (clievent.ProtocolMessageKind, clievent.ProtocolErrorContent, clievent.ResponseSendResult, error) {
	projectedKind, ok := projectProtocolMessageKind(kind)
	if !ok {
		return 0, clievent.ProtocolErrorContent{}, clievent.ResponseSendResult{}, rejectedProjection(ProjectionUnknownEnum, "response_kind", "known_enum")
	}
	projectedContent, err := projectProtocolErrorContent(content)
	if err != nil {
		return 0, clievent.ProtocolErrorContent{}, clievent.ResponseSendResult{}, err
	}
	projectedResult, err := projectResponseResult(result)
	return projectedKind, projectedContent, projectedResult, err
}
func projectResponseResult(value protocolsession.ResponseSendResult) (clievent.ResponseSendResult, error) {
	evidence, ok := responseSendEvidenceProjections[value.Evidence()]
	if !ok {
		return clievent.ResponseSendResult{}, rejectedProjection(ProjectionUnknownEnum, "response_result.evidence", "known_enum")
	}
	end, ok := responseSendEndProjections[value.End()]
	if !ok {
		return clievent.ResponseSendResult{}, rejectedProjection(ProjectionUnknownEnum, "response_result.end", "known_enum")
	}
	cleanup, ok := sendCleanupProjections[value.Cleanup()]
	if !ok {
		return clievent.ResponseSendResult{}, rejectedProjection(ProjectionUnknownEnum, "response_result.cleanup", "known_enum")
	}
	var attempts [clievent.MaxProtocolSendAttempts]clievent.SendAttemptSnapshot
	if value.AttemptCount() > len(attempts) {
		return clievent.ResponseSendResult{}, rejectedProjection(ProjectionInvalidStageFields, "response_result.attempts", "bounded_history")
	}
	for i := 0; i < value.AttemptCount(); i++ {
		source, _ := value.Attempt(i)
		var err error
		attempts[i], err = projectSendAttempt(source)
		if err != nil {
			return clievent.ResponseSendResult{}, err
		}
	}
	pending, hasPending := value.PendingAttempt()
	return clievent.NewResponseSendResult(clievent.ResponseSendResultSpec{Started: value.Started(), Evidence: evidence, End: end, Cleanup: cleanup, Attempts: attempts[:value.AttemptCount()], PendingAttemptSequence: pending.AttemptSequence, HasPendingAttempt: hasPending})
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

func protocolRejection(command clievent.Command, value sessionruntime.ProtocolObservation, err error) clievent.ObservationRejection {
	context := clievent.ObservationRejection{Event: "protocol_observation", Source: "commandprojection.ProjectProtocolObservation", Stage: "unknown"}
	if value == nil {
		return clievent.CaptureObservationRejection(context, clievent.RejectedBool("observation_present", false))
	}
	raw := value.Correlation()
	sessionBytes := raw.ProtocolSessionID.Bytes()
	operationBytes := raw.OperationID.Bytes()
	context.Session = hex.EncodeToString(sessionBytes)
	context.Operation = hex.EncodeToString(operationBytes)
	fields := []clievent.RejectionField{clievent.RejectedEnum("command", uint64(command)), clievent.RejectedEnum("role", uint64(raw.Role)), clievent.RejectedEnum("request_kind", uint64(raw.RequestKind)), clievent.RejectedIdentity("protocol_session_id", sessionBytes), clievent.RejectedIdentity("protocol_operation_id", operationBytes), clievent.RejectedString("observed_at", value.ObservedAt().String())}
	switch fact := value.(type) {
	case sessionruntime.ProtocolOperationObservation:
		context.Event = "protocol_operation"
		if stage, ok := projectProtocolOperationStage(fact.Stage); ok {
			context.Stage, _ = stage.Name()
		}
		fields = append(fields, clievent.RejectedEnum("stage", uint64(fact.Stage)), clievent.RejectedBool("has_response", fact.HasResponse), clievent.RejectedEnum("response_kind", uint64(fact.ResponseKind)), clievent.RejectedBool("has_send", fact.HasSend), clievent.RejectedEnum("send_outcome", uint64(fact.SendOutcome)), clievent.RejectedBool("send_settled", fact.SendSettled), clievent.RejectedBool("send_admitted", fact.SendAdmitted), clievent.RejectedBool("has_lane", fact.HasLane), clievent.RejectedUint("lane_id", uint64(fact.Lane.ID)), clievent.RejectedUint("lane_epoch", uint64(fact.Lane.Epoch)), clievent.RejectedEnum("cause", uint64(fact.Cause)), clievent.RejectedUint("response_count", fact.ResponseCount), clievent.RejectedBool("has_deadline", fact.HasDeadline), clievent.RejectedUint("deadline_remaining_ms", fact.DeadlineRemainingMillis))
	case sessionruntime.SenderContentDecision:
		context.Event = "sender_content_decision"
		context.Stage = "sender_content_decision"
		d := fact.Decision()
		lane, hasLane := fact.Lane()
		fields = append(fields, clievent.RejectedBool("has_lane", hasLane), clievent.RejectedUint("lane_id", uint64(lane.ID)), clievent.RejectedUint("lane_epoch", uint64(lane.Epoch)))
		fields = append(fields, clievent.RejectedEnum("content_decision.stage", uint64(d.Stage)), clievent.RejectedIdentity("content_decision.operation_id", d.OperationID.Bytes()), clievent.RejectedEnum("content_decision.request_kind", uint64(d.RequestKind)), clievent.RejectedString("content_decision.capacity_decision_id", string(d.CapacityDecisionID)), clievent.RejectedIdentity("content_decision.lease_id", d.LeaseID.Bytes()))
	case sessionruntime.ResponseSendNotStarted:
		context.Event = "protocol_response_send_not_started"
		context.Stage = "response_send_not_started"
		context.ResponseSequence = strconv.FormatUint(fact.ResponseSequence(), 10)
		fields = append(responseRejectionFields(fact.ResponseSequence(), fact.ResponseKind(), fact.Result(), fact.Content()), fields...)
	case sessionruntime.ResponseSendReturned:
		context.Event = "protocol_response_send_returned"
		context.Stage = "response_send_returned"
		context.ResponseSequence = strconv.FormatUint(fact.ResponseSequence(), 10)
		fields = append(responseRejectionFields(fact.ResponseSequence(), fact.ResponseKind(), fact.Result(), fact.Content()), fields...)
	case sessionruntime.SendAttemptSettled:
		context.Event = "protocol_send_attempt_settled"
		context.Stage = "send_attempt_settled"
		id := fact.Attempt().Identity()
		context.ResponseSequence = strconv.FormatUint(id.ResponseSequence, 10)
		context.AttemptSequence = strconv.FormatUint(uint64(id.AttemptSequence), 10)
		fields = append(attemptRejectionFields(fact.Attempt()), fields...)
		fields = append(fields, clievent.RejectedEnum("response_kind", uint64(fact.ResponseKind())))
	case sessionruntime.ReceivedProtocolError:
		context.Event = "protocol_error_received"
		context.Stage = "received_protocol_error"
		fields = append(contentRejectionFields(fact.Content()), fields...)
		fields = append(fields, clievent.RejectedUint("lane_id", uint64(fact.Lane().ID)), clievent.RejectedUint("lane_epoch", uint64(fact.Lane().Epoch)))
	}
	rejectedField := ProjectionRejection(err).Field
	if contract, ok := errors.AsType[clievent.EventContractError](err); ok {
		rejectedField = contract.Field
	}
	fields = prioritizeProtocolRejection(fields, rejectedField)
	return clievent.CaptureObservationRejection(context, fields...)
}
func contentRejectionFields(value sessionruntime.ProtocolErrorContent) []clievent.RejectionField {
	retry, hasRetry := value.RetryAfterMillis()
	return []clievent.RejectionField{clievent.RejectedEnum("protocol_error.scope", uint64(value.WireScope())), clievent.RejectedUint("protocol_error.code", uint64(value.WireCode())), clievent.RejectedBool("protocol_error.retryable", value.Retryable()), clievent.RejectedBool("protocol_error.has_retry_after", hasRetry), clievent.RejectedUint("protocol_error.retry_after_ms", uint64(retry))}
}
func attemptRejectionFields(value protocolsession.SendAttemptSnapshot) []clievent.RejectionField {
	id := value.Identity()
	return []clievent.RejectionField{clievent.RejectedUint("response_sequence", id.ResponseSequence), clievent.RejectedUint("attempt.attempt_sequence", uint64(id.AttemptSequence)), clievent.RejectedUint("attempt.lane_id", uint64(id.LaneID)), clievent.RejectedUint("attempt.lane_epoch", uint64(id.LaneEpoch)), clievent.RejectedBool("attempt.policy_admitted", value.PolicyAdmitted()), clievent.RejectedBool("attempt.settled", value.Settled()), clievent.RejectedEnum("attempt.outcome", uint64(value.Outcome())), clievent.RejectedEnum("attempt.transport_disposition", uint64(value.TransportDisposition())), clievent.RejectedEnum("attempt.end", uint64(value.End())), clievent.RejectedEnum("attempt.cause.kind", uint64(value.Cause().Kind())), clievent.RejectedString("attempt.cause.detail", value.Cause().Detail()), clievent.RejectedBool("attempt.cause.truncated", value.Cause().Truncated())}
}
func responseRejectionFields(sequence uint64, kind protocolsession.MessageKind, value protocolsession.ResponseSendResult, content sessionruntime.ProtocolErrorContent) []clievent.RejectionField {
	fields := []clievent.RejectionField{clievent.RejectedUint("response_sequence", sequence), clievent.RejectedEnum("response_kind", uint64(kind)), clievent.RejectedBool("response_result.started", value.Started()), clievent.RejectedEnum("response_result.evidence", uint64(value.Evidence())), clievent.RejectedEnum("response_result.end", uint64(value.End())), clievent.RejectedEnum("response_result.cleanup", uint64(value.Cleanup())), clievent.RejectedUint("response_result.attempt_count", uint64(value.AttemptCount()))}
	pending, hasPending := value.PendingAttempt()
	fields = append(fields, clievent.RejectedBool("response_result.has_pending_attempt", hasPending), clievent.RejectedUint("response_result.pending_attempt_sequence", uint64(pending.AttemptSequence)))
	if !content.IsZero() {
		fields = append(fields, contentRejectionFields(content)...)
	}
	for i := 0; i < value.AttemptCount(); i++ {
		attempt, _ := value.Attempt(i)
		fields = append(fields, attemptRejectionFields(attempt)...)
	}
	return fields
}

// Put every operand of the failed format rule ahead of incidental context. The
// bounded sample may omit other fields, but must preserve the values it rejects.
func prioritizeProtocolRejection(fields []clievent.RejectionField, field string) []clievent.RejectionField {
	matches := func(name string) bool {
		switch field {
		case "stage_fields":
			return name == "command" || name == "role" || name == "stage" || name == "has_response" || name == "response_kind" || name == "has_send" || name == "cause" || name == "response_count"
		case "send", "send_outcome":
			return name == "has_send" || strings.HasPrefix(name, "send_")
		case "lane", "attempt.lane":
			return strings.Contains(name, "lane")
		case "deadline":
			return name == "has_deadline" || name == "deadline_remaining_ms"
		case "response_kind":
			return name == "has_response" || name == "response_kind"
		case "protocol_error":
			return strings.HasPrefix(name, "protocol_error.") || strings.Contains(name, "lane")
		case "content_decision":
			return strings.HasPrefix(name, "content_decision.") || name == "protocol_operation_id" || name == "request_kind"
		}
		if strings.HasPrefix(field, "protocol_error.") {
			return strings.HasPrefix(name, "protocol_error.")
		}
		if strings.HasPrefix(field, "attempt.cause.") {
			return strings.HasPrefix(name, "attempt.cause.")
		}
		return name == field
	}
	ordered := make([]clievent.RejectionField, 0, len(fields))
	for _, value := range fields {
		if matches(value.Field) {
			ordered = append(ordered, value)
		}
	}
	for _, value := range fields {
		if !matches(value.Field) {
			ordered = append(ordered, value)
		}
	}
	return ordered
}
