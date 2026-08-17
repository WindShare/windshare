package clievent

type RelayLifecycleStage uint8

const (
	RelayTerminalReserved RelayLifecycleStage = iota + 1
	RelaySendAdmitted
	RelaySendRejected
	RelaySendRolledBack
	RelayRetirementDeferred
	RelayRetired
	RelayTerminalSettled
	RelayLinkRetiring
	RelayLinkClosed
	RelayTraceDropped
)

func (value RelayLifecycleStage) Name() (string, bool) {
	names := [...]string{
		"", "terminal_reserved", "send_admitted", "send_rejected", "send_rolled_back",
		"retirement_deferred", "retired", "terminal_settled", "link_retiring", "link_closed", "trace_dropped",
	}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type RelayRetirementSource uint8

const (
	RelayRetirementNone RelayRetirementSource = iota
	RelayRetirementLocalClose
	RelayRetirementTerminal
	RelayRetirementSession
	RelayRetirementLinkClose
	RelayRetirementLinkFailure
	RelayRetirementIngressFailure
)

func (value RelayRetirementSource) Name() (string, bool) {
	names := [...]string{
		"none", "local_close", "terminal", "relay_session", "link_close", "link_failure", "ingress_failure",
	}
	if int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type RelayLifecycleCause uint8

const (
	RelayCauseNone RelayLifecycleCause = iota
	RelayCauseCanceled
	RelayCauseDeadline
	RelayCauseFrameBounds
	RelayCauseEgressOverflow
	RelayCauseIngressOverflow
	RelayCauseSessionRetired
	RelayCauseProtocol
	RelayCauseClosed
	RelayCauseTransport
)

func (value RelayLifecycleCause) Name() (string, bool) {
	names := [...]string{
		"none", "canceled", "deadline", "frame_bounds", "egress_overflow", "ingress_overflow",
		"session_retired", "protocol", "closed", "transport",
	}
	if int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type WebRTCOperation uint8

const (
	WebRTCChannel WebRTCOperation = iota + 1
	WebRTCSend
	WebRTCSendTerminal
)

func (value WebRTCOperation) Name() (string, bool) {
	names := [...]string{"", "channel", "send", "send_terminal"}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type WebRTCTransition uint8

const (
	WebRTCSendAccepted WebRTCTransition = iota + 1
	WebRTCSendRejected
	WebRTCSendRetired
	WebRTCRemoteTerminalReserved
	WebRTCTerminationPending
	WebRTCClosedClean
	WebRTCClosedFailed
	WebRTCTraceDropped
)

func (value WebRTCTransition) Name() (string, bool) {
	names := [...]string{
		"", "send_accepted", "send_rejected", "send_retired", "remote_terminal_reserved",
		"termination_pending", "closed_clean", "closed_failed", "trace_dropped",
	}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type WebRTCTerminalState uint8

const (
	WebRTCTerminalNone WebRTCTerminalState = iota
	WebRTCTerminalLocalPending
	WebRTCTerminalRemotePending
)

func (value WebRTCTerminalState) Name() (string, bool) {
	names := [...]string{"none", "local_pending", "remote_pending"}
	if int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type WebRTCLifecycleCause uint8

const (
	WebRTCCauseNone WebRTCLifecycleCause = iota
	WebRTCCauseCanceled
	WebRTCCauseDeadline
	WebRTCCauseNotOpen
	WebRTCCauseNaturalRetirement
	WebRTCCauseRemoteClosed
	WebRTCCauseTerminalUnacknowledged
	WebRTCCausePeerProtocol
	WebRTCCauseTransport
	WebRTCCauseOther
)

func (value WebRTCLifecycleCause) Name() (string, bool) {
	names := [...]string{
		"none", "canceled", "deadline", "not_open", "natural_retirement", "remote_closed",
		"terminal_unacknowledged", "peer_protocol", "transport", "other",
	}
	if int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type PeerAttemptStage uint8

const (
	PeerAttemptStarted PeerAttemptStage = iota + 1
	PeerOfferReceived
	PeerAnswerCreated
	PeerAnswerSent
	PeerDataChannelOpen
	PeerLaneAdmissionStarted
	PeerAttemptAdmitted
	PeerAttemptFailed
)

func (value PeerAttemptStage) Name() (string, bool) {
	names := [...]string{
		"", "started", "offer_received", "answer_created", "answer_sent", "datachannel_open",
		"lane_admission_started", "admitted", "failed",
	}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type PeerFailureScope uint8

const (
	PeerFailureAttempt PeerFailureScope = iota + 1
	PeerFailureSession
)

func (value PeerFailureScope) Name() (string, bool) {
	switch value {
	case PeerFailureAttempt:
		return "attempt", true
	case PeerFailureSession:
		return "session", true
	default:
		return "", false
	}
}

type TransferLifecycleStage uint8

const (
	TransferDiscoveryStarted TransferLifecycleStage = iota + 1
	TransferGenerationCommitted
	TransferDiscoveryCompleted
	TransferAdmissionStarted
	TransferAdmissionCompleted
	TransferDirectoryAdmitted
	TransferDirectoryFinalized
	TransferFileEnqueued
	TransferFileStarted
	TransferFileAdmitted
	TransferFileFirstWrite
	TransferFileSettled
	TransferJobSettled
)

func (value TransferLifecycleStage) Name() (string, bool) {
	names := [...]string{
		"", "discovery_started", "generation_committed", "discovery_completed", "admission_started",
		"admission_completed", "directory_admitted", "directory_finalized", "file_enqueued",
		"file_started", "file_admitted", "file_first_write", "file_settled", "job_settled",
	}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type FileSelectionDecision uint8

const (
	FileSelectionNone FileSelectionDecision = iota
	FileSelectionInherited
	FileSelectionNodeOverride
	FileSelectionCatalogPathTarget
)

func (value FileSelectionDecision) Name() (string, bool) {
	names := [...]string{"none", "inherited", "node_override", "catalog_path_target"}
	if int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type FileSettlement uint8

const (
	FileSettlementNone FileSettlement = iota
	FilePublished
	FilePaused
	FileCollision
	FileItemBlocked
	FileFailed
)

func (value FileSettlement) Name() (string, bool) {
	names := [...]string{"none", "published", "paused", "collision", "item_blocked", "failed"}
	if int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type TreeSettlement uint8

const (
	TreeSettlementNone TreeSettlement = iota
	TreeSettlementSuccess
	TreeSettlementPartial
	TreeSettlementPaused
	TreeSettlementFailed
)

func (value TreeSettlement) Name() (string, bool) {
	names := [...]string{"none", "success", "partial", "paused", "failed"}
	if int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type FilesystemOutputOperation uint8

const (
	FilesystemCertified FilesystemOutputOperation = iota + 1
	FilesystemFeatureProbeCompleted
	FilesystemCheckpointNamespaceOpened
	FilesystemNativeLock
	FilesystemSessionOpened
	FilesystemCheckpointReconciled
	FilesystemRuntimeDecision
)

func (value FilesystemOutputOperation) Name() (string, bool) {
	names := [...]string{
		"", "certified", "feature_probe_completed", "checkpoint_namespace_opened", "native_lock",
		"session_opened", "checkpoint_reconciled", "runtime_decision",
	}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type SenderTerminalTransport uint8

const (
	SenderTerminalAccepted SenderTerminalTransport = iota + 1
	SenderTerminalNotReached
	SenderTerminalUnsettled
	SenderTerminalRejected
	SenderTerminalRetired
)

func (value SenderTerminalTransport) Name() (string, bool) {
	names := [...]string{
		"", "accepted", "not_reached", "unsettled", "rejected_before_acceptance", "retired_before_acceptance",
	}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type SenderTerminalOutcome uint8

const (
	SenderTerminalDelivered SenderTerminalOutcome = iota + 1
	SenderTerminalDropped
	SenderTerminalUnknown
)

func (value SenderTerminalOutcome) Name() (string, bool) {
	names := [...]string{"", "delivered", "dropped", "unknown"}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type SenderTerminalDecision uint8

const (
	SenderTerminalDecisionDelivered SenderTerminalDecision = iota + 1
	SenderTerminalNaturalRetirement
	SenderTerminalFailed
)

func (value SenderTerminalDecision) Name() (string, bool) {
	names := [...]string{"", "delivered", "natural_retirement", "failed"}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type CatalogStorageOperation uint8

const (
	CatalogStorageCreating CatalogStorageOperation = iota + 1
	CatalogStorageCreated
	CatalogStorageRecovering
	CatalogStorageRecovered
	CatalogStorageBudgetRejected
	CatalogStorageCleaning
	CatalogStorageCleaned
)

func (value CatalogStorageOperation) Name() (string, bool) {
	names := [...]string{
		"", "creating", "created", "recovering", "recovered", "budget_rejected", "cleaning", "cleaned",
	}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type CatalogStorageCause uint8

const (
	CatalogStorageCauseNone CatalogStorageCause = iota
	CatalogStorageCauseCanceled
	CatalogStorageCauseDeadline
	CatalogStorageCauseBudget
	CatalogStorageCauseUnexpected
)

func (value CatalogStorageCause) Name() (string, bool) {
	names := [...]string{"none", "canceled", "deadline", "budget", "unexpected"}
	if int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type RootPrefetchDecision uint8

const (
	RootPrefetchAttemptStarted RootPrefetchDecision = iota + 1
	RootPrefetchYieldedToDemand
	RootPrefetchRetryScheduled
	RootPrefetchCommitted
	RootPrefetchBudgetFailed
	RootPrefetchScanFailed
	RootPrefetchStopped
)

func (value RootPrefetchDecision) Name() (string, bool) {
	names := [...]string{
		"", "attempt_started", "yielded_to_demand", "retry_scheduled", "committed", "budget_failed", "scan_failed", "stopped",
	}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}
