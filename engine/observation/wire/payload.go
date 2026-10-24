package wire

type RelayAuthority struct {
	Scheme string `json:"scheme"`
	Host   string `json:"host"`
	Port   uint16 `json:"port"`
}

type Fault struct {
	Domain string `json:"domain"`
	Scope  string `json:"scope"`
	Code   uint16 `json:"code"`
}

type Failure struct {
	Code         string  `json:"code"`
	MessageKey   string  `json:"message_key"`
	Fault        *Fault  `json:"fault,omitempty"`
	RetryAfterMS *string `json:"retry_after_ms,omitempty"`
	SourcePath   string  `json:"source_path,omitempty"`
}

type FileOutcomes struct {
	DownloadedFiles          string `json:"downloaded_files"`
	PreviouslyPublishedFiles string `json:"previously_published_files"`
	ResumedFiles             string `json:"resumed_files"`
	PausedFiles              string `json:"paused_files"`
	CollisionFiles           string `json:"collision_files"`
	ItemBlockedFiles         string `json:"item_blocked_files"`
	FailedFiles              string `json:"failed_files"`
	ModifiedTimeWarnings     string `json:"modified_time_warnings"`
}

type CapacityWait struct {
	ActiveWaiters     string `json:"active_waiters"`
	AccumulatedWaitMS string `json:"accumulated_wait_ms"`
	Attempts          string `json:"attempts"`
}

type ProgressPayload struct {
	Discovery                string       `json:"discovery"`
	CountersExact            bool         `json:"counters_exact"`
	DiscoveredFiles          string       `json:"discovered_files"`
	DiscoveredBytes          string       `json:"discovered_bytes"`
	PublishedFiles           string       `json:"published_files"`
	PublishedBytes           string       `json:"published_bytes"`
	PreviouslyPublishedBytes string       `json:"previously_published_bytes"`
	VerifiedBytes            string       `json:"verified_bytes"`
	NewlyVerifiedBytes       string       `json:"newly_verified_bytes"`
	FileOutcomes             FileOutcomes `json:"file_outcomes"`
	CapacityWait             CapacityWait `json:"capacity_wait"`
}

type SharingSubjectPayload struct {
	SubjectKind   string  `json:"subject_kind"`
	SelectedItems string  `json:"selected_items"`
	FileBytes     *string `json:"file_bytes,omitempty"`
}

func (SharingSubjectPayload) ObservationPayload() {}

type RelayConnectedPayload struct {
	RelayAuthority RelayAuthority `json:"relay_authority"`
}

type RelayAvailabilityPayload struct {
	Available uint32 `json:"available"`
	Total     uint32 `json:"total"`
	EverReady bool   `json:"ever_ready"`
	Terminal  uint32 `json:"terminal"`
}

func (RelayAvailabilityPayload) ObservationPayload() {}

func (RelayConnectedPayload) ObservationPayload() {}

type RelayRecoveringPayload struct {
	RelayAuthority RelayAuthority        `json:"relay_authority"`
	Attempt        uint32                `json:"attempt"`
	State          string                `json:"state"`
	Failure        *Failure              `json:"failure,omitempty"`
	Details        *RelayRecoveryDetails `json:"details,omitempty"`
}

type RelayRecoveryDetails struct {
	ShareInstance *string `json:"share_instance,omitempty"`
	Generation    string  `json:"connection_generation"`
	Slow          bool    `json:"slow_wait"`
	Resume        bool    `json:"resume_registration"`
	Terminal      bool    `json:"terminal"`
	NextDelayMS   string  `json:"next_delay_ms"`
}

func (RelayRecoveringPayload) ObservationPayload() {}

type ContentPathSelectedPayload struct {
	ContentPath string `json:"content_path"`
}

func (ContentPathSelectedPayload) ObservationPayload() {}

type FallbackPayload struct {
	FromTransport string  `json:"from_transport"`
	ToTransport   string  `json:"to_transport"`
	Failure       Failure `json:"failure"`
}

func (FallbackPayload) ObservationPayload() {}

type TransferProgressPayload struct {
	ReceiveOperationID string          `json:"receive_operation_id"`
	TransferJobID      string          `json:"transfer_job_id"`
	Progress           ProgressPayload `json:"progress"`
}

func (TransferProgressPayload) ObservationPayload() {}

type WarningPayload struct {
	Failure Failure `json:"failure"`
}

func (WarningPayload) ObservationPayload() {}

type LaneAdoptedPayload struct {
	Transport string `json:"transport"`
}

func (LaneAdoptedPayload) ObservationPayload() {}

type RelayLifecyclePayload struct {
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
	HeartbeatRound   *string `json:"heartbeat_round,omitempty"`
	WaitMS           *string `json:"wait_ms,omitempty"`
	TimeoutMS        *string `json:"timeout_ms,omitempty"`
}

func (RelayLifecyclePayload) ObservationPayload() {}

type WebRTCLifecyclePayload struct {
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

func (WebRTCLifecyclePayload) ObservationPayload() {}

type PeerPhaseDeadline struct {
	Phase      string `json:"phase"`
	DeadlineMS string `json:"deadline_ms"`
}

type PeerCandidateCounts struct {
	LocalEmitted   uint32 `json:"local_emitted"`
	RemoteAccepted uint32 `json:"remote_accepted"`
}

type PeerAdmission struct {
	Disposition      string `json:"disposition"`
	ResponseDelivery string `json:"response_delivery"`
}

type PeerRejection struct {
	Code         string  `json:"code"`
	RetryAfterMS *string `json:"retry_after_ms,omitempty"`
}

type PeerFailureSummary struct {
	LastCompletedStage string `json:"last_completed_stage"`
	StageElapsedMillis string `json:"stage_elapsed_ms"`
	DeadlineExpired    bool   `json:"deadline_expired"`
	Initiator          string `json:"close_initiator"`
	Cause              string `json:"cause"`
}

type PeerFailure struct {
	Summary       *PeerFailureSummary `json:"summary,omitempty"`
	FailedAtStage string              `json:"failed_at_stage"`
	Scope         string              `json:"scope"`
	Failure       Failure             `json:"failure"`
}

type PeerAttemptPayload struct {
	Route            string               `json:"route,omitempty"`
	AttemptSequence  string               `json:"attempt_sequence"`
	AttemptElapsedMS string               `json:"attempt_elapsed_ms"`
	Stage            string               `json:"stage"`
	OfferOperationID *string              `json:"offer_operation_id,omitempty"`
	PhaseDeadline    *PeerPhaseDeadline   `json:"phase_deadline,omitempty"`
	Candidates       *PeerCandidateCounts `json:"candidates,omitempty"`
	GrantOperationID *string              `json:"grant_operation_id,omitempty"`
	Admission        *PeerAdmission       `json:"admission,omitempty"`
	Rejection        *PeerRejection       `json:"rejection,omitempty"`
	Failure          *PeerFailure         `json:"failure,omitempty"`
}

func (PeerAttemptPayload) ObservationPayload() {}

type CapacityScope struct {
	StableHandles            string `json:"stable_handles"`
	ActiveLeases             string `json:"active_leases"`
	StableHandleLimit        string `json:"stable_handle_limit"`
	ActiveLeaseLimit         string `json:"active_lease_limit"`
	ReclaimableStableHandles string `json:"reclaimable_stable_handles"`
	QuarantinedStableHandles string `json:"quarantined_stable_handles"`
	PendingAdmissions        string `json:"pending_admissions"`
	ActiveReclaims           string `json:"active_reclaims"`
}

type SenderCapacityPayload struct {
	CapacityStoreID string         `json:"capacity_store_id,omitempty"`
	CapacityShareID string         `json:"capacity_share_id,omitempty"`
	Stage           string         `json:"stage"`
	DecisionID      *string        `json:"decision_id,omitempty"`
	RevisionID      *string        `json:"revision_id,omitempty"`
	Process         CapacityScope  `json:"process"`
	Share           *CapacityScope `json:"share,omitempty"`
	Session         *CapacityScope `json:"session,omitempty"`
}

func (SenderCapacityPayload) ObservationPayload() {}

type SenderRevisionPayload struct {
	Stage      string  `json:"stage"`
	Cause      string  `json:"cause"`
	RevisionID string  `json:"revision_id"`
	LeaseID    *string `json:"lease_id,omitempty"`
}

func (SenderRevisionPayload) ObservationPayload() {}

type TransferCapacityLifecycle struct {
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

type TransferLifecyclePayload struct {
	ReceiveOperationID string                     `json:"receive_operation_id"`
	TransferJobID      string                     `json:"transfer_job_id"`
	Stage              string                     `json:"stage"`
	FileSelection      string                     `json:"file_selection"`
	FileSettlement     string                     `json:"file_settlement"`
	ItemBlockReason    *string                    `json:"item_block_reason,omitempty"`
	TreeSettlement     string                     `json:"tree_settlement"`
	Progress           ProgressPayload            `json:"progress"`
	Capacity           *TransferCapacityLifecycle `json:"capacity,omitempty"`
	Failure            *Failure                   `json:"failure,omitempty"`
}

func (TransferLifecyclePayload) ObservationPayload() {}

type SenderTerminalSendPayload struct {
	Settled              bool   `json:"settled"`
	TransportDisposition string `json:"transport_disposition"`
	Outcome              string `json:"outcome"`
	Decision             string `json:"decision"`
}

func (SenderTerminalSendPayload) ObservationPayload() {}

type SenderSessionTerminatedPayload struct {
	Trigger    string             `json:"trigger"`
	Provenance string             `json:"provenance"`
	Failure    *DiagnosticFailure `json:"failure,omitempty"`
}

func (SenderSessionTerminatedPayload) ObservationPayload() {}

type CatalogUsage struct {
	ActiveScans string `json:"active_scans"`
	ScanWork    string `json:"scan_work"`
	Entries     string `json:"entries"`
	MemoryBytes string `json:"memory_bytes"`
	SpillBytes  string `json:"spill_bytes"`
}

type CatalogStoragePayload struct {
	Operation          string       `json:"operation"`
	Cause              string       `json:"cause"`
	Usage              CatalogUsage `json:"usage"`
	LegacyRootsRemoved string       `json:"legacy_roots_removed"`
}

func (CatalogStoragePayload) ObservationPayload() {}

type RootPrefetchPayload struct {
	Decision     string `json:"decision"`
	Attempt      string `json:"attempt"`
	EntryCount   string `json:"entry_count"`
	OmittedCount string `json:"omitted_count"`
}

func (RootPrefetchPayload) ObservationPayload() {}

type ProtocolSend struct {
	Settled  bool   `json:"settled"`
	Admitted bool   `json:"admitted"`
	Outcome  string `json:"outcome"`
}

type ProtocolErrorContent struct {
	Scope        string  `json:"scope"`
	Code         uint16  `json:"code"`
	Retryable    bool    `json:"retryable"`
	RetryAfterMS *uint32 `json:"retry_after_ms,omitempty"`
}
type ProtocolContext struct {
	ObservedAt  string `json:"observed_at"`
	Role        string `json:"role"`
	RequestKind string `json:"request_kind,omitempty"`
}
type SendAttempt struct {
	Cause                SendAttemptCause `json:"cause"`
	AttemptSequence      string           `json:"attempt_sequence"`
	LaneID               uint32           `json:"lane_id"`
	LaneEpoch            uint32           `json:"lane_epoch"`
	PolicyAdmitted       bool             `json:"policy_admitted"`
	Settled              bool             `json:"settled"`
	Outcome              string           `json:"outcome"`
	TransportDisposition *string          `json:"transport_disposition,omitempty"`
	End                  string           `json:"end"`
}
type ResponseSendResult struct {
	Started                bool          `json:"started"`
	Evidence               string        `json:"evidence"`
	End                    string        `json:"end"`
	Cleanup                string        `json:"cleanup"`
	Attempts               []SendAttempt `json:"attempts"`
	PendingAttemptSequence *string       `json:"pending_attempt_sequence,omitempty"`
}
type ProtocolResponseSendPayload struct {
	ProtocolContext
	ResponseSequence string                `json:"response_sequence"`
	ResponseKind     string                `json:"response_kind"`
	ProtocolError    *ProtocolErrorContent `json:"protocol_error,omitempty"`
	ResponseResult   ResponseSendResult    `json:"response_result"`
}

func (ProtocolResponseSendPayload) ObservationPayload() {}

type ProtocolSendAttemptSettledPayload struct {
	ProtocolContext
	ResponseSequence string      `json:"response_sequence"`
	ResponseKind     string      `json:"response_kind"`
	Attempt          SendAttempt `json:"attempt"`
}

func (ProtocolSendAttemptSettledPayload) ObservationPayload() {}

type ProtocolErrorReceivedPayload struct {
	ProtocolContext
	ProtocolError *ProtocolErrorContent `json:"protocol_error"`
}

func (ProtocolErrorReceivedPayload) ObservationPayload() {}

type SenderContentDecisionPayload struct {
	ProtocolContext
	ContentDecision *SenderContentDecision `json:"content_decision"`
}

func (SenderContentDecisionPayload) ObservationPayload() {}

type SenderContentDecision struct {
	Kind               string  `json:"kind"`
	CapacityDecisionID *string `json:"capacity_decision_id,omitempty"`
	LeaseID            *string `json:"lease_id,omitempty"`
}

type RequestScheduling struct {
	ExpectedMS      string `json:"expected_ms"`
	ResponseMS      string `json:"response_ms"`
	QueuedContentMS string `json:"queued_content_ms"`
	PendingRequests uint32 `json:"pending_requests"`
}

type BlockWait struct {
	Phase         string `json:"phase"`
	WaitedMS      string `json:"waited_ms"`
	QueueProgress string `json:"queue_progress"`
}

type ProtocolOperationPayload struct {
	BlockWait               *BlockWait         `json:"block_wait,omitempty"`
	RequestScheduling       *RequestScheduling `json:"request_scheduling,omitempty"`
	ObservedAt              string             `json:"observed_at"`
	Role                    string             `json:"role"`
	Stage                   string             `json:"stage"`
	RequestKind             string             `json:"request_kind"`
	ResponseKind            *string            `json:"response_kind,omitempty"`
	Send                    *ProtocolSend      `json:"send,omitempty"`
	ResponseCount           string             `json:"response_count"`
	DeadlineRemainingMS     *string            `json:"deadline_remaining_ms,omitempty"`
	OperationElapsedMS      string             `json:"operation_elapsed_ms"`
	UsableLanesAtSelection  uint32             `json:"usable_lanes_at_selection"`
	UsableLanesAtSettlement uint32             `json:"usable_lanes_at_settlement"`
	Cause                   string             `json:"cause"`
}

func (ProtocolOperationPayload) ObservationPayload() {}

type LaneSettlementPayload struct {
	Route               string `json:"route"`
	DeliveredBlocks     string `json:"delivered_blocks"`
	DeliveredBytes      string `json:"delivered_bytes"`
	FailedBlockAttempts string `json:"failed_block_attempts"`
	ReassignedBlocks    string `json:"reassigned_blocks"`
	Incomplete          bool   `json:"incomplete"`
}

func (LaneSettlementPayload) ObservationPayload() {}

type ObserverLossPayload struct {
	OmittedSamples string                       `json:"omitted_samples"`
	Category       string                       `json:"category"`
	Reason         string                       `json:"reason"`
	Count          string                       `json:"count"`
	Rejection      *ObservationRejectionPayload `json:"rejection,omitempty"`
}

type ObservationRejectionPayload struct {
	Event            string           `json:"source_event"`
	Source           string           `json:"source_location"`
	Stage            string           `json:"source_stage"`
	Field            string           `json:"field"`
	Rule             string           `json:"rule"`
	Session          string           `json:"sample_protocol_session_id,omitempty"`
	Operation        string           `json:"sample_protocol_operation_id,omitempty"`
	Revision         string           `json:"sample_revision_id,omitempty"`
	ResponseSequence string           `json:"sample_response_sequence,omitempty"`
	AttemptSequence  string           `json:"sample_attempt_sequence,omitempty"`
	Evidence         []RejectionField `json:"evidence"`
	Truncated        bool             `json:"truncated"`
	OmittedFields    string           `json:"omitted_fields"`
	OmittedBytes     string           `json:"omitted_bytes"`
}
type RejectionField struct {
	Field          string `json:"field"`
	Representation string `json:"representation"`
	Value          string `json:"value"`
}

func (ObserverLossPayload) ObservationPayload() {}

type ReceiverTerminationPayload struct {
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

func (ReceiverTerminationPayload) ObservationPayload() {}

type SendAttemptCause struct {
	Kind      string `json:"kind"`
	Detail    string `json:"detail,omitempty"`
	Truncated bool   `json:"truncated"`
}

type EmptyPayload struct{}

func (EmptyPayload) ObservationPayload() {}
