package runtrace

import (
	"strconv"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
)

type recordV2 struct {
	SchemaVersion int    `json:"schema_version"`
	Sequence      uint64 `json:"sequence"`
	Time          string `json:"time"`
	ElapsedMS     int64  `json:"elapsed_ms"`
	Level         string `json:"level"`
	Event         string `json:"event"`
	Command       string `json:"command"`
	RunID         string `json:"run_id"`

	ReceiveOperationID              *string `json:"receive_operation_id,omitempty"`
	ProtocolSessionID               *string `json:"protocol_session_id,omitempty"`
	ProtocolOperationID             *string `json:"protocol_operation_id,omitempty"`
	TransferJobID                   *string `json:"transfer_job_id,omitempty"`
	PeerPathID                      *string `json:"peer_path_id,omitempty"`
	PeerAttemptID                   *string `json:"peer_attempt_id,omitempty"`
	LaneID                          *uint32 `json:"lane_id,omitempty"`
	LaneEpoch                       *uint32 `json:"lane_epoch,omitempty"`
	ProtocolRole                    *string `json:"protocol_role,omitempty"`
	ProtocolOperationStage          *string `json:"protocol_operation_stage,omitempty"`
	ProtocolRequestKind             *string `json:"protocol_request_kind,omitempty"`
	ProtocolResponseKind            *string `json:"protocol_response_kind,omitempty"`
	ProtocolHasSend                 *bool   `json:"protocol_has_send,omitempty"`
	ProtocolSendSettled             *bool   `json:"protocol_send_settled,omitempty"`
	ProtocolSendAdmitted            *bool   `json:"protocol_send_admitted,omitempty"`
	ProtocolSendOutcome             *string `json:"protocol_send_outcome,omitempty"`
	ProtocolResponseCount           *string `json:"protocol_response_count,omitempty"`
	ProtocolDeadlineRemainingMS     *string `json:"protocol_deadline_remaining_ms,omitempty"`
	ProtocolOperationElapsedMS      *string `json:"protocol_operation_elapsed_ms,omitempty"`
	ProtocolUsableLanesAtSelection  *string `json:"protocol_usable_lanes_at_selection,omitempty"`
	ProtocolUsableLanesAtSettlement *string `json:"protocol_usable_lanes_at_settlement,omitempty"`
	ProtocolOperationCause          *string `json:"protocol_operation_cause,omitempty"`
	ProtocolErrorScope              *string `json:"protocol_error_scope,omitempty"`
	ProtocolErrorCode               *uint16 `json:"protocol_error_code,omitempty"`
	ProtocolErrorRetryable          *bool   `json:"protocol_error_retryable,omitempty"`

	Transport     *string `json:"transport,omitempty"`
	FromTransport *string `json:"from_transport,omitempty"`
	ToTransport   *string `json:"to_transport,omitempty"`
	ContentPath   *string `json:"content_path,omitempty"`
	RelayScheme   *string `json:"relay_scheme,omitempty"`
	RelayHost     *string `json:"relay_host,omitempty"`
	RelayPort     *uint16 `json:"relay_port,omitempty"`

	SubjectKind   *string `json:"subject_kind,omitempty"`
	FileBytes     *string `json:"file_bytes,omitempty"`
	SelectedItems *string `json:"selected_items,omitempty"`
	Attempt       *uint32 `json:"attempt,omitempty"`
	State         *string `json:"state,omitempty"`

	FailureCode         *string `json:"failure_code,omitempty"`
	MessageKey          *string `json:"message_key,omitempty"`
	FaultDomain         *string `json:"fault_domain,omitempty"`
	FaultScope          *string `json:"fault_scope,omitempty"`
	FaultCode           *uint16 `json:"fault_code,omitempty"`
	RetryAfterMS        *string `json:"retry_after_ms,omitempty"`
	ExitCode            *int    `json:"exit_code,omitempty"`
	ResultStatus        *string `json:"result_status,omitempty"`
	Drift               *string `json:"drift,omitempty"`
	ResultElapsedMS     *int64  `json:"result_elapsed_ms,omitempty"`
	DestinationAdjusted *bool   `json:"destination_adjusted,omitempty"`
	StoppedCleanly      *bool   `json:"stopped_cleanly,omitempty"`

	Discovery            *string `json:"discovery,omitempty"`
	CountersExact        *bool   `json:"counters_exact,omitempty"`
	DiscoveredFiles      *string `json:"discovered_files,omitempty"`
	DiscoveredBytes      *string `json:"discovered_bytes,omitempty"`
	PublishedFiles       *string `json:"published_files,omitempty"`
	PublishedBytes       *string `json:"published_bytes,omitempty"`
	VerifiedBytes        *string `json:"verified_bytes,omitempty"`
	NewlyVerifiedBytes   *string `json:"newly_verified_bytes,omitempty"`
	DownloadedFiles      *string `json:"downloaded_files,omitempty"`
	ResumedFiles         *string `json:"resumed_files,omitempty"`
	PausedFiles          *string `json:"paused_files,omitempty"`
	CollisionFiles       *string `json:"collision_files,omitempty"`
	ItemBlockedFiles     *string `json:"item_blocked_files,omitempty"`
	FailedFiles          *string `json:"failed_files,omitempty"`
	ModifiedTimeWarnings *string `json:"modified_time_warnings,omitempty"`
	DirectoryFailures    *string `json:"directory_failures,omitempty"`
	OmittedDiagnostics   *string `json:"omitted_diagnostics,omitempty"`

	TraceIncompleteCause *string `json:"trace_incomplete_cause,omitempty"`
	LifecycleDropped     *string `json:"lifecycle_dropped,omitempty"`
	ProgressDropped      *string `json:"progress_dropped,omitempty"`
	TraceIncomplete      *bool   `json:"trace_incomplete,omitempty"`
	EventsWritten        *string `json:"events_written,omitempty"`
	WriterFailed         *bool   `json:"writer_failed,omitempty"`
	FlushFailed          *bool   `json:"flush_failed,omitempty"`
	SchemaLimited        *bool   `json:"schema_limited,omitempty"`

	RelayLinkID          *string `json:"relay_link_id,omitempty"`
	RelaySessionID       *string `json:"relay_session_id,omitempty"`
	RelaySendOperationID *string `json:"relay_send_operation_id,omitempty"`
	Stage                *string `json:"stage,omitempty"`
	Terminal             *bool   `json:"terminal,omitempty"`
	Disposition          *string `json:"disposition,omitempty"`
	RetirementSource     *string `json:"retirement_source,omitempty"`
	Cause                *string `json:"cause,omitempty"`
	DrainCause           *string `json:"drain_cause,omitempty"`
	RelayDropped         *string `json:"relay_dropped,omitempty"`

	WebRTCChannelID           *string `json:"webrtc_channel_id,omitempty"`
	WebRTCSendOperationID     *string `json:"webrtc_send_operation_id,omitempty"`
	Operation                 *string `json:"operation,omitempty"`
	Transition                *string `json:"transition,omitempty"`
	TerminalState             *string `json:"terminal_state,omitempty"`
	Dropped                   *string `json:"dropped,omitempty"`
	AttemptSequence           *string `json:"attempt_sequence,omitempty"`
	AttemptElapsedMS          *string `json:"attempt_elapsed_ms,omitempty"`
	PeerOfferOperationID      *string `json:"peer_offer_operation_id,omitempty"`
	PeerGrantOperationID      *string `json:"peer_grant_operation_id,omitempty"`
	PeerPhase                 *string `json:"peer_phase,omitempty"`
	PeerDeadlineMS            *string `json:"peer_deadline_ms,omitempty"`
	PeerAdmissionDisposition  *string `json:"peer_admission_disposition,omitempty"`
	PeerResponseDelivery      *string `json:"peer_response_delivery,omitempty"`
	PeerLaneRejectionCode     *string `json:"peer_lane_rejection_code,omitempty"`
	PeerRejectionRetryAfterMS *string `json:"peer_rejection_retry_after_ms,omitempty"`
	CandidatesLocalEmitted    *uint32 `json:"candidates_local_emitted,omitempty"`
	CandidatesRemoteAccepted  *uint32 `json:"candidates_remote_accepted,omitempty"`
	PeerFailedAtStage         *string `json:"peer_failed_at_stage,omitempty"`
	FailureScope              *string `json:"failure_scope,omitempty"`

	FileSelection  *string `json:"file_selection,omitempty"`
	FileSettlement *string `json:"file_settlement,omitempty"`
	TreeSettlement *string `json:"tree_settlement,omitempty"`

	NodeClaims                    *string `json:"node_claims,omitempty"`
	DirectoryClaims               *string `json:"directory_claims,omitempty"`
	FileClaims                    *string `json:"file_claims,omitempty"`
	ActiveFileClaims              *string `json:"active_file_claims,omitempty"`
	ReservedFileSlots             *string `json:"reserved_file_slots,omitempty"`
	DirectoryMetadataBytes        *string `json:"directory_metadata_bytes,omitempty"`
	CheckpointRecords             *string `json:"checkpoint_records,omitempty"`
	ReceiveIntentDigest           *string `json:"receive_intent_digest,omitempty"`
	OutputSessionID               *string `json:"output_session_id,omitempty"`
	FilesystemCertification       *string `json:"filesystem_certification,omitempty"`
	FilesystemRootDisposition     *string `json:"filesystem_root_disposition,omitempty"`
	FilesystemNativeLockScope     *string `json:"filesystem_native_lock_scope,omitempty"`
	FilesystemNativeLockMilestone *string `json:"filesystem_native_lock_milestone,omitempty"`
	FilesystemRuntimeComponent    *string `json:"filesystem_runtime_component,omitempty"`
	FilesystemRuntimeOperation    *string `json:"filesystem_runtime_operation,omitempty"`
	FilesystemRuntimeDecision     *string `json:"filesystem_runtime_decision,omitempty"`
	FilesystemOperationID         *string `json:"filesystem_operation_id,omitempty"`
	FilesystemClaimID             *string `json:"filesystem_claim_id,omitempty"`
	FilesystemFailureStage        *string `json:"filesystem_failure_stage,omitempty"`
	FilesystemReconciliationStep  *string `json:"filesystem_reconciliation_step,omitempty"`
	FilesystemNativeErrorClass    *string `json:"filesystem_native_error_class,omitempty"`

	Settled              *bool   `json:"settled,omitempty"`
	TransportDisposition *string `json:"transport_disposition,omitempty"`
	Outcome              *string `json:"outcome,omitempty"`
	Decision             *string `json:"decision,omitempty"`

	ActiveScans              *string `json:"active_scans,omitempty"`
	ScanWork                 *string `json:"scan_work,omitempty"`
	Entries                  *string `json:"entries,omitempty"`
	MemoryBytes              *string `json:"memory_bytes,omitempty"`
	SpillBytes               *string `json:"spill_bytes,omitempty"`
	LegacyRootsRemoved       *string `json:"legacy_roots_removed,omitempty"`
	RootPrefetchAttempt      *string `json:"root_prefetch_attempt,omitempty"`
	RootPrefetchEntryCount   *string `json:"root_prefetch_entry_count,omitempty"`
	RootPrefetchOmittedCount *string `json:"root_prefetch_omitted_count,omitempty"`

	LaneRoute           *string `json:"lane_route,omitempty"`
	DeliveredBlocks     *string `json:"delivered_blocks,omitempty"`
	DeliveredBytes      *string `json:"delivered_bytes,omitempty"`
	FailedBlockAttempts *string `json:"failed_block_attempts,omitempty"`
	ReassignedBlocks    *string `json:"reassigned_blocks,omitempty"`
	Incomplete          *bool   `json:"incomplete,omitempty"`

	ObserverLossCategory *string `json:"observer_loss_category,omitempty"`
	ObserverLossReason   *string `json:"observer_loss_reason,omitempty"`
	ObserverLossCount    *string `json:"observer_loss_count,omitempty"`

	ReceiverLocalGeneration       *string  `json:"receiver_local_generation,omitempty"`
	ReceiverTransitionAuthority   *string  `json:"receiver_transition_authority,omitempty"`
	ReceiverDisposition           *string  `json:"receiver_disposition,omitempty"`
	ReceiverTransitionProvenance  *string  `json:"receiver_transition_provenance,omitempty"`
	ReceiverConsequenceProvenance *string  `json:"receiver_consequence_provenance,omitempty"`
	ReceiverLocalStopReason       *string  `json:"receiver_local_stop_reason,omitempty"`
	ReceiverDiagnosticsTruncated  *bool    `json:"receiver_diagnostics_truncated,omitempty"`
	ReceiverBenignComponents      []string `json:"receiver_benign_components,omitempty"`
	ReceiverRetainedCauseClasses  []string `json:"receiver_retained_cause_classes,omitempty"`
	ReceiverTeardownTransitions   []string `json:"receiver_teardown_transitions,omitempty"`
	ReceiverPeerShutdownFailed    *bool    `json:"receiver_peer_shutdown_failed,omitempty"`
	ReceiverChannelDrainFailed    *bool    `json:"receiver_channel_drain_failed,omitempty"`
}

func baseRecordV2(
	runID string,
	metadata entryMetadata,
	command clievent.Command,
	level clievent.Level,
	event string,
) (recordV2, error) {
	commandName, commandOK := command.Name()
	levelName, levelOK := level.Name()
	if !commandOK || !levelOK || event == "" || metadata.sequence == 0 ||
		metadata.sequence > maxJSONSafeInteger || metadata.elapsedMS < 0 {
		return recordV2{}, ErrInvalidConfig
	}
	return recordV2{
		SchemaVersion: SchemaVersion,
		Sequence:      metadata.sequence,
		Time:          metadata.time.UTC().Format(time.RFC3339Nano),
		ElapsedMS:     metadata.elapsedMS,
		Level:         levelName,
		Event:         event,
		Command:       commandName,
		RunID:         runID,
	}, nil
}

func summaryV2(
	runID string,
	command clievent.Command,
	metadata entryMetadata,
	status Status,
) recordV2 {
	level := clievent.LevelInfo
	if !status.Complete {
		level = clievent.LevelWarning
	}
	record, _ := baseRecordV2(runID, metadata, command, level, "trace_summary")
	incomplete := !status.Complete
	record.TraceIncomplete = &incomplete
	record.LifecycleDropped = decimalPointer(status.LifecycleDropped)
	record.ProgressDropped = decimalPointer(status.ProgressDropped)
	record.EventsWritten = decimalPointer(status.EventsWritten)
	record.WriterFailed = new(status.WriterFailed)
	record.FlushFailed = new(status.FlushFailed)
	record.SchemaLimited = new(status.SchemaLimited)
	return record
}

func decimalPointer(value uint64) *string {
	encoded := strconv.FormatUint(value, 10)
	return &encoded
}
