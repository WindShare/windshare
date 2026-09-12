package clievent

type ProtocolRole uint8

const (
	ProtocolRoleReceiver ProtocolRole = iota + 1
	ProtocolRoleSender
)

func (value ProtocolRole) Name() (string, bool) {
	names := [...]string{"", "receiver", "sender"}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type ProtocolOperationStage uint8

const (
	ProtocolOperationReceiverCompleted ProtocolOperationStage = iota + 1
	ProtocolOperationReceiverFailed
	ProtocolOperationReceiverEnded
	ProtocolOperationSenderRequestReceived
	ProtocolOperationSenderResponseSettled
	ProtocolOperationSenderContentDecision
	ProtocolOperationReceiverWaitingActiveCapacity
	ProtocolOperationReceiverWaitingRetainedCapacity
	ProtocolOperationReceiverAdmissionReady
)

func (value ProtocolOperationStage) Name() (string, bool) {
	names := [...]string{
		"", "receiver_completed", "receiver_failed", "receiver_ended",
		"sender_request_received", "sender_response_settled", "sender_content_decision",
		"receiver_waiting_active_capacity", "receiver_waiting_retained_capacity", "receiver_admission_ready",
	}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type ProtocolMessageKind uint8

const (
	ProtocolMessageListChildren ProtocolMessageKind = iota + 1
	ProtocolMessageCatalogResult
	ProtocolMessageOpenRevisions
	ProtocolMessageOpenResults
	ProtocolMessageRenewLease
	ProtocolMessageReleaseLease
	ProtocolMessageRequestBlocks
	ProtocolMessageBlockFragment
	ProtocolMessageCancel
	ProtocolMessageOperationError
	ProtocolMessageSessionTerminal
	ProtocolMessageLaneAttach
	ProtocolMessageScanProgress
	ProtocolMessageOperationComplete
	ProtocolMessageLeaseResult
	ProtocolMessagePeerOffer
	ProtocolMessagePeerAnswer
	ProtocolMessagePeerCandidate
)

func (value ProtocolMessageKind) Name() (string, bool) {
	names := [...]string{
		"", "list_children", "catalog_result", "open_revisions", "open_results",
		"renew_lease", "release_lease", "request_blocks", "block_fragment", "cancel",
		"operation_error", "session_terminal", "lane_attach", "scan_progress",
		"operation_complete", "lease_result", "peer_offer", "peer_answer", "peer_candidate",
	}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

func (value ProtocolMessageKind) Request() bool {
	switch value {
	case ProtocolMessageListChildren, ProtocolMessageOpenRevisions,
		ProtocolMessageRenewLease, ProtocolMessageReleaseLease,
		ProtocolMessageRequestBlocks, ProtocolMessageLaneAttach, ProtocolMessagePeerOffer:
		return true
	default:
		return false
	}
}

type ProtocolSendOutcome uint8

const (
	ProtocolSendUnknown ProtocolSendOutcome = iota
	ProtocolSendDelivered
	ProtocolSendDropped
)

func (value ProtocolSendOutcome) Name() (string, bool) {
	names := [...]string{"unknown", "delivered", "dropped"}
	if int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type ProtocolOperationCause uint8

const (
	ProtocolOperationCauseNone ProtocolOperationCause = iota
	ProtocolOperationCauseCanceled
	ProtocolOperationCauseDeadline
	ProtocolOperationCauseRuntimeClosed
	ProtocolOperationCauseLaneUnavailable
	ProtocolOperationCauseWriterStopped
	ProtocolOperationCauseOperationClosed
	ProtocolOperationCauseProtocolFailure
)

func (value ProtocolOperationCause) Name() (string, bool) {
	names := [...]string{
		"none", "canceled", "deadline", "runtime_closed", "lane_unavailable",
		"writer_stopped", "operation_closed", "protocol_failure",
	}
	if int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type ProtocolFailureScope uint8

const (
	ProtocolFailureDirectory ProtocolFailureScope = iota + 1
	ProtocolFailureRevision
	ProtocolFailureBlock
	ProtocolFailurePeer
)

func (value ProtocolFailureScope) Name() (string, bool) {
	names := [...]string{"", "directory", "revision", "block", "peer"}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

const (
	protocolFailureRetryAfterMinMillis uint32 = 1
	protocolFailureRetryAfterMaxMillis uint32 = 30_000
)

type ProtocolFailureSettlementKind uint8

const (
	ProtocolFailureReceivedAuthenticated ProtocolFailureSettlementKind = iota + 1
	ProtocolFailureResponseSend
)

func (value ProtocolFailureSettlementKind) Name() (string, bool) {
	names := [...]string{"", "received_authenticated", "response_send"}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type ProtocolFailureResponseSendSettlement struct {
	Admitted bool
	Settled  bool
	Outcome  ProtocolSendOutcome
}

type ProtocolFailureSettlement struct {
	kind     ProtocolFailureSettlementKind
	admitted bool
	settled  bool
	outcome  ProtocolSendOutcome
}

func (value ProtocolFailureSettlement) Kind() ProtocolFailureSettlementKind {
	return value.kind
}

func (value ProtocolFailureSettlement) ResponseSend() (
	ProtocolFailureResponseSendSettlement,
	bool,
) {
	if value.kind != ProtocolFailureResponseSend {
		return ProtocolFailureResponseSendSettlement{}, false
	}
	return ProtocolFailureResponseSendSettlement{
		Admitted: value.admitted,
		Settled:  value.settled,
		Outcome:  value.outcome,
	}, true
}

// ProtocolFailureSpec is deliberately text-free. Authenticated provider
// messages stay below command projection because even reviewed diagnostics do
// not need them to correlate or classify an operation failure.
type ProtocolFailureSpec struct {
	RequestKind       ProtocolMessageKind
	WireScope         ProtocolFailureScope
	WireCode          uint16
	Retryable         bool
	RetryAfterMillis  uint32
	HasRetryAfter     bool
	ProtocolSession   ProtocolSessionID
	ProtocolOperation ProtocolOperationID
	Lane              LaneIdentity
	HasLane           bool
}

type ProtocolFailure struct {
	requestKind       ProtocolMessageKind
	wireScope         ProtocolFailureScope
	wireCode          uint16
	retryable         bool
	retryAfterMillis  uint32
	hasRetryAfter     bool
	protocolSession   ProtocolSessionID
	protocolOperation ProtocolOperationID
	lane              LaneIdentity
	hasLane           bool
	settlement        ProtocolFailureSettlement
}

func NewReceivedAuthenticatedProtocolFailure(spec ProtocolFailureSpec) (ProtocolFailure, error) {
	return newProtocolFailure(spec, ProtocolFailureSettlement{
		kind: ProtocolFailureReceivedAuthenticated,
	})
}

func NewResponseSendProtocolFailure(
	spec ProtocolFailureSpec,
	response ProtocolFailureResponseSendSettlement,
) (ProtocolFailure, error) {
	if !validProtocolFailureResponseSend(response) {
		return ProtocolFailure{}, ErrInvalidEvent
	}
	return newProtocolFailure(spec, ProtocolFailureSettlement{
		kind:     ProtocolFailureResponseSend,
		admitted: response.Admitted,
		settled:  response.Settled,
		outcome:  response.Outcome,
	})
}

func newProtocolFailure(
	spec ProtocolFailureSpec,
	settlement ProtocolFailureSettlement,
) (ProtocolFailure, error) {
	if !validProtocolFailureSpec(spec) || !validProtocolFailureSettlement(settlement) {
		return ProtocolFailure{}, ErrInvalidEvent
	}
	return ProtocolFailure{
		requestKind: spec.RequestKind, wireScope: spec.WireScope,
		wireCode: spec.WireCode, retryable: spec.Retryable,
		retryAfterMillis: spec.RetryAfterMillis, hasRetryAfter: spec.HasRetryAfter,
		protocolSession: spec.ProtocolSession, protocolOperation: spec.ProtocolOperation,
		lane: spec.Lane, hasLane: spec.HasLane, settlement: settlement,
	}, nil
}

func validProtocolFailureSpec(spec ProtocolFailureSpec) bool {
	_, scopeOK := spec.WireScope.Name()
	return spec.RequestKind.Request() && scopeOK &&
		spec.ProtocolSession.Valid() && spec.ProtocolOperation.Valid() &&
		spec.HasLane == spec.Lane.Valid() &&
		spec.Retryable == spec.HasRetryAfter &&
		(spec.HasRetryAfter || spec.RetryAfterMillis == 0) &&
		(!spec.HasRetryAfter || spec.RetryAfterMillis >= protocolFailureRetryAfterMinMillis &&
			spec.RetryAfterMillis <= protocolFailureRetryAfterMaxMillis)
}

func validProtocolFailureSettlement(value ProtocolFailureSettlement) bool {
	switch value.kind {
	case ProtocolFailureReceivedAuthenticated:
		return !value.admitted && !value.settled && value.outcome == ProtocolSendUnknown
	case ProtocolFailureResponseSend:
		return validProtocolFailureResponseSend(ProtocolFailureResponseSendSettlement{
			Admitted: value.admitted,
			Settled:  value.settled,
			Outcome:  value.outcome,
		})
	default:
		return false
	}
}

func validProtocolFailureResponseSend(value ProtocolFailureResponseSendSettlement) bool {
	switch value.Outcome {
	case ProtocolSendUnknown:
		return true
	case ProtocolSendDelivered:
		return value.Settled && value.Admitted
	case ProtocolSendDropped:
		return value.Settled
	default:
		return false
	}
}

func (value ProtocolFailure) IsZero() bool { return value.settlement.kind == 0 }

func (value ProtocolFailure) Valid() bool {
	return validProtocolFailureSpec(ProtocolFailureSpec{
		RequestKind: value.requestKind, WireScope: value.wireScope,
		WireCode: value.wireCode, Retryable: value.retryable,
		RetryAfterMillis: value.retryAfterMillis, HasRetryAfter: value.hasRetryAfter,
		ProtocolSession: value.protocolSession, ProtocolOperation: value.protocolOperation,
		Lane: value.lane, HasLane: value.hasLane,
	}) && validProtocolFailureSettlement(value.settlement)
}

func (value ProtocolFailure) RequestKind() ProtocolMessageKind { return value.requestKind }
func (value ProtocolFailure) WireScope() ProtocolFailureScope  { return value.wireScope }
func (value ProtocolFailure) WireCode() uint16                 { return value.wireCode }
func (value ProtocolFailure) Retryable() bool                  { return value.retryable }
func (value ProtocolFailure) RetryAfterMillis() (uint32, bool) {
	return value.retryAfterMillis, value.hasRetryAfter
}
func (value ProtocolFailure) ProtocolSessionID() ProtocolSessionID {
	return value.protocolSession
}
func (value ProtocolFailure) ProtocolOperationID() ProtocolOperationID {
	return value.protocolOperation
}
func (value ProtocolFailure) Lane() (LaneIdentity, bool) {
	return value.lane, value.hasLane
}
func (value ProtocolFailure) Settlement() ProtocolFailureSettlement { return value.settlement }

type SenderContentDecisionKind uint8

const (
	SenderContentCapacityBusy SenderContentDecisionKind = iota + 1
	SenderContentLeaseRelinquished
	SenderContentLeaseUndelivered
	SenderContentLeaseDetached
	SenderContentBlockLeaseReleased
	SenderContentBlockLeaseNotOwned
	SenderContentBlockLeaseExpired
	SenderContentBlockLeaseInvalid
)

func (value SenderContentDecisionKind) Name() (string, bool) {
	names := [...]string{"", "capacity_busy", "lease_relinquished", "lease_undelivered", "lease_detached",
		"block_lease_released", "block_lease_not_owned", "block_lease_expired", "block_lease_invalid"}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

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

type ProtocolOperationSpec struct {
	Command                 Command
	Role                    ProtocolRole
	Stage                   ProtocolOperationStage
	ProtocolSession         ProtocolSessionID
	ProtocolOperation       ProtocolOperationID
	RequestKind             ProtocolMessageKind
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
	Failure                 ProtocolFailure
	Cause                   ProtocolOperationCause
	ContentDecision         SenderContentDecision
}

type ProtocolOperationObserved struct{ spec ProtocolOperationSpec }

func NewProtocolOperationObserved(spec ProtocolOperationSpec) (ProtocolOperationObserved, error) {
	if err := validateProtocolOperationSpec(spec); err != nil {
		return ProtocolOperationObserved{}, err
	}
	return ProtocolOperationObserved{spec: spec}, nil
}

func validProtocolOperationSpec(spec ProtocolOperationSpec) bool {
	return validateProtocolOperationSpec(spec) == nil
}

func validateProtocolOperationSpec(spec ProtocolOperationSpec) error {
	_, roleOK := spec.Role.Name()
	_, stageOK := spec.Stage.Name()
	_, requestOK := spec.RequestKind.Name()
	_, responseOK := spec.ResponseKind.Name()
	_, sendOK := spec.SendOutcome.Name()
	_, causeOK := spec.Cause.Name()
	protocolFailureOK := validProtocolOperationFailure(spec)
	contentDecisionOK := spec.ContentDecision.Valid()
	if spec.Stage != ProtocolOperationSenderContentDecision &&
		(contentDecisionOK || spec.ContentDecision != (SenderContentDecision{})) {
		return EventContractError{Field: "content_decision", Rule: "only_on_sender_content_decision"}
	}
	switch {
	case !spec.Command.Valid():
		return EventContractError{Field: "command", Rule: "known_enum"}
	case !roleOK:
		return EventContractError{Field: "role", Rule: "known_enum"}
	case !stageOK:
		return EventContractError{Field: "stage", Rule: "known_enum"}
	case !requestOK || !spec.RequestKind.Request():
		return EventContractError{Field: "request_kind", Rule: "request_message"}
	case !sendOK:
		return EventContractError{Field: "send_outcome", Rule: "known_enum"}
	case !causeOK:
		return EventContractError{Field: "cause", Rule: "known_enum"}
	case !protocolFailureOK:
		return EventContractError{Field: "protocol_failure", Rule: "matching_operation_lane_and_settlement"}
	case !spec.ProtocolSession.Valid():
		return EventContractError{Field: "protocol_session_id", Rule: "nonzero_16_bytes"}
	case !spec.ProtocolOperation.Valid():
		return EventContractError{Field: "protocol_operation_id", Rule: "nonzero_16_bytes"}
	case spec.HasLane != spec.Lane.Valid():
		return EventContractError{Field: "lane", Rule: "presence_matches_identity"}
	case spec.HasResponse != responseOK:
		return EventContractError{Field: "response_kind", Rule: "presence_matches_kind"}
	case !spec.HasDeadline && spec.DeadlineRemainingMillis != 0:
		return EventContractError{Field: "deadline", Rule: "value_requires_presence"}
	case !spec.HasSend && (spec.SendSettled || spec.SendAdmitted || spec.SendOutcome != ProtocolSendUnknown):
		return EventContractError{Field: "send", Rule: "settlement_requires_presence"}
	}
	validStage := false
	switch spec.Stage {
	case ProtocolOperationReceiverWaitingActiveCapacity, ProtocolOperationReceiverWaitingRetainedCapacity,
		ProtocolOperationReceiverAdmissionReady:
		validStage = spec.Command == CommandGet && spec.Role == ProtocolRoleReceiver &&
			!spec.HasResponse && !spec.HasSend && spec.Cause == ProtocolOperationCauseNone
	case ProtocolOperationReceiverCompleted:
		validStage = spec.Command == CommandGet && spec.Role == ProtocolRoleReceiver &&
			spec.HasResponse && spec.ResponseCount != 0 && spec.Cause == ProtocolOperationCauseNone
	case ProtocolOperationReceiverFailed:
		validStage = spec.Command == CommandGet && spec.Role == ProtocolRoleReceiver &&
			spec.Cause != ProtocolOperationCauseNone
	case ProtocolOperationReceiverEnded:
		validStage = spec.Command == CommandGet && spec.Role == ProtocolRoleReceiver &&
			spec.Cause == ProtocolOperationCauseNone
	case ProtocolOperationSenderRequestReceived:
		validStage = !contentDecisionOK && spec.ContentDecision == (SenderContentDecision{}) &&
			spec.Command == CommandShare && spec.Role == ProtocolRoleSender &&
			!spec.HasResponse && !spec.HasSend && spec.Cause == ProtocolOperationCauseNone
	case ProtocolOperationSenderResponseSettled:
		validStage = !contentDecisionOK && spec.ContentDecision == (SenderContentDecision{}) &&
			spec.Command == CommandShare && spec.Role == ProtocolRoleSender && spec.HasResponse
	case ProtocolOperationSenderContentDecision:
		validStage = contentDecisionOK && spec.Command == CommandShare && spec.Role == ProtocolRoleSender &&
			!spec.HasResponse && !spec.HasSend && spec.Cause == ProtocolOperationCauseNone && spec.Failure.IsZero()
	}
	if !validStage {
		stage, _ := spec.Stage.Name()
		return EventContractError{Field: "stage_fields", Rule: stage}
	}
	return nil
}

func validProtocolOperationFailure(spec ProtocolOperationSpec) bool {
	failure := spec.Failure
	if failure.IsZero() {
		return failure == (ProtocolFailure{})
	}
	if !failure.Valid() || !spec.HasResponse || spec.ResponseKind != ProtocolMessageOperationError ||
		failure.ProtocolSessionID() != spec.ProtocolSession ||
		failure.ProtocolOperationID() != spec.ProtocolOperation ||
		failure.RequestKind() != spec.RequestKind {
		return false
	}
	failureLane, failureHasLane := failure.Lane()
	if failureHasLane != spec.HasLane || failureHasLane && failureLane != spec.Lane {
		return false
	}
	switch failure.Settlement().Kind() {
	case ProtocolFailureReceivedAuthenticated:
		return spec.Command == CommandGet && spec.Role == ProtocolRoleReceiver &&
			spec.Stage == ProtocolOperationReceiverFailed &&
			spec.Cause == ProtocolOperationCauseProtocolFailure
	case ProtocolFailureResponseSend:
		response, present := failure.Settlement().ResponseSend()
		return present && spec.Command == CommandShare && spec.Role == ProtocolRoleSender &&
			spec.Stage == ProtocolOperationSenderResponseSettled && spec.HasSend &&
			spec.SendAdmitted == response.Admitted && spec.SendSettled == response.Settled &&
			spec.SendOutcome == response.Outcome
	default:
		return false
	}
}

func (ProtocolOperationObserved) event()                              {}
func (value ProtocolOperationObserved) Command() Command              { return value.spec.Command }
func (ProtocolOperationObserved) Level() Level                        { return LevelDebug }
func (value ProtocolOperationObserved) Role() ProtocolRole            { return value.spec.Role }
func (value ProtocolOperationObserved) Stage() ProtocolOperationStage { return value.spec.Stage }
func (value ProtocolOperationObserved) ProtocolSessionID() ProtocolSessionID {
	return value.spec.ProtocolSession
}
func (value ProtocolOperationObserved) ProtocolOperationID() ProtocolOperationID {
	return value.spec.ProtocolOperation
}
func (value ProtocolOperationObserved) RequestKind() ProtocolMessageKind {
	return value.spec.RequestKind
}
func (value ProtocolOperationObserved) ResponseKind() (ProtocolMessageKind, bool) {
	return value.spec.ResponseKind, value.spec.HasResponse
}
func (value ProtocolOperationObserved) Lane() (LaneIdentity, bool) {
	return value.spec.Lane, value.spec.HasLane
}
func (value ProtocolOperationObserved) Send() (ProtocolSendOutcome, bool, bool, bool) {
	return value.spec.SendOutcome, value.spec.SendSettled, value.spec.SendAdmitted, value.spec.HasSend
}
func (value ProtocolOperationObserved) ResponseCount() uint64 { return value.spec.ResponseCount }
func (value ProtocolOperationObserved) DeadlineRemainingMillis() (uint64, bool) {
	return value.spec.DeadlineRemainingMillis, value.spec.HasDeadline
}
func (value ProtocolOperationObserved) OperationElapsedMillis() uint64 {
	return value.spec.OperationElapsedMillis
}
func (value ProtocolOperationObserved) UsableLanesAtSelection() uint32 {
	return value.spec.UsableLanesAtSelection
}
func (value ProtocolOperationObserved) UsableLanesAtSettlement() uint32 {
	return value.spec.UsableLanesAtSettlement
}
func (value ProtocolOperationObserved) Failure() (ProtocolFailure, bool) {
	return value.spec.Failure, !value.spec.Failure.IsZero()
}
func (value ProtocolOperationObserved) Cause() ProtocolOperationCause { return value.spec.Cause }
func (value ProtocolOperationObserved) ContentDecision() (SenderContentDecision, bool) {
	return value.spec.ContentDecision, value.spec.ContentDecision.Valid()
}
func (value ProtocolOperationObserved) Accept(visitor Visitor) error {
	return acceptProtocolOperationObserved(visitor, value)
}
