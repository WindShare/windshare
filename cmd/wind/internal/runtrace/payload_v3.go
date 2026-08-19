package runtrace

type relayAuthorityV3 struct {
	Scheme string `json:"scheme"`
	Host   string `json:"host"`
	Port   uint16 `json:"port"`
}

type faultV3 struct {
	Domain string `json:"domain"`
	Scope  string `json:"scope"`
	Code   uint16 `json:"code"`
}

type failureV3 struct {
	Code         string   `json:"code"`
	MessageKey   string   `json:"message_key"`
	Fault        *faultV3 `json:"fault,omitempty"`
	RetryAfterMS *string  `json:"retry_after_ms,omitempty"`
}

type fileOutcomesV3 struct {
	DownloadedFiles      string `json:"downloaded_files"`
	ResumedFiles         string `json:"resumed_files"`
	PausedFiles          string `json:"paused_files"`
	CollisionFiles       string `json:"collision_files"`
	ItemBlockedFiles     string `json:"item_blocked_files"`
	FailedFiles          string `json:"failed_files"`
	ModifiedTimeWarnings string `json:"modified_time_warnings"`
}

type progressPayloadV3 struct {
	Discovery          string         `json:"discovery"`
	CountersExact      bool           `json:"counters_exact"`
	DiscoveredFiles    string         `json:"discovered_files"`
	DiscoveredBytes    string         `json:"discovered_bytes"`
	PublishedFiles     string         `json:"published_files"`
	PublishedBytes     string         `json:"published_bytes"`
	VerifiedBytes      string         `json:"verified_bytes"`
	NewlyVerifiedBytes string         `json:"newly_verified_bytes"`
	FileOutcomes       fileOutcomesV3 `json:"file_outcomes"`
}

type sharingSubjectPayloadV3 struct {
	SubjectKind   string  `json:"subject_kind"`
	SelectedItems string  `json:"selected_items"`
	FileBytes     *string `json:"file_bytes,omitempty"`
}

func (sharingSubjectPayloadV3) runTracePayloadV3() {}

type relayConnectedPayloadV3 struct {
	RelayAuthority relayAuthorityV3 `json:"relay_authority"`
}

func (relayConnectedPayloadV3) runTracePayloadV3() {}

type relayRecoveringPayloadV3 struct {
	RelayAuthority relayAuthorityV3 `json:"relay_authority"`
	Attempt        uint32           `json:"attempt"`
	State          string           `json:"state"`
	Failure        *failureV3       `json:"failure,omitempty"`
}

func (relayRecoveringPayloadV3) runTracePayloadV3() {}

type contentPathSelectedPayloadV3 struct {
	ContentPath string `json:"content_path"`
}

func (contentPathSelectedPayloadV3) runTracePayloadV3() {}

type fallbackPayloadV3 struct {
	FromTransport string    `json:"from_transport"`
	ToTransport   string    `json:"to_transport"`
	Failure       failureV3 `json:"failure"`
}

func (fallbackPayloadV3) runTracePayloadV3() {}

type transferProgressPayloadV3 struct {
	ReceiveOperationID string            `json:"receive_operation_id"`
	TransferJobID      string            `json:"transfer_job_id"`
	Progress           progressPayloadV3 `json:"progress"`
}

func (transferProgressPayloadV3) runTracePayloadV3() {}

type warningPayloadV3 struct {
	Failure failureV3 `json:"failure"`
}

func (warningPayloadV3) runTracePayloadV3() {}

type commandFailedPayloadV3 struct {
	ExitCode int       `json:"exit_code"`
	Failure  failureV3 `json:"failure"`
}

func (commandFailedPayloadV3) runTracePayloadV3() {}

type transferSettledPayloadV3 struct {
	ResultStatus        string         `json:"result_status"`
	ExitCode            int            `json:"exit_code"`
	Drift               string         `json:"drift"`
	ResultElapsedMS     string         `json:"result_elapsed_ms"`
	DestinationAdjusted bool           `json:"destination_adjusted"`
	FileOutcomes        fileOutcomesV3 `json:"file_outcomes"`
	DirectoryFailures   string         `json:"directory_failures"`
	OmittedDiagnostics  string         `json:"omitted_diagnostics"`
	PublishedBytes      string         `json:"published_bytes"`
	CountersExact       bool           `json:"counters_exact"`
	Failure             *failureV3     `json:"failure,omitempty"`
}

func (transferSettledPayloadV3) runTracePayloadV3() {}

type sharingStoppedPayloadV3 struct {
	ExitCode        int        `json:"exit_code"`
	ResultElapsedMS string     `json:"result_elapsed_ms"`
	StoppedCleanly  bool       `json:"stopped_cleanly"`
	Failure         *failureV3 `json:"failure,omitempty"`
}

func (sharingStoppedPayloadV3) runTracePayloadV3() {}

type traceIncompletePayloadV3 struct {
	Cause            string `json:"cause"`
	LifecycleDropped string `json:"lifecycle_dropped"`
	ProgressDropped  string `json:"progress_dropped"`
}

func (traceIncompletePayloadV3) runTracePayloadV3() {}

type laneAdoptedPayloadV3 struct {
	Transport string `json:"transport"`
}

func (laneAdoptedPayloadV3) runTracePayloadV3() {}

type relayLifecyclePayloadV3 struct {
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

func (relayLifecyclePayloadV3) runTracePayloadV3() {}

type webRTCLifecyclePayloadV3 struct {
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

func (webRTCLifecyclePayloadV3) runTracePayloadV3() {}

type peerPhaseDeadlineV3 struct {
	Phase      string `json:"phase"`
	DeadlineMS string `json:"deadline_ms"`
}

type peerCandidateCountsV3 struct {
	LocalEmitted   uint32 `json:"local_emitted"`
	RemoteAccepted uint32 `json:"remote_accepted"`
}

type peerAdmissionV3 struct {
	Disposition      string `json:"disposition"`
	ResponseDelivery string `json:"response_delivery"`
}

type peerRejectionV3 struct {
	Code         string  `json:"code"`
	RetryAfterMS *string `json:"retry_after_ms,omitempty"`
}

type peerFailureV3 struct {
	FailedAtStage string    `json:"failed_at_stage"`
	Scope         string    `json:"scope"`
	Failure       failureV3 `json:"failure"`
}

type peerAttemptPayloadV3 struct {
	AttemptSequence  string                 `json:"attempt_sequence"`
	AttemptElapsedMS string                 `json:"attempt_elapsed_ms"`
	Stage            string                 `json:"stage"`
	OfferOperationID *string                `json:"offer_operation_id,omitempty"`
	PhaseDeadline    *peerPhaseDeadlineV3   `json:"phase_deadline,omitempty"`
	Candidates       *peerCandidateCountsV3 `json:"candidates,omitempty"`
	GrantOperationID *string                `json:"grant_operation_id,omitempty"`
	Admission        *peerAdmissionV3       `json:"admission,omitempty"`
	Rejection        *peerRejectionV3       `json:"rejection,omitempty"`
	Failure          *peerFailureV3         `json:"failure,omitempty"`
}

func (peerAttemptPayloadV3) runTracePayloadV3() {}

type transferLifecyclePayloadV3 struct {
	ReceiveOperationID string            `json:"receive_operation_id"`
	TransferJobID      string            `json:"transfer_job_id"`
	Stage              string            `json:"stage"`
	FileSelection      string            `json:"file_selection"`
	FileSettlement     string            `json:"file_settlement"`
	ItemBlockReason    *string           `json:"item_block_reason,omitempty"`
	TreeSettlement     string            `json:"tree_settlement"`
	Progress           progressPayloadV3 `json:"progress"`
	Failure            *failureV3        `json:"failure,omitempty"`
}

func (transferLifecyclePayloadV3) runTracePayloadV3() {}

type filesystemNativeLockV3 struct {
	Scope     string `json:"scope"`
	Milestone string `json:"milestone"`
}

type filesystemRuntimeDecisionV3 struct {
	Component string `json:"component"`
	Operation string `json:"operation"`
	Decision  string `json:"decision"`
}

type filesystemCorrelationV3 struct {
	OperationID *string `json:"operation_id,omitempty"`
	ClaimID     *string `json:"claim_id,omitempty"`
}

type filesystemCountersV3 struct {
	NodeClaims             string `json:"node_claims"`
	DirectoryClaims        string `json:"directory_claims"`
	FileClaims             string `json:"file_claims"`
	ActiveFileClaims       string `json:"active_file_claims"`
	ReservedFileSlots      string `json:"reserved_file_slots"`
	DirectoryMetadataBytes string `json:"directory_metadata_bytes"`
	CheckpointRecords      string `json:"checkpoint_records"`
}

type filesystemFailureV3 struct {
	Stage              string    `json:"stage"`
	ReconciliationStep *string   `json:"reconciliation_step,omitempty"`
	NativeErrorClass   *string   `json:"native_error_class,omitempty"`
	Failure            failureV3 `json:"failure"`
}

type filesystemOutputPayloadV3 struct {
	Operation           string                       `json:"operation"`
	ReceiveOperationID  *string                      `json:"receive_operation_id,omitempty"`
	ReceiveIntentDigest *string                      `json:"receive_intent_digest,omitempty"`
	OutputSessionID     *string                      `json:"output_session_id,omitempty"`
	Certification       *string                      `json:"certification,omitempty"`
	NativeLock          *filesystemNativeLockV3      `json:"native_lock,omitempty"`
	RootDisposition     *string                      `json:"root_disposition,omitempty"`
	RuntimeDecision     *filesystemRuntimeDecisionV3 `json:"runtime_decision,omitempty"`
	CheckpointDecision  *string                      `json:"checkpoint_decision,omitempty"`
	Correlation         *filesystemCorrelationV3     `json:"output_correlation,omitempty"`
	Counters            filesystemCountersV3         `json:"counters"`
	Failure             *filesystemFailureV3         `json:"failure,omitempty"`
}

func (filesystemOutputPayloadV3) runTracePayloadV3() {}

type senderTerminalSendPayloadV3 struct {
	Settled              bool   `json:"settled"`
	TransportDisposition string `json:"transport_disposition"`
	Outcome              string `json:"outcome"`
	Decision             string `json:"decision"`
}

func (senderTerminalSendPayloadV3) runTracePayloadV3() {}

type senderSessionTerminatedPayloadV3 struct {
	Trigger    string `json:"trigger"`
	Provenance string `json:"provenance"`
}

func (senderSessionTerminatedPayloadV3) runTracePayloadV3() {}

type catalogUsageV3 struct {
	ActiveScans string `json:"active_scans"`
	ScanWork    string `json:"scan_work"`
	Entries     string `json:"entries"`
	MemoryBytes string `json:"memory_bytes"`
	SpillBytes  string `json:"spill_bytes"`
}

type catalogStoragePayloadV3 struct {
	Operation          string         `json:"operation"`
	Cause              string         `json:"cause"`
	Usage              catalogUsageV3 `json:"usage"`
	LegacyRootsRemoved string         `json:"legacy_roots_removed"`
}

func (catalogStoragePayloadV3) runTracePayloadV3() {}

type rootPrefetchPayloadV3 struct {
	Decision     string `json:"decision"`
	Attempt      string `json:"attempt"`
	EntryCount   string `json:"entry_count"`
	OmittedCount string `json:"omitted_count"`
}

func (rootPrefetchPayloadV3) runTracePayloadV3() {}

type protocolSendV3 struct {
	Settled  bool   `json:"settled"`
	Admitted bool   `json:"admitted"`
	Outcome  string `json:"outcome"`
}

type ProtocolFailureV1 struct {
	RequestKind  string                      `json:"request_kind"`
	WireScope    string                      `json:"wire_scope"`
	WireCode     uint16                      `json:"wire_code"`
	Retryable    bool                        `json:"retryable"`
	RetryAfterMS *uint32                     `json:"retry_after_ms,omitempty"`
	Settlement   protocolFailureSettlementV1 `json:"settlement"`
	Correlation  CorrelationV1               `json:"correlation"`
}

type protocolFailureSettlementV1 interface {
	protocolFailureSettlementV1()
}

type receivedAuthenticatedSettlementV1 struct {
	Kind string `json:"kind"`
}

func (receivedAuthenticatedSettlementV1) protocolFailureSettlementV1() {}

type responseSendSettlementV1 struct {
	Kind     string `json:"kind"`
	Admitted bool   `json:"admitted"`
	Settled  bool   `json:"settled"`
	Outcome  string `json:"outcome"`
}

func (responseSendSettlementV1) protocolFailureSettlementV1() {}

type protocolOperationPayloadV3 struct {
	Role                    string             `json:"role"`
	Stage                   string             `json:"stage"`
	RequestKind             string             `json:"request_kind"`
	ResponseKind            *string            `json:"response_kind,omitempty"`
	Send                    *protocolSendV3    `json:"send,omitempty"`
	ResponseCount           string             `json:"response_count"`
	DeadlineRemainingMS     *string            `json:"deadline_remaining_ms,omitempty"`
	OperationElapsedMS      string             `json:"operation_elapsed_ms"`
	UsableLanesAtSelection  uint32             `json:"usable_lanes_at_selection"`
	UsableLanesAtSettlement uint32             `json:"usable_lanes_at_settlement"`
	Cause                   string             `json:"cause"`
	ProtocolFailure         *ProtocolFailureV1 `json:"protocol_failure,omitempty"`
}

func (protocolOperationPayloadV3) runTracePayloadV3() {}

type laneSettlementPayloadV3 struct {
	Route               string `json:"route"`
	DeliveredBlocks     string `json:"delivered_blocks"`
	DeliveredBytes      string `json:"delivered_bytes"`
	FailedBlockAttempts string `json:"failed_block_attempts"`
	ReassignedBlocks    string `json:"reassigned_blocks"`
	Incomplete          bool   `json:"incomplete"`
}

func (laneSettlementPayloadV3) runTracePayloadV3() {}

type observerLossPayloadV3 struct {
	Category string `json:"category"`
	Reason   string `json:"reason"`
	Count    string `json:"count"`
}

func (observerLossPayloadV3) runTracePayloadV3() {}

type receiverTerminationPayloadV3 struct {
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

func (receiverTerminationPayloadV3) runTracePayloadV3() {}

type traceSummaryPayloadV3 struct {
	Incomplete       bool   `json:"incomplete"`
	LifecycleDropped string `json:"lifecycle_dropped"`
	ProgressDropped  string `json:"progress_dropped"`
	EventsWritten    string `json:"events_written"`
	WriterFailed     bool   `json:"writer_failed"`
	FlushFailed      bool   `json:"flush_failed"`
	SchemaLimited    bool   `json:"schema_limited"`
}

func (traceSummaryPayloadV3) runTracePayloadV3() {}
