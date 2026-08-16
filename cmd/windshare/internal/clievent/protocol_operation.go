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
)

func (value ProtocolOperationStage) Name() (string, bool) {
	names := [...]string{
		"", "receiver_completed", "receiver_failed", "receiver_ended",
		"sender_request_received", "sender_response_settled",
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
	Cause                   ProtocolOperationCause
}

type ProtocolOperationObserved struct{ spec ProtocolOperationSpec }

func NewProtocolOperationObserved(spec ProtocolOperationSpec) (ProtocolOperationObserved, error) {
	if !validProtocolOperationSpec(spec) {
		return ProtocolOperationObserved{}, ErrInvalidEvent
	}
	return ProtocolOperationObserved{spec: spec}, nil
}

func validProtocolOperationSpec(spec ProtocolOperationSpec) bool {
	_, roleOK := spec.Role.Name()
	_, stageOK := spec.Stage.Name()
	_, requestOK := spec.RequestKind.Name()
	_, responseOK := spec.ResponseKind.Name()
	_, sendOK := spec.SendOutcome.Name()
	_, causeOK := spec.Cause.Name()
	if !spec.Command.Valid() || !roleOK || !stageOK || !requestOK || !spec.RequestKind.Request() ||
		!sendOK || !causeOK || !spec.ProtocolSession.Valid() || !spec.ProtocolOperation.Valid() ||
		spec.HasLane != spec.Lane.Valid() || spec.HasResponse != responseOK ||
		(!spec.HasDeadline && spec.DeadlineRemainingMillis != 0) ||
		(!spec.HasSend && (spec.SendSettled || spec.SendAdmitted || spec.SendOutcome != ProtocolSendUnknown)) {
		return false
	}
	switch spec.Stage {
	case ProtocolOperationReceiverCompleted:
		return spec.Command == CommandGet && spec.Role == ProtocolRoleReceiver &&
			spec.HasResponse && spec.ResponseCount != 0 && spec.Cause == ProtocolOperationCauseNone
	case ProtocolOperationReceiverFailed:
		return spec.Command == CommandGet && spec.Role == ProtocolRoleReceiver &&
			spec.Cause != ProtocolOperationCauseNone
	case ProtocolOperationReceiverEnded:
		return spec.Command == CommandGet && spec.Role == ProtocolRoleReceiver &&
			spec.Cause == ProtocolOperationCauseNone
	case ProtocolOperationSenderRequestReceived:
		return spec.Command == CommandShare && spec.Role == ProtocolRoleSender &&
			!spec.HasResponse && !spec.HasSend && spec.Cause == ProtocolOperationCauseNone
	case ProtocolOperationSenderResponseSettled:
		return spec.Command == CommandShare && spec.Role == ProtocolRoleSender && spec.HasResponse
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
func (value ProtocolOperationObserved) Cause() ProtocolOperationCause { return value.spec.Cause }
func (value ProtocolOperationObserved) Accept(visitor Visitor) error {
	return acceptProtocolOperationObserved(visitor, value)
}
