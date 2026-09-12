package runtrace

type relayAuthorityV4 struct {
	Scheme string `json:"scheme"`
	Host   string `json:"host"`
	Port   uint16 `json:"port"`
}

type faultV4 struct {
	Domain string `json:"domain"`
	Scope  string `json:"scope"`
	Code   uint16 `json:"code"`
}

type failureV4 struct {
	Code         string   `json:"code"`
	MessageKey   string   `json:"message_key"`
	Fault        *faultV4 `json:"fault,omitempty"`
	RetryAfterMS *string  `json:"retry_after_ms,omitempty"`
}

type fileOutcomesV4 struct {
	DownloadedFiles          string `json:"downloaded_files"`
	PreviouslyPublishedFiles string `json:"previously_published_files"`
	ResumedFiles             string `json:"resumed_files"`
	PausedFiles              string `json:"paused_files"`
	CollisionFiles           string `json:"collision_files"`
	ItemBlockedFiles         string `json:"item_blocked_files"`
	FailedFiles              string `json:"failed_files"`
	ModifiedTimeWarnings     string `json:"modified_time_warnings"`
}

type capacityWaitV4 struct {
	ActiveWaiters     string `json:"active_waiters"`
	AccumulatedWaitMS string `json:"accumulated_wait_ms"`
	Attempts          string `json:"attempts"`
}

type progressPayloadV4 struct {
	Discovery                string         `json:"discovery"`
	CountersExact            bool           `json:"counters_exact"`
	DiscoveredFiles          string         `json:"discovered_files"`
	DiscoveredBytes          string         `json:"discovered_bytes"`
	PublishedFiles           string         `json:"published_files"`
	PublishedBytes           string         `json:"published_bytes"`
	PreviouslyPublishedBytes string         `json:"previously_published_bytes"`
	VerifiedBytes            string         `json:"verified_bytes"`
	NewlyVerifiedBytes       string         `json:"newly_verified_bytes"`
	FileOutcomes             fileOutcomesV4 `json:"file_outcomes"`
	CapacityWait             capacityWaitV4 `json:"capacity_wait"`
}

type sharingSubjectPayloadV4 struct {
	SubjectKind   string  `json:"subject_kind"`
	SelectedItems string  `json:"selected_items"`
	FileBytes     *string `json:"file_bytes,omitempty"`
}

func (sharingSubjectPayloadV4) runTracePayloadV4() {}

type relayConnectedPayloadV4 struct {
	RelayAuthority relayAuthorityV4 `json:"relay_authority"`
}

func (relayConnectedPayloadV4) runTracePayloadV4() {}

type relayRecoveringPayloadV4 struct {
	RelayAuthority relayAuthorityV4 `json:"relay_authority"`
	Attempt        uint32           `json:"attempt"`
	State          string           `json:"state"`
	Failure        *failureV4       `json:"failure,omitempty"`
}

func (relayRecoveringPayloadV4) runTracePayloadV4() {}

type contentPathSelectedPayloadV4 struct {
	ContentPath string `json:"content_path"`
}

func (contentPathSelectedPayloadV4) runTracePayloadV4() {}

type fallbackPayloadV4 struct {
	FromTransport string    `json:"from_transport"`
	ToTransport   string    `json:"to_transport"`
	Failure       failureV4 `json:"failure"`
}

func (fallbackPayloadV4) runTracePayloadV4() {}

type transferProgressPayloadV4 struct {
	ReceiveOperationID string            `json:"receive_operation_id"`
	TransferJobID      string            `json:"transfer_job_id"`
	Progress           progressPayloadV4 `json:"progress"`
}

func (transferProgressPayloadV4) runTracePayloadV4() {}

type warningPayloadV4 struct {
	Failure failureV4 `json:"failure"`
}

func (warningPayloadV4) runTracePayloadV4() {}

type commandFailedPayloadV4 struct {
	ExitCode int       `json:"exit_code"`
	Failure  failureV4 `json:"failure"`
}

func (commandFailedPayloadV4) runTracePayloadV4() {}

type downloadConnectivityV4 struct {
	DownloadID            string   `json:"download_id"`
	FirstDirectElapsedMS  *string  `json:"first_direct_elapsed_ms"`
	DirectBytes           string   `json:"direct_bytes"`
	TURNBytes             string   `json:"turn_bytes"`
	ApplicationRelayBytes string   `json:"application_relay_bytes"`
	UnknownBytes          string   `json:"unknown_bytes"`
	DirectFraction        *float64 `json:"direct_fraction"`
	FallbackStallMS       string   `json:"fallback_stall_ms"`
	Incomplete            bool     `json:"incomplete"`
	Final                 bool     `json:"final"`
}

type transferSettledPayloadV4 struct {
	DownloadConnectivity *downloadConnectivityV4 `json:"download_connectivity,omitempty"`
	ResultStatus         string                  `json:"result_status"`
	ExitCode             int                     `json:"exit_code"`
	Drift                string                  `json:"drift"`
	ResultElapsedMS      string                  `json:"result_elapsed_ms"`
	DestinationAdjusted  bool                    `json:"destination_adjusted"`
	FileOutcomes         fileOutcomesV4          `json:"file_outcomes"`
	DirectoryFailures    string                  `json:"directory_failures"`
	OmittedDiagnostics   string                  `json:"omitted_diagnostics"`
	PublishedBytes       string                  `json:"published_bytes"`
	CountersExact        bool                    `json:"counters_exact"`
	Failure              *failureV4              `json:"failure,omitempty"`
}

func (transferSettledPayloadV4) runTracePayloadV4() {}

type sharingStoppedPayloadV4 struct {
	ExitCode        int        `json:"exit_code"`
	ResultElapsedMS string     `json:"result_elapsed_ms"`
	StoppedCleanly  bool       `json:"stopped_cleanly"`
	Failure         *failureV4 `json:"failure,omitempty"`
}

func (sharingStoppedPayloadV4) runTracePayloadV4() {}

type traceIncompletePayloadV4 struct {
	Cause            string `json:"cause"`
	LifecycleDropped string `json:"lifecycle_dropped"`
	ProgressDropped  string `json:"progress_dropped"`
}

func (traceIncompletePayloadV4) runTracePayloadV4() {}

type laneAdoptedPayloadV4 struct {
	Transport string `json:"transport"`
}

func (laneAdoptedPayloadV4) runTracePayloadV4() {}

type relayLifecyclePayloadV4 struct {
	LinkID           string  `json:"link_id"`
	RelaySessionID   *string `json:"relay_session_id,omitempty"`
	SendOperationID  *string `json:"send_operation_id,omitempty"`
	Stage            string  `json:"stage"`
	Terminal         bool    `json:"terminal"`
	Disposition      *string `json:"disposition,omitempty"`
	RetirementSource string  `json:"retirement_source"`
	Cause            string  `json:"cause"`
	DrainCause       string  `json:"drain_cause"`
	Dropped          *string `json:"dropped,omitempty"`
}

func (relayLifecyclePayloadV4) runTracePayloadV4() {}

type webRTCLifecyclePayloadV4 struct {
	ChannelID       string  `json:"channel_id"`
	SendOperationID *string `json:"send_operation_id,omitempty"`
	Operation       string  `json:"operation"`
	Transition      string  `json:"transition"`
	Disposition     *string `json:"disposition,omitempty"`
	State           string  `json:"state"`
	TerminalState   string  `json:"terminal_state"`
	Cause           string  `json:"cause"`
	Dropped         *string `json:"dropped,omitempty"`
}

func (webRTCLifecyclePayloadV4) runTracePayloadV4() {}

type peerPhaseDeadlineV4 struct {
	Phase      string `json:"phase"`
	DeadlineMS string `json:"deadline_ms"`
}

type peerCandidateCountsV4 struct {
	LocalEmitted   uint32 `json:"local_emitted"`
	RemoteAccepted uint32 `json:"remote_accepted"`
}

type peerAdmissionV4 struct {
	Disposition      string `json:"disposition"`
	ResponseDelivery string `json:"response_delivery"`
}

type peerRejectionV4 struct {
	Code         string  `json:"code"`
	RetryAfterMS *string `json:"retry_after_ms,omitempty"`
}

type peerFailureSummaryV4 struct {
	LastCompletedStage string `json:"last_completed_stage"`
	StageElapsedMillis string `json:"stage_elapsed_ms"`
	DeadlineExpired    bool   `json:"deadline_expired"`
	Initiator          string `json:"close_initiator"`
	Cause              string `json:"cause"`
}

type peerFailureV4 struct {
	Summary       *peerFailureSummaryV4 `json:"summary,omitempty"`
	FailedAtStage string                `json:"failed_at_stage"`
	Scope         string                `json:"scope"`
	Failure       failureV4             `json:"failure"`
}

type peerAttemptPayloadV4 struct {
	AttemptSequence  string                 `json:"attempt_sequence"`
	AttemptElapsedMS string                 `json:"attempt_elapsed_ms"`
	Stage            string                 `json:"stage"`
	OfferOperationID *string                `json:"offer_operation_id,omitempty"`
	PhaseDeadline    *peerPhaseDeadlineV4   `json:"phase_deadline,omitempty"`
	Candidates       *peerCandidateCountsV4 `json:"candidates,omitempty"`
	GrantOperationID *string                `json:"grant_operation_id,omitempty"`
	Admission        *peerAdmissionV4       `json:"admission,omitempty"`
	Rejection        *peerRejectionV4       `json:"rejection,omitempty"`
	Failure          *peerFailureV4         `json:"failure,omitempty"`
}

func (peerAttemptPayloadV4) runTracePayloadV4() {}

type capacityScopeV4 struct {
	StableHandles            string `json:"stable_handles"`
	ActiveLeases             string `json:"active_leases"`
	StableHandleLimit        string `json:"stable_handle_limit"`
	ActiveLeaseLimit         string `json:"active_lease_limit"`
	ReclaimableStableHandles string `json:"reclaimable_stable_handles"`
	QuarantinedStableHandles string `json:"quarantined_stable_handles"`
	PendingAdmissions        string `json:"pending_admissions"`
	ActiveReclaims           string `json:"active_reclaims"`
}

type senderCapacityPayloadV4 struct {
	Stage      string           `json:"stage"`
	DecisionID *string          `json:"decision_id,omitempty"`
	RevisionID *string          `json:"revision_id,omitempty"`
	Process    capacityScopeV4  `json:"process"`
	Share      *capacityScopeV4 `json:"share,omitempty"`
	Session    *capacityScopeV4 `json:"session,omitempty"`
}

func (senderCapacityPayloadV4) runTracePayloadV4() {}

type senderRevisionPayloadV4 struct {
	Stage      string  `json:"stage"`
	Cause      string  `json:"cause"`
	RevisionID string  `json:"revision_id"`
	LeaseID    *string `json:"lease_id,omitempty"`
}

func (senderRevisionPayloadV4) runTracePayloadV4() {}

type transferCapacityLifecycleV4 struct {
	WaitID              string `json:"wait_id"`
	GenerationID        string `json:"generation_id"`
	ProtocolOperationID string `json:"protocol_operation_id"`
	Attempt             string `json:"attempt"`
	HintMS              string `json:"hint_ms"`
	JitterMS            string `json:"jitter_ms"`
	DelayMS             string `json:"delay_ms"`
	AccumulatedWaitMS   string `json:"accumulated_wait_ms"`
	ActiveWaiters       uint32 `json:"active_waiters"`
}

type transferLifecyclePayloadV4 struct {
	ReceiveOperationID string                       `json:"receive_operation_id"`
	TransferJobID      string                       `json:"transfer_job_id"`
	Stage              string                       `json:"stage"`
	FileSelection      string                       `json:"file_selection"`
	FileSettlement     string                       `json:"file_settlement"`
	ItemBlockReason    *string                      `json:"item_block_reason,omitempty"`
	TreeSettlement     string                       `json:"tree_settlement"`
	Progress           progressPayloadV4            `json:"progress"`
	Capacity           *transferCapacityLifecycleV4 `json:"capacity,omitempty"`
	Failure            *failureV4                   `json:"failure,omitempty"`
}

func (transferLifecyclePayloadV4) runTracePayloadV4() {}

type filesystemNativeLockV4 struct {
	Scope     string `json:"scope"`
	Milestone string `json:"milestone"`
}

type filesystemRuntimeDecisionV4 struct {
	Component string `json:"component"`
	Operation string `json:"operation"`
	Decision  string `json:"decision"`
}

type filesystemCorrelationV4 struct {
	OperationID *string `json:"operation_id,omitempty"`
	ClaimID     *string `json:"claim_id,omitempty"`
}

type filesystemCountersV4 struct {
	NodeClaims             string `json:"node_claims"`
	DirectoryClaims        string `json:"directory_claims"`
	FileClaims             string `json:"file_claims"`
	ActiveFileClaims       string `json:"active_file_claims"`
	ReservedFileSlots      string `json:"reserved_file_slots"`
	DirectoryMetadataBytes string `json:"directory_metadata_bytes"`
	CheckpointRecords      string `json:"checkpoint_records"`
}

type filesystemFailureV4 struct {
	Stage              string    `json:"stage"`
	ReconciliationStep *string   `json:"reconciliation_step,omitempty"`
	NativeErrorClass   *string   `json:"native_error_class,omitempty"`
	Failure            failureV4 `json:"failure"`
}

type filesystemCapabilityV4 struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason"`
}

type filesystemCapabilitiesV4 struct {
	Mode              string                 `json:"mode"`
	SafePublish       filesystemCapabilityV4 `json:"safe_publish"`
	OperationRecovery filesystemCapabilityV4 `json:"operation_recovery"`
	RangeRecovery     filesystemCapabilityV4 `json:"range_recovery"`
	CrashCleanup      filesystemCapabilityV4 `json:"crash_cleanup"`
}

type filesystemOutputPayloadV4 struct {
	Capabilities        *filesystemCapabilitiesV4    `json:"capabilities,omitempty"`
	Operation           string                       `json:"operation"`
	ReceiveOperationID  *string                      `json:"receive_operation_id,omitempty"`
	ReceiveIntentDigest *string                      `json:"receive_intent_digest,omitempty"`
	OutputSessionID     *string                      `json:"output_session_id,omitempty"`
	Certification       *string                      `json:"certification,omitempty"`
	NativeLock          *filesystemNativeLockV4      `json:"native_lock,omitempty"`
	RootDisposition     *string                      `json:"root_disposition,omitempty"`
	RuntimeDecision     *filesystemRuntimeDecisionV4 `json:"runtime_decision,omitempty"`
	CheckpointDecision  *string                      `json:"checkpoint_decision,omitempty"`
	Correlation         *filesystemCorrelationV4     `json:"output_correlation,omitempty"`
	Counters            filesystemCountersV4         `json:"counters"`
	Failure             *filesystemFailureV4         `json:"failure,omitempty"`
}

func (filesystemOutputPayloadV4) runTracePayloadV4() {}

type senderTerminalSendPayloadV4 struct {
	Settled              bool   `json:"settled"`
	TransportDisposition string `json:"transport_disposition"`
	Outcome              string `json:"outcome"`
	Decision             string `json:"decision"`
}

func (senderTerminalSendPayloadV4) runTracePayloadV4() {}

type senderSessionTerminatedPayloadV4 struct {
	Trigger    string               `json:"trigger"`
	Provenance string               `json:"provenance"`
	Failure    *diagnosticFailureV4 `json:"failure,omitempty"`
}

func (senderSessionTerminatedPayloadV4) runTracePayloadV4() {}

type catalogUsageV4 struct {
	ActiveScans string `json:"active_scans"`
	ScanWork    string `json:"scan_work"`
	Entries     string `json:"entries"`
	MemoryBytes string `json:"memory_bytes"`
	SpillBytes  string `json:"spill_bytes"`
}

type catalogStoragePayloadV4 struct {
	Operation          string         `json:"operation"`
	Cause              string         `json:"cause"`
	Usage              catalogUsageV4 `json:"usage"`
	LegacyRootsRemoved string         `json:"legacy_roots_removed"`
}

func (catalogStoragePayloadV4) runTracePayloadV4() {}

type rootPrefetchPayloadV4 struct {
	Decision     string `json:"decision"`
	Attempt      string `json:"attempt"`
	EntryCount   string `json:"entry_count"`
	OmittedCount string `json:"omitted_count"`
}

func (rootPrefetchPayloadV4) runTracePayloadV4() {}

type protocolSendV4 struct {
	Settled  bool   `json:"settled"`
	Admitted bool   `json:"admitted"`
	Outcome  string `json:"outcome"`
}

type ProtocolErrorContentV4 struct {
	Scope        string  `json:"scope"`
	Code         uint16  `json:"code"`
	Retryable    bool    `json:"retryable"`
	RetryAfterMS *uint32 `json:"retry_after_ms,omitempty"`
}
type protocolContextV4 struct {
	ObservedAt  string `json:"observed_at"`
	Role        string `json:"role"`
	RequestKind string `json:"request_kind,omitempty"`
}
type sendAttemptV4 struct {
	Cause                sendAttemptCauseV4 `json:"cause"`
	AttemptSequence      string             `json:"attempt_sequence"`
	LaneID               uint32             `json:"lane_id"`
	LaneEpoch            uint32             `json:"lane_epoch"`
	PolicyAdmitted       bool               `json:"policy_admitted"`
	Settled              bool               `json:"settled"`
	Outcome              string             `json:"outcome"`
	TransportDisposition *string            `json:"transport_disposition,omitempty"`
	End                  string             `json:"end"`
}
type responseSendResultV4 struct {
	Started                bool            `json:"started"`
	Evidence               string          `json:"evidence"`
	End                    string          `json:"end"`
	Cleanup                string          `json:"cleanup"`
	Attempts               []sendAttemptV4 `json:"attempts"`
	PendingAttemptSequence *string         `json:"pending_attempt_sequence,omitempty"`
}
type protocolResponseSendPayloadV4 struct {
	protocolContextV4
	ResponseSequence string                  `json:"response_sequence"`
	ResponseKind     string                  `json:"response_kind"`
	ProtocolError    *ProtocolErrorContentV4 `json:"protocol_error,omitempty"`
	ResponseResult   responseSendResultV4    `json:"response_result"`
}

func (protocolResponseSendPayloadV4) runTracePayloadV4() {}

type protocolSendAttemptSettledPayloadV4 struct {
	protocolContextV4
	ResponseSequence string        `json:"response_sequence"`
	ResponseKind     string        `json:"response_kind"`
	Attempt          sendAttemptV4 `json:"attempt"`
}

func (protocolSendAttemptSettledPayloadV4) runTracePayloadV4() {}

type protocolErrorReceivedPayloadV4 struct {
	protocolContextV4
	ProtocolError *ProtocolErrorContentV4 `json:"protocol_error"`
}

func (protocolErrorReceivedPayloadV4) runTracePayloadV4() {}

type senderContentDecisionPayloadV4 struct {
	protocolContextV4
	ContentDecision *senderContentDecisionV4 `json:"content_decision"`
}

func (senderContentDecisionPayloadV4) runTracePayloadV4() {}

type senderContentDecisionV4 struct {
	Kind               string  `json:"kind"`
	CapacityDecisionID *string `json:"capacity_decision_id,omitempty"`
	LeaseID            *string `json:"lease_id,omitempty"`
}

type protocolOperationPayloadV4 struct {
	ObservedAt              string          `json:"observed_at"`
	Role                    string          `json:"role"`
	Stage                   string          `json:"stage"`
	RequestKind             string          `json:"request_kind"`
	ResponseKind            *string         `json:"response_kind,omitempty"`
	Send                    *protocolSendV4 `json:"send,omitempty"`
	ResponseCount           string          `json:"response_count"`
	DeadlineRemainingMS     *string         `json:"deadline_remaining_ms,omitempty"`
	OperationElapsedMS      string          `json:"operation_elapsed_ms"`
	UsableLanesAtSelection  uint32          `json:"usable_lanes_at_selection"`
	UsableLanesAtSettlement uint32          `json:"usable_lanes_at_settlement"`
	Cause                   string          `json:"cause"`
}

func (protocolOperationPayloadV4) runTracePayloadV4() {}

type laneSettlementPayloadV4 struct {
	Route               string `json:"route"`
	DeliveredBlocks     string `json:"delivered_blocks"`
	DeliveredBytes      string `json:"delivered_bytes"`
	FailedBlockAttempts string `json:"failed_block_attempts"`
	ReassignedBlocks    string `json:"reassigned_blocks"`
	Incomplete          bool   `json:"incomplete"`
}

func (laneSettlementPayloadV4) runTracePayloadV4() {}

type observerLossPayloadV4 struct {
	OmittedSamples string                         `json:"omitted_samples"`
	Category       string                         `json:"category"`
	Reason         string                         `json:"reason"`
	Count          string                         `json:"count"`
	Rejection      *observationRejectionPayloadV4 `json:"rejection,omitempty"`
}

type observationRejectionPayloadV4 struct {
	Event            string             `json:"source_event"`
	Source           string             `json:"source_location"`
	Stage            string             `json:"source_stage"`
	Field            string             `json:"field"`
	Rule             string             `json:"rule"`
	Session          string             `json:"sample_protocol_session_id,omitempty"`
	Operation        string             `json:"sample_protocol_operation_id,omitempty"`
	Revision         string             `json:"sample_revision_id,omitempty"`
	ResponseSequence string             `json:"sample_response_sequence,omitempty"`
	AttemptSequence  string             `json:"sample_attempt_sequence,omitempty"`
	Evidence         []rejectionFieldV4 `json:"evidence"`
	Truncated        bool               `json:"truncated"`
	OmittedFields    string             `json:"omitted_fields"`
	OmittedBytes     string             `json:"omitted_bytes"`
}
type rejectionFieldV4 struct {
	Field          string `json:"field"`
	Representation string `json:"representation"`
	Value          string `json:"value"`
}

type platformSetupPayloadV4 struct {
	State  string `json:"state"`
	Reason string `json:"reason"`
}

func (platformSetupPayloadV4) runTracePayloadV4() {}

func (observerLossPayloadV4) runTracePayloadV4() {}

type receiverTerminationPayloadV4 struct {
	ProtocolOperationID   *string  `json:"protocol_operation_id,omitempty"`
	LocalGeneration       string   `json:"local_generation"`
	TransitionAuthority   string   `json:"transition_authority"`
	Disposition           string   `json:"disposition"`
	TransitionProvenance  string   `json:"transition_provenance"`
	ConsequenceProvenance string   `json:"consequence_provenance"`
	LocalStopReason       string   `json:"local_stop_reason"`
	DiagnosticsTruncated  bool     `json:"diagnostics_truncated"`
	BenignComponents      []string `json:"benign_components"`
	RetainedCauseClasses  []string `json:"retained_cause_classes"`
	TeardownTransitions   []string `json:"teardown_transitions"`
	PeerShutdownFailed    bool     `json:"peer_shutdown_failed"`
	ChannelDrainFailed    bool     `json:"channel_drain_failed"`
}

func (receiverTerminationPayloadV4) runTracePayloadV4() {}

type traceSummaryPayloadV4 struct {
	RejectionEvidenceDropped string `json:"rejection_evidence_dropped"`
	Incomplete               bool   `json:"incomplete"`
	LifecycleDropped         string `json:"lifecycle_dropped"`
	ProgressDropped          string `json:"progress_dropped"`
	EventsWritten            string `json:"events_written"`
	WriterFailed             bool   `json:"writer_failed"`
	FlushFailed              bool   `json:"flush_failed"`
	SchemaLimited            bool   `json:"schema_limited"`
}

func (traceSummaryPayloadV4) runTracePayloadV4() {}

type sendAttemptCauseV4 struct {
	Kind      string `json:"kind"`
	Detail    string `json:"detail,omitempty"`
	Truncated bool   `json:"truncated"`
}
