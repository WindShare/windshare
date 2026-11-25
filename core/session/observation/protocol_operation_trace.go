package sessionobs

import (
	"strconv"
	"time"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/requestlane"
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
	ProtocolOperationReceiverAdmissionReady
	ProtocolOperationSenderRequestBelowWindow
)

// SendStatus summarizes the physical transport status of an outbound protocol frame.
type SendStatus uint8

const (
	SendStatusConfirmed SendStatus = iota + 1
	SendStatusNotSent
	SendStatusUncertain
)

// DiagnosticName belongs to the source vocabulary; recorders accept new values.
func (value SendStatus) DiagnosticName() string {
	switch value {
	case SendStatusConfirmed:
		return "confirmed"
	case SendStatusNotSent:
		return "not_sent"
	case SendStatusUncertain:
		return "uncertain"
	default:
		return "unknown_" + strconv.FormatUint(uint64(value), 10)
	}
}

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
	Lane                    session.LaneIdentity
	HasLane                 bool
	HasSend                 bool
	SendStatus              SendStatus
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
	// RequestKind may be zero when an outbound error occurs before operation lookup succeeds.
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

type ResponseSendObserved struct {
	ObservationContext
	sequence uint64
	kind     protocolsession.MessageKind
	lane     session.LaneIdentity
	status   SendStatus
	attempts uint32
	content  ProtocolErrorContent
}

func NewResponseSendObserved(
	ctx ProtocolObservationContext,
	sequence uint64,
	kind protocolsession.MessageKind,
	lane session.LaneIdentity,
	status SendStatus,
	attempts uint32,
	content ProtocolErrorContent,
) ResponseSendObserved {
	return ResponseSendObserved{
		ObservationContext: ObservationContext{ctx},
		sequence:           sequence,
		kind:               kind,
		lane:               lane,
		status:             status,
		attempts:           attempts,
		content:            content,
	}
}

func (fact ResponseSendObserved) ResponseSequence() uint64                  { return fact.sequence }
func (fact ResponseSendObserved) ResponseKind() protocolsession.MessageKind { return fact.kind }
func (fact ResponseSendObserved) Lane() session.LaneIdentity                { return fact.lane }
func (fact ResponseSendObserved) Status() SendStatus                        { return fact.status }
func (fact ResponseSendObserved) Attempts() uint32                          { return fact.attempts }
func (fact ResponseSendObserved) Content() ProtocolErrorContent             { return fact.content }

type ReceivedProtocolError struct {
	ObservationContext
	content ProtocolErrorContent
	lane    session.LaneIdentity
}

func NewReceivedProtocolError(ctx ProtocolObservationContext, content ProtocolErrorContent, lane session.LaneIdentity) ReceivedProtocolError {
	return ReceivedProtocolError{ObservationContext{ctx}, content, lane}
}

func (fact ReceivedProtocolError) Content() ProtocolErrorContent { return fact.content }

func (fact ReceivedProtocolError) Lane() session.LaneIdentity { return fact.lane }

type SenderContentDecision struct {
	ObservationContext
	decision contentflow.SenderDecisionTrace
	lane     session.LaneIdentity
	hasLane  bool
}

func NewSenderContentDecision(ctx ProtocolObservationContext, decision contentflow.SenderDecisionTrace, lane session.LaneIdentity, hasLane bool) SenderContentDecision {
	return SenderContentDecision{ObservationContext{ctx}, decision, lane, hasLane}
}

func (fact SenderContentDecision) Decision() contentflow.SenderDecisionTrace { return fact.decision }

func (fact SenderContentDecision) Lane() (session.LaneIdentity, bool) {
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
	case ProtocolOperationReceiverAdmissionReady:
		return "receiver_admission_ready"
	case ProtocolOperationSenderRequestBelowWindow:
		return "sender_request_below_window"
	default:
		return "unknown_" + strconv.FormatUint(uint64(value), 10)
	}
}

const (
	protocolOperationEventName = "protocol_operation"
	contentDecisionEventName   = "sender_content_decision"
	responseReturnedEventName  = "protocol_response_send_returned"
	protocolErrorEventName     = "protocol_error_received"
	terminalSendEventName      = "sender_terminal_send_observed"
	sessionTerminatedEventName = "sender_session_terminated"
)

func protocolDiagnosticFields(value interface {
	Correlation() ProtocolObservationCorrelation
	ObservedAt() time.Time
}) diagnostics.Fields {
	return diagnostics.Fields{
		"observed_at":  diagnostics.Text(value.ObservedAt().UTC().Format(time.RFC3339Nano)),
		"role":         diagnostics.Text(value.Correlation().Role.DiagnosticName()),
		"request_kind": diagnostics.Text(value.Correlation().RequestKind.DiagnosticName()),
	}
}

func protocolDiagnosticCorrelation(value interface {
	Correlation() ProtocolObservationCorrelation
}, lane session.LaneIdentity) diagnostics.Fields {
	c := value.Correlation()
	fields := diagnostics.Fields{
		"protocol_session_id": diagnostics.Identity(c.ProtocolSessionID[:]),
	}
	if !c.OperationID.IsZero() {
		fields["protocol_operation_id"] = diagnostics.Uint(c.OperationID.Uint64())
	}
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
		fields["send"] = diagnostics.Object(diagnostics.Fields{
			"status": diagnostics.Text(value.SendStatus.DiagnosticName()),
		})
	}
	if value.HasDeadline {
		fields["deadline_remaining_ms"] = diagnostics.Uint(value.DeadlineRemainingMillis)
	}
	if wait := value.BlockWait; wait.Phase != "" {
		fields["block_wait"] = diagnostics.Object(diagnostics.Fields{
			"phase":          diagnostics.Text(string(wait.Phase)),
			"waited_ms":      diagnostics.Millis(wait.Waited),
			"queue_progress": diagnostics.Uint(wait.QueueProgress),
		})
	}
	if scheduling := value.RequestScheduling; scheduling.Expected != 0 {
		fields["request_scheduling"] = diagnostics.Object(diagnostics.Fields{
			"expected_ms":       diagnostics.Millis(scheduling.Expected),
			"response_ms":       diagnostics.Millis(scheduling.Response),
			"queued_content_ms": diagnostics.Millis(scheduling.QueuedContent),
			"pending_requests":  diagnostics.Number(float64(scheduling.Pending)),
		})
	}
	return diagnostics.New(protocolOperationEventName, fields).At(value.ObservedAt()).Correlate(protocolDiagnosticCorrelation(value, value.Lane))
}

func (value SenderContentDecision) DiagnosticRecord() diagnostics.Record {
	fields := protocolDiagnosticFields(value)
	fields["content_decision"] = value.Decision().DiagnosticValue()
	lane, _ := value.Lane()
	return diagnostics.New(contentDecisionEventName, fields).At(value.ObservedAt()).Correlate(protocolDiagnosticCorrelation(value, lane))
}

func (value ResponseSendObserved) DiagnosticRecord() diagnostics.Record {
	fields := protocolDiagnosticFields(value)
	fields["response_sequence"] = diagnostics.Uint(value.ResponseSequence())
	fields["response_kind"] = diagnostics.Text(value.ResponseKind().DiagnosticName())
	fields["protocol_error"] = value.Content().DiagnosticValue()
	fields["send_status"] = diagnostics.Text(value.Status().DiagnosticName())
	fields["attempts"] = diagnostics.Number(float64(value.Attempts()))
	return diagnostics.New(responseReturnedEventName, fields).At(value.ObservedAt()).Correlate(protocolDiagnosticCorrelation(value, value.Lane()))
}

func (value ReceivedProtocolError) DiagnosticRecord() diagnostics.Record {
	fields := protocolDiagnosticFields(value)
	fields["protocol_error"] = value.Content().DiagnosticValue()
	return diagnostics.New(protocolErrorEventName, fields).At(value.ObservedAt()).Correlate(protocolDiagnosticCorrelation(value, value.Lane()))
}
