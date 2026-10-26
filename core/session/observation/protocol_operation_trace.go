package sessionobs

import (
	"github.com/windshare/windshare/core/diagnostics"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/requestlane"
	"strconv"
	"time"
)

// ProtocolOperationStage identifies the side and terminal observation boundary
// of one authenticated operation without exposing its request body.
type ProtocolOperationStage uint8

const (
	ProtocolOperationReceiverCompleted ProtocolOperationStage = iota + 1
	ProtocolOperationReceiverFailed
	ProtocolOperationReceiverEnded
	ProtocolOperationSenderRequestReceived
	ProtocolOperationReceiverWaitingActiveCapacity
	ProtocolOperationReceiverWaitingRetainedCapacity
	ProtocolOperationReceiverAdmissionReady
)

// ProtocolOperationObservation summarizes one RPC boundary. Content decisions carry
// only opaque coordinator/lease join keys; request bodies, file identities,
// catalog paths, and raw errors remain intentionally absent.
type ProtocolOperationObservation struct {
	observedAt              time.Time
	Stage                   ProtocolOperationStage
	Role                    protocolsession.Role
	ProtocolSessionID       protocolsession.ProtocolSessionID
	OperationID             protocolsession.OperationID
	RequestKind             protocolsession.MessageKind
	ResponseKind            protocolsession.MessageKind
	HasResponse             bool
	Lane                    sessioncontract.LaneIdentity
	HasLane                 bool
	HasSend                 bool
	SendSettled             bool
	SendAdmitted            bool
	SendOutcome             protocolsession.SendOutcome
	ResponseCount           uint64
	DeadlineRemainingMillis uint64
	HasDeadline             bool
	OperationElapsedMillis  uint64
	UsableLanesAtSelection  uint32
	UsableLanesAtSettlement uint32
	Cause                   ProtocolOperationCause
	RequestScheduling       requestlane.Estimate
	BlockWait               contentflow.BlockWaitTimeout
}

type ProtocolObservation interface {
	DiagnosticRecord() diagnostics.Record
	Correlation() ProtocolObservationCorrelation
	ObservedAt() time.Time
	protocolObservation()
}

type ProtocolObservationCorrelation struct {
	Role              protocolsession.Role
	ProtocolSessionID protocolsession.ProtocolSessionID
	OperationID       protocolsession.OperationID
	// RequestKind may be zero only on ResponseSendNotStarted when setup had no
	// operation route from which to obtain authenticated request context.
	RequestKind protocolsession.MessageKind
}

type ProtocolObservationContext struct {
	Correlation ProtocolObservationCorrelation
	ObservedAt  time.Time
}

type ObservationContext struct{ context ProtocolObservationContext }

func (fact ObservationContext) Correlation() ProtocolObservationCorrelation {
	return fact.context.Correlation
}

func (fact ObservationContext) ObservedAt() time.Time { return fact.context.ObservedAt }

func (ObservationContext) protocolObservation() {}

type ProtocolObservationTracer interface{ TraceProtocolObservation(ProtocolObservation) }

type ProtocolObservationTraceFunc func(ProtocolObservation)

func (fn ProtocolObservationTraceFunc) TraceProtocolObservation(fact ProtocolObservation) {
	if fn != nil {
		fn(fact)
	}
}

type responseSendObservation struct {
	ObservationContext
	sequence uint64
	kind     protocolsession.MessageKind
	content  ProtocolErrorContent
	result   protocolsession.ResponseSendResult
}

func (fact responseSendObservation) ResponseSequence() uint64 { return fact.sequence }

func (fact responseSendObservation) ResponseKind() protocolsession.MessageKind { return fact.kind }

func (fact responseSendObservation) Content() ProtocolErrorContent { return fact.content }

func (fact responseSendObservation) Result() protocolsession.ResponseSendResult { return fact.result }

type ResponseSendNotStarted struct{ responseSendObservation }

type ResponseSendReturned struct{ responseSendObservation }

func NewResponseSendNotStarted(ctx ProtocolObservationContext, sequence uint64, kind protocolsession.MessageKind, content ProtocolErrorContent, result protocolsession.ResponseSendResult) ResponseSendNotStarted {
	return ResponseSendNotStarted{responseSendObservation{ObservationContext{ctx}, sequence, kind, content, result}}
}

func NewResponseSendReturned(ctx ProtocolObservationContext, sequence uint64, kind protocolsession.MessageKind, content ProtocolErrorContent, result protocolsession.ResponseSendResult) ResponseSendReturned {
	return ResponseSendReturned{responseSendObservation{ObservationContext{ctx}, sequence, kind, content, result}}
}

type SendAttemptSettled struct {
	ObservationContext
	kind    protocolsession.MessageKind
	attempt protocolsession.SendAttemptSnapshot
}

func NewSendAttemptSettled(ctx ProtocolObservationContext, kind protocolsession.MessageKind, attempt protocolsession.SendAttemptSnapshot) SendAttemptSettled {
	return SendAttemptSettled{ObservationContext{ctx}, kind, attempt}
}

func (fact SendAttemptSettled) ResponseKind() protocolsession.MessageKind { return fact.kind }

func (fact SendAttemptSettled) Attempt() protocolsession.SendAttemptSnapshot { return fact.attempt }

type ReceivedProtocolError struct {
	ObservationContext
	content ProtocolErrorContent
	lane    sessioncontract.LaneIdentity
}

func NewReceivedProtocolError(ctx ProtocolObservationContext, content ProtocolErrorContent, lane sessioncontract.LaneIdentity) ReceivedProtocolError {
	return ReceivedProtocolError{ObservationContext{ctx}, content, lane}
}

func (fact ReceivedProtocolError) Content() ProtocolErrorContent { return fact.content }

func (fact ReceivedProtocolError) Lane() sessioncontract.LaneIdentity { return fact.lane }

type SenderContentDecision struct {
	ObservationContext
	decision contentflow.SenderDecisionTrace
	lane     sessioncontract.LaneIdentity
	hasLane  bool
}

func NewSenderContentDecision(ctx ProtocolObservationContext, decision contentflow.SenderDecisionTrace, lane sessioncontract.LaneIdentity, hasLane bool) SenderContentDecision {
	return SenderContentDecision{ObservationContext{ctx}, decision, lane, hasLane}
}

func (fact SenderContentDecision) Decision() contentflow.SenderDecisionTrace { return fact.decision }

func (fact SenderContentDecision) Lane() (sessioncontract.LaneIdentity, bool) {
	return fact.lane, fact.hasLane
}

func NewProtocolOperationObservation(ctx ProtocolObservationContext, fact ProtocolOperationObservation) ProtocolOperationObservation {
	fact.Role = ctx.Correlation.Role
	fact.ProtocolSessionID = ctx.Correlation.ProtocolSessionID
	fact.OperationID = ctx.Correlation.OperationID
	fact.RequestKind = ctx.Correlation.RequestKind
	fact.observedAt = ctx.ObservedAt
	return fact
}

func (fact ProtocolOperationObservation) Correlation() ProtocolObservationCorrelation {
	return ProtocolObservationCorrelation{fact.Role, fact.ProtocolSessionID, fact.OperationID, fact.RequestKind}
}

func (fact ProtocolOperationObservation) ObservedAt() time.Time { return fact.observedAt }

func (ProtocolOperationObservation) protocolObservation() {}

// DiagnosticName belongs to the source vocabulary; recorders accept new values.
func (value ProtocolOperationStage) DiagnosticName() string {
	switch value {
	case ProtocolOperationReceiverCompleted:
		return "receiver_completed"
	case ProtocolOperationReceiverFailed:
		return "receiver_failed"
	case ProtocolOperationReceiverEnded:
		return "receiver_ended"
	case ProtocolOperationSenderRequestReceived:
		return "sender_request_received"
	case ProtocolOperationReceiverWaitingActiveCapacity:
		return "receiver_waiting_active_capacity"
	case ProtocolOperationReceiverWaitingRetainedCapacity:
		return "receiver_waiting_retained_capacity"
	case ProtocolOperationReceiverAdmissionReady:
		return "receiver_admission_ready"
	default:
		return "unknown_" + strconv.FormatUint(uint64(value), 10)
	}
}

const (
	protocolOperationEventName  = "protocol_operation"
	contentDecisionEventName    = "sender_content_decision"
	responseNotStartedEventName = "protocol_response_send_not_started"
	responseReturnedEventName   = "protocol_response_send_returned"
	sendAttemptEventName        = "protocol_send_attempt_settled"
	protocolErrorEventName      = "protocol_error_received"
	terminalSendEventName       = "sender_terminal_send_observed"
	sessionTerminatedEventName  = "sender_session_terminated"
)

func protocolDiagnosticFields(value interface {
	Correlation() ProtocolObservationCorrelation
	ObservedAt() time.Time
}) diagnostics.Fields {
	return diagnostics.Fields{"observed_at": diagnostics.Text(value.ObservedAt().UTC().Format(time.RFC3339Nano)),
		"role": diagnostics.Text(value.Correlation().Role.DiagnosticName()), "request_kind": diagnostics.Text(value.Correlation().RequestKind.DiagnosticName())}
}

func protocolDiagnosticCorrelation(value interface {
	Correlation() ProtocolObservationCorrelation
}, lane sessioncontract.LaneIdentity) diagnostics.Fields {
	c := value.Correlation()
	fields := diagnostics.Fields{"protocol_session_id": diagnostics.Identity(c.ProtocolSessionID[:]), "protocol_operation_id": diagnostics.Identity(c.OperationID[:])}
	if lane.ID != 0 {
		fields["lane_id"], fields["lane_epoch"] = diagnostics.Number(float64(lane.ID)), diagnostics.Number(float64(lane.Epoch))
	}
	return fields
}

func (value ProtocolOperationObservation) DiagnosticRecord() diagnostics.Record {
	fields := protocolDiagnosticFields(value)
	fields["stage"], fields["cause"] = diagnostics.Text(value.Stage.DiagnosticName()), diagnostics.Text(value.Cause.DiagnosticName())
	fields["response_count"], fields["operation_elapsed_ms"] = diagnostics.Uint(value.ResponseCount), diagnostics.Uint(value.OperationElapsedMillis)
	fields["usable_lanes_at_selection"], fields["usable_lanes_at_settlement"] = diagnostics.Number(float64(value.UsableLanesAtSelection)), diagnostics.Number(float64(value.UsableLanesAtSettlement))
	if value.HasResponse {
		fields["response_kind"] = diagnostics.Text(value.ResponseKind.DiagnosticName())
	}
	if value.HasSend {
		fields["send"] = diagnostics.Object(diagnostics.Fields{"settled": diagnostics.Bool(value.SendSettled), "admitted": diagnostics.Bool(value.SendAdmitted), "outcome": diagnostics.Text(value.SendOutcome.DiagnosticName())})
	}
	if value.HasDeadline {
		fields["deadline_remaining_ms"] = diagnostics.Uint(value.DeadlineRemainingMillis)
	}
	if wait := value.BlockWait; wait.Phase != "" {
		fields["block_wait"] = diagnostics.Object(diagnostics.Fields{"phase": diagnostics.Text(string(wait.Phase)), "waited_ms": diagnostics.Millis(wait.Waited), "queue_progress": diagnostics.Uint(wait.QueueProgress)})
	}
	if scheduling := value.RequestScheduling; scheduling.Expected != 0 {
		fields["request_scheduling"] = diagnostics.Object(diagnostics.Fields{"expected_ms": diagnostics.Millis(scheduling.Expected), "response_ms": diagnostics.Millis(scheduling.Response),
			"queued_content_ms": diagnostics.Millis(scheduling.QueuedContent), "pending_requests": diagnostics.Number(float64(scheduling.Pending))})
	}
	return diagnostics.New(protocolOperationEventName, fields).At(value.ObservedAt()).Correlate(protocolDiagnosticCorrelation(value, value.Lane))
}

func (value SenderContentDecision) DiagnosticRecord() diagnostics.Record {
	fields := protocolDiagnosticFields(value)
	fields["content_decision"] = value.Decision().DiagnosticValue()
	lane, _ := value.Lane()
	return diagnostics.New(contentDecisionEventName, fields).At(value.ObservedAt()).Correlate(protocolDiagnosticCorrelation(value, lane))
}

func (value responseSendObservation) diagnosticRecord(name string) diagnostics.Record {
	fields := protocolDiagnosticFields(value)
	fields["response_sequence"], fields["response_kind"] = diagnostics.Uint(value.ResponseSequence()), diagnostics.Text(value.ResponseKind().DiagnosticName())
	fields["protocol_error"], fields["response_result"] = value.Content().DiagnosticValue(), value.Result().DiagnosticValue()
	return diagnostics.New(name, fields).At(value.ObservedAt()).Correlate(protocolDiagnosticCorrelation(value, sessioncontract.LaneIdentity{}))
}

func (value ResponseSendNotStarted) DiagnosticRecord() diagnostics.Record {
	return value.diagnosticRecord(responseNotStartedEventName)
}

func (value ResponseSendReturned) DiagnosticRecord() diagnostics.Record {
	return value.diagnosticRecord(responseReturnedEventName)
}

func (value SendAttemptSettled) DiagnosticRecord() diagnostics.Record {
	fields := protocolDiagnosticFields(value)
	id := value.Attempt().Identity()
	fields["response_sequence"], fields["response_kind"] = diagnostics.Uint(id.ResponseSequence), diagnostics.Text(value.ResponseKind().DiagnosticName())
	fields["attempt"] = value.Attempt().DiagnosticValue()
	return diagnostics.New(sendAttemptEventName, fields).At(value.ObservedAt()).Correlate(protocolDiagnosticCorrelation(value, sessioncontract.LaneIdentity{ID: id.LaneID, Epoch: id.LaneEpoch}))
}

func (value ReceivedProtocolError) DiagnosticRecord() diagnostics.Record {
	fields := protocolDiagnosticFields(value)
	fields["protocol_error"] = value.Content().DiagnosticValue()
	return diagnostics.New(protocolErrorEventName, fields).At(value.ObservedAt()).Correlate(protocolDiagnosticCorrelation(value, value.Lane()))
}
