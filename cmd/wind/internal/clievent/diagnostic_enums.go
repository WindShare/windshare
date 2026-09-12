package clievent

type LaneRoute uint8

const (
	LaneRouteRelay LaneRoute = iota + 1
	LaneRouteDirect
)

func (value LaneRoute) Name() (string, bool) {
	names := [...]string{"", "relay", "direct"}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type ObserverLossCategory uint8

const (
	ObserverLossRelayLifecycle ObserverLossCategory = iota + 1
	ObserverLossWebRTCLifecycle
	ObserverLossSenderAttempt
	ObserverLossReceiverTermination
	ObserverLossLaneSettlement
	ObserverLossProtocolOperation
	ObserverLossTransferLifecycle
	ObserverLossFilesystemOutput
	ObserverLossCatalogStorage
	ObserverLossRootPrefetch
	ObserverLossSenderTerminalSend
	ObserverLossSenderSessionTerminal
	ObserverLossSenderCapacity
	ObserverLossSenderRevision
	ObserverLossCommandAdapter
	ObserverLossNativeConnectivity
	ObserverLossCategoryLimit
)

func (value ObserverLossCategory) Name() (string, bool) {
	names := [...]string{
		"", "relay_lifecycle", "webrtc_lifecycle", "sender_attempt", "receiver_termination",
		"lane_settlement", "protocol_operation", "transfer_lifecycle", "filesystem_output",
		"catalog_storage", "root_prefetch", "sender_terminal_send", "sender_session_terminal",
		"sender_capacity", "sender_revision", "command_adapter", "native_connectivity",
	}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type ObserverLossReason uint8

const (
	ObserverLossUnknownEnum ObserverLossReason = iota + 1
	ObserverLossInvalidIdentity
	ObserverLossInvalidStageFields
	ObserverLossEventContract
	ObserverLossAdapterCapacityTimeout
	ObserverLossTraceQueue
	ObserverLossRecorderClosed
	ObserverLossStreamCapacity
	ObserverLossReaderNotJoined
	ObserverLossPathCapacity
	ObserverLossCleanupResidue
	ObserverLossReasonLimit
)

func (value ObserverLossReason) Name() (string, bool) {
	names := [...]string{"", "unknown_enum", "invalid_identity", "invalid_stage_field_combination", "event_contract_rejection", "adapter_capacity_timeout", "trace_queue", "recorder_closed", "stream_capacity", "reader_not_joined", "path_capacity", "cleanup_residue"}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type ReceiverTerminalOwner uint8

const (
	ReceiverTerminalUnbound ReceiverTerminalOwner = iota + 1
	ReceiverTerminalLocal
	ReceiverTerminalRemote
	ReceiverTerminalRuntime
)

func (value ReceiverTerminalOwner) Name() (string, bool) {
	names := [...]string{"", "unbound", "local", "remote", "runtime"}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type ReceiverDisposition uint8

const (
	ReceiverFallbackAllowed ReceiverDisposition = iota + 1
	ReceiverSessionUnavailable
	ReceiverSessionUnsafe
)

func (value ReceiverDisposition) Name() (string, bool) {
	names := [...]string{"", "fallback_allowed", "session_unavailable", "session_unsafe"}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type ReceiverProvenance uint8

const (
	ReceiverProvenanceUnbound ReceiverProvenance = iota + 1
	ReceiverProvenanceLocalExplicitStop
	ReceiverProvenanceLocalContextEnded
	ReceiverProvenanceLocalNegotiationFailure
	ReceiverProvenanceLocalNegotiationTimeout
	ReceiverProvenanceLocalAdmissionTimeout
	ReceiverProvenanceLocalOperationContract
	ReceiverProvenanceRemoteOperationRejected
	ReceiverProvenanceRemoteUnknownControl
	ReceiverProvenanceRemoteControlMalformed
	ReceiverProvenanceRemoteFailureMalformed
	ReceiverProvenanceRemoteFailureScopeViolation
	ReceiverProvenanceRuntimeStopping
	ReceiverProvenanceSignalingAdapterContract
	ReceiverProvenanceAuthenticatedSecondAnswer
	ReceiverProvenanceAuthenticatedFinalConflict
	ReceiverProvenanceAuthenticatedAnswerBindingMismatch
	ReceiverProvenanceAuthenticatedCandidateBindingMismatch
	ReceiverProvenanceAuthenticatedContinuationAuthorityViolation
)

func (value ReceiverProvenance) Name() (string, bool) {
	names := [...]string{"", "unbound", "local_explicit_stop", "local_context_ended", "local_negotiation_failure", "local_negotiation_timeout", "local_admission_timeout", "local_operation_contract", "remote_operation_rejected", "remote_unknown_control", "remote_control_malformed", "remote_failure_malformed", "remote_failure_scope_violation", "runtime_stopping", "signaling_adapter_contract", "authenticated_second_answer", "authenticated_final_conflict", "authenticated_answer_binding_mismatch", "authenticated_candidate_binding_mismatch", "authenticated_continuation_authority_violation"}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type ReceiverLocalStopReason uint8

const (
	ReceiverLocalStopNone ReceiverLocalStopReason = iota + 1
	ReceiverLocalStopCaller
	ReceiverLocalStopOutputAdmission
	ReceiverLocalStopRuntimeSessionFailure
	ReceiverLocalStopNormalCompletion
)

func (value ReceiverLocalStopReason) Name() (string, bool) {
	names := [...]string{"", "none", "caller_stop", "output_admission_stop", "runtime_session_failure", "normal_completion"}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type ReceiverBenignComponent uint8

const (
	ReceiverBenignContextCanceled ReceiverBenignComponent = iota + 1
	ReceiverBenignLocalCancelOperationMissing
	ReceiverBenignRemoteFinalOperationMissing
)

func (value ReceiverBenignComponent) Name() (string, bool) {
	names := [...]string{"", "context_canceled", "local_cancel_operation_missing", "remote_final_operation_missing"}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type ReceiverCauseClass uint8

const (
	ReceiverCauseRuntimeClosed ReceiverCauseClass = iota + 1
	ReceiverCauseConfiguration
	ReceiverCauseOperationMissing
	ReceiverCauseNegotiationTimeout
	ReceiverCauseAdmissionTimeout
	ReceiverCauseCandidateLimit
	ReceiverCauseChannelAdmission
	ReceiverCauseEventCapacity
	ReceiverCauseNegotiation
	ReceiverCauseProtocol
	ReceiverCauseDeadlineExceeded
	ReceiverCausePeerShutdown
	ReceiverCauseChannelDrain
	ReceiverCauseUnknown
)

func (value ReceiverCauseClass) Name() (string, bool) {
	names := [...]string{"", "runtime_closed", "configuration", "operation_missing", "negotiation_timeout", "admission_timeout", "candidate_limit", "channel_admission", "event_capacity", "negotiation", "protocol", "deadline_exceeded", "peer_shutdown", "channel_drain", "unknown"}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type PeerTeardownTransition uint8

const (
	PeerTeardownShutdownInitiated PeerTeardownTransition = iota + 1
	PeerTeardownShutdownReturned
	PeerTeardownChannelDrainStarted
	PeerTeardownChannelDrainJoined
)

func (value PeerTeardownTransition) Name() (string, bool) {
	names := [...]string{"", "peer_shutdown_initiated", "peer_shutdown_returned", "channel_drain_started", "channel_drain_joined"}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

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
	ProtocolOperationReceiverWaitingActiveCapacity
	ProtocolOperationReceiverWaitingRetainedCapacity
	ProtocolOperationReceiverAdmissionReady
)

func (value ProtocolOperationStage) Name() (string, bool) {
	names := [...]string{
		"", "receiver_completed", "receiver_failed", "receiver_ended",
		"sender_request_received",
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
	ProtocolSendUninitialized ProtocolSendOutcome = iota
	ProtocolSendUnknown
	ProtocolSendTransportConfirmed
	ProtocolSendDropped
)

func (value ProtocolSendOutcome) Name() (string, bool) {
	names := [...]string{"", "unknown", "transport_confirmed", "dropped"}
	if value == 0 || int(value) >= len(names) {
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

type ProtocolErrorScope uint8

const (
	ProtocolErrorDirectory ProtocolErrorScope = iota + 1
	ProtocolErrorRevision
	ProtocolErrorBlock
	ProtocolErrorPeer
)

func (value ProtocolErrorScope) Name() (string, bool) {
	names := [...]string{"", "directory", "revision", "block", "peer"}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

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

type ResponseSendEvidence uint8

const (
	ResponseSendEvidenceUninitialized ResponseSendEvidence = iota
	ResponseSendEvidenceDefinitelyNotSent
	ResponseSendEvidenceUncertain
	ResponseSendEvidenceTransportConfirmed
)

func (value ResponseSendEvidence) Name() (string, bool) {
	names := [...]string{"uninitialized", "definitely_not_sent", "uncertain", "transport_confirmed"}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type ResponseSendEnd uint8

const (
	ResponseSendEndUninitialized ResponseSendEnd = iota
	ResponseSendEndPreparationFailed
	ResponseSendEndRouteUnavailable
	ResponseSendEndAuthorityUnavailable
	ResponseSendEndTransportConfirmed
	ResponseSendEndPolicySuppressed
	ResponseSendEndCallerCanceled
	ResponseSendEndDeadlineExceeded
	ResponseSendEndRuntimeStopped
	ResponseSendEndRetryDisallowed
	ResponseSendEndNoUsableLane
	ResponseSendEndAttemptsExhausted
	ResponseSendEndAuthorityLost
	ResponseSendEndInvalidReceipt
)

func (value ResponseSendEnd) Name() (string, bool) {
	names := [...]string{"uninitialized", "preparation_failed", "route_unavailable", "authority_unavailable", "transport_confirmed", "policy_suppressed", "caller_canceled", "deadline_exceeded", "runtime_stopped", "retry_disallowed", "no_usable_lane", "attempts_exhausted", "authority_lost", "invalid_receipt"}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type SendAttemptEnd uint8

const (
	SendAttemptEndUninitialized SendAttemptEnd = iota
	SendAttemptEndRejectedBeforeReceipt
	SendAttemptEndSettled
	SendAttemptEndWaitingEnded
)

func (value SendAttemptEnd) Name() (string, bool) {
	names := [...]string{"uninitialized", "rejected_before_receipt", "settled", "waiting_ended"}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type SendCleanupKind uint8

const (
	SendCleanupNone SendCleanupKind = iota
	SendCleanupRouteReleased
	SendCleanupOperationRetired
	SendCleanupFailed
)

func (value SendCleanupKind) Name() (string, bool) {
	names := [...]string{"none", "route_released", "operation_retired", "failed"}
	if int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type SendAttemptCauseKind uint8

const (
	SendAttemptCauseNone SendAttemptCauseKind = iota
	SendAttemptCauseCanceled
	SendAttemptCauseDeadline
	SendAttemptCauseControlQueueFull
	SendAttemptCauseDataQueueFull
	SendAttemptCauseWriterStopped
	SendAttemptCauseWriterTerminal
	SendAttemptCauseTransportFailure
	SendAttemptCausePreparationFailure
	SendAttemptCauseAdmissionFailure
)

func (value SendAttemptCauseKind) Name() (string, bool) {
	names := [...]string{"none", "canceled", "deadline", "control_queue_full", "data_queue_full", "writer_stopped", "writer_terminal", "transport_failure", "preparation_failure", "admission_failure"}
	if int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}
