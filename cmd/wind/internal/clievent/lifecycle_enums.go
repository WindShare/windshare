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
	PeerNegotiationDeadlineArmed
	PeerNegotiationDeadlineExpired
	PeerOfferReceived
	PeerAnswerCreated
	PeerAnswerSent
	PeerDataChannelOpen
	PeerAdmissionDeadlineArmed
	PeerAdmissionDeadlineExpired
	PeerLaneHelloAuthenticated
	PeerAdmissionResponseSettled
	PeerAttemptAdmitted
	PeerAttemptFailed
)

func (value PeerAttemptStage) Name() (string, bool) {
	names := [...]string{
		"", "started", "negotiation-deadline-armed", "negotiation-deadline-expired",
		"offer-received", "answer-created", "answer-sent", "datachannel-open",
		"admission-deadline-armed", "admission-deadline-expired", "lane-hello-authenticated",
		"admission-response-settled", "admitted", "failed",
	}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type PeerAttemptPhase uint8

const (
	PeerPhaseNegotiation PeerAttemptPhase = iota + 1
	PeerPhaseAdmission
)

func (value PeerAttemptPhase) Name() (string, bool) {
	names := [...]string{"", "negotiation", "admission"}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type PeerAdmissionDisposition uint8

const (
	PeerAdmissionAccepted PeerAdmissionDisposition = iota + 1
	PeerAdmissionRejected
)

func (value PeerAdmissionDisposition) Name() (string, bool) {
	names := [...]string{"", "accepted", "rejected"}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type PeerResponseDelivery uint8

const (
	PeerResponseDelivered PeerResponseDelivery = iota + 1
	PeerResponseDeliveryFailed
)

func (value PeerResponseDelivery) Name() (string, bool) {
	names := [...]string{"", "delivered", "delivery-failed"}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type PeerLaneRejectionCode uint8

const (
	PeerLaneRejectUnknownSession PeerLaneRejectionCode = iota + 1
	PeerLaneRejectStaleEpoch
	PeerLaneRejectGrantConsumed
	PeerLaneRejectGrantExpired
	PeerLaneRejectAdmissionLimited
	PeerLaneRejectStopping
	PeerLaneRejectGrantMismatch
)

func (value PeerLaneRejectionCode) Name() (string, bool) {
	names := [...]string{
		"", "unknown-session", "stale-epoch", "grant-consumed", "grant-expired",
		"admission-limited", "stopping", "grant-mismatch",
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
	TransferCapacityRetryScheduled
	TransferCapacityRetryReady
	TransferCapacityRetrySucceeded
	TransferCapacityBudgetPaused
	TransferCapacityWaitCanceled
	TransferCapacityGenerationEnded
	TransferJobSettled
)

func (value TransferLifecycleStage) Name() (string, bool) {
	names := [...]string{
		"", "discovery_started", "generation_committed", "discovery_completed", "admission_started",
		"admission_completed", "directory_admitted", "directory_finalized", "file_enqueued",
		"file_started", "file_admitted", "file_first_write", "file_settled",
		"capacity_retry_scheduled", "capacity_retry_ready", "capacity_retry_succeeded",
		"capacity_budget_paused", "capacity_wait_canceled", "capacity_generation_ended", "job_settled",
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

type ItemBlockReason uint8

const (
	ItemBlockNone ItemBlockReason = iota
	ItemBlockStateCorrupt
	ItemBlockOwnershipUnknown
	ItemBlockPublicationAmbiguous
	ItemBlockRetirementUncertain
	ItemBlockRevisionConflict
	ItemBlockCheckpointInvalid
	ItemBlockOwnedObjectUnknown
)

func (value ItemBlockReason) Name() (string, bool) {
	names := [...]string{
		"none", "state_corrupt", "ownership_unknown", "publication_ambiguous",
		"retirement_uncertain", "revision_conflict", "checkpoint_invalid", "owned_object_unknown",
	}
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

type SenderTerminalSendTransport uint8

const (
	SenderTerminalSendAccepted SenderTerminalSendTransport = iota + 1
	SenderTerminalSendNotReached
	SenderTerminalSendUnsettled
	SenderTerminalSendRejected
	SenderTerminalSendRetired
)

func (value SenderTerminalSendTransport) Name() (string, bool) {
	names := [...]string{
		"", "accepted", "not_reached", "unsettled", "rejected_before_acceptance", "retired_before_acceptance",
	}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type SenderTerminalSendOutcome uint8

const (
	SenderTerminalSendDelivered SenderTerminalSendOutcome = iota + 1
	SenderTerminalSendDropped
	SenderTerminalSendUnknown
)

func (value SenderTerminalSendOutcome) Name() (string, bool) {
	names := [...]string{"", "delivered", "dropped", "unknown"}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type SenderTerminalSendDecision uint8

const (
	SenderTerminalSendDecisionDelivered SenderTerminalSendDecision = iota + 1
	SenderTerminalSendNaturalRetirement
	SenderTerminalSendFailed
)

func (value SenderTerminalSendDecision) Name() (string, bool) {
	names := [...]string{"", "delivered", "natural_retirement", "failed"}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type SenderSessionTerminalTrigger uint8

const (
	SenderSessionTerminalGracefulStop SenderSessionTerminalTrigger = iota + 1
	SenderSessionTerminalForcedClose
	SenderSessionTerminalPeerTerminal
	SenderSessionTerminalPathsExhausted
	SenderSessionTerminalRuntimeFailed
)

func (value SenderSessionTerminalTrigger) Name() (string, bool) {
	names := [...]string{
		"", "graceful_stop", "forced_close", "peer_terminal", "paths_exhausted", "runtime_failed",
	}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type SenderSessionTerminalProvenance uint8

const (
	SenderSessionTerminalNormalStop SenderSessionTerminalProvenance = iota + 1
	SenderSessionTerminalCallerStop
	SenderSessionTerminalRemoteClose
	SenderSessionTerminalLaneRetirement
	SenderSessionTerminalLocalFault
)

func (value SenderSessionTerminalProvenance) Name() (string, bool) {
	names := [...]string{
		"", "normal_stop", "caller_stop", "remote_close", "lane_retirement", "local_fault",
	}
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
