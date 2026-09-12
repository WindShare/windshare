package clievent

import (
	"strings"
	"time"
	"unicode/utf8"
)

const (
	protocolErrorRetryAfterMinMillis uint32 = 1
	protocolErrorRetryAfterMaxMillis uint32 = 30_000
)

type SenderContentDecision struct {
	kind               SenderContentDecisionKind
	capacityDecisionID CapacityDecisionID
	leaseID            RevisionLeaseID
}

func NewSenderCapacityDecision(id CapacityDecisionID) (SenderContentDecision, error) {
	if !id.Valid() {
		return SenderContentDecision{}, ErrInvalidEvent
	}
	return SenderContentDecision{kind: SenderContentCapacityBusy, capacityDecisionID: id}, nil
}

func NewSenderLeaseDecision(kind SenderContentDecisionKind, leaseID RevisionLeaseID) (SenderContentDecision, error) {
	if kind < SenderContentLeaseRelinquished || kind > SenderContentBlockLeaseInvalid || !leaseID.Valid() {
		return SenderContentDecision{}, ErrInvalidEvent
	}
	return SenderContentDecision{kind: kind, leaseID: leaseID}, nil
}

func (value SenderContentDecision) Kind() SenderContentDecisionKind { return value.kind }
func (value SenderContentDecision) CapacityDecisionID() (CapacityDecisionID, bool) {
	return value.capacityDecisionID, value.kind == SenderContentCapacityBusy
}
func (value SenderContentDecision) LeaseID() (RevisionLeaseID, bool) {
	return value.leaseID, value.kind >= SenderContentLeaseRelinquished && value.kind <= SenderContentBlockLeaseInvalid
}
func (value SenderContentDecision) Valid() bool {
	_, kindOK := value.kind.Name()
	if !kindOK {
		return false
	}
	if value.kind == SenderContentCapacityBusy {
		return value.capacityDecisionID.Valid() && !value.leaseID.Valid()
	}
	return !value.capacityDecisionID.Valid() && value.leaseID.Valid()
}

type ProtocolObservationContext struct {
	Command           Command
	ObservedAt        time.Time
	Role              ProtocolRole
	ProtocolSession   ProtocolSessionID
	ProtocolOperation ProtocolOperationID
	RequestKind       ProtocolMessageKind
}

type ProtocolFact interface{ protocolFact() }
type ProtocolObservationObserved struct {
	context ProtocolObservationContext
	fact    ProtocolFact
}

func (ProtocolObservationObserved) event()                      {}
func (value ProtocolObservationObserved) Command() Command      { return value.context.Command }
func (ProtocolObservationObserved) Level() Level                { return LevelDebug }
func (value ProtocolObservationObserved) ObservedAt() time.Time { return value.context.ObservedAt }
func (value ProtocolObservationObserved) Role() ProtocolRole    { return value.context.Role }
func (value ProtocolObservationObserved) ProtocolSessionID() ProtocolSessionID {
	return value.context.ProtocolSession
}
func (value ProtocolObservationObserved) ProtocolOperationID() ProtocolOperationID {
	return value.context.ProtocolOperation
}
func (value ProtocolObservationObserved) RequestKind() ProtocolMessageKind {
	return value.context.RequestKind
}
func (value ProtocolObservationObserved) Fact() ProtocolFact { return value.fact }
func (value ProtocolObservationObserved) Accept(visitor Visitor) error {
	return acceptProtocolObservationObserved(visitor, value)
}
func validProtocolObservation(value ProtocolObservationObserved) bool {
	return validateProtocolContext(value.context) == nil && value.fact != nil
}
func validateProtocolContext(value ProtocolObservationContext) error {
	_, roleOK := value.Role.Name()
	switch {
	case !value.Command.Valid():
		return EventContractError{Field: "command", Rule: "known_enum"}
	case !roleOK:
		return EventContractError{Field: "role", Rule: "known_enum"}
	case value.ObservedAt.IsZero():
		return EventContractError{Field: "observed_at", Rule: "source_timestamp"}
	case !value.ProtocolSession.Valid():
		return EventContractError{Field: "protocol_session_id", Rule: "nonzero_16_bytes"}
	case !value.ProtocolOperation.Valid():
		return EventContractError{Field: "protocol_operation_id", Rule: "nonzero_16_bytes"}
	case value.RequestKind != 0 && !value.RequestKind.Request():
		return EventContractError{Field: "request_kind", Rule: "request_message"}
	}
	return nil
}
func newProtocolObservation(context ProtocolObservationContext, fact ProtocolFact) (ProtocolObservationObserved, error) {
	if context.RequestKind == 0 {
		if _, notStarted := fact.(ResponseSendNotStartedFact); !notStarted {
			return ProtocolObservationObserved{}, EventContractError{Field: "request_kind", Rule: "request_message"}
		}
	}
	if err := validateProtocolContext(context); err != nil {
		return ProtocolObservationObserved{}, err
	}
	context.ObservedAt = context.ObservedAt.UTC()
	return ProtocolObservationObserved{context: context, fact: fact}, nil
}

type ProtocolOperationSpec struct {
	Command                 Command
	ObservedAt              time.Time
	Role                    ProtocolRole
	ProtocolSession         ProtocolSessionID
	ProtocolOperation       ProtocolOperationID
	RequestKind             ProtocolMessageKind
	Stage                   ProtocolOperationStage
	ResponseKind            ProtocolMessageKind
	HasResponse             bool
	Lane                    LaneIdentity
	HasLane                 bool
	HasSend                 bool
	SendSettled             bool
	SendAdmitted            bool
	SendOutcome             ProtocolSendOutcome
	ResponseCount           uint64
	DeadlineRemainingMillis uint64
	HasDeadline             bool
	OperationElapsedMillis  uint64
	UsableLanesAtSelection  uint32
	UsableLanesAtSettlement uint32
	Cause                   ProtocolOperationCause
}
type ProtocolOperationFact struct{ spec ProtocolOperationSpec }

func (ProtocolOperationFact) protocolFact() {}
func NewProtocolOperationObserved(spec ProtocolOperationSpec) (ProtocolObservationObserved, error) {
	if err := validateProtocolOperationSpec(spec); err != nil {
		return ProtocolObservationObserved{}, err
	}
	return newProtocolObservation(spec.context(), ProtocolOperationFact{spec: spec})
}
func validateProtocolOperationSpec(spec ProtocolOperationSpec) error {
	if err := validateProtocolContext(spec.context()); err != nil {
		return err
	}
	_, stageOK := spec.Stage.Name()
	_, responseOK := spec.ResponseKind.Name()
	_, sendOK := spec.SendOutcome.Name()
	_, causeOK := spec.Cause.Name()
	switch {
	case !stageOK:
		return EventContractError{Field: "stage", Rule: "known_enum"}
	case !causeOK:
		return EventContractError{Field: "cause", Rule: "known_enum"}
	case spec.HasSend && !sendOK:
		return EventContractError{Field: "send_outcome", Rule: "known_enum"}
	case spec.HasLane != spec.Lane.Valid():
		return EventContractError{Field: "lane", Rule: "presence_matches_identity"}
	case spec.HasResponse != responseOK:
		return EventContractError{Field: "response_kind", Rule: "presence_matches_kind"}
	case !spec.HasDeadline && spec.DeadlineRemainingMillis != 0:
		return EventContractError{Field: "deadline", Rule: "value_requires_presence"}
	case !spec.HasSend && (spec.SendSettled || spec.SendAdmitted || spec.SendOutcome != ProtocolSendUninitialized):
		return EventContractError{Field: "send", Rule: "settlement_requires_presence"}
	}
	validStage := false
	switch spec.Stage {
	case ProtocolOperationReceiverWaitingActiveCapacity, ProtocolOperationReceiverWaitingRetainedCapacity, ProtocolOperationReceiverAdmissionReady:
		validStage = spec.Command == CommandGet && spec.Role == ProtocolRoleReceiver && !spec.HasResponse && !spec.HasSend && spec.Cause == ProtocolOperationCauseNone
	case ProtocolOperationReceiverCompleted:
		validStage = spec.Command == CommandGet && spec.Role == ProtocolRoleReceiver && spec.HasResponse && spec.ResponseCount != 0 && spec.Cause == ProtocolOperationCauseNone
	case ProtocolOperationReceiverFailed:
		validStage = spec.Command == CommandGet && spec.Role == ProtocolRoleReceiver && spec.Cause != ProtocolOperationCauseNone
	case ProtocolOperationReceiverEnded:
		validStage = spec.Command == CommandGet && spec.Role == ProtocolRoleReceiver && spec.Cause == ProtocolOperationCauseNone
	case ProtocolOperationSenderRequestReceived:
		validStage = spec.Command == CommandShare && spec.Role == ProtocolRoleSender && !spec.HasResponse && !spec.HasSend && spec.Cause == ProtocolOperationCauseNone
	}
	if !validStage {
		stage, _ := spec.Stage.Name()
		return EventContractError{Field: "stage_fields", Rule: stage}
	}
	return nil
}
func (value ProtocolOperationFact) Stage() ProtocolOperationStage { return value.spec.Stage }
func (value ProtocolOperationFact) ResponseKind() (ProtocolMessageKind, bool) {
	return value.spec.ResponseKind, value.spec.HasResponse
}
func (value ProtocolOperationFact) Lane() (LaneIdentity, bool) {
	return value.spec.Lane, value.spec.HasLane
}
func (value ProtocolOperationFact) Send() (ProtocolSendOutcome, bool, bool, bool) {
	return value.spec.SendOutcome, value.spec.SendSettled, value.spec.SendAdmitted, value.spec.HasSend
}
func (value ProtocolOperationFact) ResponseCount() uint64 { return value.spec.ResponseCount }
func (value ProtocolOperationFact) DeadlineRemainingMillis() (uint64, bool) {
	return value.spec.DeadlineRemainingMillis, value.spec.HasDeadline
}
func (value ProtocolOperationFact) OperationElapsedMillis() uint64 {
	return value.spec.OperationElapsedMillis
}
func (value ProtocolOperationFact) UsableLanesAtSelection() uint32 {
	return value.spec.UsableLanesAtSelection
}
func (value ProtocolOperationFact) UsableLanesAtSettlement() uint32 {
	return value.spec.UsableLanesAtSettlement
}
func (value ProtocolOperationFact) Cause() ProtocolOperationCause { return value.spec.Cause }

type ProtocolErrorContentSpec struct {
	WireScope        ProtocolErrorScope
	WireCode         uint16
	Retryable        bool
	RetryAfterMillis uint32
	HasRetryAfter    bool
}
type ProtocolErrorContent struct{ spec ProtocolErrorContentSpec }

func NewProtocolErrorContent(spec ProtocolErrorContentSpec) (ProtocolErrorContent, error) {
	_, ok := spec.WireScope.Name()
	if !ok {
		return ProtocolErrorContent{}, EventContractError{Field: "protocol_error.scope", Rule: "known_enum"}
	}
	if spec.Retryable != spec.HasRetryAfter || (!spec.HasRetryAfter && spec.RetryAfterMillis != 0) || (spec.HasRetryAfter && (spec.RetryAfterMillis < protocolErrorRetryAfterMinMillis || spec.RetryAfterMillis > protocolErrorRetryAfterMaxMillis)) {
		return ProtocolErrorContent{}, EventContractError{Field: "protocol_error.retry_after_ms", Rule: "retry_hint_bounds"}
	}
	return ProtocolErrorContent{spec: spec}, nil
}
func (value ProtocolErrorContent) IsZero() bool                  { return value == ProtocolErrorContent{} }
func (value ProtocolErrorContent) WireScope() ProtocolErrorScope { return value.spec.WireScope }
func (value ProtocolErrorContent) WireCode() uint16              { return value.spec.WireCode }
func (value ProtocolErrorContent) Retryable() bool               { return value.spec.Retryable }
func (value ProtocolErrorContent) RetryAfterMillis() (uint32, bool) {
	return value.spec.RetryAfterMillis, value.spec.HasRetryAfter
}

type SenderContentDecisionFact struct {
	decision SenderContentDecision
	lane     LaneIdentity
	hasLane  bool
}

func (SenderContentDecisionFact) protocolFact()                         {}
func (value SenderContentDecisionFact) Decision() SenderContentDecision { return value.decision }
func (value SenderContentDecisionFact) Lane() (LaneIdentity, bool)      { return value.lane, value.hasLane }
func NewSenderContentDecisionObserved(context ProtocolObservationContext, decision SenderContentDecision, lane LaneIdentity, hasLane bool) (ProtocolObservationObserved, error) {
	if !decision.Valid() || context.Role != ProtocolRoleSender || context.Command != CommandShare {
		return ProtocolObservationObserved{}, EventContractError{Field: "content_decision", Rule: "sender_decision"}
	}
	if hasLane != lane.Valid() {
		return ProtocolObservationObserved{}, EventContractError{Field: "lane", Rule: "presence_matches_identity"}
	}
	return newProtocolObservation(context, SenderContentDecisionFact{decision: decision, lane: lane, hasLane: hasLane})
}

type ReceivedProtocolErrorFact struct {
	content ProtocolErrorContent
	lane    LaneIdentity
}

func (ReceivedProtocolErrorFact) protocolFact()                       {}
func (value ReceivedProtocolErrorFact) Content() ProtocolErrorContent { return value.content }
func (value ReceivedProtocolErrorFact) Lane() LaneIdentity            { return value.lane }
func NewReceivedProtocolErrorObserved(context ProtocolObservationContext, content ProtocolErrorContent, lane LaneIdentity) (ProtocolObservationObserved, error) {
	if content.IsZero() || !lane.Valid() {
		return ProtocolObservationObserved{}, EventContractError{Field: "protocol_error", Rule: "received_content_and_lane"}
	}
	return newProtocolObservation(context, ReceivedProtocolErrorFact{content: content, lane: lane})
}

const MaxProtocolSendAttempts = 16

type SendAttemptSpec struct {
	Cause                   SendAttemptCause
	AttemptSequence         uint32
	Lane                    LaneIdentity
	PolicyAdmitted          bool
	Settled                 bool
	Outcome                 ProtocolSendOutcome
	TransportDisposition    SendDisposition
	HasTransportDisposition bool
	End                     SendAttemptEnd
}
type SendAttemptSnapshot struct{ spec SendAttemptSpec }

func NewSendAttemptSnapshot(spec SendAttemptSpec) (SendAttemptSnapshot, error) {
	_, outcomeOK := spec.Outcome.Name()
	_, endOK := spec.End.Name()
	_, transportOK := spec.TransportDisposition.Name()
	switch {
	case spec.AttemptSequence == 0 || spec.AttemptSequence > MaxProtocolSendAttempts:
		return SendAttemptSnapshot{}, EventContractError{Field: "attempt.attempt_sequence", Rule: "bounded_nonzero_sequence"}
	case !spec.Lane.Valid():
		return SendAttemptSnapshot{}, EventContractError{Field: "attempt.lane", Rule: "valid_lane_identity"}
	case !outcomeOK:
		return SendAttemptSnapshot{}, EventContractError{Field: "attempt.outcome", Rule: "known_enum"}
	case !endOK:
		return SendAttemptSnapshot{}, EventContractError{Field: "attempt.end", Rule: "known_enum"}
	case spec.HasTransportDisposition && !transportOK:
		return SendAttemptSnapshot{}, EventContractError{Field: "attempt.transport_disposition", Rule: "known_enum"}
	}
	return SendAttemptSnapshot{spec: spec}, nil
}
func (value SendAttemptSnapshot) AttemptSequence() uint32      { return value.spec.AttemptSequence }
func (value SendAttemptSnapshot) Lane() LaneIdentity           { return value.spec.Lane }
func (value SendAttemptSnapshot) PolicyAdmitted() bool         { return value.spec.PolicyAdmitted }
func (value SendAttemptSnapshot) Settled() bool                { return value.spec.Settled }
func (value SendAttemptSnapshot) Outcome() ProtocolSendOutcome { return value.spec.Outcome }
func (value SendAttemptSnapshot) TransportDisposition() (SendDisposition, bool) {
	return value.spec.TransportDisposition, value.spec.HasTransportDisposition
}
func (value SendAttemptSnapshot) End() SendAttemptEnd { return value.spec.End }

type ResponseSendResultSpec struct {
	Started                bool
	Evidence               ResponseSendEvidence
	End                    ResponseSendEnd
	Cleanup                SendCleanupKind
	Attempts               []SendAttemptSnapshot
	PendingAttemptSequence uint32
	HasPendingAttempt      bool
}

// The projection copies evidence verbatim; the executor remains the sole owner
// of aggregation and receipt semantics.
type ResponseSendResult struct {
	started                bool
	evidence               ResponseSendEvidence
	end                    ResponseSendEnd
	cleanup                SendCleanupKind
	attempts               [MaxProtocolSendAttempts]SendAttemptSnapshot
	attemptCount           int
	pendingAttemptSequence uint32
	hasPendingAttempt      bool
}

func NewResponseSendResult(spec ResponseSendResultSpec) (ResponseSendResult, error) {
	_, evidenceOK := spec.Evidence.Name()
	_, endOK := spec.End.Name()
	_, cleanupOK := spec.Cleanup.Name()
	switch {
	case !evidenceOK:
		return ResponseSendResult{}, EventContractError{Field: "response_result.evidence", Rule: "known_enum"}
	case !endOK:
		return ResponseSendResult{}, EventContractError{Field: "response_result.end", Rule: "known_enum"}
	case !cleanupOK:
		return ResponseSendResult{}, EventContractError{Field: "response_result.cleanup", Rule: "known_enum"}
	case len(spec.Attempts) > MaxProtocolSendAttempts:
		return ResponseSendResult{}, EventContractError{Field: "response_result.attempts", Rule: "bounded_history"}
	case spec.HasPendingAttempt && (spec.PendingAttemptSequence == 0 || spec.PendingAttemptSequence > MaxProtocolSendAttempts):
		return ResponseSendResult{}, EventContractError{Field: "response_result.pending_attempt_sequence", Rule: "bounded_nonzero_sequence"}
	}
	value := ResponseSendResult{started: spec.Started, evidence: spec.Evidence, end: spec.End, cleanup: spec.Cleanup, attemptCount: len(spec.Attempts), pendingAttemptSequence: spec.PendingAttemptSequence, hasPendingAttempt: spec.HasPendingAttempt}
	for i, attempt := range spec.Attempts {
		if _, err := NewSendAttemptSnapshot(attempt.spec); err != nil {
			return ResponseSendResult{}, err
		}
		value.attempts[i] = attempt
	}
	return value, nil
}
func (value ResponseSendResult) Started() bool                  { return value.started }
func (value ResponseSendResult) Evidence() ResponseSendEvidence { return value.evidence }
func (value ResponseSendResult) End() ResponseSendEnd           { return value.end }
func (value ResponseSendResult) Cleanup() SendCleanupKind       { return value.cleanup }
func (value ResponseSendResult) AttemptCount() int              { return value.attemptCount }
func (value ResponseSendResult) Attempt(index int) (SendAttemptSnapshot, bool) {
	if index < 0 || index >= value.attemptCount {
		return SendAttemptSnapshot{}, false
	}
	return value.attempts[index], true
}
func (value ResponseSendResult) PendingAttemptSequence() (uint32, bool) {
	return value.pendingAttemptSequence, value.hasPendingAttempt
}

type ResponseSendNotStartedFact struct {
	responseSequence uint64
	responseKind     ProtocolMessageKind
	content          ProtocolErrorContent
	result           ResponseSendResult
}
type ResponseSendReturnedFact struct {
	responseSequence uint64
	responseKind     ProtocolMessageKind
	content          ProtocolErrorContent
	result           ResponseSendResult
}

func (ResponseSendNotStartedFact) protocolFact() {}
func (ResponseSendReturnedFact) protocolFact()   {}
func NewResponseSendNotStartedObserved(context ProtocolObservationContext, sequence uint64, kind ProtocolMessageKind, content ProtocolErrorContent, result ResponseSendResult) (ProtocolObservationObserved, error) {
	if err := validateResponseSend(sequence, kind, result); err != nil {
		return ProtocolObservationObserved{}, err
	}
	if result.Started() {
		return ProtocolObservationObserved{}, EventContractError{Field: "response_result.started", Rule: "not_started_fact"}
	}
	return newProtocolObservation(context, ResponseSendNotStartedFact{sequence, kind, content, result})
}
func NewResponseSendReturnedObserved(context ProtocolObservationContext, sequence uint64, kind ProtocolMessageKind, content ProtocolErrorContent, result ResponseSendResult) (ProtocolObservationObserved, error) {
	if err := validateResponseSend(sequence, kind, result); err != nil {
		return ProtocolObservationObserved{}, err
	}
	if !result.Started() {
		return ProtocolObservationObserved{}, EventContractError{Field: "response_result.started", Rule: "returned_fact"}
	}
	return newProtocolObservation(context, ResponseSendReturnedFact{sequence, kind, content, result})
}
func validateResponseSend(sequence uint64, kind ProtocolMessageKind, result ResponseSendResult) error {
	if sequence == 0 {
		return EventContractError{Field: "response_sequence", Rule: "nonzero_sequence"}
	}
	if _, ok := kind.Name(); !ok {
		return EventContractError{Field: "response_kind", Rule: "known_enum"}
	}
	if _, ok := result.Evidence().Name(); !ok {
		return EventContractError{Field: "response_result", Rule: "initialized"}
	}
	return nil
}

type SendAttemptSettledFact struct {
	responseSequence uint64
	responseKind     ProtocolMessageKind
	attempt          SendAttemptSnapshot
}

func (SendAttemptSettledFact) protocolFact()                           {}
func (value SendAttemptSettledFact) ResponseSequence() uint64          { return value.responseSequence }
func (value SendAttemptSettledFact) ResponseKind() ProtocolMessageKind { return value.responseKind }
func (value SendAttemptSettledFact) Attempt() SendAttemptSnapshot      { return value.attempt }
func NewSendAttemptSettledObserved(context ProtocolObservationContext, sequence uint64, kind ProtocolMessageKind, attempt SendAttemptSnapshot) (ProtocolObservationObserved, error) {
	if sequence == 0 {
		return ProtocolObservationObserved{}, EventContractError{Field: "response_sequence", Rule: "nonzero_sequence"}
	}
	if _, ok := kind.Name(); !ok {
		return ProtocolObservationObserved{}, EventContractError{Field: "response_kind", Rule: "known_enum"}
	}
	if _, err := NewSendAttemptSnapshot(attempt.spec); err != nil {
		return ProtocolObservationObserved{}, err
	}
	return newProtocolObservation(context, SendAttemptSettledFact{sequence, kind, attempt})
}
func (value ResponseSendNotStartedFact) ResponseSequence() uint64          { return value.responseSequence }
func (value ResponseSendNotStartedFact) ResponseKind() ProtocolMessageKind { return value.responseKind }
func (value ResponseSendNotStartedFact) Content() ProtocolErrorContent     { return value.content }
func (value ResponseSendNotStartedFact) Result() ResponseSendResult        { return value.result }
func (value ResponseSendReturnedFact) ResponseSequence() uint64            { return value.responseSequence }
func (value ResponseSendReturnedFact) ResponseKind() ProtocolMessageKind   { return value.responseKind }
func (value ResponseSendReturnedFact) Content() ProtocolErrorContent       { return value.content }
func (value ResponseSendReturnedFact) Result() ResponseSendResult          { return value.result }

func (spec ProtocolOperationSpec) context() ProtocolObservationContext {
	return ProtocolObservationContext{Command: spec.Command, ObservedAt: spec.ObservedAt, Role: spec.Role, ProtocolSession: spec.ProtocolSession, ProtocolOperation: spec.ProtocolOperation, RequestKind: spec.RequestKind}
}

const MaximumSendAttemptCauseDetailBytes = 256

type SendAttemptCause struct {
	kind      SendAttemptCauseKind
	detail    string
	truncated bool
}

func NewSendAttemptCause(kind SendAttemptCauseKind, detail string, truncated bool) (SendAttemptCause, error) {
	if _, ok := kind.Name(); !ok {
		return SendAttemptCause{}, EventContractError{Field: "attempt.cause.kind", Rule: "known_enum"}
	}
	if !utf8.ValidString(detail) || len(detail) > MaximumSendAttemptCauseDetailBytes {
		return SendAttemptCause{}, EventContractError{Field: "attempt.cause.detail", Rule: "bounded_utf8"}
	}
	return SendAttemptCause{kind: kind, detail: strings.Clone(detail), truncated: truncated}, nil
}
func (value SendAttemptCause) Kind() SendAttemptCauseKind { return value.kind }
func (value SendAttemptCause) Detail() string             { return value.detail }
func (value SendAttemptCause) Truncated() bool            { return value.truncated }
func (value SendAttemptSnapshot) Cause() SendAttemptCause { return value.spec.Cause }
