package observation

type ObserverLossCategory uint8

const (
	ObserverLossRelayLifecycle ObserverLossCategory = iota + 1
	ObserverLossWebRTCLifecycle
	ObserverLossSenderAttempt
	ObserverLossReceiverTermination
	ObserverLossLaneSettlement
	ObserverLossProtocolOperation
	ObserverLossTransferLifecycle
	ObserverLossOutputDiagnostic
	ObserverLossCatalogStorage
	ObserverLossRootPrefetch
	ObserverLossSenderTerminalSend
	ObserverLossSenderSessionTerminal
	ObserverLossSenderCapacity
	ObserverLossSenderRevision
	ObserverLossCommandAdapter
	ObserverLossNativeConnectivity
	ObserverLossEngineObservation
	ObserverLossCategoryLimit
)

func (value ObserverLossCategory) Name() (string, bool) {
	names := [...]string{
		"", "relay_lifecycle", "webrtc_lifecycle", "sender_attempt", "receiver_termination",
		"lane_settlement", "protocol_operation", "transfer_lifecycle", "output_diagnostic",
		"catalog_storage", "root_prefetch", "sender_terminal_send", "sender_session_terminal",
		"sender_capacity", "sender_revision", "command_adapter", "native_connectivity", "engine_observation",
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

func (value ObserverLossReason) IsContractRejection() bool {
	switch value {
	case ObserverLossUnknownEnum, ObserverLossInvalidIdentity, ObserverLossInvalidStageFields, ObserverLossEventContract:
		return true
	default:
		return false
	}
}
