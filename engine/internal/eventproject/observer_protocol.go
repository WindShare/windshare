package eventproject

import (
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	appobs "github.com/windshare/windshare/engine/observation"
)

func ProjectProtocolObservation(workflow appobs.Workflow, value sessionruntime.ProtocolObservation) (appobs.ProtocolObservationObserved, error) {
	event, err := projectProtocolObservation(workflow, value)
	if err == nil {
		return event, nil
	}
	return event, withRejectionContext(err, protocolRejection(workflow, value, err))
}
func projectProtocolObservation(workflow appobs.Workflow, value sessionruntime.ProtocolObservation) (appobs.ProtocolObservationObserved, error) {
	if value == nil {
		return appobs.ProtocolObservationObserved{}, rejectedProjection(ProjectionInvalidStageFields, "observation", "present_fact")
	}
	raw := value.Correlation()
	role, ok := projectProtocolRole(raw.Role)
	if !ok {
		return appobs.ProtocolObservationObserved{}, rejectedProjection(ProjectionUnknownEnum, "role", "known_enum")
	}
	sessionID, err := ProtocolSessionID(raw.ProtocolSessionID)
	if err != nil {
		return appobs.ProtocolObservationObserved{}, rejectedProjection(ProjectionInvalidIdentity, "protocol_session_id", "nonzero_16_bytes")
	}
	operationID, err := ProtocolOperationID(raw.OperationID)
	if err != nil {
		return appobs.ProtocolObservationObserved{}, rejectedProjection(ProjectionInvalidIdentity, "protocol_operation_id", "nonzero_16_bytes")
	}
	requestKind, ok := projectProtocolMessageKind(raw.RequestKind)
	_, notStarted := value.(sessionruntime.ResponseSendNotStarted)
	if !ok && (!notStarted || raw.RequestKind != 0) {
		return appobs.ProtocolObservationObserved{}, rejectedProjection(ProjectionUnknownEnum, "request_kind", "known_enum")
	}
	context := appobs.ProtocolObservationContext{Workflow: workflow, ObservedAt: value.ObservedAt(), Role: role, ProtocolSession: sessionID, ProtocolOperation: operationID, RequestKind: requestKind}
	switch fact := value.(type) {
	case sessionruntime.ProtocolOperationObservation:
		return projectProtocolOperation(context, fact)
	case sessionruntime.SenderContentDecision:
		decision, err := projectSenderContentDecision(fact.Decision())
		if err != nil {
			return appobs.ProtocolObservationObserved{}, err
		}
		if fact.Decision().OperationID != raw.OperationID || fact.Decision().RequestKind != raw.RequestKind {
			return appobs.ProtocolObservationObserved{}, rejectedProjection(ProjectionInvalidStageFields, "content_decision", "matching_operation_and_request")
		}
		lane, hasLane := fact.Lane()
		var projectedLane appobs.LaneIdentity
		if hasLane {
			projectedLane, err = LaneIdentity(lane)
			if err != nil {
				return appobs.ProtocolObservationObserved{}, err
			}
		}
		return appobs.NewSenderContentDecisionObserved(context, decision, projectedLane, hasLane)
	case sessionruntime.ResponseSendNotStarted:
		kind, content, result, err := projectResponseFields(fact.ResponseKind(), fact.Content(), fact.Result())
		if err != nil {
			return appobs.ProtocolObservationObserved{}, err
		}
		return appobs.NewResponseSendNotStartedObserved(context, fact.ResponseSequence(), kind, content, result)
	case sessionruntime.ResponseSendReturned:
		kind, content, result, err := projectResponseFields(fact.ResponseKind(), fact.Content(), fact.Result())
		if err != nil {
			return appobs.ProtocolObservationObserved{}, err
		}
		return appobs.NewResponseSendReturnedObserved(context, fact.ResponseSequence(), kind, content, result)
	case sessionruntime.SendAttemptSettled:
		kind, ok := projectProtocolMessageKind(fact.ResponseKind())
		if !ok {
			return appobs.ProtocolObservationObserved{}, rejectedProjection(ProjectionUnknownEnum, "response_kind", "known_enum")
		}
		attempt, err := projectSendAttempt(fact.Attempt())
		if err != nil {
			return appobs.ProtocolObservationObserved{}, err
		}
		return appobs.NewSendAttemptSettledObserved(context, fact.Attempt().Identity().ResponseSequence, kind, attempt)
	case sessionruntime.ReceivedProtocolError:
		content, err := projectProtocolErrorContent(fact.Content())
		if err != nil {
			return appobs.ProtocolObservationObserved{}, err
		}
		lane, err := LaneIdentity(fact.Lane())
		if err != nil {
			return appobs.ProtocolObservationObserved{}, err
		}
		return appobs.NewReceivedProtocolErrorObserved(context, content, lane)
	default:
		return appobs.ProtocolObservationObserved{}, rejectedProjection(ProjectionInvalidStageFields, "observation", "known_fact")
	}
}
func projectProtocolOperation(context appobs.ProtocolObservationContext, value sessionruntime.ProtocolOperationObservation) (appobs.ProtocolObservationObserved, error) {
	stage, ok := projectProtocolOperationStage(value.Stage)
	if !ok {
		return appobs.ProtocolObservationObserved{}, rejectedProjection(ProjectionUnknownEnum, "stage", "known_enum")
	}
	var responseKind appobs.ProtocolMessageKind
	if value.HasResponse {
		responseKind, ok = projectProtocolMessageKind(value.ResponseKind)
		if !ok {
			return appobs.ProtocolObservationObserved{}, rejectedProjection(ProjectionUnknownEnum, "response_kind", "known_enum")
		}
	}
	var outcome appobs.ProtocolSendOutcome
	if value.HasSend {
		outcome, ok = projectProtocolSendOutcome(value.SendOutcome)
		if !ok {
			return appobs.ProtocolObservationObserved{}, rejectedProjection(ProjectionUnknownEnum, "send_outcome", "known_enum")
		}
	} else if value.SendOutcome != protocolsession.SendOutcomeUninitialized {
		return appobs.ProtocolObservationObserved{}, rejectedProjection(ProjectionInvalidStageFields, "send_outcome", "requires_send")
	}
	cause, ok := projectProtocolOperationCause(value.Cause)
	if !ok {
		return appobs.ProtocolObservationObserved{}, rejectedProjection(ProjectionUnknownEnum, "cause", "known_enum")
	}
	var lane appobs.LaneIdentity
	if value.HasLane {
		var err error
		lane, err = LaneIdentity(value.Lane)
		if err != nil {
			return appobs.ProtocolObservationObserved{}, err
		}
	}
	blockWait, err := projectBlockWait(value.BlockWait)
	if err != nil {
		return appobs.ProtocolObservationObserved{}, err
	}
	return appobs.NewProtocolOperationObserved(appobs.ProtocolOperationSpec{Workflow: context.Workflow, ObservedAt: context.ObservedAt, Role: context.Role, ProtocolSession: context.ProtocolSession, ProtocolOperation: context.ProtocolOperation, RequestKind: context.RequestKind, Stage: stage, ResponseKind: responseKind, HasResponse: value.HasResponse, Lane: lane, HasLane: value.HasLane, HasSend: value.HasSend, SendSettled: value.SendSettled, SendAdmitted: value.SendAdmitted, SendOutcome: outcome, ResponseCount: value.ResponseCount, DeadlineRemainingMillis: value.DeadlineRemainingMillis, HasDeadline: value.HasDeadline, OperationElapsedMillis: value.OperationElapsedMillis, UsableLanesAtSelection: value.UsableLanesAtSelection, UsableLanesAtSettlement: value.UsableLanesAtSettlement, Cause: cause,
		BlockWait: blockWait,
		RequestScheduling: appobs.RequestSchedulingSpec{
			ExpectedMillis:      uint64(max(0, value.RequestScheduling.Expected.Milliseconds())),
			ResponseMillis:      uint64(max(0, value.RequestScheduling.Response.Milliseconds())),
			QueuedContentMillis: uint64(max(0, value.RequestScheduling.QueuedContent.Milliseconds())),
			PendingRequests:     value.RequestScheduling.Pending,
		},
	})
}
func projectBlockWait(value contentflow.BlockWaitTimeout) (appobs.BlockWaitSpec, error) {
	wait := appobs.BlockWaitSpec{
		WaitedMillis: uint64(max(0, value.Waited.Milliseconds())), QueueProgress: value.QueueProgress,
	}
	switch value.Phase {
	case "":
	case contentflow.BlockAwaitingFirstFragment:
		wait.Phase = appobs.BlockWaitAwaitingFirstFragment
	case contentflow.BlockReceivingFragments:
		wait.Phase = appobs.BlockWaitReceivingFragments
	default:
		return wait, rejectedProjection(ProjectionUnknownEnum, "block_wait", "known_phase")
	}
	return wait, nil
}

func projectProtocolErrorContent(value sessionruntime.ProtocolErrorContent) (appobs.ProtocolErrorContent, error) {
	if value.IsZero() {
		return appobs.ProtocolErrorContent{}, nil
	}
	scope, ok := projectProtocolErrorScope(value.WireScope())
	if !ok {
		return appobs.ProtocolErrorContent{}, rejectedProjection(ProjectionUnknownEnum, "protocol_error.scope", "known_enum")
	}
	retry, hasRetry := value.RetryAfterMillis()
	return appobs.NewProtocolErrorContent(appobs.ProtocolErrorContentSpec{WireScope: scope, WireCode: value.WireCode(), Retryable: value.Retryable(), RetryAfterMillis: retry, HasRetryAfter: hasRetry})
}
func projectSendAttempt(value protocolsession.SendAttemptSnapshot) (appobs.SendAttemptSnapshot, error) {
	projected, err := projectSendAttemptValue(value)
	if err == nil {
		return projected, nil
	}
	id := value.Identity()
	source := appobs.CaptureObservationRejection(appobs.ObservationRejection{Event: "protocol_send_attempt", Source: "eventproject.ProjectProtocolObservation", Stage: "send_attempt", ResponseSequence: strconv.FormatUint(id.ResponseSequence, 10), AttemptSequence: strconv.FormatUint(uint64(id.AttemptSequence), 10)}, attemptRejectionFields(value)...)
	return projected, withRejectionContext(err, source)
}
func projectSendAttemptValue(value protocolsession.SendAttemptSnapshot) (appobs.SendAttemptSnapshot, error) {
	id := value.Identity()
	lane, err := appobs.NewLaneIdentity(id.LaneID, id.LaneEpoch)
	if err != nil {
		return appobs.SendAttemptSnapshot{}, rejectedProjection(ProjectionInvalidIdentity, "attempt.lane", "valid_lane_identity")
	}
	outcome, ok := projectProtocolSendOutcome(value.Outcome())
	if !ok {
		return appobs.SendAttemptSnapshot{}, rejectedProjection(ProjectionUnknownEnum, "attempt.outcome", "known_enum")
	}
	disposition, ok := projectOptionalDisposition(value.TransportDisposition())
	if !ok {
		return appobs.SendAttemptSnapshot{}, rejectedProjection(ProjectionUnknownEnum, "attempt.transport_disposition", "known_enum")
	}
	end, ok := sendAttemptEndProjections[value.End()]
	if !ok {
		return appobs.SendAttemptSnapshot{}, rejectedProjection(ProjectionUnknownEnum, "attempt.end", "known_enum")
	}
	causeKind, ok := sendAttemptCauseProjections[value.Cause().Kind()]
	if !ok {
		return appobs.SendAttemptSnapshot{}, rejectedProjection(ProjectionUnknownEnum, "attempt.cause.kind", "known_enum")
	}
	cause, err := appobs.NewSendAttemptCause(causeKind, value.Cause().Detail(), value.Cause().Truncated())
	if err != nil {
		return appobs.SendAttemptSnapshot{}, err
	}
	return appobs.NewSendAttemptSnapshot(appobs.SendAttemptSpec{Cause: cause, AttemptSequence: id.AttemptSequence, Lane: lane, PolicyAdmitted: value.PolicyAdmitted(), Settled: value.Settled(), Outcome: outcome, TransportDisposition: disposition, HasTransportDisposition: value.TransportDisposition() != 0, End: end})
}
func projectResponseFields(kind protocolsession.MessageKind, content sessionruntime.ProtocolErrorContent, result protocolsession.ResponseSendResult) (appobs.ProtocolMessageKind, appobs.ProtocolErrorContent, appobs.ResponseSendResult, error) {
	projectedKind, ok := projectProtocolMessageKind(kind)
	if !ok {
		return 0, appobs.ProtocolErrorContent{}, appobs.ResponseSendResult{}, rejectedProjection(ProjectionUnknownEnum, "response_kind", "known_enum")
	}
	projectedContent, err := projectProtocolErrorContent(content)
	if err != nil {
		return 0, appobs.ProtocolErrorContent{}, appobs.ResponseSendResult{}, err
	}
	projectedResult, err := projectResponseResult(result)
	return projectedKind, projectedContent, projectedResult, err
}
func projectResponseResult(value protocolsession.ResponseSendResult) (appobs.ResponseSendResult, error) {
	evidence, ok := responseSendEvidenceProjections[value.Evidence()]
	if !ok {
		return appobs.ResponseSendResult{}, rejectedProjection(ProjectionUnknownEnum, "response_result.evidence", "known_enum")
	}
	end, ok := responseSendEndProjections[value.End()]
	if !ok {
		return appobs.ResponseSendResult{}, rejectedProjection(ProjectionUnknownEnum, "response_result.end", "known_enum")
	}
	cleanup, ok := sendCleanupProjections[value.Cleanup()]
	if !ok {
		return appobs.ResponseSendResult{}, rejectedProjection(ProjectionUnknownEnum, "response_result.cleanup", "known_enum")
	}
	var attempts [appobs.MaxProtocolSendAttempts]appobs.SendAttemptSnapshot
	if value.AttemptCount() > len(attempts) {
		return appobs.ResponseSendResult{}, rejectedProjection(ProjectionInvalidStageFields, "response_result.attempts", "bounded_history")
	}
	for i := 0; i < value.AttemptCount(); i++ {
		source, _ := value.Attempt(i)
		var err error
		attempts[i], err = projectSendAttempt(source)
		if err != nil {
			return appobs.ResponseSendResult{}, err
		}
	}
	pending, hasPending := value.PendingAttempt()
	return appobs.NewResponseSendResult(appobs.ResponseSendResultSpec{Started: value.Started(), Evidence: evidence, End: end, Cleanup: cleanup, Attempts: attempts[:value.AttemptCount()], PendingAttemptSequence: pending.AttemptSequence, HasPendingAttempt: hasPending})
}
func projectSenderContentDecision(
	value contentflow.SenderDecisionTrace,
) (appobs.SenderContentDecision, error) {
	if value == (contentflow.SenderDecisionTrace{}) {
		return appobs.SenderContentDecision{}, nil
	}
	switch value.Stage {
	case contentflow.SenderDecisionCapacityBusy:
		if value.CapacityDecisionID == "" {
			return appobs.SenderContentDecision{}, rejectedProjection(ProjectionInvalidIdentity, "content_decision.capacity_decision_id", "required_for_capacity_decision")
		}
		if !value.LeaseID.IsZero() {
			return appobs.SenderContentDecision{}, rejectedProjection(ProjectionInvalidStageFields, "content_decision.lease_id", "absent_for_capacity_decision")
		}
		decisionID, err := appobs.NewCapacityDecisionID(string(value.CapacityDecisionID))
		if err != nil {
			return appobs.SenderContentDecision{}, rejectedProjection(ProjectionInvalidIdentity, "content_decision.capacity_decision_id", "valid_capacity_identity")
		}
		return appobs.NewSenderCapacityDecision(decisionID)
	case contentflow.SenderDecisionLeaseRelinquished,
		contentflow.SenderDecisionLeaseUndelivered,
		contentflow.SenderDecisionLeaseDetached,
		contentflow.SenderDecisionBlockLeaseReleased,
		contentflow.SenderDecisionBlockLeaseNotOwned,
		contentflow.SenderDecisionBlockLeaseExpired,
		contentflow.SenderDecisionBlockLeaseInvalid:
		if value.CapacityDecisionID != "" {
			return appobs.SenderContentDecision{}, rejectedProjection(ProjectionInvalidStageFields, "content_decision.capacity_decision_id", "absent_for_lease_decision")
		}
		if value.LeaseID.IsZero() {
			return appobs.SenderContentDecision{}, rejectedProjection(ProjectionInvalidIdentity, "content_decision.lease_id", "nonzero_16_bytes")
		}
		kind := appobs.SenderContentLeaseRelinquished
		switch value.Stage {
		case contentflow.SenderDecisionLeaseUndelivered:
			kind = appobs.SenderContentLeaseUndelivered
		case contentflow.SenderDecisionLeaseDetached:
			kind = appobs.SenderContentLeaseDetached
		case contentflow.SenderDecisionBlockLeaseReleased:
			kind = appobs.SenderContentBlockLeaseReleased
		case contentflow.SenderDecisionBlockLeaseNotOwned:
			kind = appobs.SenderContentBlockLeaseNotOwned
		case contentflow.SenderDecisionBlockLeaseExpired:
			kind = appobs.SenderContentBlockLeaseExpired
		case contentflow.SenderDecisionBlockLeaseInvalid:
			kind = appobs.SenderContentBlockLeaseInvalid
		}
		leaseID, err := appobs.NewRevisionLeaseID(value.LeaseID.Bytes())
		if err != nil {
			return appobs.SenderContentDecision{}, rejectedProjection(ProjectionInvalidIdentity, "content_decision.lease_id", "valid_lease_identity")
		}
		return appobs.NewSenderLeaseDecision(kind, leaseID)
	default:
		return appobs.SenderContentDecision{}, rejectedProjection(ProjectionUnknownEnum, "content_decision.stage", "known_enum")
	}
}

func protocolRejection(workflow appobs.Workflow, value sessionruntime.ProtocolObservation, err error) appobs.ObservationRejection {
	context := appobs.ObservationRejection{Event: "protocol_observation", Source: "eventproject.ProjectProtocolObservation", Stage: "unknown"}
	if value == nil {
		return appobs.CaptureObservationRejection(context, appobs.RejectedBool("observation_present", false))
	}
	raw := value.Correlation()
	sessionBytes := raw.ProtocolSessionID.Bytes()
	operationBytes := raw.OperationID.Bytes()
	context.Session = hex.EncodeToString(sessionBytes)
	context.Operation = hex.EncodeToString(operationBytes)
	fields := []appobs.RejectionField{appobs.RejectedEnum("workflow", uint64(workflow)), appobs.RejectedEnum("role", uint64(raw.Role)), appobs.RejectedEnum("request_kind", uint64(raw.RequestKind)), appobs.RejectedIdentity("protocol_session_id", sessionBytes), appobs.RejectedIdentity("protocol_operation_id", operationBytes), appobs.RejectedString("observed_at", value.ObservedAt().String())}
	switch fact := value.(type) {
	case sessionruntime.ProtocolOperationObservation:
		context.Event = "protocol_operation"
		if stage, ok := projectProtocolOperationStage(fact.Stage); ok {
			context.Stage, _ = stage.Name()
		}
		fields = append(fields, appobs.RejectedEnum("stage", uint64(fact.Stage)), appobs.RejectedBool("has_response", fact.HasResponse), appobs.RejectedEnum("response_kind", uint64(fact.ResponseKind)), appobs.RejectedBool("has_send", fact.HasSend), appobs.RejectedEnum("send_outcome", uint64(fact.SendOutcome)), appobs.RejectedBool("send_settled", fact.SendSettled), appobs.RejectedBool("send_admitted", fact.SendAdmitted), appobs.RejectedBool("has_lane", fact.HasLane), appobs.RejectedUint("lane_id", uint64(fact.Lane.ID)), appobs.RejectedUint("lane_epoch", uint64(fact.Lane.Epoch)), appobs.RejectedEnum("cause", uint64(fact.Cause)), appobs.RejectedUint("response_count", fact.ResponseCount), appobs.RejectedBool("has_deadline", fact.HasDeadline), appobs.RejectedUint("deadline_remaining_ms", fact.DeadlineRemainingMillis))
	case sessionruntime.SenderContentDecision:
		context.Event = "sender_content_decision"
		context.Stage = "sender_content_decision"
		d := fact.Decision()
		lane, hasLane := fact.Lane()
		fields = append(fields, appobs.RejectedBool("has_lane", hasLane), appobs.RejectedUint("lane_id", uint64(lane.ID)), appobs.RejectedUint("lane_epoch", uint64(lane.Epoch)))
		fields = append(fields, appobs.RejectedEnum("content_decision.stage", uint64(d.Stage)), appobs.RejectedIdentity("content_decision.operation_id", d.OperationID.Bytes()), appobs.RejectedEnum("content_decision.request_kind", uint64(d.RequestKind)), appobs.RejectedString("content_decision.capacity_decision_id", string(d.CapacityDecisionID)), appobs.RejectedIdentity("content_decision.lease_id", d.LeaseID.Bytes()))
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
		fields = append(fields, appobs.RejectedEnum("response_kind", uint64(fact.ResponseKind())))
	case sessionruntime.ReceivedProtocolError:
		context.Event = "protocol_error_received"
		context.Stage = "received_protocol_error"
		fields = append(contentRejectionFields(fact.Content()), fields...)
		fields = append(fields, appobs.RejectedUint("lane_id", uint64(fact.Lane().ID)), appobs.RejectedUint("lane_epoch", uint64(fact.Lane().Epoch)))
	}
	rejectedField := ProjectionRejection(err).Field
	if contract, ok := errors.AsType[appobs.EventContractError](err); ok {
		rejectedField = contract.Field
	}
	fields = prioritizeProtocolRejection(fields, rejectedField)
	return appobs.CaptureObservationRejection(context, fields...)
}
func contentRejectionFields(value sessionruntime.ProtocolErrorContent) []appobs.RejectionField {
	retry, hasRetry := value.RetryAfterMillis()
	return []appobs.RejectionField{appobs.RejectedEnum("protocol_error.scope", uint64(value.WireScope())), appobs.RejectedUint("protocol_error.code", uint64(value.WireCode())), appobs.RejectedBool("protocol_error.retryable", value.Retryable()), appobs.RejectedBool("protocol_error.has_retry_after", hasRetry), appobs.RejectedUint("protocol_error.retry_after_ms", uint64(retry))}
}
func attemptRejectionFields(value protocolsession.SendAttemptSnapshot) []appobs.RejectionField {
	id := value.Identity()
	return []appobs.RejectionField{appobs.RejectedUint("response_sequence", id.ResponseSequence), appobs.RejectedUint("attempt.attempt_sequence", uint64(id.AttemptSequence)), appobs.RejectedUint("attempt.lane_id", uint64(id.LaneID)), appobs.RejectedUint("attempt.lane_epoch", uint64(id.LaneEpoch)), appobs.RejectedBool("attempt.policy_admitted", value.PolicyAdmitted()), appobs.RejectedBool("attempt.settled", value.Settled()), appobs.RejectedEnum("attempt.outcome", uint64(value.Outcome())), appobs.RejectedEnum("attempt.transport_disposition", uint64(value.TransportDisposition())), appobs.RejectedEnum("attempt.end", uint64(value.End())), appobs.RejectedEnum("attempt.cause.kind", uint64(value.Cause().Kind())), appobs.RejectedString("attempt.cause.detail", value.Cause().Detail()), appobs.RejectedBool("attempt.cause.truncated", value.Cause().Truncated())}
}
func responseRejectionFields(sequence uint64, kind protocolsession.MessageKind, value protocolsession.ResponseSendResult, content sessionruntime.ProtocolErrorContent) []appobs.RejectionField {
	fields := []appobs.RejectionField{appobs.RejectedUint("response_sequence", sequence), appobs.RejectedEnum("response_kind", uint64(kind)), appobs.RejectedBool("response_result.started", value.Started()), appobs.RejectedEnum("response_result.evidence", uint64(value.Evidence())), appobs.RejectedEnum("response_result.end", uint64(value.End())), appobs.RejectedEnum("response_result.cleanup", uint64(value.Cleanup())), appobs.RejectedUint("response_result.attempt_count", uint64(value.AttemptCount()))}
	pending, hasPending := value.PendingAttempt()
	fields = append(fields, appobs.RejectedBool("response_result.has_pending_attempt", hasPending), appobs.RejectedUint("response_result.pending_attempt_sequence", uint64(pending.AttemptSequence)))
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
func prioritizeProtocolRejection(fields []appobs.RejectionField, field string) []appobs.RejectionField {
	matches := func(name string) bool {
		switch field {
		case "stage_fields":
			return name == "workflow" || name == "role" || name == "stage" || name == "has_response" || name == "response_kind" || name == "has_send" || name == "cause" || name == "response_count"
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
	ordered := make([]appobs.RejectionField, 0, len(fields))
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
