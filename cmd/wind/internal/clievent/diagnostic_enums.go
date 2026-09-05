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
