package wire

type RelayOrigin struct {
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
	RelayOrigin RelayOrigin `json:"relay_origin"`
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
	RelayOrigin RelayOrigin           `json:"relay_origin"`
	Attempt     uint32                `json:"attempt"`
	State       string                `json:"state"`
	Failure     *Failure              `json:"failure,omitempty"`
	Details     *RelayRecoveryDetails `json:"details,omitempty"`
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

type EmptyPayload struct{}

func (EmptyPayload) ObservationPayload() {}
